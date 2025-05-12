// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Garbage collector: sweeping

// The sweeper consists of two different algorithms:
//
// * The object reclaimer finds and frees unmarked slots in spans. It
//   can free a whole span if none of the objects are marked, but that
//   isn't its goal. This can be driven either synchronously by
//   mcentral.cacheSpan for mcentral spans, or asynchronously by
//   sweepone, which looks at all the mcentral lists.
//
// * The span reclaimer looks for spans that contain no marked objects
//   and frees whole spans. This is a separate algorithm because
//   freeing whole spans is the hardest task for the object reclaimer,
//   but is critical when allocating new spans. The entry point for
//   this is mheap_.reclaim and it's driven by a sequential scan of
//   the page marks bitmap in the heap arenas.
//
// Both algorithms ultimately call mspan.sweep, which sweeps a single
// heap span.

// 垃圾收集器：清扫阶段

// 清扫器由两种不同的算法组成：
//
// * 对象回收器(object reclaimer)负责查找和释放span中未被标记的槽位。
//   虽然它可以释放整个span(当span中没有任何对象被标记时)，但这并不是它的主要目标。
//   这个算法可以通过两种方式触发：
//   1. 同步方式：通过mcentral.cacheSpan处理mcentral spans
//   2. 异步方式：通过sweepone扫描所有的mcentral列表
//
// * span回收器(span reclaimer)专门查找并释放那些不包含任何标记对象的span。
//   这是一个独立的算法，因为释放整个span是对象回收器最困难的任务，
//   但在分配新span时这又是非常关键的。
//   这个算法的入口点是mheap_.reclaim，它通过顺序扫描堆内存区域中的页面标记位图来工作。
//
// 这两种算法最终都会调用mspan.sweep，用于清扫单个堆span。

package runtime

import (
	"internal/abi"
	"internal/runtime/atomic"
	"unsafe"
)

var sweep sweepdata

// State of background sweep.
// 后台清扫的状态
type sweepdata struct {
	// Mutex to protect sweep data.
	// 保护清扫数据的互斥锁
	lock mutex
	// Dedicated G for sweeping.
	// 专用于清扫的G
	g *g
	// Background sweeper is parked.
	// 后台清扫器是否已停止
	parked bool

	// active tracks outstanding sweepers and the sweep
	// termination condition.
	// active 跟踪未完成的清扫器和清扫终止条件
	active activeSweep

	// centralIndex is the current unswept span class.
	// It represents an index into the mcentral span
	// sets. Accessed and updated via its load and
	// update methods. Not protected by a lock.
	// centralIndex 是当前未清扫的 span class。
	// 它表示 mcentral span 集合的索引。
	// 通过其 load 和 update 方法访问和更新。
	// 不受锁保护。
	//
	// Reset at mark termination.
	// 在标记终止时重置。
	// Used by mheap.nextSpanForSweep.
	// 由 mheap.nextSpanForSweep 使用。
	centralIndex sweepClass
}

// sweepClass is a spanClass and one bit to represent whether we're currently
// sweeping partial or full spans.
// sweepClass 是一个 spanClass，并用一位来表示我们当前是否正在
// 清扫部分或完整的 span。
type sweepClass uint32

const (
	// numSweepClasses is the number of sweep classes.
	// numSweepClasses 是清扫类的数量。
	numSweepClasses = numSpanClasses * 2
	// sweepClassDone indicates that all span classes have been swept.
	// sweepClassDone 表示所有 span 类都已清扫完毕。
	sweepClassDone sweepClass = sweepClass(^uint32(0))
)

func (s *sweepClass) load() sweepClass {
	return sweepClass(atomic.Load((*uint32)(s)))
}

func (s *sweepClass) update(sNew sweepClass) {
	// Only update *s if its current value is less than sNew,
	// since *s increases monotonically.
	// 只有当 *s 的当前值小于 sNew 时才更新 *s，
	// 因为 *s 单调递增。
	sOld := s.load()
	for sOld < sNew && !atomic.Cas((*uint32)(s), uint32(sOld), uint32(sNew)) {
		// Atomically compare and swap *s from sOld to sNew if *s == sOld.
		// If the swap fails (another goroutine updated *s first), retry.
		// 原子地比较并交换 *s 从 sOld 到 sNew，如果 *s == sOld。
		// 如果交换失败（另一个 goroutine 率先更新了 *s），则重试。
		sOld = s.load()
	}
	// TODO(mknyszek): This isn't the only place we have
	// an atomic monotonically increasing counter. It would
	// be nice to have an "atomic max" which is just implemented
	// as the above on most architectures. Some architectures
	// like RISC-V however have native support for an atomic max.
	// TODO(mknyszek): 这不是我们拥有原子单调递增计数器的唯一地方。
	// 如果有一个“原子最大值”就好了，它只是在大多数架构上实现为上述内容。
	// 然而，像 RISC-V 这样的一些架构原生支持原子最大值。
}

func (s *sweepClass) clear() {
	atomic.Store((*uint32)(s), 0)
}

// split returns the underlying span class as well as
// whether we're interested in the full or partial
// unswept lists for that class, indicated as a boolean
// (true means "full").
// split 返回底层的 span 类，以及我们是否对该类的完整或部分未清扫列表感兴趣，
// 用布尔值表示（true 表示“完整”）。
func (s sweepClass) split() (spc spanClass, full bool) {
	// 将 sweepClass 分解为 spanClass 和一个布尔值，
	// 该布尔值指示我们是否对完整的未清扫列表感兴趣。
	// sweepClass 的最低有效位用于指示完整/部分。
	// 其余位用于 spanClass。
	return spanClass(s >> 1), s&1 == 0
}

// nextSpanForSweep finds and pops the next span for sweeping from the
// central sweep buffers. It returns ownership of the span to the caller.
// Returns nil if no such span exists.
// nextSpanForSweep 从中央清扫缓冲区查找并弹出下一个要清扫的 span。
// 它将 span 的所有权返回给调用者。如果不存在这样的 span，则返回 nil。
func (h *mheap) nextSpanForSweep() *mspan {
	sg := h.sweepgen                                                  // 获取当前的 sweepgen 值
	for sc := sweep.centralIndex.load(); sc < numSweepClasses; sc++ { // 遍历所有 span class
		spc, full := sc.split()       // 将 sweepClass 分解为 spanClass 和一个布尔值，该布尔值指示我们是否对完整的未清扫列表感兴趣。
		c := &h.central[spc].mcentral // 获取对应 spanClass 的 mcentral
		var s *mspan
		if full { // 如果需要从 fullUnswept 列表中获取 span
			s = c.fullUnswept(sg).pop() // 从 fullUnswept 列表中弹出一个 span
		} else { // 否则从 partialUnswept 列表中获取 span
			s = c.partialUnswept(sg).pop() // 从 partialUnswept 列表中弹出一个 span
		}
		if s != nil { // 如果找到了 span
			// Write down that we found something so future sweepers
			// can start from here.
			// 记录我们找到了一些东西，以便将来的清扫器可以从这里开始。
			sweep.centralIndex.update(sc) // 更新 centralIndex，以便下次从这里开始
			return s                      // 返回找到的 span
		}
	}
	// Write down that we found nothing.
	// 记录我们没有找到任何东西。
	sweep.centralIndex.update(sweepClassDone) // 更新 centralIndex 为 sweepClassDone，表示所有 span class 都已清扫完毕。
	return nil                                // 返回 nil
}

const sweepDrainedMask = 1 << 31

// activeSweep is a type that captures whether sweeping
// is done, and whether there are any outstanding sweepers.
//
// Every potential sweeper must call begin() before they look
// for work, and end() after they've finished sweeping.
// activeSweep 是一个类型，用于捕获清扫是否完成，以及是否存在任何未完成的清扫器。
// 每个潜在的清扫器在查找工作之前必须调用 begin()，并在完成清扫后调用 end()。
type activeSweep struct {
	// state is divided into two parts.
	//
	// The top bit (masked by sweepDrainedMask) is a boolean
	// value indicating whether all the sweep work has been
	// drained from the queue.
	//
	// The rest of the bits are a counter, indicating the
	// number of outstanding concurrent sweepers.
	// state 分为两部分。
	// 最高位（由 sweepDrainedMask 屏蔽）是一个布尔值，指示是否已从队列中耗尽所有清扫工作。
	// 其余位是一个计数器，指示未完成的并发清扫器的数量。
	state atomic.Uint32
}

