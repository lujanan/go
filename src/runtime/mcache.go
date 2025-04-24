// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/runtime/atomic"
	"runtime/internal/sys"
	"unsafe"
)

// Per-thread (in Go, per-P) cache for small objects.
// This includes a small object cache and local allocation stats.
// No locking needed because it is per-thread (per-P).
//
// mcaches are allocated from non-GC'd memory, so any heap pointers
// must be specially handled.
// 每个线程（在 Go 中，每个 P）的小对象缓存。
// 这包括一个小对象缓存和本地分配统计信息。
// 不需要加锁，因为它是每个线程（每个 P）私有的。
//
// mcache 是从非 GC 内存中分配的，所以任何堆指针
// 都需要特殊处理。
type mcache struct {
	_ sys.NotInHeap

	// The following members are accessed on every malloc,
	// so they are grouped here for better caching.
	// 以下成员在每次 malloc 时都会被访问，
	// 所以它们被分组在这里以获得更好的缓存效果。
	nextSample uintptr // trigger heap sample after allocating this many bytes
	// 在分配这么多字节后触发堆采样
	scanAlloc uintptr // bytes of scannable heap allocated
	// 已分配的可扫描堆字节数

	// Allocator cache for tiny objects w/o pointers.
	// See "Tiny allocator" comment in malloc.go.
	// 无指针微小对象的分配器缓存。
	// 参见 malloc.go 中的 "Tiny allocator" 注释。

	// tiny points to the beginning of the current tiny block, or
	// nil if there is no current tiny block.
	//
	// tiny is a heap pointer. Since mcache is in non-GC'd memory,
	// we handle it by clearing it in releaseAll during mark
	// termination.
	//
	// tinyAllocs is the number of tiny allocations performed
	// by the P that owns this mcache.
	// tiny 指向当前微小块的开始，如果没有当前微小块则为 nil。
	//
	// tiny 是一个堆指针。由于 mcache 在非 GC 内存中，
	// 我们在标记终止期间通过 releaseAll 清除它来处理。
	//
	// tinyAllocs 是拥有此 mcache 的 P 执行的微小分配次数。
	tiny       uintptr
	tinyoffset uintptr
	tinyAllocs uintptr

	// The rest is not accessed on every malloc.
	// 其余部分不会在每次 malloc 时被访问。

	alloc [numSpanClasses]*mspan // spans to allocate from, indexed by spanClass
	// 用于分配的 spans，按 spanClass 索引

	stackcache [_NumStackOrders]stackfreelist
	// 栈缓存，用于存储不同大小的空闲栈

	// flushGen indicates the sweepgen during which this mcache
	// was last flushed. If flushGen != mheap_.sweepgen, the spans
	// in this mcache are stale and need to the flushed so they
	// can be swept. This is done in acquirep.
	// flushGen 表示此 mcache 最后一次刷新的 sweepgen。
	// 如果 flushGen != mheap_.sweepgen，此 mcache 中的 spans
	// 是过时的，需要刷新以便它们可以被清扫。
	// 这会在 acquirep 中完成。
	flushGen atomic.Uint32
}

// A gclink is a node in a linked list of blocks, like mlink,
// but it is opaque to the garbage collector.
// The GC does not trace the pointers during collection,
// and the compiler does not emit write barriers for assignments
// of gclinkptr values. Code should store references to gclinks
// as gclinkptr, not as *gclink.
// gclink 是链表中的一个节点，类似于 mlink，
// 但它对垃圾收集器是不透明的。
// GC 在收集过程中不会追踪这些指针，
// 编译器也不会为 gclinkptr 值的赋值生成写屏障。
// 代码应该将 gclink 的引用存储为 gclinkptr，而不是 *gclink。
type gclink struct {
	next gclinkptr
}

// A gclinkptr is a pointer to a gclink, but it is opaque
// to the garbage collector.
// gclinkptr 是指向 gclink 的指针，但它对垃圾收集器是不透明的。
type gclinkptr uintptr

// ptr returns the *gclink form of p.
// The result should be used for accessing fields, not stored
// in other data structures.
// ptr 返回 p 的 *gclink 形式。
// 结果应该用于访问字段，而不是存储在其他数据结构中。
func (p gclinkptr) ptr() *gclink {
	return (*gclink)(unsafe.Pointer(p))
}

type stackfreelist struct {
	list gclinkptr // linked list of free stacks
	size uintptr   // total size of stacks in list
	// list 是空闲栈的链表
	// size 是链表中栈的总大小
}

