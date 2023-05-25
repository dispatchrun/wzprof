package wzprof

import (
	"github.com/stealthrocket/wzprof/internal/gosym"
)

// A light adaptation of the unwinder from go/src/runtime/traceback. It is
// modified to work on a dedicated memory object instead of the current
// program's memory, and simplified for cases that don't concern GOARCH=wasm.
//
// uintptr has been replaced to ptr, and architecture-dependent values replaced
// for wasm. It still contains code to deal with race conditions because its was
// little work to keep around, only involves an pointer nil check to execute,
// and may be useful if wazero adds more concurrency when wasm threads support
// lands.

// Copyright (c) 2009 The Go Authors. All rights reserved.
//
// Redistribution and use in source and binary forms, with or without
// modification, are permitted provided that the following conditions are met:
//
//    * Redistributions of source code must retain the above copyright notice,
// this list of conditions and the following disclaimer.
//    * Redistributions in binary form must reproduce the above copyright
// notice, this list of conditions and the following disclaimer in the
// documentation and/or other materials provided with the distribution.
//    * Neither the name of Google Inc. nor the names of its contributors may be
// used to endorse or promote products derived from this software without
// specific prior written permission.
//
// THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
// AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
// IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE
// ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT OWNER OR CONTRIBUTORS BE
// LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR
// CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF
// SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS
// INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN
// CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE)
// ARISING IN ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE
// POSSIBILITY OF SUCH DAMAGE.

// A stkframe holds information about a single physical stack frame. Adapted
// from runtime/stkframe.go.
type stkframe struct {
	// fn is the function being run in this frame. If there is
	// inlining, this is the outermost function.
	fn *gosym.FuncInfo

	// pc is the program counter within fn.
	//
	// The meaning of this is subtle:
	//
	// - Typically, this frame performed a regular function call
	//   and this is the return PC (just after the CALL
	//   instruction). In this case, pc-1 reflects the CALL
	//   instruction itself and is the correct source of symbolic
	//   information.
	//
	// - If this frame "called" sigpanic, then pc is the
	//   instruction that panicked, and pc is the correct address
	//   to use for symbolic information.
	//
	// - If this is the innermost frame, then PC is where
	//   execution will continue, but it may not be the
	//   instruction following a CALL. This may be from
	//   cooperative preemption, in which case this is the
	//   instruction after the call to morestack. Or this may be
	//   from a signal or an un-started goroutine, in which case
	//   PC could be any instruction, including the first
	//   instruction in a function. Conventionally, we use pc-1
	//   for symbolic information, unless pc == fn.entry(), in
	//   which case we use pc.
	pc ptr

	// continpc is the PC where execution will continue in fn, or
	// 0 if execution will not continue in this frame.
	//
	// This is usually the same as pc, unless this frame "called"
	// sigpanic, in which case it's either the address of
	// deferreturn or 0 if this frame will never execute again.
	//
	// This is the PC to use to look up GC liveness for this frame.
	continpc ptr

	lr   ptr // program counter at caller aka link register
	sp   ptr // stack pointer at pc
	fp   ptr // stack pointer at caller aka frame pointer
	varp ptr // top of local variables
	argp ptr // pointer to function arguments
}

// unwindFlags control the behavior of various unwinders.
type unwindFlags uint8

const (
	// unwindPrintErrors indicates that if unwinding encounters an error, it
	// should print a message and stop without throwing. This is used for things
	// like stack printing, where it's better to get incomplete information than
	// to crash. This is also used in situations where everything may not be
	// stopped nicely and the stack walk may not be able to complete, such as
	// during profiling signals or during a crash.
	//
	// If neither unwindPrintErrors or unwindSilentErrors are set, unwinding
	// performs extra consistency checks and throws on any error.
	//
	// Note that there are a small number of fatal situations that will throw
	// regardless of unwindPrintErrors or unwindSilentErrors.
	unwindPrintErrors unwindFlags = 1 << iota

	// unwindSilentErrors silently ignores errors during unwinding.
	unwindSilentErrors

	// unwindTrap indicates that the initial PC and SP are from a trap, not a
	// return PC from a call.
	//
	// The unwindTrap flag is updated during unwinding. If set, frame.pc is the
	// address of a faulting instruction instead of the return address of a
	// call. It also means the liveness at pc may not be known.
	//
	// TODO: Distinguish frame.continpc, which is really the stack map PC, from
	// the actual continuation PC, which is computed differently depending on
	// this flag and a few other things.
	unwindTrap

	// unwindJumpStack indicates that, if the traceback is on a system stack, it
	// should resume tracing at the user stack when the system stack is
	// exhausted.
	unwindJumpStack
)

