package wzprof

import "testing"

func TestProfilerMemory_calloc(t *testing.T) {
	p := ProfilerMemory{}
	fns := p.Register()
	name := p.Listen("calloc")
	pf := fns[name].Before
	x := pf(nil, []uint64{42, 11})
	b := int64(42 * 11)
	if x != b {
		t.Errorf("calloc reported %d; want %d", x, b)
	}
}
