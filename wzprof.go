package wzprof

import (
	"context"
	"fmt"
	"hash/maphash"
	"net/http"
	"os"
	"strings"
	"time"
	"unsafe"

	"github.com/google/pprof/profile"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"golang.org/x/exp/slices"
)

type stackIteratorMaker interface {
	Make(mod api.Module, def api.FunctionDefinition, wasmsi experimental.StackIterator) experimental.StackIterator
}

// Runtime contains information specific to the guest runtime.
type Runtime struct {
	symbols       symbolizer
	stackIterator stackIteratorMaker
}

// NewRuntime creates a new runtime with sensible defaults.
func NewRuntime() *Runtime {
	return &Runtime{
		stackIterator: wasmStackIteratorMaker{},
		symbols:       noopsymbolizer{},
	}
}

// PrepareModule selects the most appropriate analysis functions for the guest
// code in the provided module.
func (r *Runtime) PrepareModule(wasm []byte, mod wazero.CompiledModule) error {
	switch {
	case compiledByGo(mod):
		s, err := buildPclntabSymbolizer(wasm, mod)
		if err != nil {
			return err
		}
		r.symbols = s
		r.stackIterator = &goStackIteratorMaker{
			goStackIterator: goStackIterator{
				rt:       r,
				unwinder: unwinder{symbols: s},
			},
		}
	default:
		r.symbols, _ = buildDwarfSymbolizer(mod) // TODO: surface error as warning?
	}

	return nil
}

type wasmStackIteratorMaker struct{}

func (w wasmStackIteratorMaker) Make(mod api.Module, def api.FunctionDefinition, wasmsi experimental.StackIterator) experimental.StackIterator {
	return wasmsi
}

type goStackIteratorMaker struct {
	goStackIterator
}

func (g *goStackIteratorMaker) Make(mod api.Module, def api.FunctionDefinition, wasmsi experimental.StackIterator) experimental.StackIterator {
	imod := mod.(experimental.InternalModule)
	mem := imod.Memory()
	g.mem = rtmem{mem}
	sp0 := uint32(imod.Global(0).Get())
	gp0 := imod.Global(2).Get()
	pc0 := g.symbols.FIDToPC(fid(def.Index()))
	g.initAt(ptr(pc0), ptr(sp0), 0, gptr(gp0), 0)
	g.first = true
	return g
}

// lrtAdapter wraps a FunctionListener to adapt its stack iterator to the
// appropriate implementation according to the module runtime.
type lrtAdapter struct {
	rt *Runtime
	l  experimental.FunctionListener
}

func (a lrtAdapter) Before(ctx context.Context, mod api.Module, def api.FunctionDefinition, params []uint64, si experimental.StackIterator) {
	si = a.rt.stackIterator.Make(mod, def, si)
	a.l.Before(ctx, mod, def, params, si)
}

func (a lrtAdapter) After(ctx context.Context, mod api.Module, def api.FunctionDefinition, results []uint64) {
	a.l.After(ctx, mod, def, results)
}

func (a lrtAdapter) Abort(ctx context.Context, mod api.Module, def api.FunctionDefinition, err error) {
	a.l.Abort(ctx, mod, def, err)
}

// Profiler is an interface implemented by all profiler types available in this
// package.
type Profiler interface {
	experimental.FunctionListenerFactory

	// Name of the profiler.
	Name() string

	// Desc is a human-readable description of the profiler.
	Desc() string

	// Count the number of execution stacks recorded in the profiler.
	Count() int

	// SampleType returns the set of value types present in samples recorded by
	// the profiler.
	SampleType() []*profile.ValueType

	// NewHandler returns a new http handler suited to expose profiles on a
	// pprof endpoint.
	NewHandler(sampleRate float64) http.Handler
}

var (
	_ Profiler = (*CPUProfiler)(nil)
	_ Profiler = (*MemoryProfiler)(nil)
)

//go:linkname nanotime runtime.nanotime
func nanotime() int64

