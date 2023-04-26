package wazeroprofiler

import (
	"time"

	"github.com/google/pprof/profile"
	"github.com/tetratelabs/wazero/api"
)

type ProfilerCPU struct{}

const DefaultCPUSampling = 0.2

func count1(params []uint64, globals []api.Global, mem api.Memory) int64 {
	return 1
}

func (p *ProfilerCPU) Register() map[string]ProfileFunction {
	return map[string]ProfileFunction{
		"count1": count1,
	}
}

func (p *ProfilerCPU) Listen(name string) string {
	return "count1"
}

func (p *ProfilerCPU) SampleType() profile.ValueType {
	return profile.ValueType{Type: "cpu_samples", Unit: "count"}
}

func (p *ProfilerCPU) Sampler() Sampler {
	return newRandomSampler(time.Now().UnixNano(), DefaultCPUSampling)
}
