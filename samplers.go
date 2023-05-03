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

import "math/rand"

// Sampler is provided to Profilers to decide how often the profiler
// instrumentation needs to be reported.
type Sampler interface {
	// Do returns true if instrumentation needs to be performed.
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

type alwaysSampler struct{}

func (s *alwaysSampler) Do() bool {
	return true
}

func newAlwaysSampler() *alwaysSampler {
	return &alwaysSampler{}
}
