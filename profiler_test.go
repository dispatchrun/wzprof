package wzprof_test

import (
	"testing"

	"github.com/stealthrocket/wzprof"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental/wazerotest"
)

func BenchmarkProfileListener(b *testing.B) {
	profiler := wzprof.NewProfileListener(&wzprof.ProfilerCPUTime{
		Sampling: 1,
	})

	module := &wazerotest.Module{
		ModuleName: "benchmark",
		Functions: []*wazerotest.Function{
			&wazerotest.Function{
				FunctionName: "F0",
				ParamTypes:   []api.ValueType{},
				ResultTypes:  []api.ValueType{},
			},
			&wazerotest.Function{
				FunctionName: "F1",
				ParamTypes:   []api.ValueType{},
				ResultTypes:  []api.ValueType{},
			},
			&wazerotest.Function{
				FunctionName: "F2",
				ParamTypes:   []api.ValueType{},
				ResultTypes:  []api.ValueType{},
			},
		},
	}

	stack := []wazerotest.StackFrame{
		{Function: module.Function(0)},
		{Function: module.Function(1)},
		{Function: module.Function(2)},
	}

	wazerotest.BenchmarkFunctionListener(b, module, stack, profiler.NewListener(stack[0].Function.Definition()))
}
