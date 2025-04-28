// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/abi"
	"internal/chacha8rand"
	"internal/goarch"
	"internal/runtime/atomic"
	"runtime/internal/sys"
	"unsafe"
)

// defined constants
const (
	// G status
	//
	// Beyond indicating the general state of a G, the G status
	// acts like a lock on the goroutine's stack (and hence its
	// ability to execute user code).
	//
	// If you add to this list, add to the list
	// of "okay during garbage collection" status
	// in mgcmark.go too.
	//
	// TODO(austin): The _Gscan bit could be much lighter-weight.
	// For example, we could choose not to run _Gscanrunnable
	// goroutines found in the run queue, rather than CAS-looping
	// until they become _Grunnable. And transitions like
	// _Gscanwaiting -> _Gscanrunnable are actually okay because
	// they don't affect stack ownership.

	// G 状态
	//
	// 除了表示 G 的一般状态外，G 状态还充当 goroutine 栈的锁
	//（从而控制其执行用户代码的能力）。
	//
	// 如果向此列表添加新状态，也请将其添加到 mgcmark.go 中的
	// "垃圾回收期间允许的状态" 列表中。
	//
	// TODO(austin): _Gscan 位可以更轻量级。
	// 例如，我们可以选择不运行在运行队列中找到的 _Gscanrunnable
	// goroutine，而不是使用 CAS 循环直到它们变为 _Grunnable。
	// 像 _Gscanwaiting -> _Gscanrunnable 这样的转换实际上是没问题的，
	// 因为它们不会影响栈的所有权。

	// _Gidle means this goroutine was just allocated and has not
	// yet been initialized.
	// _Gidle 表示这个 goroutine 刚刚被分配但尚未初始化
	_Gidle = iota // 0

	// _Grunnable means this goroutine is on a run queue. It is
	// not currently executing user code. The stack is not owned.
	// _Grunnable 表示这个 goroutine 在运行队列中。它当前没有执行用户代码。栈未被占用。
	_Grunnable // 1

	// _Grunning means this goroutine may execute user code. The
	// stack is owned by this goroutine. It is not on a run queue.
	// It is assigned an M and a P (g.m and g.m.p are valid).
	// _Grunning 表示这个 goroutine 可以执行用户代码。栈被这个 goroutine 占用。
	// 它不在运行队列中。它被分配了一个 M 和一个 P（g.m 和 g.m.p 是有效的）。
	_Grunning // 2

	// _Gsyscall means this goroutine is executing a system call.
	// It is not executing user code. The stack is owned by this
	// goroutine. It is not on a run queue. It is assigned an M.
	// _Gsyscall 表示这个 goroutine 正在执行系统调用。它没有执行用户代码。
	// 栈被这个 goroutine 占用。它不在运行队列中。它被分配了一个 M。
	_Gsyscall // 3

	// _Gwaiting means this goroutine is blocked in the runtime.
	// It is not executing user code. It is not on a run queue,
	// but should be recorded somewhere (e.g., a channel wait
	// queue) so it can be ready()d when necessary. The stack is
	// not owned *except* that a channel operation may read or
	// write parts of the stack under the appropriate channel
	// lock. Otherwise, it is not safe to access the stack after a
	// goroutine enters _Gwaiting (e.g., it may get moved).
	// _Gwaiting 表示这个 goroutine 在运行时中被阻塞。它没有执行用户代码。
	// 它不在运行队列中，但应该被记录在某个地方（例如，channel 等待队列），
	// 以便在必要时可以被 ready()。除了 channel 操作可能在适当的 channel 锁下
	// 读取或写入部分栈之外，栈未被占用。否则，在 goroutine 进入 _Gwaiting 后
	// 访问栈是不安全的（例如，它可能被移动）。
	_Gwaiting // 4

	// _Gmoribund_unused is currently unused, but hardcoded in gdb
	// scripts.
	// _Gmoribund_unused 当前未使用，但在 gdb 脚本中是硬编码的。
	_Gmoribund_unused // 5

	// _Gdead means this goroutine is currently unused. It may be
	// just exited, on a free list, or just being initialized. It
	// is not executing user code. It may or may not have a stack
	// allocated. The G and its stack (if any) are owned by the M
	// that is exiting the G or that obtained the G from the free
	// list.
	// _Gdead 表示这个 goroutine 当前未被使用。它可能刚刚退出，在空闲列表中，
	// 或者正在初始化。它没有执行用户代码。它可能有也可能没有分配栈。
	// G 和它的栈（如果有的话）由正在退出 G 的 M 或从空闲列表获取 G 的 M 拥有。
	_Gdead // 6

	// _Genqueue_unused is currently unused.
	// _Genqueue_unused 当前未使用。
	_Genqueue_unused // 7

	// _Gcopystack means this goroutine's stack is being moved. It
	// is not executing user code and is not on a run queue. The
	// stack is owned by the goroutine that put it in _Gcopystack.
	// _Gcopystack 表示这个 goroutine 的栈正在被移动。它没有执行用户代码，
	// 也不在运行队列中。栈由将其放入 _Gcopystack 的 goroutine 拥有。
	_Gcopystack // 8

	// _Gpreempted means this goroutine stopped itself for a
	// suspendG preemption. It is like _Gwaiting, but nothing is
	// yet responsible for ready()ing it. Some suspendG must CAS
	// the status to _Gwaiting to take responsibility for
	// ready()ing this G.
	// _Gpreempted 表示这个 goroutine 因为 suspendG 抢占而停止自己。
	// 它类似于 _Gwaiting，但还没有任何东西负责 ready() 它。
	// 某个 suspendG 必须将状态 CAS 到 _Gwaiting 以负责 ready() 这个 G。
	_Gpreempted // 9

	// _Gscan combined with one of the above states other than
	// _Grunning indicates that GC is scanning the stack. The
	// goroutine is not executing user code and the stack is owned
	// by the goroutine that set the _Gscan bit.
	//
	// _Gscanrunning is different: it is used to briefly block
	// state transitions while GC signals the G to scan its own
	// stack. This is otherwise like _Grunning.
	//
	// atomicstatus&~Gscan gives the state the goroutine will
	// return to when the scan completes.
	// _Gscan 与上述除 _Grunning 之外的状态组合表示 GC 正在扫描栈。
	// goroutine 没有执行用户代码，栈由设置 _Gscan 位的 goroutine 拥有。
	//
	// _Gscanrunning 不同：它用于在 GC 信号通知 G 扫描自己的栈时短暂阻塞状态转换。
	// 除此之外，它类似于 _Grunning。
	//
	// atomicstatus&~Gscan 给出 goroutine 在扫描完成时将返回的状态。
	_Gscan          = 0x1000
	_Gscanrunnable  = _Gscan + _Grunnable  // 0x1001
	_Gscanrunning   = _Gscan + _Grunning   // 0x1002
	_Gscansyscall   = _Gscan + _Gsyscall   // 0x1003
	_Gscanwaiting   = _Gscan + _Gwaiting   // 0x1004
	_Gscanpreempted = _Gscan + _Gpreempted // 0x1009
)

const (
	// P status

	// _Pidle means a P is not being used to run user code or the
	// scheduler. Typically, it's on the idle P list and available
	// to the scheduler, but it may just be transitioning between
	// other states.
	//
	// The P is owned by the idle list or by whatever is
	// transitioning its state. Its run queue is empty.
	//
	// _Pidle 表示 P 当前没有被用于运行用户代码或调度器。
	// 通常，它位于空闲 P 列表中，可供调度器使用，但也可能正在其他状态之间转换。
	//
	// P 由空闲列表或正在转换其状态的任何东西拥有。它的运行队列为空。
	_Pidle = iota

	// _Prunning means a P is owned by an M and is being used to
	// run user code or the scheduler. Only the M that owns this P
	// is allowed to change the P's status from _Prunning. The M
	// may transition the P to _Pidle (if it has no more work to
	// do), _Psyscall (when entering a syscall), or _Pgcstop (to
	// halt for the GC). The M may also hand ownership of the P
	// off directly to another M (e.g., to schedule a locked G).
	//
	// _Prunning 表示 P 被一个 M 拥有，并正在用于运行用户代码或调度器。
	// 只有拥有这个 P 的 M 才被允许将 P 的状态从 _Prunning 改变。
	// M 可以将 P 转换为 _Pidle（如果没有更多工作要做）、_Psyscall（当进入系统调用时）
	// 或 _Pgcstop（为了 GC 而停止）。M 也可以直接将 P 的所有权交给另一个 M
	//（例如，调度一个锁定的 G）。
	_Prunning

	// _Psyscall means a P is not running user code. It has
	// affinity to an M in a syscall but is not owned by it and
	// may be stolen by another M. This is similar to _Pidle but
	// uses lightweight transitions and maintains M affinity.
	//
	// Leaving _Psyscall must be done with a CAS, either to steal
	// or retake the P. Note that there's an ABA hazard: even if
	// an M successfully CASes its original P back to _Prunning
	// after a syscall, it must understand the P may have been
	// used by another M in the interim.
	//
	// _Psyscall 表示 P 没有在运行用户代码。它与一个处于系统调用的 M 有亲和性，
	// 但不被该 M 拥有，可能被另一个 M 窃取。这类似于 _Pidle，
	// 但使用轻量级转换并保持 M 亲和性。
	//
	// 离开 _Psyscall 必须使用 CAS 操作，无论是窃取还是重新获取 P。
	// 注意存在 ABA 问题：即使一个 M 在系统调用后成功将其原始 P CAS 回 _Prunning，
	// 它也必须理解在此期间 P 可能已被另一个 M 使用。
	_Psyscall

	// _Pgcstop means a P is halted for STW and owned by the M
	// that stopped the world. The M that stopped the world
	// continues to use its P, even in _Pgcstop. Transitioning
	// from _Prunning to _Pgcstop causes an M to release its P and
	// park.
	//
	// The P retains its run queue and startTheWorld will restart
	// the scheduler on Ps with non-empty run queues.
	//
	// _Pgcstop 表示 P 因为 STW（Stop The World）而停止，并被停止世界的 M 拥有。
	// 停止世界的 M 继续使用其 P，即使在 _Pgcstop 状态下。
	// 从 _Prunning 转换到 _Pgcstop 会导致 M 释放其 P 并暂停。
	//
	// P 保留其运行队列，startTheWorld 将在具有非空运行队列的 Ps 上重新启动调度器。
	_Pgcstop

	// _Pdead means a P is no longer used (GOMAXPROCS shrank). We
	// reuse Ps if GOMAXPROCS increases. A dead P is mostly
	// stripped of its resources, though a few things remain
	// (e.g., trace buffers).
	//
	// _Pdead 表示 P 不再被使用（GOMAXPROCS 缩小）。
	// 如果 GOMAXPROCS 增加，我们会重用 Ps。
	// 一个死亡的 P 大部分资源被剥离，尽管还保留了一些东西（例如，跟踪缓冲区）。
	_Pdead
)

