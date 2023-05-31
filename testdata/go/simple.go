package main

import "fmt"
import "runtime"

//go:noinline
func thealloc() []byte {
	pcs := make([]uintptr, 100)
	n := runtime.Callers(0, pcs)
	pcs = pcs[:n]

	for _, pc := range pcs {
		fmt.Println("-", pc)
	}

	return make([]byte, 11111)
}

//go:noinline
func myfunc() int {
	x := thealloc()
	for i := range x {
		x[i] = byte(i)
	}
	total := 0
	for _, i := range x {
		total += int(i)
	}
	return total
}

func main() {
	fmt.Println("this is my program")

	x := myfunc()

	// x := myOtherFunc(10000)

	fmt.Println("final length:", x)
}
