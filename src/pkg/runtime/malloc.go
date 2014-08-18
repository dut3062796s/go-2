// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"unsafe"
)

const (
	debugMalloc = false

	flagNoScan = 1 << 0 // GC doesn't have to scan object
	flagNoZero = 1 << 1 // don't zero memory

	kindArray      = 17
	kindFunc       = 19
	kindInterface  = 20
	kindPtr        = 22
	kindStruct     = 25
	kindMask       = 1<<6 - 1
	kindGCProg     = 1 << 6
	kindNoPointers = 1 << 7

	maxTinySize   = 16
	tinySizeClass = 2
	maxSmallSize  = 32 << 10

	pageShift = 13
	pageSize  = 1 << pageShift
	pageMask  = pageSize - 1

	wordsPerBitmapWord = ptrSize * 8 / 4
	gcBits             = 4
	bitsPerPointer     = 2
	bitsMask           = 1<<bitsPerPointer - 1
	pointersPerByte    = 8 / bitsPerPointer
	bitPtrMask         = bitsMask << 2
	maxGCMask          = 0 // disabled because wastes several bytes of memory
	bitsDead           = 0
	bitsPointer        = 2

	bitBoundary = 1
	bitMarked   = 2
	bitMask     = bitBoundary | bitMarked
)

// All zero-sized allocations return a pointer to this byte.
var zeroObject byte

// Maximum possible heap size.
var maxMem uintptr

