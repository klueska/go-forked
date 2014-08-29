// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"unsafe"
)

// Malloc profiling.
// Patterned after tcmalloc's algorithms; shorter code.

// NOTE(rsc): Everything here could use cas if contention became an issue.
var proflock mutex

// All memory allocations are local and do not escape outside of the profiler.
// The profiler is forbidden from referring to garbage-collected memory.

var (
	mbuckets *bucket // memory profile buckets
	bbuckets *bucket // blocking profile buckets
)

// MemProfile returns n, the number of records in the current memory profile.
// If len(p) >= n, MemProfile copies the profile into p and returns n, true.
// If len(p) < n, MemProfile does not change p and returns n, false.
//
// If inuseZero is true, the profile includes allocation records
// where r.AllocBytes > 0 but r.AllocBytes == r.FreeBytes.
// These are sites where memory was allocated, but it has all
// been released back to the runtime.
//
// Most clients should use the runtime/pprof package or
// the testing package's -test.memprofile flag instead
// of calling MemProfile directly.
func MemProfile(p []MemProfileRecord, inuseZero bool) (n int, ok bool) {
	lock(&proflock)
	clear := true
	for b := mbuckets; b != nil; b = b.allnext {
		if inuseZero || b.data.mp.alloc_bytes != b.data.mp.free_bytes {
			n++
		}
		if b.data.mp.allocs != 0 || b.data.mp.frees != 0 {
			clear = false
		}
	}
	if clear {
		// Absolutely no data, suggesting that a garbage collection
		// has not yet happened. In order to allow profiling when
		// garbage collection is disabled from the beginning of execution,
		// accumulate stats as if a GC just happened, and recount buckets.
		mprof_GC()
		mprof_GC()
		n = 0
		for b := mbuckets; b != nil; b = b.allnext {
			if inuseZero || b.data.mp.alloc_bytes != b.data.mp.free_bytes {
				n++
			}
		}
	}
	if n <= len(p) {
		ok = true
		idx := 0
		for b := mbuckets; b != nil; b = b.allnext {
			if inuseZero || b.data.mp.alloc_bytes != b.data.mp.free_bytes {
				record(&p[idx], b)
				idx++
			}
		}
	}
	unlock(&proflock)
	return
}

func mprof_GC() {
	for b := mbuckets; b != nil; b = b.allnext {
		b.data.mp.allocs += b.data.mp.prev_allocs
		b.data.mp.frees += b.data.mp.prev_frees
		b.data.mp.alloc_bytes += b.data.mp.prev_alloc_bytes
		b.data.mp.free_bytes += b.data.mp.prev_free_bytes

		b.data.mp.prev_allocs = b.data.mp.recent_allocs
		b.data.mp.prev_frees = b.data.mp.recent_frees
		b.data.mp.prev_alloc_bytes = b.data.mp.recent_alloc_bytes
		b.data.mp.prev_free_bytes = b.data.mp.recent_free_bytes

		b.data.mp.recent_allocs = 0
		b.data.mp.recent_frees = 0
		b.data.mp.recent_alloc_bytes = 0
		b.data.mp.recent_free_bytes = 0
	}
}

// Write b's data to r.
func record(r *MemProfileRecord, b *bucket) {
	r.AllocBytes = int64(b.data.mp.alloc_bytes)
	r.FreeBytes = int64(b.data.mp.free_bytes)
	r.AllocObjects = int64(b.data.mp.allocs)
	r.FreeObjects = int64(b.data.mp.frees)
	for i := 0; uintptr(i) < b.nstk && i < len(r.Stack0); i++ {
		r.Stack0[i] = *(*uintptr)(add(unsafe.Pointer(&b.stk), uintptr(i)*ptrSize))
	}
	for i := b.nstk; i < uintptr(len(r.Stack0)); i++ {
		r.Stack0[i] = 0
	}
}

// BlockProfile returns n, the number of records in the current blocking profile.
// If len(p) >= n, BlockProfile copies the profile into p and returns n, true.
// If len(p) < n, BlockProfile does not change p and returns n, false.
//
// Most clients should use the runtime/pprof package or
// the testing package's -test.blockprofile flag instead
// of calling BlockProfile directly.
func BlockProfile(p []BlockProfileRecord) (n int, ok bool) {
	lock(&proflock)
	for b := bbuckets; b != nil; b = b.allnext {
		n++
	}
	if n <= len(p) {
		ok = true
		idx := 0
		for b := bbuckets; b != nil; b = b.allnext {
			bp := (*bprofrecord)(unsafe.Pointer(&b.data))
			p[idx].Count = int64(bp.count)
			p[idx].Cycles = int64(bp.cycles)
			i := 0
			for uintptr(i) < b.nstk && i < len(p[idx].Stack0) {
				p[idx].Stack0[i] = *(*uintptr)(add(unsafe.Pointer(&b.stk), uintptr(i)*ptrSize))
				i++
			}
			for i < len(p[idx].Stack0) {
				p[idx].Stack0[i] = 0
				i++
			}
			idx++
		}
	}
	unlock(&proflock)
	return
}

// Stack formats a stack trace of the calling goroutine into buf
// and returns the number of bytes written to buf.
// If all is true, Stack formats stack traces of all other goroutines
// into buf after the trace for the current goroutine.
func Stack(buf []byte, all bool) int {
	sp := getcallersp(unsafe.Pointer(&buf))
	pc := getcallerpc(unsafe.Pointer(&buf))
	mp := acquirem()
	gp := mp.curg
	if all {
		semacquire(&worldsema, false)
		mp.gcing = 1
		releasem(mp)
		stoptheworld()
		if mp != acquirem() {
			gothrow("Stack: rescheduled")
		}
	}

	n := 0
	if len(buf) > 0 {
		gp.writebuf = buf
		goroutineheader(gp)
		traceback(pc, sp, 0, gp)
		if all {
			tracebackothers(gp)
		}
		n = len(buf) - len(gp.writebuf)
		gp.writebuf = nil
	}

	if all {
		mp.gcing = 0
		semrelease(&worldsema)
		starttheworld()
	}
	releasem(mp)
	return n
}

// ThreadCreateProfile returns n, the number of records in the thread creation profile.
// If len(p) >= n, ThreadCreateProfile copies the profile into p and returns n, true.
// If len(p) < n, ThreadCreateProfile does not change p and returns n, false.
//
// Most clients should use the runtime/pprof package instead
// of calling ThreadCreateProfile directly.
func ThreadCreateProfile(p []StackRecord) (n int, ok bool) {
	first := (*m)(atomicloadp(unsafe.Pointer(&allm)))
	for mp := first; mp != nil; mp = mp.alllink {
		n++
	}
	if n <= len(p) {
		ok = true
		i := 0
		for mp := first; mp != nil; mp = mp.alllink {
			for s := range mp.createstack {
				p[i].Stack0[s] = uintptr(mp.createstack[s])
			}
			i++
		}
	}
	return
}
