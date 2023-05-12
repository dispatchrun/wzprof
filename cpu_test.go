package wzprof

import (
	"context"
	"testing"

	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/experimental/wazerotest"
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

func TestCPUProfilerTime(t *testing.T) {
	currentTime := int64(0)

	p := NewCPUProfiler(
		TimeFunc(func() int64 { return currentTime }),
	)

	module := wazerotest.NewModule(nil,
		wazerotest.NewFunction(func(context.Context, api.Module) {}),
		wazerotest.NewFunction(func(context.Context, api.Module) {}),
		wazerotest.NewFunction(func(context.Context, api.Module) {}),
	)

	f0 := p.NewFunctionListener(module.Function(0).Definition())
	f1 := p.NewFunctionListener(module.Function(1).Definition())
	f2 := p.NewFunctionListener(module.Function(2).Definition())

	stack0 := []experimental.StackFrame{
		{Function: module.Function(0)},
	}

	stack1 := []experimental.StackFrame{
		{Function: module.Function(0)},
		{Function: module.Function(1)},
	}

	stack2 := []experimental.StackFrame{
		{Function: module.Function(0)},
		{Function: module.Function(1)},
		{Function: module.Function(2)},
	}

	def0 := stack0[0].Function.Definition()
	def1 := stack1[1].Function.Definition()
	def2 := stack2[2].Function.Definition()

	ctx := context.Background()

	const (
		t0 int64 = 1
		t1 int64 = 10
		t2 int64 = 42
		t3 int64 = 100
		t4 int64 = 101
		t5 int64 = 102
	)

	p.StartProfile()

	currentTime = t0
	f0.Before(ctx, module, def0, nil, experimental.NewStackIterator(stack0...))

	currentTime = t1
	f1.Before(ctx, module, def1, nil, experimental.NewStackIterator(stack1...))

	currentTime = t2
	f2.Before(ctx, module, def2, nil, experimental.NewStackIterator(stack2...))

	currentTime = t3
	f2.After(ctx, module, def2, nil)

	currentTime = t4
	f1.After(ctx, module, def1, nil)

	currentTime = t5
	f0.After(ctx, module, def0, nil)

	trace0 := makeStackTraceFromFrames(stack0)
	trace1 := makeStackTraceFromFrames(stack1)
	trace2 := makeStackTraceFromFrames(stack2)

	d2 := t3 - t2
	d1 := t4 - (t1 + d2)
	d0 := t5 - (t0 + d1 + d2)

	assertStackCount(t, p.counts, trace0, 1, d0)
	assertStackCount(t, p.counts, trace1, 1, d1)
	assertStackCount(t, p.counts, trace2, 1, d2)
}

func assertStackCount(t *testing.T, counts stackCounterMap, trace stackTrace, count, total int64) {
	t.Helper()
	c := counts.lookup(trace)

	if c.count() != count {
		t.Errorf("%sstack count mismatch: want=%d got=%d", trace, count, c.count())
	}

	if c.total() != total {
		t.Errorf("%sstack total mismatch: want=%d got=%d", trace, total, c.total())
	}
}

func makeStackTraceFromFrames(stackFrames []experimental.StackFrame) stackTrace {
	return makeStackTrace(stackTrace{}, experimental.NewStackIterator(stackFrames...))
}
