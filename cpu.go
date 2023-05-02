package wzprof

import (
	"time"

	"github.com/google/pprof/profile"
	"github.com/tetratelabs/wazero/api"
)

// ProfilerCPU is a sampled based CPU profiler. This profiler is counting
// the stacks for each functions. The sampling rate is between 0.0 and 1.0.
//
// CPU samples profiles provide a good overview of the CPU usage of a program.
// while keeping the overhead low.
type ProfilerCPU struct {
	// Sampling rate between 0.0 and 1.0
	Sampling float32
}

func NewProfilerCPU(sampling float32) *ProfilerCPU {
	return &ProfilerCPU{Sampling: sampling}
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
