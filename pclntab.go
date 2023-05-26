package wzprof

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"unsafe"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"

	"github.com/stealthrocket/wzprof/internal/goruntime"
)

// Try to detect if the module was compiled by golang/go (not by tinygo).
func compiledByGo(mod wazero.CompiledModule) bool {
	for _, s := range mod.CustomSections() {
		if s.Name() == "go:buildid" {
			return true
		}
	}
	return false
}

// partialPCHeader is a small fraction of the PCHEader written by the linker.
// See pclntabHeaderFromData for more details.
type partialPCHeader struct {
	address        uint64
	funcnametabOff uint64
	cutabOff       uint64
	filetabOff     uint64
}

func (p partialPCHeader) Valid() bool {
	return p.address != 0
}

// pclntabFromData rebuilds a partial pclntab header from the segments of the
// Data section of a module.
//
// Assumes the section is well-formed, and the segment has the layout described
// in the 1.20.1 linker. Returns nil if the segment is missing. Does not check
// whether pclntab contains actual useful data.
//
// The goal is to retrieve enough of the pclntab header to compute a needle for
// the moduledata using the offsets contained in this header.
//
// See layout in the linker:
// https://github.com/golang/go/blob/3e35df5edbb02ecf8efd6dd6993aabd5053bfc66/src/cmd/link/internal/ld/pcln.go#L235-L248
func pclntabHeaderFromData(b []byte) partialPCHeader {
	// magic number of the start of pclntab for Go 1.20, little endian. Also add
	// constants for the wasm arch to have fewer chances of finding something
	// that is not the pclntab. Constants:
	// https://github.com/golang/go/blob/82d5ebce96761083f5313b180c6b368be1912d42/src/cmd/internal/sys/arch.go#L257-L268
	needle := []byte{
		0xf1, 0xff, 0xff, 0xff, 0x00, 0x00, // magic number
		0x01, // MinLC
		0x08, // PtrSize
	}
	pclntabOffset := bytes.Index(b, needle)
	if pclntabOffset == -1 {
		return partialPCHeader{}
	}

	d := newDataIterator(b)
	vaddr, seg := d.SkipToDataOffset(pclntabOffset)
	vm := vmemb{Start: vaddr}
	vm.CopyAtAddress(vaddr, seg)

	magic := needle[:6]
	if !bytes.Equal(magic, seg[:len(magic)]) {
		panic("segment should start by magic")
	}

	if len(seg) < 8 {
		panic("segment should at least contain header")
	}

	readWord := func(word int) uint64 {
		for {
			start := 8 + word*8
			end := start + 8
			if vm.Has(end) {
				return binary.LittleEndian.Uint64(vm.b[start:])
			}
			vaddr, seg := d.Next()
			if seg == nil {
				panic("no more segment")
			}
			vm.CopyAtAddress(vaddr, seg)
		}
	}

	funcnametabOff := readWord(3)
	cutabOff := readWord(4)
	filetabOff := readWord(5)

	return partialPCHeader{
		address:        uint64(vaddr),
		funcnametabOff: funcnametabOff,
		cutabOff:       cutabOff,
		filetabOff:     filetabOff,
	}
}

// moduledataFromData searches the data segments to find the virtual address of
// firstmoduledata struct embedded in the module.
//
// It is tricky because there is no marker to tell us where it starts. So
// instead we look for pointers it should contain in specific fields. We compute
// those pointers from the partial contents of the pclntab we already found.
//
// We know all those addresses are in the same data segment because they are
// close enough together that they can't contain more than 8 zeroes between
// them, not triggering the compression mechanism used by the wasm linker.
func moduledataAddrFromData(pch partialPCHeader, b []byte) uint64 {
	scratch := [4 * 8]byte{}
	binary.LittleEndian.PutUint64(scratch[0*8:], pch.address)
	binary.LittleEndian.PutUint64(scratch[1*8:], pch.address+pch.funcnametabOff)
	binary.LittleEndian.PutUint64(scratch[2*8:], pch.address+pch.cutabOff)
	binary.LittleEndian.PutUint64(scratch[3*8:], pch.address+pch.filetabOff)
	start := scratch[0 : 2*8]
	cutabaddr := scratch[2*8 : 3*8]
	filetabaddr := scratch[3*8 : 4*8]
	offset := findStartOfModuleData(b, start, cutabaddr, filetabaddr)
	if offset == -1 {
		return 0
	}
	d := newDataIterator(b)
	vaddr, _ := d.SkipToDataOffset(offset)
	return uint64(vaddr)
}

