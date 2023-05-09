package wzprof

import (
	"context"
	"testing"

	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/experimental/wazerotest"
)

func TestSampledFunctionListener(t *testing.T) {
	module := wazerotest.NewModule(nil,
		wazerotest.NewFunction(func(ctx context.Context, mod api.Module) {}),
	)

	n := 0
	f := func(context.Context, api.Module, api.FunctionDefinition, []uint64, experimental.StackIterator) { n++ }

	factory := Sample(0.1, experimental.FunctionListenerFactoryFunc(
		func(def api.FunctionDefinition) experimental.FunctionListener {
			return experimental.FunctionListenerFunc(f)
		},
	))

	function := module.Function(0).Definition()
	listener := factory.NewListener(function)

	for i := 0; i < 20; i++ {
		ctx := listener.Before(context.Background(), module, function, nil, nil)
		listener.After(ctx, module, function, nil, nil)
	}

	if n != 2 {
		t.Errorf("wrong number of called to sampled listener: want=2 got=%d", n)
	}
}

func BenchmarkSampledFunctionListener(b *testing.B) {
	benchmarkFunctionListener(b,
		Sample(0.1, experimental.FunctionListenerFactoryFunc(
			func(def api.FunctionDefinition) experimental.FunctionListener {
				return experimental.FunctionListenerFunc(
					func(ctx context.Context, mod api.Module, def api.FunctionDefinition, paramValues []uint64, stackIterator experimental.StackIterator) {
					},
				)
			},
		)),
	)
}