// Allocate an object of size bytes.
// Small objects are allocated from the per-P cache's free lists.
// Large objects (> 32 kB) are allocated straight from the heap.
func gomallocgc(size uintptr, typ *_type, flags int) unsafe.Pointer {
	if size == 0 {
		return unsafe.Pointer(&zeroObject)
	}
	mp := acquirem()
	if mp.mallocing != 0 {
		gothrow("malloc/free - deadlock")
	}
	mp.mallocing = 1
	size0 := size

	c := mp.mcache
	var s *mspan
	var x unsafe.Pointer
	if size <= maxSmallSize {
		if flags&flagNoScan != 0 && size < maxTinySize {
			// Tiny allocator.
			//
			// Tiny allocator combines several tiny allocation requests
			// into a single memory block. The resulting memory block
			// is freed when all subobjects are unreachable. The subobjects
			// must be FlagNoScan (don't have pointers), this ensures that
			// the amount of potentially wasted memory is bounded.
			//
			// Size of the memory block used for combining (maxTinySize) is tunable.
			// Current setting is 16 bytes, which relates to 2x worst case memory
			// wastage (when all but one subobjects are unreachable).
			// 8 bytes would result in no wastage at all, but provides less
			// opportunities for combining.
			// 32 bytes provides more opportunities for combining,
			// but can lead to 4x worst case wastage.
			// The best case winning is 8x regardless of block size.
			//
			// Objects obtained from tiny allocator must not be freed explicitly.
			// So when an object will be freed explicitly, we ensure that
			// its size >= maxTinySize.
			//
			// SetFinalizer has a special case for objects potentially coming
			// from tiny allocator, it such case it allows to set finalizers
			// for an inner byte of a memory block.
			//
			// The main targets of tiny allocator are small strings and
			// standalone escaping variables. On a json benchmark
			// the allocator reduces number of allocations by ~12% and
			// reduces heap size by ~20%.

			tinysize := uintptr(c.tinysize)
			if size <= tinysize {
				tiny := unsafe.Pointer(c.tiny)
				// Align tiny pointer for required (conservative) alignment.
				if size&7 == 0 {
					tiny = roundup(tiny, 8)
				} else if size&3 == 0 {
					tiny = roundup(tiny, 4)
				} else if size&1 == 0 {
					tiny = roundup(tiny, 2)
				}
				size1 := size + (uintptr(tiny) - uintptr(unsafe.Pointer(c.tiny)))
				if size1 <= tinysize {
					// The object fits into existing tiny block.
					x = tiny
					c.tiny = (*byte)(add(x, size))
					c.tinysize -= uint(size1)
					mp.mallocing = 0
					releasem(mp)
					return x
				}
			}
			// Allocate a new maxTinySize block.
			s = c.alloc[tinySizeClass]
			v := s.freelist
			if v == nil {
				mp.scalararg[0] = tinySizeClass
				onM(&mcacheRefill_m)
				s = c.alloc[tinySizeClass]
				v = s.freelist
			}
			s.freelist = v.next
			s.ref++
			//TODO: prefetch v.next
			x = unsafe.Pointer(v)
			(*[2]uint64)(x)[0] = 0
			(*[2]uint64)(x)[1] = 0
			// See if we need to replace the existing tiny block with the new one
			// based on amount of remaining free space.
			if maxTinySize-size > tinysize {
				c.tiny = (*byte)(add(x, size))
				c.tinysize = uint(maxTinySize - size)
			}
			size = maxTinySize
		} else {
			var sizeclass int8
			if size <= 1024-8 {
				sizeclass = size_to_class8[(size+7)>>3]
			} else {
				sizeclass = size_to_class128[(size-1024+127)>>7]
			}
			size = uintptr(class_to_size[sizeclass])
			s = c.alloc[sizeclass]
			v := s.freelist
			if v == nil {
				mp.scalararg[0] = uint(sizeclass)
				onM(&mcacheRefill_m)
				s = c.alloc[sizeclass]
				v = s.freelist
			}
			s.freelist = v.next
			s.ref++
			//TODO: prefetch
			x = unsafe.Pointer(v)
			if flags&flagNoZero == 0 {
				v.next = nil
				if size > 2*ptrSize && ((*[2]uintptr)(x))[1] != 0 {
					memclr(unsafe.Pointer(v), size)
				}
			}
		}
		c.local_cachealloc += int(size)
	} else {
		mp.scalararg[0] = uint(size)
		mp.scalararg[1] = uint(flags)
		onM(&largeAlloc_m)
		s = (*mspan)(mp.ptrarg[0])
		mp.ptrarg[0] = nil
		x = unsafe.Pointer(uintptr(s.start << pageShift))
		size = uintptr(s.elemsize)
	}

	if flags&flagNoScan != 0 {
		// All objects are pre-marked as noscan.
		goto marked
	}

	// From here till marked label marking the object as allocated
	// and storing type info in the GC bitmap.
	{
		arena_start := uintptr(unsafe.Pointer(mheap_.arena_start))
		off := (uintptr(x) - arena_start) / ptrSize
		xbits := (*uintptr)(unsafe.Pointer(arena_start - off/wordsPerBitmapWord*ptrSize - ptrSize))
		shift := (off % wordsPerBitmapWord) * gcBits
		if debugMalloc && ((*xbits>>shift)&(bitMask|bitPtrMask)) != bitBoundary {
			println("runtime: bits =", (*xbits>>shift)&(bitMask|bitPtrMask))
			gothrow("bad bits in markallocated")
		}

		var ti, te uintptr
		var ptrmask *uint8
		if size == ptrSize {
			// It's one word and it has pointers, it must be a pointer.
			*xbits |= (bitsPointer << 2) << shift
			goto marked
		}
		if typ != nil && (uintptr(typ.gc[0])|uintptr(typ.gc[1])) != 0 && uintptr(typ.size) > ptrSize {
			if typ.kind&kindGCProg != 0 {
				nptr := (uintptr(typ.size) + ptrSize - 1) / ptrSize
				masksize := nptr
				if masksize%2 != 0 {
					masksize *= 2 // repeated
				}
				masksize = masksize * pointersPerByte / 8 // 4 bits per word
				masksize++                                // unroll flag in the beginning
				if masksize > maxGCMask && typ.gc[1] != 0 {
					// If the mask is too large, unroll the program directly
					// into the GC bitmap. It's 7 times slower than copying
					// from the pre-unrolled mask, but saves 1/16 of type size
					// memory for the mask.
					mp.ptrarg[0] = x
					mp.ptrarg[1] = unsafe.Pointer(typ)
					mp.scalararg[0] = uint(size)
					mp.scalararg[1] = uint(size0)
					onM(&unrollgcproginplace_m)
					goto marked
				}
				ptrmask = (*uint8)(unsafe.Pointer(uintptr(typ.gc[0])))
				// Check whether the program is already unrolled.
				if uintptr(goatomicloadp(unsafe.Pointer(ptrmask)))&0xff == 0 {
					mp.ptrarg[0] = unsafe.Pointer(typ)
					onM(&unrollgcprog_m)
				}
				ptrmask = (*uint8)(add(unsafe.Pointer(ptrmask), 1)) // skip the unroll flag byte
			} else {
				ptrmask = (*uint8)(unsafe.Pointer(&typ.gc[0])) // embed mask
			}
			if size == 2*ptrSize {
				xbitsb := (*uint8)(add(unsafe.Pointer(xbits), shift/8))
				*xbitsb = *ptrmask | bitBoundary
				goto marked
			}
			te = uintptr(typ.size) / ptrSize
			// If the type occupies odd number of words, its mask is repeated.
			if te%2 == 0 {
				te /= 2
			}
		}
		if size == 2*ptrSize {
			xbitsb := (*uint8)(add(unsafe.Pointer(xbits), shift/8))
			*xbitsb = (bitsPointer << 2) | (bitsPointer << 6) | bitBoundary
			goto marked
		}
		// Copy pointer bitmask into the bitmap.
		for i := uintptr(0); i < size0; i += 2 * ptrSize {
			v := uint8((bitsPointer << 2) | (bitsPointer << 6))
			if ptrmask != nil {
				v = *(*uint8)(add(unsafe.Pointer(ptrmask), ti))
				ti++
				if ti == te {
					ti = 0
				}
			}
			if i == 0 {
				v |= bitBoundary
			}
			if i+ptrSize == size0 {
				v &^= uint8(bitPtrMask << 4)
			}

			off := (uintptr(x) + i - arena_start) / ptrSize
			xbits := (*uintptr)(unsafe.Pointer(arena_start - off/wordsPerBitmapWord*ptrSize - ptrSize))
			shift := (off % wordsPerBitmapWord) * gcBits
			xbitsb := (*uint8)(add(unsafe.Pointer(xbits), shift/8))
			*xbitsb = v
		}
		if size0%(2*ptrSize) == 0 && size0 < size {
			// Mark the word after last object's word as bitsDead.
			off := (uintptr(x) + size0 - arena_start) / ptrSize
			xbits := (*uintptr)(unsafe.Pointer(arena_start - off/wordsPerBitmapWord*ptrSize - ptrSize))
			shift := (off % wordsPerBitmapWord) * gcBits
			xbitsb := (*uint8)(add(unsafe.Pointer(xbits), shift/8))
			*xbitsb = bitsDead << 2
		}
	}
marked:
	mp.mallocing = 0

	if raceenabled {
		racemalloc(x, size)
	}
	if debug.allocfreetrace != 0 {
		tracealloc(x, size, typ)
	}

	if rate := MemProfileRate; rate > 0 {
		if size < uintptr(rate) && int32(size) < c.next_sample {
			c.next_sample -= int32(size)
		} else {
			profilealloc(mp, x, size)
		}
	}

	releasem(mp)

	if memstats.heap_alloc >= memstats.next_gc {
		gogc(0)
	}

	return x
}