// returns -1 if not found.
func findStartOfModuleData(b, start, cutabaddr, filetabaddr []byte) int {
	// offset 0: pch.addr
	// offset 8: funcnametab address
	// offset 32: cutab address
	// offset 56: filetab address
	for begin := 0; begin < len(b); {
		startIndex := bytes.Index(b[begin:], start)
		if startIndex < -1 {
			return -1
		}
		i := startIndex + 32
		if !bytes.Equal(b[i:i+8], cutabaddr) {
			begin = startIndex + 1
			continue
		}

		i = startIndex + 56
		if !bytes.Equal(b[i:i+8], filetabaddr) {
			begin = startIndex + 1
			continue
		}

		return begin + startIndex
	}
	return -1
}

// fid is the ID of a function, that is its number in the function section of
// the module, which includes imports. In a given module, fid = fidx+imports.
// This type exists to avoid errors related to mixing up function ids and
// indexes.
type fid int

func preparePclntabSymbolizer(wasmbin []byte, mod wazero.CompiledModule) (*pclntab, error) {
	data := wasmdataSection(wasmbin)
	if data == nil {
		return nil, fmt.Errorf("no data section in the wasm binary")
	}
	pch := pclntabHeaderFromData(data)
	if !pch.Valid() {
		return nil, fmt.Errorf("could not find pclnheader in data section")
	}
	mdaddr := moduledataAddrFromData(pch, data)
	if mdaddr == 0 {
		return nil, fmt.Errorf("could not find moduledata in data section")
	}
	return &pclntab{
		imported: uint64(len(mod.ImportedFunctions())),
		modName:  mod.Name(),
		datap:    ptr(mdaddr),
	}, nil
}

// Copy of _func in runtime/runtime2.go. It has to have the same size.
type _func struct {
	EntryOff    uint32 // start pc, as offset from moduledata.text/pcHeader.textStart
	NameOff     int32  // function name, as index into moduledata.funcnametab.
	Args        int32  // in/out args size
	Deferreturn uint32 // offset of start of a deferreturn call instruction from entry, if any.
	Pcsp        uint32
	Pcfile      uint32
	Pcln        uint32
	Npcdata     uint32
	CuOffset    uint32 // runtime.cutab offset of this function's CU
	StartLine   int32  // line number of start of function (func keyword/TEXT directive)
	FuncID      goruntime.FuncID
	Flag        goruntime.FuncFlag
	_           [1]byte // pad
	Nfuncdata   uint8
}

// isInlined reports whether f should be re-interpreted as a *funcinl.
func (f *_func) isInlined() bool {
	return f.EntryOff == ^uint32(0) // see comment for funcinl.ones
}

// Pseudo-Func that is returned for PCs that occur in inlined code.
// A *Func can be either a *_func or a *funcinl, and they are distinguished
// by the first ptr.
type funcinl struct {
	ones      uint32 // set to ^0 to distinguish from _func
	entry     ptr    // entry of the real (the "outermost") frame
	name      string
	file      string
	line      int32
	startLine int32
}

// A srcFunc represents a logical function in the source code. This may
// correspond to an actual symbol in the binary text, or it may correspond to a
// source function that has been inlined.
type srcFunc struct {
	datap     *moduledata
	nameOff   int32
	startLine int32
	funcID    goruntime.FuncID
}

// Extra data around _func.
type funcInfo struct {
	*_func
	md *moduledata
	// offset in pclntab of the start of the _func.
	_funcoff pclntabOff
}

func (f funcInfo) srcFunc() srcFunc {
	if !f.valid() {
		return srcFunc{}
	}
	return srcFunc{f.md, f.NameOff, f.StartLine, f.FuncID}
}

func (f funcInfo) valid() bool {
	return f._func != nil
}

func (f funcInfo) entry() ptr {
	return f.md.textAddr(f.EntryOff)
}

func (f funcInfo) name() string {
	return f.md.funcName(f.NameOff)
}

// FileLine returns the file name and line number of the
// source code corresponding to the program counter pc.
// The result will not be accurate if pc is not a program
// counter within f.
func (f funcInfo) fileLine(pc ptr) (file string, line int) {
	fn := f._func
	if fn.isInlined() { // inlined version
		fi := (*funcinl)(unsafe.Pointer(fn))
		return fi.file, int(fi.line)
	}
	// Pass strict=false here, because anyone can call this function,
	// and they might just be wrong about targetpc belonging to f.
	file, line32 := funcline1(f, pc)
	return file, int(line32)
}

