package wzprof

import (
	"container/list"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/cespare/xxhash"
	"github.com/google/pprof/profile"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

type ProfileFunction func(params []uint64, globals []api.Global, mem api.Memory) int64

type Profiler interface {
	// SampleType is called once initially to register the pprof type of samples
	// collected by this profiler. Only one type permitted for now.
	SampleType() profile.ValueType

	// Sampler is called once initially to register which sampler to use for
	// this profiler.
	Sampler() Sampler

	// Register is called at the initialization of the module to register
	// possible profile functions that can be returned by Listen. Empty string
	// is not a valid key.
	Register() map[string]ProfileFunction

	// Listen is called at the initialization of the module for each function
	// 'name' definied in it. Return the name of a function as defined in
	// Register, or empty string to not listen.
	Listen(name string) string
}

// ProfilerListener is a FunctionListenerFactory carrying a list of profilers.
type ProfilerListener struct {
	profilers  []Profiler
	profileFns map[string]ProfileFunction
	samplerFns []Sampler

	hooks         map[string]*hook
	lastStackSize int
	samples       *list.List

	samplesMu sync.RWMutex

	mapper   mapper
	locCache map[pprofLocationKey]*profile.Location
}

type pprofLocationKey struct {
	Module string
	Index  uint32
	Name   string
	PC     uint64
}

type location struct {
	File    string
	Line    int64
	Column  int64
	Inlined bool
	PC      uint64
}

type mapper interface {
	Lookup(pc uint64) []location
}

func (p *ProfilerListener) PrepareSymbols(m wazero.CompiledModule) {
	var err error
	sc := m.CustomSections()

	p.mapper, err = newDwarfmapper(sc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "profiler: could not load dwarf symbols: %s", err)
	}
}

// DefaultMaxStacksCount is the default maximum number of stacks to keep in.
const DefaultMaxStacksCount = 250000

func keyFor(idx int, name string) string {
	return fmt.Sprintf("%d:%s", idx, name)
}

// NewProfileListener creates a new ProfilerListener with the given profilers.
// We currently support two profilers:
// - ProfilerCPU collects CPU usage samples based on stack counts
// - ProfilerMemory collects Memory usage based on well known allocation functions
func NewProfileListener(profilers ...Profiler) *ProfilerListener {
	profileFns := map[string]ProfileFunction{}
	samplerFns := make([]Sampler, 0, len(profilers))

	for i, p := range profilers {
		fns := p.Register()
		for name, f := range fns {
			profileFns[keyFor(i, name)] = f
		}
		samplerFns = append(samplerFns, p.Sampler())
	}

	return &ProfilerListener{
		profileFns: profileFns,
		samplerFns: samplerFns,
		hooks:      make(map[string]*hook),
		profilers:  profilers,
		samples:    list.New(),
		samplesMu:  sync.RWMutex{},
	}
}

// Register registers the ProfilerListener to the Wazero context.
// See: https://github.com/tetratelabs/wazero/blob/c82ad896f6019708f22ddd7826ff47319b7a1e54/experimental/listener.go#L27-L30
func (p *ProfilerListener) Register(ctx context.Context) context.Context {
	return context.WithValue(ctx, experimental.FunctionListenerFactoryKey{}, p)
}

type stackEntry struct {
	fn api.FunctionDefinition
	pc uint64
}

type sample struct {
	stack  []stackEntry
	values []int64
}

func (p *ProfilerListener) report(si experimental.StackIterator, values []int64) {
	sample := sample{
		stack:  make([]stackEntry, 0, p.lastStackSize+1),
		values: values,
	}
	for si.Next() {
		fn := si.FunctionDefinition()
		pc := si.SourceOffset()
		sample.stack = append(sample.stack, stackEntry{fn: fn, pc: pc})
	}
	p.samplesMu.Lock()
	if p.samples == nil {
		p.samples = list.New()
	}
	p.samples.PushBack(sample)
	if p.samples.Len() >= DefaultMaxStacksCount {
		e := p.samples.Front()
		p.samples.Remove(e)
	}
	p.samplesMu.Unlock()
	p.lastStackSize = len(sample.stack)
}

// BuildProfile builds a pprof Profile from the collected samples. After collection all samples are cleared.
func (p *ProfilerListener) BuildProfile() *profile.Profile {
	prof := &profile.Profile{
		Function:   []*profile.Function{},
		SampleType: []*profile.ValueType{},
	}

	for _, p := range p.profilers {
		t := p.SampleType()
		prof.SampleType = append(prof.SampleType, &t)
	}

	type entry struct { // TODO: this is literaly profile.Sample, use that instead
		counts    []int64
		locations []*profile.Location
	}

	counters := make(map[uint64]entry)
	bx := make([]byte, 8)

	p.samplesMu.Lock()
	for e := p.samples.Front(); e != nil; e = e.Next() {
		locations := []*profile.Location{}
		h := xxhash.New() // TODO: create once and reset?

		s := e.Value.(sample)
		for _, f := range s.stack {
			// TODO: when known, f.pc may be enough
			// instead of using name.
			binary.LittleEndian.PutUint64(bx, f.pc)
			h.Write([]byte(f.fn.Name()))
			h.Write(bx)
			locations = append(locations, p.locationForCall(prof, f))
		}

		sum64 := h.Sum64()
		e, ok := counters[sum64]
		if !ok {
			e = entry{
				counts:    make([]int64, len(s.values)),
				locations: locations,
			}
			counters[sum64] = e
		}
		for i, c := range s.values {
			e.counts[i] += c
		}
	}
	p.samples = nil
	p.samplesMu.Unlock()

	for _, count := range counters {
		prof.Sample = append(prof.Sample, &profile.Sample{
			Value:    count.counts,
			Location: count.locations,
		})
	}

	return prof
}

