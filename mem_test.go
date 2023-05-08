package wzprof

import (
	"testing"
)

func BenchmarkMemoryProfiler(b *testing.B) {
	benchmarkFunctionListener(b, NewMemoryProfiler())
}