func funcline1(f funcInfo, targetpc ptr) (file string, line int32) {
	datap := f.md
	if !f.valid() {
		return "?", 0
	}
	fileno, _ := pcvalue(f, f.Pcfile, targetpc)
	line, _ = pcvalue(f, f.Pcln, targetpc)
	if fileno == -1 || line == -1 || int(fileno) >= len(datap.filetab) {
		// print("looking for ", hex(targetpc), " in ", funcname(f), " got file=", fileno, " line=", lineno, "\n")
		return "?", 0
	}
	file = funcfile(f, fileno)
	return
}

func funcfile(f funcInfo, fileno int32) string {
	datap := f.md
	if !f.valid() {
		return "?"
	}
	// Make sure the cu index and file offset are valid
	if fileoff := datap.cutab[f.CuOffset+uint32(fileno)]; fileoff != ^uint32(0) {
		return cstring(datap.filetab[fileoff:])
	}
	// pcln section is corrupt.
	return "?"
}

// index into moduledata.pclntable byte slice. This exists because
// derefModuledata loses the virtual memory addresses of the slices it contains
// (including the one of pclntab). A few functions performs pointer arithmetic
// from the address of a _func. Instead of tracking the original virtual
// addresses of all those slices, instead we use offsets into the dereferenced
// byte array, which isn't much harded but avoids a lot of bookeeping and
// derefs.
type pclntabOff uint32

func pcdatavalue1(f funcInfo, table uint32, targetpc ptr) int32 {
	if table >= f.Npcdata {
		return -1
	}
	r, _ := pcvalue(f, pcdatastart(f, table), targetpc)
	return r
}

func pcdatastart(f funcInfo, table uint32) uint32 {
	off := f._funcoff + pclntabOff(unsafe.Sizeof(_func{})) + pclntabOff(table)*4
	return *(*uint32)(unsafe.Pointer(unsafe.SliceData(f.md.pclntable[off:])))
}

// Returns the offset from moduledata.gofunc for the i-th funcdata of f.
func funcdataoffset(f funcInfo, index uint8) uint32 {
	off := f._funcoff + pclntabOff(unsafe.Sizeof(_func{})) + pclntabOff(f.Npcdata)*4 + pclntabOff(index)*4
	return *(*uint32)(unsafe.Pointer(unsafe.SliceData(f.md.pclntable[off:])))
}

// PCLNTAB provides symbol resolution for Go using the pclntab and moduledata
// present in the memory of a given module.
//
// It is built in two steps: the first one before module instantiation
// (preparePclntabSymbolizer), to initialize the fields that cannot be guessed
// at runtime. Then it is lazily initialized from the module memory on its first
// symbol resolution.
//
// Once memory is step, it is expected to stay the same throughout the lifetime
// of this pclntab.
type pclntab struct {
	// Number of functions imported by the module.
	imported uint64
	// Name of the module.
	modName string
	// Virtual address of the firstmoduledata structure. Named like this for
	// similarity with the Go implementation.
	datap ptr

	mem vmem
	md  moduledata
}

// EnsureReady loads up from memory the necessary contents of moduledata, and
// pclntab to be able to perform symbolization and provide enough information
// about functions to walk the stack. Just once.
func (p *pclntab) EnsureReady(mem vmem) {
	if p.mem != nil {
		if p.mem != mem {
			panic("different memory used for pclntab")
		}
		return
	}
	p.mem = mem
	p.md = derefModuledata(mem, p.datap)
}

// FindFunc searches the pclntab to build the FuncInfo that contains the
// provided pc.
//
// TODO: support multiple go modules.
// TODO: cache this, as it's on the hot path.
func (p *pclntab) FindFunc(pc ptr) funcInfo {
	if pc < p.md.minpc || pc >= p.md.maxpc {
		return funcInfo{}
	}

	// https://github.com/golang/go/blob/f90b4cd6554f4f20280aa5229cf42650ed47221d/src/runtime/symtab.go#L514
	const nsub = 16
	const minfunc = 16                 // minimum function size
	const pcbucketsize = 256 * minfunc // size of bucket in the pc->func lookup table

	pcOff, ok := p.md.textOff(pc)
	if !ok {
		return funcInfo{}
	}

	x := ptr(pcOff) + p.md.text - p.md.minpc
	b := x / pcbucketsize
	i := x % pcbucketsize / (pcbucketsize / nsub)

	ffb := deref[findfuncbucket](p.mem, p.md.findfunctab+b*ptr(unsafe.Sizeof(findfuncbucket{})))

	idx := ffb.idx + uint32(ffb.subbuckets[i])

	// Find the ftab entry.
	for p.md.ftab[idx+1].entryoff <= pcOff {
		idx++
	}

	funcoff := p.md.ftab[idx].funcoff
	_f := (*_func)(unsafe.Pointer(unsafe.SliceData(p.md.pclntable[funcoff:])))

	return funcInfo{_func: _f, md: &p.md, _funcoff: pclntabOff(funcoff)}
}

