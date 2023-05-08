package wzprof

import (
	"testing"
	"time"
)

func BenchmarkMemoryProfiler(b *testing.B) {
	benchmarkFunctionListener(b, NewMemoryProfiler(time.Now))
}
