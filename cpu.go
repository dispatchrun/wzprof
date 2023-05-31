package wzprof

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/pprof/profile"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

// CPUProfiler is the implementation of a performance profiler recording
// samples of CPU time spent in functions of a WebAssembly module.
//
// The profiler generates samples of two types:
// - "sample" counts the number of function calls.
// - "cpu" records the time spent in function calls (in nanoseconds).
type CPUProfiler struct {
	p      *Profiling
	mutex  sync.Mutex
	counts stackCounterMap
	frames []cpuTimeFrame
	traces []stackTrace
	time   func() int64
	start  time.Time
	host   bool
}

// CPUProfilerOption is a type used to represent configuration options for
// CPUProfiler instances created by NewCPUProfiler.
type CPUProfilerOption func(*CPUProfiler)

// HostTime confiures a CPU time profiler to account for time spent in calls
// to host functions.
//
// Default to false.
func HostTime(enable bool) CPUProfilerOption {
	return func(p *CPUProfiler) { p.host = enable }
}

// TimeFunc configures the time function used by the CPU profiler to collect
// monotonic timestamps.
//
// By default, the system's monotonic time is used.
func TimeFunc(time func() int64) CPUProfilerOption {
	return func(p *CPUProfiler) { p.time = time }
}

type cpuTimeFrame struct {
	start int64
	sub   int64
	trace stackTrace
}

func newCPUProfiler(p *Profiling, options ...CPUProfilerOption) *CPUProfiler {
	c := &CPUProfiler{
		p:    p,
		time: nanotime,
	}
	for _, opt := range options {
		opt(c)
	}
	return c
}

// StartProfile begins recording the CPU profile. The method returns a boolean
// to indicate whether starting the profile succeeded (e.g. false is returned if
// it was already started).
func (p *CPUProfiler) StartProfile() bool {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	if p.counts != nil {
		return false // already started
	}

	p.counts = make(stackCounterMap)
	p.start = time.Now()
	return true
}

// StopProfile stops recording and returns the CPU profile. The method returns
// nil if recording of the CPU profile wasn't started.
func (p *CPUProfiler) StopProfile(sampleRate float64) *profile.Profile {
	p.mutex.Lock()
	samples, start := p.counts, p.start
	p.counts = nil
	p.mutex.Unlock()

	if samples == nil {
		return nil
	}

	duration := time.Since(start)

	if !p.host {
		for k, sample := range samples {
			if sample.stack.host() {
				delete(samples, k)
			}
		}
	}

	ratios := []float64{
		1 / sampleRate,
		// Time values are not influenced by the sampling rate so we don't have
		// to scale them out.
		1,
	}

	return buildProfile(p.p, samples, start, duration, p.SampleType(), ratios)
}

// Name returns "profile" to match the name of the CPU profiler in pprof.
func (p *CPUProfiler) Name() string {
	return "profile"
}

// Desc returns a description copied from net/http/pprof.
func (p *CPUProfiler) Desc() string {
	return profileDescriptions[p.Name()]
}

// Count returns the number of execution stacks currently recorded in p.
func (p *CPUProfiler) Count() int {
	p.mutex.Lock()
	n := len(p.counts)
	p.mutex.Unlock()
	return n
}

// SampleType returns the set of value types present in samples recorded by the
// CPU profiler.
func (p *CPUProfiler) SampleType() []*profile.ValueType {
	return []*profile.ValueType{
		{Type: "samples", Unit: "count"},
		{Type: "cpu", Unit: "nanoseconds"},
	}
}

// NewHandler returns a http handler allowing the profiler to be exposed on a
// pprof-compatible http endpoint.
//
// The sample rate is a value between 0 and 1 used to scale the profile results
// based on the sampling rate applied to the profiler so the resulting values
// remain representative.
//
// The symbolizer passed as argument is used to resolve names of program
// locations recorded in the profile.
func (p *CPUProfiler) NewHandler(sampleRate float64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		duration := 30 * time.Second

		if seconds := r.FormValue("seconds"); seconds != "" {
			n, err := strconv.ParseInt(seconds, 10, 64)
			if err == nil && n > 0 {
				duration = time.Duration(n) * time.Second
			}
		}

		ctx := r.Context()
		deadline, ok := ctx.Deadline()
		if ok {
			if timeout := time.Until(deadline); duration > timeout {
				serveError(w, http.StatusBadRequest, "profile duration exceeds server's WriteTimeout")
				return
			}
		}

		if !p.StartProfile() {
			serveError(w, http.StatusInternalServerError, "Could not enable CPU profiling: profiler already running")
			return
		}

		timer := time.NewTimer(duration)
		select {
		case <-timer.C:
		case <-ctx.Done():
		}
		timer.Stop()
		serveProfile(w, p.StopProfile(sampleRate))
	})
}

// NewFunctionListener returns a function listener suited to record CPU timings
// of calls to the function passed as argument.
func (p *CPUProfiler) NewFunctionListener(def api.FunctionDefinition) experimental.FunctionListener {
	_, skip := p.p.filteredFunctions[def.Name()]
	if skip {
		return nil
	}
	return profilingListener{p.p, cpuProfiler{p}}
}

type cpuProfiler struct{ *CPUProfiler }

func (p cpuProfiler) Before(ctx context.Context, mod api.Module, def api.FunctionDefinition, _ []uint64, si experimental.StackIterator) {
	var frame cpuTimeFrame
	p.mutex.Lock()

	if p.counts != nil {
		start := p.time()
		trace := stackTrace{}

		if i := len(p.traces); i > 0 {
			i--
			trace = p.traces[i]
			p.traces = p.traces[:i]
		}

		frame = cpuTimeFrame{
			start: start,
			trace: makeStackTrace(trace, si),
		}
	}

	p.mutex.Unlock()
	p.frames = append(p.frames, frame)
}

func (p cpuProfiler) After(ctx context.Context, mod api.Module, def api.FunctionDefinition, _ []uint64) {
	i := len(p.frames) - 1
	f := p.frames[i]
	p.frames = p.frames[:i]

	if f.start != 0 {
		duration := p.time() - f.start
		if i := len(p.frames); i > 0 {
			p.frames[i-1].sub += duration
		}
		duration -= f.sub
		p.mutex.Lock()
		if p.counts != nil {
			p.counts.observe(f.trace, duration)
		}
		p.mutex.Unlock()
		p.traces = append(p.traces, f.trace)
	}
}

func (p cpuProfiler) Abort(ctx context.Context, mod api.Module, def api.FunctionDefinition, _ error) {
	p.After(ctx, mod, def, nil)
}