// An unwinder iterates the physical stack frames of a Go sack.
//
// Typical use of an unwinder looks like:
//
//	var u unwinder
//	for u.init(gp, 0); u.valid(); u.next() {
//		// ... use frame info in u ...
//	}
type unwinder struct {
	symbols *pclntabmapper
	mem     rtmem

	// frame is the current physical stack frame, or all 0s if
	// there is no frame.
	frame stkframe

	// g is the G who's stack is being unwound. If the
	// unwindJumpStack flag is set and the unwinder jumps stacks,
	// this will be different from the initial G.
	g gptr

	// cgoCtxt is the index into g.cgoCtxt of the next frame on the cgo stack.
	// The cgo stack is unwound in tandem with the Go stack as we find marker frames.
	// cgoCtxt int

	// calleeFuncID is the function ID of the caller of the current
	// frame.
	calleeFuncID gosym.FuncID

	// flags are the flags to this unwind. Some of these are updated as we
	// unwind (see the flags documentation).
	flags unwindFlags
}

const (
	goarchPtrSize = 8 // https://github.com/golang/go/blob/bd3f44e4ffe54e9cf841ebc8356e403bb38436bd/src/internal/goarch/goarch.go#L33
	sysPCQuantum  = 1 // https://github.com/golang/go/blob/49ad23a6d23d6cc1666c22e4bc215f25f717b569/src/internal/goarch/goarch_wasm.go
)

func (u *unwinder) initAt(pc0, sp0, lr0 ptr, gp gptr, flags unwindFlags) {
	if pc0 == ptr(^uint64(0)) && sp0 == ptr(^uint64(0)) {
		panic("should have been initialized")
	}

	var frame stkframe
	frame.pc = pc0
	frame.sp = sp0

	// If the PC is zero, it's likely a nil function call.
	// Start in the caller's frame.
	if frame.pc == 0 {
		frame.pc = u.mem.derefPtr(frame.sp)
		frame.sp += goarchPtrSize
	}

	f := u.symbols.info.FindFunc(uint64(frame.pc))
	if !f.Valid() {
		panic("could not find func fo pc")
	}
	frame.fn = f

	// Populate the unwinder.
	u.frame = frame
	u.flags = flags
	u.g = gp
	u.calleeFuncID = gosym.FuncIDNormal

	u.resolveInternal(true)
}

func (u *unwinder) valid() bool {
	return u.frame.pc != 0
}