// Mutual exclusion locks.  In the uncontended case,
// as fast as spin locks (just a few user-level instructions),
// but on the contention path they sleep in the kernel.
// A zeroed Mutex is unlocked (no need to initialize each lock).
// Initialization is helpful for static lock ranking, but not required.
//
// 互斥锁。在无竞争的情况下，
// 速度与自旋锁相当（只需几条用户级指令），
// 但在竞争路径上它们会在内核中休眠。
// 零值的 Mutex 是未锁定的（不需要初始化每个锁）。
// 初始化对静态锁排序有帮助，但不是必需的。
type mutex struct {
	// Empty struct if lock ranking is disabled, otherwise includes the lock rank
	// 如果锁排序被禁用则为空结构体，否则包含锁的等级
	lockRankStruct
	// Futex-based impl treats it as uint32 key,
	// while sema-based impl as M* waitm.
	// Used to be a union, but unions break precise GC.
	// 基于 Futex 的实现将其视为 uint32 键，
	// 而基于信号量的实现将其视为 M* waitm。
	// 以前是一个联合体，但联合体会破坏精确 GC。
	key uintptr
}

// sleep and wakeup on one-time events.
// before any calls to notesleep or notewakeup,
// must call noteclear to initialize the Note.
// then, exactly one thread can call notesleep
// and exactly one thread can call notewakeup (once).
// once notewakeup has been called, the notesleep
// will return.  future notesleep will return immediately.
// subsequent noteclear must be called only after
// previous notesleep has returned, e.g. it's disallowed
// to call noteclear straight after notewakeup.
//
// notetsleep is like notesleep but wakes up after
// a given number of nanoseconds even if the event
// has not yet happened.  if a goroutine uses notetsleep to
// wake up early, it must wait to call noteclear until it
// can be sure that no other goroutine is calling
// notewakeup.
//
// notesleep/notetsleep are generally called on g0,
// notetsleepg is similar to notetsleep but is called on user g.

// 用于一次性事件的睡眠和唤醒。
// 在调用 notesleep 或 notewakeup 之前，
// 必须调用 noteclear 来初始化 Note。
// 然后，恰好有一个线程可以调用 notesleep，
// 恰好有一个线程可以调用 notewakeup（一次）。
// 一旦调用了 notewakeup，notesleep 就会返回。
// 之后的 notesleep 调用会立即返回。
// 后续的 noteclear 调用必须在前一个 notesleep 返回之后才能调用，
// 例如，不允许在 notewakeup 之后直接调用 noteclear。
//
// notetsleep 类似于 notesleep，但会在指定的纳秒数后唤醒，
// 即使事件尚未发生。如果 goroutine 使用 notetsleep 提前唤醒，
// 它必须等待直到确定没有其他 goroutine 在调用 notewakeup 时才能调用 noteclear。
//
// notesleep/notetsleep 通常在 g0 上调用，
// notetsleepg 类似于 notetsleep，但在用户 goroutine 上调用。
type note struct {
	// Futex-based impl treats it as uint32 key,
	// while sema-based impl as M* waitm.
	// Used to be a union, but unions break precise GC.
	// 基于 Futex 的实现将其视为 uint32 键，
	// 而基于信号量的实现将其视为 M* waitm。
	// 以前是一个联合体，但联合体会破坏精确 GC。
	key uintptr
}

type funcval struct {
	fn uintptr
	// variable-size, fn-specific data here
}

// iface represents a non-empty interface value.
// It contains a pointer to the interface table (itab) and the actual data.
// iface 表示一个非空接口值。
// 它包含一个指向接口表(itab)的指针和实际数据。
type iface struct {
	tab  *itab
	data unsafe.Pointer
}

// eface represents an empty interface value.
// It contains a pointer to the type information and the actual data.
// eface 表示一个空接口值。
// 它包含一个指向类型信息的指针和实际数据。
type eface struct {
	_type *_type
	data  unsafe.Pointer
}

// efaceOf converts an any pointer to an eface pointer.
// It is used to access the underlying type and data of an empty interface.
// efaceOf 将 any 指针转换为 eface 指针。
// 它用于访问空接口的底层类型和数据。
func efaceOf(ep *any) *eface {
	return (*eface)(unsafe.Pointer(ep))
}

// The guintptr, muintptr, and puintptr are all used to bypass write barriers.
// It is particularly important to avoid write barriers when the current P has
// been released, because the GC thinks the world is stopped, and an
// unexpected write barrier would not be synchronized with the GC,
// which can lead to a half-executed write barrier that has marked the object
// but not queued it. If the GC skips the object and completes before the
// queuing can occur, it will incorrectly free the object.
//
// We tried using special assignment functions invoked only when not
// holding a running P, but then some updates to a particular memory
// word went through write barriers and some did not. This breaks the
// write barrier shadow checking mode, and it is also scary: better to have
// a word that is completely ignored by the GC than to have one for which
// only a few updates are ignored.
//
// Gs and Ps are always reachable via true pointers in the
// allgs and allp lists or (during allocation before they reach those lists)
// from stack variables.
//
// Ms are always reachable via true pointers either from allm or
// freem. Unlike Gs and Ps we do free Ms, so it's important that
// nothing ever hold an muintptr across a safe point.

// guintptr、muintptr 和 puintptr 都用于绕过写屏障。
// 当当前 P 被释放时，避免写屏障尤为重要，因为 GC 认为世界已停止，
// 意外的写屏障将不会与 GC 同步，这可能导致写屏障执行了一半，
// 标记了对象但未将其加入队列。如果 GC 跳过该对象并在队列操作发生前完成，
// 它将错误地释放该对象。
//
// 我们尝试使用仅在未持有运行中的 P 时调用的特殊赋值函数，
// 但随后对特定内存字的某些更新会经过写屏障，而有些则不会。
// 这破坏了写屏障影子检查模式，而且也很可怕：
// 最好有一个完全被 GC 忽略的字，而不是一个只有部分更新被忽略的字。
//
// Gs 和 Ps 总是可以通过 allgs 和 allp 列表中的真实指针访问，
// 或者在它们到达这些列表之前通过栈变量访问。
//
// Ms 总是可以通过 allm 或 freem 中的真实指针访问。
// 与 Gs 和 Ps 不同，我们会释放 Ms，所以重要的是
// 永远不要跨安全点持有 muintptr。

// A guintptr holds a goroutine pointer, but typed as a uintptr
// to bypass write barriers. It is used in the Gobuf goroutine state
// and in scheduling lists that are manipulated without a P.
//
// The Gobuf.g goroutine pointer is almost always updated by assembly code.
// In one of the few places it is updated by Go code - func save - it must be
// treated as a uintptr to avoid a write barrier being emitted at a bad time.
// Instead of figuring out how to emit the write barriers missing in the
// assembly manipulation, we change the type of the field to uintptr,
// so that it does not require write barriers at all.
//
// Goroutine structs are published in the allg list and never freed.
// That will keep the goroutine structs from being collected.
// There is never a time that Gobuf.g's contain the only references
// to a goroutine: the publishing of the goroutine in allg comes first.
// Goroutine pointers are also kept in non-GC-visible places like TLS,
// so I can't see them ever moving. If we did want to start moving data
// in the GC, we'd need to allocate the goroutine structs from an
// alternate arena. Using guintptr doesn't make that problem any worse.
// Note that pollDesc.rg, pollDesc.wg also store g in uintptr form,
// so they would need to be updated too if g's start moving.

// guintptr 持有一个 goroutine 指针，但类型为 uintptr 以绕过写屏障。
// 它用于 Gobuf goroutine 状态和在没有 P 的情况下操作的调度列表中。
//
// Gobuf.g goroutine 指针几乎总是由汇编代码更新。
// 在少数几个由 Go 代码更新的地方之一 - func save - 它必须被视为 uintptr，
// 以避免在不适当的时机发出写屏障。
// 与其弄清楚如何在汇编操作中发出缺失的写屏障，
// 不如将字段类型更改为 uintptr，这样它根本不需要写屏障。
//
// Goroutine 结构体在 allg 列表中发布且永远不会被释放。
// 这将防止 goroutine 结构体被回收。
// Gobuf.g 永远不会是 goroutine 的唯一引用：
// goroutine 在 allg 中的发布总是先发生。
// Goroutine 指针也保存在非 GC 可见的地方，如 TLS，
// 所以我看不到它们会移动。如果我们确实想在 GC 中开始移动数据，
// 我们需要从备用区域分配 goroutine 结构体。
// 使用 guintptr 不会使这个问题变得更糟。
// 注意 pollDesc.rg、pollDesc.wg 也以 uintptr 形式存储 g，
// 所以如果 g 开始移动，它们也需要更新。
type guintptr uintptr

//go:nosplit
func (gp guintptr) ptr() *g { return (*g)(unsafe.Pointer(gp)) }

//go:nosplit
func (gp *guintptr) set(g *g) { *gp = guintptr(unsafe.Pointer(g)) }

//go:nosplit
func (gp *guintptr) cas(old, new guintptr) bool {
	return atomic.Casuintptr((*uintptr)(unsafe.Pointer(gp)), uintptr(old), uintptr(new))
}

//go:nosplit
func (gp *g) guintptr() guintptr {
	return guintptr(unsafe.Pointer(gp))
}

// setGNoWB performs *gp = new without a write barrier.
// For times when it's impractical to use a guintptr.
//
// setGNoWB 执行 *gp = new 操作而不使用写屏障。
// 用于使用 guintptr 不切实际的情况。
//
//go:nosplit
//go:nowritebarrier
func setGNoWB(gp **g, new *g) {
	(*guintptr)(unsafe.Pointer(gp)).set(new)
}

type puintptr uintptr

//go:nosplit
func (pp puintptr) ptr() *p { return (*p)(unsafe.Pointer(pp)) }

//go:nosplit
func (pp *puintptr) set(p *p) { *pp = puintptr(unsafe.Pointer(p)) }

// muintptr is a *m that is not tracked by the garbage collector.
//
// Because we do free Ms, there are some additional constrains on
// muintptrs:
//
//  1. Never hold an muintptr locally across a safe point.
//
//  2. Any muintptr in the heap must be owned by the M itself so it can
//     ensure it is not in use when the last true *m is released.
//
// muintptr 是一个不被垃圾回收器跟踪的 *m 指针。
//
// 因为我们会释放 M，所以对 muintptr 有一些额外的约束：
//
//  1. 永远不要在安全点（safe point）期间在本地持有 muintptr。
//
//  2. 堆中的任何 muintptr 必须由 M 本身拥有，这样它才能确保
//     在最后一个真正的 *m 被释放时它没有被使用。
type muintptr uintptr

//go:nosplit
func (mp muintptr) ptr() *m { return (*m)(unsafe.Pointer(mp)) }

//go:nosplit
func (mp *muintptr) set(m *m) { *mp = muintptr(unsafe.Pointer(m)) }

// setMNoWB performs *mp = new without a write barrier.
// For times when it's impractical to use an muintptr.
//
// setMNoWB 执行 *mp = new 操作而不使用写屏障。
// 用于使用 muintptr 不切实际的情况。
//
//go:nosplit
//go:nowritebarrier
func setMNoWB(mp **m, new *m) {
	(*muintptr)(unsafe.Pointer(mp)).set(new)
}