// cmallocgc is a trampoline used to call the Go malloc from C.
func cmallocgc(size uintptr, typ *_type, flags int, ret *unsafe.Pointer) {
	*ret = gomallocgc(size, typ, flags)
}

// implementation of new builtin
func newobject(typ *_type) unsafe.Pointer {
	flags := 0
	if typ.kind&kindNoPointers != 0 {
		flags |= flagNoScan
	}
	return gomallocgc(uintptr(typ.size), typ, flags)
}

// implementation of make builtin for slices
func newarray(typ *_type, n uintptr) unsafe.Pointer {
	flags := 0
	if typ.kind&kindNoPointers != 0 {
		flags |= flagNoScan
	}
	if int(n) < 0 || (typ.size > 0 && n > maxMem/uintptr(typ.size)) {
		panic("runtime: allocation size out of range")
	}
	return gomallocgc(uintptr(typ.size)*n, typ, flags)
}

// rawmem returns a chunk of pointerless memory.  It is
// not zeroed.
func rawmem(size uintptr) unsafe.Pointer {
	return gomallocgc(size, nil, flagNoScan|flagNoZero)
}

// round size up to next size class
func goroundupsize(size uintptr) uintptr {
	if size < maxSmallSize {
		if size <= 1024-8 {
			return uintptr(class_to_size[size_to_class8[(size+7)>>3]])
		}
		return uintptr(class_to_size[size_to_class128[(size-1024+127)>>7]])
	}
	if size+pageSize < size {
		return size
	}
	return (size + pageSize - 1) &^ pageMask
}