// resolveInternal fills in u.frame based on u.frame.fn, pc, and sp.
//
// innermost indicates that this is the first resolve on this stack. If
// innermost is set.
//
// On entry, u.frame contains:
//   - fn is the running function.
//   - pc is the PC in the running function.
//   - sp is the stack pointer at that program counter.
//   - For the innermost frame on LR machines, lr is the program counter that called fn.
//
// On return, u.frame contains:
//   - fp is the stack pointer of the caller.
//   - lr is the program counter that called fn.
//   - varp, argp, and continpc are populated for the current frame.
//
// If fn is a stack-jumping function, resolveInternal can change the entire
// frame state to follow that stack jump.
//
// This is internal to unwinder.
func (u *unwinder) resolveInternal(innermost bool) {
	frame := &u.frame
	gp := u.g

	f := frame.fn
	if f.Pcsp == 0 {
		// No frame information, must be external function, like race support.
		// See golang.org/issue/13568.
		u.finishInternal()
		return
	}

	// Compute function info flags.
	flag := f.Flag

	// Found an actual function.
	// Derive frame pointer.
	if frame.fp == 0 {
		// Jump over system stack transitions. If we're on g0 and there's a user
		// goroutine, try to jump. Otherwise this is a regular call.
		// We also defensively check that this won't switch M's on us,
		// which could happen at critical points in the scheduler.
		// This ensures gp.m doesn't change from a stack jump.
		if u.flags&unwindJumpStack != 0 && gp == u.mem.gMG0(gp) && u.mem.gMCurg(gp) != 0 && ptr(u.mem.gMCurg(gp)) == u.mem.gM(gp) {
			switch f.FuncID {
			case gosym.FuncID_morestack:
				// morestack does not return normally -- newstack()
				// gogo's to curg.sched. Match that.
				// This keeps morestack() from showing up in the backtrace,
				// but that makes some sense since it'll never be returned
				// to.
				gp = u.mem.gMCurg(gp)
				u.g = gp
				frame.pc = u.mem.gSchedPc(gp)
				frame.fn = u.symbols.info.FindFunc(uint64(frame.pc))
				f = frame.fn
				flag = f.Flag
				frame.lr = u.mem.gSchedLr(gp)
				frame.sp = u.mem.gSchedSp(gp)
			case gosym.FuncID_systemstack:
				// systemstack returns normally, so just follow the
				// stack transition.
				gp = u.mem.gMCurg(gp)
				u.g = gp
				frame.sp = u.mem.gSchedSp(gp)
				flag &^= gosym.FuncFlagSPWrite
			}
		}
		frame.fp = frame.sp + ptr(funcspdelta(u.symbols.info.Pctab, f, frame.pc))
		frame.fp += goarchPtrSize
	}

	// Derive link register.
	if flag&gosym.FuncFlagTopFrame != 0 {
		// This function marks the top of the stack. Stop the traceback.
		frame.lr = 0
	} else if flag&gosym.FuncFlagSPWrite != 0 {
		// The function we are in does a write to SP that we don't know
		// how to encode in the spdelta table. Examples include context
		// switch routines like runtime.gogo but also any code that switches
		// to the g0 stack to run host C code.
		if u.flags&(unwindPrintErrors|unwindSilentErrors) != 0 {
			// We can't reliably unwind the SP (we might
			// not even be on the stack we think we are),
			// so stop the traceback here.
			frame.lr = 0
		} else {
			// For a GC stack traversal, we should only see
			// an SPWRITE function when it has voluntarily preempted itself on entry
			// during the stack growth check. In that case, the function has
			// not yet had a chance to do any writes to SP and is safe to unwind.
			// isAsyncSafePoint does not allow assembly functions to be async preempted,
			// and preemptPark double-checks that SPWRITE functions are not async preempted.
			// So for GC stack traversal, we can safely ignore SPWRITE for the innermost frame,
			// but farther up the stack we'd better not find any.
			if !innermost {
				panic("traceback: unexpected SPWRITE function")
			}
		}
	} else {
		var lrPtr ptr
		if frame.lr == 0 {
			lrPtr = frame.fp - goarchPtrSize
			frame.lr = u.mem.derefPtr(lrPtr)
		}
	}

	frame.varp = frame.fp
	// On [wasm], call instruction pushes return PC before entering new function.
	frame.varp -= goarchPtrSize

	// Determine frame's 'continuation PC', where it can continue.
	// Normally this is the return address on the stack, but if sigpanic
	// is immediately below this function on the stack, then the frame
	// stopped executing due to a trap, and frame.pc is probably not
	// a safe point for looking up liveness information. In this panicking case,
	// the function either doesn't return at all (if it has no defers or if the
	// defers do not recover) or it returns from one of the calls to
	// deferproc a second time (if the corresponding deferred func recovers).
	// In the latter case, use a deferreturn call site as the continuation pc.
	frame.continpc = frame.pc
	if u.calleeFuncID == gosym.FuncID_sigpanic {
		if frame.fn.Deferreturn != 0 {
			frame.continpc = ptr(frame.fn.Entry + uint64(frame.fn.Deferreturn) + 1)
			// Note: this may perhaps keep return variables alive longer than
			// strictly necessary, as we are using "function has a defer statement"
			// as a proxy for "function actually deferred something". It seems
			// to be a minor drawback. (We used to actually look through the
			// gp._defer for a defer corresponding to this function, but that
			// is hard to do with defer records on the stack during a stack copy.)
			// Note: the +1 is to offset the -1 that
			// stack.go:getStackMap does to back up a return
			// address make sure the pc is in the CALL instruction.
		} else {
			frame.continpc = 0
		}
	}
}

