package wzprof

import (
	"github.com/stealthrocket/wzprof/internal/gosym"
)

// A light adaptation of the unwinder from go/src/runtime/traceback. It is
// modified to work on a dedicated memory object instead of the current
// program's memory, and simplified for cases that don't concern GOARCH=wasm.
//
// uintptr has been replaced to uint64.

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

// https://github.com/golang/go/blob/49ad23a6d23d6cc1666c22e4bc215f25f717b569/src/runtime/runtime2.go

// A stkframe holds information about a single physical stack frame.
// runtime/stkframe.go
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

// runtime/symbtab.go
type pcvalueCache struct {
	entries [2][8]pcvalueCacheEnt
}
type pcvalueCacheEnt struct {
	// targetpc and off together are the key of this cache entry.
	targetpc uintptr
	off      uint32
	// val is the value of this cached pcvalue entry.
	val int32
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
//
// Implementation note: This is carefully structured to be pointer-free because
// tracebacks happen in places that disallow write barriers (e.g., signals).
// Even if this is stack-allocated, its pointer-receiver methods don't know that
// their receiver is on the stack, so they still emit write barriers. Here we
// address that by carefully avoiding any pointers in this type. Another
// approach would be to split this into a mutable part that's passed by pointer
// but contains no pointers itself and an immutable part that's passed and
// returned by value and can contain pointers. We could potentially hide that
// we're doing that in trivial methods that are inlined into the caller that has
// the stack allocation, but that's fragile.
type unwinder struct {
	pclntab gosym.RuntimeInfo
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

	// cache is used to cache pcvalue lookups.
	cache pcvalueCache
}

const (
	goarchPtrSize   = 8 // https://github.com/golang/go/blob/bd3f44e4ffe54e9cf841ebc8356e403bb38436bd/src/internal/goarch/goarch.go#L33
	sysMinFrameSize = 0 // https://github.com/golang/go/blob/bd3f44e4ffe54e9cf841ebc8356e403bb38436bd/src/internal/goarch/goarch_wasm.go
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

	f := u.pclntab.FindFunc(uint64(frame.pc))
	if !f.Valid() {
		panic("couldn't find func fo pc")
		// if flags&unwindSilentErrors == 0 {
		// 	print("runtime: g ", gp.goid, ": unknown pc ", hex(frame.pc), "\n")
		// 	tracebackHexdump(gp.stack, &frame, 0)
		// }
		// if flags&(unwindPrintErrors|unwindSilentErrors) == 0 {
		// 	throw("unknown pc")
		// }
		// *u = unwinder{}
		// return
	}
	frame.fn = f

	// Populate the unwinder.
	*u = unwinder{
		frame: frame,
		// g:            gp.guintptr(),
		// cgoCtxt:      len(gp.cgoCtxt) - 1,
		// calleeFuncID: abi.FuncIDNormal,
		flags: flags,
	}

	isSyscall := frame.pc == pc0 && frame.sp == sp0 && pc0 == gp.syscallpc && sp0 == gp.syscallsp
	u.resolveInternal(true, isSyscall)
}

func (u *unwinder) valid() bool {
	return u.frame.pc != 0
}

// resolveInternal fills in u.frame based on u.frame.fn, pc, and sp.
//
// innermost indicates that this is the first resolve on this stack. If
// innermost is set, isSyscall indicates that the PC/SP was retrieved from
// gp.syscall*; this is otherwise ignored.
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
func (u *unwinder) resolveInternal(innermost, isSyscall bool) {
	frame := &u.frame
	gp := u.g.ptr()

	f := frame.fn
	if f.Pcsp == 0 {
		// No frame information, must be external function, like race support.
		// See golang.org/issue/13568.
		u.finishInternal()
		return
	}

	// Compute function info flags.
	flag := f.Flag
	// if f.funcID == abi.FuncID_cgocallback {
	// 	// cgocallback does write SP to switch from the g0 to the curg stack,
	// 	// but it carefully arranges that during the transition BOTH stacks
	// 	// have cgocallback frame valid for unwinding through.
	// 	// So we don't need to exclude it with the other SP-writing functions.
	// 	flag &^= abi.FuncFlagSPWrite
	// }
	// if isSyscall {
	// 	// Some Syscall functions write to SP, but they do so only after
	// 	// saving the entry PC/SP using entersyscall.
	// 	// Since we are using the entry PC/SP, the later SP write doesn't matter.
	// 	flag &^= abi.FuncFlagSPWrite
	// }

	// Found an actual function.
	// Derive frame pointer.
	if frame.fp == 0 {
		// Jump over system stack transitions. If we're on g0 and there's a user
		// goroutine, try to jump. Otherwise this is a regular call.
		// We also defensively check that this won't switch M's on us,
		// which could happen at critical points in the scheduler.
		// This ensures gp.m doesn't change from a stack jump.
		if u.flags&unwindJumpStack != 0 && gp == gp.m.g0 && gp.m.curg != nil && gp.m.curg.m == gp.m {
			switch f.FuncID {
			case gosym.FuncID_morestack:
				// morestack does not return normally -- newstack()
				// gogo's to curg.sched. Match that.
				// This keeps morestack() from showing up in the backtrace,
				// but that makes some sense since it'll never be returned
				// to.
				gp = gp.m.curg
				u.g.set(gp)
				frame.pc = gp.sched.pc
				frame.fn = u.pclntab.FindFunc(uint64(frame.pc))
				f = frame.fn
				flag = f.Flag
				frame.lr = gp.sched.lr
				frame.sp = gp.sched.sp
				// u.cgoCtxt = len(gp.cgoCtxt) - 1
			case gosym.FuncID_systemstack:
				// systemstack returns normally, so just follow the
				// stack transition.

				// if usesLR && funcspdelta(f, frame.pc, &u.cache) == 0 {
				// 	// We're at the function prologue and the stack
				// 	// switch hasn't happened, or epilogue where we're
				// 	// about to return. Just unwind normally.
				// 	// Do this only on LR machines because on x86
				// 	// systemstack doesn't have an SP delta (the LL
				// 	// instruction opens the frame), therefore no way					// to check.
				// 	flag &^= abi.FuncFlagSPWrite
				// 	break
				// }

				gp = gp.m.curg
				u.g.set(gp)
				frame.sp = gp.sched.sp
				// u.cgoCtxt = len(gp.cgoCtxt) - 1
				flag &^= gosym.FuncFlagSPWrite
			}
		}
		frame.fp = frame.sp + uintptr(funcspdelta(f, frame.pc, &u.cache))
		// if !usesLR {
		// On x86, call instruction pushes return PC before entering new function.
		frame.fp += goarchPtrSize
		//}
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
				//				panic("traceback")
			}
		}
	} else {
		var lrPtr ptr
		// if usesLR {
		// 	if innermost && frame.sp < frame.fp || frame.lr == 0 {
		// 		lrPtr = frame.sp
		// 		frame.lr = *(*uintptr)(unsafe.Pointer(lrPtr))
		// 	}
		//} else {
		if frame.lr == 0 {
			lrPtr = frame.fp - goarchPtrSize
			frame.lr = u.mem.derefPtr(lrPtr)
		}
		//}
	}

	frame.varp = frame.fp
	// if !usesLR {
	// On x86, call instruction pushes return PC before entering new function.
	frame.varp -= goarchPtrSize
	//}

	// For architectures with frame pointers, if there's
	// a frame, then there's a saved frame pointer here.
	//
	// NOTE: This code is not as general as it looks.
	// On x86, the ABI is to save the frame pointer word at the
	// top of the stack frame, so we have to back down over it.
	// On arm64, the frame pointer should be at the bottom of
	// the stack (with R29 (aka FP) = RSP), in which case we would
	// not want to do the subtraction here. But we started out without
	// any frame pointer, and when we wanted to add it, we didn't
	// want to break all the assembly doing direct writes to 8(RSP)
	// to set the first parameter to a called function.
	// So we decided to write the FP link *below* the stack pointer
	// (with R29 = RSP - 8 in Go functions).
	// This is technically ABI-compatible but not standard.
	// And it happens to end up mimicking the x86 layout.
	// Other architectures may make different decisions.
	// if frame.varp > frame.sp && framepointer_enabled {
	// 	frame.varp -= goarchPtrSize
	// }

	frame.argp = frame.fp + sysMinFrameSize

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
	// gp := u.g.ptr()

	// Do not unwind past the bottom of the stack.
	if frame.lr == 0 {
		u.finishInternal()
		return
	}
	flr := u.pclntab.FindFunc(uint64(frame.lr))
	if !flr.Valid() {
		// This happens if you get a profiling interrupt at just the wrong time.
		// In that context it is okay to stop early.
		// But if no error flags are set, we're doing a garbage collection and must
		// get everything, so crash loudly.
		fail := u.flags&(unwindPrintErrors|unwindSilentErrors) == 0
		doPrint := false
		// doPrint := u.flags&unwindSilentErrors == 0
		// if doPrint && gp.m.incgo && f.FuncID == gosym.FuncID_sigpanic {
		// 	// We can inject sigpanic
		// 	// calls directly into C code,
		// 	// in which case we'll see a C
		// 	// return PC. Don't complain.
		// 	doPrint = false
		// }
		if fail || doPrint {
			panic("this should not do print")
			//			print("runtime: g ", gp.goid, ": unexpected return pc for ", funcname(f), " called from ", hex(frame.lr), "\n")
			//			tracebackHexdump(gp.stack, frame, 0)
		}
		if fail {
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

	// On link register architectures, sighandler saves the LR on stack
	// before faking a call.
	// if usesLR && injectedCall {
	// 	x := *(*uintptr)(unsafe.Pointer(frame.sp))
	// 	frame.sp += alignUp(sys.MinFrameSize, sys.StackAlign)
	// 	f = findfunc(frame.pc)
	// 	frame.fn = f
	// 	if !f.valid() {
	// 		frame.pc = x
	// 	} else if funcspdelta(f, frame.pc, &u.cache) == 0 {
	// 		frame.lr = x
	// 	}
	// }

	u.resolveInternal(false, false)
}

// finishInternal is an unwinder-internal helper called after the stack has been
// exhausted. It sets the unwinder to an invalid state and checks that it
// successfully unwound the entire stack.
func (u *unwinder) finishInternal() {
	u.frame.pc = 0

	// Note that panic != nil is okay here: there can be leftover panics,
	// because the defers on the panic stack do not nest in frame order as
	// they do on the defer stack. If you have:
	//
	//	frame 1 defers d1
	//	frame 2 defers d2
	//	frame 3 defers d3
	//	frame 4 panics
	//	frame 4's panic starts running defers
	//	frame 5, running d3, defers d4
	//	frame 5 panics
	//	frame 5's panic starts running defers
	//	frame 6, running d4, garbage collects
	//	frame 6, running d2, garbage collects
	//
	// During the execution of d4, the panic stack is d4 -> d3, which
	// is nested properly, and we'll treat frame 3 as resumable, because we
	// can find d3. (And in fact frame 3 is resumable. If d4 recovers
	// and frame 5 continues running, d3, d3 can recover and we'll
	// resume execution in (returning from) frame 3.)
	//
	// During the execution of d2, however, the panic stack is d2 -> d3,
	// which is inverted. The scan will match d2 to frame 2 but having
	// d2 on the stack until then means it will not match d3 to frame 3.
	// This is okay: if we're running d2, then all the defers after d2 have
	// completed and their corresponding frames are dead. Not finding d3
	// for frame 3 means we'll set frame 3's continpc == 0, which is correct
	// (frame 3 is dead). At the end of the walk the panic stack can thus
	// contain defers (d3 in this case) for dead frames. The inversion here
	// always indicates a dead frame, and the effect of the inversion on the
	// scan is to hide those dead frames, so the scan is still okay:
	// what's left on the panic stack are exactly (and only) the dead frames.
	//
	// We require callback != nil here because only when callback != nil
	// do we know that gentraceback is being called in a "must be correct"
	// context as opposed to a "best effort" context. The tracebacks with
	// callbacks only happen when everything is stopped nicely.
	// At other times, such as when gathering a stack for a profiling signal
	// or when printing a traceback during a crash, everything may not be
	// stopped nicely, and the stack walk may not be able to complete.
	gp := u.g.ptr()
	if u.flags&(unwindPrintErrors|unwindSilentErrors) == 0 && u.frame.sp != gp.stktopsp {
		//		print("runtime: g", gp.goid, ": frame.sp=", hex(u.frame.sp), " top=", hex(gp.stktopsp), "\n")
		//		print("\tstack=[", hex(gp.stack.lo), "-", hex(gp.stack.hi), "\n")
		panic("traceback did not unwind completely")
	}
}
