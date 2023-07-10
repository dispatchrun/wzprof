package wzprof

import (
	"debug/dwarf"
	"encoding/binary"
	"fmt"
	"path/filepath"
	"strings"
	"unsafe"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

const (
	runtimeAddrName = "_PyRuntime"
	versionAddrName = "Py_Version"
)

func supportedPython(wasmbin []byte) bool {
	p, err := newDwarfParserFromBin(wasmbin)
	if err != nil {
		return false
	}

	versionAddr := pythonAddress(p, versionAddrName)
	if versionAddr == 0 {
		return false
	}

	data := wasmdataSection(wasmbin)
	if data == nil {
		return false
	}

	var versionhex uint32
	d := newDataIterator(data)
	for {
		vaddr, seg := d.Next()
		if seg == nil || vaddr > int64(versionAddr) {
			break
		}

		end := vaddr + int64(len(seg))
		if int64(versionAddr)+4 >= end {
			continue
		}

		offset := int64(versionAddr) - vaddr
		versionhex = binary.LittleEndian.Uint32(seg[offset:])
		break
	}

	// see cpython patchlevel.h
	major := (versionhex >> 24) & 0xFF
	minor := (versionhex >> 16) & 0xFF
	return major == 3 && minor == 11
}

func preparePython(mod wazero.CompiledModule) (*python, error) {
	p, err := newDwarfparser(mod)
	if err != nil {
		return nil, fmt.Errorf("could not build dwarf parser: %w", err)
	}
	runtimeAddr := pythonAddress(p, runtimeAddrName)
	if runtimeAddr == 0 {
		return nil, fmt.Errorf("could not find python runtime address")
	}
	return &python{
		pyrtaddr: ptr32(runtimeAddr),
	}, nil
}

func pythonAddress(p dwarfparser, name string) uint32 {
	for {
		ent, err := p.r.Next()
		if err != nil || ent == nil {
			break
		}
		if ent.Tag != dwarf.TagVariable {
			continue
		}
		n, _ := ent.Val(dwarf.AttrName).(string)
		if n != name {
			continue
		}
		return getDwarfLocationAddress(ent)
	}
	return 0
}

type python struct {
	pyrtaddr ptr32
	counter  uint64
}

func getDwarfLocationAddress(ent *dwarf.Entry) uint32 {
	f := ent.AttrField(dwarf.AttrLocation)
	if f == nil {
		return 0
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

// Padding of fields in various CPython structs. They are calculated
// by writing a function in any CPython module, and executing it with
// wazero.
//
// TODO: look into using CGO and #import<Python.h> to generate them
// instead.
const (
	// _PyRuntimeState.
	padTstateCurrentInRT = 360
	// PyThreadState.
	padCframeInThreadState = 40
	// _PyCFrame.
	padCurrentFrameInCFrame = 4
	// _PyInterpreterFrame.
	padPreviousInFrame  = 24
	padCodeInFrame      = 16
	padPrevInstrInFrame = 28
	padOwnerInFrame     = 37
	// PyCodeObject.
	padFilenameInCodeObject       = 80
	padNameInCodeObject           = 84
	padCodeAdaptiveInCodeObject   = 116
	padFirstlinenoInCodeObject    = 48
	padLinearrayInCodeObject      = 104
	padLinetableInCodeObject      = 92
	padFirstTraceableInCodeObject = 108
	sizeCodeUnit                  = 2
	// PyASCIIObject.
	padStateInAsciiObject  = 16
	padLengthInAsciiObject = 8
	sizeAsciiObject        = 24
	// PyBytesObject.
	padSvalInBytesObject = 16
	padSizeInBytesObject = 8
	// Enum constants.
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
	line, _ := lineForFrame(p.mem, p.framep, codep)
	file := derefPyUnicodeUtf8(p.mem, codep+padFilenameInCodeObject)
	name := derefPyUnicodeUtf8(p.mem, codep+padNameInCodeObject)
	return pyfuncall{
		file: file,
		name: functionName(file, name),
		addr: deref[uint32](p.mem, p.framep+padPrevInstrInFrame),
		line: line,
	}
}

func functionName(path, function string) string {
	mod := ""
	const frozenPrefix = "<frozen "
	if strings.HasPrefix(path, frozenPrefix) {
		mod = path[len(frozenPrefix) : len(path)-1]
	} else {
		file := filepath.Base(path)
		mod = file[:len(file)-len(filepath.Ext(file))]
	}

	if function == "<module>" {
		return mod
	}
	return mod + "." + function
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

func lineForFrame(m vmem, framep, codep ptr32) (int32, bool) {
	codestart := codep + padCodeAdaptiveInCodeObject
	previnstr := deref[ptr32](m, framep+padPrevInstrInFrame)
	firstlineno := deref[int32](m, codep+padFirstlinenoInCodeObject)

	if previnstr < codestart {
		return firstlineno, false
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

	return ar_line, true
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
		x = -x
	}
	return x
}