// dummy mspan that contains no free objects.
// 不包含空闲对象的虚拟 mspan。
var emptymspan mspan

// allocmcache allocates a new mcache.
// allocmcache 分配一个新的 mcache。
func allocmcache() *mcache {
	var c *mcache
	// Execute on the system stack to avoid stack growth.
	// 在系统栈上执行以避免栈增长。
	systemstack(func() {
		lock(&mheap_.lock)
		// Allocate memory for the mcache from the cache allocator.
		// 从缓存分配器中为 mcache 分配内存。
		c = (*mcache)(mheap_.cachealloc.alloc())
		// Initialize the flush generation number to the current sweep generation.
		// 将刷新代数值初始化为当前的清扫代数值。
		c.flushGen.Store(mheap_.sweepgen)
		unlock(&mheap_.lock)
	})
	// Initialize all alloc spans to empty spans.
	// 将所有分配 span 初始化为空 span。
	for i := range c.alloc {
		c.alloc[i] = &emptymspan
	}
	// Set the next sampling point for memory profiling.
	// 设置内存分析的下一个采样点。
	c.nextSample = nextSample()
	return c
}

// freemcache releases resources associated with this
// mcache and puts the object onto a free list.
//
// In some cases there is no way to simply release
// resources, such as statistics, so donate them to
// a different mcache (the recipient).
// freemcache 释放与此 mcache 关联的资源并将对象放入空闲列表。
//
// 在某些情况下，无法简单地释放资源（如统计信息），
// 因此将它们捐赠给另一个 mcache（接收者）。
func freemcache(c *mcache) {
	systemstack(func() {
		// 释放 mcache 中的所有 span
		c.releaseAll()
		// 清除栈缓存
		stackcache_clear(c)

		// NOTE(rsc,rlh): If gcworkbuffree comes back, we need to coordinate
		// with the stealing of gcworkbufs during garbage collection to avoid
		// a race where the workbuf is double-freed.
		// gcworkbuffree(c.gcworkbuf)
		// 注意(rsc,rlh)：如果 gcworkbuffree 恢复使用，我们需要与垃圾收集期间
		// 的 gcworkbufs 窃取操作协调，以避免 workbuf 被双重释放的竞态条件。
		// gcworkbuffree(c.gcworkbuf)

		// 获取堆锁，将 mcache 对象归还给缓存分配器
		lock(&mheap_.lock)
		mheap_.cachealloc.free(unsafe.Pointer(c))
		unlock(&mheap_.lock)
	})
}

// getMCache is a convenience function which tries to obtain an mcache.
//
// Returns nil if we're not bootstrapping or we don't have a P. The caller's
// P must not change, so we must be in a non-preemptible state.
// getMCache 是一个便捷函数，用于获取 mcache。
//
// 如果不在引导阶段或没有 P，则返回 nil。调用者的 P 不能改变，
// 因此我们必须在不可抢占的状态下。
func getMCache(mp *m) *mcache {
	// Grab the mcache, since that's where stats live.
	// 获取 mcache，因为统计信息存储在那里。
	pp := mp.p.ptr()
	var c *mcache
	if pp == nil {
		// We will be called without a P while bootstrapping,
		// in which case we use mcache0, which is set in mallocinit.
		// mcache0 is cleared when bootstrapping is complete,
		// by procresize.
		// 在引导阶段，我们可能会在没有 P 的情况下被调用，
		// 此时我们使用 mcache0，它在 mallocinit 中设置。
		// mcache0 在引导完成时由 procresize 清除。
		c = mcache0
	} else {
		// 如果有 P，则使用 P 关联的 mcache
		c = pp.mcache
	}
	return c
}

