package wzprof

import (
	"time"

	"github.com/google/pprof/profile"
	"github.com/tetratelabs/wazero/api"
)

// ProfilerCPU instruments function calls for cpu_samples.
type ProfilerCPU struct {
	// Sampling rate between 0.0 and 0.2.
	Sampling float32
}

func count1(mod api.Module, params []uint64) int64 {
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
	return newRandomSampler(time.Now().UnixNano(), p.Sampling)
}

var _ Profiler = &ProfilerCPU{}