type gobuf struct {
	// The offsets of sp, pc, and g are known to (hard-coded in) libmach.
	//
	// ctxt is unusual with respect to GC: it may be a
	// heap-allocated funcval, so GC needs to track it, but it
	// needs to be set and cleared from assembly, where it's
	// difficult to have write barriers. However, ctxt is really a
	// saved, live register, and we only ever exchange it between
	// the real register and the gobuf. Hence, we treat it as a
	// root during stack scanning, which means assembly that saves
	// and restores it doesn't need write barriers. It's still
	// typed as a pointer so that any other writes from Go get
	// write barriers.

	// sp、pc 和 g 的偏移量在 libmach 中是已知的（硬编码的）。
	//
	// ctxt 在 GC 方面比较特殊：它可能是一个堆分配的 funcval，
	// 所以 GC 需要跟踪它，但它需要在汇编中设置和清除，在那里很难使用写屏障。
	// 然而，ctxt 实际上是一个保存的、活动的寄存器，我们只在真实的寄存器和 gobuf 之间交换它。
	// 因此，我们在栈扫描时将其视为根，这意味着保存和恢复它的汇编代码不需要写屏障。
	// 它仍然被类型化为指针，这样任何来自 Go 的其他写入都会获得写屏障。

	sp   uintptr        // 栈指针
	pc   uintptr        // 程序计数器
	g    guintptr       // 当前 goroutine
	ctxt unsafe.Pointer // 上下文指针，用于保存函数调用上下文
	ret  uintptr        // 返回值
	lr   uintptr        // 链接寄存器
	bp   uintptr        // 基址指针，用于支持帧指针的架构
}

// sudog (pseudo-g) represents a g in a wait list, such as for sending/receiving
// on a channel.
//
// sudog is necessary because the g ↔ synchronization object relation
// is many-to-many. A g can be on many wait lists, so there may be
// many sudogs for one g; and many gs may be waiting on the same
// synchronization object, so there may be many sudogs for one object.
//
// sudogs are allocated from a special pool. Use acquireSudog and
// releaseSudog to allocate and free them.

// sudog（伪goroutine）表示等待列表中的一个goroutine，例如在channel上进行发送/接收操作时。
//
// sudog 是必要的，因为 goroutine 和同步对象之间的关系是多对多的。
// 一个 goroutine 可以位于多个等待列表中，因此一个 goroutine 可能对应多个 sudog；
// 同时，多个 goroutine 可能等待同一个同步对象，因此一个对象也可能对应多个 sudog。
//
// sudog 是从一个特殊的池中分配的。使用 acquireSudog 和 releaseSudog 来分配和释放它们。
type sudog struct {
	// The following fields are protected by the hchan.lock of the
	// channel this sudog is blocking on. shrinkstack depends on
	// this for sudogs involved in channel ops.
	// 以下字段受此 sudog 阻塞的 channel 的 hchan.lock 保护。
	// 对于涉及 channel 操作的 sudog，shrinkstack 依赖于此。

	g *g // 关联的 goroutine

	next *sudog         // 等待队列中的下一个 sudog
	prev *sudog         // 等待队列中的前一个 sudog
	elem unsafe.Pointer // data element (may point to stack)
	// 数据元素（可能指向栈）

	// The following fields are never accessed concurrently.
	// For channels, waitlink is only accessed by g.
	// For semaphores, all fields (including the ones above)
	// are only accessed when holding a semaRoot lock.
	// 以下字段永远不会被并发访问。
	// 对于 channel，waitlink 只能被 g 访问。
	// 对于信号量，所有字段（包括上面的字段）只有在持有 semaRoot 锁时才能访问。

	acquiretime int64  // 获取锁的时间
	releasetime int64  // 释放锁的时间
	ticket      uint32 // 用于信号量的票据

	// isSelect indicates g is participating in a select, so
	// g.selectDone must be CAS'd to win the wake-up race.
	// isSelect 表示 g 正在参与 select 操作，因此必须使用 CAS 操作 g.selectDone 来赢得唤醒竞争。
	isSelect bool

	// success indicates whether communication over channel c
	// succeeded. It is true if the goroutine was awoken because a
	// value was delivered over channel c, and false if awoken
	// because c was closed.
	// success 表示通过 channel c 的通信是否成功。
	// 如果 goroutine 因为通过 channel c 传递了值而被唤醒，则为 true；
	// 如果因为 c 被关闭而被唤醒，则为 false。
	success bool

	// waiters is a count of semaRoot waiting list other than head of list,
	// clamped to a uint16 to fit in unused space.
	// Only meaningful at the head of the list.
	// (If we wanted to be overly clever, we could store a high 16 bits
	// in the second entry in the list.)
	// waiters 是 semaRoot 等待列表中除头部以外的计数，
	// 被限制为 uint16 以适应未使用的空间。
	// 仅在列表头部有意义。
	// （如果我们想要过度聪明，我们可以在列表的第二个条目中存储高 16 位。）
	waiters uint16

	parent *sudog // semaRoot binary tree
	// 父节点，用于 semaRoot 二叉树
	waitlink *sudog // g.waiting list or semaRoot
	// 等待链接，用于 g.waiting 列表或 semaRoot
	waittail *sudog // semaRoot
	// 等待队列尾部，用于 semaRoot
	c *hchan // channel
	// 关联的 channel
}

type libcall struct {
	fn   uintptr // function pointer
	n    uintptr // number of parameters
	args uintptr // parameters
	r1   uintptr // return values
	r2   uintptr // second return value
	err  uintptr // error number
}

// Stack describes a Go execution stack.
// The bounds of the stack are exactly [lo, hi),
// with no implicit data structures on either side.
// Stack 描述了 Go 的执行栈。
// 栈的边界严格定义为 [lo, hi)，两边都没有隐式的数据结构。
type stack struct {
	lo uintptr // 栈的低地址边界
	hi uintptr // 栈的高地址边界
}

// heldLockInfo gives info on a held lock and the rank of that lock
// heldLockInfo 提供了关于已持有锁的信息和该锁的等级
type heldLockInfo struct {
	lockAddr uintptr  // 锁的地址
	rank     lockRank // 锁的等级
}

type g struct {
	// Stack parameters.
	// stack describes the actual stack memory: [stack.lo, stack.hi).
	// stackguard0 is the stack pointer compared in the Go stack growth prologue.
	// It is stack.lo+StackGuard normally, but can be StackPreempt to trigger a preemption.
	// stackguard1 is the stack pointer compared in the //go:systemstack stack growth prologue.
	// It is stack.lo+StackGuard on g0 and gsignal stacks.
	// It is ~0 on other goroutine stacks, to trigger a call to morestackc (and crash).
	// 栈参数。
	// stack 描述了实际的栈内存范围：[stack.lo, stack.hi)。
	// stackguard0 是在 Go 栈增长序言中比较的栈指针。
	// 正常情况下它是 stack.lo+StackGuard，但也可以是 StackPreempt 来触发抢占。
	// stackguard1 是在 //go:systemstack 栈增长序言中比较的栈指针。
	// 在 g0 和 gsignal 栈上它是 stack.lo+StackGuard。
	// 在其他 goroutine 栈上是 ~0，用于触发对 morestackc 的调用（并导致崩溃）。
	// 栈描述符，包含栈的边界信息，偏移量对 runtime/cgo 可见
	stack stack // offset known to runtime/cgo
	// 栈保护指针0，用于栈增长检查，偏移量对 liblink 可见
	stackguard0 uintptr // offset known to liblink
	// 栈保护指针1，用于系统栈增长检查，偏移量对 liblink 可见
	stackguard1 uintptr // offset known to liblink

	// 最内层的 panic 结构，偏移量对 liblink 可见
	_panic *_panic // innermost panic - offset known to liblink
	// 最内层的 defer 结构
	_defer *_defer // innermost defer
	// 当前关联的 m，偏移量对 arm liblink 可见
	m *m // current m; offset known to arm liblink
	// 调度相关的上下文信息
	sched gobuf
	// 系统调用时的栈指针，当状态为 Gsyscall 时，用于垃圾回收
	syscallsp uintptr // if status==Gsyscall, syscallsp = sched.sp to use during gc
	// 系统调用时的程序计数器，当状态为 Gsyscall 时，用于垃圾回收
	syscallpc uintptr // if status==Gsyscall, syscallpc = sched.pc to use during gc
	// 系统调用时的基址指针，当状态为 Gsyscall 时，用于帧指针回溯
	syscallbp uintptr // if status==Gsyscall, syscallbp = sched.bp to use in fpTraceback
	// 栈顶期望的栈指针，用于回溯检查
	stktopsp uintptr // expected sp at top of stack, to check in traceback
	// param is a generic pointer parameter field used to pass
	// values in particular contexts where other storage for the
	// parameter would be difficult to find. It is currently used
	// in four ways:
	// 1. When a channel operation wakes up a blocked goroutine, it sets param to
	//    point to the sudog of the completed blocking operation.
	// 2. By gcAssistAlloc1 to signal back to its caller that the goroutine completed
	//    the GC cycle. It is unsafe to do so in any other way, because the goroutine's
	//    stack may have moved in the meantime.
	// 3. By debugCallWrap to pass parameters to a new goroutine because allocating a
	//    closure in the runtime is forbidden.
	// 4. When a panic is recovered and control returns to the respective frame,
	//    param may point to a savedOpenDeferState.
	// param 是一个通用的指针参数字段，用于在特定上下文中传递值，
	// 这些情况下很难找到其他存储参数的地方。目前它被用于四种场景：
	// 1. 当 channel 操作唤醒一个被阻塞的 goroutine 时，param 被设置为指向
	//    已完成阻塞操作的 sudog。
	// 2. gcAssistAlloc1 使用它来向调用者发出信号，表明 goroutine 已完成
	//    GC 周期。由于 goroutine 的栈可能在此期间移动，用其他方式这样做是不安全的。
	// 3. debugCallWrap 使用它来向新的 goroutine 传递参数，因为在运行时中
	//    禁止分配闭包。
	// 4. 当 panic 被恢复且控制返回到相应的帧时，param 可能指向一个
	//    savedOpenDeferState。
	param        unsafe.Pointer // 通用指针参数，用于在不同场景下传递值
	atomicstatus atomic.Uint32  // 原子操作的状态值，表示 goroutine 的当前状态
	// 栈锁，用于 sigprof 和 scang 操作；TODO：考虑将其合并到 atomicstatus 中
	stackLock uint32   // sigprof/scang lock; TODO: fold in to atomicstatus
	goid      uint64   // goroutine 的唯一标识符
	schedlink guintptr // 调度器链表中的下一个 goroutine
	// 阻塞开始的大致时间戳
	waitsince int64 // approx time when the g become blocked
	// 当状态为 Gwaiting 时的等待原因
	waitreason waitReason // if status==Gwaiting

	// 抢占信号，与 stackguard0 = stackpreempt 重复
	preempt bool // preemption signal, duplicates stackguard0 = stackpreempt
	// 抢占时是否转换为 _Gpreempted 状态；否则仅进行调度
	preemptStop bool // transition to _Gpreempted on preemption; otherwise, just deschedule
	// 是否在同步安全点收缩栈
	preemptShrink bool // shrink stack at synchronous safe point

	// asyncSafePoint is set if g is stopped at an asynchronous
	// safe point. This means there are frames on the stack
	// without precise pointer information.
	// asyncSafePoint 表示 goroutine 是否在异步安全点停止。
	// 这意味着栈上有一些帧没有精确的指针信息。
	asyncSafePoint bool

	// panic (instead of crash) on unexpected fault address
	// 在遇到意外的错误地址时触发 panic（而不是崩溃）
	paniconfault bool

	// g has scanned stack; protected by _Gscan bit in status
	// 表示 goroutine 的栈已经被扫描过；受 _Gscan 状态位保护
	gcscandone bool

	// must not split stack
	// 表示不能分割栈
	throwsplit bool
	// activeStackChans indicates that there are unlocked channels
	// pointing into this goroutine's stack. If true, stack
	// copying needs to acquire channel locks to protect these
	// areas of the stack.
	// activeStackChans 表示是否有未锁定的 channel 指向这个 goroutine 的栈。
	// 如果为 true，栈复制时需要获取 channel 锁来保护这些栈区域。
	activeStackChans bool
	// parkingOnChan indicates that the goroutine is about to
	// park on a chansend or chanrecv. Used to signal an unsafe point
	// for stack shrinking.
	// parkingOnChan 表示 goroutine 即将在 chansend 或 chanrecv 上阻塞。
	// 用于标记栈收缩的不安全点。
	parkingOnChan atomic.Bool
	// inMarkAssist indicates whether the goroutine is in mark assist.
	// Used by the execution tracer.
	// inMarkAssist 表示 goroutine 是否处于标记辅助阶段。
	// 用于执行追踪器。
	inMarkAssist bool
	// coroexit 是 coroswitch_m 的参数
	coroexit bool // argument to coroswitch_m

	// 忽略竞态检测事件
	raceignore int8 // ignore race detection events
	// 是否禁用从 C 语言的回调
	nocgocallback bool // whether disable callback from C
	// 是否正在跟踪此 G 的调度延迟统计信息
	tracking bool // whether we're tracking this G for sched latency statistics
	// 用于决定是否跟踪此 G
	trackingSeq uint8 // used to decide whether to track this G
	// G 上次开始被跟踪时的时间戳
	trackingStamp int64 // timestamp of when the G last started being tracked
	// 可运行状态的时间长度，运行时会被清除，仅在跟踪时使用
	runnableTime int64 // the amount of time spent runnable, cleared when running, only used when tracking
	// 锁定的 M
	lockedm muintptr
	// 信号
	sig uint32
	// 写缓冲区
	writebuf []byte
	// 信号代码 0
	sigcode0 uintptr
	// 信号代码 1
	sigcode1 uintptr
	// 信号程序计数器
	sigpc uintptr
	// 创建此 goroutine 的父 goroutine 的 ID
	parentGoid uint64 // goid of goroutine that created this goroutine
	// 创建此 goroutine 的 go 语句的程序计数器
	gopc uintptr // pc of go statement that created this goroutine
	// 创建此 goroutine 的祖先 goroutine 信息（仅在 debug.tracebackancestors 启用时使用）
	ancestors *[]ancestorInfo // ancestor information goroutine(s) that created this goroutine (only used if debug.tracebackancestors)
	// goroutine 函数的程序计数器
	startpc uintptr // pc of goroutine function
	// 竞态检测上下文
	racectx uintptr
	// 此 G 正在等待的 sudog 结构（具有有效的 elem 指针）；按锁顺序排列
	waiting *sudog // sudog structures this g is waiting on (that have a valid elem ptr); in lock order
	// cgo 回溯上下文
	cgoCtxt []uintptr // cgo traceback context
	// 性能分析标签
	labels unsafe.Pointer // profiler labels
	// time.Sleep 的缓存计时器
	timer *timer // cached timer for time.Sleep
	// 休眠到何时
	sleepWhen int64 // when to sleep until
	// 是否参与 select 操作以及是否有人赢得了竞态
	selectDone atomic.Uint32 // are we participating in a select and did someone win the race?

	// goroutineProfiled indicates the status of this goroutine's stack for the
	// current in-progress goroutine profile
	// goroutineProfiled 表示当前正在进行的 goroutine 分析中此 goroutine 栈的状态
	goroutineProfiled goroutineProfileStateHolder

	// argument during coroutine transfers
	// 协程传输期间的参数
	coroarg *coro

	// Per-G tracer state.
	// 每个 G 的追踪器状态
	trace gTraceState

	// Per-G GC state
	// 每个 G 的垃圾回收状态

	// gcAssistBytes is this G's GC assist credit in terms of
	// bytes allocated. If this is positive, then the G has credit
	// to allocate gcAssistBytes bytes without assisting. If this
	// is negative, then the G must correct this by performing
	// scan work. We track this in bytes to make it fast to update
	// and check for debt in the malloc hot path. The assist ratio
	// determines how this corresponds to scan work debt.
	// gcAssistBytes 表示此 G 的 GC 辅助信用额度，以分配的字节数为单位。
	// 如果为正，则 G 有信用额度可以在不辅助的情况下分配 gcAssistBytes 字节。
	// 如果为负，则 G 必须通过执行扫描工作来纠正。
	// 我们以字节为单位跟踪这个值，以便在 malloc 热点路径上快速更新和检查债务。
	// 辅助比率决定了这与扫描工作债务的对应关系。
	gcAssistBytes int64
}

