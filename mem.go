package wzprof

import (
	"encoding/binary"
	"fmt"

	"github.com/google/pprof/profile"
	"github.com/tetratelabs/wazero/api"
)

// ProfilerMemory instruments known allocator functions for memory
// allocations (alloc_space).
type ProfilerMemory struct{}

func profileStack0int32(params []uint64, globals []api.Global, mem api.Memory) int64 {
	return int64(int32(params[0]))
}

func profileStack1int32(params []uint64, globals []api.Global, mem api.Memory) int64 {
	return int64(int32(params[1]))
}

func profileGoStack0int32(params []uint64, globals []api.Global, mem api.Memory) int64 {
	// TODO: this assumes all values on the stack are 64bits. this is probably wrong.
	sp := int32(globals[0].Get())
	offset := sp + 8*(int32(0)+1) // +1 for the return address
	b, ok := mem.Read(uint32(offset), 8)
	if !ok {
		panic(fmt.Sprintf("could not read go stack entry at offset %d", offset))
	}
	v := binary.LittleEndian.Uint64(b)
	return int64(v)
}

func (p *ProfilerMemory) Register() map[string]ProfileFunction {
	return map[string]ProfileFunction{
		"profileStack0int32":   profileStack0int32,
		"profileStack1int32":   profileStack1int32,
		"profileGoStack0int32": profileGoStack0int32,
	}
}

func (p *ProfilerMemory) Listen(name string) string {
	switch name {
	// C standard library, Rust
	case "malloc":
		return "profileStack0int32"
	case "calloc":
		return "profileStack0int32"
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

var _ Profiler = &ProfilerMemory{}
