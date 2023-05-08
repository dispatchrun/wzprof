package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/pprof/profile"
)

// This test file performs end-to-end validation of the profiler on actual wasm
// binary files. They are located in testdata. Tests are very sensitive to the
// content of those files. If you rebuild them, you will likely need to rebuild
// the expected samples below. Use the printSamples() function to help you with
// that.

func TestDataCSimple(t *testing.T) {
	testMemoryProfiler(t, "../../testdata/c/simple.wasm", []sample{
		{
			[]int64{10, 1},
			[]frame{
				{"malloc", 0, false},
				{"func1", 6, false},
				{"main", 34, false},
				{"__main_void", 0, false},
				{"_start", 0, false},
			},
		},
		{
			[]int64{20, 1},
			[]frame{
				{"malloc", 0, false},
				{"func21", 12, false},
				{"func2", 18, false},
				{"main", 35, false},
				{"__main_void", 0, false},
				{"_start", 0, false},
			},
		},
		{
			[]int64{30, 1},
			[]frame{
				{"malloc", 0, false},
				{"func31", 29, true},
				{"func3", 23, false},
				{"main", 36, false},
				{"__main_void", 0, false},
				{"_start", 0, false},
			},
		},
	})
}

func TestDataRustSimple(t *testing.T) {
	testMemoryProfiler(t, "../../testdata/rust/simple/target/wasm32-wasi/debug/simple.wasm", []sample{
		{
			[]int64{120, 1},
			[]frame{
				{"malloc", 0, false},                                                                        // malloc
				{"std:sys:wasi:alloc:{impl#0}:alloc", 381, true},                                            // _ZN3std3sys4wasi5alloc81_$LT$impl$u20$core..alloc..global..GlobalAlloc$u20$for$u20$std..alloc..System$GT$5alloc17hf06d843ee28c936eE
				{"std:alloc:__default_lib_allocator:__rdl_alloc", 14, false},                                // std:alloc:__default_lib_allocator:__rdl_alloc
				{"__rust_alloc", 0, false},                                                                  // __rust_alloc
				{"core:alloc:layout:size", 173, true},                                                       // _ZN4core5alloc6layout6Layout4size17h4a3a848dac2e5d6cE
				{"core:alloc:layout:dangling", 174, true},                                                   // _ZN4core5alloc6layout6Layout8dangling17h205051d0cdadc81fE
				{"alloc:alloc:alloc_impl", 95, false},                                                       // _ZN5alloc5alloc6Global10alloc_impl17h579ac88351552cb7E
				{"alloc:alloc:{impl#1}:allocate", 237, false},                                               // _ZN63_$LT$alloc..alloc..Global$u20$as$u20$core..alloc..Allocator$GT$8allocate17hcb9ff3e2ca003c84E
				{"core:alloc:layout:array<i32>", 176, true},                                                 // _ZN4core5alloc6layout6Layout5array17hb8c955ded92025c1E
				{"alloc:raw_vec:allocate_in<i32, alloc::alloc::Global>", 185, false},                        // _ZN5alloc7raw_vec19RawVec$LT$T$C$A$GT$11allocate_in17hec12f02c19409feeE
				{"alloc:vec:with_capacity_in<i32, alloc::alloc::Global>", 483, true},                        // _ZN5alloc3vec16Vec$LT$T$C$A$GT$16with_capacity_in17h574658446754b2caE
				{"alloc:vec:with_capacity<i32>", 131, false},                                                // _ZN5alloc3vec12Vec$LT$T$GT$13with_capacity17hea1d94514f4fb20fE
				{"simple:allocate_even_more_memory", 29, false},                                             // _ZN6simple25allocate_even_more_memory17h94cc86da44bad945E
				{"simple:allocate_more_memory", 23, false},                                                  // _ZN6simple20allocate_more_memory17h4594ee16911b70d7E
				{"simple:allocate_memory", 13, false},                                                       // _ZN6simple15allocate_memory17hb0084bacecc50a31E
				{"simple:main", 4, false},                                                                   // _ZN6simple4main17h7c6bec49f74488e8E
				{"core:ops:function:FnOnce:call_once<fn(), ()>", 250, false},                                // _ZN4core3ops8function6FnOnce9call_once17h65afd749b06e87d3E
				{"std:sys_common:backtrace:__rust_begin_short_backtrace<fn(), ()>", 121, false},             // _ZN3std10sys_common9backtrace28__rust_begin_short_backtrace17h46f307b03ffe9605E
				{"std:process:to_i32", 166, true},                                                           // _ZN3std7process8ExitCode6to_i3217h04fa3a639ce3318dE
				{"std:rt:lang_start:{closure#0}<()>", 166, false},                                           // _ZN3std2rt10lang_start28_$u7b$$u7b$closure$u7d$$u7d$17h820e14cd6a99f492E
				{"std:panic:catch_unwind<std::rt::lang_start_internal::{closure_env#1}, ()>", 147, true},    // _ZN3std5panic12catch_unwind17h9087a606b40b7d51E
				{"std:panic:catch_unwind<std::rt::lang_start_internal::{closure_env#2}, isize>", 148, true}, // _ZN3std5panic12catch_unwind17h09dbc99d0be4be1fE
				{"std:panic:catch_unwind<fn(), ()>", 153, true},                                             // _ZN3std5panic12catch_unwind17he6fc2a53d5cadc61E
				{"std:rt:lang_start_internal", 287, false},                                                  // _ZN3std2rt19lang_start_internal17h38aaea5d7881ae71E
				{"std:rt:lang_start<()>", 165, false},                                                       // _ZN3std2rt10lang_start17hb2321e0751704c7cE
				{"__main_void", 0, false},                                                                   // __main_void
				{"_start", 0, false},                                                                        // _start
			},
		},
	})
}

