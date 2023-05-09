package wzprof

import (
	"bytes"
	"fmt"
	"hash/maphash"
	"os"
	"strings"
	"time"
	"unsafe"

	"github.com/google/pprof/profile"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"golang.org/x/exp/slices"
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

type Symbolizer interface {
	// LocationsForPC returns a list of function locations for a given program
	// counter, starting from current function followed by the inlined
	// functions, in order of inlining. Result if empty if the pc cannot
	// be resolved in the dwarf data.
	LocationsForPC(pc uint64) []Location
}

type Location struct {
	File    string
	Line    int64
	Column  int64
	Inlined bool
	PC      uint64
	// Linkage Name if present, Name otherwise.
	// Only present for inlined functions.
	StableName string
	HumanName  string
}

func locationForCall(symbols Symbolizer, def api.FunctionDefinition, pc uint64, funcs map[string]*profile.Function) []profile.Line {
	// Cache miss. Get or create function and all the line
	// locations associated with inlining.
	var locations []Location
	var symbolFound bool

	if symbols != nil && pc > 0 {
		locations = symbols.LocationsForPC(pc)
		symbolFound = len(locations) > 0
	}
	if len(locations) == 0 {
		// If we don't have a source location, attach to a
		// generic location whithin the function.
		locations = []Location{{}}
	}
	// Provide defaults in case we couldn't resolve DWARF informations for
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

	return lines
}

type locationKey struct {
	module string
	index  uint32
	name   string
	pc     uint64
}

func makeLocationKey(fn api.FunctionDefinition, pc uint64) locationKey {
	return locationKey{
		module: fn.ModuleName(),
		index:  fn.Index(),
		name:   fn.Name(),
		pc:     pc,
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

func (sc *stackCounter) subtract(value int64) {
	if total := sc.total(); total < value {
		sc.value[1] = 0
	} else {
		sc.value[1] -= value
	}
}

func (sc *stackCounter) sampleLocation() stackTrace {
	return sc.stack
}

func (sc *stackCounter) sampleValue() []int64 {
	return sc.value[:]
}

type stackFrame struct {
	fn api.FunctionDefinition
	pc uint64
}

type stackTrace struct {
	fns []api.FunctionDefinition
	pcs []uint64
	key uint64
}

func makeStackTrace(st stackTrace, si experimental.StackIterator) stackTrace {
	st.fns = st.fns[:0]
	st.pcs = st.pcs[:0]
	for si.Next() {
		st.fns = append(st.fns, si.FunctionDefinition())
		st.pcs = append(st.pcs, si.SourceOffset())
	}
	st.key = maphash.Bytes(stackTraceHashSeed, st.bytes())
	return st
}

func (st stackTrace) host() bool {
	return len(st.fns) > 0 && st.fns[0].GoFunction() != nil
}

func (st stackTrace) contains(sx stackTrace) bool {
	if len(st.fns) < len(sx.fns) {
		return false
	}
	n := len(st.fns) - len(sx.fns)
	st.fns = st.fns[n:]
	st.pcs = st.pcs[n:]
	if st.fns[0] != sx.fns[0] {
		return false
	}
	st.pcs = st.pcs[1:]
	sx.pcs = sx.pcs[1:]
	return bytes.Equal(st.bytes(), sx.bytes())
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
		fmt.Fprintf(sb, "@%016x: %s\n", frame.pc, frame.fn.Name())
	}
	return sb.String()
}

var stackTraceHashSeed = maphash.MakeSeed()

type sampleType interface {
	sampleLocation() stackTrace
	sampleValue() []int64
}

func buildProfile[T sampleType](sampleRate float64, symbols Symbolizer, samples map[uint64]T, start time.Time, duration time.Duration, sampleType []*profile.ValueType) *profile.Profile {
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

			key := makeLocationKey(fn, pc)
			loc := locationCache[key]
			if loc == nil {
				loc = &profile.Location{
					ID:      locationID,
					Line:    locationForCall(symbols, fn, pc, functionCache),
					Address: pc,
				}
				locationID++
				locationCache[key] = loc
			}

			location[i] = loc
		}

		prof.Sample = append(prof.Sample, &profile.Sample{
			Location: location,
			Value:    sample.sampleValue(),
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

	if sampleRate < 1 {
		prof.Scale(1 / sampleRate)
	}
	return prof
}
