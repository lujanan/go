// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/goarch"
	"internal/runtime/atomic"
	"runtime/internal/sys"
	"unsafe"
)

const (
	_WorkbufSize = 2048 // in bytes; larger values result in less contention
	// _WorkbufSize 工作缓冲区的大小，以字节为单位；较大的值可以减少竞争
	// 译注：workbuf是用于GC并发标记的工作缓冲区，用于存放待处理的灰色对象指针。
	//      灰色对象是指已被标记，但其内部指针尚未扫描的对象。
	//      增大workbuf的大小可以减少多个GC worker并发访问全局work list的频率，从而减少竞争。

	// workbufAlloc is the number of bytes to allocate at a time
	// for new workbufs. This must be a multiple of pageSize and
	// should be a multiple of _WorkbufSize.
	//
	// Larger values reduce workbuf allocation overhead. Smaller
	// values reduce heap fragmentation.
	workbufAlloc = 32 << 10
	// workbufAlloc 每次为新的 workbuf 分配的字节数。
	// 这个值必须是 pageSize 的倍数，并且应该是 _WorkbufSize 的倍数。
	// 较大的值会减少 workbuf 的分配开销。
	// 较小的值会减少堆碎片。
	// 译注：pageSize是操作系统页面大小，通常为4KB。
	//      workbufAlloc越大，分配次数越少，但可能导致更多的内存浪费。
	//      workbufAlloc越小，分配次数越多，但可以更有效地利用内存。
)

func init() {
	if workbufAlloc%pageSize != 0 || workbufAlloc%_WorkbufSize != 0 {
		throw("bad workbufAlloc")
	}
	// init 函数用于检查 workbufAlloc 的值是否有效。
	// 如果 workbufAlloc 不是 pageSize 或 _WorkbufSize 的倍数，则抛出异常。
	// 译注：这里确保workbufAlloc能够被完整地分割成页或workbuf，避免内存对齐问题。
}

// Garbage collector work pool abstraction.
//
// This implements a producer/consumer model for pointers to grey
// objects. A grey object is one that is marked and on a work
// queue. A black object is marked and not on a work queue.
//
// Write barriers, root discovery, stack scanning, and object scanning
// produce pointers to grey objects. Scanning consumes pointers to
// grey objects, thus blackening them, and then scans them,
// potentially producing new pointers to grey objects.

// A gcWork provides the interface to produce and consume work for the
// garbage collector.
//
// A gcWork can be used on the stack as follows:
//
//	(preemption must be disabled)
//	gcw := &getg().m.p.ptr().gcw
//	.. call gcw.put() to produce and gcw.tryGet() to consume ..
//
// It's important that any use of gcWork during the mark phase prevent
// the garbage collector from transitioning to mark termination since
// gcWork may locally hold GC work buffers. This can be done by
// disabling preemption (systemstack or acquirem).

// 垃圾收集器工作池抽象。
//
// 这为灰色对象的指针实现了一个生产者/消费者模型。
// 灰色对象是指已标记且在工作队列中的对象。
// 黑色对象是指已标记但不在工作队列中的对象。
//
// 写屏障、根发现、栈扫描和对象扫描会产生指向灰色对象的指针。
// 扫描会消耗指向灰色对象的指针，从而将它们变黑，然后扫描它们，
// 可能会产生指向新的灰色对象的指针。