// WriteProfile writes a profile to a file at the given path.
func WriteProfile(path string, prof *profile.Profile) error {
	w, err := os.Create(path)
	if err != nil {
		return err
	}
	defer w.Close()
	return prof.Write(w)
}

type symbolizer interface {
	// Locations returns a list of function locations for a given program
	// counter, and the address it found them at. Locations start from
	// current function followed by the inlined functions, in order of
	// inlining. Result if empty if the pc cannot be resolved.
	Locations(fn experimental.InternalFunction, pc experimental.ProgramCounter) (uint64, []location)
}

type noopsymbolizer struct{}

func (s noopsymbolizer) Locations(fn experimental.InternalFunction, pc experimental.ProgramCounter) (uint64, []location) {
	return 0, nil
}

type location struct {
	File    string
	Line    int64
	Column  int64
	Inlined bool
	// Linkage Name if present, Name otherwise.
	// Only present for inlined functions.
	StableName string
	HumanName  string
}

func locationForCall(rt *Runtime, fn experimental.InternalFunction, pc experimental.ProgramCounter, funcs map[string]*profile.Function) *profile.Location {
	// Cache miss. Get or create function and all the line
	// locations associated with inlining.
	var locations []location
	var symbolFound bool
	def := fn.Definition()

	out := &profile.Location{}

	if pc > 0 {
		out.Address, locations = rt.symbols.Locations(fn, pc)
		symbolFound = len(locations) > 0
	}
	if len(locations) == 0 {
		// If we don't have a source location, attach to a
		// generic location within the function.
		locations = []location{{}}
	}
	// Provide defaults in case we couldn't resolve DWARF information for
	// the main function call's PC.
	if locations[0].StableName == "" {
		locations[0].StableName = def.Name()
	}
	if locations[0].HumanName == "" {
		locations[0].HumanName = def.Name()
	}

	lines := make([]profile.Line, len(locations))

	for i, loc := range locations {
		pprofFn := funcs[loc.StableName]

		if pprofFn == nil {
			pprofFn = &profile.Function{
				ID:         uint64(len(funcs)) + 1, // 0 is reserved by pprof
				Name:       loc.HumanName,
				SystemName: loc.StableName,
				Filename:   loc.File,
			}
			funcs[loc.StableName] = pprofFn
		} else if symbolFound {
			// Sometimes the function had to be created while the PC
			// wasn't found by the symbol mapper. Attempt to correct
			// it if we had a successful match this time.
			pprofFn.Name = locations[i].HumanName
			pprofFn.SystemName = locations[i].StableName
			pprofFn.Filename = locations[i].File
		}

		// Pprof expects lines to start with the root of the inlined
		// calls. DWARF encodes that information the other way around,
		// so we fill lines backwards.
		lines[len(locations)-(i+1)] = profile.Line{
			Function: pprofFn,
			Line:     loc.Line,
		}
	}

	out.Line = lines
	return out
}

type locationKey struct {
	module string
	index  uint32
	name   string
	pc     uint64
}

func makeLocationKey(fn api.FunctionDefinition, pc experimental.ProgramCounter) locationKey {
	return locationKey{
		module: fn.ModuleName(),
		index:  fn.Index(),
		name:   fn.Name(),
		pc:     uint64(pc),
	}
}

type stackCounterMap map[uint64]*stackCounter

func (scm stackCounterMap) lookup(st stackTrace) *stackCounter {
	sc := scm[st.key]
	if sc == nil {
		sc = &stackCounter{stack: st.clone()}
		scm[st.key] = sc
	}
	return sc
}

func (scm stackCounterMap) observe(st stackTrace, val int64) {
	scm.lookup(st).observe(val)
}

func (scm stackCounterMap) len() int {
	return len(scm)
}

type stackCounter struct {
	stack stackTrace
	value [2]int64 // count, total
}

func (sc *stackCounter) observe(value int64) {
	sc.value[0] += 1
	sc.value[1] += value
}

func (sc *stackCounter) count() int64 {
	return sc.value[0]
}

func (sc *stackCounter) total() int64 {
	return sc.value[1]
}