func (u *unwinder) next() {
	frame := &u.frame
	f := frame.fn

	// Do not unwind past the bottom of the stack.
	if frame.lr == 0 {
		u.finishInternal()
		return
	}
	flr := u.symbols.info.FindFunc(uint64(frame.lr))
	if !flr.Valid() {
		// This happens if you get a profiling interrupt at just the wrong time.
		// In that context it is okay to stop early.
		// But if no error flags are set, we're doing a garbage collection and must
		// get everything, so crash loudly.
		if u.flags&(unwindPrintErrors|unwindSilentErrors) == 0 {
			panic("unknown caller pc")
		}
		frame.lr = 0
		u.finishInternal()
		return
	}

	if frame.pc == frame.lr && frame.sp == frame.fp {
		// If the next frame is identical to the current frame, we cannot make progress.
		// print("runtime: traceback stuck. pc=", hex(frame.pc), " sp=", hex(frame.sp), "\n")
		// tracebackHexdump(gp.stack, frame, frame.sp)
		panic("traceback stuck")
	}

	injectedCall := f.FuncID == gosym.FuncID_sigpanic || f.FuncID == gosym.FuncID_asyncPreempt || f.FuncID == gosym.FuncID_debugCallV2
	if injectedCall {
		u.flags |= unwindTrap
	} else {
		u.flags &^= unwindTrap
	}

	// Unwind to next frame.
	u.calleeFuncID = f.FuncID
	frame.fn = flr
	frame.pc = frame.lr
	frame.lr = 0
	frame.sp = frame.fp
	frame.fp = 0

	u.resolveInternal(false)
}

// symPC returns the PC that should be used for symbolizing the current frame.
// Specifically, this is the PC of the last instruction executed in this frame.
//
// If this frame did a normal call, then frame.pc is a return PC, so this will
// return frame.pc-1, which points into the CALL instruction. If the frame was
// interrupted by a signal (e.g., profiler, segv, etc) then frame.pc is for the
// trapped instruction, so this returns frame.pc. See issue #34123. Finally,
// frame.pc can be at function entry when the frame is initialized without
// actually running code, like in runtime.mstart, in which case this returns
// frame.pc because that's the best we can do.
func (u *unwinder) symPC() ptr {
	if u.flags&unwindTrap == 0 && u.frame.pc > ptr(u.frame.fn.Entry) {
		// Regular call.
		return u.frame.pc - 1
	}
	// Trapping instruction or we're at the function entry point.
	return u.frame.pc
}

// finishInternal is an unwinder-internal helper called after the stack has been
// exhausted. It sets the unwinder to an invalid state.
func (u *unwinder) finishInternal() {
	u.frame.pc = 0
}

func funcspdelta(pctab []byte, f *gosym.FuncInfo, targetpc ptr) int32 {
	x, _ := pcvalue(pctab, f, f.Pcsp, targetpc, true)
	return x
}

// Returns the PCData value, and the PC where this value starts.
func pcvalue(pctab []byte, f *gosym.FuncInfo, off uint32, targetpc ptr, strict bool) (int32, ptr) {
	if off == 0 {
		return -1, 0
	}

	if !f.Valid() {
		panic("no module data")
		// if strict && panicking.Load() == 0 {
		// 	println("runtime: no module data for", hex(f.entry()))
		// 	throw("no module data")
		// }
		// return -1, 0
	}
	p := pctab[off:]
	pc := ptr(f.Entry)
	prevpc := pc
	val := int32(-1)
	for {
		var ok bool
		p, ok = step(p, &pc, &val, pc == ptr(f.Entry))
		if !ok {
			break
		}
		if targetpc < pc {
			// Replace a random entry in the cache. Random
			// replacement prevents a performance cliff if
			// a recursive stack's cycle is slightly
			// larger than the cache.
			// Put the new element at the beginning,
			// since it is the most likely to be newly used.
			// if cache != nil {
			// 	x := pcvalueCacheKey(targetpc)
			// 	e := &cache.entries[x]
			// 	ci := fastrandn(uint32(len(cache.entries[x])))
			// 	e[ci] = e[0]
			// 	e[0] = pcvalueCacheEnt{
			// 		targetpc: targetpc,
			// 		off:      off,
			// 		val:      val,
			// 	}
			// }

			return val, prevpc
		}
		prevpc = pc
	}

	panic("invalid pc-encoded table")
}

// step advances to the next pc, value pair in the encoded table.
func step(p []byte, pc *ptr, val *int32, first bool) (newp []byte, ok bool) {
	// For both uvdelta and pcdelta, the common case (~70%)
	// is that they are a single byte. If so, avoid calling readvarint.
	uvdelta := uint32(p[0])
	if uvdelta == 0 && !first {
		return nil, false
	}
	n := uint32(1)
	if uvdelta&0x80 != 0 {
		n, uvdelta = readvarint(p)
	}
	*val += int32(-(uvdelta & 1) ^ (uvdelta >> 1))
	p = p[n:]

	pcdelta := uint32(p[0])
	n = 1
	if pcdelta&0x80 != 0 {
		n, pcdelta = readvarint(p)
	}
	p = p[n:]
	*pc += ptr(pcdelta * sysPCQuantum)
	return p, true
}

