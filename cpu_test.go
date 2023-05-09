package wzprof

import (
	"testing"
)

func BenchmarkCPUProfilerOn(b *testing.B) {
	p := NewCPUProfiler()
	p.StartProfile()
	benchmarkFunctionListener(b, p)
}

func BenchmarkCPUProfilerOff(b *testing.B) {
	p := NewCPUProfiler()
	benchmarkFunctionListener(b, p)
}