// begin registers a new sweeper. Returns a sweepLocker
// for acquiring spans for sweeping. Any outstanding sweeper blocks
// sweep termination.
//
// If the sweepLocker is invalid, the caller can be sure that all
// outstanding sweep work has been drained, so there is nothing left
// to sweep. Note that there may be sweepers currently running, so
// this does not indicate that all sweeping has completed.
//
// Even if the sweepLocker is invalid, its sweepGen is always valid.
// begin 注册一个新的 sweeper。返回一个 sweepLocker，用于获取 span 以进行清扫。任何未完成的 sweeper 都会阻止清扫终止。
// 如果 sweepLocker 无效，则调用者可以确定所有未完成的清扫工作都已耗尽，因此没有剩余的清扫工作。
// 请注意，可能存在正在运行的 sweeper，因此这并不表示所有清扫都已完成。
// 即使 sweepLocker 无效，其 sweepGen 始终有效。
func (a *activeSweep) begin() sweepLocker {
	for { // 无限循环，直到成功注册 sweeper 或确定没有更多工作
		state := a.state.Load()          // 加载当前的 activeSweep 状态
		if state&sweepDrainedMask != 0 { // 检查是否设置了 sweepDrainedMask，表示所有清扫工作都已耗尽
			return sweepLocker{mheap_.sweepgen, false} // 如果已耗尽，则返回一个无效的 sweepLocker，但 sweepGen 仍然有效
		}
		if a.state.CompareAndSwap(state, state+1) { // 尝试原子地将 sweeper 计数器加 1
			return sweepLocker{mheap_.sweepgen, true} // 如果成功，则返回一个有效的 sweepLocker，其中包含当前的 sweepGen
		}
		// 如果 CompareAndSwap 失败，则表示有其他 sweeper 正在尝试注册，因此循环重试
	}
}

// end deregisters a sweeper. Must be called once for each time
// begin is called if the sweepLocker is valid.
// end 注销一个 sweeper。如果 sweepLocker 有效，则每次调用 begin 都必须调用一次。
func (a *activeSweep) end(sl sweepLocker) {
	// Check that the sweeper being deregistered belongs to the current sweep generation.
	// 检查正在注销的 sweeper 是否属于当前的清扫代。
	if sl.sweepGen != mheap_.sweepgen {
		throw("sweeper left outstanding across sweep generations")
	}
	// Loop until the sweeper is successfully deregistered.
	// 循环直到 sweeper 成功注销。
	for {
		// Load the current state of the active sweep.
		// 加载 active sweep 的当前状态。
		state := a.state.Load()
		// Check for mismatched begin/end calls. This can happen if end is called
		// more times than begin.
		// 检查 begin/end 调用是否不匹配。如果调用 end 的次数多于 begin，则可能发生这种情况。
		if (state&^sweepDrainedMask)-1 >= sweepDrainedMask {
			throw("mismatched begin/end of activeSweep")
		}
		// Attempt to atomically decrement the sweeper count.
		// 尝试原子地减少 sweeper 计数。
		if a.state.CompareAndSwap(state, state-1) {
			// If the state was not sweepDrainedMask, then there are still
			// other sweepers running, so just return.
			// 如果状态不是 sweepDrainedMask，则表示仍有其他 sweeper 正在运行，因此只需返回。
			if state != sweepDrainedMask {
				return
			}
			// If this was the last sweeper and the sweep was drained,
			// then print some stats if gc pacertrace is enabled.
			// 如果这是最后一个 sweeper 并且清扫已耗尽，则在启用 gc pacertrace 时打印一些统计信息。
			if debug.gcpacertrace > 0 {
				live := gcController.heapLive.Load()
				print("pacer: sweep done at heap size ", live>>20, "MB; allocated ", (live-mheap_.sweepHeapLiveBasis)>>20, "MB during sweep; swept ", mheap_.pagesSwept.Load(), " pages at ", mheap_.sweepPagesPerByte, " pages/byte\n")
			}
			// Successfully deregistered the sweeper.
			// 成功注销 sweeper。
			return
		}
		// The CompareAndSwap failed, so try again.
		// CompareAndSwap 失败，因此重试。
	}
}

// markDrained marks the active sweep cycle as having drained
// all remaining work. This is safe to be called concurrently
// with all other methods of activeSweep, though may race.
//
// Returns true if this call was the one that actually performed
// the mark.
// markDrained 标记 active sweep 循环已耗尽所有剩余工作。
// 可以与其他 activeSweep 方法并发调用，但可能会发生竞争。
// 如果此调用实际执行了标记，则返回 true。
func (a *activeSweep) markDrained() bool {
	for {
		// Load the current state of the active sweep.
		// 加载 active sweep 的当前状态。
		state := a.state.Load()
		// Check if the sweep has already been drained.
		// 检查 sweep 是否已被耗尽。
		if state&sweepDrainedMask != 0 {
			return false
		}
		// Attempt to atomically mark the sweep as drained.
		// 尝试原子地将 sweep 标记为已耗尽。
		if a.state.CompareAndSwap(state, state|sweepDrainedMask) {
			return true
		}
		// The CompareAndSwap failed, so try again.
		// CompareAndSwap 失败，因此重试。
	}
}

// sweepers returns the current number of active sweepers.
// sweepers 返回当前活跃的 sweeper 的数量。
func (a *activeSweep) sweepers() uint32 {
	// Load the state and mask out the sweepDrained bit to get the number of sweepers.
	// 加载状态并屏蔽 sweepDrained 位，以获取 sweeper 的数量。
	return a.state.Load() &^ sweepDrainedMask
}

// isDone returns true if all sweep work has been drained and no more
// outstanding sweepers exist. That is, when the sweep phase is
// completely done.
// isDone 返回 true，如果所有 sweep 工作都已耗尽，并且不存在更多未完成的 sweeper。
// 也就是说，当 sweep 阶段完全完成时。
func (a *activeSweep) isDone() bool {
	// Check if the state is equal to the sweepDrainedMask, which indicates that the sweep is done.
	// 检查状态是否等于 sweepDrainedMask，这表示 sweep 已完成。
	return a.state.Load() == sweepDrainedMask
}

// reset sets up the activeSweep for the next sweep cycle.
//
// The world must be stopped.
// reset 为下一个 sweep 循环设置 activeSweep。
// 必须停止 world。
func (a *activeSweep) reset() {
	assertWorldStopped()
	// Reset the state to 0 to prepare for the next sweep cycle.
	// 将状态重置为 0，为下一个 sweep 循环做准备。
	a.state.Store(0)
}

// finishsweep_m ensures that all spans are swept.
//
// The world must be stopped. This ensures there are no sweeps in
// progress.
//
// finishsweep_m 确保所有 span 都被扫描。
//
// world 必须停止。这确保没有正在进行的扫描。
//
// finishsweep_m is called at the end of the sweep phase to ensure that
// all spans have been swept. This is done by calling sweepone() until
// it returns ^uintptr(0), which indicates that there are no more spans
// to sweep.
// finishsweep_m 在扫描阶段结束时被调用，以确保所有 span 都已被扫描。
// 这是通过调用 sweepone() 直到它返回 ^uintptr(0) 来完成的，
// 这表示没有更多要扫描的 span。
//
// The world must be stopped because this function accesses global
// data structures that are not protected by locks. If the world were
// not stopped, there would be a race condition between this function
// and other functions that access the same data structures.
// world 必须停止，因为此函数访问不受锁保护的全局数据结构。
// 如果 world 没有停止，则此函数和访问相同数据结构的其他函数之间
// 存在竞争条件。
//
//go:nowritebarrier
func finishsweep_m() {
	assertWorldStopped() // 确保 world 停止

	// Sweeping must be complete before marking commences, so
	// sweep any unswept spans. If this is a concurrent GC, there
	// shouldn't be any spans left to sweep, so this should finish
	// instantly. If GC was forced before the concurrent sweep
	// finished, there may be spans to sweep.
	// 在开始标记之前，必须完成 sweeping，因此扫描任何未扫描的 span。
	// 如果这是一个并发 GC，则不应有任何剩余的 span 需要扫描，因此这应该立即完成。
	// 如果在并发 sweep 完成之前强制执行 GC，则可能需要扫描 span。
	for sweepone() != ^uintptr(0) { // 扫描所有未扫描的 span
	}

	// Make sure there aren't any outstanding sweepers left.
	// At this point, with the world stopped, it means one of two
	// things. Either we were able to preempt a sweeper, or that
	// a sweeper didn't call sweep.active.end when it should have.
	// Both cases indicate a bug, so throw.
	// 确保没有遗留的 sweeper。
	// 此时，在 world 停止的情况下，这意味着两件事之一。
	// 要么我们能够抢占一个 sweeper，要么一个 sweeper 没有在应该调用 sweep.active.end 时调用它。
	// 这两种情况都表明存在 bug，因此抛出异常。
	if sweep.active.sweepers() != 0 { // 确保没有遗留的 sweeper
		throw("active sweepers found at start of mark phase") // 如果有，抛出异常
	}

	// Reset all the unswept buffers, which should be empty.
	// Do this in sweep termination as opposed to mark termination
	// so that we can catch unswept spans and reclaim blocks as
	// soon as possible.
	// 重置所有未扫描的缓冲区，这些缓冲区应该是空的。
	// 这样做是在扫描终止时而不是在标记终止时，
	// 这样我们就可以尽快捕获未扫描的 span 并回收块。
	sg := mheap_.sweepgen           // 获取 sweep generation，用于区分不同的 sweep 周期
	for i := range mheap_.central { // 遍历 central 列表，central 列表包含了所有 mcentral
		c := &mheap_.central[i].mcentral // 获取 mcentral，mcentral 负责管理特定大小的 span
		c.partialUnswept(sg).reset()     // 重置部分未扫描的 span 列表，这些 span 包含部分空闲对象
		c.fullUnswept(sg).reset()        // 重置完全未扫描的 span 列表，这些 span 不包含任何空闲对象
	}

	// Sweeping is done, so there won't be any new memory to
	// scavenge for a bit.
	//
	// If the scavenger isn't already awake, wake it up. There's
	// definitely work for it to do at this point.
	// 扫描已完成，因此暂时不会有新的内存需要整理。
	//
	// 如果 scavenger 还没有被唤醒，就唤醒它。
	// 此时肯定有工作要做。
	scavenger.wake() // 唤醒 scavenger，因为 sweeping 完成，可能需要进行内存整理

	nextMarkBitArenaEpoch() // 进入下一个 mark bit arena 周期
}