// refill acquires a new span of span class spc for c. This span will
// have at least one free object. The current span in c must be full.
//
// Must run in a non-preemptible context since otherwise the owner of
// c could change.
// refill 为 c 获取一个新的 span class 为 spc 的 span。这个 span 将
// 至少有一个空闲对象。c 中当前的 span 必须是满的。
//
// 必须在不可抢占的上下文中运行，否则 c 的所有者可能会改变。
func (c *mcache) refill(spc spanClass) {
	// Return the current cached span to the central lists.
	// 将当前缓存的 span 返回到中心列表
	s := c.alloc[spc]

	if s.allocCount != s.nelems {
		throw("refill of span with free space remaining")
	}
	if s != &emptymspan {
		// Mark this span as no longer cached.
		// 标记这个 span 不再被缓存
		if s.sweepgen != mheap_.sweepgen+3 {
			throw("bad sweepgen in refill")
		}
		mheap_.central[spc].mcentral.uncacheSpan(s)

		// Count up how many slots were used and record it.
		// 计算使用了多少个槽位并记录
		stats := memstats.heapStats.acquire()
		slotsUsed := int64(s.allocCount) - int64(s.allocCountBeforeCache)
		atomic.Xadd64(&stats.smallAllocCount[spc.sizeclass()], slotsUsed)

		// Flush tinyAllocs.
		// 刷新微小分配计数
		if spc == tinySpanClass {
			atomic.Xadd64(&stats.tinyAllocCount, int64(c.tinyAllocs))
			c.tinyAllocs = 0
		}
		memstats.heapStats.release()

		// Count the allocs in inconsistent, internal stats.
		// 在不一致、内部的统计中计算分配
		bytesAllocated := slotsUsed * int64(s.elemsize)
		gcController.totalAlloc.Add(bytesAllocated)

		// Clear the second allocCount just to be safe.
		// 为了安全起见，清除第二个 allocCount
		s.allocCountBeforeCache = 0
	}

	// Get a new cached span from the central lists.
	// 从中心列表获取一个新的缓存 span
	s = mheap_.central[spc].mcentral.cacheSpan()
	if s == nil {
		throw("out of memory")
	}

	if s.allocCount == s.nelems {
		throw("span has no free space")
	}

	// Indicate that this span is cached and prevent asynchronous
	// sweeping in the next sweep phase.
	// 标记这个 span 已被缓存，并防止在下一个清扫阶段进行异步清扫
	s.sweepgen = mheap_.sweepgen + 3

	// Store the current alloc count for accounting later.
	// 存储当前的分配计数，以便后续统计
	s.allocCountBeforeCache = s.allocCount

	// Update heapLive and flush scanAlloc.
	//
	// We have not yet allocated anything new into the span, but we
	// assume that all of its slots will get used, so this makes
	// heapLive an overestimate.
	//
	// When the span gets uncached, we'll fix up this overestimate
	// if necessary (see releaseAll).
	//
	// We pick an overestimate here because an underestimate leads
	// the pacer to believe that it's in better shape than it is,
	// which appears to lead to more memory used. See #53738 for
	// more details.
	// 更新 heapLive 并刷新 scanAlloc
	//
	// 我们还没有在 span 中分配任何新对象，但我们
	// 假设它的所有槽位都会被使用，所以这会使
	// heapLive 被高估。
	//
	// 当 span 被取消缓存时，如果需要，我们会修正这个
	// 高估值（参见 releaseAll）。
	//
	// 我们在这里选择高估是因为低估会导致
	// pacer 认为它的状态比实际更好，
	// 这似乎会导致使用更多内存。详见 #53738。
	usedBytes := uintptr(s.allocCount) * s.elemsize
	gcController.update(int64(s.npages*pageSize)-int64(usedBytes), int64(c.scanAlloc))
	c.scanAlloc = 0

	c.alloc[spc] = s
}

// allocLarge allocates a span for a large object.
// allocLarge 为大对象分配一个 span
func (c *mcache) allocLarge(size uintptr, noscan bool) *mspan {
	// Check for overflow.
	// 检查是否溢出
	if size+_PageSize < size {
		throw("out of memory")
	}
	// Calculate number of pages needed.
	// 计算需要的页数
	npages := size >> _PageShift
	if size&_PageMask != 0 {
		npages++
	}

	// Deduct credit for this span allocation and sweep if
	// necessary. mHeap_Alloc will also sweep npages, so this only
	// pays the debt down to npage pages.
	// 扣除此次 span 分配的信用，必要时进行清扫。
	// mHeap_Alloc 也会清扫 npages，所以这里只将债务减少到 npage 页。
	deductSweepCredit(npages*_PageSize, npages)

	// Create span class and allocate span.
	// 创建 span 类别并分配 span
	spc := makeSpanClass(0, noscan)
	s := mheap_.alloc(npages, spc)
	if s == nil {
		throw("out of memory")
	}

	// Count the alloc in consistent, external stats.
	// 在一致的外部统计中记录分配
	stats := memstats.heapStats.acquire()
	atomic.Xadd64(&stats.largeAlloc, int64(npages*pageSize))
	atomic.Xadd64(&stats.largeAllocCount, 1)
	memstats.heapStats.release()

	// Count the alloc in inconsistent, internal stats.
	// 在不一致的内部统计中记录分配
	gcController.totalAlloc.Add(int64(npages * pageSize))

	// Update heapLive.
	// 更新 heapLive
	gcController.update(int64(s.npages*pageSize), 0)

	// Put the large span in the mcentral swept list so that it's
	// visible to the background sweeper.
	// 将大 span 放入 mcentral 的已清扫列表中，使其对后台清扫器可见
	mheap_.central[spc].mcentral.fullSwept(mheap_.sweepgen).push(s)
	s.limit = s.base() + size
	s.initHeapBits(false)
	return s
}