// Locations perform the symolization of a physical pc belongging to a provided
// function. Used when building the profile from the collected samples.
func (p *pclntab) Locations(gofunc experimental.InternalFunction, pc experimental.ProgramCounter) (uint64, []location) {
	// Assumption that pclntabmapper is only used in conjuction with
	// goStackIterator.
	f := gofunc.(goFunction)

	locs := []location{}

	var calleeFuncID goruntime.FuncID

	iu, uf := newInlineUnwinder(p, f.mem, f.info, symPC(f.info, ptr(pc)))
	for ; uf.valid(); uf = iu.next(uf) {
		sf := iu.srcFunc(uf)
		if sf.funcID == goruntime.FuncIDWrapper && elideWrapperCalling(calleeFuncID) {
			// skip wrappers like stdlib
			continue
		}
		ipc := uf.pc
		calleeFuncID = sf.funcID

		file, line, fn := p.PCToLine(ipc)
		if !fn.valid() {
			continue
		}
		locs = append(locs, location{
			File:       file,
			Line:       int64(line),
			StableName: fn.name(),
			HumanName:  fn.name(),
		})
	}

	// reverse locations to follow dwarf's convention
	for i, j := 0, len(locs)-1; i < j; i, j = i+1, j-1 {
		locs[i], locs[j] = locs[j], locs[i]
	}

	return uint64(pc), locs
}

// symPC returns the PC that should be used for symbolizing the current frame.
// Specifically, this is the PC of the last instruction executed in this frame.
//
// If this frame did a normal call, then frame.pc is a return PC, so this will
// return frame.pc-1, which points into the CALL instruction. Finally, frame.pc
// can be at function entry when the frame is initialized without actually
// running code, like in runtime.mstart, in which case this returns frame.pc
// because that's the best we can do.
func symPC(fn funcInfo, pc ptr) ptr {
	if pc > fn.entry() {
		// Regular call.
		return pc - 1
	}
	// We're at the function entry point.
	return pc
}

// https://github.com/golang/go/blob/4859392cc29a35a0126e249ecdedbd022c755b20/src/cmd/link/internal/wasm/asm.go#L45
const funcValueOffset = 0x1000

func (p *pclntab) PCToFID(pc ptr) fid {
	return fid(uint64(pc)>>16 + p.imported - funcValueOffset)
}

func (p *pclntab) FIDToPC(f fid) ptr {
	return ptr((funcValueOffset + f - fid(p.imported)) << 16)
}

func (p *pclntab) PCToName(pc ptr) string {
	f := p.FindFunc(pc)
	if !f.valid() {
		return ""
	}
	return f.name()
}

func (p *pclntab) PCToLine(pc ptr) (file string, line int, f funcInfo) {
	f = p.FindFunc(pc)
	if !f.valid() {
		return
	}
	file, line = f.fileLine(pc)
	return
}

// gptr represents a *g in the guest memory. It exists for the same reason as
// ptr, but is a separate type to avoid confusion between the two. The main
// difference is a gptr is not supposed to have arithmetic done on it outside
// rtmem. Also, easier to replace guintptr with a dedicated type.
type gptr ptr

// Layout of g struct:
//
// size, index, field
// 8,    0,     stack.lo
// 8,    1,     stack.hi
// 8,    2,     stackguard0
// 8,    3,     stackguard1
// 8,    4,     _panic
// 8,    5,     _defer
// 8,    6,     m
// 8,    7,     sched.sp
// 8,    8,     sched.pc
// 8,    9,     sched.g
// 8,    10,    sched.ctxt
// 8,    11,    sched.ret
// 8,    12,    sched.lr
// more fields that we don't care about

