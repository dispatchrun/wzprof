package wzprof

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"unsafe"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"

	"github.com/stealthrocket/wzprof/internal/gosym"
)

func compiledByGo(mod wazero.CompiledModule) bool {
	for _, s := range mod.CustomSections() {
		if s.Name() == "go:buildid" {
			return true
		}
	}
	return false
}

// wasmdataSection parses a WASM binary and returns the bytes of the WASM "Data"
// section. Returns nil if the sections do not exist.
//
// It is a very weak parser: it should be called on a valid module, or it may
// panic.
//
// This function exists because Wazero doesn't expose the Code and Data sections
// on its CompiledModule and they are needed to retrieve pclntab on Go-compiled
// modules.
func wasmdataSection(b []byte) []byte {
	const dataSectionId = 11

	b = b[8:] // skip magic+version
	for len(b) > 2 {
		id := b[0]
		b = b[1:]
		length, n := binary.Uvarint(b)
		b = b[n:]

		if id == dataSectionId {
			return b[:length]
		}
		b = b[length:]
	}
	return nil
}

// dataIterator iterates over the segments contained in a wasm Data section.
// Only support mode 0 (memory 0 + offset) segments.
type dataIterator struct {
	b []byte // remaining bytes in the Data section
	n uint64 // number of segments

	// offset of b in the Data section.
	offset int
}

// newDataIterator prepares an iterator using the bytes of a well-formed data
// section.
func newDataIterator(b []byte) dataIterator {
	segments, r := binary.Uvarint(b)
	return dataIterator{
		b:      b[r:],
		n:      segments,
		offset: r,
	}
}

func (d *dataIterator) read(n int) (b []byte) {
	b, d.b = d.b[:n], d.b[n:]
	d.offset += n
	return b
}

func (d *dataIterator) skip(n int) {
	d.b = d.b[n:]
	d.offset += n
}

func (d *dataIterator) byte() byte {
	b := d.b[0]
	d.skip(1)
	return b
}

func (d *dataIterator) varint() int64 {
	x, n := sleb128(64, d.b)
	d.skip(n)
	return x
}

func sleb128(size int, b []byte) (result int64, read int) {
	// The difference between sleb128 and protobuf's binary.Varint is that
	// the latter puts the sign at the least significant bit.
	shift := 0

	var byte byte
	for {
		byte = b[0]
		read++
		b = b[1:]

		result |= (int64(0b01111111&byte) << shift)
		shift += 7
		if 0b10000000&byte == 0 {
			break
		}
	}
	if (shift < size) && (0x40&byte > 0) {
		result |= (^0 << shift)
	}
	return result, read
}

func (d *dataIterator) uvarint() uint64 {
	x, n := binary.Uvarint(d.b)
	d.skip(n)
	return x
}

// Next returns the bytes of the following segment, and its offset in virtual
// memory, or a nil slice if there are no more segment.
func (d *dataIterator) Next() (offset int64, seg []byte) {
	if d.n == 0 {
		return 0, nil
	}

	// Format of mode 0 segment:
	//
	// varuint32 - mode (1 byte, 0)
	// byte      - i32.const (0x41)
	// varint64  - virtual address
	// byte      - end of expression (0x0B)
	// varuint64 - length
	// bytes     - raw bytes of the segment

	mode := d.uvarint()
	if mode != 0x0 {
		panic(fmt.Errorf("unsupported mode %#x", mode))
	}

	v := d.byte()
	if v != 0x41 {
		panic(fmt.Errorf("expected constant i32.const (0x41); got %#x", v))
	}

	offset = d.varint()

	v = d.byte()
	if v != 0x0B {
		panic(fmt.Errorf("expected end of expr (0x0B); got %#x", v))
	}

	length := d.uvarint()
	seg = d.read(int(length))
	d.n--

	return offset, seg
}

// SkipToDataOffset iterates over segments to return the bytes at a given data
// offset, until the end of the segment that contains the offset, and the
// virtual address of the byte at that offset.
//
// Panics if offset was already passed or the offset is out of bounds.
func (d *dataIterator) SkipToDataOffset(offset int) (int64, []byte) {
	if offset < d.offset {
		panic(fmt.Errorf("offset %d requested by already at %d", offset, d.offset))
	}
	end := d.offset + len(d.b)
	if offset >= d.offset+len(d.b) {
		panic(fmt.Errorf("offset %d requested past data section %d", offset, end))
	}

	for d.offset <= offset {
		vaddr, seg := d.Next()
		if d.offset < offset {
			continue
		}
		o := len(seg) + offset - d.offset
		return vaddr + int64(o), seg[o:]
	}

	return 0, nil
}

type partialPCHeader struct {
	address        uint64
	funcnametabOff uint64
	cutabOff       uint64
	filetabOff     uint64
}