// gcWork 提供了为垃圾收集器生产和消费工作的接口。
//
// gcWork 可以像这样在栈上使用：
//
//	(必须禁用抢占)
//	gcw := &getg().m.p.ptr().gcw
//	.. 调用 gcw.put() 来生产，调用 gcw.tryGet() 来消费 ..
//
// 重要的是，在标记阶段使用 gcWork 必须阻止垃圾收集器转换到标记终止阶段，
// 因为 gcWork 可能会在本地保存 GC 工作缓冲区。
// 这可以通过禁用抢占（systemstack 或 acquirem）来完成。
type gcWork struct {
	// wbuf1 and wbuf2 are the primary and secondary work buffers.
	// wbuf1 和 wbuf2 是主要的和次要的工作缓冲区。
	//
	// This can be thought of as a stack of both work buffers'
	// pointers concatenated. When we pop the last pointer, we
	// shift the stack up by one work buffer by bringing in a new
	// full buffer and discarding an empty one. When we fill both
	// buffers, we shift the stack down by one work buffer by
	// bringing in a new empty buffer and discarding a full one.
	// This way we have one buffer's worth of hysteresis, which
	// amortizes the cost of getting or putting a work buffer over
	// at least one buffer of work and reduces contention on the
	// global work lists.
	// 这可以被认为是一个由两个工作缓冲区的指针连接而成的栈。
	// 当我们弹出最后一个指针时，我们通过引入一个新的满缓冲区并丢弃一个空缓冲区，
	// 将栈向上移动一个工作缓冲区。当我们填满两个缓冲区时，我们通过引入一个新的空缓冲区并丢弃一个满缓冲区，
	// 将栈向下移动一个工作缓冲区。这样，我们就有一个缓冲区滞后，
	// 这分摊了获取或放置工作缓冲区的成本，至少在一个缓冲区的工作上，并减少了全局工作列表上的争用。
	//
	// wbuf1 is always the buffer we're currently pushing to and
	// popping from and wbuf2 is the buffer that will be discarded
	// next.
	// wbuf1 始终是我们当前正在推入和弹出的缓冲区，wbuf2 是下一个要丢弃的缓冲区。
	//
	// Invariant: Both wbuf1 and wbuf2 are nil or neither are.
	// 不变式：wbuf1 和 wbuf2 要么都为 nil，要么都不为 nil。
	wbuf1, wbuf2 *workbuf

	// Bytes marked (blackened) on this gcWork. This is aggregated
	// into work.bytesMarked by dispose.
	// bytesMarked 记录了在此 gcWork 上标记（染黑）的字节数。
	// 这些字节数通过 dispose 函数累加到 work.bytesMarked 中。
	bytesMarked uint64

	// Heap scan work performed on this gcWork. This is aggregated into
	// gcController by dispose and may also be flushed by callers.
	// Other types of scan work are flushed immediately.
	// heapScanWork 记录了在此 gcWork 上执行的堆扫描工作量。
	// 这些工作量通过 dispose 函数累加到 gcController 中，
	// 也可以由调用者刷新。其他类型的扫描工作会立即刷新。
	heapScanWork int64

	// flushedWork indicates that a non-empty work buffer was
	// flushed to the global work list since the last gcMarkDone
	// termination check. Specifically, this indicates that this
	// gcWork may have communicated work to another gcWork.
	// flushedWork 表示自上次 gcMarkDone 终止检查以来，
	// 一个非空的工作缓冲区已刷新到全局工作列表中。
	// 具体来说，这表明此 gcWork 可能已将工作传递给另一个 gcWork。
	flushedWork bool
}

// Most of the methods of gcWork are go:nowritebarrierrec because the
// write barrier itself can invoke gcWork methods but the methods are
// not generally re-entrant. Hence, if a gcWork method invoked the
// write barrier while the gcWork was in an inconsistent state, and
// the write barrier in turn invoked a gcWork method, it could
// permanently corrupt the gcWork.
//
// gcWork 的大多数方法都使用了 go:nowritebarrierrec，
// 因为写屏障本身可以调用 gcWork 的方法，但这些方法通常不是可重入的。
// 因此，如果一个 gcWork 方法在 gcWork 处于不一致状态时调用了写屏障，
// 而写屏障反过来又调用了一个 gcWork 方法，这可能会永久损坏 gcWork。

func (w *gcWork) init() {
	// Initialize the work buffers for this gcWork.
	// 初始化此 gcWork 的工作缓冲区。
	w.wbuf1 = getempty()
	// Try to get a full work buffer from the global queue.
	// 尝试从全局队列中获取一个已满的工作缓冲区。
	wbuf2 := trygetfull()
	// If there are no full work buffers available, get an empty one.
	// 如果没有可用的已满工作缓冲区，则获取一个空的工作缓冲区。
	if wbuf2 == nil {
		wbuf2 = getempty()
	}
	w.wbuf2 = wbuf2
}