// Layout of M struct:
//
// size, offset, field
// 8,    0,      g0
// 56,   8,      morebuf
// 8,    64,     divmod, -
// 8,    72,     procid
// 8,    80,     gsignal
// 0,    88,     goSigStack
// 0,    88,     sigmask
// 48,   88,     tls
// 8,    136,    mstartfn
// 8,    144,    curg
// more fields we don't care about
//
// goSigStack and sigmask are 0 because
// https://github.com/golang/go/blob/b950cc8f11dc31cc9f6cfbed883818a7aa3abe94/src/runtime/os_wasm.go#L132

func gM(m vmem, g gptr) ptr {
	return deref[ptr](m, ptr(g)+8*6)
}

func gMG0(m vmem, g gptr) gptr {
	return deref[gptr](m, gM(m, g)+0)
}

func gMCurg(m vmem, g gptr) gptr {
	return deref[gptr](m, gM(m, g)+144)
}

func gSchedSp(m vmem, g gptr) ptr {
	return deref[ptr](m, ptr(g)+8*7)
}

func gSchedPc(m vmem, g gptr) ptr {
	return deref[ptr](m, ptr(g)+8*8)
}

func gSchedLr(m vmem, g gptr) ptr {
	return deref[ptr](m, ptr(g)+8*12)
}

// goStackIterator iterates over the physical frames of the Go stack. It is up
// to the symbolizer (pclntabmapper) to expand those into logical frames to
// account for inlining.
type goStackIterator struct {
	first   bool
	pclntab *pclntab
	pc      ptr
	unwinder
}

func (s *goStackIterator) Next() bool {
	if !s.valid() {
		return false
	}

	if s.first {
		s.first = false
		s.pc = s.frame.pc
		return true
	}

	s.next()
	if !s.valid() {
		return false
	}
	s.pc = s.frame.pc
	return true
}

func (s *goStackIterator) ProgramCounter() experimental.ProgramCounter {
	return experimental.ProgramCounter(s.pc)
}

func (s *goStackIterator) Function() experimental.InternalFunction {
	return goFunction{
		mem:  s.mem,
		sym:  s.symbols,
		info: s.frame.fn,
		pc:   s.frame.pc,
	}
}

func (s *goStackIterator) Parameters() []uint64 {
	panic("implement me")
}

var _ experimental.StackIterator = (*goStackIterator)(nil)

// elideWrapperCalling reports whether a wrapper function that called
// function id should be elided from stack traces.
func elideWrapperCalling(id goruntime.FuncID) bool {
	// If the wrapper called a panic function instead of the
	// wrapped function, we want to include it in stacks.
	return !(id == goruntime.FuncID_gopanic || id == goruntime.FuncID_sigpanic || id == goruntime.FuncID_panicwrap)
}

// goFunction is a lazy implementation of wazero's FunctionDefinition and
// InternalFunction, as the goStackIterator cannot map to an internal *function
// in wazero.
type goFunction struct {
	mem  vmem
	sym  *pclntab
	info funcInfo
	pc   ptr

	api.FunctionDefinition // required for WazeroOnly
}

func (f goFunction) Definition() api.FunctionDefinition {
	return f
}

func (f goFunction) SourceOffsetForPC(experimental.ProgramCounter) uint64 {
	panic("does not make sense")
}

func (f goFunction) ModuleName() string {
	return f.sym.modName
}

func (f goFunction) Index() uint32 {
	return uint32(f.sym.PCToFID(f.pc))
}

func (f goFunction) Import() (string, string, bool) {
	panic("implement me")
}

func (f goFunction) ExportNames() []string {
	panic("implement me")
}

func (f goFunction) Name() string {
	return f.sym.PCToName(f.pc)
}

func (f goFunction) DebugName() string {
	panic("implement me")
}

func (f goFunction) GoFunction() interface{} {
	// This is never a host function
	return nil
}

func (f goFunction) ParamTypes() []api.ValueType {
	panic("implement me")
}

func (f goFunction) ParamNames() []string {
	panic("implement me")
}

func (f goFunction) ResultTypes() []api.ValueType {
	panic("implement me")
}

func (f goFunction) ResultNames() []string {
	panic("implement me")
}

type functab struct {
	entryoff uint32 // relative to runtime.text
	funcoff  uint32
}

// Mapping information for secondary text sections.
type textsect struct {
	vaddr    ptr // prelinked section vaddr
	end      ptr // vaddr + section length
	baseaddr ptr // relocated section address
}

