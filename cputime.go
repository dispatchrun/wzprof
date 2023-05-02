package wzprof

import (
	"time"

	"github.com/google/pprof/profile"
	"github.com/tetratelabs/wazero/api"
)

// ProfileProcessor records the active CPU time of the WASM guest.
// This profiler doesn't take into account the time "off-cpu" (e.g. waiting for I/O).
// Here, all host-functions are considered "off-cpu" from a guest perspective.
type ProfilerCPUTime struct {
	// Sampling rate between 0.0 and 1.0.
	Sampling float32
}

func NewProfilerCPUTime(sampling float32) *ProfilerCPUTime {
	return &ProfilerCPUTime{Sampling: sampling}
}

type cputime struct{}

func (p cputime) Before(mod api.Module, params []uint64) int64 {
	return time.Now().UnixNano()
}

func (p cputime) After(in int64, results []uint64) int64 {
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

// PostProcess removes all samples that are "off-cpu" aka all time spent executing host-functions.
func (p *ProfilerCPUTime) PostProcess(prof *profile.Profile, idx int, offLocations []*profile.Location) {
	for _, sample := range prof.Sample {
		for _, off := range offLocations {
			processSample(prof, sample, off)
		}
	}
}

func processSample(prof *profile.Profile, s *profile.Sample, off *profile.Location) {
	match := false
	for _, loc := range s.Location {
		if loc == off {
			match = true
			break
		}
	}

	if match {
		for _, loc := range s.Location {
			for _, sample := range prof.Sample {
				if loc == sample.Location[0] {
					sample.Value[0] = 0
				}
			}
		}
	}
}

var _ Profiler = &ProfilerCPUTime{}
