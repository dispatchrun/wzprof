package wzprof

import "math/rand"

type Sampler interface {
	Do() bool
}

type randomSampler struct {
	rand   *rand.Rand
	chance float32
}

func newRandomSampler(seed int64, chance float32) *randomSampler {
	return &randomSampler{
		rand:   rand.New(rand.NewSource(seed)),
		chance: chance,
	}
}

func (s *randomSampler) Do() bool {
	return s.rand.Float32() < s.chance
}

type alwaysSampler struct{}

func (s *alwaysSampler) Do() bool {
	return true
}

func newAlwaysSampler() *alwaysSampler {
	return &alwaysSampler{}
}