func (sc *stackCounter) sampleLocation() stackTrace {
	return sc.stack
}

func (sc *stackCounter) sampleValue() []int64 {
	return sc.value[:]
}

func (sc *stackCounter) String() string {
	return fmt.Sprintf("{count:%d,total:%d}", sc.count(), sc.total())
}

// Compile-time check that program counters are uint64 values.
var _ = assertTypeIsUint64[experimental.ProgramCounter]()

func assertTypeIsUint64[T ~uint64]() bool {
	return true
}

type stackFrame struct {
	fn experimental.InternalFunction
	pc experimental.ProgramCounter
}

type stackTrace struct {
	fns []experimental.InternalFunction
	pcs []experimental.ProgramCounter
	key uint64
}

func makeStackTrace(st stackTrace, si experimental.StackIterator) stackTrace {
	st.fns = st.fns[:0]
	st.pcs = st.pcs[:0]

	for si.Next() {
		st.fns = append(st.fns, si.Function())
		st.pcs = append(st.pcs, si.ProgramCounter())
	}
	st.key = maphash.Bytes(stackTraceHashSeed, st.bytes())
	return st
}

func (st stackTrace) host() bool {
	return len(st.fns) > 0 && st.fns[0].Definition().GoFunction() != nil
}

func (st stackTrace) len() int {
	return len(st.pcs)
}

func (st stackTrace) index(i int) stackFrame {
	return stackFrame{
		fn: st.fns[i],
		pc: st.pcs[i],
	}
}

func (st stackTrace) clone() stackTrace {
	return stackTrace{
		fns: slices.Clone(st.fns),
		pcs: slices.Clone(st.pcs),
		key: st.key,
	}
}

func (st stackTrace) bytes() []byte {
	pcs := unsafe.SliceData(st.pcs)
	return unsafe.Slice((*byte)(unsafe.Pointer(pcs)), 8*len(st.pcs))
}

func (st stackTrace) String() string {
	sb := new(strings.Builder)
	for i, n := 0, st.len(); i < n; i++ {
		frame := st.index(i)
		fndef := frame.fn.Definition()
		fmt.Fprintf(sb, "%016x: %s\n", frame.pc, fndef.DebugName())
	}
	return sb.String()
}

var stackTraceHashSeed = maphash.MakeSeed()

type sampleType interface {
	sampleLocation() stackTrace
	sampleValue() []int64
}

func buildProfile[T sampleType](r *Runtime, samples map[uint64]T, start time.Time, duration time.Duration, sampleType []*profile.ValueType, ratios []float64) *profile.Profile {
	prof := &profile.Profile{
		SampleType:    sampleType,
		Sample:        make([]*profile.Sample, 0, len(samples)),
		TimeNanos:     start.UnixNano(),
		DurationNanos: int64(duration),
	}

	locationID := uint64(1)
	locationCache := make(map[locationKey]*profile.Location)
	functionCache := make(map[string]*profile.Function)

	for _, sample := range samples {
		stack := sample.sampleLocation()
		location := make([]*profile.Location, stack.len())

		for i := range location {
			fn := stack.fns[i]
			pc := stack.pcs[i]

			def := fn.Definition()
			key := makeLocationKey(def, pc)
			loc := locationCache[key]
			if loc == nil {
				loc = locationForCall(r, fn, pc, functionCache)
				loc.ID = locationID
				locationID++
				locationCache[key] = loc
			}

			location[i] = loc
		}

		prof.Sample = append(prof.Sample, &profile.Sample{
			Location: location,
			Value:    sample.sampleValue()[:len(sampleType)],
		})
	}

	prof.Location = make([]*profile.Location, len(locationCache))
	prof.Function = make([]*profile.Function, len(functionCache))

	for _, loc := range locationCache {
		prof.Location[loc.ID-1] = loc
	}

	for _, fn := range functionCache {
		prof.Function[fn.ID-1] = fn
	}

	if err := prof.ScaleN(ratios[:len(sampleType)]); err != nil {
		panic(err)
	}
	return prof
}
