package wzprof

import (
	"testing"
)

func BenchmarkMemoryProfiler(b *testing.B) {
	p, err := ProfilingFor(nil).MemoryProfiler()
	if err != nil {
		b.Fatal(err)
	}
	benchmarkFunctionListener(b, p)
}
