package wzprof

import (
	"testing"
)

func BenchmarkMemoryProfiler(b *testing.B) {
	rt := NewRuntime()
	benchmarkFunctionListener(b, NewMemoryProfiler(rt))
}