// bgsweep 是一个低优先级的后台 goroutine，用于执行垃圾回收的清扫工作。
// 它通过 channel 与主线程通信，当清扫工作完成后，主线程会从 channel 中读取数据，
// 从而知道清扫工作已经完成。
func bgsweep(c chan int) {
	sweep.g = getg()

	lockInit(&sweep.lock, lockRankSweep) // 初始化 sweep.lock，用于同步 sweep 过程
	lock(&sweep.lock)                    // 获取 sweep.lock，确保 sweep 过程的互斥性
	sweep.parked = true                  // 标记 bgsweep goroutine 为 parked 状态，表示它正在等待
	c <- 1                               // 向 channel c 发送一个信号，通知主 goroutine bgsweep 已经启动

	goparkunlock(&sweep.lock, waitReasonGCSweepWait, traceBlockGCSweep, 1) // 将 bgsweep goroutine 挂起，等待被唤醒，同时释放 sweep.lock

	for {
		// bgsweep attempts to be a "low priority" goroutine by intentionally
		// yielding time. It's OK if it doesn't run, because goroutines allocating
		// memory will sweep and ensure that all spans are swept before the next
		// GC cycle. We really only want to run when we're idle.
		// bgsweep 尝试成为一个“低优先级”的 goroutine，通过故意让出时间片来实现。
		// 即使它不运行也没关系，因为分配内存的 goroutine 会执行 sweep 操作，
		// 并确保在下一个 GC 周期之前所有 span 都被 sweep。我们只希望在空闲时运行。
		//
		// However, calling Gosched after each span swept produces a tremendous
		// amount of tracing events, sometimes up to 50% of events in a trace. It's
		// also inefficient to call into the scheduler so much because sweeping a
		// single span is in general a very fast operation, taking as little as 30 ns
		// on modern hardware. (See #54767.)
		// 然而，在每次 sweep span 后调用 Gosched 会产生大量的 tracing 事件，
		// 有时甚至占 trace 事件的 50%。频繁调用调度器也是低效的，
		// 因为 sweep 单个 span 通常是一个非常快速的操作，在现代硬件上只需 30 纳秒。
		//
		// As a result, bgsweep sweeps in batches, and only calls into the scheduler
		// at the end of every batch. Furthermore, it only yields its time if there
		// isn't spare idle time available on other cores. If there's available idle
		// time, helping to sweep can reduce allocation latencies by getting ahead of
		// the proportional sweeper and having spans ready to go for allocation.
		// 因此，bgsweep 分批执行 sweep 操作，并且仅在每个批次结束时才调用调度器。
		// 此外，只有在其他核心上没有空闲时间时，它才会让出时间片。
		// 如果有可用的空闲时间，帮助 sweep 可以通过抢在比例 sweeper 之前完成 sweep，
		// 并准备好用于分配的 span，从而减少分配延迟。
		const sweepBatchSize = 10       // 定义 sweep 的批次大小，每次 sweep 10 个 span
		nSwept := 0                     // 记录当前批次中 sweep 的 span 数量
		for sweepone() != ^uintptr(0) { // 调用 sweepone 函数 sweep 一个 span，直到没有 span 可 sweep 为止
			nSwept++                        // 增加 sweep 的 span 数量
			if nSwept%sweepBatchSize == 0 { // 如果 sweep 的 span 数量达到批次大小
				goschedIfBusy() // 如果系统繁忙，则让出时间片，避免过度占用 CPU 资源
			}
		}
		for freeSomeWbufs(true) { // 释放一些 write buffer，用于减少内存占用
			// N.B. freeSomeWbufs is already batched internally.
			// 注意：freeSomeWbufs 已经在内部进行了批处理。
			goschedIfBusy() // 如果系统繁忙，则让出时间片，避免过度占用 CPU 资源
		}
		lock(&sweep.lock)   // 获取 sweep.lock，确保后续操作的互斥性
		if !isSweepDone() { // 检查 sweep 是否完成
			// This can happen if a GC runs between
			// gosweepone returning ^0 above
			// and the lock being acquired.
			// 这可能发生在 gosweepone 返回 ^0 之后，但在获取锁之前，GC 运行了。
			unlock(&sweep.lock) // 释放 sweep.lock
			continue            // 继续下一次循环，重新执行 sweep 操作
		}
		sweep.parked = true                                                    // 标记 bgsweep goroutine 为 parked 状态，表示它正在等待
		goparkunlock(&sweep.lock, waitReasonGCSweepWait, traceBlockGCSweep, 1) // 将 bgsweep goroutine 挂起，等待被唤醒，同时释放 sweep.lock
	}
}

// sweepLocker acquires sweep ownership of spans.
// sweepLocker 用于获取 span 的 sweep 所有权。
type sweepLocker struct {
	// sweepGen is the sweep generation of the heap.
	// sweepGen 是堆的 sweep 代数。
	sweepGen uint32
	// valid indicates whether this sweepLocker is valid.
	// valid 表示此 sweepLocker 是否有效。
	valid bool
}

// sweepLocked represents sweep ownership of a span.
// sweepLocked 表示 span 的 sweep 所有权。
type sweepLocked struct {
	*mspan
}

// tryAcquire attempts to acquire sweep ownership of span s. If it
// successfully acquires ownership, it blocks sweep completion.
// tryAcquire 尝试获取 span s 的 sweep 所有权。如果成功获取所有权，它会阻止 sweep 完成。
func (l *sweepLocker) tryAcquire(s *mspan) (sweepLocked, bool) {
	if !l.valid { // 检查 sweepLocker 是否有效
		throw("use of invalid sweepLocker") // 如果 sweepLocker 无效，则抛出异常
	}
	// Check before attempting to CAS.
	// 在尝试 CAS 之前进行检查。
	if atomic.Load(&s.sweepgen) != l.sweepGen-2 { // 检查 span 的 sweep 代数是否为 l.sweepGen-2
		return sweepLocked{}, false // 如果不是，则返回 sweepLocked{} 和 false，表示获取所有权失败
	}
	// Attempt to acquire sweep ownership of s.
	// 尝试获取 span s 的 sweep 所有权。
	if !atomic.Cas(&s.sweepgen, l.sweepGen-2, l.sweepGen-1) { // 尝试将 span 的 sweep 代数从 l.sweepGen-2 更新为 l.sweepGen-1，使用 CAS 操作
		return sweepLocked{}, false // 如果 CAS 操作失败，则返回 sweepLocked{} 和 false，表示获取所有权失败
	}
	return sweepLocked{s}, true // 如果 CAS 操作成功，则返回 sweepLocked{s} 和 true，表示成功获取所有权
}

