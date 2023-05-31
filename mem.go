package wzprof

import (
	"context"
	"encoding/binary"
	"net/http"
	"sync"
	"time"

	"github.com/google/pprof/profile"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

// MemoryProfiler is the implementation of a performance profiler recording
// samples of memory allocation and utilization.
//
// The profiler generates the following samples:
// - "alloc_objects" records the locations where objects are allocated
// - "alloc_space"   records the locations where bytes are allocated
// - "inuse_objects" records the allocation of active objects
// - "inuse_space"   records the bytes used by active objects
//
// "alloc_objects" and "alloc_space" are all time counters since the start of
// the program, while "inuse_objects" and "inuse_space" capture the current state
// of the program at the time the profile is taken.
type MemoryProfiler struct {
	p     *Profiling
	mutex sync.Mutex
	alloc stackCounterMap
	inuse map[uint32]memoryAllocation
	start time.Time
}

// MemoryProfilerOption is a type used to represent configuration options for
// MemoryProfiler instances created by NewMemoryProfiler.
type MemoryProfilerOption func(*MemoryProfiler)

// InuseMemory is a memory profiler option which enables tracking of allocated
// and freed objects to generate snapshots of the current state of a program
// memory.
func InuseMemory(enable bool) MemoryProfilerOption {
	return func(p *MemoryProfiler) {
		if enable {
			p.inuse = make(map[uint32]memoryAllocation)
		}
	}
}

type memoryAllocation struct {
	*stackCounter
	size uint32
}

// newMemoryProfiler constructs a new instance of MemoryProfiler using the given
// time function to record the profile execution time.
func newMemoryProfiler(p *Profiling, options ...MemoryProfilerOption) *MemoryProfiler {
	m := &MemoryProfiler{
		p:     p,
		alloc: make(stackCounterMap),
		start: time.Now(),
	}
	for _, opt := range options {
		opt(m)
	}
	return m
}

// NewProfile takes a snapshot of the current memory allocation state and builds
// a profile representing the state of the program memory.
func (p *MemoryProfiler) NewProfile(sampleRate float64) *profile.Profile {
	ratio := 1 / sampleRate
	return buildProfile(p.p, p.snapshot(), p.start, time.Since(p.start), p.SampleType(),
		[]float64{ratio, ratio, ratio, ratio},
	)
}

// Name returns "allocs" to match the name of the memory profiler in pprof.
func (p *MemoryProfiler) Name() string {
	return "allocs"
}

// Desc returns a description copied from net/http/pprof.
func (p *MemoryProfiler) Desc() string {
	return profileDescriptions[p.Name()]
}

// Count returns the number of allocation stacks recorded in p.
func (p *MemoryProfiler) Count() int {
	p.mutex.Lock()
	n := p.alloc.len()
	p.mutex.Unlock()
	return n
}

// SampleType returns the set of value types present in samples recorded by the
// memory profiler.
func (p *MemoryProfiler) SampleType() []*profile.ValueType {
	sampleType := []*profile.ValueType{
		{Type: "alloc_objects", Unit: "count"},
		{Type: "alloc_space", Unit: "byte"},
	}

	if p.inuse != nil {
		// TODO: when can track freeing of garbage collected languages like Go,
		// this should be enabled by default, and we can remove the slicing of
		// sample values in buildProfile.
		sampleType = append(sampleType,
			&profile.ValueType{Type: "inuse_objects", Unit: "count"},
			&profile.ValueType{Type: "inuse_space", Unit: "byte"},
		)
	}

	return sampleType
}

type memorySample struct {
	stack stackTrace
	value [4]int64 // allocCount, allocBytes, inuseCount, inuseBytes
}

func (m *memorySample) sampleLocation() stackTrace {
	return m.stack
}

func (m *memorySample) sampleValue() []int64 {
	return m.value[:]
}

func (p *MemoryProfiler) snapshot() map[uint64]*memorySample {
	// We hold an exclusive lock while getting a snapshot of the profiler state.
	// This will block concurrent calls to malloc/free/etc... We accept the cost
	// since it only happens when the memory profile is captured, and memory
	// allocation is generally accepted as being a potentially costly operation.
	p.mutex.Lock()
	defer p.mutex.Unlock()

	samples := make(map[uint64]*memorySample, len(p.alloc))

	for _, alloc := range p.alloc {
		p := samples[alloc.stack.key]
		if p == nil {
			p = &memorySample{stack: alloc.stack}
			samples[alloc.stack.key] = p
		}
		p.value[0] += alloc.count()
		p.value[1] += alloc.total()
	}

	for _, inuse := range p.inuse {
		p := samples[inuse.stack.key]
		p.value[2] += 1
		p.value[3] += int64(inuse.size)
	}

	return samples
}

// NewHandler returns a http handler allowing the profiler to be exposed on a
// pprof-compatible http endpoint.
//
// The sample rate is a value between 0 and 1 used to scale the profile results
// based on the sampling rate applied to the profiler so the resulting values
// remain representative.
//
// The symbolizer passed as argument is used to resolve names of program
// locations recorded in the profile.
func (p *MemoryProfiler) NewHandler(sampleRate float64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveProfile(w, p.NewProfile(sampleRate))
	})
}

