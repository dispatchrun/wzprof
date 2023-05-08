package wzprof

import (
	"testing"
)

func BenchmarkCPUProfiler(b *testing.B) {
	p := NewCPUProfiler()
	p.StartProfile()
	benchmarkFunctionListener(b, p)
}