// Write writes the collected samples to the given writer.
func (p *ProfilerListener) Write(w io.Writer) error {
	prof := p.BuildProfile()
	return prof.Write(w)
}

func (p *ProfilerListener) locationForCall(prof *profile.Profile, f stackEntry) *profile.Location {
	if p.locCache == nil {
		p.locCache = map[pprofLocationKey]*profile.Location{}
	}

	locKey := pprofLocationKey{
		Module: f.fn.ModuleName(),
		Index:  f.fn.Index(),
		Name:   f.fn.Name(),
		PC:     f.pc,
	}

	if loc, ok := p.locCache[locKey]; ok {

		return loc
	}

	// Cache miss. Get or create function and all the line
	// locations associated with inlining.

	var locations []location
	if p.mapper != nil && f.pc > 0 {
		locations = p.mapper.Lookup(f.pc)
	}
	if len(locations) == 0 {
		// If we don't have a source location, attach to a
		// generic location whithin the function.
		locations = []location{{}}
	}

	stableName := f.fn.ModuleName() + "." + f.fn.Name()

	var pprofFn *profile.Function
	for _, f := range prof.Function {
		if f.SystemName == stableName {
			pprofFn = f
			break
		}
	}
	if pprofFn == nil {
		pprofFn = &profile.Function{
			ID:         uint64(len(prof.Function)) + 1, // 0 is reserved by pprof
			Name:       f.fn.DebugName(),
			SystemName: stableName,
			Filename:   locations[0].File,
		}
		prof.Function = append(prof.Function, pprofFn)
	} else {
		if pprofFn.Filename == "" && locations[0].File != "" {
			pprofFn.Filename = locations[0].File
		}
	}

	lines := make([]profile.Line, len(locations))
	for i, s := range locations {
		lines[len(locations)-i-1] = profile.Line{
			Function: pprofFn,
			Line:     s.Line,
		}
	}

	loc := &profile.Location{
		ID:      uint64(len(prof.Location)) + 1, // 0 reserved by pprof
		Line:    lines,
		Address: locations[0].PC,
	}
	prof.Location = append(prof.Location, loc)
	p.locCache[locKey] = loc

	return loc
}

// NewListener implements experimental.FunctionListenerFactory.
func (p *ProfilerListener) NewListener(def api.FunctionDefinition) experimental.FunctionListener {
	funcNames := make([]string, len(p.profilers))

	some := false
	for i, profiler := range p.profilers {
		funcNames[i] = profiler.Listen(def.Name())
		some = true
	}

	if !some {
		return nil
	}

	hookKey := strings.Join(funcNames, "|")

	if h, ok := p.hooks[hookKey]; ok {
		return h
	}

	h := &hook{
		profiler: p,
		fns:      make([]ProfileFunction, len(p.profilers)),
		samplers: make([]Sampler, len(p.profilers)),
		values:   make([]int64, len(p.profilers)),
	}

	for i, name := range funcNames {
		if name == "" {
			continue
		}
		h.fns[i] = p.profileFns[keyFor(i, name)]
		h.samplers[i] = p.samplerFns[i]
	}

	p.hooks[hookKey] = h

	return h
}

type hook struct {
	profiler *ProfilerListener
	samplers []Sampler
	fns      []ProfileFunction
	values   []int64
}

// Before implements experimental.FunctionListener.
func (h *hook) Before(ctx context.Context, mod api.Module, fnd api.FunctionDefinition, params []uint64, si experimental.StackIterator) context.Context {
	imod := mod.(experimental.InternalModule) // TODO: remove those casts by changing api.FunctionDefinition
	globals := imod.ViewGlobals()
	mem := mod.Memory()
	any := false
	for i, sampler := range h.samplers {
		if sampler == nil {
			continue
		}
		if sampler.Do() {
			h.values[i] = h.fns[i](params, globals, mem)
			any = true
		} else {
			h.values[i] = 0
		}
	}
	if any {
		h.profiler.report(si, h.values)
	}
	return ctx
}

// After implements experimental.FunctionListener.
func (h *hook) After(ctx context.Context, mod api.Module, fnd api.FunctionDefinition, err error, results []uint64) {
	// not used
}