func testMemoryProfiler(t *testing.T, path string, expectedSamples []sample) {
	prog := &program{
		filePath:   path,
		sampleRate: 1,
		memProfile: filepath.Join(t.TempDir(), "mem.pprof"),
	}

	err := prog.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(prog.memProfile)
	if err != nil {
		t.Fatalf("can't open profile file %s: %s", prog.memProfile, err)
	}
	defer f.Close()
	p, err := profile.Parse(f)
	if err != nil {
		t.Fatalf("error parsing profile: %s", err)
	}
	if err := p.CheckValid(); err != nil {
		t.Fatalf("invalid profile: %s", err)
	}

	expectedTypes := []string{
		"alloc_space",
		"alloc_objects",
		"inuse_space",
		"inuse_objects",
	}

	if len(p.SampleType) != len(expectedTypes) {
		t.Errorf("expected %d sample types; got %d", len(expectedTypes), len(p.SampleType))
	}

	for i, e := range expectedTypes {
		if p.SampleType[i].Type != e {
			t.Fatalf("expected sample type %d to be %s; was %s", i, e, p.SampleType[i].Type)
		}
	}

	// printSamples(p.Sample)

	// TODO: pre-process samples to assess faster.
expected:
	for esi, expected := range expectedSamples {
	sample:
		for si, actual := range p.Sample {
			stack := expected.stack
			for _, loc := range actual.Location {
				for i, line := range loc.Line {
					if len(stack) == 0 {
						continue sample
					}
					if line.Function.Name == stack[0].name && line.Line == stack[0].line {
						inline := i < len(loc.Line)-1
						if inline != stack[0].inlined {
							t.Errorf("stack frame was supposed to be inlined %t; was %t", stack[0].inlined, inline)
						}
						stack = stack[1:]
					} else {
						continue sample
					}
				}
			}
			if len(stack) == 0 {
				// TODO: test "inuse_*" samples
				for i, e := range expected.values {
					if e != actual.Value[i] {
						t.Errorf("expected sample matched %d, but value %d was %d; expected %d", si, i, actual.Value[i], e)
					}
				}
				continue expected
			}
		}
		t.Errorf("expected sample %d not found in profile", esi)
	}
}

type frame struct {
	name    string
	line    int64
	inlined bool
}

type sample struct {
	values []int64
	stack  []frame
}

// printSamples outputs the samples list in a way that can be copy-pasted in the
// tests above.
//
// Pairs well with grep. Example to find the stack trace of allocate_even that
// ends up allocating 120 bytes:
//
//	go test ./...|grep -C 15 allocate_even|grep -A 30 120
func printSamples(samples []*profile.Sample) {
	for i, s := range samples {
		fmt.Println("Sample", i, "-------------", s.Value)
		for _, loc := range s.Location {
			for li, line := range loc.Line {
				inline := li < len(loc.Line)-1
				fmt.Printf("\t{\"%s\", %d, %t}, // %s\n", line.Function.Name, line.Line, inline, line.Function.SystemName)
			}
		}
	}
}
