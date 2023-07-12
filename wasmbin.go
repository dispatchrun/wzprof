package wzprof

import (
	"encoding/binary"
	"fmt"
)

// Returns true if the wasm module binary b contains a custom section with this
// name.
func wasmHasCustomSection(b []byte, name string) bool {
	return wasmCustomSection(b, name) != nil
}

// Returns the byte content of a custom section with name, or nil.
func wasmCustomSection(b []byte, name string) []byte {
	const customSectionId = 0
	if len(b) < 8 {
		return nil
	}
	b = b[8:] // skip magic+version
	for len(b) > 2 {
		id := b[0]
		b = b[1:]
		length, n := binary.Uvarint(b)
		b = b[n:]

		if id == customSectionId {
			nameLen, n := binary.Uvarint(b)
			b = b[n:]
			m := string(b[:nameLen])
			if m == name {
				return b[nameLen : length-uint64(n)]
			}
			b = b[length-uint64(n):]
		} else {
			b = b[length:]
		}
	}
	return nil
}

// The functions in this file inspect the contents of a well-formed wasm-binary.
// They are very weak parsers: they should be called on a valid module, or may
// panic. Eventually this code should be replaced by exposing the right APIs
// from wazero to access data and segments.

// wasmdataSection parses a WASM binary and returns the bytes of the WASM "Data"
// section. Returns nil if the sections do not exist.
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

	offset int // offset of b in the Data section.
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

// Next returns the bytes of the following segment, and its address in virtual
// memory, or a nil slice if there are no more segment.
func (d *dataIterator) Next() (vaddr int64, seg []byte) {
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

	vaddr = d.varint()

	v = d.byte()
	if v != 0x0B {
		panic(fmt.Errorf("expected end of expr (0x0B); got %#x", v))
	}

	length := d.uvarint()
	seg = d.read(int(length))
	d.n--

	return vaddr, seg
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

// vmemb is a helper to rebuild virtual memory from data segments.
type vmemb struct {
	// Virtual address of the first byte of memory.
	Start int64
	// Reconstructed memory buffer.
	b []byte
}

func (m *vmemb) Has(addr int) bool {
	return addr < len(m.b)
}

func (m *vmemb) CopyAtAddress(addr int64, b []byte) {
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
