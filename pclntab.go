package wzprof

import (
	"bytes"
	"debug/gosym"
	"encoding/binary"
	"fmt"

	"github.com/tetratelabs/wazero"
)

func moduleIsGo(mod wazero.CompiledModule) bool {
	// TODO: need to figure out a reliable way that this code has been generated
	// by golang/go.
	return false
}

type section struct {
	// Offset since start of binary of the first byte of the section (after the
	// section size).
	Offset uint64
	Data   []byte
}

func (s section) Valid() bool {
	return s.Data != nil
}

// wasmbin parses a WASM binary and returns the bytes of the WASM "Code" and
// "Data" sections. Returns nils if the sections do not exist.
//
// It is a very weak parser: it should be called on a valid module, or it may
// panic.
//
// This function exists because Wazero doesn't expose the Code and Data sections
// on its CompiledModule and they are needed to retrieve pclntab on Go-compiled
// modules.
func wasmbinSections(b []byte) (imports, code, data, name section) {
	const (
		customSectionId = 0
		importSectionId = 2
		codeSectionId   = 10
		dataSectionId   = 11
	)

	offset := uint64(0)

	b = b[8:] // skip magic+version
	offset += 8
	for len(b) > 0 {
		id := b[0]
		b = b[1:]
		offset++
		length, n := binary.Uvarint(b)
		b = b[n:]
		offset += uint64(n)
		switch id {
		case importSectionId:
			imports = section{offset, b[:length]}
		case codeSectionId:
			code = section{offset, b[:length]}
		case dataSectionId:
			data = section{offset, b[:length]}
		case customSectionId:
			if data.Valid() { // in order: import, code, data, name
				// check name to be 'name'
				nameLen, n := binary.Uvarint(b)
				x := string(b[n : n+int(nameLen)])
				if "name" == x {
					offset += uint64(n) + nameLen
					b = b[uint64(n)+nameLen:]
					name = section{offset, b[:length-uint64(n)-nameLen]}
					return
				}
			}
		}
		b = b[length:]
		offset += length
	}
	return section{}, section{}, section{}, section{}
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

// SkipToOffset iterates over segments to return the bytes at a given data
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

// pclntabFromData rebuilds the full pclntab from the segments of the Data
// section of the module (b).
//
// Assumes the section is well-formed, and the segment has the layout described
// in the 1.20.1 linker. Returns nil if the segment is missing. Does not check
// whether pclntab contains actual useful data.
//
// See layout in the linker: https://github.com/golang/go/blob/3e35df5edbb02ecf8efd6dd6993aabd5053bfc66/src/cmd/link/internal/wasm/asm.go#L169-L185
func pclntabFromData(data section) []byte {
	b := data.Data
	// magic number of the start of pclntab for Go 1.20, little endian.
	magic := []byte{0xf1, 0xff, 0xff, 0xff, 0x00, 0x00}
	pclntabOffset := bytes.Index(b, magic)
	if pclntabOffset == -1 {
		return nil
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
			x, err := vm.PclntabOffset(word)
			if err == nil {
				return x
			}
			if err == fault {
				vaddr, seg := d.Next()
				if seg == nil {
					panic("no more segment")
				}
				vm.CopyAtAddress(vaddr, seg)
			} else {
				panic("unhandled error")
			}
		}
	}

	nfunctab := readWord(0)
	nfiletab := readWord(1)
	pcstart := readWord(2)
	funcnametabAddr := readWord(3)
	cutabAddr := readWord(4)
	filetabAddr := readWord(5)
	pctabAddr := readWord(6)
	funcdataAddr := readWord(7)
	functabAddr := readWord(7)

	fmt.Println("nfunctab:", nfunctab)
	fmt.Println("nfiletab:", nfiletab)
	fmt.Println("pcstart:", pcstart)
	fmt.Println("funcnametabAddr:", funcnametabAddr)
	fmt.Println("cutabAddr:", cutabAddr)
	fmt.Println("filetabAddr:", filetabAddr)
	fmt.Println("pctabAddr:", pctabAddr)
	fmt.Println("funcdataAddr:", funcdataAddr)
	fmt.Println("functabAddr:", functabAddr)

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

	return vm.b
}

// https://pkg.go.dev/debug/gosym@go1.20.4

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