// put enqueues a pointer for the garbage collector to trace.
// obj must point to the beginning of a heap object or an oblet.
// obj 必须指向堆对象或 oblet 的开头。
//
//go:nowritebarrierrec
func (w *gcWork) put(obj uintptr) {
	flushed := false
	wbuf := w.wbuf1 // 获取当前工作缓冲区
	// Record that this may acquire the wbufSpans or heap lock to
	// allocate a workbuf.
	lockWithRankMayAcquire(&work.wbufSpans.lock, lockRankWbufSpans) // 记录可能获取 wbufSpans 或堆锁以分配 workbuf
	lockWithRankMayAcquire(&mheap_.lock, lockRankMheap)             // 记录可能获取 mheap 锁
	if wbuf == nil {                                                // 如果当前工作缓冲区为空
		w.init()       // 初始化 gcWork，获取新的工作缓冲区
		wbuf = w.wbuf1 // 重新获取工作缓冲区
		// wbuf is empty at this point.
		// wbuf 在此时是空的。
	} else if wbuf.nobj == len(wbuf.obj) { // 如果当前工作缓冲区已满
		w.wbuf1, w.wbuf2 = w.wbuf2, w.wbuf1 // 交换 wbuf1 和 wbuf2，将已满的缓冲区移到 wbuf2
		wbuf = w.wbuf1                      // 重新获取工作缓冲区 (wbuf1)
		if wbuf.nobj == len(wbuf.obj) {     // 如果交换后的 wbuf1 仍然是满的 (说明 wbuf2 也是满的)
			putfull(wbuf)        // 将已满的缓冲区放入全局队列
			w.flushedWork = true // 标记已刷新工作
			wbuf = getempty()    // 获取一个新的空工作缓冲区
			w.wbuf1 = wbuf       // 将新的空缓冲区赋值给 wbuf1
			flushed = true       // 标记已刷新
		}
	}

	wbuf.obj[wbuf.nobj] = obj // 将对象指针放入工作缓冲区
	wbuf.nobj++               // 增加工作缓冲区中的对象数量

	// If we put a buffer on full, let the GC controller know so
	// it can encourage more workers to run. We delay this until
	// the end of put so that w is in a consistent state, since
	// enlistWorker may itself manipulate w.
	// 如果我们将一个缓冲区放满了，让 GC 控制器知道，这样它可以鼓励更多的工作者运行。
	// 我们将此操作延迟到 put 的末尾，以便 w 处于一致的状态，因为 enlistWorker 本身可能会操作 w。
	if flushed && gcphase == _GCmark {
		gcController.enlistWorker() // 通知 GC 控制器，可能有更多工作需要处理
	}
}

// putFast does a put and reports whether it can be done quickly
// otherwise it returns false and the caller needs to call put.
// putFast 执行一个 put 操作，并报告它是否可以快速完成。
// 否则返回 false，调用者需要调用 put。
//
//go:nowritebarrierrec
func (w *gcWork) putFast(obj uintptr) bool {
	wbuf := w.wbuf1                                // 获取当前工作缓冲区
	if wbuf == nil || wbuf.nobj == len(wbuf.obj) { // 如果当前工作缓冲区为空或者已满
		return false // 返回 false，表示不能快速完成
	}

	wbuf.obj[wbuf.nobj] = obj // 将对象指针放入工作缓冲区
	wbuf.nobj++               // 增加工作缓冲区中的对象数量
	return true               // 返回 true，表示可以快速完成
}

// putBatch performs a put on every pointer in obj. See put for
// constraints on these pointers.
// putBatch 对 obj 中的每个指针执行 put 操作。有关这些指针的约束，请参见 put。
//
//go:nowritebarrierrec
func (w *gcWork) putBatch(obj []uintptr) {
	if len(obj) == 0 {
		return
	}

	flushed := false
	wbuf := w.wbuf1 // 获取当前工作缓冲区
	if wbuf == nil {
		w.init()       // 初始化 gcWork，获取新的工作缓冲区
		wbuf = w.wbuf1 // 重新获取工作缓冲区
	}

	for len(obj) > 0 { // 循环处理 obj 中的所有指针
		for wbuf.nobj == len(wbuf.obj) { // 如果当前工作缓冲区已满
			putfull(wbuf)                          // 将已满的缓冲区放入全局队列
			w.flushedWork = true                   // 标记已刷新工作
			w.wbuf1, w.wbuf2 = w.wbuf2, getempty() // 交换 wbuf1 和 wbuf2，并获取一个新的空缓冲区
			wbuf = w.wbuf1                         // 重新获取工作缓冲区 (wbuf1)
			flushed = true                         // 标记已刷新
		}
		n := copy(wbuf.obj[wbuf.nobj:], obj) // 将 obj 中的指针复制到工作缓冲区
		wbuf.nobj += n                       // 增加工作缓冲区中的对象数量
		obj = obj[n:]                        // 更新 obj，指向未处理的指针
	}

	if flushed && gcphase == _GCmark { // 如果刷新了缓冲区，并且当前处于标记阶段
		gcController.enlistWorker() // 通知 GC 控制器，可能有更多工作需要处理
	}
}

