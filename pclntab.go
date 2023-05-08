package wzprof

import (
	"bytes"
	"debug/gosym"
	"encoding/binary"
	"fmt"

	"github.com/tetratelabs/wazero"
)

func moduleIsGo(mod wazero.CompiledModule) bool {
	// TODO
	return false
}

// wasmbinData parses a WASM binary and returns the bytes of the WASM "Data"
// section. Returns nil if the section does not exist.
//
// It is a very weak parser: it should be called on a valid module or it may
// panic.
//
// This function exists because Wazero doesn't expose the Data section on its
// CompiledModule and it is needed to retrieve pclntab on Go-compiled modules.
func wasmbinData(b []byte) []byte {
	const dataSectionId = 11

	b = b[8:] // skip magic+version
	for len(b) > 0 {
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

// godebugSections returns the symtab and pclntab segments from the provided
// bytes of the Data section. Assumes the section is well formed, and the
// segments are positioned at the layout described in the linker. If either
// segment is missing, this function returns nil,nil. It does not check whether
// symtab and pclntab contains actual useful data.
//
// See layout in the linker: https://github.com/golang/go/blob/3e35df5edbb02ecf8efd6dd6993aabd5053bfc66/src/cmd/link/internal/wasm/asm.go#L169-L185
//
// See https://docs.google.com/document/d/1lyPIbmsYbXnpNj57a261hgOYVpNRcgydurVQIyZOz_o/pub for 0xfffffffb

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

// Return the bytes of the next segment, and its offset in virtual memory, or a
// nil slice if there is no more segment.
func (d *dataIterator) Next() (offset int64, seg []byte) {
	if d.n == 0 {
		return 0, nil
	}

	start := d.offset

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

	fmt.Printf("READ SEGMENT: offset=%d vaddr=%d len=%d\n", start, offset, length)

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
			fmt.Println("SKIPPED")
			continue
		}
		o := len(seg) + offset - d.offset
		return vaddr + int64(o), seg[o:]
	}

	return 0, nil
}

// pclntabFromData rebuilds the full pclntab from the segments of the Data
// section of the module (b).
func pclntabFromData(b []byte) []byte {
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
	fmt.Println("END:", end)
	fillUntil(int(end))

	// hack
	fillUntil(300256)

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
	fmt.Println("TOTAL VM SIZE:", len(m.b))
}

type pclntabmapper struct {
	t       *gosym.Table
	pcstart uint64
}

func (p pclntabmapper) Lookup(s stackEntry) []location {
	fmt.Println("fn:", s.fn.Name(), "pc:", s.pc, "into:", s.pc+p.pcstart)
	pc := s.pc
	file, line, fn := p.t.PCToLine(pc + p.pcstart)
	if fn == nil {
		fmt.Println("could not lookup", pc)
		return nil
	}

	fmt.Println("FOUND PC!", pc)
	fmt.Println("--->", file, "line:", line)
	fmt.Println("->", fn)

	return []location{{
		File: file,
		Line: int64(line),
		PC:   pc,
		// TODO: names
	}}
}

func newPclntabmapper(pclntab []byte) (mapper, error) {
	fmt.Println("pclntab size:", len(pclntab))
	lt := gosym.NewLineTable(pclntab, 0)
	t, err := gosym.NewTable(nil, lt)
	if err != nil {
		return nil, err
	}
	pcstart := t.Funcs[0].Entry
	return pclntabmapper{t: t, pcstart: pcstart}, nil
}