var fault error = fmt.Errorf("segment fault")

func (m *vmem) Has(addr int) bool {
	return addr < len(m.b)
}

func (m *vmem) PclntabOffset(word int) (uint64, error) {
	s := 8 + word*m.Ptrsize
	e := s + 8

	if !m.Has(e) {
		return 0, fault
	}

	res := binary.LittleEndian.Uint64(m.b[s:])

	fmt.Printf("word=%d -> addr=%d :: res=%d\n", word, s, res)
	return res, nil
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

type fnRange struct {
	OffsetStart uint64
	OffsetEnd   uint64
	FnID        uint64
}

type codemap struct {
	fnmaps []funcmap
}

// Return the index of the first needle opcode in this block. Ignores opcodes
// inside nested blocks. -1 if not found.
func findInBlock(needle []byte, hay []byte) int {
	i := 0
	for i+len(needle) <= len(hay) {
		b := hay[i:]
		if bytes.HasPrefix(b, needle) {
			return i
		}

		// end of the current block
		if b[0] == 0x0B {
			i++
			break
		}

		i += skipInstr(b)
	}
	return -1
}

func skipInstr(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	o := b[0]
	i := 1

	if o >= 0x45 && o <= 0xC4 {
		// no argument
		return i
	}

	// TODO: handle missing opcodes
	switch o {
	// No argument.
	case 0x00, 0x01, 0x0F, 0xD1, 0x1A, 0x1B:

	case 0x02: // block
		_, n := sleb128(33, b[i:]) // blocktype
		i += n
		i += skipExpr(b[i:])

	case 0x03:
		_, n := sleb128(33, b[i:]) // blocktype
		i += n
		i += skipExpr(b[i:])
	case 0x04:
		_, n := sleb128(33, b[i:]) // blocktype
		i += n
		i += skipIf(b[i:])

	// 1 u32 argument
	case 0x0C, 0x0D, 0x10, 0xD2, 0x20, 0x21, 0x22, 0x23, 0x24, 0x25, 0x26:
		_, n := binary.Uvarint(b[i:])
		i += n

	// 1 s32 arg
	case 0x41:
		_, n := sleb128(32, b[i:])
		i += n
	// 1 s64 arg
	case 0x42:
		_, n := sleb128(64, b[i:])
		i += n

	// br_table
	case 0x0E:
		c, n := binary.Uvarint(b[i:])
		i += n
		for j := 0; j < int(c); j++ {
			_, n := binary.Uvarint(b[i:])
			i += n
		}
		_, n = binary.Uvarint(b[i:])
		i += n

	// 2 u32 arguments
	case 0x11, 0x28, 0x29, 0x2A, 0x2B, 0x2C, 0x2D, 0x2E, 0x2F, 0x30, 0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x37, 0x38, 0x39, 0x3A, 0x3B, 0x3C, 0x3D, 0x3E:
		_, n := binary.Uvarint(b[i:])
		i += n
		_, n = binary.Uvarint(b[i:])
		i += n

	// 1 byte argument
	case 0xD0:
		i++

	// vector of bytes
	case 0x1C:
		x, n := binary.Uvarint(b[i:])
		i += n + int(x)

	case 0xFC:
		x, n := binary.Uvarint(b[i:])
		i += n
		switch x {
		case 12, 14:
			_, n := binary.Uvarint(b[i:])
			i += n
			_, n = binary.Uvarint(b[i:])
			i += n
		default:
			_, n := binary.Uvarint(b[i:])
			i += n
		}
	default:
		panic(fmt.Errorf("unhandled opcode: %x", o))
	}
	return i
}

func skipIf(b []byte) int {
	i := 0
	for len(b) > 1 {
		if b[i] == 0x05 {
			i++
			continue
		}
		if b[i] == 0x0B {
			i++
			break
		}
		i += skipInstr(b[i:])
	}
	return i
}

// returns how many bytes were skipped
func skipExpr(b []byte) int {
	i := 0
	for len(b) > 1 {
		if b[i] == 0x0B {
			i++
			return i
		}
		i += skipInstr(b[i:])
	}
	return i
}

type funcmap struct {
	Name  string
	Start uint64 // offset from start of Code
	End   uint64 // offset from start of Code

	Frame  int
	Jumps  []int    // maps PC_B to block number
	Blocks [][2]int // [start (offset from fnstart), end (offset from fnstart)]

	// for debugging
	ID int
}

func parseFnCode(b []byte, startfunc int) funcmap {
	if len(b) < 6 {
		// Need at least these instructions to have a non 0 frame:
		// 01 7f       | local[0] type=i32
		// 23 00       | global.get 0
		// 21 01       | local.set 1
		// And those for a jump table:
		// 02 40       | block
		// 0e 01 00 01 | br_table 0 1
		// 0b          | end
		return funcmap{}
	}
	offset := 0

	// start with locals
	localsCount, n := binary.Uvarint(b)
	b = b[n:]
	offset += n
	for i := 0; i < int(localsCount); i++ {
		_, n := binary.Uvarint(b) // count of locals of that type
		b = b[n+1:]               // +1 because valtype is 1 byte
		offset += n + 1
	}

	if len(b) < 4 {
		return funcmap{}
	}

	// 23 00       | global.get 0
	// 21 01       | local.set 1
	if len(b) < 4 {
		return funcmap{}
	}
	b = b[4:]
	offset += 4

	// 12e697: 02 40                      | block
	// 12e699: 02 40                      |   block
	// 12e69b: 02 40                      |     block
	// 12e69d: 02 40                      |       block
	// 12e69f: 02 40                      |         block
	// 12e6a1: 02 40                      |           block
	// 12e6a3: 02 40                      |             block
	// 12e6a5: 20 00                      |               local.get 0
	// 12e6a7: 0e 12 00 00 00 00 00 00 00 |               br_table 0 0 0 0 0 0 0 1 1 2 3 3 3 3 3 3 4 4 5
	// 12e6b0: 01 01 02 03 03 03 03 03 03 |
	// 12e6b9: 04 04 05                   |
	// 12e6bc: 0b                         |             end

	fm := funcmap{}
	blockDepth := 0
	for len(b) >= 3 { // 02 40 ... 0b
		if b[0] == 0x02 && b[1] == 0x40 {
			blockDepth++
			b = b[2:]
			offset += 2
			continue
		}
		if b[0] == 0x20 && b[1] == 0x00 && b[2] == 0x0E {
			fm.Blocks = make([][2]int, blockDepth)

			// The block containing br_table *does not count* when performing
			// the jump. Meaning block 0 is the parent of this block.

			b = b[3:]
			offset += 3
			// expect br_table
			x, n := binary.Uvarint(b)
			b = b[n:]
			offset += n
			fm.Jumps = make([]int, x)
			for i := 0; i < int(x); i++ {
				v, n := binary.Uvarint(b)
				b = b[n:]
				offset += n
				fm.Jumps[i] = int(v)
				if int(v) > len(fm.Blocks)-1 {
					fmt.Println("warning: jump table pointing to unknown block")
				}
			}
			_, n = binary.Uvarint(b)
			b = b[n:]
			offset += n

			// expect end
			if b[0] != 0x0B {
				return funcmap{}
			}
			b = b[1:]
			offset += 1

			break
		}
		// unknown pattern, bail
		return funcmap{}
	}

	// Mark the beginning of block 0
	fm.Blocks[len(fm.Blocks)-blockDepth][0] = startfunc + offset

	// Try to figure out the frame size. Look forward to find the `local.tee 1`
	// in this block, skipping over inside blocks (such as ifs), then back track
	// to retrieve the value of the constant.
	//
	// 12e6e8: 20 01                      |             local.get 1
	// 12e6ea: 41 f0 00                   |             i32.const 112
	// 12e6ed: 6b                         |             i32.sub
	// 12e6ee: 22 01                      |             local.tee 1
	// 12e6f0: 24 00                      |             global.set 0
	i := findInBlock([]byte{0x22, 0x01}, b)
	if i >= 0 {
		//		fmt.Printf("found local.tee at: %x\n", offset+startfunc+i)

		// backtrack until the start of i32.const
		i--
		i-- // i32.sub (0x6B)
		i-- // the last byte of the operand
		for ; i > 0; i-- {
			if b[i] == 0x41 {
				break
			}
			if b[i]&0x80 == 0 {
				fmt.Println("warning: only continuation bytes are expected until 0x41")
				// TODO: still try to fix blocks
				return funcmap{}
			}
		}
		// i now points to 0x41.
		i++
		size, n := sleb128(32, b[i:])
		fm.Frame = int(size)
		i += n
		b = b[i:]
		offset += i
	}

	for blockDepth > 0 {
		i = findInBlock([]byte{0x0B}, b)
		if i < 0 {
			fmt.Println(b)
			panic("unfinished block")
		}
		b = b[i+1:]
		offset += i + 1 // +1 to include the end opcode.

		fm.Blocks[len(fm.Blocks)-blockDepth][1] = startfunc + offset
		blockDepth--
		if blockDepth > 0 {
			fm.Blocks[len(fm.Blocks)-blockDepth][0] = startfunc + offset
		}
	}

	return fm
}

//
// https://github.com/stealthrocket/go/blob/7213f2e72003325df2cebb731de838ac01f20fb6/src/cmd/internal/obj/wasm/wasmobj.go#L357-L364

type funcNameIter struct {
	b     []byte
	ready bool
	count int
}

// For reads the names table to find the name for the given function index.
// Returns empty string if not found (though empty string is a valid function
// name). As names are stored in increasing function indexes, this function must
// be called in increasing order index.
func (it *funcNameIter) For(idx uint32) string {
	if !it.ready {
		// Assume b contains the whole section. Go to the function names
		// subsection and replace b with it.
		b := it.b
		for len(b) > 1 {
			id := b[0]
			size, n := binary.Uvarint(b[1:])
			const functionNamesSubsection = 1
			b = b[1+uint64(n):]
			if id == functionNamesSubsection {
				it.b = b[:size]
				it.ready = true
				count, n := binary.Uvarint(it.b)
				it.b = b[n:]
				it.count = int(count)
				break
			}
			b = b[size:]
		}
		if len(b) == 0 {
			panic("function name section not found")
		}
	}
	// A name map assigns names to indices in a given index space. It consists of a
	// vector of index/name pairs in order of increasing index value. Each index must
	// be unique, but the assigned names need not be.

	for it.count > 0 {
		i, n := binary.Uvarint(it.b)
		size, n2 := binary.Uvarint(it.b[n:])
		o := n + n2
		if uint32(i) == idx {
			name := string(it.b[o : o+int(size)])
			it.b = it.b[o+int(size):]
			it.count--
			return name
		}
		if uint32(i) > idx {
			// Do not consume the bytes, as the next call may need them.
			return ""
		}
		it.b = it.b[o+int(size):]
		it.count--
	}
	return ""
}

const codeSecOffset = 0x001277 // offset of the code section in this wasm binary.

func functionImportsCount(imports section) uint32 {
	fncount := uint32(0)
	b := imports.Data
	count, n := binary.Uvarint(b)
	b = b[n:]
	for i := uint64(0); i < count; i++ {
		// skip module name
		s, n := binary.Uvarint(b)
		b = b[uint64(n)+s:]
		// skip value name
		s, n = binary.Uvarint(b)
		b = b[uint64(n)+s:]
		kind := b[0]
		b = b[1:]
		switch kind {
		case 0x00: // function
			fncount++
			_, n = binary.Uvarint(b) // skip typeid
			b = b[uint64(n):]
		case 0x01:
			b = b[1:] //reftype
			fallthrough
		case 0x02:
			hasmax := b[0] == 1
			b = b[1:]
			_, n = binary.Uvarint(b) // skip min
			b = b[uint64(n):]
			if hasmax {
				_, n = binary.Uvarint(b) // skip max
				b = b[uint64(n):]
			}
		case 0x03:
			b = b[2:] // valtype + mut
		}
	}
	return fncount
}

func buildCodemap(code, name, imports section) codemap {
	startFuncIdx := functionImportsCount(imports)
	fnit := funcNameIter{b: name.Data}

	b := code.Data
	// https://webassembly.github.io/spec/core/binary/modules.html#binary-codesec
	offset := uint64(0)

	count, n := binary.Uvarint(b)
	b = b[n:]
	offset += uint64(n)

	fnmaps := make([]funcmap, 0, count)

	for i := 0; i < int(count); i++ {
		funcIdx := startFuncIdx + uint32(i)
		size, n := binary.Uvarint(b)
		offset += uint64(n)
		b = b[n:]
		fncode := b[:int(size)]

		fnmap := parseFnCode(fncode, int(offset))
		fnmap.Name = fnit.For(funcIdx)
		fnmap.ID = i
		fnmap.Start = offset

		b = b[int(size):]
		offset += size

		fnmap.End = offset
		fnmaps = append(fnmaps, fnmap)

		fmt.Printf("func[%d] at %x-%x :: framesize=%d\n", fnmap.ID+14, fnmap.Start+codeSecOffset, fnmap.End+codeSecOffset, fnmap.Frame)
		if len(fnmap.Jumps) > 0 {
			fmt.Printf("\tJumps:")
			for i, x := range fnmap.Jumps {
				fmt.Printf(" %d->%d", i, x)
			}
			fmt.Println("")
		}
		for i, block := range fnmap.Blocks {
			fmt.Printf("\tBlock %d: %x -> %x\n", i, codeSecOffset+fnmap.Start+uint64(block[0]), codeSecOffset+fnmap.Start+uint64(block[1]))
		}
	}

	if len(b) != 0 {
		panic("leftover bytes")
	}

	return codemap{fnmaps: fnmaps}
}

type pclntabmapper struct {
	m codemap
	t *gosym.Table
}

func BuildPclntabSymbolizer(wasmbin []byte) (Symbolizer, error) {
	imports, code, data, name := wasmbinSections(wasmbin)
	codemap := buildCodemap(code, name, imports)
	pclntab := pclntabFromData(data)

	lt := gosym.NewLineTable(pclntab, 0)
	t, err := gosym.NewTable(nil, lt)
	if err != nil {
		return nil, err
	}

	//const funcValueOffset = 0x1000
	//for fn := 0; fn <= 1337-14; fn++ {
	//	m := 6
	//	if fn == 1323 {
	//		m = 100
	//	}
	//	for pcb := 0; pcb <= m; pcb++ {
	//		pc := (funcValueOffset+uint64(fn))<<16 | uint64(pcb)
	//		fmt.Printf("PC_F=%d, PC_B=%d, pc=%d: ", fn, pcb, pc)
	//		file, line, _ := t.PCToLine(pc)
	//		fmt.Println(file, line)
	//	}
	//}
	//panic("STOP")

	return pclntabmapper{
		m: codemap,
		t: t,
	}, nil
}

func (p pclntabmapper) LocationsForSourceOffset(offset uint64) []Location {
	var pc uint64

	// https://github.com/golang/go/blob/3e35df5edbb02ecf8efd6dd6993aabd5053bfc66/src/cmd/link/internal/wasm/asm.go#L45
	const funcValueOffset = 0x1000
	/*
		for idx, f := range p.m.fnmaps {
			fmt.Println("->", idx, f.ID, len(f.Blocks))
		}
	*/

	for idx, f := range p.m.fnmaps {
		if f.Start <= offset && offset < f.End {
			for j := idx + 1; j < len(p.m.fnmaps); j++ {
				if p.m.fnmaps[j].Start <= offset && offset < p.m.fnmaps[j].End {
					panic("there is another match")
				}
			}

			// https://github.com/golang/go/blob/3e35df5edbb02ecf8efd6dd6993aabd5053bfc66/src/cmd/link/internal/wasm/asm.go#L142-L158
			pcF := (funcValueOffset + uint64(f.ID)) << 16

			fmt.Println("matched fn", f.ID, len(f.Blocks))

			blockNum := -1
			for i, b := range f.Blocks {
				if b[0] <= int(offset) && int(offset) < b[1] {
					blockNum = i
					fmt.Println("--->block", i)
					break
				}
			}
			if blockNum == -1 {
				fmt.Println("warning: matched function but not block")
				fmt.Println(idx)
				fmt.Println(offset)
				fmt.Println(len(f.Blocks))
				return nil
			}

			pcB := uint64(len(f.Blocks)) // default to any PC_B not registered to a block.
			for x, blk := range f.Jumps {
				if blk == blockNum {
					pcB = uint64(x)
					break
				}
			}

			pc = pcF | pcB
			break
		}
	}

	file, line, fn := p.t.PCToLine(pc)
	if fn == nil {
		return nil
	}

	return []Location{{
		File:         file,
		Line:         int64(line),
		SourceOffset: pc,
		// TODO: names
	}}
}