// NewFunctionListener returns a function listener suited to install a hook on
// functions responsible for memory allocation.
//
// The listener recognizes multiple memory allocation functions used by
// compilers and libraries. It uses the function name to detect memory
// allocators, currently supporting libc, Go, and TinyGo.
func (p *MemoryProfiler) NewFunctionListener(def api.FunctionDefinition) experimental.FunctionListener {
	switch def.Name() {
	// C standard library, Rust
	case "malloc":
		return profilingListener{p.p, &mallocProfiler{memory: p}}
	case "calloc":
		return profilingListener{p.p, &callocProfiler{memory: p}}
	case "realloc":
		return profilingListener{p.p, &reallocProfiler{memory: p}}
	case "free":
		return profilingListener{p.p, &freeProfiler{memory: p}}

	// Go
	case "runtime.mallocgc":
		return profilingListener{p.p, &goRuntimeMallocgcProfiler{memory: p}}

	// TinyGo
	case "runtime.alloc":
		return profilingListener{p.p, &mallocProfiler{memory: p}}

	default:
		return nil
	}
}

func (p *MemoryProfiler) observeAlloc(addr, size uint32, stack stackTrace) {
	p.mutex.Lock()
	alloc := p.alloc.lookup(stack)
	alloc.observe(int64(size))
	if p.inuse != nil {
		p.inuse[addr] = memoryAllocation{alloc, size}
	}
	p.mutex.Unlock()
}

func (p *MemoryProfiler) observeFree(addr uint32) {
	if p.inuse != nil {
		p.mutex.Lock()
		delete(p.inuse, addr)
		p.mutex.Unlock()
	}
}

type mallocProfiler struct {
	memory *MemoryProfiler
	size   uint32
	stack  stackTrace
}

func (p *mallocProfiler) Before(ctx context.Context, mod api.Module, def api.FunctionDefinition, params []uint64, si experimental.StackIterator) {
	p.size = api.DecodeU32(params[0])
	p.stack = makeStackTrace(p.stack, si)
}

func (p *mallocProfiler) After(ctx context.Context, mod api.Module, def api.FunctionDefinition, results []uint64) {
	p.memory.observeAlloc(api.DecodeU32(results[0]), p.size, p.stack)
}

func (p *mallocProfiler) Abort(ctx context.Context, mod api.Module, def api.FunctionDefinition, _ error) {
}

type callocProfiler struct {
	memory *MemoryProfiler
	count  uint32
	size   uint32
	stack  stackTrace
}

func (p *callocProfiler) Before(ctx context.Context, mod api.Module, def api.FunctionDefinition, params []uint64, si experimental.StackIterator) {
	p.count = api.DecodeU32(params[0])
	p.size = api.DecodeU32(params[1])
	p.stack = makeStackTrace(p.stack, si)
}

func (p *callocProfiler) After(ctx context.Context, mod api.Module, def api.FunctionDefinition, results []uint64) {
	p.memory.observeAlloc(api.DecodeU32(results[0]), p.count*p.size, p.stack)
}

func (p *callocProfiler) Abort(ctx context.Context, mod api.Module, def api.FunctionDefinition, _ error) {
}

type reallocProfiler struct {
	memory *MemoryProfiler
	addr   uint32
	size   uint32
	stack  stackTrace
}

func (p *reallocProfiler) Before(ctx context.Context, mod api.Module, def api.FunctionDefinition, params []uint64, si experimental.StackIterator) {
	p.addr = api.DecodeU32(params[0])
	p.size = api.DecodeU32(params[1])
	p.stack = makeStackTrace(p.stack, si)
}

func (p *reallocProfiler) After(ctx context.Context, mod api.Module, def api.FunctionDefinition, results []uint64) {
	p.memory.observeFree(p.addr)
	p.memory.observeAlloc(api.DecodeU32(results[0]), p.size, p.stack)
}

func (p *reallocProfiler) Abort(ctx context.Context, mod api.Module, def api.FunctionDefinition, _ error) {
}

type freeProfiler struct {
	memory *MemoryProfiler
	addr   uint32
}

func (p *freeProfiler) Before(ctx context.Context, mod api.Module, def api.FunctionDefinition, params []uint64, si experimental.StackIterator) {
	p.addr = api.DecodeU32(params[0])
}

func (p *freeProfiler) After(ctx context.Context, mod api.Module, def api.FunctionDefinition, _ []uint64) {
	p.memory.observeFree(p.addr)
}

func (p *freeProfiler) Abort(ctx context.Context, mod api.Module, def api.FunctionDefinition, _ error) {
	p.After(ctx, mod, def, nil)
}

type goRuntimeMallocgcProfiler struct {
	memory *MemoryProfiler
	size   uint32
	stack  stackTrace
}

func (p *goRuntimeMallocgcProfiler) Before(ctx context.Context, mod api.Module, def api.FunctionDefinition, params []uint64, wasmsi experimental.StackIterator) {
	imod := mod.(experimental.InternalModule)
	mem := imod.Memory()

	sp := uint32(imod.Global(0).Get())
	offset := sp + 8*(uint32(0)+1) // +1 for the return address
	b, ok := mem.Read(offset, 8)
	if ok {
		p.size = binary.LittleEndian.Uint32(b)
		p.stack = makeStackTrace(p.stack, wasmsi)
	} else {
		p.size = 0
	}
}

func (p *goRuntimeMallocgcProfiler) After(ctx context.Context, mod api.Module, def api.FunctionDefinition, _ []uint64) {
	if p.size != 0 {
		// TODO: get the returned pointer
		addr := uint32(0)
		p.memory.observeAlloc(addr, p.size, p.stack)
	}
}

func (p *goRuntimeMallocgcProfiler) Abort(ctx context.Context, mod api.Module, def api.FunctionDefinition, _ error) {
	p.After(ctx, mod, def, nil)
}