// pclntabFromData rebuilds the full pclntab from the segments of the Data
// section of the module (b).
//
// Assumes the section is well-formed, and the segment has the layout described
// in the 1.20.1 linker. Returns nil if the segment is missing. Does not check
// whether pclntab contains actual useful data.
//
// See layout in the linker: https://github.com/golang/go/blob/3e35df5edbb02ecf8efd6dd6993aabd5053bfc66/src/cmd/link/internal/wasm/asm.go#L169-L185
func pclntabFromData(b []byte) (partialPCHeader, []byte) {
	// magic number of the start of pclntab for Go 1.20, little endian.
	magic := []byte{0xf1, 0xff, 0xff, 0xff, 0x00, 0x00}
	pclntabOffset := bytes.Index(b, magic)
	if pclntabOffset == -1 {
		return partialPCHeader{}, nil
	}

	d := newDataIterator(b)
	vaddr, seg := d.SkipToDataOffset(pclntabOffset)
	vm := vmem{Start: vaddr}
	vm.CopyAtAddress(vaddr, seg)

	if !bytes.Equal(magic, seg[:len(magic)]) {
		panic("segment should start by magic")
	}

	if len(seg) < 8 {
		panic("segment should at least contain header")
	}
	vm.Quantum = seg[len(magic)+0]
	vm.Ptrsize = int(seg[len(magic)+1])

	if vm.Ptrsize != 8 {
		panic("only supports 64bit pclntab")
	}

	fillUntil := func(addr int) {
		// fill the vm with segments until it has data at addr.
		for !vm.Has(addr) {
			vaddr, seg := d.Next()
			if seg == nil {
				panic("no more segment")
			}
			vm.CopyAtAddress(vaddr, seg)
		}
	}

	readWord := func(word int) uint64 {
		for {
			x, ok := vm.PclntabOffset(word)
			if ok {
				return x
			}
			vaddr, seg := d.Next()
			if seg == nil {
				panic("no more segment")
			}
			vm.CopyAtAddress(vaddr, seg)
		}
	}

	nfunctab := readWord(0)
	// nfiletab := readWord(1)
	// pcstart := readWord(2)
	funcnametabOff := readWord(3)
	cutabOff := readWord(4)
	filetabOff := readWord(5)
	// pctabAddr := readWord(6)
	// funcdataAddr := readWord(7)
	functabAddr := readWord(7)

	functabFieldSize := 4

	functabsize := (int(nfunctab)*2 + 1) * functabFieldSize
	end := functabAddr + uint64(functabsize)
	fillUntil(int(end))

	// TODO: try to actually guess the end of pclntab.
	for {
		vaddr, seg := d.Next()
		if seg == nil {
			break
		}
		vm.CopyAtAddress(vaddr, seg)
	}

	if !bytes.HasPrefix(vm.b, magic) {
		panic("pclntab should start with magic")
	}
	if uint64(len(vm.b)) < end {
		panic("reconstructed pclntab should at least include end of functab")
	}

	pch := partialPCHeader{
		address:        uint64(vaddr),
		funcnametabOff: funcnametabOff,
		cutabOff:       cutabOff,
		filetabOff:     filetabOff,
	}

	return pch, vm.b
}

