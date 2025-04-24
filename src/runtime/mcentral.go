// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Central free lists.
//
// See malloc.go for an overview.
//
// The mcentral doesn't actually contain the list of free objects; the mspan does.
// Each mcentral is two lists of mspans: those with free objects (c->nonempty)
// and those that are completely allocated (c->empty).

// 中心空闲列表。
//
// 参见 malloc.go 中的概述。
//
// mcentral 实际上并不包含空闲对象的列表；这个列表是由 mspan 维护的。
// 每个 mcentral 包含两个 mspan 列表：一个包含有空闲对象的 mspan（c->nonempty）
// 和另一个包含已完全分配的 mspan（c->empty）。

package runtime

import (
	"internal/runtime/atomic"
	"runtime/internal/sys"
)

// Central list of free objects of a given size.
// 特定大小的对象的中心空闲列表。
type mcentral struct {
	_         sys.NotInHeap
	spanclass spanClass

	// partial and full contain two mspan sets: one of swept in-use
	// spans, and one of unswept in-use spans. These two trade
	// roles on each GC cycle. The unswept set is drained either by
	// allocation or by the background sweeper in every GC cycle,
	// so only two roles are necessary.
	//
	// sweepgen is increased by 2 on each GC cycle, so the swept
	// spans are in partial[sweepgen/2%2] and the unswept spans are in
	// partial[1-sweepgen/2%2]. Sweeping pops spans from the
	// unswept set and pushes spans that are still in-use on the
	// swept set. Likewise, allocating an in-use span pushes it
	// on the swept set.
	//
	// Some parts of the sweeper can sweep arbitrary spans, and hence
	// can't remove them from the unswept set, but will add the span
	// to the appropriate swept list. As a result, the parts of the
	// sweeper and mcentral that do consume from the unswept list may
	// encounter swept spans, and these should be ignored.
	// partial 和 full 包含两个 mspan 集合：一个是已清扫的使用中的 spans，
	// 另一个是未清扫的使用中的 spans。这两个集合在每个 GC 周期中交换角色。
	// 未清扫的集合在每个 GC 周期中要么通过分配，要么通过后台清扫器被清空，
	// 所以只需要两个角色。
	//
	// sweepgen 在每个 GC 周期增加 2，所以已清扫的 spans 在 partial[sweepgen/2%2] 中，
	// 未清扫的 spans 在 partial[1-sweepgen/2%2] 中。清扫操作从未清扫集合中弹出 spans，
	// 并将仍在使用的 spans 推入已清扫集合。同样，分配一个使用中的 span 会将其推入已清扫集合。
	//
	// 清扫器的某些部分可以清扫任意 spans，因此不能将它们从未清扫集合中移除，
	// 但会将 span 添加到适当的已清扫列表中。因此，从未清扫列表中消费的清扫器和 mcentral 部分
	// 可能会遇到已清扫的 spans，这些应该被忽略。
	partial [2]spanSet // list of spans with a free object
	// 包含空闲对象的 spans 列表
	full [2]spanSet // list of spans with no free objects
	// 不包含空闲对象的 spans 列表
}

// Initialize a single central free list.
// 初始化一个中心空闲列表
func (c *mcentral) init(spc spanClass) {
	c.spanclass = spc
	// 初始化 partial 和 full 数组中的 spineLock 锁
	// 这些锁用于保护 spanSet 的 spine 数组的并发访问
	lockInit(&c.partial[0].spineLock, lockRankSpanSetSpine)
	lockInit(&c.partial[1].spineLock, lockRankSpanSetSpine)
	lockInit(&c.full[0].spineLock, lockRankSpanSetSpine)
	lockInit(&c.full[1].spineLock, lockRankSpanSetSpine)
}

// partialUnswept returns the spanSet which holds partially-filled
// unswept spans for this sweepgen.
// partialUnswept 返回当前 sweepgen 对应的未清扫的部分填充的 spanSet
// 由于 sweepgen 每轮 GC 增加 2，所以使用 1-sweepgen/2%2 来获取正确的索引
func (c *mcentral) partialUnswept(sweepgen uint32) *spanSet {
	return &c.partial[1-sweepgen/2%2]
}

// partialSwept returns the spanSet which holds partially-filled
// swept spans for this sweepgen.
// partialSwept 返回当前 sweepgen 对应的已清扫的部分填充的 spanSet
// 使用 sweepgen/2%2 来获取正确的索引
func (c *mcentral) partialSwept(sweepgen uint32) *spanSet {
	return &c.partial[sweepgen/2%2]
}

// fullUnswept returns the spanSet which holds unswept spans without any
// free slots for this sweepgen.
// fullUnswept 返回当前 sweepgen 对应的未清扫的已满的 spanSet
// 同样使用 1-sweepgen/2%2 来获取正确的索引
func (c *mcentral) fullUnswept(sweepgen uint32) *spanSet {
	return &c.full[1-sweepgen/2%2]
}