// tryGet dequeues a pointer for the garbage collector to trace.
//
// If there are no pointers remaining in this gcWork or in the global
// queue, tryGet returns 0.  Note that there may still be pointers in
// other gcWork instances or other caches.
//
// tryGet 从队列中取出一个指针，供垃圾回收器跟踪。
//
// 如果此 gcWork 或全局队列中没有剩余指针，tryGet 返回 0。
// 请注意，其他 gcWork 实例或其他缓存中可能仍然存在指针。
//
//go:nowritebarrierrec
func (w *gcWork) tryGet() uintptr {
	wbuf := w.wbuf1  // 获取当前工作缓冲区
	if wbuf == nil { // 如果当前工作缓冲区为空
		w.init()       // 初始化 gcWork，获取新的工作缓冲区
		wbuf = w.wbuf1 // 重新获取工作缓冲区
		// wbuf is empty at this point.
		// 此时 wbuf 为空。
	}
	if wbuf.nobj == 0 { // 如果当前工作缓冲区中没有对象
		w.wbuf1, w.wbuf2 = w.wbuf2, w.wbuf1 // 交换 wbuf1 和 wbuf2，尝试使用备用缓冲区
		wbuf = w.wbuf1                      // 重新获取工作缓冲区
		if wbuf.nobj == 0 {                 // 如果备用缓冲区也为空
			owbuf := wbuf       // 暂存当前空缓冲区
			wbuf = trygetfull() // 尝试从全局队列中获取一个已满的缓冲区
			if wbuf == nil {    // 如果全局队列中也没有已满的缓冲区
				return 0 // 返回 0，表示没有可用的指针
			}
			putempty(owbuf) // 将之前暂存的空缓冲区放回空闲列表
			w.wbuf1 = wbuf  // 将从全局队列获取的缓冲区设置为当前工作缓冲区
		}
	}

	wbuf.nobj--                // 减少工作缓冲区中的对象数量
	return wbuf.obj[wbuf.nobj] // 返回工作缓冲区中的下一个对象指针
}

// tryGetFast dequeues a pointer for the garbage collector to trace
// if one is readily available. Otherwise it returns 0 and
// the caller is expected to call tryGet().
// tryGetFast 从当前工作缓冲区中快速出队一个指针，供垃圾回收器跟踪。
// 如果指针立即可用，则返回该指针。否则，返回 0，并期望调用者调用 tryGet()。
//
//go:nowritebarrierrec
func (w *gcWork) tryGetFast() uintptr {
	wbuf := w.wbuf1                    // 获取当前工作缓冲区
	if wbuf == nil || wbuf.nobj == 0 { // 如果当前工作缓冲区为空或者没有对象
		return 0 // 返回 0，表示没有可用的指针
	}

	wbuf.nobj--                // 减少工作缓冲区中的对象数量
	return wbuf.obj[wbuf.nobj] // 返回工作缓冲区中的下一个对象指针
}

