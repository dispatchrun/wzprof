package main

import "fmt"

// //go:noinline
// func myfunc() {
// 	things := make([]byte, 0, 42)

// 	for i := 0; i < 100; i++ {
// 		things = append(things, byte(i))
// 	}

// 	fmt.Println("final length:", len(things))
// }

// //go:noinline
// func myOtherFunc(s int) int {
// 	m := make(map[int]int, 0)
// 	for i := 0; i < s; i++ {
// 		m[i] = i
// 	}
// 	return len(m)
// }

//go:noinline
func thealloc() []byte {
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
