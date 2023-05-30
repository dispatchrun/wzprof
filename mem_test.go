package wzprof

import (
	"testing"
)

func BenchmarkMemoryProfiler(b *testing.B) {
	s := NewSupport(nil)
	benchmarkFunctionListener(b, NewMemoryProfiler(s))
}