// dispose returns any cached pointers to the global queue.
// The buffers are being put on the full queue so that the
// write barriers will not simply reacquire them before the
// GC can inspect them. This helps reduce the mutator's
// ability to hide pointers during the concurrent mark phase.
// dispose 将任何缓存的指针返回到全局队列。
// 这些缓冲区被放入 full 队列，以便写屏障不会在 GC 检查它们之前简单地重新获取它们。
// 这有助于减少 mutator 在并发标记阶段隐藏指针的能力。
//
//go:nowritebarrierrec
func (w *gcWork) dispose() {
	if wbuf := w.wbuf1; wbuf != nil { // 如果 wbuf1 不为空
		if wbuf.nobj == 0 { // 如果 wbuf1 中没有对象
			putempty(wbuf) // 将 wbuf1 放回空闲列表
		} else {
			putfull(wbuf)        // 将 wbuf1 放入已满列表
			w.flushedWork = true // 标记已刷新工作
		}
		w.wbuf1 = nil // 将 wbuf1 设置为空
	}

	wbuf := w.wbuf2  // 获取 wbuf2
	if wbuf != nil { // 如果 wbuf2 不为空
		if wbuf.nobj == 0 { // 如果 wbuf2 中没有对象
			putempty(wbuf) // 将 wbuf2 放回空闲列表
		} else {
			putfull(wbuf)        // 将 wbuf2 放入已满列表
			w.flushedWork = true // 标记已刷新工作
		}
		w.wbuf2 = nil // 将 wbuf2 设置为空
	}
	if w.bytesMarked != 0 {
		// dispose happens relatively infrequently. If this
		// atomic becomes a problem, we should first try to
		// dispose less and if necessary aggregate in a per-P
		// counter.
		// dispose 发生的频率相对较低。如果这个原子操作成为一个问题，
		// 我们应该首先尝试减少 dispose 的次数，并在必要时在每个 P 的计数器中聚合。
		atomic.Xadd64(&work.bytesMarked, int64(w.bytesMarked)) // 将标记的字节数添加到全局计数器
		w.bytesMarked = 0                                      // 重置本地标记的字节数
	}
	if w.heapScanWork != 0 {
		gcController.heapScanWork.Add(w.heapScanWork) // 将堆扫描工作量添加到全局计数器
		w.heapScanWork = 0                            // 重置本地堆扫描工作量
	}
}

// balance moves some work that's cached in this gcWork back on the
// global queue.
// balance 将缓存在此 gcWork 中的一些工作移回全局队列。
//
//go:nowritebarrierrec
func (w *gcWork) balance() {
	if w.wbuf1 == nil { // 如果 wbuf1 为空
		return // 没有工作可平衡，直接返回
	}
	if wbuf := w.wbuf2; wbuf.nobj != 0 { // 如果 wbuf2 不为空
		putfull(wbuf)        // 将 wbuf2 放入已满列表
		w.flushedWork = true // 标记已刷新工作
		w.wbuf2 = getempty() // 从空闲列表获取一个新的空缓冲区并赋值给 wbuf2
	} else if wbuf := w.wbuf1; wbuf.nobj > 4 { // 如果 wbuf1 中的对象数量大于 4
		w.wbuf1 = handoff(wbuf) // 将 wbuf1 中的一半对象移到全局队列，并将剩余对象留在 wbuf1 中
		w.flushedWork = true    // handoff did putfull // 标记已刷新工作，handoff 内部已经调用了 putfull
	} else {
		return // wbuf1 中的对象数量小于等于 4，没有足够的工作可平衡，直接返回
	}
	// We flushed a buffer to the full list, so wake a worker.
	// 我们将一个缓冲区刷新到已满列表，因此唤醒一个 worker。
	if gcphase == _GCmark { // 如果当前是标记阶段
		gcController.enlistWorker() // 增加一个 worker 来处理全局队列中的工作
	}
}

// empty reports whether w has no mark work available.
// empty 报告 w 是否没有可用的标记工作。
//
//go:nowritebarrierrec
func (w *gcWork) empty() bool {
	return w.wbuf1 == nil || (w.wbuf1.nobj == 0 && w.wbuf2.nobj == 0) // 如果 wbuf1 为空，或者 wbuf1 和 wbuf2 都为空，则表示没有可用的标记工作
}

// Internally, the GC work pool is kept in arrays in work buffers.
// The gcWork interface caches a work buffer until full (or empty) to
// avoid contending on the global work buffer lists.
// 内部地，GC工作池保存在工作缓冲区（work buffer）的数组中。
// gcWork接口缓存一个工作缓冲区，直到它满或空，以避免在全局工作缓冲区列表上竞争。
// 这样做是为了减少对全局工作队列的争用，提高并发性能。

type workbufhdr struct {
	node lfnode // must be first
	nobj int    // number of objects in buffer
	// node 必须是第一个字段
	// nobj 缓冲区中的对象数量
}