// findfuncbucket is an array of these structures.
// Each bucket represents 4096 bytes of the text segment.
// Each subbucket represents 256 bytes of the text segment.
// To find a function given a pc, locate the bucket and subbucket for
// that pc. Add together the idx and subbucket value to obtain a
// function index. Then scan the functab array starting at that
// index to find the target function.
// This table uses 20 bytes for every 4096 bytes of code, or ~0.5% overhead.
type findfuncbucket struct {
	idx        uint32
	subbuckets [16]byte
}

// moduledata comes from runtime/symtab.go. It is important it keeps the same
// layout to be rebuilt from memory. If you uncomment a field here, make sure to
// update derefModuleData accordingly.
// nolint:unused
type moduledata struct {
	pcHeader              ptr
	funcnametab           []byte
	cutab                 []uint32
	filetab               []byte
	pctab                 []byte
	pclntable             []byte
	ftab                  []functab
	findfunctab           ptr
	minpc, maxpc          ptr
	text, etext           ptr
	noptrdata, enoptrdata ptr
	data, edata           ptr
	bss, ebss             ptr
	noptrbss, enoptrbss   ptr
	covctrs, ecovctrs     ptr
	end, gcdata, gcbss    ptr
	types, etypes         ptr
	rodata                ptr
	gofunc                ptr //  go.func.*
	textsectmap           []textsect
	// more fields we don't care about now.
	// ...
	// next *moduledata
}

// funcName returns the string at nameOff in the function name table.
func (md moduledata) funcName(nameOff int32) string {
	if nameOff == 0 {
		return ""
	}
	x := md.funcnametab[nameOff:]
	return cstring(x)
}

// Captures the first null-terminated string from b.
// TODO: no alloc.
func cstring(b []byte) string {
	i := bytes.IndexByte(b, 0)
	if i < 0 {
		return ""
	}
	return string(b[:i])
}

// textOff is the opposite of textAddr. It converts a PC to a (virtual) offset
// to md.text, and returns if the PC is in any Go text section.
func (md moduledata) textOff(pc ptr) (uint32, bool) {
	res := uint32(pc - md.text)
	if len(md.textsectmap) > 1 {
		for i, sect := range md.textsectmap {
			if sect.baseaddr > pc {
				// pc is not in any section.
				return 0, false
			}
			end := sect.baseaddr + (sect.end - sect.vaddr)
			// For the last section, include the end address (etext), as it is included in the functab.
			if i == len(md.textsectmap) {
				end++
			}
			if pc < end {
				res = uint32(pc - sect.baseaddr + sect.vaddr)
				break
			}
		}
	}
	return res, true
}

// textAddr returns md.text + off, with special handling for multiple text
// sections. off is a (virtual) offset computed at internal linking time, before
// the external linker adjusts the sections' base addresses.
//
// The text, or instruction stream is generated as one large buffer. The off
// (offset) for a function is its offset within this buffer. If the total text
// size gets too large, there can be issues on platforms like ppc64 if the
// target of calls are too far for the call instruction. To resolve the large
// text issue, the text is split into multiple text sections to allow the linker
// to generate long calls when necessary. When this happens, the vaddr for each
// text section is set to its offset within the text. Each function's offset is
// compared against the section vaddrs and ends to determine the containing
// section. Then the section relative offset is added to the section's relocated
// baseaddr to compute the function address.
func (md moduledata) textAddr(off32 uint32) ptr {
	off := ptr(off32)
	res := md.text + off
	if len(md.textsectmap) > 1 {
		for i, sect := range md.textsectmap {
			// For the last section, include the end address (etext), as it is included in the functab.
			if off >= sect.vaddr && off < sect.end || (i == len(md.textsectmap)-1 && off == sect.end) {
				res = sect.baseaddr + off - sect.vaddr
				break
			}
		}
	}
	return res
}

// Retrieve module data from memory, including slices.
func derefModuledata(mem vmem, addr ptr) moduledata {
	m := deref[moduledata](mem, addr)
	m.funcnametab = derefGoSlice(mem, m.funcnametab)
	m.cutab = derefGoSlice(mem, m.cutab)
	m.filetab = derefGoSlice(mem, m.filetab)
	m.pctab = derefGoSlice(mem, m.pctab)
	m.pclntable = derefGoSlice(mem, m.pclntable)
	m.ftab = derefGoSlice(mem, m.ftab)
	m.textsectmap = derefGoSlice(mem, m.textsectmap)
	return m
}
