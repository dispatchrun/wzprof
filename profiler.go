package wzprof

import (
	"container/list"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/cespare/xxhash"
	"github.com/google/pprof/profile"
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

	hooks map[string]*hook

	lastStackSize int
	samples       *list.List
	samplesMu     sync.RWMutex
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

type sample struct {
	stack  []api.FunctionDefinition
	values []int64
}

func (p *ProfilerListener) report(si experimental.StackIterator, values []int64) {
	sample := sample{
		stack:  make([]api.FunctionDefinition, 0, p.lastStackSize+1),
		values: values,
	}
	for si.Next() {
		fn := si.FunctionDefinition()
		sample.stack = append(sample.stack, fn)
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

	p.samplesMu.Lock()
	for e := p.samples.Front(); e != nil; e = e.Next() {
		locations := []*profile.Location{}
		h := xxhash.New()

		s := e.Value.(sample)
		for _, f := range s.stack {
			h.Write([]byte(f.DebugName()))
			locations = append(locations, locationForCall(prof, f.ModuleName(), f.Index(), f.DebugName()))
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

func locationForCall(p *profile.Profile, moduleName string, index uint32, name string) *profile.Location {
	// so far, 1 location = 1 function
	key := fmt.Sprintf("%s.%s[%d]", moduleName, name, index)

	for _, loc := range p.Location {
		if loc.Line[0].Function.SystemName == key {
			return loc
		}
	}

	fn := &profile.Function{
		ID:         uint64(len(p.Function)) + 1, // 0 is reserved by pprof
		Name:       fmt.Sprintf("%s.%s", moduleName, name),
		SystemName: key,
	}
	p.Function = append(p.Function, fn)

	loc := &profile.Location{
		ID: uint64(len(p.Location)) + 1, // 0 is reserved by pprof
		Line: []profile.Line{{
			Function: fn,
		}},
	}
	p.Location = append(p.Location, loc)
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