// sweepone sweeps some unswept heap span and returns the number of pages returned
// to the heap, or ^uintptr(0) if there was nothing to sweep.
// sweepone 扫描一些未扫描的堆 span 并返回返回给堆的页数，或者 ^uintptr(0) 如果没有可扫描的 span。
func sweepone() uintptr {
	gp := getg()

	// Increment locks to ensure that the goroutine is not preempted
	// in the middle of sweep thus leaving the span in an inconsistent state for next GC
	// 增加锁的数量，以确保 goroutine 在 sweep 过程中不会被抢占，从而使 span 处于不一致的状态，影响下一次 GC
	gp.m.locks++

	// TODO(austin): sweepone is almost always called in a loop;
	// lift the sweepLocker into its callers.
	// TODO(austin)：sweepone 几乎总是在循环中被调用；将 sweepLocker 提升到其调用者中。
	sl := sweep.active.begin() // 获取一个 sweepLocker，用于获取 span 的 sweep 所有权
	if !sl.valid {             // 检查 sweepLocker 是否有效
		gp.m.locks--       // 如果 sweepLocker 无效，则减少锁的数量
		return ^uintptr(0) // 返回 ^uintptr(0)，表示没有可扫描的 span
	}

	// Find a span to sweep.
	// 查找要 sweep 的 span。
	npages := ^uintptr(0) // 初始化 npages 为 ^uintptr(0)，表示没有可扫描的 span
	var noMoreWork bool   // 声明一个变量 noMoreWork，用于表示是否没有更多的工作
	for {
		s := mheap_.nextSpanForSweep() // 获取下一个需要 sweep 的 span
		if s == nil {                  // 如果没有找到需要 sweep 的 span
			noMoreWork = sweep.active.markDrained() // 标记 sweep 队列已耗尽
			break                                   // 退出循环
		}
		if state := s.state.get(); state != mSpanInUse { // 检查 span 的状态是否为 mSpanInUse
			// This can happen if direct sweeping already
			// swept this span, but in that case the sweep
			// generation should always be up-to-date.
			// 这可能发生在直接 sweeping 已经 sweep 了此 span 的情况下，但如果是这种情况，sweep 的代应该始终是最新的。
			if !(s.sweepgen == sl.sweepGen || s.sweepgen == sl.sweepGen+3) { // 检查 span 的 sweep 代数是否与 sweepLocker 的 sweep 代数匹配
				print("runtime: bad span s.state=", state, " s.sweepgen=", s.sweepgen, " sweepgen=", sl.sweepGen, "\n") // 如果不匹配，则打印错误信息
				throw("non in-use span in unswept list")                                                                // 抛出异常
			}
			continue // 继续下一次循环
		}
		if s, ok := sl.tryAcquire(s); ok { // 尝试获取 span 的 sweep 所有权
			// Sweep the span we found.
			// Sweep 我们找到的 span。
			npages = s.npages   // 获取 span 的页数
			if s.sweep(false) { // Sweep span
				// Whole span was freed. Count it toward the
				// page reclaimer credit since these pages can
				// now be used for span allocation.
				// 整个 span 被释放。将其计入页面回收器信用，因为这些页面现在可以用于 span 分配。
				mheap_.reclaimCredit.Add(npages) // 增加 mheap_ 的回收信用
			} else {
				// Span is still in-use, so this returned no
				// pages to the heap and the span needs to
				// move to the swept in-use list.
				// Span 仍然在使用中，因此这不会向堆返回任何页面，并且 span 需要移动到已 sweep 的使用中列表。
				npages = 0 // 将 npages 设置为 0
			}
			break // 退出循环
		}
	}
	sweep.active.end(sl)

	if noMoreWork {
		// The sweep list is empty. There may still be
		// concurrent sweeps running, but we're at least very
		// close to done sweeping.
		// sweep 列表为空。可能仍然有并发的 sweeps 运行，但我们至少非常接近完成 sweeping。

		// Move the scavenge gen forward (signaling
		// that there's new work to do) and wake the scavenger.
		// 移动 scavenge gen（表示有新的工作要做）并唤醒 scavenger。
		//
		// The scavenger is signaled by the last sweeper because once
		// sweeping is done, we will definitely have useful work for
		// the scavenger to do, since the scavenger only runs over the
		// heap once per GC cycle. This update is not done during sweep
		// termination because in some cases there may be a long delay
		// between sweep done and sweep termination (e.g. not enough
		// allocations to trigger a GC) which would be nice to fill in
		// with scavenging work.
		// scavenger 由最后一个 sweeper 发出信号，因为一旦 sweeping 完成，我们将肯定有有用的工作供 scavenger 执行，
		// 因为 scavenger 每个 GC 周期只在堆上运行一次。此更新不是在 sweep 终止期间完成的，
		// 因为在某些情况下，sweep 完成和 sweep 终止之间可能存在很长的延迟（例如，没有足够的分配来触发 GC），
		// 这将很高兴用 scavenging 工作来填补。
		if debug.scavtrace > 0 {
			systemstack(func() {
				lock(&mheap_.lock)

				// Get released stats.
				// 获取已释放的统计信息。
				releasedBg := mheap_.pages.scav.releasedBg.Load()
				releasedEager := mheap_.pages.scav.releasedEager.Load()

				// Print the line.
				// 打印该行。
				printScavTrace(releasedBg, releasedEager, false)

				// Update the stats.
				// 更新统计信息。
				mheap_.pages.scav.releasedBg.Add(-releasedBg)
				mheap_.pages.scav.releasedEager.Add(-releasedEager)
				unlock(&mheap_.lock)
			})
		}
		scavenger.ready() // 唤醒 scavenger。
	}

	gp.m.locks--
	return npages
}

// isSweepDone reports whether all spans are swept.
//
// Note that this condition may transition from false to true at any
// time as the sweeper runs. It may transition from true to false if a
// GC runs; to prevent that the caller must be non-preemptible or must
// somehow block GC progress.
// isSweepDone 报告是否所有 span 都已完成清理。
//
// 注意，随着 sweeper 的运行，此条件可能随时从 false 变为 true。如果 GC 运行，它可能会从 true 变为 false；
// 为了防止这种情况，调用者必须是不可抢占的，或者必须以某种方式阻止 GC 进度。
func isSweepDone() bool {
	return sweep.active.isDone()
}

// Returns only when span s has been swept.
//
//go:nowritebarrier
func (s *mspan) ensureSwept() {
	// Caller must disable preemption.
	// Otherwise when this function returns the span can become unswept again
	// (if GC is triggered on another goroutine).
	// 调用者必须禁用抢占。
	// 否则，当此函数返回时，span 可能会再次变为未清理状态
	// （如果在另一个 goroutine 上触发了 GC）。
	gp := getg()
	if gp.m.locks == 0 && gp.m.mallocing == 0 && gp != gp.m.g0 {
		throw("mspan.ensureSwept: m is not locked")
	}

	// If this operation fails, then that means that there are
	// no more spans to be swept. In this case, either s has already
	// been swept, or is about to be acquired for sweeping and swept.
	// 如果此操作失败，则表示没有更多 span 需要清理。
	// 在这种情况下，s 要么已经被清理，要么即将被获取以进行清理。
	sl := sweep.active.begin()
	if sl.valid {
		// The caller must be sure that the span is a mSpanInUse span.
		// 调用者必须确保 span 是一个 mSpanInUse span。
		if s, ok := sl.tryAcquire(s); ok {
			s.sweep(false)
			sweep.active.end(sl)
			return
		}
		sweep.active.end(sl)
	}

	// Unfortunately we can't sweep the span ourselves. Somebody else
	// got to it first. We don't have efficient means to wait, but that's
	// OK, it will be swept fairly soon.
	// 遗憾的是，我们无法自己清理 span。其他人先拿到了它。
	// 我们没有有效的等待方式，但这没关系，它很快就会被清理。
	for {
		spangen := atomic.Load(&s.sweepgen)
		if spangen == sl.sweepGen || spangen == sl.sweepGen+3 {
			break
		}
		osyield()
	}
}

