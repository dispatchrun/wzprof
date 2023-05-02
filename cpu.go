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

type cpuprocessor struct{}

func (p cpuprocessor) Before(mod api.Module, params []uint64) int64 {
	return 1
}

func (p cpuprocessor) After(in int64, results []uint64) int64 {
	return in
}

func (p *ProfilerCPU) Register() map[string]ProfileProcessor {
	return map[string]ProfileProcessor{
		"count1": cpuprocessor{},
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

func (p *ProfilerCPU) PostProcess(prof *profile.Profile, idx int, offLocations []*profile.Location) {
	// no post process for this profiler
}

var _ Profiler = &ProfilerCPU{}
