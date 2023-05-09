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
	mutex sync.Mutex
	alloc stackCounterMap
	inuse map[uint32]memoryAllocation
	start time.Time
}

// MemoryProfilerOption is a type used to represent configuration options for
// MemoryProfiler instances created by NewMemoryProfiler.
type MemoryProfilerOption func(*MemoryProfiler)

type memoryAllocation struct {
	*stackCounter
	size uint32
}

// NewMemoryProfiler constructs a new instance of MemoryProfiler using the given
// time function to record the profile execution time.
func NewMemoryProfiler(opts ...MemoryProfilerOption) *MemoryProfiler {
	p := &MemoryProfiler{
		alloc: make(stackCounterMap),
		inuse: make(map[uint32]memoryAllocation),
		start: time.Now(),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// NewProfile takes a snapshot of the current memory allocation state and builds
// a profile representing the state of the program memory.
func (p *MemoryProfiler) NewProfile(sampleRate float64, symbols Symbolizer) *profile.Profile {
	return buildProfile(sampleRate, symbols, p.snapshot(), p.start, time.Since(p.start),
		[]*profile.ValueType{
			{Type: "alloc_objects", Unit: "count"},
			{Type: "alloc_space", Unit: "byte"},
			{Type: "inuse_objects", Unit: "count"},
			{Type: "inuse_space", Unit: "byte"},
		},
	)
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
func (p *MemoryProfiler) NewHandler(sampleRate float64, symbols Symbolizer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveProfile(w, p.NewProfile(sampleRate, symbols))
	})
}

// NewFunctionListener returns a function listener suited to install a hook on
// functions responsible for memory allocation.
//
// The listener recognizes multiple memory alloction functions used by compilers
// and libraries. It uses the function name to detect memory allocators,
// currently supporting libc, Go, and TinyGo.
func (p *MemoryProfiler) NewFunctionListener(def api.FunctionDefinition) experimental.FunctionListener {
	switch def.Name() {
	// C standard library, Rust
	case "malloc":
		return &mallocProfiler{memory: p}
	case "calloc":
		return &callocProfiler{memory: p}
	case "realloc":
		return &reallocProfiler{memory: p}
	case "free":
		return &freeProfiler{memory: p}

	// Go
	case "runtime.mallocgc":
		return &goRuntimeMallocgcProfiler{memory: p}

	// TinyGo
	case "runtime.alloc":
		return &mallocProfiler{memory: p}

	default:
		return nil
	}
}

func (p *MemoryProfiler) observeAlloc(addr, size uint32, stack stackTrace) {
	p.mutex.Lock()
	alloc := p.alloc.lookup(stack)
	alloc.observe(int64(size))
	p.inuse[addr] = memoryAllocation{alloc, size}
	p.mutex.Unlock()
}

func (p *MemoryProfiler) observeFree(addr uint32) {
	p.mutex.Lock()
	delete(p.inuse, addr)
	p.mutex.Unlock()
}

type mallocProfiler struct {
	memory *MemoryProfiler
	size   uint32
	stack  stackTrace
}

func (p *mallocProfiler) Before(ctx context.Context, mod api.Module, def api.FunctionDefinition, params []uint64, si experimental.StackIterator) context.Context {
	p.size = api.DecodeU32(params[0])
	p.stack = makeStackTrace(p.stack, si)
	return ctx
}

func (p *mallocProfiler) After(ctx context.Context, mod api.Module, def api.FunctionDefinition, err error, results []uint64) {
	if err == nil {
		p.memory.observeAlloc(api.DecodeU32(results[0]), p.size, p.stack)
	}
}

type callocProfiler struct {
	memory *MemoryProfiler
	count  uint32
	size   uint32
	stack  stackTrace
}

func (p *callocProfiler) Before(ctx context.Context, mod api.Module, def api.FunctionDefinition, params []uint64, si experimental.StackIterator) context.Context {
	p.count = api.DecodeU32(params[0])
	p.size = api.DecodeU32(params[1])
	p.stack = makeStackTrace(p.stack, si)
	return ctx
}

func (p *callocProfiler) After(ctx context.Context, mod api.Module, def api.FunctionDefinition, err error, results []uint64) {
	if err == nil {
		p.memory.observeAlloc(api.DecodeU32(results[0]), p.count*p.size, p.stack)
	}
}

type reallocProfiler struct {
	memory *MemoryProfiler
	addr   uint32
	size   uint32
	stack  stackTrace
}

func (p *reallocProfiler) Before(ctx context.Context, mod api.Module, def api.FunctionDefinition, params []uint64, si experimental.StackIterator) context.Context {
	p.addr = api.DecodeU32(params[0])
	p.size = api.DecodeU32(params[1])
	p.stack = makeStackTrace(p.stack, si)
	return ctx
}

func (p *reallocProfiler) After(ctx context.Context, mod api.Module, def api.FunctionDefinition, err error, results []uint64) {
	if err == nil {
		p.memory.observeFree(p.addr)
		p.memory.observeAlloc(api.DecodeU32(results[0]), p.size, p.stack)
	}
}

type freeProfiler struct {
	memory *MemoryProfiler
	addr   uint32
}

func (p *freeProfiler) Before(ctx context.Context, mod api.Module, def api.FunctionDefinition, params []uint64, si experimental.StackIterator) context.Context {
	p.addr = api.DecodeU32(params[0])
	return ctx
}

func (p *freeProfiler) After(ctx context.Context, mod api.Module, def api.FunctionDefinition, err error, results []uint64) {
	if err == nil {
		p.memory.observeFree(p.addr)
	}
}

type goRuntimeMallocgcProfiler struct {
	memory *MemoryProfiler
	size   uint32
	stack  stackTrace
}

func (p *goRuntimeMallocgcProfiler) Before(ctx context.Context, mod api.Module, def api.FunctionDefinition, params []uint64, si experimental.StackIterator) context.Context {
	imod := mod.(experimental.InternalModule)
	mem := imod.Memory()

	sp := int32(imod.Global(0).Get())
	offset := sp + 8*(int32(0)+1) // +1 for the return address
	b, ok := mem.Read(uint32(offset), 8)
	if ok {
		p.size = binary.LittleEndian.Uint32(b)
		p.stack = makeStackTrace(p.stack, si)
	} else {
		p.size = 0
	}
	return ctx
}

func (p *goRuntimeMallocgcProfiler) After(ctx context.Context, mod api.Module, def api.FunctionDefinition, err error, results []uint64) {
	if err == nil && p.size != 0 {
		// TODO: get the returned pointer
		addr := uint32(0)
		p.memory.observeAlloc(addr, p.size, p.stack)
	}
}
