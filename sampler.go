package wzprof

import (
	"context"
	"math"

	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

// SampledFunctionListenerFactory returns a function listener factory which
// creates listeners where calls to their Before/After methods is sampled
// at the given sample rate.
//
// Giving a zero or negative sampling rate disables the function listeners
// entirely.
//
// Giving a sampling rate of one or more disables sampling, function listeners
// are invoked for all function calls.
func SampledFunctionListenerFactory(sampleRate float64, factory experimental.FunctionListenerFactory) experimental.FunctionListenerFactory {
	if sampleRate <= 0 {
		return emptyFunctionListenerFactory{}
	}
	if sampleRate >= 1 {
		return factory
	}
	sampler := new(sampler)
	sampler.cycle = uint64(math.Ceil(1 / sampleRate))
	sampler.count = sampler.cycle
	return experimental.FunctionListenerFactoryFunc(func(def api.FunctionDefinition) experimental.FunctionListener {
		lstn := factory.NewListener(def)
		if lstn == nil {
			return nil
		}
		return &sampledFunctionListener{
			sampler: sampler,
			lstn:    lstn,
		}
	})
}

type emptyFunctionListenerFactory struct{}

func (emptyFunctionListenerFactory) NewListener(api.FunctionDefinition) experimental.FunctionListener {
	return nil
}

type sampler struct {
	count uint64
	cycle uint64
	stack bitstack
}

type sampledFunctionListener struct {
	*sampler
	lstn experimental.FunctionListener
}

func (s *sampledFunctionListener) Before(ctx context.Context, mod api.Module, def api.FunctionDefinition, params []uint64, stack experimental.StackIterator) context.Context {
	bit := uint(0)

	if s.count--; s.count == 0 {
		s.count = s.cycle
		bit = 1
		ctx = s.lstn.Before(ctx, mod, def, params, stack)
	}

	s.stack.push(bit)
	return ctx
}

func (s *sampledFunctionListener) After(ctx context.Context, mod api.Module, def api.FunctionDefinition, err error, results []uint64) {
	if s.stack.pop() != 0 {
		s.lstn.After(ctx, mod, def, err, results)
	}
}

type bitstack struct {
	bits []uint64
	size uint
}

func (s *bitstack) push(bit uint) {
	index := s.size / 64
	shift := s.size % 64

	if index >= uint(len(s.bits)) {
		bits := make([]uint64, index+1)
		copy(bits, s.bits)
		s.bits = bits
	}

	s.bits[index] &= ^(uint64(1) << shift)
	s.bits[index] |= uint64(bit&1) << shift
	s.size++
}

func (s *bitstack) pop() uint {
	s.size--
	index := s.size / 64
	shift := s.size % 64
	return uint(s.bits[index]>>shift) & 1
}
