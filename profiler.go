//  Copyright 2023 Stealth Rocket, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package wzprof

import (
	"container/list"
	"context"
	"encoding/binary"
	"fmt"
	"hash/maphash"
	"io"
	"log"
	"strings"
	"sync"

	"github.com/google/pprof/profile"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

// Profiler is provided to NewProfilerListener and called when appropriate to
// measure samples.
type ProfileProcessor interface {
	// BeforeFunction inspects the state of the wasm module to return a
	// sample value. Implementations of this signature should consider
	// that they are called on the hot path of the profiler, so they
	// should minimize allocations and return as quickly as possible.
	Before(mod api.Module, params []uint64) int64
	// AfterFunction is called right before the hooked function returns.
	// `in` is the value returned by BeforeFunction.
	After(before int64, results []uint64) int64
}

// Profiler is provided to NewProfilerListener and called when
// appropriate to measure samples.
type Profiler interface {
	// SampleType is called once initially to register the pprof type of
	// samples collected by this profiler. Only one type permitted for now.
	SampleType() profile.ValueType

	// Sampler is called once initially to register which sampler to use for
	// this profiler.
	Sampler() Sampler

	// Register is called at the initialization of the module to register
	// possible profile functions that can be returned by Listen. Empty string
	// is not a valid key.
	Register() map[string]ProfileProcessor

	// Listen is called at the initialization of the module for each
	// function 'name' definied in it. Return the name of a function as
	// defined in Register, or empty string to not listen.
	Listen(name string) string

	PostProcess(prof *profile.Profile, idx int, locations []*profile.Location)
}

// ProfilerListener is a FunctionListenerFactory injecting a set of Profilers
// and generates a pprof profile.
type ProfilerListener struct {
	profilers    []Profiler
	processorFns map[string]ProfileProcessor
	samplerFns   []Sampler

	hooks         map[string]*hook
	lastStackSize int
	samples       *list.List

	samplesMu sync.RWMutex

	mapper   mapper
	locCache map[pprofLocationKey]*profile.Location
}

type pprofLocationKey struct {
	Module string
	Index  uint32
	Name   string
	PC     uint64
}

type location struct {
	File    string
	Line    int64
	Column  int64
	Inlined bool
	PC      uint64
	// Linkage Name if present, Name otherwise.
	// Only present for inlined functions.
	StableName string
	HumanName  string
}

type mapper interface {
	Lookup(pc uint64) []location
}

// PrepareSymbols gives the opportunity to ProfileListener to prepare a suitable
// symbol mapper for a compiled wasm module. If available, those symbols will be
// used to provide source-level profiling.
func (p *ProfilerListener) PrepareSymbols(m wazero.CompiledModule) {
	var err error
	sc := m.CustomSections()

	p.mapper, err = newDwarfmapper(sc)
	if err != nil {
		log.Printf("profiler: could not load dwarf symbols: %s", err)
	}
}

// DefaultMaxStacksCount is the default maximum number of stacks to keep in.
const DefaultMaxStacksCount = 250000

func keyFor(idx int, name string) string {
	return fmt.Sprintf("%d:%s", idx, name)
}

// NewProfileListener creates a new ProfilerListener with the given profilers.
// There are two built-in profilers:
//
//   - ProfilerCPU collects CPU usage samples based on stack counts
//   - ProfilerMemory collects Memory usage based on well known allocation
//     functions
func NewProfileListener(profilers ...Profiler) *ProfilerListener {
	processorFns := map[string]ProfileProcessor{}
	samplerFns := make([]Sampler, 0, len(profilers))

	for i, p := range profilers {
		fns := p.Register()
		for name, f := range fns {
			processorFns[keyFor(i, name)] = f
		}
		samplerFns = append(samplerFns, p.Sampler())
	}

	return &ProfilerListener{
		processorFns: processorFns,
		samplerFns:   samplerFns,
		hooks:        make(map[string]*hook),
		profilers:    profilers,
		samples:      list.New(),
		samplesMu:    sync.RWMutex{},
	}
}

// Register registers the ProfilerListener to the Wazero context.
// See: https://github.com/tetratelabs/wazero/blob/c82ad896f6019708f22ddd7826ff47319b7a1e54/experimental/listener.go#L27-L30
func (p *ProfilerListener) Register(ctx context.Context) context.Context {
	return context.WithValue(ctx, experimental.FunctionListenerFactoryKey{}, p)
}

type stackEntry struct {
	fn api.FunctionDefinition
	pc uint64
}

type sample struct {
	stack  []stackEntry
	values []int64
	isIO   bool
	isSet  bool
}

func (p *ProfilerListener) report(si experimental.StackIterator, values []int64) {
	sample := sample{
		stack:  make([]stackEntry, 0, p.lastStackSize+1),
		values: make([]int64, len(values)),
	}
	copy(sample.values, values)
	for si.Next() {
		fn := si.FunctionDefinition()
		pc := uint64(0) // si.SourceOffset()
		sample.stack = append(sample.stack, stackEntry{fn: fn, pc: pc})
	}
	p.samplesMu.Lock()
	if p.samples == nil {
		p.samples = list.New()
	}
	p.samples.PushBack(sample)
	if p.samples.Len() >= DefaultMaxStacksCount {
		e := p.samples.Front()
		p.samples.Remove(e)
	}
	p.samplesMu.Unlock()
	p.lastStackSize = len(sample.stack)
}

func (p *ProfilerListener) reportSample(s sample) {
	p.samplesMu.Lock()
	if p.samples == nil {
		p.samples = list.New()
	}
	p.samples.PushBack(s)
	if p.samples.Len() >= DefaultMaxStacksCount {
		e := p.samples.Front()
		p.samples.Remove(e)
	}
	p.samplesMu.Unlock()
	p.lastStackSize = len(s.stack)
}

type offCPULocations struct {
	location *profile.Location
	val      int64
}

var hashseed = maphash.MakeSeed()

// BuildProfile builds a pprof Profile from the collected
// samples. After collection all samples are cleared.
func (p *ProfilerListener) BuildProfile() *profile.Profile {
	prof := &profile.Profile{
		Function:   []*profile.Function{},
		SampleType: []*profile.ValueType{},
	}

	for _, p := range p.profilers {
		t := p.SampleType()
		prof.SampleType = append(prof.SampleType, &t)
	}

	type entry struct { // TODO: this is literaly profile.Sample, use that instead
		counts    []int64
		locations []*profile.Location
	}

	counters := make(map[uint64]entry)
	bx := make([]byte, 8)

	var off []*profile.Location

	p.samplesMu.Lock()
	for e := p.samples.Front(); e != nil; e = e.Next() {
		locations := []*profile.Location{}
		h := maphash.Hash{}
		h.SetSeed(hashseed)

		s := e.Value.(sample)
		for _, f := range s.stack {
			// TODO: when known, f.pc may be enough
			// instead of using name.
			binary.LittleEndian.PutUint64(bx, f.pc)
			h.WriteString(f.fn.Name())
			h.Write(bx)
			loc := p.locationForCall(prof, f)
			locations = append(locations, loc)
		}

		if s.isIO {
			off = append(off, locations[0])
		}

		sum64 := h.Sum64()
		e, ok := counters[sum64]
		if !ok {
			e = entry{
				counts:    make([]int64, len(s.values)),
				locations: locations,
			}
			counters[sum64] = e
		}
		for i, c := range s.values {
			e.counts[i] += c
		}
	}

	p.samples = nil
	p.samplesMu.Unlock()

	for _, count := range counters {
		sample := &profile.Sample{
			Value:    count.counts,
			Location: count.locations,
		}
		prof.Sample = append(prof.Sample, sample)

		for i, p := range p.profilers {
			p.PostProcess(prof, i, off)
		}
	}

	return prof
}

// Write writes the collected samples to the given writer.
func (p *ProfilerListener) Write(w io.Writer) error {
	prof := p.BuildProfile()
	return prof.Write(w)
}

func (p *ProfilerListener) locationForCall(prof *profile.Profile, f stackEntry) *profile.Location {
	if p.locCache == nil {
		p.locCache = map[pprofLocationKey]*profile.Location{}
	}

	locKey := pprofLocationKey{
		Module: f.fn.ModuleName(),
		Index:  f.fn.Index(),
		Name:   f.fn.Name(),
		PC:     f.pc,
	}

	if loc, ok := p.locCache[locKey]; ok {
		return loc
	}

	// Cache miss. Get or create function and all the line
	// locations associated with inlining.
	var locations []location
	mapperFound := false
	if p.mapper != nil && f.pc > 0 {
		locations = p.mapper.Lookup(f.pc)
		if len(locations) > 0 {
			mapperFound = true
		}
	}
	if len(locations) == 0 {
		// If we don't have a source location, attach to a
		// generic location whithin the function.
		locations = []location{{}}
	}
	// Provide defaults in case we couldn't resolve DWARF informations for
	// the main function call's PC.
	if locations[0].StableName == "" {
		locations[0].StableName = f.fn.Name()
	}
	if locations[0].HumanName == "" {
		locations[0].HumanName = f.fn.Name()
	}

	lines := make([]profile.Line, len(locations))

	for i, loc := range locations {
		var pprofFn *profile.Function
		for _, f := range prof.Function {
			if f.SystemName == loc.StableName {
				pprofFn = f
				break
			}
		}
		if pprofFn == nil {
			pprofFn = &profile.Function{
				ID:         uint64(len(prof.Function)) + 1, // 0 is reserved by pprof
				Name:       loc.HumanName,
				SystemName: loc.StableName,
				Filename:   loc.File,
			}
			prof.Function = append(prof.Function, pprofFn)
		} else if mapperFound {
			// Sometimes the function had to be created while the PC
			// wasn't found by the symbol mapper. Attempt to correct
			// it if we had a successful match this time.
			pprofFn.Name = locations[i].HumanName
			pprofFn.SystemName = locations[i].StableName
			pprofFn.Filename = locations[i].File

		}
		// Pprof expects lines to start with the root of the inlined
		// calls. DWARF encodes that information the other way around,
		// so we fill lines backwards.
		lines[len(locations)-i-1] = profile.Line{
			Function: pprofFn,
			Line:     loc.Line,
		}
	}

	loc := &profile.Location{
		ID:      uint64(len(prof.Location)) + 1, // 0 reserved by pprof
		Line:    lines,
		Address: f.pc,
	}
	prof.Location = append(prof.Location, loc)
	p.locCache[locKey] = loc

	return loc
}

// NewListener implements Wazero's experimental.FunctionListenerFactory.
func (p *ProfilerListener) NewListener(def api.FunctionDefinition) experimental.FunctionListener {
	funcNames := make([]string, len(p.profilers))

	some := false
	for i, profiler := range p.profilers {
		funcNames[i] = profiler.Listen(def.Name())
		some = true
	}

	if !some {
		return nil
	}

	hookKey := strings.Join(funcNames, "|")

	if h, ok := p.hooks[hookKey]; ok {
		return h
	}

	h := &hook{
		profiler:   p,
		processors: make([]ProfileProcessor, len(p.profilers)),
		samplers:   make([]Sampler, len(p.profilers)),
		values:     make([]int64, len(p.profilers)),
	}

	for i, name := range funcNames {
		if name == "" {
			continue
		}
		h.processors[i] = p.processorFns[keyFor(i, name)]
		h.samplers[i] = p.samplerFns[i]
	}

	p.hooks[hookKey] = h

	return h
}

type hook struct {
	profiler   *ProfilerListener
	samplers   []Sampler
	processors []ProfileProcessor
	values     []int64
	samples    []sample
}

func (p *ProfilerListener) createSample(si experimental.StackIterator, values []int64) sample {
	sample := sample{
		stack:  make([]stackEntry, 0, p.lastStackSize+1),
		values: values,
	}
	for si.Next() {
		fn := si.FunctionDefinition()
		pc := uint64(0) // si.SourceOffset()
		sample.stack = append(sample.stack, stackEntry{fn: fn, pc: pc})
	}
	return sample
}

// Before implements experimental.FunctionListener.
func (h *hook) Before(ctx context.Context, mod api.Module, fnd api.FunctionDefinition, params []uint64, si experimental.StackIterator) context.Context {
	any := false
	for i, sampler := range h.samplers {
		if sampler == nil {
			h.values[i] = 0
			continue
		}
		if sampler.Do() {
			h.values[i] = h.processors[i].Before(mod, params)
			any = true
		} else {
			h.values[i] = 0
		}
	}
	var s sample
	if any {
		s = h.profiler.createSample(si, h.values)
		if fnd.GoFunction() != nil {
			s.isIO = true
		}
		s.isSet = true
		//ctx = context.WithValue(ctx, "sample", s)
	}
	h.samples = append(h.samples, s)
	return ctx
}

// After implements experimental.FunctionListener.
func (h *hook) After(ctx context.Context, mod api.Module, fnd api.FunctionDefinition, err error, results []uint64) {
	// v := ctx.Value("sample")
	// if v == nil {
	// 	return
	// }
	//sample := v.(sample)
	sample := h.samples[len(h.samples)-1]
	if sample.isSet {
		deltas := make([]int64, len(sample.values))
		for i, processor := range h.processors {
			if processor == nil {
				// FIXME: processor should never be nil here.
				continue
			}
			deltas[i] = processor.After(sample.values[i], results)

		}
		sample.values = deltas
		h.profiler.reportSample(sample)
	}
	h.samples = h.samples[:len(h.samples)-1]
}
