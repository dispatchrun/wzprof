package main

//go:noinline
func myalloc1(x int) byte {
	s := make([]byte, x)
	return s[len(s)-1]
}

//go:noinline
func myalloc2() byte {
	s := make([]byte, 100)
	return s[len(s)-1]
}

func intermediate(x int) byte {
	return myalloc1(x)
}

func main() {
	// first call to alloc
	a := myalloc1(42)
	// second call to alloc, through inlined function
	b := intermediate(50)
	// third call, not inlined
	c := myalloc2()

	println(a + b + c)
}