type workbuf struct {
	_ sys.NotInHeap // mark type as not containing pointers
	workbufhdr
	// account for the above fields
	obj [(_WorkbufSize - unsafe.Sizeof(workbufhdr{})) / goarch.PtrSize]uintptr // array of objects
	// _ sys.NotInHeap 标记类型不包含指针
	// obj 对象数组
}

// workbuf factory routines. These funcs are used to manage the
// workbufs.
// If the GC asks for some work these are the only routines that
// make wbufs available to the GC.
// workbuf 工厂方法。这些函数用于管理 workbuf。
// 如果 GC 请求一些工作，这些是唯一使 wbuf 可用于 GC 的例程。

func (b *workbuf) checknonempty() {
	if b.nobj == 0 {
		throw("workbuf is empty")
	}
	// 检查 workbuf 是否为空
	// 如果为空，则抛出异常
}

func (b *workbuf) checkempty() {
	if b.nobj != 0 {
		throw("workbuf is not empty")
	}
	// 检查 workbuf 是否不为空
	// 如果不为空，则抛出异常
}

// getempty pops an empty work buffer off the work.empty list,
// allocating new buffers if none are available.
//
// getempty 从 work.empty 列表中弹出一个空闲 work buffer，如果没有可用的，则分配新的 buffer。
//
//go:nowritebarrier
func getempty() *workbuf {
	var b *workbuf       // 声明一个 workbuf 类型的指针变量 b，用于存储获取到的空 workbuf
	if work.empty != 0 { // 检查全局的 work.empty 链表是否为空，如果不为空，则表示有可用的空 workbuf
		b = (*workbuf)(work.empty.pop()) // 从 work.empty 链表中弹出一个 workbuf，并将其赋值给 b
		if b != nil {                    // 如果成功从 work.empty 链表中弹出一个 workbuf
			b.checkempty() // 检查弹出的 workbuf 是否确实为空，如果不是空，则抛出异常
		}
	}
	// Record that this may acquire the wbufSpans or heap lock to
	// allocate a workbuf.
	lockWithRankMayAcquire(&work.wbufSpans.lock, lockRankWbufSpans) // 记录：此操作可能会获取 wbufSpans 锁或堆锁来分配 workbuf
	lockWithRankMayAcquire(&mheap_.lock, lockRankMheap)             // 记录：此操作可能会获取 mheap_ 锁
	if b == nil {                                                   // 如果 b 仍然为空，则表示没有可用的空 workbuf，需要分配新的 workbuf
		// Allocate more workbufs.
		var s *mspan                          // 声明一个 mspan 类型的指针变量 s，用于存储分配到的 mspan
		if work.wbufSpans.free.first != nil { // 检查 work.wbufSpans.free 链表是否为空，如果不为空，则表示有可用的空闲 mspan
			lock(&work.wbufSpans.lock)    // 获取 work.wbufSpans.lock 锁，以保护 work.wbufSpans.free 链表
			s = work.wbufSpans.free.first // 从 work.wbufSpans.free 链表中获取第一个 mspan，并将其赋值给 s
			if s != nil {                 // 如果成功从 work.wbufSpans.free 链表中获取一个 mspan
				work.wbufSpans.free.remove(s) // 从 work.wbufSpans.free 链表中移除 s
				work.wbufSpans.busy.insert(s) // 将 s 插入到 work.wbufSpans.busy 链表中，表示该 mspan 正在被使用
			}
			unlock(&work.wbufSpans.lock) // 释放 work.wbufSpans.lock 锁
		}
		if s == nil { // 如果 s 仍然为空，则表示没有可用的空闲 mspan，需要从堆中分配新的 mspan
			systemstack(func() { // 在系统栈上执行一个函数，以避免栈溢出
				s = mheap_.allocManual(workbufAlloc/pageSize, spanAllocWorkBuf) // 从堆中手动分配一个 mspan，大小为 workbufAlloc/pageSize 个页面，类型为 spanAllocWorkBuf
			})
			if s == nil { // 如果分配失败
				throw("out of memory") // 抛出 "out of memory" 异常，表示内存不足
			}
			// Record the new span in the busy list.
			lock(&work.wbufSpans.lock)    // 获取 work.wbufSpans.lock 锁，以保护 work.wbufSpans.busy 链表
			work.wbufSpans.busy.insert(s) // 将 s 插入到 work.wbufSpans.busy 链表中，表示该 mspan 正在被使用
			unlock(&work.wbufSpans.lock)  // 释放 work.wbufSpans.lock 锁
		}
		// Slice up the span into new workbufs. Return one and
		// put the rest on the empty list.
		for i := uintptr(0); i+_WorkbufSize <= workbufAlloc; i += _WorkbufSize { // 遍历分配到的 mspan，将其分割成多个 workbuf
			newb := (*workbuf)(unsafe.Pointer(s.base() + i)) // 创建一个新的 workbuf，其地址为 s.base() + i
			newb.nobj = 0                                    // 将 newb 的 nobj 字段设置为 0，表示该 workbuf 为空
			lfnodeValidate(&newb.node)                       // 验证 newb 的 lfnode 字段，确保其有效
			if i == 0 {                                      // 如果 i 等于 0，则表示这是第一个 workbuf
				b = newb // 将 b 指向 newb，表示将第一个 workbuf 返回给调用者
			} else { // 否则，表示这是后续的 workbuf
				putempty(newb) // 将 newb 放入 work.empty 链表中，以便后续使用
			}
		}
	}
	return b // 返回获取到的空 workbuf
}

