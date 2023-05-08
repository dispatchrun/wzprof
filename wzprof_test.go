package wzprof

import (
	"context"
	"testing"

	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/experimental/wazerotest"
)

func benchmarkFunctionListener(b *testing.B, factory experimental.FunctionListenerFactory) {
	malloc := wazerotest.NewFunction(func(ctx context.Context, mod api.Module, size uint32) uint32 {
		return 0
	})

	malloc.FunctionName = "malloc"
	malloc.ExportNames = []string{"malloc"}

	module := wazerotest.NewModule(nil,
		malloc,
	)

	stack := []wazerotest.StackFrame{
		{Function: malloc, Params: []uint64{42}, Results: []uint64{0}},
	}

	wazerotest.BenchmarkFunctionListener(b, module, stack,
		factory.NewListener(malloc.Definition()),
	)
}
