package main

//go:noinline
func myalloc1(x int) byte {
	s := make([]byte, x)
	return s[len(s)-1] | (byte(x) & 0b11)
}

var global int

//go:noinline
func myalloc2() byte {
	s := make([]byte, global)
	return s[len(s)-1] | 0b100
}

func intermediate(x int) byte {
	return myalloc1(x)
}

func main() {
	// first call to alloc
	a := myalloc1(41)
	// second call to alloc, through inlined function
	b := intermediate(50)
	// third call, not inlined
	global = 100
	c := myalloc2()

	println(a + b + c)
}
