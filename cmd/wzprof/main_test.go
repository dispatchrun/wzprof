package main

import (
	"context"
	"os"
	"testing"

	"github.com/google/pprof/profile"
)

func TestDataSimple(t *testing.T) {
	profilePath := t.TempDir() + "/profile.pb.gz"
	prog := program{
		WasmPath:  "../../internal/testdata/simple.wasm",
		File:      profilePath,
		Sampling:  1.0,
		Profilers: "cpu,mem",
	}
	err := prog.Execute(context.Background())
	if err != nil {
		t.Fatal("unexpected error:", err)
	}

	f, err := os.Open(profilePath)
	if err != nil {
		t.Fatalf("can't open profile file %s: %s", profilePath, err)
	}
	defer f.Close()
	p, err := profile.Parse(f)
	if err != nil {
		t.Fatalf("error parsing profile: %s", err)
	}

	err = p.CheckValid()
	if err != nil {
		t.Errorf("invalid profile: %s", err)
	}

	expectedTypes := []string{"cpu_samples", "alloc_space"}

	if len(p.SampleType) != len(expectedTypes) {
		t.Errorf("expected %d sample types; got %d", len(expectedTypes), len(p.SampleType))
	}

	for i, e := range expectedTypes {
		if p.SampleType[i].Type != e {
			t.Fatalf("expected sample type %d to be %s; was %s", i, e, p.SampleType[0].Type)
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

	expectedSamples := []sample{
		{
			[]int64{1, 10},
			[]frame{
				{".malloc", 0, false},
				{".func1", 6, false},
				{".main", 34, false},
				{".__main_void", 0, false},
				{"._start", 0, false},
			},
		},
		{
			[]int64{1, 20},
			[]frame{
				{".malloc", 0, false},
				{".func21", 12, false},
				{".func2", 18, false},
				{".main", 35, false},
				{".__main_void", 0, false},
				{"._start", 0, false},
			},
		},
		{
			[]int64{1, 30},
			[]frame{
				{".malloc", 0, false},
				// TODO: inlining does not seem to work with this example
				// {".func31", 23, true},
				{".func3", 23, false},
				{".main", 36, false},
				{".__main_void", 0, false},
				{"._start", 0, false},
			},
		},
	}

	// TODO: pre-process samples to assess faster.
expected:
	for esi, expected := range expectedSamples {
	sample:
		for si, actual := range p.Sample {
			stack := expected.stack
			for _, loc := range actual.Location {
				for _, line := range loc.Line {
					if len(stack) == 0 {
						continue sample
					}
					if line.Function.Name == stack[0].name && line.Line == stack[0].line {
						stack = stack[1:]
					} else {
						continue sample
					}
				}
			}
			if len(stack) == 0 {
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