func profilealloc(mp *m, x unsafe.Pointer, size uintptr) {
	c := mp.mcache
	rate := MemProfileRate
	if size < uintptr(rate) {
		// pick next profile time
		// If you change this, also change allocmcache.
		if rate > 0x3fffffff { // make 2*rate not overflow
			rate = 0x3fffffff
		}
		next := int32(fastrand2()) % (2 * int32(rate))
		// Subtract the "remainder" of the current allocation.
		// Otherwise objects that are close in size to sampling rate
		// will be under-sampled, because we consistently discard this remainder.
		next -= (int32(size) - c.next_sample)
		if next < 0 {
			next = 0
		}
		c.next_sample = next
	}
	mp.scalararg[0] = uint(size)
	mp.ptrarg[0] = x
	onM(&mprofMalloc_m)
}

// force = 1 - do GC regardless of current heap usage
// force = 2 - go GC and eager sweep
func gogc(force int32) {
	if memstats.enablegc == 0 {
		return
	}

	// TODO: should never happen?  Only C calls malloc while holding a lock?
	mp := acquirem()
	if mp.locks > 1 {
		releasem(mp)
		return
	}
	releasem(mp)

	if panicking != 0 {
		return
	}
	if gcpercent == gcpercentUnknown {
		golock(&mheap_.lock)
		if gcpercent == gcpercentUnknown {
			gcpercent = goreadgogc()
		}
		gounlock(&mheap_.lock)
	}
	if gcpercent < 0 {
		return
	}

	semacquire(&worldsema, false)

	if force == 0 && memstats.heap_alloc < memstats.next_gc {
		// typically threads which lost the race to grab
		// worldsema exit here when gc is done.
		semrelease(&worldsema)
		return
	}

	// Ok, we're doing it!  Stop everybody else
	startTime := gonanotime()
	mp = acquirem()
	mp.gcing = 1
	stoptheworld()

	clearpools()

	// Run gc on the g0 stack.  We do this so that the g stack
	// we're currently running on will no longer change.  Cuts
	// the root set down a bit (g0 stacks are not scanned, and
	// we don't need to scan gc's internal state).  We also
	// need to switch to g0 so we can shrink the stack.
	n := 1
	if debug.gctrace > 1 {
		n = 2
	}
	for i := 0; i < n; i++ {
		if i > 0 {
			startTime = gonanotime()
		}
		// switch to g0, call gc, then switch back
		mp.scalararg[0] = uint(startTime)
		if force >= 2 {
			mp.scalararg[1] = 1 // eagersweep
		} else {
			mp.scalararg[1] = 0
		}
		onM(&gc_m)
	}

	// all done
	mp.gcing = 0
	semrelease(&worldsema)
	starttheworld()
	releasem(mp)

	// now that gc is done, kick off finalizer thread if needed
	if !concurrentSweep {
		// give the queued finalizers, if any, a chance to run
		gosched()
	}
}

// GC runs a garbage collection.

// GC 运行一次垃圾回收。
func GC() {
	gogc(2)
}

