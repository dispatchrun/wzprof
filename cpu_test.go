package wzprof

import (
	"testing"
	"time"
)

func BenchmarkCPUProfiler(b *testing.B) {
	p := NewCPUProfiler(time.Now)
	p.StartProfile()
	benchmarkFunctionListener(b, p)
}
