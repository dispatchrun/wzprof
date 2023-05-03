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
	"encoding/binary"
	"fmt"

	"github.com/google/pprof/profile"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

// ProfilerMemory instruments known allocator functions for memory
// allocations (alloc_space).
type ProfilerMemory struct{}

type profileStack0int32 struct{}

func (p profileStack0int32) Before(mod api.Module, params []uint64) int64 {
	return int64(int32(params[0]))
}
func (p profileStack0int32) After(in int64, results []uint64) int64 {
	return in
}

type profileStackCalloc struct{}

func (p profileStackCalloc) Before(mod api.Module, params []uint64) int64 {
	return int64(int32(params[0])) * int64(int32(params[1]))
}

func (profileStackCalloc) After(in int64, results []uint64) int64 {
	return in
}

type profileStack1int32 struct{}

func (p profileStack1int32) Before(mod api.Module, params []uint64) int64 {
	return int64(int32(params[1]))
}

func (p profileStack1int32) After(in int64, results []uint64) int64 {
	return in
}

type profileGoStack0int32 struct{}

func (p profileGoStack0int32) Before(mod api.Module, params []uint64) int64 {
	imod := mod.(experimental.InternalModule)
	mem := imod.Memory()

	sp := int32(imod.Global(0).Get())
	offset := sp + 8*(int32(0)+1) // +1 for the return address
	b, ok := mem.Read(uint32(offset), 8)
	if !ok {
		panic(fmt.Sprintf("could not read go stack entry at offset %d", offset))
	}
	v := binary.LittleEndian.Uint64(b)
	return int64(v)
}

func (p profileGoStack0int32) After(in int64, results []uint64) int64 {
	return in
}

func (p *ProfilerMemory) Register() map[string]ProfileProcessor {
	return map[string]ProfileProcessor{
		"profileStack0int32":   profileStack0int32{},
		"profileStack1int32":   profileStack1int32{},
		"profileStackCalloc":   profileStackCalloc{},
		"profileGoStack0int32": profileGoStack0int32{},
	}
}

func (p *ProfilerMemory) Listen(name string) string {
	switch name {
	// C standard library, Rust
	case "malloc":
		return "profileStack0int32"
	case "calloc":
		return "profileStackCalloc"
	case "realloc":
		return "profileStack1int32"

	// Go
	case "runtime.mallocgc":
		return "profileGoStack0int32"

	// TinyGo
	case "runtime.alloc":
		return "profileStack0int32"

	default:
		return ""
	}
}

func (p *ProfilerMemory) SampleType() profile.ValueType {
	return profile.ValueType{Type: "alloc_space", Unit: "bytes"}
}

func (p *ProfilerMemory) Sampler() Sampler {
	return newAlwaysSampler()
}

func (p *ProfilerMemory) PostProcess(prof *profile.Profile, idx int, offLocations []*profile.Location) {
	// no post process for this profiler
}

var _ Profiler = &ProfilerMemory{}
