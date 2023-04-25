package wazeroprofiler

import (
	"container/list"
	"context"
	"fmt"
	"io"

	"github.com/cespare/xxhash"
	"github.com/google/pprof/profile"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

type CPUProfiler struct {
	stacks  *list.List
	sc      *stackCounters
	profile *profile.Profile
}

type stackCounters struct {
	counters  map[uint64]int64
	locations map[uint64][]*profile.Location
}

func NewCPUProfiler() *CPUProfiler {
	p := &profile.Profile{
		Function:   []*profile.Function{},
		SampleType: []*profile.ValueType{{Type: "cpu_samples", Unit: "count"}},
	}

	sc := &stackCounters{
		counters:  make(map[uint64]int64),
		locations: make(map[uint64][]*profile.Location),
	}

	return &CPUProfiler{
		stacks:  list.New(),
		sc:      sc,
		profile: p,
	}
}

func (p *CPUProfiler) Register(ctx context.Context) context.Context {
	return context.WithValue(ctx, experimental.FunctionListenerFactoryKey{}, p)
}

func (p *CPUProfiler) Write(w io.Writer) error {
	p.collectSamples()

	return p.profile.Write(w)
}

func (p *CPUProfiler) NewListener(def api.FunctionDefinition) experimental.FunctionListener {
	return p
}

func (p *CPUProfiler) Before(ctx context.Context, mod api.Module, fnd api.FunctionDefinition, params []uint64, si experimental.StackIterator, globals experimental.Globals) context.Context {
	p.walk(si)
	return ctx
}

func (p *CPUProfiler) After(ctx context.Context, mod api.Module, fnd api.FunctionDefinition, err error, results []uint64) {
	// Not implemented
}

type funcDef struct {
	debugName  string
	moduleName string
	index      uint32
	name       string
}

type stack []funcDef

func (p *CPUProfiler) walk(si experimental.StackIterator) {
	s := stack{}
	for si.Next() {
		s = append(s, funcDef{
			debugName:  si.FnType().DebugName(),
			moduleName: si.FnType().ModuleName(),
			index:      si.FnType().Index(),
			name:       si.FnType().Name(),
		})
	}
	p.stacks.PushBack(s)
}

func (p *CPUProfiler) consumeStacks() {
	for e := p.stacks.Front(); e != nil; e = e.Next() {
		locations := []*profile.Location{}
		h := xxhash.New()

		s := e.Value.(stack)
		for _, f := range s {
			h.Write([]byte(f.debugName))
			locations = append(locations, locationForCall(p.profile, f.moduleName, f.index, f.name))
		}

		sum64 := h.Sum64()
		if c, ok := p.sc.counters[sum64]; ok {
			c++
			p.sc.counters[sum64] = c
			continue
		}

		p.sc.counters[sum64] = 1
		p.sc.locations[sum64] = locations
	}
}

func (p *CPUProfiler) collectSamples() {
	p.consumeStacks()

	//TODO: flush after collect?
	for si, count := range p.sc.counters {
		p.profile.Sample = append(p.profile.Sample, &profile.Sample{
			Value:    []int64{count},
			Location: p.sc.locations[si],
		})
	}
}

// took from memprofiler
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