// fullSwept returns the spanSet which holds swept spans without any
// free slots for this sweepgen.
// fullSwept 返回当前 sweepgen 对应的已清扫的已满的 spanSet
// 使用 sweepgen/2%2 来获取正确的索引，因为 sweepgen 每轮 GC 增加 2
// 这样可以在两个 spanSet 之间交替使用，一个用于当前代，一个用于下一代
func (c *mcentral) fullSwept(sweepgen uint32) *spanSet {
	return &c.full[sweepgen/2%2]
}

// Allocate a span to use in an mcache.
// 为 mcache 分配一个 span
func (c *mcentral) cacheSpan() *mspan {
	// Deduct credit for this span allocation and sweep if necessary.
	// 扣除这个 span 分配的信用，并在必要时进行清扫
	spanBytes := uintptr(class_to_allocnpages[c.spanclass.sizeclass()]) * _PageSize
	deductSweepCredit(spanBytes, 0)

	traceDone := false
	trace := traceAcquire()
	if trace.ok() {
		trace.GCSweepStart()
		traceRelease(trace)
	}

	// If we sweep spanBudget spans without finding any free
	// space, just allocate a fresh span. This limits the amount
	// of time we can spend trying to find free space and
	// amortizes the cost of small object sweeping over the
	// benefit of having a full free span to allocate from. By
	// setting this to 100, we limit the space overhead to 1%.
	//
	// TODO(austin,mknyszek): This still has bad worst-case
	// throughput. For example, this could find just one free slot
	// on the 100th swept span. That limits allocation latency, but
	// still has very poor throughput. We could instead keep a
	// running free-to-used budget and switch to fresh span
	// allocation if the budget runs low.
	// 如果我们清扫了 spanBudget 个 span 后仍然没有找到任何空闲空间，
	// 就直接分配一个新的 span。这限制了我们尝试寻找空闲空间的时间，
	// 并将小对象清扫的成本分摊到拥有一个完全空闲的 span 来分配的好处上。
	// 通过将其设置为 100，我们将空间开销限制在 1%。
	//
	// TODO(austin,mknyszek): 这仍然有很糟糕的最坏情况吞吐量。
	// 例如，这可能在第 100 个已清扫的 span 上只找到一个空闲槽位。
	// 这限制了分配延迟，但仍然有非常差的吞吐量。我们可以改为维护一个
	// 运行中的空闲到已使用预算，如果预算变低就切换到新的 span 分配。
	spanBudget := 100

	var s *mspan
	var sl sweepLocker

	// Try partial swept spans first.
	// 首先尝试部分已清扫的 spans
	sg := mheap_.sweepgen
	if s = c.partialSwept(sg).pop(); s != nil {
		goto havespan
	}
	sl = sweep.active.begin()
	if sl.valid {
		// Now try partial unswept spans.
		// 现在尝试部分未清扫的 spans
		for ; spanBudget >= 0; spanBudget-- {
			s = c.partialUnswept(sg).pop()
			if s == nil {
				break
			}
			if s, ok := sl.tryAcquire(s); ok {
				// We got ownership of the span, so let's sweep it and use it.
				// 我们获得了 span 的所有权，所以让我们清扫它并使用它
				s.sweep(true)
				sweep.active.end(sl)
				goto havespan
			}
			// We failed to get ownership of the span, which means it's being or
			// has been swept by an asynchronous sweeper that just couldn't remove it
			// from the unswept list. That sweeper took ownership of the span and
			// responsibility for either freeing it to the heap or putting it on the
			// right swept list. Either way, we should just ignore it (and it's unsafe
			// for us to do anything else).
			// 我们未能获得 span 的所有权，这意味着它正在被或已经被异步清扫器清扫，
			// 但该清扫器无法将其从未清扫列表中移除。该清扫器获得了 span 的所有权，
			// 并负责将其释放到堆中或放入正确的已清扫列表。无论哪种情况，
			// 我们都应该忽略它（对我们来说做其他任何事情都是不安全的）。
		}
		// Now try full unswept spans, sweeping them and putting them into the
		// right list if we fail to get a span.
		// 现在尝试完全未清扫的 spans，如果无法获取 span，则清扫它们并将其放入正确的列表
		for ; spanBudget >= 0; spanBudget-- {
			s = c.fullUnswept(sg).pop()
			if s == nil {
				break
			}
			if s, ok := sl.tryAcquire(s); ok {
				// We got ownership of the span, so let's sweep it.
				// 我们获得了 span 的所有权，所以让我们清扫它
				s.sweep(true)
				// Check if there's any free space.
				// 检查是否有任何空闲空间
				freeIndex := s.nextFreeIndex()
				if freeIndex != s.nelems {
					s.freeindex = freeIndex
					sweep.active.end(sl)
					goto havespan
				}
				// Add it to the swept list, because sweeping didn't give us any free space.
				// 将其添加到已清扫列表，因为清扫没有给我们任何空闲空间
				c.fullSwept(sg).push(s.mspan)
			}
			// See comment for partial unswept spans.
			// 参见部分未清扫 spans 的注释
		}
		sweep.active.end(sl)
	}
	trace = traceAcquire()
	if trace.ok() {
		trace.GCSweepDone()
		traceDone = true
		traceRelease(trace)
	}

	// We failed to get a span from the mcentral so get one from mheap.
	// 我们无法从 mcentral 获取 span，所以从 mheap 获取一个
	s = c.grow()
	if s == nil {
		return nil
	}

	// At this point s is a span that should have free slots.
	// 此时 s 是一个应该有空闲槽位的 span
havespan:
	if !traceDone {
		trace := traceAcquire()
		if trace.ok() {
			trace.GCSweepDone()
			traceRelease(trace)
		}
	}
	// 计算 span 中空闲对象的数量
	n := int(s.nelems) - int(s.allocCount)
	// 检查 span 是否真的有空闲对象
	if n == 0 || s.freeindex == s.nelems || s.allocCount == s.nelems {
		throw("span has no free objects")
	}
	// 计算空闲位图的基础字节位置
	// 将 freeindex 向下舍入到 64 的倍数
	freeByteBase := s.freeindex &^ (64 - 1)
	// 计算对应的字节索引
	whichByte := freeByteBase / 8
	// Init alloc bits cache.
	// 初始化分配位图缓存
	s.refillAllocCache(whichByte)

	// Adjust the allocCache so that s.freeindex corresponds to the low bit in
	// s.allocCache.
	// 调整 allocCache 使 s.freeindex 对应到 s.allocCache 的低位
	s.allocCache >>= s.freeindex % 64

	return s
}

