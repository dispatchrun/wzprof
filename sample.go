package wzprof

import (
	"context"
	"math"

	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

// Sample returns a function listener factory which creates listeners where
// calls to their Before/After methods is sampled at the given sample rate.
//
// Giving a zero or negative sampling rate disables the function listeners
// entirely.
//
// Giving a sampling rate of one or more disables sampling, function listeners
// are invoked for all function calls.
func Sample(sampleRate float64, factory experimental.FunctionListenerFactory) experimental.FunctionListenerFactory {
	if sampleRate <= 0 {
		return emptyFunctionListenerFactory{}
	}
	if sampleRate >= 1 {
		return factory
	}
	cycle := uint32(math.Ceil(1 / sampleRate))
	return experimental.FunctionListenerFactoryFunc(func(def api.FunctionDefinition) experimental.FunctionListener {
		lstn := factory.NewFunctionListener(def)
		if lstn == nil {
			return nil
		}
		sampled := &sampledFunctionListener{
			cycle: cycle,
			count: cycle,
			lstn:  lstn,
		}
		sampled.stack.bits = sampled.bits[:]
		return sampled
	})
}

type emptyFunctionListenerFactory struct{}

func (emptyFunctionListenerFactory) NewFunctionListener(api.FunctionDefinition) experimental.FunctionListener {
	return nil
}

type sampledFunctionListener struct {
	count uint32
	cycle uint32
	bits  [1]uint64
	stack bitstack
	lstn  experimental.FunctionListener
}

func (s *sampledFunctionListener) Before(ctx context.Context, mod api.Module, def api.FunctionDefinition, params []uint64, stack experimental.StackIterator) {
	bit := uint(0)

	if s.count--; s.count == 0 {
		s.count = s.cycle
		s.lstn.Before(ctx, mod, def, params, stack)
		bit = 1
	}

	s.stack.push(bit)
}

func (s *sampledFunctionListener) After(ctx context.Context, mod api.Module, def api.FunctionDefinition, results []uint64) {
	if s.stack.pop() != 0 {
		s.lstn.After(ctx, mod, def, results)
	}
}

func (s *sampledFunctionListener) Abort(ctx context.Context, mod api.Module, def api.FunctionDefinition, err error) {
	if s.stack.pop() != 0 {
		s.lstn.Abort(ctx, mod, def, err)
	}
}

type bitstack struct {
	size uint
	bits []uint64
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