func (c *mcache) releaseAll() {
	// Take this opportunity to flush scanAlloc.
	// 利用这个机会刷新 scanAlloc
	scanAlloc := int64(c.scanAlloc)
	c.scanAlloc = 0

	sg := mheap_.sweepgen
	dHeapLive := int64(0)
	for i := range c.alloc {
		s := c.alloc[i]
		if s != &emptymspan {
			// 计算已使用的槽位数
			slotsUsed := int64(s.allocCount) - int64(s.allocCountBeforeCache)
			s.allocCountBeforeCache = 0

			// Adjust smallAllocCount for whatever was allocated.
			// 调整已分配对象的小对象计数
			stats := memstats.heapStats.acquire()
			atomic.Xadd64(&stats.smallAllocCount[spanClass(i).sizeclass()], slotsUsed)
			memstats.heapStats.release()

			// Adjust the actual allocs in inconsistent, internal stats.
			// We assumed earlier that the full span gets allocated.
			// 调整不一致的内部统计中的实际分配
			// 我们之前假设整个 span 都被分配了
			gcController.totalAlloc.Add(slotsUsed * int64(s.elemsize))

			if s.sweepgen != sg+1 {
				// refill conservatively counted unallocated slots in gcController.heapLive.
				// Undo this.
				//
				// If this span was cached before sweep, then gcController.heapLive was totally
				// recomputed since caching this span, so we don't do this for stale spans.
				// refill 保守地计算了 gcController.heapLive 中未分配的槽位
				// 撤销这个计算
				//
				// 如果这个 span 在清扫前被缓存，那么 gcController.heapLive 从缓存这个 span 开始
				// 就被完全重新计算了，所以我们不对过时的 span 执行这个操作
				dHeapLive -= int64(s.nelems-s.allocCount) * int64(s.elemsize)
			}

			// Release the span to the mcentral.
			// 将 span 释放回 mcentral
			mheap_.central[i].mcentral.uncacheSpan(s)
			c.alloc[i] = &emptymspan
		}
	}
	// Clear tinyalloc pool.
	// 清除微小分配池
	c.tiny = 0
	c.tinyoffset = 0

	// Flush tinyAllocs.
	// 刷新微小分配计数
	stats := memstats.heapStats.acquire()
	atomic.Xadd64(&stats.tinyAllocCount, int64(c.tinyAllocs))
	c.tinyAllocs = 0
	memstats.heapStats.release()

	// Update heapLive and heapScan.
	// 更新 heapLive 和 heapScan
	gcController.update(dHeapLive, scanAlloc)
}

// prepareForSweep flushes c if the system has entered a new sweep phase
// since c was populated. This must happen between the sweep phase
// starting and the first allocation from c.
// prepareForSweep 在系统进入新的清扫阶段时刷新 mcache c。
// 这必须在清扫阶段开始和从 c 进行第一次分配之间发生。
func (c *mcache) prepareForSweep() {
	// Alternatively, instead of making sure we do this on every P
	// between starting the world and allocating on that P, we
	// could leave allocate-black on, allow allocation to continue
	// as usual, use a ragged barrier at the beginning of sweep to
	// ensure all cached spans are swept, and then disable
	// allocate-black. However, with this approach it's difficult
	// to avoid spilling mark bits into the *next* GC cycle.
	// 另一种方案是，我们可以在启动世界和在该 P 上分配之间，
	// 保持 allocate-black 开启，允许分配继续正常进行，
	// 在清扫开始时使用不规则的屏障来确保所有缓存的 spans 都被清扫，
	// 然后禁用 allocate-black。然而，使用这种方法很难避免将标记位溢出到下一个 GC 周期。
	sg := mheap_.sweepgen
	flushGen := c.flushGen.Load()
	if flushGen == sg {
		return
	} else if flushGen != sg-2 {
		println("bad flushGen", flushGen, "in prepareForSweep; sweepgen", sg)
		throw("bad flushGen")
	}
	c.releaseAll()
	stackcache_clear(c)
	c.flushGen.Store(mheap_.sweepgen) // Synchronizes with gcStart
	// 与 gcStart 同步
}
