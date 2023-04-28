package wzprof

import (
	"time"

	"github.com/google/pprof/profile"
	"github.com/tetratelabs/wazero/api"
)

// ProfilerCPU instruments function calls for cpu_samples.
type ProfilerCPUTime struct {
	// Sampling rate between 0.0 and 0.2.
	Sampling float32
}

type cputime struct{}

func (p cputime) PreFunction(params []uint64, globals []api.Global, mem api.Memory) int64 {
	return time.Now().UnixNano()
}

func (p cputime) PostFunction(in int64, results []uint64) int64 {
	return time.Now().UnixNano() - in
}

func (p *ProfilerCPUTime) Register() map[string]ProfileProcessor {
	return map[string]ProfileProcessor{
		"time": cputime{},
	}
}

func (p *ProfilerCPUTime) Listen(name string) string {
	return "time"
}

func (p *ProfilerCPUTime) SampleType() profile.ValueType {
	return profile.ValueType{Type: "cpu_time", Unit: "nanosecond"}
}

func (p *ProfilerCPUTime) Sampler() Sampler {
	return newRandomSampler(time.Now().UnixNano(), p.Sampling)
}

var _ Profiler = &ProfilerCPU{}
