package main

import (
	"encoding/binary"
	"fmt"
	"reflect"
	"unsafe"
)

type pcHeader struct {
	magic          uint32  // 0xFFFFFFF1
	pad1, pad2     uint8   // 0,0
	minLC          uint8   // min instruction size
	ptrSize        uint8   // size of a ptr in bytes
	nfunc          int     // number of functions in the module
	nfiles         uint    // number of entries in the file tab
	textStart      uintptr // base for function entry PC offsets in this module, equal to moduledata.text
	funcnameOffset uintptr // offset to the funcnametab variable from pcHeader
	cuOffset       uintptr // offset to the cutab variable from pcHeader
	filetabOffset  uintptr // offset to the filetab variable from pcHeader
	pctabOffset    uintptr // offset to the pctab variable from pcHeader
	pclnOffset     uintptr // offset to the pclntab variable from pcHeader
}

type functab struct {
	entryoff uint32 // relative to runtime.text
	funcoff  uint32
}

type textsect struct {
	vaddr    uintptr // prelinked section vaddr
	end      uintptr // vaddr + section length
	baseaddr uintptr // relocated section address
}

type itab struct {
	inter uintptr
	_type uintptr
	hash  uint32 // copy of _type.hash. Used for type switches.
	_     [4]byte
	fun   [1]uintptr // variable sized. fun[0]==0 means _type does not implement inter.
}

type ptabEntry struct {
	name int32
	typ  int32
}

type modulehash struct {
	modulename   string
	linktimehash string
	runtimehash  *string
}

type initTask struct {
	state uint32 // 0 = uninitialized, 1 = in progress, 2 = done
	nfns  uint32
	// followed by nfns pcs, uintptr sized, one per init function to run
}

type bitvector struct {
	n        int32 // # of bits
	bytedata *uint8
}

type moduledata struct {
	//	sys.NotInHeap // Only in static data

	pcHeader    *pcHeader // 0
	funcnametab []byte    // 8
	cutab       []uint32  // 32
	filetab     []byte    // 56
	pctab       []byte    // 80
	pclntable   []byte    // 104
	ftab        []functab // 128
	findfunctab uintptr   // 152

	// -- minpc and max pc can be zeroes
	minpc uintptr // 160
	maxpc uintptr

	text, etext           uintptr
	noptrdata, enoptrdata uintptr
	data, edata           uintptr
	bss, ebss             uintptr
	noptrbss, enoptrbss   uintptr
	covctrs, ecovctrs     uintptr
	end, gcdata, gcbss    uintptr
	types, etypes         uintptr
	rodata                uintptr
	gofunc                uintptr // go.func.*

	textsectmap []textsect
	typelinks   []int32 // offsets from types
	itablinks   []*itab

	ptab []ptabEntry

	pluginpath string
	pkghashes  []modulehash

	// This slice records the initializing tasks that need to be
	// done to start up the program. It is built by the linker.
	inittasks []*initTask

	modulename   string
	modulehashes []modulehash

	hasmain uint8 // 1 if module contains the main function, 0 otherwise

	gcdatamask, gcbssmask bitvector

	typemap map[uint32]uintptr // offset to *_rtype in previous module

	bad bool // module failed to load and should be ignored

	next *moduledata
}

//go:linkname firstmoduledata runtime.firstmoduledata
var firstmoduledata moduledata

func main() {
	println("hello")
	println(unsafe.Pointer(&firstmoduledata))
	m := firstmoduledata
	fmt.Println("pcHeader:", uint64(uintptr(unsafe.Pointer(m.pcHeader))))
	fmt.Println("minpc=", m.minpc)
	fmt.Println("maxpc=", m.maxpc)

	if m.pcHeader.magic != 0xFFFFFFF1 || m.pcHeader.pad1 != 0x0 || m.pcHeader.pad2 != 0x0 {
		panic("pcHeader.magic invalid")
	}

	hdr := (*reflect.SliceHeader)(unsafe.Pointer(&m.funcnametab))
	fmt.Println("funcnametab:", "Data=", hdr.Data, "Len=", hdr.Len, "Cap=", hdr.Cap)

	target := []byte{}
	t := (*reflect.SliceHeader)(unsafe.Pointer(&target))
	t.Data = uintptr(unsafe.Pointer(&firstmoduledata))
	t.Len = int(unsafe.Sizeof(moduledata{}))
	t.Cap = t.Len

	fmt.Println("SIZE OF MODULEDATA:", t.Len)

	fmt.Println(target)

	start := 0
	inzeroes := false
	lowest := -1
	for i, b := range target {
		if inzeroes {
			if b == 0 {
				continue
			} else {
				inzeroes = false
				rl := i - start
				if rl >= 8 {
					lowest = start
					break
				}
			}
		} else {
			if b == 0 {
				inzeroes = true
				start = i
			} else {
				continue
			}
		}
	}

	if inzeroes && lowest < 0 {
		rl := len(target) - start
		if rl >= 8 {
			lowest = start
		}
	}

	fmt.Println("lowest=", lowest)

	wordsTarget := []uint64{}
	t = (*reflect.SliceHeader)(unsafe.Pointer(&wordsTarget))
	t.Data = uintptr(unsafe.Pointer(&firstmoduledata))
	t.Len = int(unsafe.Sizeof(moduledata{})) / 8
	t.Cap = t.Len
	// for i, x := range wordsTarget {
	// 	fmt.Println(i, ":", i*8, ":", x)
	// }

	// first, let's assume we can reliably find the address of pcHeader
	// using linear search for its magic header, and are able to decode its
	// contents.
	needle := make([]byte, (6*3+1)*8) // 1 address + 3 slices

	pchp := uint64(uintptr(unsafe.Pointer(m.pcHeader)))
	funcnametabp := pchp + uint64(m.pcHeader.funcnameOffset)
	cutabp := pchp + uint64(m.pcHeader.cuOffset)

	// linear scan of funcnametab fo find zeroes, which indicates the number
	// of entries (it's a collection of null-terminated strings).
	funcnametabn := uint64(0)
	target = []byte{}
	t = (*reflect.SliceHeader)(unsafe.Pointer(&target))
	t.Data = uintptr(funcnametabp)
	t.Len = int(cutabp - funcnametabp)
	t.Cap = t.Len
	for _, x := range target {
		if x == 0 {
			//			fmt.Println("->", string(target[lastI:i]))

			funcnametabn++
		}
	}
	fmt.Println(funcnametabn)

	// *pcHeader
	binary.LittleEndian.PutUint64(needle[0:8], pchp)
	// funcnametab
	binary.LittleEndian.PutUint64(needle[8:16], funcnametabp)  // Data
	binary.LittleEndian.PutUint64(needle[16:24], funcnametabn) // Len
	binary.LittleEndian.PutUint64(needle[24:32], funcnametabn) // Cap
	// cutab
	binary.LittleEndian.PutUint64(needle[32:40], cutabp) // Data

	fmt.Println(needle)
}