// gTrackingPeriod is the number of transitions out of _Grunning between
// latency tracking runs.
// gTrackingPeriod 是在两次延迟跟踪运行之间从 _Grunning 状态转换的次数
const gTrackingPeriod = 8

const (
	// tlsSlots is the number of pointer-sized slots reserved for TLS on some platforms,
	// like Windows.
	// tlsSlots 是在某些平台（如 Windows）上为 TLS 保留的指针大小的槽位数
	tlsSlots = 6
	// tlsSize 是 TLS 的总大小，等于槽位数乘以指针大小
	tlsSize = tlsSlots * goarch.PtrSize
)

// Values for m.freeWait.
// m.freeWait 的取值
const (
	// freeMStack = 0 // M done, free stack and reference.
	// freeMStack = 0 // M 已完成，释放栈和引用
	freeMStack = 0 // M done, free stack and reference.
	// freeMRef = 1 // M done, free reference.
	// freeMRef = 1 // M 已完成，仅释放引用
	freeMRef = 1 // M done, free reference.
	// freeMWait = 2 // M still in use.
	// freeMWait = 2 // M 仍在使用中
	freeMWait = 2 // M still in use.
)

type m struct {
	// g0 是一个特殊的 goroutine，它拥有调度栈，用于执行调度相关的操作
	g0 *g // goroutine with scheduling stack
	// morebuf 是 morestack 函数的参数，用于栈扩展
	morebuf gobuf // gobuf arg to morestack
	// divmod 是 ARM 架构上用于除法和取模运算的分母，liblink 已知此值
	divmod uint32 // div/mod denominator for arm - known to liblink
	// _ 是一个填充字段，用于将下一个字段对齐到 8 字节边界
	_ uint32 // align next field to 8 bytes

	// Fields not known to debuggers.
	// 用于调试器，但偏移量不是硬编码的
	procid uint64 // for debuggers, but offset not hard-coded
	// 处理信号的 goroutine
	gsignal *g // signal-handling g
	// Go 分配的信号处理栈
	goSigStack gsignalStack // Go-allocated signal handling stack
	// 保存的信号掩码存储
	sigmask sigset // storage for saved signal mask
	// 线程本地存储（用于 x86 外部寄存器）
	tls      [tlsSlots]uintptr // thread-local storage (for x86 extern register)
	mstartfn func()            // M 的启动函数
	// 当前正在运行的 goroutine
	curg *g // current running goroutine
	// 在致命信号期间运行的 goroutine
	caughtsig guintptr // goroutine running during fatal signal
	// 用于执行 Go 代码的关联 P（如果不执行 Go 代码则为 nil）
	p     puintptr // attached p for executing go code (nil if not executing go code)
	nextp puintptr // 下一个要关联的 P
	// 执行系统调用之前关联的 P
	oldp      puintptr  // the p that was attached before executing a syscall
	id        int64     // M 的唯一标识符
	mallocing int32     // 是否正在执行内存分配
	throwing  throwType // 是否正在抛出异常
	// 如果不为空字符串，则保持 curg 在此 M 上运行
	preemptoff string // if != "", keep curg running on this m
	locks      int32  // M 持有的锁数量
	dying      int32  // M 是否正在退出
	profilehz  int32  // 性能分析采样频率
	// M 没有工作并正在积极寻找工作
	spinning bool // m is out of work and is actively looking for work
	// M 是否在 note 上阻塞
	blocked bool // m is blocked on a note
	// 在 C 线程上调用 sigaltstack 进行初始化
	newSigstack bool // minit on C thread called sigaltstack
	printlock   int8 // 打印锁
	// M 是否正在执行 cgo 调用
	incgo bool // m is executing a cgo call
	// M 是否是额外的 M
	isextra bool // m is an extra m
	// M 是否是额外的 M 且不执行 Go 代码
	isExtraInC bool // m is an extra m that is not executing Go code
	// M 是否是信号处理器中的额外 M
	isExtraInSig bool // m is an extra m in a signal handler
	// 是否可以安全地释放 g0 和删除 M（取值为 freeMRef、freeMStack 或 freeMWait）
	freeWait   atomic.Uint32 // Whether it is safe to free g0 and delete m (one of freeMRef, freeMStack, freeMWait)
	needextram bool          // 是否需要额外的 M
	// g0 栈是否有准确的边界
	g0StackAccurate bool  // whether the g0 stack has accurate bounds
	traceback       uint8 // 回溯级别
	// cgo 调用的总次数
	ncgocall uint64 // number of cgo calls in total
	// 当前正在进行的 cgo 调用数量
	ncgo int32 // number of cgo calls currently in progress
	// 如果非零，表示 cgoCallers 正在临时使用中
	cgoCallersUse atomic.Uint32 // if non-zero, cgoCallers in use temporarily
	// 如果在 cgo 调用中崩溃，用于回溯的 cgo 调用信息
	cgoCallers *cgoCallers // cgo traceback if crashing in cgo call
	park       note        // 用于暂停 M 的 note
	// 在 allm 链表中的下一个 M
	alllink   *m       // on allm
	schedlink muintptr // 调度器链表中的下一个 M
	lockedg   guintptr // 被锁定的 goroutine
	// 创建此线程的栈，用于 StackRecord.Stack0，因此必须与其对齐
	createstack [32]uintptr // stack that created this thread, it's used for StackRecord.Stack0, so it must align with it.
	// 外部 LockOSThread 的跟踪
	lockedExt uint32 // tracking for external LockOSThread
	// 内部 lockOSThread 的跟踪
	lockedInt uint32 // tracking for internal lockOSThread
	// 等待锁的下一个 M
	nextwaitm muintptr // next m waiting for lock

	mLockProfile mLockProfile // fields relating to runtime.lock contention
	// 与运行时锁竞争相关的字段
	profStack []uintptr // used for memory/block/mutex stack traces
	// 用于内存/阻塞/互斥锁的栈跟踪

	// wait* are used to carry arguments from gopark into park_m, because
	// there's no stack to put them on. That is their sole purpose.
	// wait* 用于将参数从 gopark 传递到 park_m，因为没有栈可以存放它们。这是它们的唯一目的。
	// 等待解锁函数
	waitunlockf func(*g, unsafe.Pointer) bool
	// 等待的锁
	waitlock unsafe.Pointer
	// 等待跟踪跳过的帧数
	waitTraceSkip int
	// 等待跟踪阻塞原因
	waitTraceBlockReason traceBlockReason

	// 系统调用计数
	syscalltick uint32
	// 在 sched.freem 上的空闲 M 链表
	freelink *m // on sched.freem
	// M 的跟踪状态
	trace mTraceState

	// these are here because they are too large to be on the stack
	// of low-level NOSPLIT functions.
	// 这些字段放在这里是因为它们太大，无法放在低级 NOSPLIT 函数的栈上
	// 库调用信息
	libcall libcall
	// 用于 CPU 分析器的库调用 PC
	libcallpc uintptr // for cpu profiler
	// 库调用栈指针
	libcallsp uintptr
	// 库调用 goroutine
	libcallg guintptr
	// 在 Windows 上存储系统调用参数
	winsyscall winlibcall // stores syscall parameters on windows

	// VDSO 调用期间的栈指针（如果不在调用中则为 0）
	vdsoSP uintptr // SP for traceback while in VDSO call (0 if not in call)
	// VDSO 调用期间的程序计数器
	vdsoPC uintptr // PC for traceback while in VDSO call

	// preemptGen counts the number of completed preemption
	// signals. This is used to detect when a preemption is
	// requested, but fails.
	// preemptGen 计算已完成的抢占信号数量。这用于检测何时请求抢占但失败。
	// 抢占生成计数
	preemptGen atomic.Uint32

	// Whether this is a pending preemption signal on this M.
	// 这是否是此 M 上的待处理抢占信号
	// 待处理信号
	signalPending atomic.Uint32

	// pcvalue lookup cache
	// pcvalue 查找缓存
	// PC 值缓存
	pcvalueCache pcvalueCache

	// 每个 M 的调试日志
	dlogPerM

	// 操作系统特定的 M 字段
	mOS

	// ChaCha8 随机数生成器状态
	chacha8 chacha8rand.State
	// 快速随机数
	cheaprand uint64

	// Up to 10 locks held by this m, maintained by the lock ranking code.
	// 此 M 持有的最多 10 个锁，由锁排名代码维护
	// 持有的锁数量
	locksHeldLen int
	// 持有的锁信息
	locksHeld [10]heldLockInfo
}