// sweep frees or collects finalizers for blocks not marked in the mark phase.
// It clears the mark bits in preparation for the next GC round.
// Returns true if the span was returned to heap.
// If preserve=true, don't return it to heap nor relink in mcentral lists;
// caller takes care of it.
// sweep 释放或收集未在标记阶段标记的块的 finalizers。
// 它清除标记位，为下一个 GC 轮次做准备。
// 如果 span 被返回给堆，则返回 true。
// 如果 preserve=true，不要将 span 返回给堆或重新链接到 mcentral 列表；
// 调用者负责处理它。
func (sl *sweepLocked) sweep(preserve bool) bool {
	// It's critical that we enter this function with preemption disabled,
	// GC must not start while we are in the middle of this function.
	// 关键在于，进入此函数时必须禁用抢占，
	// 在此函数执行期间，GC 不得启动。
	gp := getg()
	if gp.m.locks == 0 && gp.m.mallocing == 0 && gp != gp.m.g0 {
		throw("mspan.sweep: m is not locked")
	}

	s := sl.mspan
	if !preserve {
		// We'll release ownership of this span. Nil it out to
		// prevent the caller from accidentally using it.
		// 如果不保留 span，我们将释放对此 span 的所有权。
		// 将其设置为 nil，以防止调用者意外使用它。
		sl.mspan = nil
	}

	sweepgen := mheap_.sweepgen
	if state := s.state.get(); state != mSpanInUse || s.sweepgen != sweepgen-1 {
		// 如果 span 的状态不是 mSpanInUse，或者 sweepgen 不是 sweepgen-1，则抛出异常。
		print("mspan.sweep: state=", state, " sweepgen=", s.sweepgen, " mheap.sweepgen=", sweepgen, "\n")
		throw("mspan.sweep: bad span state")
	}

	trace := traceAcquire()
	if trace.ok() {
		// 如果 trace 有效，则记录 span 的页数。
		trace.GCSweepSpan(s.npages * _PageSize)
		traceRelease(trace)
	}

	// 增加 mheap_ 的已扫页数。
	mheap_.pagesSwept.Add(int64(s.npages))

	spc := s.spanclass
	size := s.elemsize

	// The allocBits indicate which unmarked objects don't need to be
	// processed since they were free at the end of the last GC cycle
	// and were not allocated since then.
	// If the allocBits index is >= s.freeindex and the bit
	// is not marked then the object remains unallocated
	// since the last GC.
	// This situation is analogous to being on a freelist.
	// allocBits 指示哪些未标记的对象不需要处理，因为它们在上一个 GC 周期结束时是空闲的，
	// 并且自那时以来没有被分配。
	// 如果 allocBits 索引 >= s.freeindex 并且该位未被标记，
	// 则该对象自上次 GC 以来仍未被分配。
	// 这种情况类似于在空闲列表中。

	// Unlink & free special records for any objects we're about to free.
	// Two complications here:
	// 1. An object can have both finalizer and profile special records.
	//    In such case we need to queue finalizer for execution,
	//    mark the object as live and preserve the profile special.
	// 2. A tiny object can have several finalizers setup for different offsets.
	//    If such object is not marked, we need to queue all finalizers at once.
	// Both 1 and 2 are possible at the same time.
	// 取消链接并释放我们将要释放的任何对象的特殊记录。
	// 这里有两个复杂情况：
	// 1. 一个对象可以同时具有 finalizer 和 profile 特殊记录。
	//    在这种情况下，我们需要将 finalizer 排队以供执行，
	//    将对象标记为 live 并保留 profile 特殊记录。
	// 2. 一个微小的对象可以为不同的偏移量设置多个 finalizer。
	//    如果这样的对象未被标记，我们需要一次性将所有 finalizer 排队。
	// 1 和 2 可能同时发生。
	hadSpecials := s.specials != nil
	siter := newSpecialsIter(s)
	for siter.valid() {
		// A finalizer can be set for an inner byte of an object, find object beginning.
		// finalizer 可以被设置为对象的内部字节，找到对象的起始位置。
		objIndex := uintptr(siter.s.offset) / size // 计算对象在其 span 中的索引。Calculate the index of the object within its span.
		p := s.base() + objIndex*size              // 计算对象的起始地址。Calculate the starting address of the object.
		mbits := s.markBitsForIndex(objIndex)      // 获取对象对应的 mark 位。Get the mark bits corresponding to the object.
		if !mbits.isMarked() {
			// This object is not marked and has at least one special record.
			// 这个对象未被标记，并且至少有一个特殊记录。
			// Pass 1: see if it has a finalizer.
			// 第一步：查看它是否有一个 finalizer。
			hasFinAndRevived := false
			endOffset := p - s.base() + size
			for tmp := siter.s; tmp != nil && uintptr(tmp.offset) < endOffset; tmp = tmp.next {
				if tmp.kind == _KindSpecialFinalizer {
					// Stop freeing of object if it has a finalizer.
					// 如果对象有一个 finalizer，则停止释放该对象。
					mbits.setMarkedNonAtomic()
					hasFinAndRevived = true
					break
				}
			}
			if hasFinAndRevived {
				// Pass 2: queue all finalizers and clear any weak handles. Weak handles are cleared
				// before finalization as specified by the internal/weak package. See the documentation
				// for that package for more details.
				// 第二步：将所有 finalizer 排队并清除所有 weak handle。按照 internal/weak 包的规定，weak handle 在 finalization 之前被清除。
				// 更多细节请参考该包的文档。
				for siter.valid() && uintptr(siter.s.offset) < endOffset {
					// Find the exact byte for which the special was setup
					// (as opposed to object beginning).
					// 找到设置 special 的确切字节（而不是对象开始）。
					special := siter.s                      // 获取当前的 special 记录。Get the current special record.
					p := s.base() + uintptr(special.offset) // 计算 special 记录对应的地址。Calculate the address corresponding to the special record.
					if special.kind == _KindSpecialFinalizer || special.kind == _KindSpecialWeakHandle {
						// 如果 special 记录的类型是 finalizer 或 weak handle。If the special record type is finalizer or weak handle.
						siter.unlinkAndNext()                         // 从链表中移除 special 记录，并移动到下一个记录。Remove the special record from the list and move to the next record.
						freeSpecial(special, unsafe.Pointer(p), size) // 释放 special 记录。Free the special record.
					} else {
						// All other specials only apply when an object is freed,
						// so just keep the special record.
						// 所有其他的 special 记录只在对象被释放时应用，所以只需保留 special 记录。
						siter.next() // 移动到下一个 special 记录。Move to the next special record.
					}
				}
			} else {
				// Pass 2: the object is truly dead, free (and handle) all specials.
				// 第二步：对象确实已经死亡，释放（并处理）所有 special 记录。
				for siter.valid() && uintptr(siter.s.offset) < endOffset {
					// Find the exact byte for which the special was setup
					// (as opposed to object beginning).
					// 找到设置 special 的确切字节（而不是对象开始）。
					special := siter.s                            // 获取当前的 special 记录。Get the current special record.
					p := s.base() + uintptr(special.offset)       // 计算 special 记录对应的地址。Calculate the address corresponding to the special record.
					siter.unlinkAndNext()                         // 从链表中移除 special 记录，并移动到下一个记录。Remove the special record from the list and move to the next record.
					freeSpecial(special, unsafe.Pointer(p), size) // 释放 special 记录。Free the special record.
				}
			}
		} else {
			// object is still live
			// 对象仍然存活
			if siter.s.kind == _KindSpecialReachable {
				// If the special record is a reachable special, unlink it and mark it as reachable.
				// 如果 special 记录是一个 reachable special，则将其取消链接并标记为 reachable。
				special := siter.unlinkAndNext()                              // Unlink the special record and move to the next. 取消链接 special 记录并移动到下一个记录。
				(*specialReachable)(unsafe.Pointer(special)).reachable = true // Mark the special as reachable. 将 special 标记为 reachable。
				freeSpecial(special, unsafe.Pointer(p), size)                 // Free the special record. 释放 special 记录。
			} else {
				// Otherwise, keep the special record.
				// 否则，保留 special 记录。
				// keep special record
				// 保留 special 记录
				siter.next() // Move to the next special record. 移动到下一个 special 记录。
			}
		}
	}
	// If the span had specials at some point, but doesn't have any now,
	// inform the span that it has no specials anymore. This is an optimization
	// to avoid scanning the span's specials list in the future.
	// 如果 span 曾经有 special 对象，但现在没有任何 special 对象，
	// 则通知 span 它不再有 special 对象。这是一种优化，
	// 可以避免将来扫描 span 的 special 对象列表。
	if hadSpecials && s.specials == nil {
		spanHasNoSpecials(s)
	}

	if traceAllocFreeEnabled() || debug.clobberfree != 0 || raceenabled || msanenabled || asanenabled {
		// Find all newly freed objects.
		// 查找所有新释放的对象。
		mbits := s.markBitsForBase()
		// Get the mark bits for the base of the span.
		// 获取 span 基本地址的 mark bits。
		abits := s.allocBitsForIndex(0)
		// Get the allocation bits for the first object in the span.
		// 获取 span 中第一个对象的分配位。
		for i := uintptr(0); i < uintptr(s.nelems); i++ {
			// Iterate over all elements in the span.
			// 遍历 span 中的所有元素。
			if !mbits.isMarked() && (abits.index < uintptr(s.freeindex) || abits.isMarked()) {
				// If the object is not marked and either its index is less than freeindex or it is marked as allocated...
				// 如果对象未被标记，并且其索引小于 freeindex 或被标记为已分配...
				x := s.base() + i*s.elemsize
				// Calculate the address of the object.
				// 计算对象的地址。
				if traceAllocFreeEnabled() {
					// If tracing of allocation and free events is enabled...
					// 如果启用了分配和释放事件的跟踪...
					trace := traceAcquire()
					// Acquire a trace buffer.
					// 获取一个跟踪缓冲区。
					if trace.ok() {
						// If the trace buffer is valid...
						// 如果跟踪缓冲区有效...
						trace.HeapObjectFree(x)
						// Record the heap object free event.
						// 记录堆对象释放事件。
						traceRelease(trace)
						// Release the trace buffer.
						// 释放跟踪缓冲区。
					}
				}
				if debug.clobberfree != 0 {
					// If clobberfree is enabled...
					// 如果启用了 clobberfree...
					clobberfree(unsafe.Pointer(x), size)
					// Overwrite the freed memory with a known pattern.
					// 用已知的模式覆盖释放的内存。
				}
				// User arenas are handled on explicit free.
				// 用户 arena 在显式释放时处理。
				if raceenabled && !s.isUserArenaChunk {
					// If the race detector is enabled and this is not a user arena chunk...
					// 如果启用了 race 检测器，并且这不是用户 arena chunk...
					racefree(unsafe.Pointer(x), size)
					// Notify the race detector that the object has been freed.
					// 通知 race 检测器该对象已被释放。
				}
				if msanenabled && !s.isUserArenaChunk {
					// If memory sanitizer is enabled and this is not a user arena chunk...
					// 如果启用了内存清理器，并且这不是用户 arena chunk...
					msanfree(unsafe.Pointer(x), size)
					// Notify the memory sanitizer that the object has been freed.
					// 通知内存清理器该对象已被释放。
				}
				if asanenabled && !s.isUserArenaChunk {
					// If address sanitizer is enabled and this is not a user arena chunk...
					// 如果启用了地址清理器，并且这不是用户 arena chunk...
					asanpoison(unsafe.Pointer(x), size)
					// Poison the freed memory.
					// 污染释放的内存。
				}
			}
			mbits.advance()
			// Advance to the next mark bit.
			// 前进到下一个 mark bit。
			abits.advance()
			// Advance to the next allocation bit.
			// 前进到下一个分配 bit。
		}
	}

	// Check for zombie objects.
	// 检查是否存在 zombie 对象。
	if s.freeindex < s.nelems {
		// Everything < freeindex is allocated and hence
		// cannot be zombies.
		//
		// Check the first bitmap byte, where we have to be
		// careful with freeindex.
		// 小于 freeindex 的所有对象都已分配，因此不可能是 zombie 对象。
		//
		// 检查第一个位图字节，我们需要小心处理 freeindex。
		obj := uintptr(s.freeindex)
		// 获取 freeindex 对应的对象索引。
		if (*s.gcmarkBits.bytep(obj / 8)&^*s.allocBits.bytep(obj / 8))>>(obj%8) != 0 {
			// 如果 gcmarkBits 中标记了该对象，但 allocBits 中未标记，则表示该对象是 zombie 对象。
			s.reportZombies()
			// 报告 zombie 对象。
		}
		// Check remaining bytes.
		// 检查剩余的字节。
		for i := obj/8 + 1; i < divRoundUp(uintptr(s.nelems), 8); i++ {
			// 遍历剩余的字节。
			if *s.gcmarkBits.bytep(i)&^*s.allocBits.bytep(i) != 0 {
				// 如果 gcmarkBits 中标记了该对象，但 allocBits 中未标记，则表示该对象是 zombie 对象。
				s.reportZombies()
				// 报告 zombie 对象。
			}
		}
	}

	// Count the number of free objects in this span.
	// 统计此 span 中的空闲对象数量。
	nalloc := uint16(s.countAlloc())
	// 计算当前已分配的对象数量。
	nfreed := s.allocCount - nalloc
	// 计算已释放的对象数量。
	if nalloc > s.allocCount {
		// The zombie check above should have caught this in
		// more detail.
		// 上面的 zombie 对象检查应该已经更详细地捕获了这一点。
		print("runtime: nelems=", s.nelems, " nalloc=", nalloc, " previous allocCount=", s.allocCount, " nfreed=", nfreed, "\n")
		throw("sweep increased allocation count")
	}

	s.allocCount = nalloc
	// 更新 span 的已分配对象计数。
	s.freeindex = 0 // reset allocation index to start of span.
	// 将分配索引重置为 span 的开头。
	s.freeIndexForScan = 0
	// 重置用于扫描的空闲索引。
	if traceEnabled() {
		getg().m.p.ptr().trace.reclaimed += uintptr(nfreed) * s.elemsize
	}

	// gcmarkBits becomes the allocBits.
	// get a fresh cleared gcmarkBits in preparation for next GC
	// gcmarkBits 变为 allocBits。
	// 获取一个新的清除后的 gcmarkBits，为下一次 GC 做准备。
	s.allocBits = s.gcmarkBits
	// 将 gcmarkBits 赋值给 allocBits，表示分配位图更新为上次 GC 的标记位图。
	s.gcmarkBits = newMarkBits(uintptr(s.nelems))
	// 创建一个新的 gcmarkBits，用于下一次 GC。

	// refresh pinnerBits if they exists
	// 如果存在 pinnerBits，则刷新它们。
	if s.pinnerBits != nil {
		s.refreshPinnerBits()
	}

	// Initialize alloc bits cache.
	// 初始化分配位缓存。
	s.refillAllocCache(0)
	// 重新填充分配缓存。

	// The span must be in our exclusive ownership until we update sweepgen,
	// check for potential races.
	// span 必须由我们独占拥有，直到我们更新 sweepgen，检查潜在的竞争。
	if state := s.state.get(); state != mSpanInUse || s.sweepgen != sweepgen-1 {
		print("mspan.sweep: state=", state, " sweepgen=", s.sweepgen, " mheap.sweepgen=", sweepgen, "\n")
		throw("mspan.sweep: bad span state after sweep")
	}
	if s.sweepgen == sweepgen+1 || s.sweepgen == sweepgen+3 {
		throw("swept cached span")
	}

	// We need to set s.sweepgen = h.sweepgen only when all blocks are swept,
	// because of the potential for a concurrent free/SetFinalizer.
	// 只有当所有块都被清理后，我们才需要设置 s.sweepgen = h.sweepgen，因为可能存在并发的 free/SetFinalizer。
	//
	// But we need to set it before we make the span available for allocation
	// (return it to heap or mcentral), because allocation code assumes that a
	// span is already swept if available for allocation.
	// 但是，我们需要在使 span 可用于分配（将其返回到堆或 mcentral）之前设置它，因为分配代码假定 span 如果可用于分配，则已经被清理。
	//
	// Serialization point.
	// 序列化点。
	// At this point the mark bits are cleared and allocation ready
	// to go so release the span.
	// 此时，标记位已清除，并且分配已准备好，因此释放 span。
	atomic.Store(&s.sweepgen, sweepgen)

	if s.isUserArenaChunk {
		if preserve {
			// This is a case that should never be handled by a sweeper that
			// preserves the span for reuse.
			// 这是一种不应该由保留 span 以供重用的 sweeper 处理的情况。
			throw("sweep: tried to preserve a user arena span")
		}
		if nalloc > 0 {
			// There still exist pointers into the span or the span hasn't been
			// freed yet. It's not ready to be reused. Put it back on the
			// full swept list for the next cycle.
			// 仍然存在指向 span 的指针，或者 span 尚未被释放。它尚未准备好重用。将其放回完整的已清理列表，以供下一个周期使用。
			mheap_.central[spc].mcentral.fullSwept(sweepgen).push(s)
			return false
		}

		// It's only at this point that the sweeper doesn't actually need to look
		// at this arena anymore, so subtract from pagesInUse now.
		// 只有在这个时候，sweeper 才不需要再查看这个 arena，所以现在从 pagesInUse 中减去。
		mheap_.pagesInUse.Add(-s.npages)
		s.state.set(mSpanDead)

		// The arena is ready to be recycled. Remove it from the quarantine list
		// and place it on the ready list. Don't add it back to any sweep lists.
		// arena 准备好回收了。从隔离列表中删除它，并将其放在就绪列表中。不要将其添加回任何清理列表。
		systemstack(func() {
			// It's the arena code's responsibility to get the chunk on the quarantine
			// list by the time all references to the chunk are gone.
			// arena 代码有责任在所有对 chunk 的引用都消失后，将 chunk 放在隔离列表中。
			if s.list != &mheap_.userArena.quarantineList {
				throw("user arena span is on the wrong list")
			}
			lock(&mheap_.lock)
			mheap_.userArena.quarantineList.remove(s)
			mheap_.userArena.readyList.insert(s)
			unlock(&mheap_.lock)
		})
		return false
	}

	if spc.sizeclass() != 0 {
		// Handle spans for small objects.
		// 处理小对象的 span。
		if nfreed > 0 {
			// Only mark the span as needing zeroing if we've freed any
			// objects, because a fresh span that had been allocated into,
			// wasn't totally filled, but then swept, still has all of its
			// free slots zeroed.
			// 只有在我们释放了任何对象时，才将 span 标记为需要清零，因为一个新 span 如果已经被分配到，
			// 但没有完全填满，然后被清理，仍然有所有空闲的槽被清零。
			s.needzero = 1
			stats := memstats.heapStats.acquire()
			atomic.Xadd64(&stats.smallFreeCount[spc.sizeclass()], int64(nfreed))
			memstats.heapStats.release()

			// Count the frees in the inconsistent, internal stats.
			// 在不一致的内部统计数据中计算释放的数量。
			gcController.totalFree.Add(int64(nfreed) * int64(s.elemsize))
		}
		if !preserve {
			// The caller may not have removed this span from whatever
			// unswept set its on but taken ownership of the span for
			// sweeping by updating sweepgen. If this span still is in
			// an unswept set, then the mcentral will pop it off the
			// set, check its sweepgen, and ignore it.
			// 调用者可能没有从任何未清理的集合中删除此 span，而是通过更新 sweepgen 来获取 span 的所有权以进行清理。
			// 如果此 span 仍然在未清理的集合中，则 mcentral 将从集合中弹出它，检查其 sweepgen，并忽略它。
			if nalloc == 0 {
				// Free totally free span directly back to the heap.
				// 将完全空闲的 span 直接释放回堆。
				mheap_.freeSpan(s)
				return true
			}
			// Return span back to the right mcentral list.
			// 将 span 返回到正确的 mcentral 列表。
			if nalloc == s.nelems {
				mheap_.central[spc].mcentral.fullSwept(sweepgen).push(s)
			} else {
				mheap_.central[spc].mcentral.partialSwept(sweepgen).push(s)
			}
		}
	} else if !preserve {
		// Handle spans for large objects.
		// 处理大对象的 span。
		if nfreed != 0 {
			// Free large object span to heap.
			// 释放大对象的 span 到堆中。

			// Count the free in the consistent, external stats.
			//
			// Do this before freeSpan, which might update heapStats' inHeap
			// value. If it does so, then metrics that subtract object footprint
			// from inHeap might overflow. See #67019.
			// 在一致的外部统计数据中计算释放的数量。
			//
			// 在 freeSpan 之前执行此操作，freeSpan 可能会更新 heapStats 的 inHeap 值。
			// 如果这样做，则从 inHeap 中减去对象 footprint 的指标可能会溢出。参见 #67019。
			stats := memstats.heapStats.acquire()
			atomic.Xadd64(&stats.largeFreeCount, 1)      // Increment the count of large frees. 增加大对象释放的计数。
			atomic.Xadd64(&stats.largeFree, int64(size)) // Increment the total size of large frees. 增加大对象释放的总大小。
			memstats.heapStats.release()

			// Count the free in the inconsistent, internal stats.
			// 在不一致的内部统计数据中计算释放的数量。
			gcController.totalFree.Add(int64(size))

			// NOTE(rsc,dvyukov): The original implementation of efence
			// in CL 22060046 used sysFree instead of sysFault, so that
			// the operating system would eventually give the memory
			// back to us again, so that an efence program could run
			// longer without running out of memory. Unfortunately,
			// calling sysFree here without any kind of adjustment of the
			// heap data structures means that when the memory does
			// come back to us, we have the wrong metadata for it, either in
			// the mspan structures or in the garbage collection bitmap.
			// Using sysFault here means that the program will run out of
			// memory fairly quickly in efence mode, but at least it won't
			// have mysterious crashes due to confused memory reuse.
			// It should be possible to switch back to sysFree if we also
			// implement and then call some kind of mheap.deleteSpan.
			// NOTE(rsc,dvyukov): efence 的原始实现在 CL 22060046 中使用 sysFree 而不是 sysFault，
			// 这样操作系统最终会将内存还给我们，以便 efence 程序可以运行更长时间而不会耗尽内存。
			// 不幸的是，在没有任何堆数据结构调整的情况下调用 sysFree 意味着当内存返回给我们时，
			// 我们的元数据是错误的，无论是在 mspan 结构中还是在垃圾收集位图中。
			// 在这里使用 sysFault 意味着程序在 efence 模式下会很快耗尽内存，但至少它不会因为混淆的内存重用而发生神秘的崩溃。
			// 如果我们还实现然后调用某种 mheap.deleteSpan，则应该可以切换回 sysFree。

			if debug.efence > 0 {
				// In efence mode, fault the memory instead of freeing it.
				// 在 efence 模式下，fault 内存而不是释放它。
				s.limit = 0                              // prevent mlookup from finding this span 防止 mlookup 找到这个 span
				sysFault(unsafe.Pointer(s.base()), size) // Fault the memory. Fault 内存。
			} else {
				// Free the span back to the heap.
				// 将 span 释放回堆。
				mheap_.freeSpan(s)
			}
			if s.largeType != nil && s.largeType.TFlag&abi.TFlagUnrolledBitmap != 0 {
				// The unrolled GCProg bitmap is allocated separately.
				// Free the space for the unrolled bitmap.
				// 解开的 GCProg 位图是单独分配的。
				// 释放用于解开的位图的空间。
				systemstack(func() {
					// Get the span for the large type.
					// 获取 large type 的 span。
					s := spanOf(uintptr(unsafe.Pointer(s.largeType)))
					// Free the manual span.
					// 释放 manual span。
					mheap_.freeManual(s, spanAllocPtrScalarBits)
				})
				// Make sure to zero this pointer without putting the old
				// value in a write buffer, as the old value might be an
				// invalid pointer. See arena.go:(*mheap).allocUserArenaChunk.
				// 确保将此指针清零，而不要将旧值放入写入缓冲区中，因为旧值可能是一个无效的指针。
				// 参见 arena.go:(*mheap).allocUserArenaChunk。
				*(*uintptr)(unsafe.Pointer(&s.largeType)) = 0
			}
			return true
		}

		// Add a large span directly onto the full+swept list.
		// 将大对象的 span 直接添加到完整的已清理列表中。
		mheap_.central[spc].mcentral.fullSwept(sweepgen).push(s)
	}
	return false
}

