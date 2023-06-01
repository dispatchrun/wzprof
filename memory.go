//go:build amd64 || arm64

package wzprof

import (
	"fmt"
	"reflect"
	"unsafe"
)

// ptr represents an address in the guest memory. It replaces unintptr in the
// original unwinder code. Here, the unwinder executes in the host, so this type
// helps to avoid dereferencing the host memory.
type ptr uint64

// vmem is the minimum interface required for virtual memory accesses in this
// package. Is is used to read guest memory and rebuild the constructs needed
// for symbolization. It manipulates ptr to avoid confusion between host and
// guest memory.
//
// The functions that operate on it assume both the guest and host are 64bits
// little-endian machines. That way they can cast bytes to specific types
// without having the deserialize them and perform the endianess or size
// conversion.
//
// uintptr/unsafe.Pointer are used to manipulate memory seen by the host, and
// ptr is used to represent memory inside the guest.
type vmem interface {
	Read(address, size uint32) ([]byte, bool)
}

// deref the bytes at address p in virtual memory, casting them back as T. It is
// not recursive: if T is a struct and contains pointers or slices, deref does
// not bring their contents from memory. Pointers can be deref'd themselves, and
// derefSlice can help to bring the contents of slices to the host memory.
func deref[T any](r vmem, p ptr) T {
	var t T
	s := uint32(unsafe.Sizeof(t))
	b, ok := r.Read(uint32(p), s)
	if !ok {
		panic(fmt.Errorf("invalid virtual memory read at %#x size %d", p, s))
	}
	return *(*T)(unsafe.Pointer((unsafe.SliceData(b))))
}

// derefGoSlice takes a slice whose data pointer targets the guest memory, and
// returns a copy the slice's contents in host memory. It is not recursive. Cap
// is set to Len, no matter its initial value.
func derefGoSlice[T any](r vmem, s []T) []T {
	count := len(s)
	sh := (*reflect.SliceHeader)(unsafe.Pointer(&s))
	dp := ptr(sh.Data)
	res := make([]T, count)
	for i := 0; i < count; i++ {
		res[i] = derefArrayIndex[T](r, dp, int32(i))
	}
	return res
}

// Reads the i-th element of an array that starts at address p.
func derefArrayIndex[T any](r vmem, p ptr, i int32) T {
	var t T
	s := ptr(unsafe.Sizeof(t))
	return deref[T](r, p+ptr(i)*s)
}
