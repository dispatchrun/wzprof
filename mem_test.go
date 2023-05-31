package wzprof

import (
	"testing"
)

func BenchmarkMemoryProfiler(b *testing.B) {
	p := ProfilingFor(nil).MemoryProfiler()
	benchmarkFunctionListener(b, p)
}