type p struct {
	id          int32      // P 的唯一标识符
	status      uint32     // one of pidle/prunning/... // P 的状态，可以是空闲、运行等
	link        puintptr   // 指向下一个 P 的指针，用于 P 的链表
	schedtick   uint32     // incremented on every scheduler call // 每次调度器调用时递增的计数器
	syscalltick uint32     // incremented on every system call // 每次系统调用时递增的计数器
	sysmontick  sysmontick // last tick observed by sysmon // 系统监控器最后观察到的时钟周期
	m           muintptr   // back-link to associated m (nil if idle) // 关联的 M 的反向链接（空闲时为 nil）
	mcache      *mcache    // 内存分配缓存
	pcache      pageCache  // 页面缓存
	raceprocctx uintptr    // 竞态检测上下文

	deferpool    []*_defer   // pool of available defer structs (see panic.go) // 可用的 defer 结构体池（参见 panic.go）
	deferpoolbuf [32]*_defer // defer 结构体的缓冲区，大小为 32

	// Cache of goroutine ids, amortizes accesses to runtime·sched.goidgen.
	// goroutine ID 的缓存，用于分摊对 runtime·sched.goidgen 的访问
	goidcache    uint64 // 当前缓存的 goroutine ID 起始值
	goidcacheend uint64 // 当前缓存的 goroutine ID 结束值

	// Queue of runnable goroutines. Accessed without lock.
	// 可运行 goroutine 的队列。无需加锁访问。
	runqhead uint32        // 队列头部索引
	runqtail uint32        // 队列尾部索引
	runq     [256]guintptr // 固定大小的 goroutine 队列，最多可存储 256 个 goroutine
	// runnext, if non-nil, is a runnable G that was ready'd by
	// the current G and should be run next instead of what's in
	// runq if there's time remaining in the running G's time
	// slice. It will inherit the time left in the current time
	// slice. If a set of goroutines is locked in a
	// communicate-and-wait pattern, this schedules that set as a
	// unit and eliminates the (potentially large) scheduling
	// latency that otherwise arises from adding the ready'd
	// goroutines to the end of the run queue.
	//
	// Note that while other P's may atomically CAS this to zero,
	// only the owner P can CAS it to a valid G.
	//
	// runnext 如果非空，表示一个可运行的 G，它是由当前 G 准备好的，
	// 如果当前运行的 G 的时间片还有剩余，它应该优先于 runq 中的 G 运行。
	// 它会继承当前时间片剩余的时间。如果一组 goroutine 被锁定在通信-等待模式中，
	// 这会将这组 goroutine 作为一个单元进行调度，并消除将准备好的 goroutine
	// 添加到运行队列末尾时可能产生的（潜在较大的）调度延迟。
	//
	// 注意，虽然其他 P 可以原子地将此值 CAS 为 0，
	// 但只有拥有此 P 的 M 才能将其 CAS 为一个有效的 G。
	runnext guintptr

	// Available G's (status == Gdead)
	// 可用的 G（状态为 Gdead）
	gFree struct {
		gList
		n int32
	}

	// Cache of sudog objects
	// sudog 对象的缓存
	sudogcache []*sudog
	sudogbuf   [128]*sudog

	// Cache of mspan objects from the heap.
	// 堆中 mspan 对象的缓存
	mspancache struct {
		// We need an explicit length here because this field is used
		// in allocation codepaths where write barriers are not allowed,
		// and eliminating the write barrier/keeping it eliminated from
		// slice updates is tricky, more so than just managing the length
		// ourselves.
		// 我们需要在这里显式指定长度，因为这个字段用于不允许写屏障的分配路径中，
		// 消除写屏障/保持写屏障在切片更新时被消除是很棘手的，
		// 比我们自己管理长度要复杂得多。
		len int
		buf [128]*mspan
	}

	// Cache of a single pinner object to reduce allocations from repeated
	// pinner creation.
	// 单个 pinner 对象的缓存，用于减少重复创建 pinner 对象时的内存分配
	pinnerCache *pinner

	trace pTraceState

	// 每个 P 的持久化分配器，用于避免互斥锁
	palloc persistentAlloc // per-P to avoid mutex

	// Per-P GC state
	// 每个 P 的 GC 状态
	// 辅助分配所花费的纳秒数
	gcAssistTime int64 // Nanoseconds in assistAlloc
	// 分式标记工作器所花费的纳秒数（原子操作）
	gcFractionalMarkTime int64 // Nanoseconds in fractional mark worker (atomic)

	// limiterEvent tracks events for the GC CPU limiter.
	// limiterEvent 跟踪 GC CPU 限制器的事件
	limiterEvent limiterEvent

	// gcMarkWorkerMode is the mode for the next mark worker to run in.
	// That is, this is used to communicate with the worker goroutine
	// selected for immediate execution by
	// gcController.findRunnableGCWorker. When scheduling other goroutines,
	// this field must be set to gcMarkWorkerNotWorker.
	// gcMarkWorkerMode 是下一个标记工作器运行的模式。
	// 也就是说，这用于与通过 gcController.findRunnableGCWorker 选择立即执行的
	// 工作 goroutine 进行通信。在调度其他 goroutine 时，
	// 此字段必须设置为 gcMarkWorkerNotWorker。
	gcMarkWorkerMode gcMarkWorkerMode
	// gcMarkWorkerStartTime is the nanotime() at which the most recent
	// mark worker started.
	// gcMarkWorkerStartTime 是最近一次标记工作器启动时的 nanotime()
	gcMarkWorkerStartTime int64

	// gcw is this P's GC work buffer cache. The work buffer is
	// filled by write barriers, drained by mutator assists, and
	// disposed on certain GC state transitions.
	// gcw 是这个 P 的 GC 工作缓冲区缓存。工作缓冲区由写屏障填充，
	// 由 mutator 辅助程序清空，并在某些 GC 状态转换时被处理。
	gcw gcWork

	// wbBuf is this P's GC write barrier buffer.
	//
	// TODO: Consider caching this in the running G.
	// wbBuf 是这个 P 的 GC 写屏障缓冲区。
	//
	// TODO: 考虑将其缓存在运行的 G 中。
	wbBuf wbBuf

	runSafePointFn uint32 // if 1, run sched.safePointFn at next safe point
	// 如果为 1，则在下一个安全点运行 sched.safePointFn

	// statsSeq is a counter indicating whether this P is currently
	// writing any stats. Its value is even when not, odd when it is.
	// statsSeq 是一个计数器，指示此 P 当前是否正在写入任何统计信息。
	// 当不写入时其值为偶数，写入时为奇数。
	statsSeq atomic.Uint32

	// Timer heap.
	// 定时器堆
	timers timers

	// maxStackScanDelta accumulates the amount of stack space held by
	// live goroutines (i.e. those eligible for stack scanning).
	// Flushed to gcController.maxStackScan once maxStackScanSlack
	// or -maxStackScanSlack is reached.
	// maxStackScanDelta 累积由存活的 goroutine 持有的栈空间量
	//（即那些有资格进行栈扫描的 goroutine）。
	// 当达到 maxStackScanSlack 或 -maxStackScanSlack 时，
	// 会被刷新到 gcController.maxStackScan。
	maxStackScanDelta int64

	// gc-time statistics about current goroutines
	// Note that this differs from maxStackScan in that this
	// accumulates the actual stack observed to be used at GC time (hi - sp),
	// not an instantaneous measure of the total stack size that might need
	// to be scanned (hi - lo).
	// 关于当前 goroutine 的 GC 时间统计信息
	// 注意这与 maxStackScan 不同，因为它累积的是在 GC 时观察到的实际使用的栈空间（hi - sp），
	// 而不是可能需要扫描的总栈大小的瞬时测量值（hi - lo）。
	scannedStackSize uint64 // stack size of goroutines scanned by this P
	scannedStacks    uint64 // number of goroutines scanned by this P

	// preempt is set to indicate that this P should be enter the
	// scheduler ASAP (regardless of what G is running on it).
	// preempt 设置为 true 表示这个 P 应该尽快进入调度器
	//（无论当前正在运行什么 G）。
	preempt bool

	// gcStopTime is the nanotime timestamp that this P last entered _Pgcstop.
	// gcStopTime 是这个 P 最后一次进入 _Pgcstop 状态时的 nanotime 时间戳。
	gcStopTime int64

	// Padding is no longer needed. False sharing is now not a worry because p is large enough
	// that its size class is an integer multiple of the cache line size (for any of our architectures).
	// 不再需要填充。由于 p 的大小足够大，其大小类是缓存行大小的整数倍
	//（对于我们的任何架构都是如此），所以现在不需要担心伪共享问题。
}