// putempty puts a workbuf onto the work.empty list.
// Upon entry this goroutine owns b. The lfstack.push relinquishes ownership.
//
//go:nowritebarrier
func putempty(b *workbuf) {
	b.checkempty()
	work.empty.push(&b.node)
}

// putfull puts the workbuf on the work.full list for the GC.
// putfull accepts partially full buffers so the GC can avoid competing
// with the mutators for ownership of partially full buffers.
//
//go:nowritebarrier
func putfull(b *workbuf) {
	b.checknonempty()
	work.full.push(&b.node)
}

// trygetfull tries to get a full or partially empty workbuffer.
// If one is not immediately available return nil.
//
//go:nowritebarrier
func trygetfull() *workbuf {
	b := (*workbuf)(work.full.pop())
	if b != nil {
		b.checknonempty()
		return b
	}
	return b
}

//go:nowritebarrier
func handoff(b *workbuf) *workbuf {
	// Make new buffer with half of b's pointers.
	b1 := getempty()
	n := b.nobj / 2
	b.nobj -= n
	b1.nobj = n
	memmove(unsafe.Pointer(&b1.obj[0]), unsafe.Pointer(&b.obj[b.nobj]), uintptr(n)*unsafe.Sizeof(b1.obj[0]))

	// Put b on full list - let first half of b get stolen.
	putfull(b)
	return b1
}

// prepareFreeWorkbufs moves busy workbuf spans to free list so they
// can be freed to the heap. This must only be called when all
// workbufs are on the empty list.
func prepareFreeWorkbufs() {
	lock(&work.wbufSpans.lock)
	if work.full != 0 {
		throw("cannot free workbufs when work.full != 0")
	}
	// Since all workbufs are on the empty list, we don't care
	// which ones are in which spans. We can wipe the entire empty
	// list and move all workbuf spans to the free list.
	work.empty = 0
	work.wbufSpans.free.takeAll(&work.wbufSpans.busy)
	unlock(&work.wbufSpans.lock)
}

// freeSomeWbufs frees some workbufs back to the heap and returns
// true if it should be called again to free more.
func freeSomeWbufs(preemptible bool) bool {
	const batchSize = 64 // ~1–2 µs per span.
	lock(&work.wbufSpans.lock)
	if gcphase != _GCoff || work.wbufSpans.free.isEmpty() {
		unlock(&work.wbufSpans.lock)
		return false
	}
	systemstack(func() {
		gp := getg().m.curg
		for i := 0; i < batchSize && !(preemptible && gp.preempt); i++ {
			span := work.wbufSpans.free.first
			if span == nil {
				break
			}
			work.wbufSpans.free.remove(span)
			mheap_.freeManual(span, spanAllocWorkBuf)
		}
	})
	more := !work.wbufSpans.free.isEmpty()
	unlock(&work.wbufSpans.lock)
	return more
}
