package wazeroprofiler

import (
	"container/list"
	"context"
	"fmt"
	"io"
	"math/rand"
	"time"

	"github.com/cespare/xxhash"
	"github.com/google/pprof/profile"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

const DefaultMaxStacksCount = 250000
const DefaultSampling = 0.2

type CPUProfiler struct {
	stacks  *list.List
	sc      *stackCounters
	profile *profile.Profile
	sampler Sampler

	buf []*api.FunctionDefinition
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
		sampler: newRandomSampler(time.Now().UnixNano(), DefaultSampling),
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

func (p *CPUProfiler) Before(ctx context.Context, mod api.Module, fnd api.FunctionDefinition, params []uint64, si experimental.StackIterator) context.Context {
	if p.sampler.Do() {
		p.walk(si)
	}
	return ctx
}

func (p *CPUProfiler) After(ctx context.Context, mod api.Module, fnd api.FunctionDefinition, err error, results []uint64) {
	// Not implemented
}

func (p *CPUProfiler) walk(si experimental.StackIterator) {
	s := p.buf
	if s == nil {
		s = make([]*api.FunctionDefinition, 50)
	}

	i := 0
	for si.Next() {
		if i >= len(s) {
			b := make([]*api.FunctionDefinition, len(s)*2)
			copy(b, s)
			s = b
		}
		fn := si.FunctionDefinition()
		s[i] = &fn
		i++

	}

	p.stacks.PushBack(s[:i])
	if p.stacks.Len() >= DefaultMaxStacksCount {
		e := p.stacks.Front()
		p.stacks.Remove(e)
	}
}

func (p *CPUProfiler) consumeStacks() {
	for e := p.stacks.Front(); e != nil; e = e.Next() {
		locations := []*profile.Location{}
		h := xxhash.New()

		s := e.Value.([]*api.FunctionDefinition)
		for _, f := range s {
			h.Write([]byte((*f).DebugName()))
			locations = append(locations, locationForCall(p.profile, (*f).ModuleName(), (*f).Index(), (*f).Name()))
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

type Sampler interface {
	Do() bool
}

type randomSampler struct {
	rand   *rand.Rand
	chance float32
}

func newRandomSampler(seed int64, chance float32) *randomSampler {
	return &randomSampler{
		rand:   rand.New(rand.NewSource(seed)),
		chance: chance,
	}
}

func (s *randomSampler) Do() bool {
	return s.rand.Float32() < s.chance
}