type schedt struct {
	goidgen atomic.Uint64 // 用于生成 goroutine ID 的计数器
	// 上次网络轮询的时间，如果当前正在轮询则为 0
	lastpoll atomic.Int64 // time of last network poll, 0 if currently polling
	// 当前轮询休眠到的时间点
	pollUntil atomic.Int64 // time to which current poll is sleeping

	lock mutex // 调度器锁，用于保护调度器状态

	// When increasing nmidle, nmidlelocked, nmsys, or nmfreed, be
	// sure to call checkdead().
	// 当增加 nmidle、nmidlelocked、nmsys 或 nmfreed 时，
	// 必须调用 checkdead() 检查死锁

	// 等待工作的空闲 M 列表
	midle muintptr // idle m's waiting for work
	// 等待工作的空闲 M 数量
	nmidle int32 // number of idle m's waiting for work
	// 等待工作的锁定 M 数量
	nmidlelocked int32 // number of locked m's waiting for work
	// 已创建的 M 数量和下一个 M 的 ID
	mnext int64 // number of m's that have been created and next M ID
	// 允许的最大 M 数量（超过则终止）
	maxmcount int32 // maximum number of m's allowed (or die)
	// 不计入死锁检查的系统 M 数量
	nmsys int32 // number of system m's not counted for deadlock
	// 已释放的 M 累计数量
	nmfreed int64 // cumulative number of freed m's

	// 系统 goroutine 的数量
	ngsys atomic.Int32 // number of system goroutines

	// 空闲的 P 列表
	pidle  puintptr     // idle p's
	npidle atomic.Int32 // 空闲 P 的数量
	// 自旋的 M 数量，参见 proc.go 中的 "Worker thread parking/unparking" 注释
	nmspinning atomic.Int32 // See "Worker thread parking/unparking" comment in proc.go.
	// 是否需要自旋，参见 proc.go 中的 "Delicate dance" 注释。布尔值。设置为 1 时必须持有 sched.lock
	needspinning atomic.Uint32 // See "Delicate dance" comment in proc.go. Boolean. Must hold sched.lock to set to 1.

	// Global runnable queue.
	// 全局可运行队列
	runq     gQueue // 全局可运行 goroutine 队列
	runqsize int32  // 全局可运行队列的大小

	// disable controls selective disabling of the scheduler.
	//
	// Use schedEnableUser to control this.
	//
	// disable is protected by sched.lock.
	// disable 控制调度器的选择性禁用。
	//
	// 使用 schedEnableUser 来控制这个功能。
	//
	// disable 受 sched.lock 保护。
	disable struct {
		// user disables scheduling of user goroutines.
		// user 禁用用户 goroutine 的调度。
		user     bool
		runnable gQueue // pending runnable Gs
		n        int32  // length of runnable
	}

	// Global cache of dead G's.
	// 已死亡的 G 的全局缓存。
	gFree struct {
		lock    mutex
		stack   gList // Gs with stacks
		noStack gList // Gs without stacks
		n       int32
	}

	// Central cache of sudog structs.
	// sudog 结构体的中央缓存。
	sudoglock  mutex
	sudogcache *sudog

	// Central pool of available defer structs.
	// 可用的 defer 结构体的中央池。
	deferlock mutex
	deferpool *_defer

	// freem is the list of m's waiting to be freed when their
	// m.exited is set. Linked through m.freelink.
	// freem 是等待被释放的 M 列表，当它们的 m.exited 被设置时。
	// 通过 m.freelink 链接。
	freem *m

	// gcwaiting 表示垃圾回收器正在等待运行
	gcwaiting atomic.Bool // gc is waiting to run
	// stopwait 用于等待所有 P 停止的计数器
	stopwait int32
	// stopnote 用于通知所有 P 停止的信号量
	stopnote note
	// sysmonwait 表示系统监控器正在等待
	sysmonwait atomic.Bool
	// sysmonnote 用于通知系统监控器的信号量
	sysmonnote note

	// safePointFn should be called on each P at the next GC
	// safepoint if p.runSafePointFn is set.
	// safePointFn 应该在下一个 GC 安全点被调用，如果 p.runSafePointFn 被设置的话
	// safePointFn 是在安全点执行的函数
	safePointFn func(*p)
	// safePointWait 用于等待所有 P 到达安全点的计数器
	safePointWait int32
	// safePointNote 用于通知所有 P 到达安全点的信号量
	safePointNote note

	// profilehz 表示 CPU 性能分析的采样频率
	profilehz int32 // cpu profiling rate

	// procresizetime 记录最后一次修改 gomaxprocs 的纳秒时间戳
	procresizetime int64 // nanotime() of last change to gomaxprocs
	// totaltime 记录从开始到 procresizetime 时刻的 gomaxprocs 时间积分
	totaltime int64 // ∫gomaxprocs dt up to procresizetime

	// sysmonlock protects sysmon's actions on the runtime.
	//
	// Acquire and hold this mutex to block sysmon from interacting
	// with the rest of the runtime.
	// sysmonlock 保护 sysmon 对运行时的操作。
	//
	// 获取并持有此互斥锁可以阻止 sysmon 与运行时的其他部分交互。
	sysmonlock mutex

	// timeToRun is a distribution of scheduling latencies, defined
	// as the sum of time a G spends in the _Grunnable state before
	// it transitions to _Grunning.
	// timeToRun 是调度延迟的分布，定义为 G 在从 _Grunnable 状态转换到
	// _Grunning 状态之前所花费的时间总和。
	timeToRun timeHistogram

	// idleTime is the total CPU time Ps have "spent" idle.
	//
	// Reset on each GC cycle.
	// idleTime 是 P 处于空闲状态的总 CPU 时间。
	//
	// 在每个 GC 周期重置。
	idleTime atomic.Int64

	// totalMutexWaitTime is the sum of time goroutines have spent in _Gwaiting
	// with a waitreason of the form waitReasonSync{RW,}Mutex{R,}Lock.
	// totalMutexWaitTime 是 goroutine 在 _Gwaiting 状态下等待互斥锁的总时间，
	// 等待原因的形式为 waitReasonSync{RW,}Mutex{R,}Lock。
	totalMutexWaitTime atomic.Int64

	// stwStoppingTimeGC/Other are distributions of stop-the-world stopping
	// latencies, defined as the time taken by stopTheWorldWithSema to get
	// all Ps to stop. stwStoppingTimeGC covers all GC-related STWs,
	// stwStoppingTimeOther covers the others.
	// stwStoppingTimeGC/Other 是 stop-the-world 停止延迟的分布，
	// 定义为 stopTheWorldWithSema 使所有 P 停止所需的时间。
	// stwStoppingTimeGC 涵盖所有与 GC 相关的 STW，
	// stwStoppingTimeOther 涵盖其他情况。
	stwStoppingTimeGC    timeHistogram
	stwStoppingTimeOther timeHistogram

	// stwTotalTimeGC/Other are distributions of stop-the-world total
	// latencies, defined as the total time from stopTheWorldWithSema to
	// startTheWorldWithSema. This is a superset of
	// stwStoppingTimeGC/Other. stwTotalTimeGC covers all GC-related STWs,
	// stwTotalTimeOther covers the others.
	// stwTotalTimeGC/Other 是 stop-the-world 总延迟的分布，
	// 定义为从 stopTheWorldWithSema 到 startTheWorldWithSema 的总时间。
	// 这是 stwStoppingTimeGC/Other 的超集。
	// stwTotalTimeGC 涵盖所有与 GC 相关的 STW，
	// stwTotalTimeOther 涵盖其他情况。
	stwTotalTimeGC    timeHistogram
	stwTotalTimeOther timeHistogram

	// totalRuntimeLockWaitTime (plus the value of lockWaitTime on each M in
	// allm) is the sum of time goroutines have spent in _Grunnable and with an
	// M, but waiting for locks within the runtime. This field stores the value
	// for Ms that have exited.
	// totalRuntimeLockWaitTime（加上 allm 中每个 M 的 lockWaitTime 值）
	// 是 goroutine 在 _Grunnable 状态且有 M 时，等待运行时内部锁的总时间。
	// 此字段存储已退出的 M 的值。
	totalRuntimeLockWaitTime atomic.Int64
}

// Values for the flags field of a sigTabT.
// sigTabT 的 flags 字段的值。
const (
	// 允许 signal.Notify 接收信号，即使信号来自内核
	_SigNotify = 1 << iota // let signal.Notify have signal, even if from kernel
	// 如果 signal.Notify 不处理该信号，则安静地退出
	_SigKill // if signal.Notify doesn't take it, exit quietly
	// 如果 signal.Notify 不处理该信号，则大声地退出（打印错误信息）
	_SigThrow // if signal.Notify doesn't take it, exit loudly
	// 如果信号来自内核，则触发 panic
	_SigPanic // if the signal is from the kernel, panic
	// 如果信号未被显式请求，则不监控它
	_SigDefault // if the signal isn't explicitly requested, don't monitor it
	// 导致所有运行时进程退出（仅在 Plan 9 上使用）
	_SigGoExit // cause all runtime procs to exit (only used on Plan 9).
	// 不显式安装处理程序，但向现有的 libc 处理程序添加 SA_ONSTACK
	_SigSetStack // Don't explicitly install handler, but add SA_ONSTACK to existing libc handler
	// 始终解除阻塞；参见 blockableSig
	_SigUnblock // always unblock; see blockableSig
	// _SIG_DFL 的默认行为是忽略该信号
	_SigIgn // _SIG_DFL action is to ignore the signal
)

// Layout of in-memory per-function information prepared by linker
// See https://golang.org/s/go12symtab.
// Keep in sync with linker (../cmd/link/internal/ld/pcln.go:/pcln.go:/pclntab)
// and with package debug/gosym and with symtab.go in package runtime.
// 链接器准备的内存中每个函数的信息布局
// 参见 https://golang.org/s/go12symtab
// 需要与链接器 (../cmd/link/internal/ld/pcln.go:/pclntab)、
// debug/gosym 包以及 runtime 包中的 symtab.go 保持同步
type _func struct {
	// 仅在静态数据中使用
	sys.NotInHeap // Only in static data

	// 函数入口点相对于 moduledata.text/pcHeader.textStart 的偏移量
	entryOff uint32 // start pc, as offset from moduledata.text/pcHeader.textStart
	// 函数名称在 moduledata.funcnametab 中的索引
	nameOff int32 // function name, as index into moduledata.funcnametab.

	// 输入/输出参数的大小
	args int32 // in/out args size
	// 如果有的话，deferreturn 调用指令相对于入口点的偏移量
	deferreturn uint32 // offset of start of a deferreturn call instruction from entry, if any.

	pcsp    uint32 // 栈指针程序计数器
	pcfile  uint32 // 文件程序计数器
	pcln    uint32 // 行号程序计数器
	npcdata uint32 // PC 数据项的数量
	// 该函数的编译单元在 runtime.cutab 中的偏移量
	cuOffset uint32 // runtime.cutab offset of this function's CU
	// 函数开始的行号（func 关键字/TEXT 指令）
	startLine int32 // line number of start of function (func keyword/TEXT directive)
	// 为某些特殊的运行时函数设置
	funcID abi.FuncID   // set for certain special runtime functions
	flag   abi.FuncFlag // 函数标志
	_      [1]byte      // pad
	// 填充字节
	// 必须是最后一个字段，必须以 uint32 对齐的边界结束
	nfuncdata uint8 // must be last, must end on a uint32-aligned boundary

	// The end of the struct is followed immediately by two variable-length
	// arrays that reference the pcdata and funcdata locations for this
	// function.
	// 结构体末尾紧接着两个变长数组，用于引用该函数的 pcdata 和 funcdata 位置

	// pcdata contains the offset into moduledata.pctab for the start of
	// that index's table. e.g.,
	// &moduledata.pctab[_func.pcdata[_PCDATA_UnsafePoint]] is the start of
	// the unsafe point table.
	//
	// An offset of 0 indicates that there is no table.
	//
	// pcdata [npcdata]uint32
	// pcdata 包含 moduledata.pctab 中该索引表的起始偏移量。
	// 例如，&moduledata.pctab[_func.pcdata[_PCDATA_UnsafePoint]] 是不安全点表的起始位置。
	// 偏移量为 0 表示没有对应的表。

	// funcdata contains the offset past moduledata.gofunc which contains a
	// pointer to that index's funcdata. e.g.,
	// *(moduledata.gofunc +  _func.funcdata[_FUNCDATA_ArgsPointerMaps]) is
	// the argument pointer map.
	//
	// An offset of ^uint32(0) indicates that there is no entry.
	//
	// funcdata [nfuncdata]uint32
	// funcdata 包含 moduledata.gofunc 之后的偏移量，该偏移量包含指向该索引的 funcdata 的指针。
	// 例如，*(moduledata.gofunc + _func.funcdata[_FUNCDATA_ArgsPointerMaps]) 是参数指针映射。
	// 偏移量为 ^uint32(0) 表示没有对应的条目。
}

