package wzprof

import (
	"debug/dwarf"
	"encoding/binary"
	"fmt"
	"unsafe"

	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

// Heuristic to guess whether the wasm binary is actually CPython, based on its
// DWARF information.
//
// It loops over compile units to find one named "Programs/python.c". It should
// be fast since it's the first compile unit when we build CPython.
func guessPython(p dwarfparser) bool {
	for {
		ent, err := p.r.Next()
		if err != nil || ent == nil {
			break
		}
		if ent.Tag != dwarf.TagCompileUnit {
			p.r.SkipChildren()
			continue
		}
		name, _ := ent.Val(dwarf.AttrName).(string)
		if name == "Programs/python.c" {
			return true
		}
		p.r.SkipChildren()
	}
	return false
}

type python struct {
	dwarf    dwarfparser
	pyrtaddr ptr32

	counter uint64
}

func preparePython(dwarf dwarfparser) (*python, error) {
	pyrtaddr := findPyRuntime(dwarf)
	if pyrtaddr == 0 {
		return nil, fmt.Errorf("could not find _PyRuntime address")
	}
	return &python{
		dwarf:    dwarf,
		pyrtaddr: ptr32(pyrtaddr),
	}, nil
}

// Find the address of the _PyRuntime symbol from the dwarf information.
// Returns 0 if not found.
func findPyRuntime(p dwarfparser) uint32 {
	for {
		ent, err := p.r.Next()
		if err != nil || ent == nil {
			break
		}
		if ent.Tag != dwarf.TagVariable {
			continue
		}
		name, _ := ent.Val(dwarf.AttrName).(string)
		if name != "_PyRuntime" {
			continue
		}
		f := ent.AttrField(dwarf.AttrLocation)
		if f == nil {
			panic("_PyRuntime does not have a location")
		}
		if f.Class != dwarf.ClassExprLoc {
			panic(fmt.Errorf("invalid location class: %s", f.Class))
		}
		const DW_OP_addr = 0x3
		loc := f.Val.([]byte)
		if len(loc) == 0 || loc[0] != DW_OP_addr {
			panic(fmt.Errorf("unexpected address format: %X", loc))
		}
		return binary.LittleEndian.Uint32(loc[1:])
	}
	return 0
}

// Padding of fields in various CPython structs. They are calculated
// by writing a function in any CPython module, and executing it with
// wazero.
//
// TODO: look into using CGO and #import<Python.h> to generate them
// instead.
const (
	// _PyRuntimeState
	padTstateCurrentInRT = 360
	// PyThreadState
	padCframeInThreadState = 40
	// _PyCFrame
	padCurrentFrameInCFrame = 4
	// _PyInterpreterFrame
	padPreviousInFrame  = 24
	padCodeInFrame      = 16
	padPrevInstrInFrame = 28
	padOwnerInFrame     = 37
	// PyCodeObject
	padFilenameInCodeObject       = 80
	padNameInCodeObject           = 84
	padCodeAdaptiveInCodeObject   = 116
	padFirstlinenoInCodeObject    = 48
	padLinearrayInCodeObject      = 104
	padLinetableInCodeObject      = 92
	padFirstTraceableInCodeObject = 108
	sizeCodeUnit                  = 2
	// PyASCIIObject
	padStateInAsciiObject  = 16
	padLengthInAsciiObject = 8
	sizeAsciiObject        = 24
	// PyBytesObject
	padSvalInBytesObject = 16
	padSizeInBytesObject = 8
	// Enum constants
	enumCodeLocation1         = 11
	enumCodeLocation2         = 12
	enumCodeLocationNoCol     = 13
	enumCodeLocationLong      = 14
	enumFrameOwnedByGenerator = 1
)

func (p *python) Locations(fn experimental.InternalFunction, pc experimental.ProgramCounter) (uint64, []location) {
	call := fn.(pyfuncall)

	loc := location{
		File:       call.file,
		Line:       int64(call.line),
		HumanName:  call.name,
		StableName: call.name,
	}

	return uint64(call.addr), []location{loc}
}

func (p *python) Stackiter(mod api.Module, def api.FunctionDefinition, wasmsi experimental.StackIterator) experimental.StackIterator {
	m := mod.Memory()
	tsp := deref[ptr32](m, p.pyrtaddr+padTstateCurrentInRT)
	cframep := deref[ptr32](m, tsp+padCframeInThreadState)
	framep := deref[ptr32](m, cframep+padCurrentFrameInCFrame)

	return &pystackiter{
		namedbg: def.DebugName(),
		counter: &p.counter,
		mem:     m,
		framep:  framep,
	}
}

type pystackiter struct {
	namedbg string
	counter *uint64
	mem     api.Memory
	started bool
	framep  ptr32 // _PyInterpreterFrame*
}

func (p *pystackiter) Next() bool {
	if !p.started {
		p.started = true
		return p.framep != 0
	}

	oldframe := p.framep
	p.framep = deref[ptr32](p.mem, p.framep+padPreviousInFrame)
	if oldframe == p.framep {
		fmt.Printf("frame previous field pointer to the same frame: %x", p.framep)
		p.framep = 0
		return false
	}
	return p.framep != 0
}

func (p *pystackiter) ProgramCounter() experimental.ProgramCounter {
	*p.counter += 1
	return experimental.ProgramCounter(*p.counter)
}

func (p *pystackiter) Function() experimental.InternalFunction {
	codep := deref[ptr32](p.mem, p.framep+padCodeInFrame)
	return pyfuncall{
		file: derefPyUnicodeUtf8(p.mem, codep+padFilenameInCodeObject),
		name: derefPyUnicodeUtf8(p.mem, codep+padNameInCodeObject),
		addr: deref[uint32](p.mem, p.framep+padPrevInstrInFrame),
		line: lineForFrame(p.mem, p.framep, codep),
	}
}

func (p *pystackiter) Parameters() []uint64 {
	panic("TODO parameters()")
}

// pyfuncall represent a specific place in the python source where a
// function call occurred.
type pyfuncall struct {
	file string
	name string
	line int32
	addr uint32

	api.FunctionDefinition // required for WazeroOnly
}

func (f pyfuncall) Definition() api.FunctionDefinition {
	return f
}

func (f pyfuncall) SourceOffsetForPC(pc experimental.ProgramCounter) uint64 {
	panic("does not make sense")
}

func (f pyfuncall) ModuleName() string {
	return "<unknown>" // TODO
}

func (f pyfuncall) Index() uint32 {
	return 42 // TODO
}

func (f pyfuncall) Import() (string, string, bool) {
	panic("implement me")
}

func (f pyfuncall) ExportNames() []string {
	panic("implement me")
}

func (f pyfuncall) Name() string {
	return f.name
}

func (f pyfuncall) DebugName() string {
	return f.name
}

func (f pyfuncall) GoFunction() interface{} {
	return nil
}

func (f pyfuncall) ParamTypes() []api.ValueType {
	panic("implement me")
}

func (f pyfuncall) ParamNames() []string {
	panic("implement me")
}

func (f pyfuncall) ResultTypes() []api.ValueType {
	panic("implement me")
}

func (f pyfuncall) ResultNames() []string {
	panic("implement me")
}

// Return the utf8 encoding of a PyUnicode object. It is a
// re-implementation of PyUnicode_AsUTF8. The bytes are copied from
// the vmem, so the returned string is safe to use.
func pyUnicodeUTf8(m vmem, p ptr32) string {
	statep := p + padStateInAsciiObject
	state := deref[uint8](m, statep)
	compact := state&(1<<5) > 0
	ascii := state&(1<<6) > 0
	if !compact || !ascii {
		panic("only support ascii-compact utf8 representation")
	}

	length := deref[int32](m, p+padLengthInAsciiObject)
	bytes := derefArray[byte](m, p+sizeAsciiObject, uint32(length))
	return unsafe.String(unsafe.SliceData(bytes), len(bytes))
}

func derefPyUnicodeUtf8(m vmem, p ptr32) string {
	x := deref[ptr32](m, p)
	return pyUnicodeUTf8(m, x)
}

func lineForFrame(m vmem, framep, codep ptr32) int32 {
	codestart := codep + padCodeAdaptiveInCodeObject
	previnstr := deref[ptr32](m, framep+padPrevInstrInFrame)
	firstlineno := deref[int32](m, codep+padFirstlinenoInCodeObject)

	if previnstr < codestart {
		return firstlineno
	}

	linearray := deref[ptr32](m, codep+padLinearrayInCodeObject)
	if linearray != 0 {
		fmt.Println("LINEARRAY PANIC")
		panic("can't handle code sections with line arrays")
	}

	codebytes := deref[ptr32](m, codep+padLinetableInCodeObject)
	if codebytes == 0 {
		fmt.Println("CODEBYTES PANIC")
		panic("code section must have a linetable")
	}

	length := deref[int32](m, codebytes+padSizeInBytesObject)
	linetable := codebytes + padSvalInBytesObject
	addrq := int32(previnstr - codestart)

	lo_next := linetable             // pointer to the current byte in the line table
	limit := lo_next + ptr32(length) // pointer to the end of the linetable
	ar_end := int32(0)               // offset into the code section
	computed_line := firstlineno     // current known line number
	ar_line := int32(-1)             // line for the current bytecode

	for ar_end <= addrq && lo_next < limit {
		lineDelta := int32(0)
		ptr := lo_next

		entry := deref[uint8](m, ptr)
		code := (entry >> 3) & 15
		switch code {
		case enumCodeLocation1:
			lineDelta = 1
		case enumCodeLocation2:
			lineDelta = 2
		case enumCodeLocationNoCol, enumCodeLocationLong:
			lineDelta = pysvarint(m, ptr+1)
		}

		computed_line += lineDelta

		if (entry >> 3) == 0x1F {
			ar_line = -1
		} else {
			ar_line = computed_line
		}

		ar_end += (int32(entry&7) + 1) * sizeCodeUnit

		lo_next++
		for lo_next < limit && (deref[uint8](m, lo_next)&128 == 0) {
			lo_next++
		}
	}

	return ar_line
}

// Python-specific implementation of protobuf signed varints. However
// it only uses 7 bits, as python uses the most significant bit to
// store whether an entry starts on that byte.
func pysvarint(m vmem, p ptr32) int32 {
	read := deref[uint8](m, p)
	val := uint32(read & 63)
	shift := 0
	for read&64 > 0 {
		read = deref[uint8](m, p)
		p++
		shift += 6
		val |= uint32(read&63) << shift
	}

	x := int32(val >> 1)
	if val&1 > 0 {
		x = ^x
	}
	return x
}
