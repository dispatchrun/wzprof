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