// Pseudo-Func that is returned for PCs that occur in inlined code.
// A *Func can be either a *_func or a *funcinl, and they are distinguished
// by the first uintptr.
//
// TODO(austin): Can we merge this with inlinedCall?
// 内联代码中出现的 PC 所返回的伪函数。
// *Func 可以是 *_func 或 *funcinl，它们通过第一个 uintptr 来区分。
//
// TODO(austin): 我们能否将其与 inlinedCall 合并？
type funcinl struct {
	ones      uint32  // set to ^0 to distinguish from _func
	entry     uintptr // entry of the real (the "outermost") frame
	name      string
	file      string
	line      int32
	startLine int32
}

// itab 是接口表，用于存储接口类型和具体类型之间的映射关系
// itab is the interface table, used to store the mapping between interface types and concrete types
type itab = abi.ITab

// Lock-free stack node.
// Also known to export_test.go.
// 无锁栈节点。
// 在 export_test.go 中也有使用。
type lfnode struct {
	next    uint64  // 指向下一个节点的指针，使用 uint64 类型以支持原子操作
	pushcnt uintptr // 记录节点被推入栈的次数，用于 ABA 问题的检测
}

// forcegcstate 结构体用于管理强制垃圾回收的状态
// forcegcstate struct is used to manage the state of forced garbage collection
type forcegcstate struct {
	lock mutex       // 互斥锁，用于保护对 forcegcstate 的并发访问
	g    *g          // 指向执行强制垃圾回收的 goroutine
	idle atomic.Bool // 原子布尔值，表示强制垃圾回收是否处于空闲状态
}

// A _defer holds an entry on the list of deferred calls.
// If you add a field here, add code to clear it in deferProcStack.
// This struct must match the code in cmd/compile/internal/ssagen/ssa.go:deferstruct
// and cmd/compile/internal/ssagen/ssa.go:(*state).call.
// Some defers will be allocated on the stack and some on the heap.
// All defers are logically part of the stack, so write barriers to
// initialize them are not required. All defers must be manually scanned,
// and for heap defers, marked.
//
// _defer 结构体用于存储延迟调用的列表项。
// 如果在此添加字段，需要在 deferProcStack 中添加清除该字段的代码。
// 此结构体必须与 cmd/compile/internal/ssagen/ssa.go:deferstruct 和
// cmd/compile/internal/ssagen/ssa.go:(*state).call 中的代码匹配。
// 一些 defer 会被分配在栈上，一些会被分配在堆上。
// 所有 defer 在逻辑上都是栈的一部分，因此初始化它们不需要写屏障。
// 所有 defer 必须手动扫描，对于堆上的 defer，还需要标记。
type _defer struct {
	heap      bool    // 表示该 defer 是否分配在堆上
	rangefunc bool    // 是否为 range-over-func 列表中的 defer
	sp        uintptr // 记录 defer 时的栈指针
	pc        uintptr // 记录 defer 时的程序计数器
	fn        func()  // 延迟执行的函数，对于开放编码的 defer 可能为 nil
	link      *_defer // 指向 G 上的下一个 defer，可以指向堆或栈上的 defer

	// If rangefunc is true, *head is the head of the atomic linked list
	// during a range-over-func execution.
	// 如果 rangefunc 为 true，*head 是 range-over-func 执行期间的原子链表的头节点
	head *atomic.Pointer[_defer]
}

// A _panic holds information about an active panic.
//
// A _panic value must only ever live on the stack.
//
// The argp and link fields are stack pointers, but don't need special
// handling during stack growth: because they are pointer-typed and
// _panic values only live on the stack, regular stack pointer
// adjustment takes care of them.
type _panic struct {
	// argp 指向在 panic 期间运行的延迟调用的参数；不能移动 - 由 liblink 使用
	argp unsafe.Pointer // pointer to arguments of deferred call run during panic; cannot move - known to liblink
	// arg 是传递给 panic 的参数
	arg any // argument to panic
	// link 指向更早的 panic，形成 panic 链
	link *_panic // link to earlier panic

	// startPC 和 startSP 记录 _panic.start 被调用的位置
	// startPC and startSP track where _panic.start was called.
	startPC uintptr
	startSP unsafe.Pointer

	// 当前正在运行延迟调用的栈帧
	// The current stack frame that we're running deferred calls for.
	sp unsafe.Pointer // 栈指针
	lr uintptr        // 链接寄存器
	fp unsafe.Pointer // 帧指针

	// retpc 存储 panic 应该跳转回的 PC，如果最后一个由 _panic.next() 返回的函数恢复了 panic
	// retpc stores the PC where the panic should jump back to, if the
	// function last returned by _panic.next() recovers the panic.
	retpc uintptr

	// 处理开放编码 defer 的额外状态
	// Extra state for handling open-coded defers.
	deferBitsPtr *uint8         // 指向 defer 位图的指针
	slotsPtr     unsafe.Pointer // 指向 defer 槽的指针

	recovered   bool // 表示这个 panic 是否已被恢复
	goexit      bool // 表示是否通过 goexit 退出
	deferreturn bool // 表示是否在 deferreturn 中
}

// savedOpenDeferState tracks the extra state from _panic that's
// necessary for deferreturn to pick up where gopanic left off,
// without needing to unwind the stack.
// savedOpenDeferState 跟踪 _panic 的额外状态，这些状态对于 deferreturn
// 在 gopanic 停止的地方继续执行是必要的，而不需要展开栈。
type savedOpenDeferState struct {
	retpc           uintptr // 返回程序计数器，记录恢复后继续执行的地址
	deferBitsOffset uintptr // defer 位图的偏移量，用于定位 defer 状态
	slotsOffset     uintptr // defer 槽的偏移量，用于定位 defer 参数
}

// ancestorInfo records details of where a goroutine was started.
// ancestorInfo 记录 goroutine 启动位置的详细信息。
type ancestorInfo struct {
	// pcs 存储这个 goroutine 栈上的程序计数器值
	pcs []uintptr // pcs from the stack of this goroutine
	// goid 是这个 goroutine 的 ID；原始 goroutine 可能已经结束
	goid uint64 // goroutine id of this goroutine; original goroutine possibly dead
	// gopc 是创建这个 goroutine 的 go 语句的程序计数器值
	gopc uintptr // pc of go statement that created this goroutine
}

// A waitReason explains why a goroutine has been stopped.
// See gopark. Do not re-use waitReasons, add new ones.
// waitReason 解释了 goroutine 被停止的原因。
// 参见 gopark。不要重用 waitReasons，需要时添加新的原因。
type waitReason uint8

const (
	waitReasonZero                  waitReason = iota // ""
	waitReasonGCAssistMarking                         // "GC assist marking"
	waitReasonIOWait                                  // "IO wait"
	waitReasonChanReceiveNilChan                      // "chan receive (nil chan)"
	waitReasonChanSendNilChan                         // "chan send (nil chan)"
	waitReasonDumpingHeap                             // "dumping heap"
	waitReasonGarbageCollection                       // "garbage collection"
	waitReasonGarbageCollectionScan                   // "garbage collection scan"
	waitReasonPanicWait                               // "panicwait"
	waitReasonSelect                                  // "select"
	waitReasonSelectNoCases                           // "select (no cases)"
	waitReasonGCAssistWait                            // "GC assist wait"
	waitReasonGCSweepWait                             // "GC sweep wait"
	waitReasonGCScavengeWait                          // "GC scavenge wait"
	waitReasonChanReceive                             // "chan receive"
	waitReasonChanSend                                // "chan send"
	waitReasonFinalizerWait                           // "finalizer wait"
	waitReasonForceGCIdle                             // "force gc (idle)"
	waitReasonSemacquire                              // "semacquire"
	waitReasonSleep                                   // "sleep"
	waitReasonSyncCondWait                            // "sync.Cond.Wait"
	waitReasonSyncMutexLock                           // "sync.Mutex.Lock"
	waitReasonSyncRWMutexRLock                        // "sync.RWMutex.RLock"
	waitReasonSyncRWMutexLock                         // "sync.RWMutex.Lock"
	waitReasonTraceReaderBlocked                      // "trace reader (blocked)"
	waitReasonWaitForGCCycle                          // "wait for GC cycle"
	waitReasonGCWorkerIdle                            // "GC worker (idle)"
	waitReasonGCWorkerActive                          // "GC worker (active)"
	waitReasonPreempted                               // "preempted"
	waitReasonDebugCall                               // "debug call"
	waitReasonGCMarkTermination                       // "GC mark termination"
	waitReasonStoppingTheWorld                        // "stopping the world"
	waitReasonFlushProcCaches                         // "flushing proc caches"
	waitReasonTraceGoroutineStatus                    // "trace goroutine status"
	waitReasonTraceProcStatus                         // "trace proc status"
	waitReasonPageTraceFlush                          // "page trace flush"
	waitReasonCoroutine                               // "coroutine"
	waitReasonGCWeakToStrongWait                      // "GC weak to strong wait"
)

