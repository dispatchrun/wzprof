package wzprof

import (
	"testing"
)

func BenchmarkMemoryProfiler(b *testing.B) {
	rt := NewRuntime(nil)
	benchmarkFunctionListener(b, NewMemoryProfiler(rt))
}