// reportZombies reports any marked but free objects in s and throws.
//
// This generally means one of the following:
//
// 1. User code converted a pointer to a uintptr and then back
// unsafely, and a GC ran while the uintptr was the only reference to
// an object.
//
// 2. User code (or a compiler bug) constructed a bad pointer that
// points to a free slot, often a past-the-end pointer.
//
// 3. The GC two cycles ago missed a pointer and freed a live object,
// but it was still live in the last cycle, so this GC cycle found a
// pointer to that object and marked it.
// reportZombies 报告 s 中任何已标记但已释放的对象，并抛出异常。
//
// 这通常意味着以下情况之一：
//
//  1. 用户代码将指针转换为 uintptr，然后又以不安全的方式转换回来，
//     并且在 uintptr 是对对象的唯一引用时运行了 GC。
//
//  2. 用户代码（或编译器错误）构造了一个错误的指针，该指针指向一个空闲槽，
//     通常是指向末尾的指针。
//
//  3. 两次 GC 循环前，GC 遗漏了一个指针并释放了一个活动对象，
//     但它在上次循环中仍然处于活动状态，因此本次 GC 循环找到了指向该对象的指针并对其进行了标记。
func (s *mspan) reportZombies() {
	// reportZombies reports any marked but free objects in s and throws.
	// reportZombies 报告 s 中任何已标记但已释放的对象，并抛出异常。
	printlock()
	print("runtime: marked free object in span ", s, ", elemsize=", s.elemsize, " freeindex=", s.freeindex, " (bad use of unsafe.Pointer? try -d=checkptr)\n")
	mbits := s.markBitsForBase()                      // 获取 mark bits 的起始地址
	abits := s.allocBitsForIndex(0)                   // 获取 alloc bits 的起始地址
	for i := uintptr(0); i < uintptr(s.nelems); i++ { // 遍历 span 中的所有元素
		addr := s.base() + i*s.elemsize                       // 计算第 i 个元素的地址
		print(hex(addr))                                      // 打印地址
		alloc := i < uintptr(s.freeindex) || abits.isMarked() // 判断该元素是否已分配
		if alloc {
			print(" alloc") // 打印 "alloc"
		} else {
			print(" free ") // 打印 "free"
		}
		if mbits.isMarked() { // 判断该元素是否被标记
			print(" marked  ") // 打印 "marked"
		} else {
			print(" unmarked") // 打印 "unmarked"
		}
		zombie := mbits.isMarked() && !alloc // 判断该元素是否是 zombie object (被标记但未分配)
		if zombie {
			print(" zombie") // 打印 "zombie"
		}
		print("\n") // 换行
		if zombie { // 如果是 zombie object
			length := s.elemsize // 获取元素大小
			if length > 1024 {   // 如果元素大小大于 1024
				length = 1024 // 则截断为 1024
			}
			hexdumpWords(addr, addr+length, nil) // 打印该地址的十六进制内容
		}
		mbits.advance() // mark bits 指针前进一位
		abits.advance() // alloc bits 指针前进一位
	}
	throw("found pointer to free object") // 抛出异常，表示找到了指向已释放对象的指针
}