var waitReasonStrings = [...]string{
	waitReasonZero:                  "",
	waitReasonGCAssistMarking:       "GC assist marking",
	waitReasonIOWait:                "IO wait",
	waitReasonChanReceiveNilChan:    "chan receive (nil chan)",
	waitReasonChanSendNilChan:       "chan send (nil chan)",
	waitReasonDumpingHeap:           "dumping heap",
	waitReasonGarbageCollection:     "garbage collection",
	waitReasonGarbageCollectionScan: "garbage collection scan",
	waitReasonPanicWait:             "panicwait",
	waitReasonSelect:                "select",
	waitReasonSelectNoCases:         "select (no cases)",
	waitReasonGCAssistWait:          "GC assist wait",
	waitReasonGCSweepWait:           "GC sweep wait",
	waitReasonGCScavengeWait:        "GC scavenge wait",
	waitReasonChanReceive:           "chan receive",
	waitReasonChanSend:              "chan send",
	waitReasonFinalizerWait:         "finalizer wait",
	waitReasonForceGCIdle:           "force gc (idle)",
	waitReasonSemacquire:            "semacquire",
	waitReasonSleep:                 "sleep",
	waitReasonSyncCondWait:          "sync.Cond.Wait",
	waitReasonSyncMutexLock:         "sync.Mutex.Lock",
	waitReasonSyncRWMutexRLock:      "sync.RWMutex.RLock",
	waitReasonSyncRWMutexLock:       "sync.RWMutex.Lock",
	waitReasonTraceReaderBlocked:    "trace reader (blocked)",
	waitReasonWaitForGCCycle:        "wait for GC cycle",
	waitReasonGCWorkerIdle:          "GC worker (idle)",
	waitReasonGCWorkerActive:        "GC worker (active)",
	waitReasonPreempted:             "preempted",
	waitReasonDebugCall:             "debug call",
	waitReasonGCMarkTermination:     "GC mark termination",
	waitReasonStoppingTheWorld:      "stopping the world",
	waitReasonFlushProcCaches:       "flushing proc caches",
	waitReasonTraceGoroutineStatus:  "trace goroutine status",
	waitReasonTraceProcStatus:       "trace proc status",
	waitReasonPageTraceFlush:        "page trace flush",
	waitReasonCoroutine:             "coroutine",
	waitReasonGCWeakToStrongWait:    "GC weak to strong wait",
}

func (w waitReason) String() string {
	if w < 0 || w >= waitReason(len(waitReasonStrings)) {
		return "unknown wait reason"
	}
	return waitReasonStrings[w]
}

func (w waitReason) isMutexWait() bool {
	return w == waitReasonSyncMutexLock ||
		w == waitReasonSyncRWMutexRLock ||
		w == waitReasonSyncRWMutexLock
}

func (w waitReason) isWaitingForGC() bool {
	return isWaitingForGC[w]
}

// isWaitingForGC indicates that a goroutine is only entering _Gwaiting and
// setting a waitReason because it needs to be able to let the GC take ownership
// of its stack. The G is always actually executing on the system stack, in
// these cases.
//
// TODO(mknyszek): Consider replacing this with a new dedicated G status.

// isWaitingForGC 表示一个 goroutine 进入 _Gwaiting 状态并设置 waitReason
// 仅仅是因为它需要让 GC 能够接管其栈的所有权。在这些情况下，G 实际上
// 总是在系统栈上执行。
//
// TODO(mknyszek): 考虑用一个新的专用 G 状态来替代这个机制。
var isWaitingForGC = [len(waitReasonStrings)]bool{
	waitReasonStoppingTheWorld:      true,
	waitReasonGCMarkTermination:     true,
	waitReasonGarbageCollection:     true,
	waitReasonGarbageCollectionScan: true,
	waitReasonTraceGoroutineStatus:  true,
	waitReasonTraceProcStatus:       true,
	waitReasonPageTraceFlush:        true,
	waitReasonGCAssistMarking:       true,
	waitReasonGCWorkerActive:        true,
	waitReasonFlushProcCaches:       true,
}

var (
	// allm is the list of all m's that have been created by
	// the program and are not yet dead.
	// allm 是所有已创建且尚未死亡的 m 的列表
	allm *m

	// gomaxprocs is the maximum number of CPUs that can be executing
	// simultaneously. It is set by GOMAXPROCS.
	// gomaxprocs 是可以同时执行的最大 CPU 数量，由 GOMAXPROCS 环境变量设置
	gomaxprocs int32

	// ncpu is the number of CPUs on the machine.
	// ncpu 是机器上的 CPU 数量
	ncpu int32

	// forcegc is used to force a GC cycle.
	// forcegc 用于强制触发 GC 周期
	forcegc forcegcstate

	// sched is the global scheduler state.
	// sched 是全局调度器状态
	sched schedt

	// newprocs is the number of processors that should be running.
	// newprocs 是应该运行的处理器数量
	newprocs int32
)

var (
	// allpLock protects P-less reads and size changes of allp, idlepMask,
	// and timerpMask, and all writes to allp.
	// allpLock 保护对 allp 的无 P 读取和大小更改，以及 idlepMask 和 timerpMask，
	// 以及对 allp 的所有写入操作。
	allpLock mutex

	// len(allp) == gomaxprocs; may change at safe points, otherwise
	// immutable.
	// allp 的长度等于 gomaxprocs；可以在安全点更改，否则不可变。
	allp []*p

	// Bitmask of Ps in _Pidle list, one bit per P. Reads and writes must
	// be atomic. Length may change at safe points.
	//
	// Each P must update only its own bit. In order to maintain
	// consistency, a P going idle must the idle mask simultaneously with
	// updates to the idle P list under the sched.lock, otherwise a racing
	// pidleget may clear the mask before pidleput sets the mask,
	// corrupting the bitmap.
	//
	// N.B., procresize takes ownership of all Ps in stopTheWorldWithSema.
	// idlepMask 是 _Pidle 列表中 P 的位掩码，每个 P 对应一个位。读取和写入必须是原子的。
	// 长度可以在安全点更改。
	//
	// 每个 P 只能更新自己的位。为了保持一致性，一个 P 进入空闲状态时必须同时更新空闲掩码
	// 和在 sched.lock 下的空闲 P 列表，否则竞态的 pidleget 可能会在 pidleput 设置掩码之前
	// 清除掩码，从而破坏位图。
	//
	// 注意：procresize 在 stopTheWorldWithSema 中获取所有 P 的所有权。
	idlepMask pMask

	// Bitmask of Ps that may have a timer, one bit per P. Reads and writes
	// must be atomic. Length may change at safe points.
	//
	// Ideally, the timer mask would be kept immediately consistent on any timer
	// operations. Unfortunately, updating a shared global data structure in the
	// timer hot path adds too much overhead in applications frequently switching
	// between no timers and some timers.
	//
	// As a compromise, the timer mask is updated only on pidleget / pidleput. A
	// running P (returned by pidleget) may add a timer at any time, so its mask
	// must be set. An idle P (passed to pidleput) cannot add new timers while
	// idle, so if it has no timers at that time, its mask may be cleared.
	//
	// Thus, we get the following effects on timer-stealing in findrunnable:
	//
	//   - Idle Ps with no timers when they go idle are never checked in findrunnable
	//     (for work- or timer-stealing; this is the ideal case).
	//   - Running Ps must always be checked.
	//   - Idle Ps whose timers are stolen must continue to be checked until they run
	//     again, even after timer expiration.
	//
	// When the P starts running again, the mask should be set, as a timer may be
	// added at any time.
	//
	// TODO(prattmic): Additional targeted updates may improve the above cases.
	// e.g., updating the mask when stealing a timer.
	// timerpMask 是可能具有定时器的 P 的位掩码，每个 P 对应一个位。读取和写入必须是原子的。
	// 长度可以在安全点更改。
	//
	// 理想情况下，定时器掩码应该在每次定时器操作时立即保持一致性。不幸的是，在定时器热点路径上
	// 更新共享的全局数据结构会在频繁在无定时器和有定时器之间切换的应用程序中添加过多开销。
	//
	// 作为折衷方案，定时器掩码仅在 pidleget / pidleput 时更新。一个运行的 P（由 pidleget 返回）
	// 可以随时添加定时器，所以它的掩码必须被设置。一个空闲的 P（传递给 pidleput）在空闲时不能添加
	// 新的定时器，所以如果它在那个时间没有定时器，它的掩码可以被清除。
	//
	// 因此，我们在 findrunnable 中的定时器窃取会得到以下效果：
	//
	//   - 进入空闲状态时没有定时器的空闲 P 永远不会在 findrunnable 中被检查
	//     （对于工作窃取或定时器窃取；这是理想情况）。
	//   - 运行的 P 必须始终被检查。
	//   - 定时器被窃取的空闲 P 必须继续被检查，直到它们再次运行，即使在定时器到期后。
	//
	// 当 P 再次开始运行时，掩码应该被设置，因为定时器可能随时被添加。
	//
	// TODO(prattmic): 额外的针对性更新可能会改善上述情况。
	// 例如，在窃取定时器时更新掩码。
	timerpMask pMask
)

// goarmsoftfp is used by runtime/cgo assembly.
//
//go:linkname goarmsoftfp

var (
	// Pool of GC parked background workers. Entries are type
	// *gcBgMarkWorkerNode.
	// GC 后台工作者的停放池。条目类型为 *gcBgMarkWorkerNode。
	gcBgMarkWorkerPool lfstack

	// Total number of gcBgMarkWorker goroutines. Protected by worldsema.
	// gcBgMarkWorker goroutine 的总数。由 worldsema 保护。
	gcBgMarkWorkerCount int32

	// Information about what cpu features are available.
	// Packages outside the runtime should not use these
	// as they are not an external api.
	// Set on startup in asm_{386,amd64}.s
	// 关于可用 CPU 特性的信息。
	// 运行时包外的代码不应使用这些变量，
	// 因为它们不是外部 API。
	// 在 asm_{386,amd64}.s 中启动时设置。
	processorVersionInfo uint32
	isIntel              bool
)

// set by cmd/link on arm systems
// accessed using linkname by internal/runtime/atomic.
//
// goarm should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/creativeprojects/go-selfupdate
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
// 由 cmd/link 在 arm 系统上设置
// 通过 linkname 被 internal/runtime/atomic 访问
//
// goarm 应该是一个内部细节，
// 但许多包通过 linkname 访问它。
// 著名的"耻辱堂"成员包括：
//   - github.com/creativeprojects/go-selfupdate
//
// 不要删除或更改类型签名。
// 参见 go.dev/issue/67401。
//
//go:linkname goarm
var (
	goarm       uint8 // ARM 架构版本
	goarmsoftfp uint8 // ARM 软浮点标志
)

// Set by the linker so the runtime can determine the buildmode.
// 由链接器设置，以便运行时可以确定构建模式。
var (
	islibrary bool // -buildmode=c-shared
	// 是否为共享库模式 (-buildmode=c-shared)
	isarchive bool // -buildmode=c-archive
	// 是否为归档模式 (-buildmode=c-archive)
)

// Must agree with internal/buildcfg.FramePointerEnabled.
// 必须与 internal/buildcfg.FramePointerEnabled 保持一致。
const framepointer_enabled = GOARCH == "amd64" || GOARCH == "arm64"

// 帧指针是否启用，仅在 amd64 或 arm64 架构上启用

// getcallerfp returns the frame pointer of the caller of the caller
// of this function.
// getcallerfp 返回调用此函数的调用者的调用者的帧指针。
//
//go:nosplit
//go:noinline
func getcallerfp() uintptr {
	fp := getfp() // This frame's FP.
	// 获取当前帧的帧指针
	if fp != 0 {
		fp = *(*uintptr)(unsafe.Pointer(fp)) // The caller's FP.
		// 获取调用者的帧指针
		fp = *(*uintptr)(unsafe.Pointer(fp)) // The caller's caller's FP.
		// 获取调用者的调用者的帧指针
	}
	return fp
}
