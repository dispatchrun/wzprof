package wzprof

import "testing"

func TestProfilerMemory_calloc(t *testing.T) {
	p := ProfilerMemory{}
	fns := p.Register()
	name := p.Listen("calloc")
	pf := fns[name]
	x := pf([]uint64{42, 11}, nil, nil)
	b := int64(42 * 11)
	if x != b {
		t.Errorf("calloc reported %d; want %d", x, b)
	}
}