// deductSweepCredit deducts sweep credit for allocating a span of
// size spanBytes. This must be performed *before* the span is
// allocated to ensure the system has enough credit. If necessary, it
// performs sweeping to prevent going in to debt. If the caller will
// also sweep pages (e.g., for a large allocation), it can pass a
// non-zero callerSweepPages to leave that many pages unswept.
//
// deductSweepCredit makes a worst-case assumption that all spanBytes
// bytes of the ultimately allocated span will be available for object
// allocation.
//
// deductSweepCredit is the core of the "proportional sweep" system.
// It uses statistics gathered by the garbage collector to perform
// enough sweeping so that all pages are swept during the concurrent
// sweep phase between GC cycles.
//
// mheap_ must NOT be locked.
// deductSweepCredit 用于扣除分配大小为 spanBytes 的 span 的 sweep 信用。
// 这必须在 span 被分配 *之前* 执行，以确保系统有足够的信用。
// 如果必要，它会执行 sweeping 以防止陷入 debt（负债）。
// 如果调用者也将 sweep 页面（例如，对于大型分配），它可以传递一个非零的 callerSweepPages，
// 以留下那么多未 sweep 的页面。
//
// deductSweepCredit 做出最坏情况的假设，即最终分配的 span 的所有 spanBytes 字节都可用于对象分配。
//
// deductSweepCredit 是 "proportional sweep"（比例扫描）系统的核心。
// 它使用垃圾收集器收集的统计信息来执行足够的 sweeping，以便在 GC 周期之间的并发 sweep 阶段 sweep 所有页面。
//
// mheap_ 绝对不能被锁定。
func deductSweepCredit(spanBytes uintptr, callerSweepPages uintptr) {
	if mheap_.sweepPagesPerByte == 0 {
		// Proportional sweep is done or disabled.
		// 如果比例扫描已完成或禁用，则直接返回。
		return
	}

	trace := traceAcquire()
	if trace.ok() {
		trace.GCSweepStart()
		traceRelease(trace)
	}

	// Fix debt if necessary.
	// 必要时修复 debt（负债）。
retry:
	sweptBasis := mheap_.pagesSweptBasis.Load() // 获取已扫描页面的基数
	live := gcController.heapLive.Load()        // 获取堆的 live 大小
	liveBasis := mheap_.sweepHeapLiveBasis      // 获取扫描时的堆 live 基数
	newHeapLive := spanBytes                    // 新分配的 span 大小
	if liveBasis < live {
		// Only do this subtraction when we don't overflow. Otherwise, pagesTarget
		// might be computed as something really huge, causing us to get stuck
		// sweeping here until the next mark phase.
		//
		// Overflow can happen here if gcPaceSweeper is called concurrently with
		// sweeping (i.e. not during a STW, like it usually is) because this code
		// is intentionally racy. A concurrent call to gcPaceSweeper can happen
		// if a GC tuning parameter is modified and we read an older value of
		// heapLive than what was used to set the basis.
		//
		// This state should be transient, so it's fine to just let newHeapLive
		// be a relatively small number. We'll probably just skip this attempt to
		// sweep.
		//
		// See issue #57523.
		// 只有当我们不溢出时才进行此减法。否则，pagesTarget 可能会被计算为一个非常大的数字，
		// 导致我们陷入 sweeping 阶段，直到下一个标记阶段。
		//
		// 如果 gcPaceSweeper 与 sweeping 并发调用（即，不是在 STW 期间，像通常那样），
		// 则可能发生溢出，因为此代码有意存在竞争。如果修改了 GC 调整参数，并且我们读取了比用于设置基数的 heapLive 更旧的值，
		// 则可能发生并发调用 gcPaceSweeper。
		//
		// 这种状态应该是短暂的，所以让 newHeapLive 成为一个相对较小的数字是可以的。
		// 我们可能只会跳过这次 sweeping 尝试。
		//
		// 参见 issue #57523.
		newHeapLive += uintptr(live - liveBasis) // 计算新的堆 live 大小
	}
	pagesTarget := int64(mheap_.sweepPagesPerByte*float64(newHeapLive)) - int64(callerSweepPages) // 计算目标扫描页面数
	for pagesTarget > int64(mheap_.pagesSwept.Load()-sweptBasis) {                                // 如果目标扫描页面数大于当前已扫描页面数
		if sweepone() == ^uintptr(0) { // 执行一次 sweep
			mheap_.sweepPagesPerByte = 0 // 如果 sweep 失败，则禁用比例扫描
			break
		}
		if mheap_.pagesSweptBasis.Load() != sweptBasis { // 如果扫描基数发生变化
			// Sweep pacing changed. Recompute debt.
			goto retry // 重新计算 debt
		}
	}

	trace = traceAcquire()
	if trace.ok() {
		trace.GCSweepDone()
		traceRelease(trace)
	}
}

