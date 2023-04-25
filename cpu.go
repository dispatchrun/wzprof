package wazeroprofiler

import (
	"context"
	"fmt"

	"github.com/cespare/xxhash"
	"github.com/google/pprof/profile"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

type CPUProfiler struct {
	sc      *stackCounters
	profile *profile.Profile
}

type stackCounters struct {
	counters  map[uint64]int64
	locations map[uint64][]*profile.Location
}

func NewCPUProfilerListener() *CPUProfiler {
	p := &profile.Profile{
		Function:   []*profile.Function{},
		SampleType: []*profile.ValueType{{Type: "cpu_samples", Unit: "count"}},
	}

	sc := &stackCounters{
		counters:  make(map[uint64]int64),
		locations: make(map[uint64][]*profile.Location),
	}

	return &CPUProfiler{
		sc:      sc,
		profile: p,
	}
}

func (p *CPUProfiler) Before(ctx context.Context, mod api.Module, fnd api.FunctionDefinition, params []uint64, si experimental.StackIterator, globals experimental.Globals) context.Context {
	p.populateProfile(si)
	return ctx
}

func (p *CPUProfiler) After(ctx context.Context, mod api.Module, fnd api.FunctionDefinition, err error, results []uint64) {
	// Not implemented
}

func (p *CPUProfiler) populateProfile(si experimental.StackIterator) {
	locations := []*profile.Location{}

	h := xxhash.New()
	for si.Next() {
		t := si.FnType()

		h.Write([]byte(t.DebugName()))

		locations = append(locations, locationForCall(p.profile, t.ModuleName(), t.Index(), t.Name()))
	}

	if c, ok := p.sc.counters[h.Sum64()]; ok {
		c++
		p.sc.counters[h.Sum64()] = c
		return
	}

	p.sc.counters[h.Sum64()] = 1
	p.sc.locations[h.Sum64()] = locations
}

func (p *CPUProfiler) updateSamples() {
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