// SetFinalizer sets the finalizer associated with x to f.
// When the garbage collector finds an unreachable block
// with an associated finalizer, it clears the association and runs
// f(x) in a separate goroutine.  This makes x reachable again, but
// now without an associated finalizer.  Assuming that SetFinalizer
// is not called again, the next time the garbage collector sees
// that x is unreachable, it will free x.
//
// SetFinalizer(x, nil) clears any finalizer associated with x.
//
// The argument x must be a pointer to an object allocated by
// calling new or by taking the address of a composite literal.
// The argument f must be a function that takes a single argument
// to which x's type can be assigned, and can have arbitrary ignored return
// values. If either of these is not true, SetFinalizer aborts the
// program.
//
// Finalizers are run in dependency order: if A points at B, both have
// finalizers, and they are otherwise unreachable, only the finalizer
// for A runs; once A is freed, the finalizer for B can run.
// If a cyclic structure includes a block with a finalizer, that
// cycle is not guaranteed to be garbage collected and the finalizer
// is not guaranteed to run, because there is no ordering that
// respects the dependencies.
//
// The finalizer for x is scheduled to run at some arbitrary time after
// x becomes unreachable.
// There is no guarantee that finalizers will run before a program exits,
// so typically they are useful only for releasing non-memory resources
// associated with an object during a long-running program.
// For example, an os.File object could use a finalizer to close the
// associated operating system file descriptor when a program discards
// an os.File without calling Close, but it would be a mistake
// to depend on a finalizer to flush an in-memory I/O buffer such as a
// bufio.Writer, because the buffer would not be flushed at program exit.
//
// It is not guaranteed that a finalizer will run if the size of *x is
// zero bytes.
//
// A single goroutine runs all finalizers for a program, sequentially.
// If a finalizer must run for a long time, it should do so by starting
// a new goroutine.

// SetFinalizer 为 f 设置与 x 相关联的终结器。
// 当垃圾回收器找到一个无法访问的块及与其相关联的终结器时，就会清理该关联，
// 并在一个独立的Go程中运行f(x)。这会使 x 再次变得可访问，但现在没有了相关联的终结器。
// 假设 SetFinalizer 未被再次调用，当下一次垃圾回收器发现 x 无法访问时，就会释放 x。
//
// SetFinalizer(x, nil) 会清理任何与 x 相关联的终结器。
//
// 实参 x 必须是一个对象的指针，该对象通过调用新的或获取一个复合字面地址来分配。
// 实参 f 必须是一个函数，该函数获取一个 x 的类型的单一实参，并拥有可任意忽略的返回值。
// 只要这些条件有一个不满足，SetFinalizer 就会跳过该程序。
//
// 终结器按照依赖顺序运行：若 A 指向 B，则二者都有终结器，当只有 A 的终结器运行时，
// 它们才无法访问；一旦 A 被释放，则 B 的终结器便可运行。若循环依赖的结构包含块及其终结器，
// 则该循环并不能保证被垃圾回收，而其终结器并不能保证运行，这是因为其依赖没有顺序。
//
// x 的终结器预定为在 x 无法访问后的任意时刻运行。无法保证终结器会在程序退出前运行，
// 因此它们通常只在长时间运行的程序中释放一个关联至对象的非内存资源时使用。
// 例如，当程序丢弃 os.File 而没有调用 Close 时，该 os.File 对象便可使用一个终结器
// 来关闭与其相关联的操作系统文件描述符，但依赖终结器去刷新一个内存中的I/O缓存是错误的，
// 因为该缓存不会在程序退出时被刷新。
//
// 一个程序的单个Go程会按顺序运行所有的终结器。若某个终结器需要长时间运行，
// 它应当通过开始一个新的Go程来继续。
// TODO: 仍需校对及语句优化
func SetFinalizer(obj interface{}, finalizer interface{}) {
	// We do just enough work here to make the mcall type safe.
	// The rest is done on the M stack.
	e := (*eface)(unsafe.Pointer(&obj))
	typ := e._type
	if typ == nil {
		gothrow("runtime.SetFinalizer: first argument is nil")
	}
	if typ.kind&kindMask != kindPtr {
		gothrow("runtime.SetFinalizer: first argument is " + *typ._string + ", not pointer")
	}

	f := (*eface)(unsafe.Pointer(&finalizer))
	ftyp := f._type
	if ftyp != nil && ftyp.kind&kindMask != kindFunc {
		gothrow("runtime.SetFinalizer: second argument is " + *ftyp._string + ", not a function")
	}
	mp := acquirem()
	mp.ptrarg[0] = unsafe.Pointer(typ)
	mp.ptrarg[1] = e.data
	mp.ptrarg[2] = unsafe.Pointer(ftyp)
	mp.ptrarg[3] = f.data
	onM(&setFinalizer_m)
	releasem(mp)
}