// readvarint reads a varint from p.
func readvarint(p []byte) (read uint32, val uint32) {
	var v, shift, n uint32
	for {
		b := p[n]
		n++
		v |= uint32(b&0x7F) << (shift & 31)
		if b&0x80 == 0 {
			break
		}
		shift += 7
	}
	return n, v
}

// inlinedCall is the encoding of entries in the FUNCDATA_InlTree table.
type inlinedCall struct {
	funcID    gosym.FuncID // type of the called function
	_         [3]byte
	nameOff   int32 // offset into pclntab for name of called function
	parentPc  int32 // position of an instruction whose source position is the call site (offset from entry)
	startLine int32 // line number of start of function (func keyword/TEXT directive)
}

type inlineUnwinder struct {
	symbols *pclntabmapper
	mem     rtmem
	f       *gosym.FuncInfo
	inlTree ptr // Address of the array of inlinedCall entries
}

// next returns the frame representing uf's logical caller.
func (u *inlineUnwinder) next(uf inlineFrame) inlineFrame {
	if uf.index < 0 {
		uf.pc = 0
		return uf
	}
	c := readSliceIndex[inlinedCall](u.mem, u.inlTree, uf.index)
	return u.resolveInternal(ptr(u.f.Entry) + ptr(c.parentPc))
}

// srcFunc returns the srcFunc representing the given frame.
func (u *inlineUnwinder) srcFunc(uf inlineFrame) gosym.SrcFunc {
	if uf.index < 0 {
		return u.f.SrcFunc()
	}
	t := readSliceIndex[inlinedCall](u.mem, u.inlTree, uf.index)
	// t := &u.inlTree[uf.index]
	return gosym.SrcFunc{
		// u.f.datap,
		t.nameOff,
		t.startLine,
		t.funcID,
	}
}

func (u *inlineUnwinder) resolveInternal(pc ptr) inlineFrame {
	return inlineFrame{
		pc: pc,
		// Conveniently, this returns -1 if there's an error, which is the same
		// value we use for the outermost frame.
		index: pcdatavalue1(u.symbols.info.Pctab, u.f, gosym.PCDATA_InlTreeIndex, pc, false),
	}
}

func pcdatavalue1(pctab []byte, f *gosym.FuncInfo, table uint32, targetpc ptr, strict bool) int32 {
	if table >= f.Npcdata {
		return -1
	}
	r, _ := pcvalue(pctab, f, f.PcdataOffset(table), targetpc, strict)
	return r
}

// An inlineFrame is a position in an inlineUnwinder.
type inlineFrame struct {
	// pc is the PC giving the file/line metadata of the current frame. This is
	// always a "call PC" (not a "return PC"). This is 0 when the iterator is
	// exhausted.
	pc ptr

	// index is the index of the current record in inlTree, or -1 if we are in
	// the outermost function.
	index int32
}

func (uf inlineFrame) valid() bool {
	return uf.pc != 0
}

func newInlineUnwinder(symbols *pclntabmapper, mem rtmem, f *gosym.FuncInfo, pc ptr) (inlineUnwinder, inlineFrame) {
	inldataAddr := funcdata(symbols, f, gosym.FUNCDATA_InlTree)
	if inldataAddr == 0 {
		return inlineUnwinder{symbols: symbols, mem: mem, f: f}, inlineFrame{pc: pc, index: -1}
	}
	u := inlineUnwinder{symbols: symbols, mem: mem, f: f, inlTree: inldataAddr}
	return u, u.resolveInternal(pc)
}

// funcdata returns a pointer to the ith funcdata for f.
// funcdata should be kept in sync with cmd/link:writeFuncs.
func funcdata(symbols *pclntabmapper, f *gosym.FuncInfo, i uint8) ptr {
	if i < 0 || i >= f.Nfuncdata {
		return 0
	}
	base := symbols.moduledata.gofunc
	// base := f.datap.gofunc // load gofunc address early so that we calculate during cache misses
	off := f.FuncDataOffset(i)

	// Return off == ^uint32(0) ? 0 : f.datap.gofunc + uintptr(off), but without branches.
	// The compiler calculates mask on most architectures using conditional assignment.
	var mask ptr
	if off == ^uint32(0) {
		mask = 1
	}
	mask--
	raw := base + ptr(off)
	return raw & mask
}