// Return span from an mcache.
//
// s must have a span class corresponding to this
// mcentral and it must not be empty.
// 从 mcache 返回 span。
//
// s 必须具有与此 mcentral 对应的 span class，
// 并且它不能为空。
func (c *mcentral) uncacheSpan(s *mspan) {
	if s.allocCount == 0 {
		throw("uncaching span but s.allocCount == 0")
	}

	sg := mheap_.sweepgen
	stale := s.sweepgen == sg+1

	// Fix up sweepgen.
	// 修复 sweepgen
	if stale {
		// Span was cached before sweep began. It's our
		// responsibility to sweep it.
		//
		// Set sweepgen to indicate it's not cached but needs
		// sweeping and can't be allocated from. sweep will
		// set s.sweepgen to indicate s is swept.
		// span 在清扫开始前被缓存。我们有责任清扫它。
		//
		// 设置 sweepgen 表示它未被缓存但需要清扫，
		// 且不能从中分配。sweep 将设置 s.sweepgen
		// 表示 s 已被清扫。
		atomic.Store(&s.sweepgen, sg-1)
	} else {
		// Indicate that s is no longer cached.
		// 表示 s 不再被缓存
		atomic.Store(&s.sweepgen, sg)
	}

	// Put the span in the appropriate place.
	// 将 span 放到适当的位置
	if stale {
		// It's stale, so just sweep it. Sweeping will put it on
		// the right list.
		//
		// We don't use a sweepLocker here. Stale cached spans
		// aren't in the global sweep lists, so mark termination
		// itself holds up sweep completion until all mcaches
		// have been swept.
		// 它是过时的，所以直接清扫它。清扫会将它放到正确的列表中。
		//
		// 这里我们不使用 sweepLocker。过时的缓存 spans
		// 不在全局清扫列表中，所以标记终止本身会阻止
		// 清扫完成，直到所有 mcaches 都被清扫。
		ss := sweepLocked{s}
		ss.sweep(false)
	} else {
		if int(s.nelems)-int(s.allocCount) > 0 {
			// Put it back on the partial swept list.
			// 将它放回部分已清扫列表
			c.partialSwept(sg).push(s)
		} else {
			// There's no free space and it's not stale, so put it on the
			// full swept list.
			// 没有空闲空间且不是过时的，所以将它放到
			// 完全已清扫列表中
			c.fullSwept(sg).push(s)
		}
	}
}

// grow allocates a new empty span from the heap and initializes it for c's size class.
// grow 从堆中分配一个新的空 span，并为其 size class 进行初始化。
func (c *mcentral) grow() *mspan {
	// 获取该 size class 需要分配的页数
	npages := uintptr(class_to_allocnpages[c.spanclass.sizeclass()])
	// 获取该 size class 的对象大小
	size := uintptr(class_to_size[c.spanclass.sizeclass()])

	// 从堆中分配指定页数的 span
	s := mheap_.alloc(npages, c.spanclass)
	if s == nil {
		return nil
	}

	// Use division by multiplication and shifts to quickly compute:
	// n := (npages << _PageShift) / size
	// 使用乘法和位移快速计算：
	// n := (npages << _PageShift) / size
	// 计算 span 中可以容纳的对象数量
	n := s.divideByElemSize(npages << _PageShift)
	// 设置 span 的 limit 为 base 加上所有对象的总大小
	s.limit = s.base() + size*n
	// 初始化堆位图，false 表示不需要清零
	s.initHeapBits(false)
	return s
}
