//  Copyright 2023 Stealth Rocket, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

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

	// If true, the profiler will account for time spent in host-functions.
	IncludeIO bool
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
			p.processSample(prof, sample, off)
		}
	}
}

func (p *ProfilerCPUTime) processSample(prof *profile.Profile, s *profile.Sample, off *profile.Location) {
	if p.IncludeIO {
		return
	}

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