// moduledataFromData searches the data segments to find and reconstruct the
// firstmoduledata struct embedded in the module.
//
// It is tricky because there is no marker to tell us where it starts. So
// instead we look for pointers it should contain in specific fields. We compute
// those pointers from the partial contents of the pclntab we already found.
//
// We know all those addresses are in the same data segment, but the whole
// moduledata may not be.
func moduledataFromData(pch partialPCHeader, b []byte) (moduledata, error) {
	start := make([]byte, 16) // pchaddr and funcnametabaddr are one after the other.
	binary.LittleEndian.PutUint64(start[:8], pch.address)
	binary.LittleEndian.PutUint64(start[8:], pch.address+pch.funcnametabOff)
	cutabaddr := make([]byte, 8)
	binary.LittleEndian.PutUint64(cutabaddr, pch.address+pch.cutabOff)
	filetabaddr := make([]byte, 8)
	binary.LittleEndian.PutUint64(filetabaddr, pch.address+pch.filetabOff)

	offset := findStartOfModuleData(b, start, cutabaddr, filetabaddr)

	if offset == -1 {
		return moduledata{}, fmt.Errorf("could not find moduledata bytes in data segments")
	}

	d := newDataIterator(b)
	vaddr, seg := d.SkipToDataOffset(offset)
	vm := vmem{Start: vaddr}
	vm.CopyAtAddress(vaddr, seg)

	size := int(unsafe.Sizeof(moduledata{}))

	// fill the vm with segments until it has enough data
	for !vm.Has(size) {
		vaddr, seg := d.Next()
		if seg == nil {
			panic("no more segment")
		}
		vm.CopyAtAddress(vaddr, seg)
	}

	b = vm.b
	md := moduledata{}
	// TODO: parse other fields?
	md.gofunc = ptr(binary.LittleEndian.Uint64(b[320:]))

	return md, nil
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

// vmem is a helper to rebuild virtual memory from data segments.
type vmem struct {
	// Virtual address of the first byte of memory.
	Start int64

	// pclntab layout format.
	Quantum byte
	Ptrsize int

	// Reconstructed memory buffer.
	b []byte
}

func (m *vmem) Has(addr int) bool {
	return addr < len(m.b)
}

func (m *vmem) PclntabOffset(word int) (uint64, bool) {
	s := 8 + word*m.Ptrsize
	e := s + 8
	if !m.Has(e) {
		return 0, false
	}
	res := binary.LittleEndian.Uint64(m.b[s:])
	return res, true
}

func (m *vmem) CopyAtAddress(addr int64, b []byte) {
	end := int64(len(m.b)) + m.Start
	if addr < end {
		panic(fmt.Errorf("address %d already mapped (end=%d)", addr, end))
	}
	size := len(m.b)
	zeroes := int(addr - end)
	total := zeroes + len(b) + size
	if cap(m.b) < total {
		new := make([]byte, total)
		copy(new, m.b)
		m.b = new
	} else {
		m.b = m.b[:total]
	}
	copy(m.b[size+zeroes:], b)

	if m.Start+int64(len(m.b)) != addr+int64(len(b)) {
		panic("invalid copy")
	}
}

// fid is the ID of a function, that is its number in the function section of
// the module, which includes imports. In a given module, fid = fidx+imports.
type fid int

func buildPclntabSymbolizer(wasmbin []byte, mod wazero.CompiledModule) (*pclntabmapper, error) {
	data := wasmdataSection(wasmbin)
	if data == nil {
		return nil, fmt.Errorf("no data section in the wasm binary")
	}
	pch, pclntab := pclntabFromData(data)
	md, err := moduledataFromData(pch, data)
	if err != nil {
		return nil, err
	}
	lt := gosym.NewLineTable(pclntab, 0)
	t, err := gosym.NewTable(nil, lt)
	if err != nil {
		return nil, err
	}
	return &pclntabmapper{
		t:          t,
		info:       lt.RuntimeInfo(),
		imported:   uint64(len(mod.ImportedFunctions())),
		modName:    mod.Name(),
		moduledata: md,
	}, nil
}

type pclntabmapper struct {
	t          *gosym.Table
	info       gosym.RuntimeInfo
	imported   uint64
	modName    string
	moduledata moduledata
}

func (p *pclntabmapper) Locations(gofunc experimental.InternalFunction, pc experimental.ProgramCounter) (uint64, []location) {
	// use the inline unwinder to retrieve all the location for this PC.

	// Assumption that pclntabmapper is only used in conjuction with
	// goStackIterator.
	f := gofunc.(goFunction)

	locs := []location{}

	var calleeFuncID gosym.FuncID

	iu, uf := newInlineUnwinder(p, f.mem, f.info, symPC(f.info, ptr(pc)))
	for ; uf.valid(); uf = iu.next(uf) {
		sf := iu.srcFunc(uf)
		if sf.FuncID == gosym.FuncIDWrapper && elideWrapperCalling(calleeFuncID) {
			// skip wrappers like stdlib
			continue
		}
		ipc := uf.pc
		calleeFuncID = sf.FuncID

		file, line, fn := p.t.PCToLine(uint64(ipc))
		if fn == nil {
			continue
		}
		locs = append(locs, location{
			File:       file,
			Line:       int64(line),
			StableName: fn.Name,
			HumanName:  fn.Name,
		})
	}

	// reverse locations to follow dwarf's convention
	for i, j := 0, len(locs)-1; i < j; i, j = i+1, j-1 {
		locs[i], locs[j] = locs[j], locs[i]
	}

	return uint64(pc), locs
}

// https://github.com/golang/go/blob/4859392cc29a35a0126e249ecdedbd022c755b20/src/cmd/link/internal/wasm/asm.go#L45
const funcValueOffset = 0x1000

func (p *pclntabmapper) PCToFID(pc ptr) fid {
	return fid(uint64(pc)>>16 + p.imported - funcValueOffset)
}

func (p *pclntabmapper) FIDToPC(f fid) ptr {
	return ptr((funcValueOffset + f - fid(p.imported)) << 16)
}

func (p *pclntabmapper) PCToName(pc ptr) string {
	f := p.t.PCToFunc(uint64(pc))
	if f == nil {
		return ""
	}
	return f.Name
}

// ptr represents a unintptr in the original unwinder code. Here, the unwinder
// executes in the host, so this type helps to avoid dereferencing the host
// memory.
type ptr uint64

// gptr represents a *g in the original code. It exists for the same reason as
// ptr, but is a separate type to avoid confusion between the two. The main
// difference is a gPtr is not supposed to have arithmetic done on it outside
// rtmem. Also, easier to replace guintptr with a dedicated type.
type gptr uint64

// wrapper around Wazero's Memory to provide helpers for the implementation of
// unwinder.
//
// Note: we could implement deref generically by reading the right number of
// bytes for the shape and unsafe cast to the desired type. However, this would
// break if the host is not little endian or uses a different pointer size type.
// Taking the longer route here of providing dedicated function that perform
// explicit endianess conversions, but this can probably be made faster with the
// generic method in our most common architectures.
type rtmem struct {
	api.Memory
}

func (r rtmem) readU64(p ptr) uint64 {
	x, ok := r.ReadUint64Le(uint32(p))
	if !ok {
		panic("invalid pointer dereference")
	}
	return x
}

// equivalent to *uintptr.
func (r rtmem) derefPtr(p ptr) ptr {
	return ptr(r.readU64(p))
}

// Reads the index-th element of a slice that starts at address p.
func readSliceIndex[T any](r rtmem, p ptr, index int32) T {
	var t T
	s := uint32(unsafe.Sizeof(t))
	b, ok := r.Read(uint32(p)+uint32(index)*s, s)
	if !ok {
		panic("invalid slice index read")
	}
	t = *(*T)(unsafe.Pointer((unsafe.SliceData(b))))
	return t
}

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

func (r rtmem) gM(g gptr) ptr {
	return ptr(r.readU64(ptr(g) + 8*6))
}

func (r rtmem) gMG0(g gptr) gptr {
	m := r.gM(g)
	return gptr(r.readU64(m + 0))
}

func (r rtmem) gMCurg(g gptr) gptr {
	m := r.gM(g)
	return gptr(r.readU64(m + 144))
}

func (r rtmem) gSchedSp(g gptr) ptr {
	return ptr(r.readU64(ptr(g) + 8*7))
}

func (r rtmem) gSchedPc(g gptr) ptr {
	return ptr(r.readU64(ptr(g) + 8*8))
}

func (r rtmem) gSchedLr(g gptr) ptr {
	return ptr(r.readU64(ptr(g) + 8*12))
}

// goStackIterator iterates over the physical frames of the Go stack. It is up
// to the symbolizer (pclntabmapper) to expand those into logical frames to
// account for inlining.
type goStackIterator struct {
	first bool
	rt    *Runtime
	pc    ptr
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
	// TODO
	return nil
}

var _ experimental.StackIterator = (*goStackIterator)(nil)

// elideWrapperCalling reports whether a wrapper function that called
// function id should be elided from stack traces.
func elideWrapperCalling(id gosym.FuncID) bool {
	// If the wrapper called a panic function instead of the
	// wrapped function, we want to include it in stacks.
	return !(id == gosym.FuncID_gopanic || id == gosym.FuncID_sigpanic || id == gosym.FuncID_panicwrap)
}

// goFunction is a lazy implementation of wazero's FunctionDefinition and
// InternalFunction, as the goStackIterator cannot map to an internal *function
// in wazero.
type goFunction struct {
	mem  rtmem
	sym  *pclntabmapper
	info *gosym.FuncInfo
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

type moduledata struct {
	pcHeader              ptr       // 0
	funcnametab           []byte    // 8
	cutab                 []uint32  // 32
	filetab               []byte    // 56
	pctab                 []byte    // 80
	pclntable             []byte    // 104
	ftab                  []functab // 128
	findfunctab           ptr       // 152
	minpc, maxpc          ptr       // 160
	text, etext           ptr       // 176
	noptrdata, enoptrdata ptr       // 192
	data, edata           ptr       // 208
	bss, ebss             ptr       // 224
	noptrbss, enoptrbss   ptr       // 240
	covctrs, ecovctrs     ptr       // 256
	end, gcdata, gcbss    ptr       // 272
	types, etypes         ptr       // 296
	rodata                ptr       // 312
	gofunc                ptr       // 320 go.func.*
	//
	//textsectmap []textsect
	//typelinks   []int32 // offsets from types
	//itablinks   []*itab
	//
	//ptab []ptabEntry
	//
	//pluginpath string
	//pkghashes  []modulehash
	//
	//// This slice records the initializing tasks that need to be
	//// done to start up the program. It is built by the linker.
	//inittasks []*initTask
	//
	//modulename   string
	//modulehashes []modulehash
	//
	//hasmain uint8 // 1 if module contains the main function, 0 otherwise
	//
	//gcdatamask, gcbssmask bitvector
	//
	//typemap map[uint32]uintptr // offset to *_rtype in previous module
	//
	//bad bool // module failed to load and should be ignored
	//
	//next *moduledata
}