// clobberfree sets the memory content at x to bad content, for debugging
// purposes.
// clobberfree 函数用于调试目的，将 x 指向的内存内容设置为坏内容。
func clobberfree(x unsafe.Pointer, size uintptr) {
	// size (span.elemsize) is always a multiple of 4.
	// size (span.elemsize) 总是 4 的倍数。
	for i := uintptr(0); i < size; i += 4 {
		*(*uint32)(add(x, i)) = 0xdeadbeef
	}
}

// gcPaceSweeper updates the sweeper's pacing parameters.
//
// Must be called whenever the GC's pacing is updated.
//
// The world must be stopped, or mheap_.lock must be held.
// gcPaceSweeper 函数更新 sweeper 的步调参数。
//
// 必须在每次 GC 的步调更新时调用。
//
// 必须停止 world，或者持有 mheap_.lock。
func gcPaceSweeper(trigger uint64) {
	assertWorldStoppedOrLockHeld(&mheap_.lock) // 确保 world 停止或持有 mheap_.lock

	// Update sweep pacing.
	// 更新 sweep 步调。
	if isSweepDone() { // 如果 sweep 完成
		mheap_.sweepPagesPerByte = 0 // 设置 sweepPagesPerByte 为 0
	} else {
		// Concurrent sweep needs to sweep all of the in-use
		// pages by the time the allocated heap reaches the GC
		// trigger. Compute the ratio of in-use pages to sweep
		// per byte allocated, accounting for the fact that
		// some might already be swept.
		// 并发 sweep 需要在分配的堆达到 GC 触发器时 sweep 所有正在使用的页面。
		// 计算每个分配字节要 sweep 的正在使用页面的比率，考虑到某些页面可能已经被 sweep。
		heapLiveBasis := gcController.heapLive.Load()         // 获取堆 live 大小基数
		heapDistance := int64(trigger) - int64(heapLiveBasis) // 计算堆距离
		// Add a little margin so rounding errors and
		// concurrent sweep are less likely to leave pages
		// unswept when GC starts.
		// 添加一点余量，以便在 GC 启动时，舍入误差和并发 sweep 不太可能留下未 sweep 的页面。
		heapDistance -= 1024 * 1024   // 减去 1MB 的余量
		if heapDistance < _PageSize { // 如果堆距离小于页大小
			// Avoid setting the sweep ratio extremely high
			// 避免将 sweep 比率设置得过高
			heapDistance = _PageSize // 设置堆距离为页大小
		}
		pagesSwept := mheap_.pagesSwept.Load()                      // 获取已 sweep 的页面数
		pagesInUse := mheap_.pagesInUse.Load()                      // 获取正在使用的页面数
		sweepDistancePages := int64(pagesInUse) - int64(pagesSwept) // 计算 sweep 距离页面数
		if sweepDistancePages <= 0 {                                // 如果 sweep 距离页面数小于等于 0
			mheap_.sweepPagesPerByte = 0 // 设置 sweepPagesPerByte 为 0
		} else {
			mheap_.sweepPagesPerByte = float64(sweepDistancePages) / float64(heapDistance) // 计算 sweepPagesPerByte
			mheap_.sweepHeapLiveBasis = heapLiveBasis                                      // 设置 sweepHeapLiveBasis
			// Write pagesSweptBasis last, since this
			// signals concurrent sweeps to recompute
			// their debt.
			// 最后写入 pagesSweptBasis，因为这会通知并发 sweep 重新计算其 debt。
			mheap_.pagesSweptBasis.Store(pagesSwept) // 存储已 sweep 的页面数基数
		}
	}
}
