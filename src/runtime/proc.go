// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/abi"
	"internal/cpu"
	"internal/goarch"
	"internal/goos"
	"internal/runtime/atomic"
	"internal/runtime/exithook"
	"internal/stringslite"
	"runtime/internal/sys"
	"unsafe"
)

// set using cmd/go/internal/modload.ModInfoProg
// 由cmd/go/internal/modload.ModInfoProg设置
var modinfo string

// Goroutine scheduler
// The scheduler's job is to distribute ready-to-run goroutines over worker threads.
// Goroutine调度器
// 调度器的工作是将准备就绪的goroutine分配到工作线程上执行
//
// The main concepts are:
// G - goroutine.
// M - worker thread, or machine.
// P - processor, a resource that is required to execute Go code.
//     M must have an associated P to execute Go code, however it can be
//     blocked or in a syscall w/o an associated P.
// 主要概念:
// G - goroutine，Go的轻量级线程
// M - 工作线程，或称为machine
// P - 处理器，执行Go代码所需的资源
//     M必须关联一个P才能执行Go代码，但在系统调用时可以不关联P
//
// Design doc at https://golang.org/s/go11sched.
// 设计文档见 https://golang.org/s/go11sched

// Worker thread parking/unparking.
// We need to balance between keeping enough running worker threads to utilize
// available hardware parallelism and parking excessive running worker threads
// to conserve CPU resources and power. This is not simple for two reasons:
// (1) scheduler state is intentionally distributed (in particular, per-P work
// queues), so it is not possible to compute global predicates on fast paths;
// (2) for optimal thread management we would need to know the future (don't park
// a worker thread when a new goroutine will be readied in near future).
// 工作线程的挂起和唤醒
// 我们需要在保持足够运行的工作线程以利用可用硬件并行性和挂起过多运行的工作线程以节省CPU资源和功耗之间取得平衡
// 这并不简单，原因有二:
// (1) 调度器状态是故意分布式的(特别是每个P的工作队列)，因此无法在快速路径上计算全局谓词
// (2) 为了最优的线程管理，我们需要预知未来(不要在新goroutine即将就绪时挂起工作线程)
//
// Three rejected approaches that would work badly:
// 1. Centralize all scheduler state (would inhibit scalability).
// 2. Direct goroutine handoff. That is, when we ready a new goroutine and there
//    is a spare P, unpark a thread and handoff it the thread and the goroutine.
//    This would lead to thread state thrashing, as the thread that readied the
//    goroutine can be out of work the very next moment, we will need to park it.
//    Also, it would destroy locality of computation as we want to preserve
//    dependent goroutines on the same thread; and introduce additional latency.
// 3. Unpark an additional thread whenever we ready a goroutine and there is an
//    idle P, but don't do handoff. This would lead to excessive thread parking/
//    unparking as the additional threads will instantly park without discovering
//    any work to do.
// 三个被拒绝的糟糕方案:
// 1. 集中所有调度器状态(会抑制可扩展性)
// 2. 直接goroutine交接。即当新goroutine就绪且有空闲P时，唤醒一个线程并将goroutine交给它
//    这会导致线程状态抖动，因为准备goroutine的线程可能下一秒就没工作了，我们需要挂起它
//    同时，这会破坏计算局部性，因为我们希望将相关的goroutine保留在同一线程上；并引入额外的延迟
// 3. 每当goroutine就绪且有空闲P时就唤醒一个额外线程，但不进行交接
//    这会导致过多的线程挂起/唤醒，因为额外线程会立即挂起而找不到工作
//
// The current approach:
// 当前方案:
//
// This approach applies to three primary sources of potential work: readying a
// goroutine, new/modified-earlier timers, and idle-priority GC. See below for
// additional details.
// 此方案适用于三个主要的工作来源：goroutine就绪、新的/修改的早期定时器，以及空闲优先的GC
// 详见下文
//
// We unpark an additional thread when we submit work if (this is wakep()):
// 1. There is an idle P, and
// 2. There are no "spinning" worker threads.
// 当提交工作时，如果满足以下条件，我们会唤醒一个额外线程(这是wakep()):
// 1. 存在空闲的P，且
// 2. 没有"自旋"的工作线程
//
// A worker thread is considered spinning if it is out of local work and did
// not find work in the global run queue or netpoller; the spinning state is
// denoted in m.spinning and in sched.nmspinning. Threads unparked this way are
// also considered spinning; we don't do goroutine handoff so such threads are
// out of work initially. Spinning threads spin on looking for work in per-P
// run queues and timer heaps or from the GC before parking. If a spinning
// thread finds work it takes itself out of the spinning state and proceeds to
// execution. If it does not find work it takes itself out of the spinning
// state and then parks.
// 当工作线程没有本地工作且未在全局运行队列或网络轮询器中找到工作时，它被认为是自旋的
// 自旋状态由m.spinning和sched.nmspinning表示
// 以这种方式唤醒的线程也被认为是自旋的；我们不进行goroutine交接，因此这些线程最初没有工作
// 自旋线程在挂起前会自旋查找每个P的运行队列、定时器堆或GC中的工作
// 如果自旋线程找到工作，它会退出自旋状态并开始执行
// 如果没找到工作，它会退出自旋状态然后挂起
//
// If there is at least one spinning thread (sched.nmspinning>1), we don't
// unpark new threads when submitting work. To compensate for that, if the last
// spinning thread finds work and stops spinning, it must unpark a new spinning
// thread. This approach smooths out unjustified spikes of thread unparking,
// but at the same time guarantees eventual maximal CPU parallelism
// utilization.
// 如果至少有一个自旋线程(sched.nmspinning>1)，我们在提交工作时不会唤醒新线程
// 为了补偿这一点，如果最后一个自旋线程找到工作并停止自旋，它必须唤醒一个新的自旋线程
// 这种方法可以平滑不合理的线程唤醒峰值，同时保证最终达到最大CPU并行度利用率
//
// The main implementation complication is that we need to be very careful
// during spinning->non-spinning thread transition. This transition can race
// with submission of new work, and either one part or another needs to unpark
// another worker thread. If they both fail to do that, we can end up with
// semi-persistent CPU underutilization.
// 主要实现难点是我们在自旋->非自旋线程转换时需要非常小心
// 这个转换可能与新工作的提交产生竞争，要么这部分要么那部分需要唤醒另一个工作线程
// 如果它们都失败了，我们可能会陷入半持久性的CPU利用率不足
//
// The general pattern for submission is:
// 1. Submit work to the local or global run queue, timer heap, or GC state.
// 2. #StoreLoad-style memory barrier.
// 3. Check sched.nmspinning.
// 提交工作的一般模式是:
// 1. 将工作提交到本地或全局运行队列、定时器堆或GC状态
// 2. #StoreLoad风格的内存屏障
// 3. 检查sched.nmspinning
//
// The general pattern for spinning->non-spinning transition is:
// 1. Decrement nmspinning.
// 2. #StoreLoad-style memory barrier.
// 3. Check all per-P work queues and GC for new work.
// 自旋->非自旋转换的一般模式是:
// 1. 减少nmspinning
// 2. #StoreLoad风格的内存屏障
// 3. 检查所有P的工作队列和GC是否有新工作
//
// Note that all this complexity does not apply to global run queue as we are
// not sloppy about thread unparking when submitting to global queue. Also see
// comments for nmspinning manipulation.
// 注意，所有这些复杂性不适用于全局运行队列，因为我们在提交到全局队列时不会随意唤醒线程
// 另请参阅nmspinning操作的注释
//
// How these different sources of work behave varies, though it doesn't affect
// the synchronization approach:
// 这些不同的工作来源的行为各不相同，但这并不影响同步方法：
// * Ready goroutine: this is an obvious source of work; the goroutine is
//   immediately ready and must run on some thread eventually.
// * 就绪的goroutine：这是一个明显的工作来源；goroutine已经准备就绪，
//   最终必须在某个线程上运行
// * New/modified-earlier timer: The current timer implementation (see time.go)
//   uses netpoll in a thread with no work available to wait for the soonest
//   timer. If there is no thread waiting, we want a new spinning thread to go
//   wait.
// * 新的/修改过的更早的定时器：当前的定时器实现(参见time.go)在没有工作可做时，
//   使用netpoll来等待最近的定时器。如果没有线程在等待，我们希望一个新的自旋线程去等待
// * Idle-priority GC: The GC wakes a stopped idle thread to contribute to
//   background GC work (note: currently disabled per golang.org/issue/19112).
//   Also see golang.org/issue/44313, as this should be extended to all GC
//   workers.
// * 空闲优先级的GC：GC会唤醒一个停止的空闲线程来参与后台GC工作
//   (注意：目前根据golang.org/issue/19112已禁用)。
//   另请参阅golang.org/issue/44313，因为这应该扩展到所有GC工作线程

var (
	// m0 is the initial machine (M) for the program.
	// It is created by the OS and is responsible for running the main goroutine.
	// m0是程序的初始机器(M)
	// 它由操作系统创建，负责运行主goroutine
	m0 m

	// g0 is the initial goroutine for the program.
	// It is created by the runtime and is responsible for running the runtime code.
	// g0是程序的初始goroutine
	// 它由运行时创建，负责运行运行时代码
	g0 g

	// mcache0 is the initial memory cache for the program.
	// It is used to cache memory allocations for the initial M.
	// mcache0是程序的初始内存缓存
	// 它用于缓存初始M的内存分配
	mcache0 *mcache

	// raceprocctx0 is the initial context for race detection.
	// It is used to track the context of the initial goroutine for race detection.
	// raceprocctx0是竞态检测的初始上下文
	// 它用于跟踪初始goroutine的竞态检测上下文
	raceprocctx0 uintptr

	// raceFiniLock is a mutex used to synchronize race detector finalization.
	// It ensures that race detector cleanup operations are thread-safe.
	// raceFiniLock是用于同步竞态检测器终结的互斥锁
	// 它确保竞态检测器清理操作是线程安全的
	raceFiniLock mutex
)

// This slice records the initializing tasks that need to be
// done to start up the runtime. It is built by the linker.
var runtime_inittasks []*initTask

// main_init_done is a signal used by cgocallbackg that initialization
// has been completed. It is made before _cgo_notify_runtime_init_done,
// so all cgo calls can rely on it existing. When main_init is complete,
// it is closed, meaning cgocallbackg can reliably receive from it.
var main_init_done chan bool

//go:linkname main_main main.main
func main_main()

// mainStarted indicates that the main M has started.
var mainStarted bool

// runtimeInitTime is the nanotime() at which the runtime started.
var runtimeInitTime int64

// Value to use for signal mask for newly created M's.
var initSigmask sigset

// The main goroutine.
func main() {
	mp := getg().m

	// Racectx of m0->g0 is used only as the parent of the main goroutine.
	// It must not be used for anything else.
	// m0->g0的racectx仅用作主goroutine的父goroutine
	// 不能用于其他任何用途
	mp.g0.racectx = 0

	// Max stack size is 1 GB on 64-bit, 250 MB on 32-bit.
	// Using decimal instead of binary GB and MB because
	// they look nicer in the stack overflow failure message.
	// 64位系统最大栈大小为1GB，32位系统为250MB
	// 使用十进制而不是二进制的GB和MB是因为
	// 这样在栈溢出错误消息中看起来更美观
	if goarch.PtrSize == 8 {
		maxstacksize = 1000000000
	} else {
		maxstacksize = 250000000
	}

	// An upper limit for max stack size. Used to avoid random crashes
	// after calling SetMaxStack and trying to allocate a stack that is too big,
	// since stackalloc works with 32-bit sizes.
	// 最大栈大小的上限。用于避免在调用SetMaxStack后
	// 尝试分配过大的栈时发生随机崩溃，
	// 因为stackalloc使用32位大小
	maxstackceiling = 2 * maxstacksize

	// Allow newproc to start new Ms.
	// 允许newproc启动新的M
	mainStarted = true

	if haveSysmon {
		systemstack(func() {
			newm(sysmon, nil, -1)
		})
	}

	// Lock the main goroutine onto this, the main OS thread,
	// during initialization. Most programs won't care, but a few
	// do require certain calls to be made by the main thread.
	// Those can arrange for main.main to run in the main thread
	// by calling runtime.LockOSThread during initialization
	// to preserve the lock.
	// 在初始化期间将主goroutine锁定到主OS线程上。
	// 大多数程序不会在意这一点，但少数程序确实需要某些调用在主线程中执行。
	// 这些程序可以通过在初始化期间调用runtime.LockOSThread
	// 来确保main.main在主线程中运行，以保持锁定状态。
	lockOSThread()

	if mp != &m0 {
		throw("runtime.main not on m0")
	}

	// Record when the world started.
	// Must be before doInit for tracing init.
	// 记录世界开始的时间
	// 必须在doInit之前执行，以便追踪初始化过程
	runtimeInitTime = nanotime()
	if runtimeInitTime == 0 {
		throw("nanotime returning zero")
	}

	// 如果启用了初始化追踪，设置追踪信息
	// 记录当前goroutine的ID并激活追踪
	if debug.inittrace != 0 {
		inittrace.id = getg().goid
		inittrace.active = true
	}

	// 执行runtime包的初始化任务
	// 必须在defer之前执行，以确保正确的初始化顺序
	doInit(runtime_inittasks) // Must be before defer.

	// Defer unlock so that runtime.Goexit during init does the unlock too.
	// 延迟解锁，这样在初始化过程中调用runtime.Goexit时也会执行解锁
	// 使用needUnlock标志来控制是否需要解锁，避免重复解锁
	needUnlock := true
	defer func() {
		if needUnlock {
			unlockOSThread()
		}
	}()

	// 启用垃圾回收器
	// 在初始化完成后启动GC，开始管理内存
	gcenable()

	// Create a channel to signal when main initialization is complete
	// 创建一个通道用于通知主初始化完成
	main_init_done = make(chan bool)
	if iscgo {
		// Check for required CGO functions
		// 检查必需的CGO函数是否存在
		if _cgo_pthread_key_created == nil {
			throw("_cgo_pthread_key_created missing")
		}

		if _cgo_thread_start == nil {
			throw("_cgo_thread_start missing")
		}
		// On non-Windows platforms, check for environment variable functions
		// 在非Windows平台上，检查环境变量相关函数
		if GOOS != "windows" {
			if _cgo_setenv == nil {
				throw("_cgo_setenv missing")
			}
			if _cgo_unsetenv == nil {
				throw("_cgo_unsetenv missing")
			}
		}
		// Check for runtime initialization notification function
		// 检查运行时初始化通知函数
		if _cgo_notify_runtime_init_done == nil {
			throw("_cgo_notify_runtime_init_done missing")
		}

		// Set the x_crosscall2_ptr C function pointer variable point to crosscall2.
		// 设置x_crosscall2_ptr C函数指针变量指向crosscall2
		if set_crosscall2 == nil {
			throw("set_crosscall2 missing")
		}
		set_crosscall2()

		// Start the template thread in case we enter Go from
		// a C-created thread and need to create a new thread.
		// 启动模板线程，以防我们从C创建的线程进入Go并需要创建新线程
		startTemplateThread()
		// Notify CGO that runtime initialization is complete
		// 通知CGO运行时初始化已完成
		cgocall(_cgo_notify_runtime_init_done, nil)
	}

	// Run the initializing tasks. Depending on build mode this
	// list can arrive a few different ways, but it will always
	// contain the init tasks computed by the linker for all the
	// packages in the program (excluding those added at runtime
	// by package plugin). Run through the modules in dependency
	// order (the order they are initialized by the dynamic
	// loader, i.e. they are added to the moduledata linked list).
	// 运行初始化任务。根据构建模式，这个列表可能以几种不同的方式到达，
	// 但它总是包含链接器为程序中所有包计算的init任务
	// (不包括在运行时由plugin包添加的任务)。
	// 按照依赖顺序遍历模块(它们被动态加载器初始化的顺序，
	// 即它们被添加到moduledata链表中的顺序)。
	for m := &firstmoduledata; m != nil; m = m.next {
		doInit(m.inittasks)
	}

	// Disable init tracing after main init done to avoid overhead
	// of collecting statistics in malloc and newproc
	// 主初始化完成后禁用init跟踪，以避免在malloc和newproc中收集统计信息的开销
	inittrace.active = false

	// 关闭主初始化完成通道，通知其他等待的goroutine初始化已完成
	close(main_init_done)

	// 标记不需要锁定，并解锁OS线程
	needUnlock = false
	unlockOSThread()

	if isarchive || islibrary {
		// A program compiled with -buildmode=c-archive or c-shared
		// has a main, but it is not executed.
		// 如果程序是以c-archive或c-shared模式编译的，
		// 虽然它有main函数，但不会被执行
		return
	}
	fn := main_main // make an indirect call, as the linker doesn't know the address of the main package when laying down the runtime
	// 使用间接调用，因为在链接器设置运行时的时候还不知道main包的地址
	fn()
	if raceenabled {
		runExitHooks(0) // run hooks now, since racefini does not return
		// 如果启用了竞态检测，现在运行退出钩子，因为racefini不会返回
		racefini()
	}

	// Make racy client program work: if panicking on
	// another goroutine at the same time as main returns,
	// let the other goroutine finish printing the panic trace.
	// Once it does, it will exit. See issues 3934 and 20018.
	// 让竞态客户端程序正常工作：如果在main返回的同时另一个goroutine正在panic，
	// 让那个goroutine完成打印panic跟踪信息。一旦完成，它就会退出。
	// 参见issues 3934和20018
	if runningPanicDefers.Load() != 0 {
		// Running deferred functions should not take long.
		// 运行deferred函数不应该花费太长时间
		for c := 0; c < 1000; c++ {
			if runningPanicDefers.Load() == 0 {
				break
			}
			Gosched()
		}
	}
	if panicking.Load() != 0 {
		// 如果还有panic正在进行，将当前goroutine永久挂起
		gopark(nil, nil, waitReasonPanicWait, traceBlockForever, 1)
	}
	runExitHooks(0)

	exit(0)
	// 以下代码永远不会执行，用于确保程序不会意外返回
	for {
		var x *int32
		*x = 0
	}
}

// os_beforeExit is called from os.Exit(0).
//
//go:linkname os_beforeExit os.runtime_beforeExit
func os_beforeExit(exitCode int) {
	runExitHooks(exitCode)
	if exitCode == 0 && raceenabled {
		racefini()
	}
}

func init() {
	exithook.Gosched = Gosched
	exithook.Goid = func() uint64 { return getg().goid }
	exithook.Throw = throw
}

func runExitHooks(code int) {
	exithook.Run(code)
}

// start forcegc helper goroutine
func init() {
	go forcegchelper()
}

func forcegchelper() {
	forcegc.g = getg()
	lockInit(&forcegc.lock, lockRankForcegc)
	for {
		lock(&forcegc.lock)
		if forcegc.idle.Load() {
			throw("forcegc: phase error")
		}
		forcegc.idle.Store(true)
		goparkunlock(&forcegc.lock, waitReasonForceGCIdle, traceBlockSystemGoroutine, 1)
		// this goroutine is explicitly resumed by sysmon
		if debug.gctrace > 0 {
			println("GC forced")
		}
		// Time-triggered, fully concurrent.
		gcStart(gcTrigger{kind: gcTriggerTime, now: nanotime()})
	}
}

// Gosched yields the processor, allowing other goroutines to run. It does not
// suspend the current goroutine, so execution resumes automatically.
//
//go:nosplit
func Gosched() {
	checkTimeouts()
	mcall(gosched_m)
}

// goschedguarded yields the processor like gosched, but also checks
// for forbidden states and opts out of the yield in those cases.
//
//go:nosplit
func goschedguarded() {
	mcall(goschedguarded_m)
}

// goschedIfBusy yields the processor like gosched, but only does so if
// there are no idle Ps or if we're on the only P and there's nothing in
// the run queue. In both cases, there is freely available idle time.
//
//go:nosplit
func goschedIfBusy() {
	gp := getg()
	// Call gosched if gp.preempt is set; we may be in a tight loop that
	// doesn't otherwise yield.
	if !gp.preempt && sched.npidle.Load() > 0 {
		return
	}
	mcall(gosched_m)
}

// Puts the current goroutine into a waiting state and calls unlockf on the
// system stack.
//
// If unlockf returns false, the goroutine is resumed.
//
// unlockf must not access this G's stack, as it may be moved between
// the call to gopark and the call to unlockf.
//
// Note that because unlockf is called after putting the G into a waiting
// state, the G may have already been readied by the time unlockf is called
// unless there is external synchronization preventing the G from being
// readied. If unlockf returns false, it must guarantee that the G cannot be
// externally readied.
//
// Reason explains why the goroutine has been parked. It is displayed in stack
// traces and heap dumps. Reasons should be unique and descriptive. Do not
// re-use reasons, add new ones.
//
// gopark should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - gvisor.dev/gvisor
//   - github.com/sagernet/gvisor
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname gopark
func gopark(unlockf func(*g, unsafe.Pointer) bool, lock unsafe.Pointer, reason waitReason, traceReason traceBlockReason, traceskip int) {
	if reason != waitReasonSleep {
		checkTimeouts() // timeouts may expire while two goroutines keep the scheduler busy
	}
	mp := acquirem()
	gp := mp.curg
	status := readgstatus(gp)
	if status != _Grunning && status != _Gscanrunning {
		throw("gopark: bad g status")
	}
	mp.waitlock = lock
	mp.waitunlockf = unlockf
	gp.waitreason = reason
	mp.waitTraceBlockReason = traceReason
	mp.waitTraceSkip = traceskip
	releasem(mp)
	// can't do anything that might move the G between Ms here.
	mcall(park_m)
}

// Puts the current goroutine into a waiting state and unlocks the lock.
// The goroutine can be made runnable again by calling goready(gp).
func goparkunlock(lock *mutex, reason waitReason, traceReason traceBlockReason, traceskip int) {
	gopark(parkunlock_c, unsafe.Pointer(lock), reason, traceReason, traceskip)
}

// goready should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - gvisor.dev/gvisor
//   - github.com/sagernet/gvisor
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname goready
func goready(gp *g, traceskip int) {
	systemstack(func() {
		ready(gp, traceskip, true)
	})
}

//go:nosplit
func acquireSudog() *sudog {
	// Delicate dance: the semaphore implementation calls
	// acquireSudog, acquireSudog calls new(sudog),
	// new calls malloc, malloc can call the garbage collector,
	// and the garbage collector calls the semaphore implementation
	// in stopTheWorld.
	// Break the cycle by doing acquirem/releasem around new(sudog).
	// The acquirem/releasem increments m.locks during new(sudog),
	// which keeps the garbage collector from being invoked.
	mp := acquirem()
	pp := mp.p.ptr()
	if len(pp.sudogcache) == 0 {
		lock(&sched.sudoglock)
		// First, try to grab a batch from central cache.
		for len(pp.sudogcache) < cap(pp.sudogcache)/2 && sched.sudogcache != nil {
			s := sched.sudogcache
			sched.sudogcache = s.next
			s.next = nil
			pp.sudogcache = append(pp.sudogcache, s)
		}
		unlock(&sched.sudoglock)
		// If the central cache is empty, allocate a new one.
		if len(pp.sudogcache) == 0 {
			pp.sudogcache = append(pp.sudogcache, new(sudog))
		}
	}
	n := len(pp.sudogcache)
	s := pp.sudogcache[n-1]
	pp.sudogcache[n-1] = nil
	pp.sudogcache = pp.sudogcache[:n-1]
	if s.elem != nil {
		throw("acquireSudog: found s.elem != nil in cache")
	}
	releasem(mp)
	return s
}

//go:nosplit
func releaseSudog(s *sudog) {
	if s.elem != nil {
		throw("runtime: sudog with non-nil elem")
	}
	if s.isSelect {
		throw("runtime: sudog with non-false isSelect")
	}
	if s.next != nil {
		throw("runtime: sudog with non-nil next")
	}
	if s.prev != nil {
		throw("runtime: sudog with non-nil prev")
	}
	if s.waitlink != nil {
		throw("runtime: sudog with non-nil waitlink")
	}
	if s.c != nil {
		throw("runtime: sudog with non-nil c")
	}
	gp := getg()
	if gp.param != nil {
		throw("runtime: releaseSudog with non-nil gp.param")
	}
	mp := acquirem() // avoid rescheduling to another P
	pp := mp.p.ptr()
	if len(pp.sudogcache) == cap(pp.sudogcache) {
		// Transfer half of local cache to the central cache.
		var first, last *sudog
		for len(pp.sudogcache) > cap(pp.sudogcache)/2 {
			n := len(pp.sudogcache)
			p := pp.sudogcache[n-1]
			pp.sudogcache[n-1] = nil
			pp.sudogcache = pp.sudogcache[:n-1]
			if first == nil {
				first = p
			} else {
				last.next = p
			}
			last = p
		}
		lock(&sched.sudoglock)
		last.next = sched.sudogcache
		sched.sudogcache = first
		unlock(&sched.sudoglock)
	}
	pp.sudogcache = append(pp.sudogcache, s)
	releasem(mp)
}

// called from assembly.
func badmcall(fn func(*g)) {
	throw("runtime: mcall called on m->g0 stack")
}

func badmcall2(fn func(*g)) {
	throw("runtime: mcall function returned")
}

func badreflectcall() {
	panic(plainError("arg size to reflect.call more than 1GB"))
}

//go:nosplit
//go:nowritebarrierrec
func badmorestackg0() {
	if !crashStackImplemented {
		writeErrStr("fatal: morestack on g0\n")
		return
	}

	g := getg()
	switchToCrashStack(func() {
		print("runtime: morestack on g0, stack [", hex(g.stack.lo), " ", hex(g.stack.hi), "], sp=", hex(g.sched.sp), ", called from\n")
		g.m.traceback = 2 // include pc and sp in stack trace
		traceback1(g.sched.pc, g.sched.sp, g.sched.lr, g, 0)
		print("\n")

		throw("morestack on g0")
	})
}

//go:nosplit
//go:nowritebarrierrec
func badmorestackgsignal() {
	writeErrStr("fatal: morestack on gsignal\n")
}

//go:nosplit
func badctxt() {
	throw("ctxt != 0")
}

// gcrash is a fake g that can be used when crashing due to bad
// stack conditions.
var gcrash g

var crashingG atomic.Pointer[g]

// Switch to crashstack and call fn, with special handling of
// concurrent and recursive cases.
//
// Nosplit as it is called in a bad stack condition (we know
// morestack would fail).
//
//go:nosplit
//go:nowritebarrierrec
func switchToCrashStack(fn func()) {
	me := getg()
	if crashingG.CompareAndSwapNoWB(nil, me) {
		switchToCrashStack0(fn) // should never return
		abort()
	}
	if crashingG.Load() == me {
		// recursive crashing. too bad.
		writeErrStr("fatal: recursive switchToCrashStack\n")
		abort()
	}
	// Another g is crashing. Give it some time, hopefully it will finish traceback.
	usleep_no_g(100)
	writeErrStr("fatal: concurrent switchToCrashStack\n")
	abort()
}

// Disable crash stack on Windows for now. Apparently, throwing an exception
// on a non-system-allocated crash stack causes EXCEPTION_STACK_OVERFLOW and
// hangs the process (see issue 63938).
const crashStackImplemented = GOOS != "windows"

//go:noescape
func switchToCrashStack0(fn func()) // in assembly

func lockedOSThread() bool {
	gp := getg()
	return gp.lockedm != 0 && gp.m.lockedg != 0
}

var (
	// allgs contains all Gs ever created (including dead Gs), and thus
	// never shrinks.
	//
	// Access via the slice is protected by allglock or stop-the-world.
	// Readers that cannot take the lock may (carefully!) use the atomic
	// variables below.
	// allgs包含所有曾经创建的G(包括已死亡的G)，因此永远不会收缩
	// 通过切片访问受allglock或stop-the-world保护
	// 无法获取锁的读者可以(小心地!)使用下面的原子变量
	allglock mutex
	allgs    []*g

	// allglen and allgptr are atomic variables that contain len(allgs) and
	// &allgs[0] respectively. Proper ordering depends on totally-ordered
	// loads and stores. Writes are protected by allglock.
	//
	// allgptr is updated before allglen. Readers should read allglen
	// before allgptr to ensure that allglen is always <= len(allgptr). New
	// Gs appended during the race can be missed. For a consistent view of
	// all Gs, allglock must be held.
	//
	// allgptr copies should always be stored as a concrete type or
	// unsafe.Pointer, not uintptr, to ensure that GC can still reach it
	// even if it points to a stale array.
	// allglen和allgptr是原子变量，分别包含len(allgs)和&allgs[0]
	// 正确的顺序依赖于完全有序的加载和存储
	// 写入受allglock保护
	//
	// allgptr在allglen之前更新
	// 读者应该先读取allglen再读取allgptr，以确保allglen始终<=len(allgptr)
	// 在竞态期间追加的新G可能会被遗漏
	// 要获得所有G的一致视图，必须持有allglock
	//
	// allgptr的副本应该始终存储为具体类型或unsafe.Pointer，而不是uintptr
	// 以确保即使它指向一个过时的数组，GC仍然可以访问它
	allglen uintptr
	allgptr **g
)

func allgadd(gp *g) {
	// Check if the goroutine is in _Gidle state, which is invalid for adding to allgs
	// 检查goroutine是否处于_Gidle状态，这种状态不能添加到allgs中
	if readgstatus(gp) == _Gidle {
		throw("allgadd: bad status Gidle")
	}

	// Lock the allglock mutex to protect concurrent access to allgs
	// 锁定allglock互斥锁以保护对allgs的并发访问
	lock(&allglock)

	// Append the new goroutine to the allgs slice
	// 将新的goroutine追加到allgs切片中
	allgs = append(allgs, gp)

	// Update allgptr if the address of the first element has changed
	// 如果第一个元素的地址发生变化，则更新allgptr
	if &allgs[0] != allgptr {
		atomicstorep(unsafe.Pointer(&allgptr), unsafe.Pointer(&allgs[0]))
	}

	// Update the atomic length counter
	// 更新原子长度计数器
	atomic.Storeuintptr(&allglen, uintptr(len(allgs)))

	// Release the lock
	// 释放锁
	unlock(&allglock)
}

// allGsSnapshot returns a snapshot of the slice of all Gs.
//
// The world must be stopped or allglock must be held.
func allGsSnapshot() []*g {
	assertWorldStoppedOrLockHeld(&allglock)

	// Because the world is stopped or allglock is held, allgadd
	// cannot happen concurrently with this. allgs grows
	// monotonically and existing entries never change, so we can
	// simply return a copy of the slice header. For added safety,
	// we trim everything past len because that can still change.
	return allgs[:len(allgs):len(allgs)]
}

// atomicAllG returns &allgs[0] and len(allgs) for use with atomicAllGIndex.
func atomicAllG() (**g, uintptr) {
	length := atomic.Loaduintptr(&allglen)
	ptr := (**g)(atomic.Loadp(unsafe.Pointer(&allgptr)))
	return ptr, length
}

// atomicAllGIndex returns ptr[i] with the allgptr returned from atomicAllG.
func atomicAllGIndex(ptr **g, i uintptr) *g {
	return *(**g)(add(unsafe.Pointer(ptr), i*goarch.PtrSize))
}

// forEachG calls fn on every G from allgs.
//
// forEachG takes a lock to exclude concurrent addition of new Gs.
func forEachG(fn func(gp *g)) {
	lock(&allglock)
	for _, gp := range allgs {
		fn(gp)
	}
	unlock(&allglock)
}

// forEachGRace calls fn on every G from allgs.
//
// forEachGRace avoids locking, but does not exclude addition of new Gs during
// execution, which may be missed.
func forEachGRace(fn func(gp *g)) {
	ptr, length := atomicAllG()
	for i := uintptr(0); i < length; i++ {
		gp := atomicAllGIndex(ptr, i)
		fn(gp)
	}
	return
}

const (
	// Number of goroutine ids to grab from sched.goidgen to local per-P cache at once.
	// 16 seems to provide enough amortization, but other than that it's mostly arbitrary number.
	// 从sched.goidgen一次性获取到每个P的本地缓存中的goroutine ID数量
	// 16这个数字似乎提供了足够的分摊效果，除此之外，这主要是一个任意选择的数字
	_GoidCacheBatch = 16
)

// cpuinit sets up CPU feature flags and calls internal/cpu.Initialize. env should be the complete
// value of the GODEBUG environment variable.
func cpuinit(env string) {
	switch GOOS {
	case "aix", "darwin", "ios", "dragonfly", "freebsd", "netbsd", "openbsd", "illumos", "solaris", "linux":
		cpu.DebugOptions = true
	}
	cpu.Initialize(env)

	// Support cpu feature variables are used in code generated by the compiler
	// to guard execution of instructions that can not be assumed to be always supported.
	switch GOARCH {
	case "386", "amd64":
		x86HasPOPCNT = cpu.X86.HasPOPCNT
		x86HasSSE41 = cpu.X86.HasSSE41
		x86HasFMA = cpu.X86.HasFMA

	case "arm":
		armHasVFPv4 = cpu.ARM.HasVFPv4

	case "arm64":
		arm64HasATOMICS = cpu.ARM64.HasATOMICS
	}
}

// getGodebugEarly extracts the environment variable GODEBUG from the environment on
// Unix-like operating systems and returns it. This function exists to extract GODEBUG
// early before much of the runtime is initialized.
func getGodebugEarly() string {
	const prefix = "GODEBUG="
	var env string
	switch GOOS {
	case "aix", "darwin", "ios", "dragonfly", "freebsd", "netbsd", "openbsd", "illumos", "solaris", "linux":
		// Similar to goenv_unix but extracts the environment value for
		// GODEBUG directly.
		// TODO(moehrmann): remove when general goenvs() can be called before cpuinit()
		n := int32(0)
		for argv_index(argv, argc+1+n) != nil {
			n++
		}

		for i := int32(0); i < n; i++ {
			p := argv_index(argv, argc+1+i)
			s := unsafe.String(p, findnull(p))

			if stringslite.HasPrefix(s, prefix) {
				env = gostring(p)[len(prefix):]
				break
			}
		}
	}
	return env
}

// The bootstrap sequence is:
//
//	call osinit
//	call schedinit
//	make & queue new G
//	call runtime·mstart
//
// The new G calls runtime·main.

// 引导序列如下:
//
//	调用osinit - 初始化操作系统相关的设置
//	调用schedinit - 初始化调度器
//	创建并排队新的G - 创建主goroutine
//	调用runtime·mstart - 启动M(机器线程)
//
// 新创建的G会调用runtime·main函数
func schedinit() {
	// Initialize various locks in the scheduler
	// 初始化调度器中的各种锁
	lockInit(&sched.lock, lockRankSched)             // 调度器主锁
	lockInit(&sched.sysmonlock, lockRankSysmon)      // 系统监控锁
	lockInit(&sched.deferlock, lockRankDefer)        // defer锁
	lockInit(&sched.sudoglock, lockRankSudog)        // sudog锁
	lockInit(&deadlock, lockRankDeadlock)            // 死锁检测锁
	lockInit(&paniclk, lockRankPanic)                // panic锁
	lockInit(&allglock, lockRankAllg)                // 所有goroutine锁
	lockInit(&allpLock, lockRankAllp)                // 所有P锁
	lockInit(&reflectOffs.lock, lockRankReflectOffs) // 反射偏移量锁
	lockInit(&finlock, lockRankFin)                  // 终结器锁
	lockInit(&cpuprof.lock, lockRankCpuprof)         // CPU分析锁

	// Initialize specialized locks with multiple rank types
	// 初始化具有多种等级类型的专用锁
	allocmLock.init(lockRankAllocmR, lockRankAllocmRInternal, lockRankAllocmW) // M分配锁
	execLock.init(lockRankExecR, lockRankExecRInternal, lockRankExecW)         // 执行锁

	// Initialize tracing locks
	// 初始化追踪锁
	traceLockInit()

	// Enforce that this lock is always a leaf lock.
	// All of this lock's critical sections should be
	// extremely short.
	// 强制此锁始终为叶子锁
	// 此锁的所有临界区应该非常短
	lockInit(&memstats.heapStats.noPLock, lockRankLeafRank) // 堆统计无P锁

	// raceinit must be the first call to race detector.
	// In particular, it must be done before mallocinit below calls racemapshadow.
	// raceinit必须是竞态检测器的第一个调用
	// 特别是，它必须在mallocinit调用racemapshadow之前完成
	gp := getg()
	if raceenabled {
		gp.racectx, raceprocctx0 = raceinit()
	}

	// 设置调度器最大M数量为10000
	sched.maxmcount = 10000
	// 初始化崩溃文件描述符为无效值
	crashFD.Store(^uintptr(0))

	// The world starts stopped.
	// 世界初始状态为停止状态
	worldStopped()

	// 初始化时钟，尽可能早地运行
	ticks.init() // run as early as possible
	// 验证模块数据
	moduledataverify()
	// 初始化栈分配器
	stackinit()
	// 初始化内存分配器
	mallocinit()
	// 获取早期调试设置
	godebug := getGodebugEarly()
	// 初始化CPU相关设置，必须在alginit之前运行
	cpuinit(godebug) // must run before alginit
	// 初始化随机数生成器，必须在alginit和mcommoninit之前运行
	randinit() // must run before alginit, mcommoninit
	// 初始化算法相关设置，在此调用之前不能使用maps、hash和rand
	alginit() // maps, hash, rand must not be used before this call
	// 初始化M的通用设置
	mcommoninit(gp.m, -1)
	// 初始化模块，提供activeModules
	modulesinit() // provides activeModules
	// 初始化类型链接，使用maps和activeModules
	typelinksinit() // uses maps, activeModules
	// 初始化接口表，使用activeModules
	itabsinit() // uses activeModules
	// 初始化栈对象，必须在GC启动之前运行
	stkobjinit() // must run before GC starts

	// 保存当前M的信号掩码
	sigsave(&gp.m.sigmask)
	// 设置初始信号掩码
	initSigmask = gp.m.sigmask

	// 初始化命令行参数
	goargs()
	// 初始化环境变量
	goenvs()
	// 执行安全检查
	secure()
	// 检查文件描述符
	checkfds()
	// 解析调试变量
	parsedebugvars()
	// 初始化垃圾回收器
	gcinit()

	// Allocate stack space that can be used when crashing due to bad stack
	// conditions, e.g. morestack on g0.
	// 为崩溃时分配栈空间，用于处理栈条件错误的情况，例如g0上的morestack
	gcrash.stack = stackalloc(16384)
	gcrash.stackguard0 = gcrash.stack.lo + 1000
	gcrash.stackguard1 = gcrash.stack.lo + 1000

	// if disableMemoryProfiling is set, update MemProfileRate to 0 to turn off memprofile.
	// Note: parsedebugvars may update MemProfileRate, but when disableMemoryProfiling is
	// set to true by the linker, it means that nothing is consuming the profile, it is
	// safe to set MemProfileRate to 0.
	// 如果禁用了内存分析，将MemProfileRate设置为0以关闭内存分析
	// 注意：parsedebugvars可能会更新MemProfileRate，但当linker将disableMemoryProfiling设置为true时，
	// 意味着没有东西在消费这个profile，此时将MemProfileRate设置为0是安全的
	if disableMemoryProfiling {
		MemProfileRate = 0
	}

	// mcommoninit runs before parsedebugvars, so init profstacks again.
	// mcommoninit在parsedebugvars之前运行，所以需要重新初始化profstacks
	mProfStackInit(gp.m)

	lock(&sched.lock)
	// Store the current time as the last polling time
	// 存储当前时间作为最后一次轮询时间
	sched.lastpoll.Store(nanotime())

	// Set the number of processors to the number of CPU cores by default
	// 默认将处理器数量设置为CPU核心数
	procs := ncpu

	// Check if GOMAXPROCS environment variable is set and valid
	// 检查是否设置了GOMAXPROCS环境变量且有效
	if n, ok := atoi32(gogetenv("GOMAXPROCS")); ok && n > 0 {
		// If valid, use the value from GOMAXPROCS
		// 如果有效，使用GOMAXPROCS的值
		procs = n
	}

	// Resize the number of processors and check for any runnable goroutines
	// 调整处理器数量并检查是否有任何可运行的goroutine
	if procresize(procs) != nil {
		// If there are runnable goroutines during bootstrap, throw an error
		// 如果在引导过程中发现可运行的goroutine，抛出错误
		throw("unknown runnable goroutine during bootstrap")
	}
	unlock(&sched.lock)

	// World is effectively started now, as P's can run.
	// 现在世界实际上已经启动了，因为P可以运行了
	worldStarted()

	if buildVersion == "" {
		// Condition should never trigger. This code just serves
		// to ensure runtime·buildVersion is kept in the resulting binary.
		// 这个条件永远不会触发。这段代码只是用来确保
		// runtime·buildVersion被保留在最终生成的二进制文件中
		buildVersion = "unknown"
	}
	if len(modinfo) == 1 {
		// Condition should never trigger. This code just serves
		// to ensure runtime·modinfo is kept in the resulting binary.
		// 这个条件永远不会触发。这段代码只是用来确保
		// runtime·modinfo被保留在最终生成的二进制文件中
		modinfo = ""
	}
}

func dumpgstatus(gp *g) {
	thisg := getg()
	print("runtime:   gp: gp=", gp, ", goid=", gp.goid, ", gp->atomicstatus=", readgstatus(gp), "\n")
	print("runtime: getg:  g=", thisg, ", goid=", thisg.goid, ",  g->atomicstatus=", readgstatus(thisg), "\n")
}

// sched.lock must be held.
func checkmcount() {
	assertLockHeld(&sched.lock)

	// Exclude extra M's, which are used for cgocallback from threads
	// created in C.
	//
	// The purpose of the SetMaxThreads limit is to avoid accidental fork
	// bomb from something like millions of goroutines blocking on system
	// calls, causing the runtime to create millions of threads. By
	// definition, this isn't a problem for threads created in C, so we
	// exclude them from the limit. See https://go.dev/issue/60004.
	// 排除额外的M，这些M用于来自C语言创建的线程的cgocallback
	//
	// SetMaxThreads限制的目的是避免意外的fork炸弹，比如数百万个goroutine
	// 在系统调用上阻塞，导致运行时创建数百万个线程。根据定义，这对于在C中
	// 创建的线程来说不是问题，所以我们将它们排除在限制之外。
	// 参见 https://go.dev/issue/60004
	count := mcount() - int32(extraMInUse.Load()) - int32(extraMLength.Load())
	if count > sched.maxmcount {
		print("runtime: program exceeds ", sched.maxmcount, "-thread limit\n")
		throw("thread exhaustion")
	}
}

// mReserveID returns the next ID to use for a new m. This new m is immediately
// considered 'running' by checkdead.
//
// sched.lock must be held.
// mReserveID 返回用于新m的下一个ID。这个新m会被checkdead立即视为"运行中"。
//
// 调用此函数时必须持有sched.lock锁。
func mReserveID() int64 {
	assertLockHeld(&sched.lock)

	// 检查mnext是否会溢出
	// 如果mnext+1小于mnext，说明发生了整数溢出
	if sched.mnext+1 < sched.mnext {
		throw("runtime: thread ID overflow")
	}
	// 获取当前mnext值作为新m的ID
	id := sched.mnext
	// 递增mnext计数器
	sched.mnext++
	// 检查m的数量是否超过限制
	checkmcount()
	return id
}

// Pre-allocated ID may be passed as 'id', or omitted by passing -1.
// 预分配的ID可以作为'id'传入，或者通过传入-1来省略。
func mcommoninit(mp *m, id int64) {
	gp := getg()

	// g0 stack won't make sense for user (and is not necessary unwindable).
	// g0栈对用户来说没有意义（而且不需要可展开）。
	if gp != gp.m.g0 {
		callers(1, mp.createstack[:])
	}

	lock(&sched.lock)

	if id >= 0 {
		mp.id = id
	} else {
		mp.id = mReserveID()
	}

	mrandinit(mp)

	mpreinit(mp)
	if mp.gsignal != nil {
		mp.gsignal.stackguard1 = mp.gsignal.stack.lo + stackGuard
	}

	// Add to allm so garbage collector doesn't free g->m
	// when it is just in a register or thread-local storage.
	// 添加到allm中，这样垃圾收集器就不会在g->m仅存在于寄存器或线程本地存储中时释放它。
	mp.alllink = allm

	// NumCgoCall() and others iterate over allm w/o schedlock,
	// so we need to publish it safely.
	// NumCgoCall()和其他函数在没有schedlock的情况下遍历allm，
	// 所以我们需要安全地发布它。
	atomicstorep(unsafe.Pointer(&allm), unsafe.Pointer(mp))
	unlock(&sched.lock)

	// Allocate memory to hold a cgo traceback if the cgo call crashes.
	// 如果cgo调用崩溃，分配内存来保存cgo回溯信息。
	if iscgo || GOOS == "solaris" || GOOS == "illumos" || GOOS == "windows" {
		mp.cgoCallers = new(cgoCallers)
	}
	mProfStackInit(mp)
}

// mProfStackInit is used to eagerly initialize stack trace buffers for
// profiling. Lazy allocation would have to deal with reentrancy issues in
// malloc and runtime locks for mLockProfile.
// TODO(mknyszek): Implement lazy allocation if this becomes a problem.
// mProfStackInit用于急切地初始化用于性能分析的栈跟踪缓冲区。
// 延迟分配将不得不处理malloc和mLockProfile的运行时锁中的重入问题。
// TODO(mknyszek): 如果这成为一个问题，实现延迟分配。
func mProfStackInit(mp *m) {
	if debug.profstackdepth == 0 {
		// debug.profstack is set to 0 by the user, or we're being called from
		// schedinit before parsedebugvars.
		// debug.profstack由用户设置为0，或者我们在parsedebugvars之前从schedinit被调用。
		return
	}
	mp.profStack = makeProfStackFP()
	mp.mLockProfile.stack = makeProfStackFP()
}

// makeProfStackFP creates a buffer large enough to hold a maximum-sized stack
// trace as well as any additional frames needed for frame pointer unwinding
// with delayed inline expansion.
func makeProfStackFP() []uintptr {
	// The "1" term is to account for the first stack entry being
	// taken up by a "skip" sentinel value for profilers which
	// defer inline frame expansion until the profile is reported.
	// The "maxSkip" term is for frame pointer unwinding, where we
	// want to end up with debug.profstackdebth frames but will discard
	// some "physical" frames to account for skipping.
	return make([]uintptr, 1+maxSkip+debug.profstackdepth)
}

// makeProfStack returns a buffer large enough to hold a maximum-sized stack
// trace.
func makeProfStack() []uintptr { return make([]uintptr, debug.profstackdepth) }

//go:linkname pprof_makeProfStack
func pprof_makeProfStack() []uintptr { return makeProfStack() }

func (mp *m) becomeSpinning() {
	mp.spinning = true
	sched.nmspinning.Add(1)
	sched.needspinning.Store(0)
}

func (mp *m) hasCgoOnStack() bool {
	return mp.ncgo > 0 || mp.isextra
}

const (
	// osHasLowResTimer indicates that the platform's internal timer system has a low resolution,
	// typically on the order of 1 ms or more.
	osHasLowResTimer = GOOS == "windows" || GOOS == "openbsd" || GOOS == "netbsd"

	// osHasLowResClockInt is osHasLowResClock but in integer form, so it can be used to create
	// constants conditionally.
	osHasLowResClockInt = goos.IsWindows

	// osHasLowResClock indicates that timestamps produced by nanotime on the platform have a
	// low resolution, typically on the order of 1 ms or more.
	osHasLowResClock = osHasLowResClockInt > 0
)

// Mark gp ready to run.
func ready(gp *g, traceskip int, next bool) {
	status := readgstatus(gp)

	// Mark runnable.
	mp := acquirem() // disable preemption because it can be holding p in a local var
	if status&^_Gscan != _Gwaiting {
		dumpgstatus(gp)
		throw("bad g->status in ready")
	}

	// status is Gwaiting or Gscanwaiting, make Grunnable and put on runq
	trace := traceAcquire()
	casgstatus(gp, _Gwaiting, _Grunnable)
	if trace.ok() {
		trace.GoUnpark(gp, traceskip)
		traceRelease(trace)
	}
	runqput(mp.p.ptr(), gp, next)
	wakep()
	releasem(mp)
}

// freezeStopWait is a large value that freezetheworld sets
// sched.stopwait to in order to request that all Gs permanently stop.
const freezeStopWait = 0x7fffffff

// freezing is set to non-zero if the runtime is trying to freeze the
// world.
var freezing atomic.Bool

// Similar to stopTheWorld but best-effort and can be called several times.
// There is no reverse operation, used during crashing.
// This function must not lock any mutexes.
func freezetheworld() {
	freezing.Store(true)
	if debug.dontfreezetheworld > 0 {
		// Don't prempt Ps to stop goroutines. That will perturb
		// scheduler state, making debugging more difficult. Instead,
		// allow goroutines to continue execution.
		//
		// fatalpanic will tracebackothers to trace all goroutines. It
		// is unsafe to trace a running goroutine, so tracebackothers
		// will skip running goroutines. That is OK and expected, we
		// expect users of dontfreezetheworld to use core files anyway.
		//
		// However, allowing the scheduler to continue running free
		// introduces a race: a goroutine may be stopped when
		// tracebackothers checks its status, and then start running
		// later when we are in the middle of traceback, potentially
		// causing a crash.
		//
		// To mitigate this, when an M naturally enters the scheduler,
		// schedule checks if freezing is set and if so stops
		// execution. This guarantees that while Gs can transition from
		// running to stopped, they can never transition from stopped
		// to running.
		//
		// The sleep here allows racing Ms that missed freezing and are
		// about to run a G to complete the transition to running
		// before we start traceback.
		usleep(1000)
		return
	}

	// stopwait and preemption requests can be lost
	// due to races with concurrently executing threads,
	// so try several times
	for i := 0; i < 5; i++ {
		// this should tell the scheduler to not start any new goroutines
		sched.stopwait = freezeStopWait
		sched.gcwaiting.Store(true)
		// this should stop running goroutines
		if !preemptall() {
			break // no running goroutines
		}
		usleep(1000)
	}
	// to be sure
	usleep(1000)
	preemptall()
	usleep(1000)
}

// All reads and writes of g's status go through readgstatus, casgstatus
// castogscanstatus, casfrom_Gscanstatus.
//
//go:nosplit
func readgstatus(gp *g) uint32 {
	return gp.atomicstatus.Load()
}

// The Gscanstatuses are acting like locks and this releases them.
// If it proves to be a performance hit we should be able to make these
// simple atomic stores but for now we are going to throw if
// we see an inconsistent state.
func casfrom_Gscanstatus(gp *g, oldval, newval uint32) {
	success := false

	// Check that transition is valid.
	switch oldval {
	default:
		print("runtime: casfrom_Gscanstatus bad oldval gp=", gp, ", oldval=", hex(oldval), ", newval=", hex(newval), "\n")
		dumpgstatus(gp)
		throw("casfrom_Gscanstatus:top gp->status is not in scan state")
	case _Gscanrunnable,
		_Gscanwaiting,
		_Gscanrunning,
		_Gscansyscall,
		_Gscanpreempted:
		if newval == oldval&^_Gscan {
			success = gp.atomicstatus.CompareAndSwap(oldval, newval)
		}
	}
	if !success {
		print("runtime: casfrom_Gscanstatus failed gp=", gp, ", oldval=", hex(oldval), ", newval=", hex(newval), "\n")
		dumpgstatus(gp)
		throw("casfrom_Gscanstatus: gp->status is not in scan state")
	}
	releaseLockRankAndM(lockRankGscan)
}

// This will return false if the gp is not in the expected status and the cas fails.
// This acts like a lock acquire while the casfromgstatus acts like a lock release.
func castogscanstatus(gp *g, oldval, newval uint32) bool {
	switch oldval {
	case _Grunnable,
		_Grunning,
		_Gwaiting,
		_Gsyscall:
		if newval == oldval|_Gscan {
			r := gp.atomicstatus.CompareAndSwap(oldval, newval)
			if r {
				acquireLockRankAndM(lockRankGscan)
			}
			return r

		}
	}
	print("runtime: castogscanstatus oldval=", hex(oldval), " newval=", hex(newval), "\n")
	throw("castogscanstatus")
	panic("not reached")
}

// casgstatusAlwaysTrack is a debug flag that causes casgstatus to always track
// various latencies on every transition instead of sampling them.
var casgstatusAlwaysTrack = false

// If asked to move to or from a Gscanstatus this will throw. Use the castogscanstatus
// and casfrom_Gscanstatus instead.
// casgstatus will loop if the g->atomicstatus is in a Gscan status until the routine that
// put it in the Gscan state is finished.
//
// 如果尝试转换到或从Gscan状态，此函数将抛出异常。应该使用castogscanstatus
// 和casfrom_Gscanstatus来代替。
// 如果g->atomicstatus处于Gscan状态，casgstatus将循环等待直到将其置入
// Gscan状态的例程完成。
//
//go:nosplit
func casgstatus(gp *g, oldval, newval uint32) {
	if (oldval&_Gscan != 0) || (newval&_Gscan != 0) || oldval == newval {
		systemstack(func() {
			// Call on the systemstack to prevent print and throw from counting
			// against the nosplit stack reservation.
			// 在系统栈上调用以防止print和throw计入nosplit栈保留空间
			print("runtime: casgstatus: oldval=", hex(oldval), " newval=", hex(newval), "\n")
			throw("casgstatus: bad incoming values")
		})
	}

	// 尝试获取Gscan锁，可能会获取也可能不会
	lockWithRankMayAcquire(nil, lockRankGscan)

	// See https://golang.org/cl/21503 for justification of the yield delay.
	// 参见 https://golang.org/cl/21503 了解yield延迟的合理性说明
	const yieldDelay = 5 * 1000
	var nextYield int64

	// loop if gp->atomicstatus is in a scan state giving
	// GC time to finish and change the state to oldval.
	// 如果gp->atomicstatus处于扫描状态，则循环等待
	// 给GC时间完成并将状态改变为oldval
	for i := 0; !gp.atomicstatus.CompareAndSwap(oldval, newval); i++ {
		if oldval == _Gwaiting && gp.atomicstatus.Load() == _Grunnable {
			systemstack(func() {
				// Call on the systemstack to prevent throw from counting
				// against the nosplit stack reservation.
				// 在系统栈上调用以防止throw计入nosplit栈保留空间
				throw("casgstatus: waiting for Gwaiting but is Grunnable")
			})
		}
		if i == 0 {
			nextYield = nanotime() + yieldDelay
		}
		if nanotime() < nextYield {
			// 在yield延迟时间内，尝试最多10次procyield
			// 每次procyield都会让出CPU时间片
			for x := 0; x < 10 && gp.atomicstatus.Load() != oldval; x++ {
				procyield(1)
			}
		} else {
			// 超过yield延迟时间后，使用osyield让出CPU
			// 并更新下一次yield的时间点
			osyield()
			nextYield = nanotime() + yieldDelay/2
		}
	}

	if oldval == _Grunning {
		// Track every gTrackingPeriod time a goroutine transitions out of running.
		// 每当goroutine从运行状态转换出来时，每隔gTrackingPeriod次就跟踪一次
		if casgstatusAlwaysTrack || gp.trackingSeq%gTrackingPeriod == 0 {
			// 如果启用了始终跟踪，或者当前跟踪序列号是gTrackingPeriod的倍数，则开启跟踪
			gp.tracking = true
		}
		gp.trackingSeq++ // 增加跟踪序列号
	}
	if !gp.tracking {
		// 如果不需要跟踪，直接返回
		return
	}

	// Handle various kinds of tracking.
	//
	// Currently:
	// - Time spent in runnable.
	// - Time spent blocked on a sync.Mutex or sync.RWMutex.
	// 处理各种类型的跟踪
	//
	// 目前跟踪:
	// - 在可运行状态(runnable)下花费的时间
	// - 在sync.Mutex或sync.RWMutex上阻塞的时间
	switch oldval {
	case _Grunnable:
		// We transitioned out of runnable, so measure how much
		// time we spent in this state and add it to
		// runnableTime.
		// 我们从可运行状态转换出来，所以测量在这个状态下
		// 花费了多少时间，并将其添加到runnableTime中
		now := nanotime()
		gp.runnableTime += now - gp.trackingStamp
		gp.trackingStamp = 0
	case _Gwaiting:
		if !gp.waitreason.isMutexWait() {
			// Not blocking on a lock.
			// 不是在锁上阻塞
			break
		}
		// Blocking on a lock, measure it. Note that because we're
		// sampling, we have to multiply by our sampling period to get
		// a more representative estimate of the absolute value.
		// gTrackingPeriod also represents an accurate sampling period
		// because we can only enter this state from _Grunning.
		// 在锁上阻塞，测量这个时间。注意因为我们是在采样，
		// 所以需要乘以采样周期来获得更准确的绝对值估计。
		// gTrackingPeriod也代表一个准确的采样周期，因为我们
		// 只能从_Grunning状态进入这个状态
		now := nanotime()
		sched.totalMutexWaitTime.Add((now - gp.trackingStamp) * gTrackingPeriod)
		gp.trackingStamp = 0
	}
	switch newval {
	case _Gwaiting:
		if !gp.waitreason.isMutexWait() {
			// Not blocking on a lock.
			// 不是在锁上阻塞
			break
		}
		// Blocking on a lock. Write down the timestamp.
		// 在锁上阻塞，记录时间戳
		now := nanotime()
		gp.trackingStamp = now
	case _Grunnable:
		// We just transitioned into runnable, so record what
		// time that happened.
		// 我们刚刚转换到可运行状态，所以记录这个时间点
		now := nanotime()
		gp.trackingStamp = now
	case _Grunning:
		// We're transitioning into running, so turn off
		// tracking and record how much time we spent in
		// runnable.
		// 我们正在转换到运行状态，所以关闭跟踪
		// 并记录在可运行状态下花费的时间
		gp.tracking = false
		sched.timeToRun.record(gp.runnableTime)
		gp.runnableTime = 0
	}
}

// casGToWaiting transitions gp from old to _Gwaiting, and sets the wait reason.
//
// Use this over casgstatus when possible to ensure that a waitreason is set.
func casGToWaiting(gp *g, old uint32, reason waitReason) {
	// Set the wait reason before calling casgstatus, because casgstatus will use it.
	gp.waitreason = reason
	casgstatus(gp, old, _Gwaiting)
}

// casGToWaitingForGC transitions gp from old to _Gwaiting, and sets the wait reason.
// The wait reason must be a valid isWaitingForGC wait reason.
//
// Use this over casgstatus when possible to ensure that a waitreason is set.
func casGToWaitingForGC(gp *g, old uint32, reason waitReason) {
	if !reason.isWaitingForGC() {
		throw("casGToWaitingForGC with non-isWaitingForGC wait reason")
	}
	casGToWaiting(gp, old, reason)
}

// casgstatus(gp, oldstatus, Gcopystack), assuming oldstatus is Gwaiting or Grunnable.
// Returns old status. Cannot call casgstatus directly, because we are racing with an
// async wakeup that might come in from netpoll. If we see Gwaiting from the readgstatus,
// it might have become Grunnable by the time we get to the cas. If we called casgstatus,
// it would loop waiting for the status to go back to Gwaiting, which it never will.
//
//go:nosplit
func casgcopystack(gp *g) uint32 {
	for {
		oldstatus := readgstatus(gp) &^ _Gscan
		if oldstatus != _Gwaiting && oldstatus != _Grunnable {
			throw("copystack: bad status, not Gwaiting or Grunnable")
		}
		if gp.atomicstatus.CompareAndSwap(oldstatus, _Gcopystack) {
			return oldstatus
		}
	}
}

// casGToPreemptScan transitions gp from _Grunning to _Gscan|_Gpreempted.
//
// TODO(austin): This is the only status operation that both changes
// the status and locks the _Gscan bit. Rethink this.
func casGToPreemptScan(gp *g, old, new uint32) {
	if old != _Grunning || new != _Gscan|_Gpreempted {
		throw("bad g transition")
	}
	acquireLockRankAndM(lockRankGscan)
	for !gp.atomicstatus.CompareAndSwap(_Grunning, _Gscan|_Gpreempted) {
	}
}

// casGFromPreempted attempts to transition gp from _Gpreempted to
// _Gwaiting. If successful, the caller is responsible for
// re-scheduling gp.
func casGFromPreempted(gp *g, old, new uint32) bool {
	if old != _Gpreempted || new != _Gwaiting {
		throw("bad g transition")
	}
	gp.waitreason = waitReasonPreempted
	return gp.atomicstatus.CompareAndSwap(_Gpreempted, _Gwaiting)
}

// stwReason is an enumeration of reasons the world is stopping.
type stwReason uint8

// Reasons to stop-the-world.
//
// Avoid reusing reasons and add new ones instead.
const (
	stwUnknown                     stwReason = iota // "unknown"
	stwGCMarkTerm                                   // "GC mark termination"
	stwGCSweepTerm                                  // "GC sweep termination"
	stwWriteHeapDump                                // "write heap dump"
	stwGoroutineProfile                             // "goroutine profile"
	stwGoroutineProfileCleanup                      // "goroutine profile cleanup"
	stwAllGoroutinesStack                           // "all goroutines stack trace"
	stwReadMemStats                                 // "read mem stats"
	stwAllThreadsSyscall                            // "AllThreadsSyscall"
	stwGOMAXPROCS                                   // "GOMAXPROCS"
	stwStartTrace                                   // "start trace"
	stwStopTrace                                    // "stop trace"
	stwForTestCountPagesInUse                       // "CountPagesInUse (test)"
	stwForTestReadMetricsSlow                       // "ReadMetricsSlow (test)"
	stwForTestReadMemStatsSlow                      // "ReadMemStatsSlow (test)"
	stwForTestPageCachePagesLeaked                  // "PageCachePagesLeaked (test)"
	stwForTestResetDebugLog                         // "ResetDebugLog (test)"
)

func (r stwReason) String() string {
	return stwReasonStrings[r]
}

func (r stwReason) isGC() bool {
	return r == stwGCMarkTerm || r == stwGCSweepTerm
}

// If you add to this list, also add it to src/internal/trace/parser.go.
// If you change the values of any of the stw* constants, bump the trace
// version number and make a copy of this.
var stwReasonStrings = [...]string{
	stwUnknown:                     "unknown",
	stwGCMarkTerm:                  "GC mark termination",
	stwGCSweepTerm:                 "GC sweep termination",
	stwWriteHeapDump:               "write heap dump",
	stwGoroutineProfile:            "goroutine profile",
	stwGoroutineProfileCleanup:     "goroutine profile cleanup",
	stwAllGoroutinesStack:          "all goroutines stack trace",
	stwReadMemStats:                "read mem stats",
	stwAllThreadsSyscall:           "AllThreadsSyscall",
	stwGOMAXPROCS:                  "GOMAXPROCS",
	stwStartTrace:                  "start trace",
	stwStopTrace:                   "stop trace",
	stwForTestCountPagesInUse:      "CountPagesInUse (test)",
	stwForTestReadMetricsSlow:      "ReadMetricsSlow (test)",
	stwForTestReadMemStatsSlow:     "ReadMemStatsSlow (test)",
	stwForTestPageCachePagesLeaked: "PageCachePagesLeaked (test)",
	stwForTestResetDebugLog:        "ResetDebugLog (test)",
}

// worldStop provides context from the stop-the-world required by the
// start-the-world.
type worldStop struct {
	reason           stwReason
	startedStopping  int64
	finishedStopping int64
	stoppingCPUTime  int64
}

// Temporary variable for stopTheWorld, when it can't write to the stack.
//
// Protected by worldsema.
var stopTheWorldContext worldStop

// stopTheWorld stops all P's from executing goroutines, interrupting
// all goroutines at GC safe points and records reason as the reason
// for the stop. On return, only the current goroutine's P is running.
// stopTheWorld must not be called from a system stack and the caller
// must not hold worldsema. The caller must call startTheWorld when
// other P's should resume execution.
//
// stopTheWorld is safe for multiple goroutines to call at the
// same time. Each will execute its own stop, and the stops will
// be serialized.
//
// This is also used by routines that do stack dumps. If the system is
// in panic or being exited, this may not reliably stop all
// goroutines.
//
// Returns the STW context. When starting the world, this context must be
// passed to startTheWorld.
func stopTheWorld(reason stwReason) worldStop {
	semacquire(&worldsema)
	gp := getg()
	gp.m.preemptoff = reason.String()
	systemstack(func() {
		// Mark the goroutine which called stopTheWorld preemptible so its
		// stack may be scanned.
		// This lets a mark worker scan us while we try to stop the world
		// since otherwise we could get in a mutual preemption deadlock.
		// We must not modify anything on the G stack because a stack shrink
		// may occur. A stack shrink is otherwise OK though because in order
		// to return from this function (and to leave the system stack) we
		// must have preempted all goroutines, including any attempting
		// to scan our stack, in which case, any stack shrinking will
		// have already completed by the time we exit.
		//
		// N.B. The execution tracer is not aware of this status
		// transition and handles it specially based on the
		// wait reason.
		casGToWaitingForGC(gp, _Grunning, waitReasonStoppingTheWorld)
		stopTheWorldContext = stopTheWorldWithSema(reason) // avoid write to stack
		casgstatus(gp, _Gwaiting, _Grunning)
	})
	return stopTheWorldContext
}

// startTheWorld undoes the effects of stopTheWorld.
//
// w must be the worldStop returned by stopTheWorld.
func startTheWorld(w worldStop) {
	systemstack(func() { startTheWorldWithSema(0, w) })

	// worldsema must be held over startTheWorldWithSema to ensure
	// gomaxprocs cannot change while worldsema is held.
	//
	// Release worldsema with direct handoff to the next waiter, but
	// acquirem so that semrelease1 doesn't try to yield our time.
	//
	// Otherwise if e.g. ReadMemStats is being called in a loop,
	// it might stomp on other attempts to stop the world, such as
	// for starting or ending GC. The operation this blocks is
	// so heavy-weight that we should just try to be as fair as
	// possible here.
	//
	// We don't want to just allow us to get preempted between now
	// and releasing the semaphore because then we keep everyone
	// (including, for example, GCs) waiting longer.
	mp := acquirem()
	mp.preemptoff = ""
	semrelease1(&worldsema, true, 0)
	releasem(mp)
}

// stopTheWorldGC has the same effect as stopTheWorld, but blocks
// until the GC is not running. It also blocks a GC from starting
// until startTheWorldGC is called.
func stopTheWorldGC(reason stwReason) worldStop {
	semacquire(&gcsema)
	return stopTheWorld(reason)
}

// startTheWorldGC undoes the effects of stopTheWorldGC.
//
// w must be the worldStop returned by stopTheWorld.
func startTheWorldGC(w worldStop) {
	startTheWorld(w)
	semrelease(&gcsema)
}

// Holding worldsema grants an M the right to try to stop the world.
var worldsema uint32 = 1

// Holding gcsema grants the M the right to block a GC, and blocks
// until the current GC is done. In particular, it prevents gomaxprocs
// from changing concurrently.
//
// TODO(mknyszek): Once gomaxprocs and the execution tracer can handle
// being changed/enabled during a GC, remove this.
var gcsema uint32 = 1

// stopTheWorldWithSema is the core implementation of stopTheWorld.
// The caller is responsible for acquiring worldsema and disabling
// preemption first and then should stopTheWorldWithSema on the system
// stack:
//
//	semacquire(&worldsema, 0)
//	m.preemptoff = "reason"
//	var stw worldStop
//	systemstack(func() {
//		stw = stopTheWorldWithSema(reason)
//	})
//
// When finished, the caller must either call startTheWorld or undo
// these three operations separately:
//
//	m.preemptoff = ""
//	systemstack(func() {
//		now = startTheWorldWithSema(stw)
//	})
//	semrelease(&worldsema)
//
// It is allowed to acquire worldsema once and then execute multiple
// startTheWorldWithSema/stopTheWorldWithSema pairs.
// Other P's are able to execute between successive calls to
// startTheWorldWithSema and stopTheWorldWithSema.
// Holding worldsema causes any other goroutines invoking
// stopTheWorld to block.
//
// Returns the STW context. When starting the world, this context must be
// passed to startTheWorldWithSema.
func stopTheWorldWithSema(reason stwReason) worldStop {
	trace := traceAcquire()
	if trace.ok() {
		trace.STWStart(reason)
		traceRelease(trace)
	}
	gp := getg()

	// If we hold a lock, then we won't be able to stop another M
	// that is blocked trying to acquire the lock.
	if gp.m.locks > 0 {
		throw("stopTheWorld: holding locks")
	}

	lock(&sched.lock)
	start := nanotime() // exclude time waiting for sched.lock from start and total time metrics.
	sched.stopwait = gomaxprocs
	sched.gcwaiting.Store(true)
	preemptall()
	// stop current P
	gp.m.p.ptr().status = _Pgcstop // Pgcstop is only diagnostic.
	gp.m.p.ptr().gcStopTime = start
	sched.stopwait--
	// try to retake all P's in Psyscall status
	trace = traceAcquire()
	for _, pp := range allp {
		s := pp.status
		if s == _Psyscall && atomic.Cas(&pp.status, s, _Pgcstop) {
			if trace.ok() {
				trace.ProcSteal(pp, false)
			}
			pp.syscalltick++
			pp.gcStopTime = nanotime()
			sched.stopwait--
		}
	}
	if trace.ok() {
		traceRelease(trace)
	}

	// stop idle P's
	now := nanotime()
	for {
		pp, _ := pidleget(now)
		if pp == nil {
			break
		}
		pp.status = _Pgcstop
		pp.gcStopTime = nanotime()
		sched.stopwait--
	}
	wait := sched.stopwait > 0
	unlock(&sched.lock)

	// wait for remaining P's to stop voluntarily
	if wait {
		for {
			// wait for 100us, then try to re-preempt in case of any races
			if notetsleep(&sched.stopnote, 100*1000) {
				noteclear(&sched.stopnote)
				break
			}
			preemptall()
		}
	}

	finish := nanotime()
	startTime := finish - start
	if reason.isGC() {
		sched.stwStoppingTimeGC.record(startTime)
	} else {
		sched.stwStoppingTimeOther.record(startTime)
	}

	// Double-check we actually stopped everything, and all the invariants hold.
	// Also accumulate all the time spent by each P in _Pgcstop up to the point
	// where everything was stopped. This will be accumulated into the total pause
	// CPU time by the caller.
	stoppingCPUTime := int64(0)
	bad := ""
	if sched.stopwait != 0 {
		bad = "stopTheWorld: not stopped (stopwait != 0)"
	} else {
		for _, pp := range allp {
			if pp.status != _Pgcstop {
				bad = "stopTheWorld: not stopped (status != _Pgcstop)"
			}
			if pp.gcStopTime == 0 && bad == "" {
				bad = "stopTheWorld: broken CPU time accounting"
			}
			stoppingCPUTime += finish - pp.gcStopTime
			pp.gcStopTime = 0
		}
	}
	if freezing.Load() {
		// Some other thread is panicking. This can cause the
		// sanity checks above to fail if the panic happens in
		// the signal handler on a stopped thread. Either way,
		// we should halt this thread.
		lock(&deadlock)
		lock(&deadlock)
	}
	if bad != "" {
		throw(bad)
	}

	worldStopped()

	return worldStop{
		reason:           reason,
		startedStopping:  start,
		finishedStopping: finish,
		stoppingCPUTime:  stoppingCPUTime,
	}
}

// reason is the same STW reason passed to stopTheWorld. start is the start
// time returned by stopTheWorld.
//
// now is the current time; prefer to pass 0 to capture a fresh timestamp.
//
// stattTheWorldWithSema returns now.
func startTheWorldWithSema(now int64, w worldStop) int64 {
	assertWorldStopped()

	mp := acquirem() // disable preemption because it can be holding p in a local var
	if netpollinited() {
		list, delta := netpoll(0) // non-blocking
		injectglist(&list)
		netpollAdjustWaiters(delta)
	}
	lock(&sched.lock)

	procs := gomaxprocs
	if newprocs != 0 {
		procs = newprocs
		newprocs = 0
	}
	p1 := procresize(procs)
	sched.gcwaiting.Store(false)
	if sched.sysmonwait.Load() {
		sched.sysmonwait.Store(false)
		notewakeup(&sched.sysmonnote)
	}
	unlock(&sched.lock)

	worldStarted()

	for p1 != nil {
		p := p1
		p1 = p1.link.ptr()
		if p.m != 0 {
			mp := p.m.ptr()
			p.m = 0
			if mp.nextp != 0 {
				throw("startTheWorld: inconsistent mp->nextp")
			}
			mp.nextp.set(p)
			notewakeup(&mp.park)
		} else {
			// Start M to run P.  Do not start another M below.
			newm(nil, p, -1)
		}
	}

	// Capture start-the-world time before doing clean-up tasks.
	if now == 0 {
		now = nanotime()
	}
	totalTime := now - w.startedStopping
	if w.reason.isGC() {
		sched.stwTotalTimeGC.record(totalTime)
	} else {
		sched.stwTotalTimeOther.record(totalTime)
	}
	trace := traceAcquire()
	if trace.ok() {
		trace.STWDone()
		traceRelease(trace)
	}

	// Wakeup an additional proc in case we have excessive runnable goroutines
	// in local queues or in the global queue. If we don't, the proc will park itself.
	// If we have lots of excessive work, resetspinning will unpark additional procs as necessary.
	wakep()

	releasem(mp)

	return now
}

// usesLibcall indicates whether this runtime performs system calls
// via libcall.
func usesLibcall() bool {
	switch GOOS {
	case "aix", "darwin", "illumos", "ios", "solaris", "windows":
		return true
	case "openbsd":
		return GOARCH != "mips64"
	}
	return false
}

// mStackIsSystemAllocated indicates whether this runtime starts on a
// system-allocated stack.
func mStackIsSystemAllocated() bool {
	switch GOOS {
	case "aix", "darwin", "plan9", "illumos", "ios", "solaris", "windows":
		return true
	case "openbsd":
		return GOARCH != "mips64"
	}
	return false
}

// mstart is the entry-point for new Ms.
// It is written in assembly, uses ABI0, is marked TOPFRAME, and calls mstart0.
// mstart是新的M的入口点
// 它用汇编语言编写，使用ABI0，被标记为TOPFRAME，并调用mstart0
func mstart()

// mstart0 is the Go entry-point for new Ms.
// This must not split the stack because we may not even have stack
// bounds set up yet.
//
// May run during STW (because it doesn't have a P yet), so write
// barriers are not allowed.
//
// mstart0是新的M的Go入口点
// 这里不能分割栈，因为我们可能还没有设置栈边界
// 可能在STW期间运行(因为它还没有P)，所以不允许写屏障
//
//go:nosplit
//go:nowritebarrierrec
func mstart0() {
	gp := getg()

	// 检查是否使用系统分配的栈
	osStack := gp.stack.lo == 0
	if osStack {
		// Initialize stack bounds from system stack.
		// Cgo may have left stack size in stack.hi.
		// minit may update the stack bounds.
		//
		// Note: these bounds may not be very accurate.
		// We set hi to &size, but there are things above
		// it. The 1024 is supposed to compensate this,
		// but is somewhat arbitrary.
		// 从系统栈初始化栈边界
		// Cgo可能已经在stack.hi中留下了栈大小
		// minit可能会更新栈边界
		// 注意：这些边界可能不是很准确
		// 我们将hi设置为&size，但在它上面还有东西
		// 1024是为了补偿这个，但有些随意
		size := gp.stack.hi
		if size == 0 {
			size = 16384 * sys.StackGuardMultiplier
		}
		gp.stack.hi = uintptr(noescape(unsafe.Pointer(&size)))
		gp.stack.lo = gp.stack.hi - size + 1024
	}
	// Initialize stack guard so that we can start calling regular
	// Go code.
	// 初始化栈保护，这样我们就可以开始调用常规的Go代码
	gp.stackguard0 = gp.stack.lo + stackGuard
	// This is the g0, so we can also call go:systemstack
	// functions, which check stackguard1.
	// 这是g0，所以我们也可以调用go:systemstack函数，这些函数会检查stackguard1
	gp.stackguard1 = gp.stackguard0
	mstart1()

	// Exit this thread.
	// 退出这个线程
	if mStackIsSystemAllocated() {
		// Windows, Solaris, illumos, Darwin, AIX and Plan 9 always system-allocate
		// the stack, but put it in gp.stack before mstart,
		// so the logic above hasn't set osStack yet.
		// Windows、Solaris、illumos、Darwin、AIX和Plan 9总是系统分配栈
		// 但在mstart之前将其放入gp.stack中
		// 所以上面的逻辑还没有设置osStack
		osStack = true
	}
	mexit(osStack)
}

// The go:noinline is to guarantee the getcallerpc/getcallersp below are safe,
// so that we can set up g0.sched to return to the call of mstart1 above.
// go:noinline 是为了确保下面的 getcallerpc/getcallersp 是安全的，
// 这样我们就可以设置 g0.sched 返回到上面 mstart1 的调用处。
//
//go:noinline
func mstart1() {
	gp := getg()

	if gp != gp.m.g0 {
		throw("bad runtime·mstart")
	}

	// Set up m.g0.sched as a label returning to just
	// after the mstart1 call in mstart0 above, for use by goexit0 and mcall.
	// We're never coming back to mstart1 after we call schedule,
	// so other calls can reuse the current frame.
	// And goexit0 does a gogo that needs to return from mstart1
	// and let mstart0 exit the thread.
	// 设置 m.g0.sched 作为一个标签，返回到上面 mstart0 中 mstart1 调用之后的位置，
	// 供 goexit0 和 mcall 使用。
	// 在调用 schedule 之后，我们永远不会返回到 mstart1，
	// 所以其他调用可以重用当前栈帧。
	// goexit0 执行 gogo 需要从 mstart1 返回，
	// 并让 mstart0 退出线程。
	gp.sched.g = guintptr(unsafe.Pointer(gp))
	gp.sched.pc = getcallerpc()
	gp.sched.sp = getcallersp()

	asminit() // 初始化汇编相关设置
	minit()   // 初始化 M 相关设置

	// Install signal handlers; after minit so that minit can
	// prepare the thread to be able to handle the signals.
	// 安装信号处理器；在 minit 之后，这样 minit 可以
	// 准备线程以能够处理信号。
	if gp.m == &m0 {
		mstartm0() // 如果是 m0，执行特殊的初始化
	}

	if fn := gp.m.mstartfn; fn != nil {
		fn() // 如果设置了 mstartfn，执行它
	}

	if gp.m != &m0 {
		acquirep(gp.m.nextp.ptr()) // 如果不是 m0，获取下一个 P
		gp.m.nextp = 0
	}
	schedule() // 开始调度循环
}

// mstartm0 implements part of mstart1 that only runs on the m0.
//
// Write barriers are allowed here because we know the GC can't be
// running yet, so they'll be no-ops.
//
//go:yeswritebarrierrec
func mstartm0() {
	// Create an extra M for callbacks on threads not created by Go.
	// An extra M is also needed on Windows for callbacks created by
	// syscall.NewCallback. See issue #6751 for details.
	if (iscgo || GOOS == "windows") && !cgoHasExtraM {
		cgoHasExtraM = true
		newextram()
	}
	initsig(false)
}

// mPark causes a thread to park itself, returning once woken.
//
//go:nosplit
func mPark() {
	gp := getg()
	notesleep(&gp.m.park)
	noteclear(&gp.m.park)
}

// mexit tears down and exits the current thread.
//
// Don't call this directly to exit the thread, since it must run at
// the top of the thread stack. Instead, use gogo(&gp.m.g0.sched) to
// unwind the stack to the point that exits the thread.
//
// It is entered with m.p != nil, so write barriers are allowed. It
// will release the P before exiting.
//
//go:yeswritebarrierrec
func mexit(osStack bool) {
	mp := getg().m

	if mp == &m0 {
		// This is the main thread. Just wedge it.
		//
		// On Linux, exiting the main thread puts the process
		// into a non-waitable zombie state. On Plan 9,
		// exiting the main thread unblocks wait even though
		// other threads are still running. On Solaris we can
		// neither exitThread nor return from mstart. Other
		// bad things probably happen on other platforms.
		//
		// We could try to clean up this M more before wedging
		// it, but that complicates signal handling.
		handoffp(releasep())
		lock(&sched.lock)
		sched.nmfreed++
		checkdead()
		unlock(&sched.lock)
		mPark()
		throw("locked m0 woke up")
	}

	sigblock(true)
	unminit()

	// Free the gsignal stack.
	if mp.gsignal != nil {
		stackfree(mp.gsignal.stack)
		// On some platforms, when calling into VDSO (e.g. nanotime)
		// we store our g on the gsignal stack, if there is one.
		// Now the stack is freed, unlink it from the m, so we
		// won't write to it when calling VDSO code.
		mp.gsignal = nil
	}

	// Remove m from allm.
	lock(&sched.lock)
	for pprev := &allm; *pprev != nil; pprev = &(*pprev).alllink {
		if *pprev == mp {
			*pprev = mp.alllink
			goto found
		}
	}
	throw("m not found in allm")
found:
	// Events must not be traced after this point.

	// Delay reaping m until it's done with the stack.
	//
	// Put mp on the free list, though it will not be reaped while freeWait
	// is freeMWait. mp is no longer reachable via allm, so even if it is
	// on an OS stack, we must keep a reference to mp alive so that the GC
	// doesn't free mp while we are still using it.
	//
	// Note that the free list must not be linked through alllink because
	// some functions walk allm without locking, so may be using alllink.
	//
	// N.B. It's important that the M appears on the free list simultaneously
	// with it being removed so that the tracer can find it.
	mp.freeWait.Store(freeMWait)
	mp.freelink = sched.freem
	sched.freem = mp
	unlock(&sched.lock)

	atomic.Xadd64(&ncgocall, int64(mp.ncgocall))
	sched.totalRuntimeLockWaitTime.Add(mp.mLockProfile.waitTime.Load())

	// Release the P.
	handoffp(releasep())
	// After this point we must not have write barriers.

	// Invoke the deadlock detector. This must happen after
	// handoffp because it may have started a new M to take our
	// P's work.
	lock(&sched.lock)
	sched.nmfreed++
	checkdead()
	unlock(&sched.lock)

	if GOOS == "darwin" || GOOS == "ios" {
		// Make sure pendingPreemptSignals is correct when an M exits.
		// For #41702.
		if mp.signalPending.Load() != 0 {
			pendingPreemptSignals.Add(-1)
		}
	}

	// Destroy all allocated resources. After this is called, we may no
	// longer take any locks.
	mdestroy(mp)

	if osStack {
		// No more uses of mp, so it is safe to drop the reference.
		mp.freeWait.Store(freeMRef)

		// Return from mstart and let the system thread
		// library free the g0 stack and terminate the thread.
		return
	}

	// mstart is the thread's entry point, so there's nothing to
	// return to. Exit the thread directly. exitThread will clear
	// m.freeWait when it's done with the stack and the m can be
	// reaped.
	exitThread(&mp.freeWait)
}

// forEachP calls fn(p) for every P p when p reaches a GC safe point.
// If a P is currently executing code, this will bring the P to a GC
// safe point and execute fn on that P. If the P is not executing code
// (it is idle or in a syscall), this will call fn(p) directly while
// preventing the P from exiting its state. This does not ensure that
// fn will run on every CPU executing Go code, but it acts as a global
// memory barrier. GC uses this as a "ragged barrier."
//
// The caller must hold worldsema. fn must not refer to any
// part of the current goroutine's stack, since the GC may move it.
func forEachP(reason waitReason, fn func(*p)) {
	systemstack(func() {
		gp := getg().m.curg
		// Mark the user stack as preemptible so that it may be scanned.
		// Otherwise, our attempt to force all P's to a safepoint could
		// result in a deadlock as we attempt to preempt a worker that's
		// trying to preempt us (e.g. for a stack scan).
		//
		// N.B. The execution tracer is not aware of this status
		// transition and handles it specially based on the
		// wait reason.
		casGToWaitingForGC(gp, _Grunning, reason)
		forEachPInternal(fn)
		casgstatus(gp, _Gwaiting, _Grunning)
	})
}

// forEachPInternal calls fn(p) for every P p when p reaches a GC safe point.
// It is the internal implementation of forEachP.
//
// The caller must hold worldsema and either must ensure that a GC is not
// running (otherwise this may deadlock with the GC trying to preempt this P)
// or it must leave its goroutine in a preemptible state before it switches
// to the systemstack. Due to these restrictions, prefer forEachP when possible.
//
//go:systemstack
func forEachPInternal(fn func(*p)) {
	mp := acquirem()
	pp := getg().m.p.ptr()

	lock(&sched.lock)
	if sched.safePointWait != 0 {
		throw("forEachP: sched.safePointWait != 0")
	}
	sched.safePointWait = gomaxprocs - 1
	sched.safePointFn = fn

	// Ask all Ps to run the safe point function.
	for _, p2 := range allp {
		if p2 != pp {
			atomic.Store(&p2.runSafePointFn, 1)
		}
	}
	preemptall()

	// Any P entering _Pidle or _Psyscall from now on will observe
	// p.runSafePointFn == 1 and will call runSafePointFn when
	// changing its status to _Pidle/_Psyscall.

	// Run safe point function for all idle Ps. sched.pidle will
	// not change because we hold sched.lock.
	for p := sched.pidle.ptr(); p != nil; p = p.link.ptr() {
		if atomic.Cas(&p.runSafePointFn, 1, 0) {
			fn(p)
			sched.safePointWait--
		}
	}

	wait := sched.safePointWait > 0
	unlock(&sched.lock)

	// Run fn for the current P.
	fn(pp)

	// Force Ps currently in _Psyscall into _Pidle and hand them
	// off to induce safe point function execution.
	for _, p2 := range allp {
		s := p2.status

		// We need to be fine-grained about tracing here, since handoffp
		// might call into the tracer, and the tracer is non-reentrant.
		trace := traceAcquire()
		if s == _Psyscall && p2.runSafePointFn == 1 && atomic.Cas(&p2.status, s, _Pidle) {
			if trace.ok() {
				// It's important that we traceRelease before we call handoffp, which may also traceAcquire.
				trace.ProcSteal(p2, false)
				traceRelease(trace)
			}
			p2.syscalltick++
			handoffp(p2)
		} else if trace.ok() {
			traceRelease(trace)
		}
	}

	// Wait for remaining Ps to run fn.
	if wait {
		for {
			// Wait for 100us, then try to re-preempt in
			// case of any races.
			//
			// Requires system stack.
			if notetsleep(&sched.safePointNote, 100*1000) {
				noteclear(&sched.safePointNote)
				break
			}
			preemptall()
		}
	}
	if sched.safePointWait != 0 {
		throw("forEachP: not done")
	}
	for _, p2 := range allp {
		if p2.runSafePointFn != 0 {
			throw("forEachP: P did not run fn")
		}
	}

	lock(&sched.lock)
	sched.safePointFn = nil
	unlock(&sched.lock)
	releasem(mp)
}

// runSafePointFn runs the safe point function, if any, for this P.
// This should be called like
//
//	if getg().m.p.runSafePointFn != 0 {
//	    runSafePointFn()
//	}
//
// runSafePointFn must be checked on any transition in to _Pidle or
// _Psyscall to avoid a race where forEachP sees that the P is running
// just before the P goes into _Pidle/_Psyscall and neither forEachP
// nor the P run the safe-point function.
func runSafePointFn() {
	p := getg().m.p.ptr()
	// Resolve the race between forEachP running the safe-point
	// function on this P's behalf and this P running the
	// safe-point function directly.
	if !atomic.Cas(&p.runSafePointFn, 1, 0) {
		return
	}
	sched.safePointFn(p)
	lock(&sched.lock)
	sched.safePointWait--
	if sched.safePointWait == 0 {
		notewakeup(&sched.safePointNote)
	}
	unlock(&sched.lock)
}

// When running with cgo, we call _cgo_thread_start
// to start threads for us so that we can play nicely with
// foreign code.
var cgoThreadStart unsafe.Pointer

type cgothreadstart struct {
	g   guintptr
	tls *uint64
	fn  unsafe.Pointer
}

// Allocate a new m unassociated with any thread.
// Can use p for allocation context if needed.
// fn is recorded as the new m's m.mstartfn.
// id is optional pre-allocated m ID. Omit by passing -1.
//
// This function is allowed to have write barriers even if the caller
// isn't because it borrows pp.
//
// 分配一个新的未与任何线程关联的m。
// 如果需要的话，可以使用p作为分配上下文。
// fn被记录为新m的m.mstartfn。
// id是可选的预分配m ID。通过传递-1来省略。
//
// 即使调用者不允许写屏障，这个函数也允许有写屏障，
// 因为它借用了pp。
//
//go:yeswritebarrierrec
func allocm(pp *p, fn func(), id int64) *m {
	allocmLock.rlock()

	// The caller owns pp, but we may borrow (i.e., acquirep) it. We must
	// disable preemption to ensure it is not stolen, which would make the
	// caller lose ownership.
	// 调用者拥有pp，但我们可能会借用它（即通过acquirep）。
	// 我们必须禁用抢占以确保它不会被偷走，否则会导致调用者失去所有权。
	acquirem()

	gp := getg()
	if gp.m.p == 0 {
		acquirep(pp) // temporarily borrow p for mallocs in this function
		// 如果当前M没有关联的P，则临时借用传入的P用于此函数中的内存分配
	}

	// Release the free M list. We need to do this somewhere and
	// this may free up a stack we can use.
	// 释放空闲的M列表。我们需要在某个地方做这件事，
	// 这可能会释放出我们可以使用的栈空间。
	if sched.freem != nil {
		lock(&sched.lock)
		var newList *m
		for freem := sched.freem; freem != nil; {
			// Wait for freeWait to indicate that freem's stack is unused.
			// 等待freeWait表明freem的栈未被使用。
			wait := freem.freeWait.Load()
			// If the M is waiting to be freed (freeMWait), move it to the new list
			// 如果M正在等待被释放（freeMWait），将其移动到新列表中
			if wait == freeMWait {
				// Save the next M in the free list
				// 保存空闲列表中的下一个M
				next := freem.freelink
				// Link the current M to the new list
				// 将当前M链接到新列表
				freem.freelink = newList
				// Update the new list head to point to the current M
				// 更新新列表头指向当前M
				newList = freem
				// Move to the next M in the free list
				// 移动到空闲列表中的下一个M
				freem = next
				continue
			}
			// Drop any remaining trace resources.
			// Ms can continue to emit events all the way until wait != freeMWait,
			// so it's only safe to call traceThreadDestroy at this point.
			// 释放任何剩余的跟踪资源。
			// M可以继续发出事件直到wait != freeMWait，
			// 所以只有在这个时候调用traceThreadDestroy才是安全的。
			if traceEnabled() || traceShuttingDown() {
				traceThreadDestroy(freem)
			}
			// Free the stack if needed. For freeMRef, there is
			// nothing to do except drop freem from the sched.freem
			// list.
			// 如果需要的话释放栈空间。对于freeMRef，
			// 除了从sched.freem列表中删除freem外不需要做任何事情。
			if wait == freeMStack {
				// stackfree must be on the system stack, but allocm is
				// reachable off the system stack transitively from
				// startm.
				// stackfree必须在系统栈上执行，但allocm可以
				// 通过startm间接地从系统栈外访问。
				systemstack(func() {
					stackfree(freem.g0.stack)
				})
			}
			freem = freem.freelink
		}
		sched.freem = newList
		unlock(&sched.lock)
	}

	// Create a new M structure
	// 创建一个新的M结构体
	mp := new(m)
	// Set the start function for this M
	// 设置这个M的启动函数
	mp.mstartfn = fn
	// Initialize common fields of the M
	// 初始化M的公共字段
	mcommoninit(mp, id)

	// In case of cgo or Solaris or illumos or Darwin, pthread_create will make us a stack.
	// Windows and Plan 9 will layout sched stack on OS stack.
	// 在cgo或Solaris或illumos或Darwin的情况下，pthread_create会为我们创建栈。
	// Windows和Plan 9会在操作系统栈上布局调度栈。
	if iscgo || mStackIsSystemAllocated() {
		// For systems where the OS creates the stack, allocate a g0 without a stack
		// 对于由操作系统创建栈的系统，分配一个没有栈的g0
		mp.g0 = malg(-1)
	} else {
		// For other systems, allocate a g0 with a stack of size 16384 * StackGuardMultiplier
		// 对于其他系统，分配一个栈大小为16384 * StackGuardMultiplier的g0
		mp.g0 = malg(16384 * sys.StackGuardMultiplier)
	}
	// Set the M pointer in g0 to point back to this M
	// 在g0中设置M指针指向这个M
	mp.g0.m = mp

	// If the provided P is the same as the current M's P, release it
	// 如果提供的P与当前M的P相同，则释放它
	if pp == gp.m.p.ptr() {
		releasep()
	}

	releasem(gp.m)
	allocmLock.runlock()
	return mp
}

// needm is called when a cgo callback happens on a
// thread without an m (a thread not created by Go).
// In this case, needm is expected to find an m to use
// and return with m, g initialized correctly.
// Since m and g are not set now (likely nil, but see below)
// needm is limited in what routines it can call. In particular
// it can only call nosplit functions (textflag 7) and cannot
// do any scheduling that requires an m.
//
// In order to avoid needing heavy lifting here, we adopt
// the following strategy: there is a stack of available m's
// that can be stolen. Using compare-and-swap
// to pop from the stack has ABA races, so we simulate
// a lock by doing an exchange (via Casuintptr) to steal the stack
// head and replace the top pointer with MLOCKED (1).
// This serves as a simple spin lock that we can use even
// without an m. The thread that locks the stack in this way
// unlocks the stack by storing a valid stack head pointer.
//
// In order to make sure that there is always an m structure
// available to be stolen, we maintain the invariant that there
// is always one more than needed. At the beginning of the
// program (if cgo is in use) the list is seeded with a single m.
// If needm finds that it has taken the last m off the list, its job
// is - once it has installed its own m so that it can do things like
// allocate memory - to create a spare m and put it on the list.
//
// Each of these extra m's also has a g0 and a curg that are
// pressed into service as the scheduling stack and current
// goroutine for the duration of the cgo callback.
//
// It calls dropm to put the m back on the list,
// 1. when the callback is done with the m in non-pthread platforms,
// 2. or when the C thread exiting on pthread platforms.
//
// The signal argument indicates whether we're called from a signal
// handler.
//
//go:nosplit
func needm(signal bool) {
	if (iscgo || GOOS == "windows") && !cgoHasExtraM {
		// Can happen if C/C++ code calls Go from a global ctor.
		// Can also happen on Windows if a global ctor uses a
		// callback created by syscall.NewCallback. See issue #6751
		// for details.
		//
		// Can not throw, because scheduler is not initialized yet.
		writeErrStr("fatal error: cgo callback before cgo call\n")
		exit(1)
	}

	// Save and block signals before getting an M.
	// The signal handler may call needm itself,
	// and we must avoid a deadlock. Also, once g is installed,
	// any incoming signals will try to execute,
	// but we won't have the sigaltstack settings and other data
	// set up appropriately until the end of minit, which will
	// unblock the signals. This is the same dance as when
	// starting a new m to run Go code via newosproc.
	var sigmask sigset
	sigsave(&sigmask)
	sigblock(false)

	// getExtraM is safe here because of the invariant above,
	// that the extra list always contains or will soon contain
	// at least one m.
	mp, last := getExtraM()

	// Set needextram when we've just emptied the list,
	// so that the eventual call into cgocallbackg will
	// allocate a new m for the extra list. We delay the
	// allocation until then so that it can be done
	// after exitsyscall makes sure it is okay to be
	// running at all (that is, there's no garbage collection
	// running right now).
	mp.needextram = last

	// Store the original signal mask for use by minit.
	mp.sigmask = sigmask

	// Install TLS on some platforms (previously setg
	// would do this if necessary).
	osSetupTLS(mp)

	// Install g (= m->g0) and set the stack bounds
	// to match the current stack.
	setg(mp.g0)
	sp := getcallersp()
	callbackUpdateSystemStack(mp, sp, signal)

	// Should mark we are already in Go now.
	// Otherwise, we may call needm again when we get a signal, before cgocallbackg1,
	// which means the extram list may be empty, that will cause a deadlock.
	mp.isExtraInC = false

	// Initialize this thread to use the m.
	asminit()
	minit()

	// Emit a trace event for this dead -> syscall transition,
	// but only if we're not in a signal handler.
	//
	// N.B. the tracer can run on a bare M just fine, we just have
	// to make sure to do this before setg(nil) and unminit.
	var trace traceLocker
	if !signal {
		trace = traceAcquire()
	}

	// mp.curg is now a real goroutine.
	casgstatus(mp.curg, _Gdead, _Gsyscall)
	sched.ngsys.Add(-1)

	if !signal {
		if trace.ok() {
			trace.GoCreateSyscall(mp.curg)
			traceRelease(trace)
		}
	}
	mp.isExtraInSig = signal
}

// Acquire an extra m and bind it to the C thread when a pthread key has been created.
//
//go:nosplit
func needAndBindM() {
	needm(false)

	if _cgo_pthread_key_created != nil && *(*uintptr)(_cgo_pthread_key_created) != 0 {
		cgoBindM()
	}
}

// newextram allocates m's and puts them on the extra list.
// It is called with a working local m, so that it can do things
// like call schedlock and allocate.
func newextram() {
	c := extraMWaiters.Swap(0)
	if c > 0 {
		for i := uint32(0); i < c; i++ {
			oneNewExtraM()
		}
	} else if extraMLength.Load() == 0 {
		// Make sure there is at least one extra M.
		oneNewExtraM()
	}
}

// oneNewExtraM allocates an m and puts it on the extra list.
func oneNewExtraM() {
	// Create extra goroutine locked to extra m.
	// The goroutine is the context in which the cgo callback will run.
	// The sched.pc will never be returned to, but setting it to
	// goexit makes clear to the traceback routines where
	// the goroutine stack ends.
	mp := allocm(nil, nil, -1)
	gp := malg(4096)
	gp.sched.pc = abi.FuncPCABI0(goexit) + sys.PCQuantum
	gp.sched.sp = gp.stack.hi
	gp.sched.sp -= 4 * goarch.PtrSize // extra space in case of reads slightly beyond frame
	gp.sched.lr = 0
	gp.sched.g = guintptr(unsafe.Pointer(gp))
	gp.syscallpc = gp.sched.pc
	gp.syscallsp = gp.sched.sp
	gp.stktopsp = gp.sched.sp
	// malg returns status as _Gidle. Change to _Gdead before
	// adding to allg where GC can see it. We use _Gdead to hide
	// this from tracebacks and stack scans since it isn't a
	// "real" goroutine until needm grabs it.
	casgstatus(gp, _Gidle, _Gdead)
	gp.m = mp
	mp.curg = gp
	mp.isextra = true
	// mark we are in C by default.
	mp.isExtraInC = true
	mp.lockedInt++
	mp.lockedg.set(gp)
	gp.lockedm.set(mp)
	gp.goid = sched.goidgen.Add(1)
	if raceenabled {
		gp.racectx = racegostart(abi.FuncPCABIInternal(newextram) + sys.PCQuantum)
	}
	// put on allg for garbage collector
	allgadd(gp)

	// gp is now on the allg list, but we don't want it to be
	// counted by gcount. It would be more "proper" to increment
	// sched.ngfree, but that requires locking. Incrementing ngsys
	// has the same effect.
	sched.ngsys.Add(1)

	// Add m to the extra list.
	addExtraM(mp)
}

// dropm puts the current m back onto the extra list.
//
// 1. On systems without pthreads, like Windows
// dropm is called when a cgo callback has called needm but is now
// done with the callback and returning back into the non-Go thread.
//
// The main expense here is the call to signalstack to release the
// m's signal stack, and then the call to needm on the next callback
// from this thread. It is tempting to try to save the m for next time,
// which would eliminate both these costs, but there might not be
// a next time: the current thread (which Go does not control) might exit.
// If we saved the m for that thread, there would be an m leak each time
// such a thread exited. Instead, we acquire and release an m on each
// call. These should typically not be scheduling operations, just a few
// atomics, so the cost should be small.
//
// 2. On systems with pthreads
// dropm is called while a non-Go thread is exiting.
// We allocate a pthread per-thread variable using pthread_key_create,
// to register a thread-exit-time destructor.
// And store the g into a thread-specific value associated with the pthread key,
// when first return back to C.
// So that the destructor would invoke dropm while the non-Go thread is exiting.
// This is much faster since it avoids expensive signal-related syscalls.
//
// This always runs without a P, so //go:nowritebarrierrec is required.
//
// This may run with a different stack than was recorded in g0 (there is no
// call to callbackUpdateSystemStack prior to dropm), so this must be
// //go:nosplit to avoid the stack bounds check.
//
//go:nowritebarrierrec
//go:nosplit
func dropm() {
	// Clear m and g, and return m to the extra list.
	// After the call to setg we can only call nosplit functions
	// with no pointer manipulation.
	mp := getg().m

	// Emit a trace event for this syscall -> dead transition.
	//
	// N.B. the tracer can run on a bare M just fine, we just have
	// to make sure to do this before setg(nil) and unminit.
	var trace traceLocker
	if !mp.isExtraInSig {
		trace = traceAcquire()
	}

	// Return mp.curg to dead state.
	casgstatus(mp.curg, _Gsyscall, _Gdead)
	mp.curg.preemptStop = false
	sched.ngsys.Add(1)

	if !mp.isExtraInSig {
		if trace.ok() {
			trace.GoDestroySyscall()
			traceRelease(trace)
		}
	}

	// Trash syscalltick so that it doesn't line up with mp.old.syscalltick anymore.
	//
	// In the new tracer, we model needm and dropm and a goroutine being created and
	// destroyed respectively. The m then might get reused with a different procid but
	// still with a reference to oldp, and still with the same syscalltick. The next
	// time a G is "created" in needm, it'll return and quietly reacquire its P from a
	// different m with a different procid, which will confuse the trace parser. By
	// trashing syscalltick, we ensure that it'll appear as if we lost the P to the
	// tracer parser and that we just reacquired it.
	//
	// Trash the value by decrementing because that gets us as far away from the value
	// the syscall exit code expects as possible. Setting to zero is risky because
	// syscalltick could already be zero (and in fact, is initialized to zero).
	mp.syscalltick--

	// Reset trace state unconditionally. This goroutine is being 'destroyed'
	// from the perspective of the tracer.
	mp.curg.trace.reset()

	// Flush all the M's buffers. This is necessary because the M might
	// be used on a different thread with a different procid, so we have
	// to make sure we don't write into the same buffer.
	if traceEnabled() || traceShuttingDown() {
		// Acquire sched.lock across thread destruction. One of the invariants of the tracer
		// is that a thread cannot disappear from the tracer's view (allm or freem) without
		// it noticing, so it requires that sched.lock be held over traceThreadDestroy.
		//
		// This isn't strictly necessary in this case, because this thread never leaves allm,
		// but the critical section is short and dropm is rare on pthread platforms, so just
		// take the lock and play it safe. traceThreadDestroy also asserts that the lock is held.
		lock(&sched.lock)
		traceThreadDestroy(mp)
		unlock(&sched.lock)
	}
	mp.isExtraInSig = false

	// Block signals before unminit.
	// Unminit unregisters the signal handling stack (but needs g on some systems).
	// Setg(nil) clears g, which is the signal handler's cue not to run Go handlers.
	// It's important not to try to handle a signal between those two steps.
	sigmask := mp.sigmask
	sigblock(false)
	unminit()

	setg(nil)

	// Clear g0 stack bounds to ensure that needm always refreshes the
	// bounds when reusing this M.
	g0 := mp.g0
	g0.stack.hi = 0
	g0.stack.lo = 0
	g0.stackguard0 = 0
	g0.stackguard1 = 0
	mp.g0StackAccurate = false

	putExtraM(mp)

	msigrestore(sigmask)
}

// bindm store the g0 of the current m into a thread-specific value.
//
// We allocate a pthread per-thread variable using pthread_key_create,
// to register a thread-exit-time destructor.
// We are here setting the thread-specific value of the pthread key, to enable the destructor.
// So that the pthread_key_destructor would dropm while the C thread is exiting.
//
// And the saved g will be used in pthread_key_destructor,
// since the g stored in the TLS by Go might be cleared in some platforms,
// before the destructor invoked, so, we restore g by the stored g, before dropm.
//
// We store g0 instead of m, to make the assembly code simpler,
// since we need to restore g0 in runtime.cgocallback.
//
// On systems without pthreads, like Windows, bindm shouldn't be used.
//
// NOTE: this always runs without a P, so, nowritebarrierrec required.
//
//go:nosplit
//go:nowritebarrierrec
func cgoBindM() {
	if GOOS == "windows" || GOOS == "plan9" {
		fatal("bindm in unexpected GOOS")
	}
	g := getg()
	if g.m.g0 != g {
		fatal("the current g is not g0")
	}
	if _cgo_bindm != nil {
		asmcgocall(_cgo_bindm, unsafe.Pointer(g))
	}
}

// A helper function for EnsureDropM.
//
// getm should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - fortio.org/log
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname getm
func getm() uintptr {
	return uintptr(unsafe.Pointer(getg().m))
}

var (
	// Locking linked list of extra M's, via mp.schedlink. Must be accessed
	// only via lockextra/unlockextra.
	//
	// Can't be atomic.Pointer[m] because we use an invalid pointer as a
	// "locked" sentinel value. M's on this list remain visible to the GC
	// because their mp.curg is on allgs.
	extraM atomic.Uintptr
	// Number of M's in the extraM list.
	extraMLength atomic.Uint32
	// Number of waiters in lockextra.
	extraMWaiters atomic.Uint32

	// Number of extra M's in use by threads.
	extraMInUse atomic.Uint32
)

// lockextra locks the extra list and returns the list head.
// The caller must unlock the list by storing a new list head
// to extram. If nilokay is true, then lockextra will
// return a nil list head if that's what it finds. If nilokay is false,
// lockextra will keep waiting until the list head is no longer nil.
//
//go:nosplit
func lockextra(nilokay bool) *m {
	const locked = 1

	incr := false
	for {
		old := extraM.Load()
		if old == locked {
			osyield_no_g()
			continue
		}
		if old == 0 && !nilokay {
			if !incr {
				// Add 1 to the number of threads
				// waiting for an M.
				// This is cleared by newextram.
				extraMWaiters.Add(1)
				incr = true
			}
			usleep_no_g(1)
			continue
		}
		if extraM.CompareAndSwap(old, locked) {
			return (*m)(unsafe.Pointer(old))
		}
		osyield_no_g()
		continue
	}
}

//go:nosplit
func unlockextra(mp *m, delta int32) {
	extraMLength.Add(delta)
	extraM.Store(uintptr(unsafe.Pointer(mp)))
}

// Return an M from the extra M list. Returns last == true if the list becomes
// empty because of this call.
//
// Spins waiting for an extra M, so caller must ensure that the list always
// contains or will soon contain at least one M.
//
//go:nosplit
func getExtraM() (mp *m, last bool) {
	mp = lockextra(false)
	extraMInUse.Add(1)
	unlockextra(mp.schedlink.ptr(), -1)
	return mp, mp.schedlink.ptr() == nil
}

// Returns an extra M back to the list. mp must be from getExtraM. Newly
// allocated M's should use addExtraM.
//
//go:nosplit
func putExtraM(mp *m) {
	extraMInUse.Add(-1)
	addExtraM(mp)
}

// Adds a newly allocated M to the extra M list.
//
//go:nosplit
func addExtraM(mp *m) {
	mnext := lockextra(true)
	mp.schedlink.set(mnext)
	unlockextra(mp, 1)
}

var (
	// allocmLock is locked for read when creating new Ms in allocm and their
	// addition to allm. Thus acquiring this lock for write blocks the
	// creation of new Ms.
	allocmLock rwmutex

	// execLock serializes exec and clone to avoid bugs or unspecified
	// behaviour around exec'ing while creating/destroying threads. See
	// issue #19546.
	execLock rwmutex
)

// These errors are reported (via writeErrStr) by some OS-specific
// versions of newosproc and newosproc0.
const (
	failthreadcreate  = "runtime: failed to create new OS thread\n"
	failallocatestack = "runtime: failed to allocate stack for the new OS thread\n"
)

// newmHandoff contains a list of m structures that need new OS threads.
// This is used by newm in situations where newm itself can't safely
// start an OS thread.
var newmHandoff struct {
	lock mutex

	// newm points to a list of M structures that need new OS
	// threads. The list is linked through m.schedlink.
	newm muintptr

	// waiting indicates that wake needs to be notified when an m
	// is put on the list.
	waiting bool
	wake    note

	// haveTemplateThread indicates that the templateThread has
	// been started. This is not protected by lock. Use cas to set
	// to 1.
	haveTemplateThread uint32
}

// Create a new m. It will start off with a call to fn, or else the scheduler.
// fn needs to be static and not a heap allocated closure.
// May run with m.p==nil, so write barriers are not allowed.
//
// id is optional pre-allocated m ID. Omit by passing -1.
//
// 创建一个新的m。它将从调用fn开始，或者调用调度器。
// fn必须是静态的，不能是堆分配的闭包。
// 可能在m.p为nil的情况下运行，所以不允许写屏障。
//
// id是可选的预分配m ID。通过传递-1来省略。
//
//go:nowritebarrierrec
func newm(fn func(), pp *p, id int64) {
	// allocm adds a new M to allm, but they do not start until created by
	// the OS in newm1 or the template thread.
	//
	// doAllThreadsSyscall requires that every M in allm will eventually
	// start and be signal-able, even with a STW.
	//
	// Disable preemption here until we start the thread to ensure that
	// newm is not preempted between allocm and starting the new thread,
	// ensuring that anything added to allm is guaranteed to eventually
	// start.
	//
	// allocm 将一个新的 M 添加到 allm 中，但它们要等到在 newm1 或模板线程中
	// 由操作系统创建后才会启动。
	//
	// doAllThreadsSyscall 要求 allm 中的每个 M 最终都会启动并且可以接收信号，
	// 即使在 STW (Stop The World) 期间也是如此。
	//
	// 在这里禁用抢占，直到我们启动线程，以确保 newm 在 allocm 和启动新线程之间
	// 不会被抢占，从而确保添加到 allm 中的任何内容最终都会启动。
	acquirem()

	// allocm adds a new M to allm, but they do not start until created by
	// the OS in newm1 or the template thread.
	// allocm 将一个新的 M 添加到 allm 中，但它们要等到在 newm1 或模板线程中
	// 由操作系统创建后才会启动
	mp := allocm(pp, fn, id)
	// Set the next P to run on this M
	// 设置这个 M 要运行的下一个 P
	mp.nextp.set(pp)
	// Initialize the signal mask for this M
	// 初始化这个 M 的信号掩码
	mp.sigmask = initSigmask
	if gp := getg(); gp != nil && gp.m != nil && (gp.m.lockedExt != 0 || gp.m.incgo) && GOOS != "plan9" {
		// We're on a locked M or a thread that may have been
		// started by C. The kernel state of this thread may
		// be strange (the user may have locked it for that
		// purpose). We don't want to clone that into another
		// thread. Instead, ask a known-good thread to create
		// the thread for us.
		//
		// This is disabled on Plan 9. See golang.org/issue/22227.
		//
		// TODO: This may be unnecessary on Windows, which
		// doesn't model thread creation off fork.
		//
		// 我们当前在一个锁定的M上，或者一个可能由C启动的线程上。
		// 这个线程的内核状态可能很奇怪（用户可能为了特定目的锁定了它）。
		// 我们不想将这种状态克隆到另一个线程中。
		// 相反，我们请求一个已知状态良好的线程来为我们创建线程。
		//
		// 这在Plan 9上被禁用。参见golang.org/issue/22227。
		//
		// TODO: 这在Windows上可能是不必要的，因为Windows不使用fork来创建线程。
		lock(&newmHandoff.lock)
		// Check if we have a template thread available
		// 检查是否有可用的模板线程
		if newmHandoff.haveTemplateThread == 0 {
			throw("on a locked thread with no template thread")
		}
		// Link the new M to the template thread's queue
		// 将新的M链接到模板线程的队列中
		mp.schedlink = newmHandoff.newm
		newmHandoff.newm.set(mp)
		// If the template thread is waiting, wake it up to process the new M
		// 如果模板线程正在等待，唤醒它来处理新的M
		if newmHandoff.waiting {
			newmHandoff.waiting = false
			notewakeup(&newmHandoff.wake)
		}
		unlock(&newmHandoff.lock)
		// The M has not started yet, but the template thread does not
		// participate in STW, so it will always process queued Ms and
		// it is safe to releasem.
		//
		// M还没有启动，但模板线程不参与STW（Stop The World），
		// 所以它会始终处理队列中的M，因此可以安全地释放当前M。
		releasem(getg().m)
		return
	}
	newm1(mp)
	releasem(getg().m)
}

func newm1(mp *m) {
	if iscgo {
		var ts cgothreadstart
		if _cgo_thread_start == nil {
			throw("_cgo_thread_start missing")
		}
		ts.g.set(mp.g0)
		ts.tls = (*uint64)(unsafe.Pointer(&mp.tls[0]))
		ts.fn = unsafe.Pointer(abi.FuncPCABI0(mstart))
		if msanenabled {
			msanwrite(unsafe.Pointer(&ts), unsafe.Sizeof(ts))
		}
		if asanenabled {
			asanwrite(unsafe.Pointer(&ts), unsafe.Sizeof(ts))
		}
		execLock.rlock() // Prevent process clone.
		asmcgocall(_cgo_thread_start, unsafe.Pointer(&ts))
		execLock.runlock()
		return
	}
	execLock.rlock() // Prevent process clone.
	newosproc(mp)
	execLock.runlock()
}

// startTemplateThread starts the template thread if it is not already
// running.
//
// The calling thread must itself be in a known-good state.
func startTemplateThread() {
	if GOARCH == "wasm" { // no threads on wasm yet
		return
	}

	// Disable preemption to guarantee that the template thread will be
	// created before a park once haveTemplateThread is set.
	mp := acquirem()
	if !atomic.Cas(&newmHandoff.haveTemplateThread, 0, 1) {
		releasem(mp)
		return
	}
	newm(templateThread, nil, -1)
	releasem(mp)
}

// templateThread is a thread in a known-good state that exists solely
// to start new threads in known-good states when the calling thread
// may not be in a good state.
//
// Many programs never need this, so templateThread is started lazily
// when we first enter a state that might lead to running on a thread
// in an unknown state.
//
// templateThread runs on an M without a P, so it must not have write
// barriers.
//
//go:nowritebarrierrec
func templateThread() {
	lock(&sched.lock)
	sched.nmsys++
	checkdead()
	unlock(&sched.lock)

	for {
		lock(&newmHandoff.lock)
		for newmHandoff.newm != 0 {
			newm := newmHandoff.newm.ptr()
			newmHandoff.newm = 0
			unlock(&newmHandoff.lock)
			for newm != nil {
				next := newm.schedlink.ptr()
				newm.schedlink = 0
				newm1(newm)
				newm = next
			}
			lock(&newmHandoff.lock)
		}
		newmHandoff.waiting = true
		noteclear(&newmHandoff.wake)
		unlock(&newmHandoff.lock)
		notesleep(&newmHandoff.wake)
	}
}

// Stops execution of the current m until new work is available.
// Returns with acquired P.
func stopm() {
	gp := getg()

	if gp.m.locks != 0 {
		throw("stopm holding locks")
	}
	if gp.m.p != 0 {
		throw("stopm holding p")
	}
	if gp.m.spinning {
		throw("stopm spinning")
	}

	lock(&sched.lock)
	mput(gp.m)
	unlock(&sched.lock)
	mPark()
	acquirep(gp.m.nextp.ptr())
	gp.m.nextp = 0
}

func mspinning() {
	// startm's caller incremented nmspinning. Set the new M's spinning.
	getg().m.spinning = true
}

// Schedules some M to run the p (creates an M if necessary).
// If p==nil, tries to get an idle P, if no idle P's does nothing.
// May run with m.p==nil, so write barriers are not allowed.
// If spinning is set, the caller has incremented nmspinning and must provide a
// P. startm will set m.spinning in the newly started M.
//
// Callers passing a non-nil P must call from a non-preemptible context. See
// comment on acquirem below.
//
// Argument lockheld indicates whether the caller already acquired the
// scheduler lock. Callers holding the lock when making the call must pass
// true. The lock might be temporarily dropped, but will be reacquired before
// returning.
//
// Must not have write barriers because this may be called without a P.
//
// 调度某个M来运行p（如果需要的话会创建一个新的M）。
// 如果p为nil，尝试获取一个空闲的P，如果没有空闲的P则什么都不做。
// 可能在m.p为nil的情况下运行，所以不允许写屏障。
// 如果spinning被设置，调用者必须已经增加了nmspinning计数并提供
// 一个P。startm将在新启动的M中设置m.spinning。
//
// 传入非nil P的调用者必须从不可抢占的上下文中调用。参见
// 下面acquirem的注释。
//
// 参数lockheld表示调用者是否已经获取了调度器锁。
// 在调用时持有锁的调用者必须传入true。
// 锁可能会暂时释放，但在返回前会重新获取。
//
// 不能有写屏障，因为这个函数可能在P不存在的情况下被调用。
//
//go:nowritebarrierrec
func startm(pp *p, spinning, lockheld bool) {
	// Disable preemption.
	//
	// Every owned P must have an owner that will eventually stop it in the
	// event of a GC stop request. startm takes transient ownership of a P
	// (either from argument or pidleget below) and transfers ownership to
	// a started M, which will be responsible for performing the stop.
	//
	// Preemption must be disabled during this transient ownership,
	// otherwise the P this is running on may enter GC stop while still
	// holding the transient P, leaving that P in limbo and deadlocking the
	// STW.
	//
	// Callers passing a non-nil P must already be in non-preemptible
	// context, otherwise such preemption could occur on function entry to
	// startm. Callers passing a nil P may be preemptible, so we must
	// disable preemption before acquiring a P from pidleget below.
	//
	// 禁用抢占。
	//
	// 每个被拥有的P都必须有一个所有者，该所有者最终会在GC停止请求时停止它。
	// startm会临时获取一个P的所有权（要么从参数获取，要么从下面的pidleget获取），
	// 然后将所有权转移给一个已启动的M，该M将负责执行停止操作。
	//
	// 在这种临时所有权期间必须禁用抢占，否则当前运行的P可能会在仍然持有临时P的情况下
	// 进入GC停止状态，导致该P处于不确定状态并造成STW死锁。
	//
	// 传入非nil P的调用者必须已经处于不可抢占的上下文中，否则这种抢占可能会在进入startm函数时发生。
	// 传入nil P的调用者可能是可抢占的，所以我们必须从下面的pidleget获取P之前禁用抢占。
	mp := acquirem()
	if !lockheld {
		lock(&sched.lock)
	}
	if pp == nil {
		if spinning {
			// TODO(prattmic): All remaining calls to this function
			// with _p_ == nil could be cleaned up to find a P
			// before calling startm.
			throw("startm: P required for spinning=true")
		}
		// 尝试从空闲P列表中获取一个P
		// 如果spinning为true，则必须提供一个P，因为自旋的M需要立即开始工作
		pp, _ = pidleget(0)
		if pp == nil {
			// 如果没有可用的空闲P
			if !lockheld {
				// 如果调用者没有持有锁，需要释放调度器锁
				unlock(&sched.lock)
			}
			// 释放当前M的抢占禁用状态
			releasem(mp)
			return
		}
	}
	nmp := mget()
	if nmp == nil {
		// No M is available, we must drop sched.lock and call newm.
		// However, we already own a P to assign to the M.
		//
		// Once sched.lock is released, another G (e.g., in a syscall),
		// could find no idle P while checkdead finds a runnable G but
		// no running M's because this new M hasn't started yet, thus
		// throwing in an apparent deadlock.
		// This apparent deadlock is possible when startm is called
		// from sysmon, which doesn't count as a running M.
		//
		// Avoid this situation by pre-allocating the ID for the new M,
		// thus marking it as 'running' before we drop sched.lock. This
		// new M will eventually run the scheduler to execute any
		// queued G's.
		//
		// 尝试获取一个空闲的M
		// 如果没有可用的M，我们需要释放调度器锁并调用newm来创建新的M
		// 不过，我们已经拥有了一个可以分配给新M的P
		//
		// 一旦释放了调度器锁，另一个G（例如在系统调用中的G）
		// 可能会发现没有空闲的P，而checkdead却发现有可运行的G
		// 但没有运行的M，因为新创建的M还没有启动
		// 这会导致一个表面上的死锁
		// 当startm被sysmon调用时，这种表面上的死锁是可能发生的
		// 因为sysmon不被计算为一个运行的M
		//
		// 为了避免这种情况，我们在释放调度器锁之前预先分配新M的ID
		// 这样在释放锁之前就将新M标记为"运行中"
		// 这个新M最终会运行调度器来执行任何排队的G
		id := mReserveID()
		unlock(&sched.lock)

		var fn func()
		if spinning {
			// The caller incremented nmspinning, so set m.spinning in the new M.
			// 调用者已经增加了nmspinning计数，所以在新的M中设置m.spinning标志
			fn = mspinning
		}
		newm(fn, pp, id)

		if lockheld {
			lock(&sched.lock)
		}
		// Ownership transfer of pp committed by start in newm.
		// Preemption is now safe.
		// P的所有权转移由newm中的start完成
		// 现在可以安全地进行抢占
		releasem(mp)
		return
	}
	if !lockheld {
		unlock(&sched.lock)
	}
	if nmp.spinning {
		throw("startm: m is spinning")
	}
	if nmp.nextp != 0 {
		throw("startm: m has p")
	}
	if spinning && !runqempty(pp) {
		throw("startm: p has runnable gs")
	}
	// The caller incremented nmspinning, so set m.spinning in the new M.
	// 调用者已经增加了nmspinning计数，所以在新的M中设置m.spinning标志
	nmp.spinning = spinning
	nmp.nextp.set(pp)
	notewakeup(&nmp.park)
	// Ownership transfer of pp committed by wakeup. Preemption is now
	// safe.
	// P的所有权转移由wakeup完成。现在可以安全地进行抢占
	releasem(mp)
}

// Hands off P from syscall or locked M.
// Always runs without a P, so write barriers are not allowed.
// 从系统调用或锁定的M中交出P
// 总是在没有P的情况下运行，所以不允许写屏障
//
//go:nowritebarrierrec
func handoffp(pp *p) {
	// handoffp must start an M in any situation where
	// findrunnable would return a G to run on pp.
	// handoffp必须在任何findrunnable会返回一个G在pp上运行的情况下启动一个M

	// if it has local work, start it straight away
	// 如果它有本地工作，立即启动它
	if !runqempty(pp) || sched.runqsize != 0 {
		startm(pp, false, false)
		return
	}
	// if there's trace work to do, start it straight away
	// 如果有追踪工作要做，立即启动它
	if (traceEnabled() || traceShuttingDown()) && traceReaderAvailable() != nil {
		startm(pp, false, false)
		return
	}
	// if it has GC work, start it straight away
	// 如果有GC工作要做，立即启动它
	if gcBlackenEnabled != 0 && gcMarkWorkAvailable(pp) {
		startm(pp, false, false)
		return
	}
	// no local work, check that there are no spinning/idle M's,
	// otherwise our help is not required
	// 没有本地工作，检查是否没有自旋/空闲的M
	// 否则我们不需要提供帮助
	if sched.nmspinning.Load()+sched.npidle.Load() == 0 && sched.nmspinning.CompareAndSwap(0, 1) { // TODO: fast atomic
		sched.needspinning.Store(0)
		startm(pp, true, false)
		return
	}
	// Lock the scheduler to prevent concurrent modifications
	// 锁定调度器以防止并发修改
	lock(&sched.lock)

	// Check if GC is waiting
	// 检查GC是否在等待
	if sched.gcwaiting.Load() {
		// Mark P as stopped for GC
		// 将P标记为因GC而停止
		pp.status = _Pgcstop
		pp.gcStopTime = nanotime()
		sched.stopwait--
		// If all Ps have stopped, wake up the GC
		// 如果所有P都已停止，唤醒GC
		if sched.stopwait == 0 {
			notewakeup(&sched.stopnote)
		}
		unlock(&sched.lock)
		return
	}

	// Check if there's a safe point function to run
	// 检查是否有安全点函数需要运行
	if pp.runSafePointFn != 0 && atomic.Cas(&pp.runSafePointFn, 1, 0) {
		// Execute the safe point function
		// 执行安全点函数
		sched.safePointFn(pp)
		sched.safePointWait--
		// If all Ps have executed the safe point, wake up the waiting goroutine
		// 如果所有P都已执行完安全点，唤醒等待的goroutine
		if sched.safePointWait == 0 {
			notewakeup(&sched.safePointNote)
		}
	}

	// Check if there are runnable goroutines in the global queue
	// 检查全局队列中是否有可运行的goroutine
	if sched.runqsize != 0 {
		unlock(&sched.lock)
		startm(pp, false, false)
		return
	}
	// If this is the last running P and nobody is polling network,
	// need to wakeup another M to poll network.
	// 如果这是最后一个运行的P，并且没有人在轮询网络
	// 需要唤醒另一个M来轮询网络
	if sched.npidle.Load() == gomaxprocs-1 && sched.lastpoll.Load() != 0 {
		unlock(&sched.lock)
		startm(pp, false, false)
		return
	}

	// The scheduler lock cannot be held when calling wakeNetPoller below
	// because wakeNetPoller may call wakep which may call startm.
	// 在调用wakeNetPoller时不能持有调度器锁
	// 因为wakeNetPoller可能会调用wakep，而wakep可能会调用startm
	when := pp.timers.wakeTime()
	pidleput(pp, 0)
	unlock(&sched.lock)

	if when != 0 {
		wakeNetPoller(when)
	}
}

// Tries to add one more P to execute G's.
// Called when a G is made runnable (newproc, ready).
// Must be called with a P.
//
// wakep should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - gvisor.dev/gvisor
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname wakep
func wakep() {
	// Be conservative about spinning threads, only start one if none exist
	// already.
	if sched.nmspinning.Load() != 0 || !sched.nmspinning.CompareAndSwap(0, 1) {
		return
	}

	// Disable preemption until ownership of pp transfers to the next M in
	// startm. Otherwise preemption here would leave pp stuck waiting to
	// enter _Pgcstop.
	//
	// See preemption comment on acquirem in startm for more details.
	mp := acquirem()

	var pp *p
	lock(&sched.lock)
	pp, _ = pidlegetSpinning(0)
	if pp == nil {
		if sched.nmspinning.Add(-1) < 0 {
			throw("wakep: negative nmspinning")
		}
		unlock(&sched.lock)
		releasem(mp)
		return
	}
	// Since we always have a P, the race in the "No M is available"
	// comment in startm doesn't apply during the small window between the
	// unlock here and lock in startm. A checkdead in between will always
	// see at least one running M (ours).
	unlock(&sched.lock)

	startm(pp, true, false)

	releasem(mp)
}

// Stops execution of the current m that is locked to a g until the g is runnable again.
// Returns with acquired P.
//
// 停止当前被锁定到某个g的m的执行，直到该g再次变为可运行状态
// 返回时已获取P
func stoplockedm() {
	gp := getg()

	// 检查锁定状态的一致性
	// 确保当前m确实被锁定到某个g，且该g也正确锁定到当前m
	if gp.m.lockedg == 0 || gp.m.lockedg.ptr().lockedm.ptr() != gp.m {
		throw("stoplockedm: inconsistent locking")
	}
	if gp.m.p != 0 {
		// Schedule another M to run this p.
		// 如果当前m持有P，则释放P并调度另一个M来运行它
		pp := releasep()
		handoffp(pp)
	}
	// 增加空闲且锁定的M计数
	incidlelocked(1)
	// Wait until another thread schedules lockedg again.
	// 等待直到另一个线程重新调度lockedg
	mPark()
	// 检查lockedg的状态
	status := readgstatus(gp.m.lockedg.ptr())
	if status&^_Gscan != _Grunnable {
		print("runtime:stoplockedm: lockedg (atomicstatus=", status, ") is not Grunnable or Gscanrunnable\n")
		dumpgstatus(gp.m.lockedg.ptr())
		throw("stoplockedm: not runnable")
	}
	// 获取nextp并清除nextp字段
	acquirep(gp.m.nextp.ptr())
	gp.m.nextp = 0
}

// Schedules the locked m to run the locked gp.
// May run during STW, so write barriers are not allowed.
//
//go:nowritebarrierrec
func startlockedm(gp *g) {
	mp := gp.lockedm.ptr()
	if mp == getg().m {
		throw("startlockedm: locked to me")
	}
	if mp.nextp != 0 {
		throw("startlockedm: m has p")
	}
	// directly handoff current P to the locked m
	incidlelocked(-1)
	pp := releasep()
	mp.nextp.set(pp)
	notewakeup(&mp.park)
	stopm()
}

// Stops the current m for stopTheWorld.
// Returns when the world is restarted.
func gcstopm() {
	gp := getg()

	if !sched.gcwaiting.Load() {
		throw("gcstopm: not waiting for gc")
	}
	if gp.m.spinning {
		gp.m.spinning = false
		// OK to just drop nmspinning here,
		// startTheWorld will unpark threads as necessary.
		if sched.nmspinning.Add(-1) < 0 {
			throw("gcstopm: negative nmspinning")
		}
	}
	pp := releasep()
	lock(&sched.lock)
	pp.status = _Pgcstop
	pp.gcStopTime = nanotime()
	sched.stopwait--
	if sched.stopwait == 0 {
		notewakeup(&sched.stopnote)
	}
	unlock(&sched.lock)
	stopm()
}

// Schedules gp to run on the current M.
// If inheritTime is true, gp inherits the remaining time in the
// current time slice. Otherwise, it starts a new time slice.
// Never returns.
//
// Write barriers are allowed because this is called immediately after
// acquiring a P in several places.
//
// 调度gp在当前M上运行
// 如果inheritTime为true，gp继承当前时间片剩余的时间
// 否则，gp开始一个新的时间片
// 此函数永远不会返回
//
// 允许写屏障，因为此函数在获取P后立即被调用
//
//go:yeswritebarrierrec
func execute(gp *g, inheritTime bool) {
	mp := getg().m

	if goroutineProfile.active {
		// Make sure that gp has had its stack written out to the goroutine
		// profile, exactly as it was when the goroutine profiler first stopped
		// the world.
		// 确保gp的栈已经被写入goroutine profile中，
		// 与goroutine profiler第一次停止世界时的状态完全一致
		tryRecordGoroutineProfile(gp, nil, osyield)
	}

	// Assign gp.m before entering _Grunning so running Gs have an
	// M.
	// 在进入_Grunning状态之前设置gp.m，这样运行的G就有了一个M
	mp.curg = gp
	gp.m = mp
	// 将goroutine的状态从_Grunnable改为_Grunning
	casgstatus(gp, _Grunnable, _Grunning)
	// 重置等待时间
	gp.waitsince = 0
	// 重置抢占标志
	gp.preempt = false
	// 设置栈保护边界
	gp.stackguard0 = gp.stack.lo + stackGuard
	// 如果不继承时间片，则增加调度计数
	if !inheritTime {
		mp.p.ptr().schedtick++
	}

	// Check whether the profiler needs to be turned on or off.
	// 检查是否需要开启或关闭性能分析器
	hz := sched.profilehz
	if mp.profilehz != hz {
		// 如果当前M的采样频率与调度器的采样频率不同，
		// 则更新当前M的采样频率
		setThreadCPUProfiler(hz)
	}

	// 获取追踪器，用于记录goroutine的启动事件
	trace := traceAcquire()
	if trace.ok() {
		// 记录goroutine开始执行的事件
		trace.GoStart()
		// 释放追踪器
		traceRelease(trace)
	}

	gogo(&gp.sched)
}

// Finds a runnable goroutine to execute.
// Tries to steal from other P's, get g from local or global queue, poll network.
// tryWakeP indicates that the returned goroutine is not normal (GC worker, trace
// reader) so the caller should try to wake a P.
// 查找一个可运行的goroutine来执行
// 尝试从其他P偷取、从本地或全局队列获取g、轮询网络
// tryWakeP表示返回的goroutine不是普通的(GC工作线程、跟踪读取器)，
// 所以调用者应该尝试唤醒一个P
func findRunnable() (gp *g, inheritTime, tryWakeP bool) {
	mp := getg().m

	// The conditions here and in handoffp must agree: if
	// findrunnable would return a G to run, handoffp must start
	// an M.
	// 这里的条件必须与handoffp中的条件一致：
	// 如果findrunnable会返回一个G来运行，handoffp必须启动一个M

top:
	pp := mp.p.ptr()
	// 如果GC正在等待，停止当前M并重新开始调度
	if sched.gcwaiting.Load() {
		gcstopm()
		goto top
	}
	// 如果P有安全点函数需要运行，则运行它
	if pp.runSafePointFn != 0 {
		runSafePointFn()
	}

	// now and pollUntil are saved for work stealing later,
	// which may steal timers. It's important that between now
	// and then, nothing blocks, so these numbers remain mostly
	// relevant.
	// 保存当前时间和下次轮询时间，用于后续的工作窃取
	// 这些时间可能用于窃取定时器。重要的是在现在和那时之间
	// 不能有任何阻塞，这样这些时间值才能保持相关性
	now, pollUntil, _ := pp.timers.check(0)

	// Try to schedule the trace reader.
	// 尝试调度跟踪读取器
	if traceEnabled() || traceShuttingDown() {
		gp := traceReader()
		if gp != nil {
			trace := traceAcquire()
			// 将跟踪读取器的状态从等待改为可运行
			casgstatus(gp, _Gwaiting, _Grunnable)
			if trace.ok() {
				// 记录goroutine被唤醒的事件
				trace.GoUnpark(gp, 0)
				traceRelease(trace)
			}
			// 返回跟踪读取器goroutine，不继承时间片，需要尝试唤醒P
			return gp, false, true
		}
	}

	// Try to schedule a GC worker.
	// 尝试调度一个GC工作线程
	if gcBlackenEnabled != 0 {
		// 如果GC标记阶段已启用
		gp, tnow := gcController.findRunnableGCWorker(pp, now)
		// 尝试从GC控制器获取一个可运行的GC工作线程
		// 同时获取当前时间戳
		if gp != nil {
			// 如果找到了可运行的GC工作线程
			return gp, false, true
			// 返回该GC工作线程，不继承时间片，需要尝试唤醒P
		}
		now = tnow
		// 更新当前时间戳
	}

	// Check the global runnable queue once in a while to ensure fairness.
	// Otherwise two goroutines can completely occupy the local runqueue
	// by constantly respawning each other.
	// 定期检查全局可运行队列以确保公平性
	// 否则两个goroutine可能会通过不断互相重生
	// 完全占据本地运行队列
	if pp.schedtick%61 == 0 && sched.runqsize > 0 {
		// 每61个调度周期检查一次全局队列
		// 且全局队列中有goroutine时
		lock(&sched.lock)
		// 获取全局队列中的一个goroutine
		// 第二个参数1表示只获取一个goroutine
		gp := globrunqget(pp, 1)
		unlock(&sched.lock)
		if gp != nil {
			// 如果成功获取到goroutine
			// 返回该goroutine，不继承时间片，不需要唤醒P
			return gp, false, false
		}
	}

	// Wake up the finalizer G.
	// 唤醒终结器G
	if fingStatus.Load()&(fingWait|fingWake) == fingWait|fingWake {
		// 检查终结器状态是否为等待且需要唤醒
		if gp := wakefing(); gp != nil {
			// 尝试唤醒终结器G
			ready(gp, 0, true)
			// 将终结器G设置为就绪状态
			// 参数0表示不继承时间片
			// 参数true表示需要尝试唤醒P
		}
	}
	if *cgo_yield != nil {
		// 如果cgo_yield函数指针不为空
		asmcgocall(*cgo_yield, nil)
		// 调用cgo_yield函数，让出CPU时间片
	}

	// local runq
	// 检查本地运行队列
	if gp, inheritTime := runqget(pp); gp != nil {
		// 尝试从本地运行队列获取一个goroutine
		// inheritTime表示是否继承时间片
		return gp, inheritTime, false
		// 返回获取到的goroutine，继承时间片状态，不需要唤醒P
	}

	// global runq
	// 检查全局运行队列
	if sched.runqsize != 0 {
		// 如果全局运行队列不为空
		lock(&sched.lock)
		// 获取全局队列中的goroutine
		// 第二个参数0表示获取尽可能多的goroutine
		gp := globrunqget(pp, 0)
		unlock(&sched.lock)
		if gp != nil {
			// 如果成功获取到goroutine
			// 返回该goroutine，不继承时间片，不需要唤醒P
			return gp, false, false
		}
	}

	// Poll network.
	// This netpoll is only an optimization before we resort to stealing.
	// We can safely skip it if there are no waiters or a thread is blocked
	// in netpoll already. If there is any kind of logical race with that
	// blocked thread (e.g. it has already returned from netpoll, but does
	// not set lastpoll yet), this thread will do blocking netpoll below
	// anyway.
	// 轮询网络
	// 这个netpoll只是在开始窃取工作之前的优化
	// 如果没有等待者或者已经有线程在netpoll中被阻塞，我们可以安全地跳过它
	// 如果与那个被阻塞的线程存在任何逻辑竞争（例如它已经从netpoll返回，
	// 但还没有设置lastpoll），这个线程无论如何都会在下面执行阻塞的netpoll
	if netpollinited() && netpollAnyWaiters() && sched.lastpoll.Load() != 0 {
		// 检查网络轮询器是否已初始化、是否有等待者、且上次轮询时间不为0
		if list, delta := netpoll(0); !list.empty() { // non-blocking
			// 执行非阻塞的网络轮询，获取就绪的goroutine列表和等待者数量变化
			gp := list.pop()
			// 从列表中取出第一个goroutine
			injectglist(&list)
			// 将剩余的goroutine注入到运行队列中
			netpollAdjustWaiters(delta)
			// 调整网络轮询器的等待者数量
			trace := traceAcquire()
			// 获取追踪器
			casgstatus(gp, _Gwaiting, _Grunnable)
			// 将goroutine状态从等待改为可运行
			if trace.ok() {
				trace.GoUnpark(gp, 0)
				// 记录goroutine被唤醒的事件
				traceRelease(trace)
				// 释放追踪器
			}
			return gp, false, false
			// 返回获取到的goroutine，不继承时间片，不需要唤醒P
		}
	}

	// Spinning Ms: steal work from other Ps.
	//
	// Limit the number of spinning Ms to half the number of busy Ps.
	// This is necessary to prevent excessive CPU consumption when
	// GOMAXPROCS>>1 but the program parallelism is low.
	// 自旋的M：从其他P窃取工作
	//
	// 将自旋M的数量限制为繁忙P数量的一半
	// 当GOMAXPROCS远大于1但程序并行度较低时，这可以防止CPU过度消耗
	if mp.spinning || 2*sched.nmspinning.Load() < gomaxprocs-sched.npidle.Load() {
		// 如果当前M正在自旋，或者自旋M的数量小于(总P数-空闲P数)/2
		if !mp.spinning {
			// 如果当前M不在自旋状态
			mp.becomeSpinning()
			// 将M设置为自旋状态
		}

		gp, inheritTime, tnow, w, newWork := stealWork(now)
		// 尝试从其他P窃取工作，返回：
		// gp: 窃取到的goroutine
		// inheritTime: 是否继承时间片
		// tnow: 当前时间
		// w: 下一个定时器触发时间
		// newWork: 是否有新的工作
		if gp != nil {
			// 如果成功窃取到goroutine
			// Successfully stole.
			return gp, inheritTime, false
			// 返回窃取到的goroutine，继承时间片状态，不需要唤醒P
		}
		if newWork {
			// 如果有新的工作（定时器或GC工作）
			// There may be new timer or GC work; restart to
			// discover.
			goto top
			// 跳转到函数开始处重新查找工作
		}

		now = tnow
		// 更新当前时间
		if w != 0 && (pollUntil == 0 || w < pollUntil) {
			// 如果存在下一个定时器触发时间，且该时间早于当前pollUntil时间
			// Earlier timer to wait for.
			pollUntil = w
			// 更新pollUntil为更早的定时器时间
		}
	}

	// We have nothing to do.
	//
	// If we're in the GC mark phase, can safely scan and blacken objects,
	// and have work to do, run idle-time marking rather than give up the P.
	// 我们无事可做
	//
	// 如果我们在GC标记阶段，可以安全地扫描和标记对象，
	// 并且有工作要做，运行空闲时间标记而不是放弃P
	if gcBlackenEnabled != 0 && gcMarkWorkAvailable(pp) && gcController.addIdleMarkWorker() {
		// 如果GC标记阶段已启用，当前P有标记工作可用，并且成功添加空闲标记工作器
		node := (*gcBgMarkWorkerNode)(gcBgMarkWorkerPool.pop())
		// 从GC后台标记工作器池中获取一个工作器节点
		if node != nil {
			// 如果成功获取到工作器节点
			pp.gcMarkWorkerMode = gcMarkWorkerIdleMode
			// 设置P的GC标记工作器模式为空闲模式
			gp := node.gp.ptr()
			// 获取工作器节点对应的goroutine

			trace := traceAcquire()
			// 获取追踪器
			casgstatus(gp, _Gwaiting, _Grunnable)
			// 将goroutine状态从等待状态转换为可运行状态
			if trace.ok() {
				// 如果追踪器有效
				trace.GoUnpark(gp, 0)
				// 记录goroutine被唤醒的事件
				traceRelease(trace)
				// 释放追踪器
			}
			return gp, false, false
			// 返回找到的goroutine，不继承时间片，不需要唤醒P
		}
		gcController.removeIdleMarkWorker()
		// 如果未能获取到工作器节点，移除空闲标记工作器
	}

	// wasm only:
	// If a callback returned and no other goroutine is awake,
	// then wake event handler goroutine which pauses execution
	// until a callback was triggered.
	// wasm 专用:
	// 如果回调已返回且没有其他goroutine处于唤醒状态,
	// 则唤醒事件处理goroutine,该goroutine会暂停执行
	// 直到触发回调
	gp, otherReady := beforeIdle(now, pollUntil)
	if gp != nil {
		trace := traceAcquire()
		casgstatus(gp, _Gwaiting, _Grunnable)
		if trace.ok() {
			trace.GoUnpark(gp, 0)
			traceRelease(trace)
		}
		return gp, false, false
	}
	if otherReady {
		goto top
	}

	// Before we drop our P, make a snapshot of the allp slice,
	// which can change underfoot once we no longer block
	// safe-points. We don't need to snapshot the contents because
	// everything up to cap(allp) is immutable.
	// 在释放P之前，对allp切片进行快照
	// 因为一旦我们不再阻塞安全点，allp可能会在脚下发生变化
	// 我们不需要对内容进行快照，因为直到cap(allp)的所有内容都是不可变的
	allpSnapshot := allp
	// Also snapshot masks. Value changes are OK, but we can't allow
	// len to change out from under us.
	// 同时也对掩码进行快照。值的变化是可以的，但我们不能允许
	// len在我们不知情的情况下发生变化
	idlepMaskSnapshot := idlepMask
	timerpMaskSnapshot := timerpMask

	// return P and block
	// 返回P并阻塞
	lock(&sched.lock)
	// 获取调度器锁

	if sched.gcwaiting.Load() || pp.runSafePointFn != 0 {
		// 如果正在等待GC或者需要运行安全点函数
		unlock(&sched.lock)
		goto top
		// 释放锁并跳转到top重新查找可运行的goroutine
	}

	if sched.runqsize != 0 {
		// 如果全局运行队列中有goroutine
		gp := globrunqget(pp, 0)
		// 从全局运行队列获取一个goroutine
		unlock(&sched.lock)
		return gp, false, false
		// 释放锁并返回找到的goroutine
	}

	if !mp.spinning && sched.needspinning.Load() == 1 {
		// 如果当前M不是spinning状态且需要spinning
		// See "Delicate dance" comment below.
		mp.becomeSpinning()
		// 将M转换为spinning状态
		unlock(&sched.lock)
		goto top
		// 释放锁并跳转到top重新查找可运行的goroutine
	}

	if releasep() != pp {
		// 释放当前P，如果返回的P不是当前P则抛出异常
		throw("findrunnable: wrong p")
	}

	now = pidleput(pp, now)
	// 将P放回空闲P列表
	unlock(&sched.lock)
	// 释放调度器锁

	// Delicate dance: thread transitions from spinning to non-spinning
	// state, potentially concurrently with submission of new work. We must
	// drop nmspinning first and then check all sources again (with
	// #StoreLoad memory barrier in between). If we do it the other way
	// around, another thread can submit work after we've checked all
	// sources but before we drop nmspinning; as a result nobody will
	// unpark a thread to run the work.
	//
	// This applies to the following sources of work:
	//
	// * Goroutines added to the global or a per-P run queue.
	// * New/modified-earlier timers on a per-P timer heap.
	// * Idle-priority GC work (barring golang.org/issue/19112).
	//
	// If we discover new work below, we need to restore m.spinning as a
	// signal for resetspinning to unpark a new worker thread (because
	// there can be more than one starving goroutine).
	//
	// However, if after discovering new work we also observe no idle Ps
	// (either here or in resetspinning), we have a problem. We may be
	// racing with a non-spinning M in the block above, having found no
	// work and preparing to release its P and park. Allowing that P to go
	// idle will result in loss of work conservation (idle P while there is
	// runnable work). This could result in complete deadlock in the
	// unlikely event that we discover new work (from netpoll) right as we
	// are racing with _all_ other Ps going idle.
	//
	// We use sched.needspinning to synchronize with non-spinning Ms going
	// idle. If needspinning is set when they are about to drop their P,
	// they abort the drop and instead become a new spinning M on our
	// behalf. If we are not racing and the system is truly fully loaded
	// then no spinning threads are required, and the next thread to
	// naturally become spinning will clear the flag.
	//
	// Also see "Worker thread parking/unparking" comment at the top of the
	// file.

	// 微妙的舞蹈：线程从spinning状态转换到non-spinning状态，
	// 可能同时有新工作提交。我们必须先减少nmspinning，
	// 然后再检查所有工作源（在这之间有一个#StoreLoad内存屏障）。
	// 如果我们以相反的顺序执行，另一个线程可能在我们检查完所有工作源之后、
	// 但在我们减少nmspinning之前提交工作；结果就是没有人会唤醒一个线程来执行这个工作。
	//
	// 这适用于以下工作源：
	//
	// * 添加到全局或每个P的运行队列中的goroutine
	// * 每个P的定时器堆上的新/修改的定时器
	// * 空闲优先级的GC工作（除了golang.org/issue/19112的情况）
	//
	// 如果我们在下面发现新工作，我们需要恢复m.spinning作为信号，
	// 让resetspinning唤醒一个新的工作线程（因为可能有多个饥饿的goroutine）。
	//
	// 然而，如果在发现新工作后我们也观察到没有空闲的P
	// （无论是在这里还是在resetspinning中），我们就会遇到问题。
	// 我们可能与上面代码块中的non-spinning M竞争，
	// 那个M没有找到工作并准备释放它的P并park。
	// 允许那个P进入空闲状态将导致工作守恒的损失
	// （在有可运行工作的情况下有P处于空闲状态）。
	// 这可能导致完全死锁，在极不可能的情况下，
	// 当我们与所有其他P进入空闲状态竞争时，
	// 我们正好从netpoll发现了新工作。
	//
	// 我们使用sched.needspinning来与进入空闲状态的non-spinning M同步。
	// 如果它们在准备释放P时needspinning被设置，
	// 它们会中止释放操作，而是代表我们成为一个新的spinning M。
	// 如果我们没有在竞争，并且系统确实完全负载，
	// 那么就不需要spinning线程，
	// 下一个自然成为spinning的线程将清除这个标志。
	//
	// 另请参见文件顶部的"Worker thread parking/unparking"注释。
	wasSpinning := mp.spinning
	if mp.spinning {
		mp.spinning = false
		if sched.nmspinning.Add(-1) < 0 {
			throw("findrunnable: negative nmspinning")
		}

		// Note the for correctness, only the last M transitioning from
		// spinning to non-spinning must perform these rechecks to
		// ensure no missed work. However, the runtime has some cases
		// of transient increments of nmspinning that are decremented
		// without going through this path, so we must be conservative
		// and perform the check on all spinning Ms.
		//
		// See https://go.dev/issue/43997.
		// 注意：为了正确性，只有最后一个从spinning状态转换到non-spinning状态的M
		// 必须执行这些重新检查以确保没有遗漏的工作。然而，运行时有一些情况下
		// nmspinning会临时增加，然后不经过这个路径就减少，所以我们必须保守地
		// 在所有spinning的M上执行这个检查。
		//
		// 参见 https://go.dev/issue/43997。

		// Check global and P runqueues again.
		// 再次检查全局和P的运行队列。

		// Lock the scheduler to check the global run queue.
		// 锁定调度器以检查全局运行队列
		lock(&sched.lock)
		// Check if there are any goroutines in the global run queue.
		// 检查全局运行队列中是否有任何goroutine
		if sched.runqsize != 0 {
			// Try to get an idle P that can be used for spinning.
			// 尝试获取一个可以用于spinning的空闲P
			pp, _ := pidlegetSpinning(0)
			if pp != nil {
				// Get a goroutine from the global run queue.
				// 从全局运行队列获取一个goroutine
				gp := globrunqget(pp, 0)
				if gp == nil {
					// This should never happen - if runqsize is non-zero, we should find a goroutine.
					// 这不应该发生 - 如果runqsize不为零，我们应该找到一个goroutine
					throw("global runq empty with non-zero runqsize")
				}
				// Unlock the scheduler and acquire the P we found.
				// 解锁调度器并获取我们找到的P
				unlock(&sched.lock)
				acquirep(pp)
				// Mark this M as spinning and return the found goroutine.
				// 将此M标记为spinning并返回找到的goroutine
				mp.becomeSpinning()
				return gp, false, false
			}
		}
		unlock(&sched.lock)

		// Check all Ps for a runnable G to steal.
		// 检查所有P中是否有可窃取的runnable G
		pp := checkRunqsNoP(allpSnapshot, idlepMaskSnapshot)
		if pp != nil {
			// If we found a P with work, acquire it and become spinning.
			// 如果找到了有工作的P，获取它并进入spinning状态
			acquirep(pp)
			mp.becomeSpinning()
			goto top
		}

		// Check for idle-priority GC work again.
		// 再次检查是否有空闲优先级的GC工作
		pp, gp := checkIdleGCNoP()
		if pp != nil {
			// If we found both a P and a GC worker G, acquire the P and become spinning.
			// 如果同时找到了P和GC工作G，获取P并进入spinning状态
			acquirep(pp)
			mp.becomeSpinning()

			// Run the idle worker.
			// 运行空闲工作器
			pp.gcMarkWorkerMode = gcMarkWorkerIdleMode
			trace := traceAcquire()
			// Change the GC worker's status from waiting to runnable.
			// 将GC工作器的状态从waiting改为runnable
			casgstatus(gp, _Gwaiting, _Grunnable)
			if trace.ok() {
				// Record the unpark event in the trace.
				// 在trace中记录unpark事件
				trace.GoUnpark(gp, 0)
				traceRelease(trace)
			}
			return gp, false, false
		}

		// Finally, check for timer creation or expiry concurrently with
		// transitioning from spinning to non-spinning.
		// 最后，在从spinning状态转换到non-spinning状态的同时，
		// 检查是否有定时器创建或到期
		//
		// Note that we cannot use checkTimers here because it calls
		// adjusttimers which may need to allocate memory, and that isn't
		// allowed when we don't have an active P.
		// 注意我们不能在这里使用checkTimers，因为它会调用adjusttimers，
		// 而adjusttimers可能需要分配内存，这在没有活跃P的情况下是不允许的
		pollUntil = checkTimersNoP(allpSnapshot, timerpMaskSnapshot, pollUntil)
	}

	// Poll network until next timer.
	// 轮询网络直到下一个定时器触发
	if netpollinited() && (netpollAnyWaiters() || pollUntil != 0) && sched.lastpoll.Swap(0) != 0 {
		// 如果网络轮询器已初始化，并且(有等待的网络事件 或 有定时器到期时间)，
		// 且成功将sched.lastpoll从非0值交换为0
		sched.pollUntil.Store(pollUntil)
		// 存储下一个定时器到期时间
		if mp.p != 0 {
			// 如果当前M已经绑定了P，不应该进行网络轮询
			throw("findrunnable: netpoll with p")
		}
		if mp.spinning {
			// 如果当前M处于spinning状态，不应该进行网络轮询
			throw("findrunnable: netpoll with spinning")
		}
		// Calculate the delay time for network polling
		// 计算网络轮询的延迟时间
		delay := int64(-1)
		if pollUntil != 0 {
			// If we have a timer deadline, calculate the delay
			// 如果有定时器截止时间，计算延迟
			if now == 0 {
				// Get current time if not already available
				// 如果当前时间不可用，获取当前时间
				now = nanotime()
			}
			// Calculate time until next timer expires
			// 计算到下一个定时器到期的时间
			delay = pollUntil - now
			if delay < 0 {
				// Ensure delay is not negative
				// 确保延迟不为负数
				delay = 0
			}
		}
		if faketime != 0 {
			// When using fake time, just poll.
			delay = 0
		}
		list, delta := netpoll(delay) // block until new work is available
		// Refresh now again, after potentially blocking.
		now = nanotime()
		sched.pollUntil.Store(0)
		sched.lastpoll.Store(now)
		if faketime != 0 && list.empty() {
			// Using fake time and nothing is ready; stop M.
			// When all M's stop, checkdead will call timejump.
			stopm()
			goto top
		}
		lock(&sched.lock)
		pp, _ := pidleget(now)
		unlock(&sched.lock)
		if pp == nil {
			injectglist(&list)
			netpollAdjustWaiters(delta)
		} else {
			acquirep(pp)
			if !list.empty() {
				gp := list.pop()
				injectglist(&list)
				netpollAdjustWaiters(delta)
				trace := traceAcquire()
				casgstatus(gp, _Gwaiting, _Grunnable)
				if trace.ok() {
					trace.GoUnpark(gp, 0)
					traceRelease(trace)
				}
				return gp, false, false
			}
			if wasSpinning {
				mp.becomeSpinning()
			}
			goto top
		}
	} else if pollUntil != 0 && netpollinited() {
		pollerPollUntil := sched.pollUntil.Load()
		if pollerPollUntil == 0 || pollerPollUntil > pollUntil {
			netpollBreak()
		}
	}
	stopm()
	goto top
}

// pollWork reports whether there is non-background work this P could
// be doing. This is a fairly lightweight check to be used for
// background work loops, like idle GC. It checks a subset of the
// conditions checked by the actual scheduler.
func pollWork() bool {
	if sched.runqsize != 0 {
		return true
	}
	p := getg().m.p.ptr()
	if !runqempty(p) {
		return true
	}
	if netpollinited() && netpollAnyWaiters() && sched.lastpoll.Load() != 0 {
		if list, delta := netpoll(0); !list.empty() {
			injectglist(&list)
			netpollAdjustWaiters(delta)
			return true
		}
	}
	return false
}

// stealWork attempts to steal a runnable goroutine or timer from any P.
//
// If newWork is true, new work may have been readied.
//
// If now is not 0 it is the current time. stealWork returns the passed time or
// the current time if now was passed as 0.
func stealWork(now int64) (gp *g, inheritTime bool, rnow, pollUntil int64, newWork bool) {
	pp := getg().m.p.ptr()

	ranTimer := false

	const stealTries = 4
	for i := 0; i < stealTries; i++ {
		stealTimersOrRunNextG := i == stealTries-1

		for enum := stealOrder.start(cheaprand()); !enum.done(); enum.next() {
			if sched.gcwaiting.Load() {
				// GC work may be available.
				return nil, false, now, pollUntil, true
			}
			p2 := allp[enum.position()]
			if pp == p2 {
				continue
			}

			// Steal timers from p2. This call to checkTimers is the only place
			// where we might hold a lock on a different P's timers. We do this
			// once on the last pass before checking runnext because stealing
			// from the other P's runnext should be the last resort, so if there
			// are timers to steal do that first.
			//
			// We only check timers on one of the stealing iterations because
			// the time stored in now doesn't change in this loop and checking
			// the timers for each P more than once with the same value of now
			// is probably a waste of time.
			//
			// timerpMask tells us whether the P may have timers at all. If it
			// can't, no need to check at all.
			if stealTimersOrRunNextG && timerpMask.read(enum.position()) {
				tnow, w, ran := p2.timers.check(now)
				now = tnow
				if w != 0 && (pollUntil == 0 || w < pollUntil) {
					pollUntil = w
				}
				if ran {
					// Running the timers may have
					// made an arbitrary number of G's
					// ready and added them to this P's
					// local run queue. That invalidates
					// the assumption of runqsteal
					// that it always has room to add
					// stolen G's. So check now if there
					// is a local G to run.
					if gp, inheritTime := runqget(pp); gp != nil {
						return gp, inheritTime, now, pollUntil, ranTimer
					}
					ranTimer = true
				}
			}

			// Don't bother to attempt to steal if p2 is idle.
			if !idlepMask.read(enum.position()) {
				if gp := runqsteal(pp, p2, stealTimersOrRunNextG); gp != nil {
					return gp, false, now, pollUntil, ranTimer
				}
			}
		}
	}

	// No goroutines found to steal. Regardless, running a timer may have
	// made some goroutine ready that we missed. Indicate the next timer to
	// wait for.
	return nil, false, now, pollUntil, ranTimer
}

// Check all Ps for a runnable G to steal.
//
// On entry we have no P. If a G is available to steal and a P is available,
// the P is returned which the caller should acquire and attempt to steal the
// work to.
func checkRunqsNoP(allpSnapshot []*p, idlepMaskSnapshot pMask) *p {
	for id, p2 := range allpSnapshot {
		if !idlepMaskSnapshot.read(uint32(id)) && !runqempty(p2) {
			lock(&sched.lock)
			pp, _ := pidlegetSpinning(0)
			if pp == nil {
				// Can't get a P, don't bother checking remaining Ps.
				unlock(&sched.lock)
				return nil
			}
			unlock(&sched.lock)
			return pp
		}
	}

	// No work available.
	return nil
}

// Check all Ps for a timer expiring sooner than pollUntil.
//
// Returns updated pollUntil value.
func checkTimersNoP(allpSnapshot []*p, timerpMaskSnapshot pMask, pollUntil int64) int64 {
	for id, p2 := range allpSnapshot {
		if timerpMaskSnapshot.read(uint32(id)) {
			w := p2.timers.wakeTime()
			if w != 0 && (pollUntil == 0 || w < pollUntil) {
				pollUntil = w
			}
		}
	}

	return pollUntil
}

// Check for idle-priority GC, without a P on entry.
//
// If some GC work, a P, and a worker G are all available, the P and G will be
// returned. The returned P has not been wired yet.
func checkIdleGCNoP() (*p, *g) {
	// N.B. Since we have no P, gcBlackenEnabled may change at any time; we
	// must check again after acquiring a P. As an optimization, we also check
	// if an idle mark worker is needed at all. This is OK here, because if we
	// observe that one isn't needed, at least one is currently running. Even if
	// it stops running, its own journey into the scheduler should schedule it
	// again, if need be (at which point, this check will pass, if relevant).
	if atomic.Load(&gcBlackenEnabled) == 0 || !gcController.needIdleMarkWorker() {
		return nil, nil
	}
	if !gcMarkWorkAvailable(nil) {
		return nil, nil
	}

	// Work is available; we can start an idle GC worker only if there is
	// an available P and available worker G.
	//
	// We can attempt to acquire these in either order, though both have
	// synchronization concerns (see below). Workers are almost always
	// available (see comment in findRunnableGCWorker for the one case
	// there may be none). Since we're slightly less likely to find a P,
	// check for that first.
	//
	// Synchronization: note that we must hold sched.lock until we are
	// committed to keeping it. Otherwise we cannot put the unnecessary P
	// back in sched.pidle without performing the full set of idle
	// transition checks.
	//
	// If we were to check gcBgMarkWorkerPool first, we must somehow handle
	// the assumption in gcControllerState.findRunnableGCWorker that an
	// empty gcBgMarkWorkerPool is only possible if gcMarkDone is running.
	lock(&sched.lock)
	pp, now := pidlegetSpinning(0)
	if pp == nil {
		unlock(&sched.lock)
		return nil, nil
	}

	// Now that we own a P, gcBlackenEnabled can't change (as it requires STW).
	if gcBlackenEnabled == 0 || !gcController.addIdleMarkWorker() {
		pidleput(pp, now)
		unlock(&sched.lock)
		return nil, nil
	}

	node := (*gcBgMarkWorkerNode)(gcBgMarkWorkerPool.pop())
	if node == nil {
		pidleput(pp, now)
		unlock(&sched.lock)
		gcController.removeIdleMarkWorker()
		return nil, nil
	}

	unlock(&sched.lock)

	return pp, node.gp.ptr()
}

// wakeNetPoller wakes up the thread sleeping in the network poller if it isn't
// going to wake up before the when argument; or it wakes an idle P to service
// timers and the network poller if there isn't one already.
func wakeNetPoller(when int64) {
	if sched.lastpoll.Load() == 0 {
		// In findrunnable we ensure that when polling the pollUntil
		// field is either zero or the time to which the current
		// poll is expected to run. This can have a spurious wakeup
		// but should never miss a wakeup.
		pollerPollUntil := sched.pollUntil.Load()
		if pollerPollUntil == 0 || pollerPollUntil > when {
			netpollBreak()
		}
	} else {
		// There are no threads in the network poller, try to get
		// one there so it can handle new timers.
		if GOOS != "plan9" { // Temporary workaround - see issue #42303.
			wakep()
		}
	}
}

func resetspinning() {
	gp := getg()
	if !gp.m.spinning {
		throw("resetspinning: not a spinning m")
	}
	gp.m.spinning = false
	nmspinning := sched.nmspinning.Add(-1)
	if nmspinning < 0 {
		throw("findrunnable: negative nmspinning")
	}
	// M wakeup policy is deliberately somewhat conservative, so check if we
	// need to wakeup another P here. See "Worker thread parking/unparking"
	// comment at the top of the file for details.
	wakep()
}

// injectglist adds each runnable G on the list to some run queue,
// and clears glist. If there is no current P, they are added to the
// global queue, and up to npidle M's are started to run them.
// Otherwise, for each idle P, this adds a G to the global queue
// and starts an M. Any remaining G's are added to the current P's
// local run queue.
// This may temporarily acquire sched.lock.
// Can run concurrently with GC.
func injectglist(glist *gList) {
	if glist.empty() {
		return
	}

	// Mark all the goroutines as runnable before we put them
	// on the run queues.
	head := glist.head.ptr()
	var tail *g
	qsize := 0
	trace := traceAcquire()
	for gp := head; gp != nil; gp = gp.schedlink.ptr() {
		tail = gp
		qsize++
		casgstatus(gp, _Gwaiting, _Grunnable)
		if trace.ok() {
			trace.GoUnpark(gp, 0)
		}
	}
	if trace.ok() {
		traceRelease(trace)
	}

	// Turn the gList into a gQueue.
	var q gQueue
	q.head.set(head)
	q.tail.set(tail)
	*glist = gList{}

	startIdle := func(n int) {
		for i := 0; i < n; i++ {
			mp := acquirem() // See comment in startm.
			lock(&sched.lock)

			pp, _ := pidlegetSpinning(0)
			if pp == nil {
				unlock(&sched.lock)
				releasem(mp)
				break
			}

			startm(pp, false, true)
			unlock(&sched.lock)
			releasem(mp)
		}
	}

	pp := getg().m.p.ptr()
	if pp == nil {
		lock(&sched.lock)
		globrunqputbatch(&q, int32(qsize))
		unlock(&sched.lock)
		startIdle(qsize)
		return
	}

	npidle := int(sched.npidle.Load())
	var (
		globq gQueue
		n     int
	)
	for n = 0; n < npidle && !q.empty(); n++ {
		g := q.pop()
		globq.pushBack(g)
	}
	if n > 0 {
		lock(&sched.lock)
		globrunqputbatch(&globq, int32(n))
		unlock(&sched.lock)
		startIdle(n)
		qsize -= n
	}

	if !q.empty() {
		runqputbatch(pp, &q, qsize)
	}

	// Some P's might have become idle after we loaded `sched.npidle`
	// but before any goroutines were added to the queue, which could
	// lead to idle P's when there is work available in the global queue.
	// That could potentially last until other goroutines become ready
	// to run. That said, we need to find a way to hedge
	//
	// Calling wakep() here is the best bet, it will do nothing in the
	// common case (no racing on `sched.npidle`), while it could wake one
	// more P to execute G's, which might end up with >1 P's: the first one
	// wakes another P and so forth until there is no more work, but this
	// ought to be an extremely rare case.
	//
	// Also see "Worker thread parking/unparking" comment at the top of the file for details.
	wakep()
}

// One round of scheduler: find a runnable goroutine and execute it.
// Never returns.
// 调度器的一轮调度：找到一个可运行的goroutine并执行它
// 此函数永远不会返回
func schedule() {
	mp := getg().m

	if mp.locks != 0 {
		throw("schedule: holding locks")
	}

	if mp.lockedg != 0 {
		stoplockedm()
		execute(mp.lockedg.ptr(), false) // Never returns.
	}

	// We should not schedule away from a g that is executing a cgo call,
	// since the cgo call is using the m's g0 stack.
	// 我们不应该从正在执行cgo调用的g中调度出去
	// 因为cgo调用正在使用m的g0栈
	if mp.incgo {
		throw("schedule: in cgo")
	}

top:
	pp := mp.p.ptr()
	pp.preempt = false

	// Safety check: if we are spinning, the run queue should be empty.
	// Check this before calling checkTimers, as that might call
	// goready to put a ready goroutine on the local run queue.
	// 安全检查：如果我们在自旋，运行队列应该是空的
	// 在调用checkTimers之前检查这一点，因为它可能会调用
	// goready将就绪的goroutine放入本地运行队列
	if mp.spinning && (pp.runnext != 0 || pp.runqhead != pp.runqtail) {
		throw("schedule: spinning with local work")
	}

	gp, inheritTime, tryWakeP := findRunnable() // blocks until work is available
	// 查找可运行的goroutine，如果没有可运行的goroutine会阻塞直到有工作可用
	// gp: 找到的可运行goroutine
	// inheritTime: 是否继承时间片
	// tryWakeP: 是否需要尝试唤醒P

	if debug.dontfreezetheworld > 0 && freezing.Load() {
		// See comment in freezetheworld. We don't want to perturb
		// scheduler state, so we didn't gcstopm in findRunnable, but
		// also don't want to allow new goroutines to run.
		//
		// Deadlock here rather than in the findRunnable loop so if
		// findRunnable is stuck in a loop we don't perturb that
		// either.
		// 参见freezetheworld中的注释。我们不想干扰调度器状态，
		// 所以没有在findRunnable中调用gcstopm，但也不想允许新的goroutine运行
		// 在这里死锁而不是在findRunnable循环中，这样如果findRunnable卡在循环中，
		// 我们也不会干扰它
		lock(&deadlock)
		lock(&deadlock)
	}

	// This thread is going to run a goroutine and is not spinning anymore,
	// so if it was marked as spinning we need to reset it now and potentially
	// start a new spinning M.
	// 这个线程即将运行一个goroutine，不再处于自旋状态，
	// 所以如果它被标记为自旋状态，我们需要现在重置它，
	// 并可能启动一个新的自旋M。
	if mp.spinning {
		resetspinning()
	}

	if sched.disable.user && !schedEnabled(gp) {
		// Scheduling of this goroutine is disabled. Put it on
		// the list of pending runnable goroutines for when we
		// re-enable user scheduling and look again.
		// 如果用户调度被禁用且当前goroutine不允许被调度，
		// 将其放入待运行goroutine列表中，等待用户调度重新启用时再次尝试
		lock(&sched.lock)
		if schedEnabled(gp) {
			// Something re-enabled scheduling while we
			// were acquiring the lock.
			// 在获取锁的过程中，调度被重新启用了
			unlock(&sched.lock)
		} else {
			// 将goroutine添加到待运行队列
			sched.disable.runnable.pushBack(gp)
			// 增加待运行goroutine计数
			sched.disable.n++
			unlock(&sched.lock)
			// 跳转到top重新开始调度
			goto top
		}
	}

	// If about to schedule a not-normal goroutine (a GCworker or tracereader),
	// wake a P if there is one.
	// 如果要调度一个非普通goroutine(如GC工作线程或跟踪读取器)，
	// 如果有可用的P，则唤醒一个P
	if tryWakeP {
		wakep()
	}
	if gp.lockedm != 0 {
		// Hands off own p to the locked m,
		// then blocks waiting for a new p.
		// 将当前P交给被锁定的M，
		// 然后阻塞等待获取一个新的P
		startlockedm(gp)
		goto top
	}

	execute(gp, inheritTime)
}

// dropg removes the association between m and the current goroutine m->curg (gp for short).
// Typically a caller sets gp's status away from Grunning and then
// immediately calls dropg to finish the job. The caller is also responsible
// for arranging that gp will be restarted using ready at an
// appropriate time. After calling dropg and arranging for gp to be
// readied later, the caller can do other work but eventually should
// call schedule to restart the scheduling of goroutines on this m.
func dropg() {
	gp := getg()

	setMNoWB(&gp.m.curg.m, nil)
	setGNoWB(&gp.m.curg, nil)
}

func parkunlock_c(gp *g, lock unsafe.Pointer) bool {
	unlock((*mutex)(lock))
	return true
}

// park continuation on g0.
func park_m(gp *g) {
	mp := getg().m

	trace := traceAcquire()

	if trace.ok() {
		// Trace the event before the transition. It may take a
		// stack trace, but we won't own the stack after the
		// transition anymore.
		trace.GoPark(mp.waitTraceBlockReason, mp.waitTraceSkip)
	}
	// N.B. Not using casGToWaiting here because the waitreason is
	// set by park_m's caller.
	casgstatus(gp, _Grunning, _Gwaiting)
	if trace.ok() {
		traceRelease(trace)
	}

	dropg()

	if fn := mp.waitunlockf; fn != nil {
		ok := fn(gp, mp.waitlock)
		mp.waitunlockf = nil
		mp.waitlock = nil
		if !ok {
			trace := traceAcquire()
			casgstatus(gp, _Gwaiting, _Grunnable)
			if trace.ok() {
				trace.GoUnpark(gp, 2)
				traceRelease(trace)
			}
			execute(gp, true) // Schedule it back, never returns.
		}
	}
	schedule()
}

// goschedImpl是调度器内部使用的函数，用于将goroutine从运行状态切换到可运行状态
// 它处理跟踪事件的记录，并确保状态转换的安全性
func goschedImpl(gp *g, preempted bool) {
	// 获取跟踪器实例
	trace := traceAcquire()
	// 读取goroutine的状态
	status := readgstatus(gp)
	// 检查goroutine是否处于运行状态(排除扫描标记)
	if status&^_Gscan != _Grunning {
		// 如果状态不正确，打印goroutine状态并抛出异常
		dumpgstatus(gp)
		throw("bad g status")
	}
	if trace.ok() {
		// Trace the event before the transition. It may take a
		// stack trace, but we won't own the stack after the
		// transition anymore.
		// 在状态转换前记录跟踪事件。这可能需要获取栈跟踪，
		// 但在状态转换后我们将不再拥有该栈。
		if preempted {
			// 如果是被抢占，记录抢占事件
			trace.GoPreempt()
		} else {
			// 否则记录调度事件
			trace.GoSched()
		}
	}
	// 将goroutine状态从运行中(_Grunning)改为可运行(_Grunnable)
	casgstatus(gp, _Grunning, _Grunnable)
	if trace.ok() {
		// 释放跟踪器
		traceRelease(trace)
	}

	// 解除goroutine与当前M的绑定
	dropg()
	// 获取调度器锁
	lock(&sched.lock)
	// 将goroutine放入全局运行队列
	globrunqput(gp)
	// 释放调度器锁
	unlock(&sched.lock)

	// 如果主goroutine已启动，尝试唤醒一个P来运行goroutine
	if mainStarted {
		wakep()
	}

	// 重新调度，寻找下一个要运行的goroutine
	schedule()
}

// Gosched continuation on g0.
func gosched_m(gp *g) {
	goschedImpl(gp, false)
}

// goschedguarded is a forbidden-states-avoided version of gosched_m.
func goschedguarded_m(gp *g) {
	if !canPreemptM(gp.m) {
		gogo(&gp.sched) // never return
	}
	goschedImpl(gp, false)
}

func gopreempt_m(gp *g) {
	goschedImpl(gp, true)
}

// preemptPark parks gp and puts it in _Gpreempted.
//
//go:systemstack
func preemptPark(gp *g) {
	status := readgstatus(gp)
	if status&^_Gscan != _Grunning {
		dumpgstatus(gp)
		throw("bad g status")
	}

	if gp.asyncSafePoint {
		// Double-check that async preemption does not
		// happen in SPWRITE assembly functions.
		// isAsyncSafePoint must exclude this case.
		f := findfunc(gp.sched.pc)
		if !f.valid() {
			throw("preempt at unknown pc")
		}
		if f.flag&abi.FuncFlagSPWrite != 0 {
			println("runtime: unexpected SPWRITE function", funcname(f), "in async preempt")
			throw("preempt SPWRITE")
		}
	}

	// Transition from _Grunning to _Gscan|_Gpreempted. We can't
	// be in _Grunning when we dropg because then we'd be running
	// without an M, but the moment we're in _Gpreempted,
	// something could claim this G before we've fully cleaned it
	// up. Hence, we set the scan bit to lock down further
	// transitions until we can dropg.
	casGToPreemptScan(gp, _Grunning, _Gscan|_Gpreempted)
	dropg()

	// Be careful about how we trace this next event. The ordering
	// is subtle.
	//
	// The moment we CAS into _Gpreempted, suspendG could CAS to
	// _Gwaiting, do its work, and ready the goroutine. All of
	// this could happen before we even get the chance to emit
	// an event. The end result is that the events could appear
	// out of order, and the tracer generally assumes the scheduler
	// takes care of the ordering between GoPark and GoUnpark.
	//
	// The answer here is simple: emit the event while we still hold
	// the _Gscan bit on the goroutine. We still need to traceAcquire
	// and traceRelease across the CAS because the tracer could be
	// what's calling suspendG in the first place, and we want the
	// CAS and event emission to appear atomic to the tracer.
	trace := traceAcquire()
	if trace.ok() {
		trace.GoPark(traceBlockPreempted, 0)
	}
	casfrom_Gscanstatus(gp, _Gscan|_Gpreempted, _Gpreempted)
	if trace.ok() {
		traceRelease(trace)
	}
	schedule()
}

// goyield is like Gosched, but it:
// - emits a GoPreempt trace event instead of a GoSched trace event
// - puts the current G on the runq of the current P instead of the globrunq
//
// goyield should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - gvisor.dev/gvisor
//   - github.com/sagernet/gvisor
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname goyield
func goyield() {
	checkTimeouts()
	mcall(goyield_m)
}

func goyield_m(gp *g) {
	trace := traceAcquire()
	pp := gp.m.p.ptr()
	if trace.ok() {
		// Trace the event before the transition. It may take a
		// stack trace, but we won't own the stack after the
		// transition anymore.
		trace.GoPreempt()
	}
	casgstatus(gp, _Grunning, _Grunnable)
	if trace.ok() {
		traceRelease(trace)
	}
	dropg()
	runqput(pp, gp, false)
	schedule()
}

// Finishes execution of the current goroutine.
func goexit1() {
	if raceenabled {
		racegoend()
	}
	trace := traceAcquire()
	if trace.ok() {
		trace.GoEnd()
		traceRelease(trace)
	}
	mcall(goexit0)
}

// goexit continuation on g0.
func goexit0(gp *g) {
	gdestroy(gp)
	schedule()
}

func gdestroy(gp *g) {
	mp := getg().m
	pp := mp.p.ptr()

	casgstatus(gp, _Grunning, _Gdead)
	gcController.addScannableStack(pp, -int64(gp.stack.hi-gp.stack.lo))
	if isSystemGoroutine(gp, false) {
		sched.ngsys.Add(-1)
	}
	gp.m = nil
	locked := gp.lockedm != 0
	gp.lockedm = 0
	mp.lockedg = 0
	gp.preemptStop = false
	gp.paniconfault = false
	gp._defer = nil // should be true already but just in case.
	gp._panic = nil // non-nil for Goexit during panic. points at stack-allocated data.
	gp.writebuf = nil
	gp.waitreason = waitReasonZero
	gp.param = nil
	gp.labels = nil
	gp.timer = nil

	if gcBlackenEnabled != 0 && gp.gcAssistBytes > 0 {
		// Flush assist credit to the global pool. This gives
		// better information to pacing if the application is
		// rapidly creating an exiting goroutines.
		assistWorkPerByte := gcController.assistWorkPerByte.Load()
		scanCredit := int64(assistWorkPerByte * float64(gp.gcAssistBytes))
		gcController.bgScanCredit.Add(scanCredit)
		gp.gcAssistBytes = 0
	}

	dropg()

	if GOARCH == "wasm" { // no threads yet on wasm
		gfput(pp, gp)
		return
	}

	if locked && mp.lockedInt != 0 {
		print("runtime: mp.lockedInt = ", mp.lockedInt, "\n")
		throw("exited a goroutine internally locked to the OS thread")
	}
	gfput(pp, gp)
	if locked {
		// The goroutine may have locked this thread because
		// it put it in an unusual kernel state. Kill it
		// rather than returning it to the thread pool.

		// Return to mstart, which will release the P and exit
		// the thread.
		if GOOS != "plan9" { // See golang.org/issue/22227.
			gogo(&mp.g0.sched)
		} else {
			// Clear lockedExt on plan9 since we may end up re-using
			// this thread.
			mp.lockedExt = 0
		}
	}
}

// save updates getg().sched to refer to pc and sp so that a following
// gogo will restore pc and sp.
//
// save must not have write barriers because invoking a write barrier
// can clobber getg().sched.
//
//go:nosplit
//go:nowritebarrierrec
func save(pc, sp, bp uintptr) {
	gp := getg()

	if gp == gp.m.g0 || gp == gp.m.gsignal {
		// m.g0.sched is special and must describe the context
		// for exiting the thread. mstart1 writes to it directly.
		// m.gsignal.sched should not be used at all.
		// This check makes sure save calls do not accidentally
		// run in contexts where they'd write to system g's.
		throw("save on system g not allowed")
	}

	gp.sched.pc = pc
	gp.sched.sp = sp
	gp.sched.lr = 0
	gp.sched.ret = 0
	gp.sched.bp = bp
	// We need to ensure ctxt is zero, but can't have a write
	// barrier here. However, it should always already be zero.
	// Assert that.
	if gp.sched.ctxt != nil {
		badctxt()
	}
}

// The goroutine g is about to enter a system call.
// Record that it's not using the cpu anymore.
// This is called only from the go syscall library and cgocall,
// not from the low-level system calls used by the runtime.
//
// Entersyscall cannot split the stack: the save must
// make g->sched refer to the caller's stack segment, because
// entersyscall is going to return immediately after.
//
// Nothing entersyscall calls can split the stack either.
// We cannot safely move the stack during an active call to syscall,
// because we do not know which of the uintptr arguments are
// really pointers (back into the stack).
// In practice, this means that we make the fast path run through
// entersyscall doing no-split things, and the slow path has to use systemstack
// to run bigger things on the system stack.
//
// reentersyscall is the entry point used by cgo callbacks, where explicitly
// saved SP and PC are restored. This is needed when exitsyscall will be called
// from a function further up in the call stack than the parent, as g->syscallsp
// must always point to a valid stack frame. entersyscall below is the normal
// entry point for syscalls, which obtains the SP and PC from the caller.
//
//go:nosplit
func reentersyscall(pc, sp, bp uintptr) {
	trace := traceAcquire()
	gp := getg()

	// Disable preemption because during this function g is in Gsyscall status,
	// but can have inconsistent g->sched, do not let GC observe it.
	gp.m.locks++

	// Entersyscall must not call any function that might split/grow the stack.
	// (See details in comment above.)
	// Catch calls that might, by replacing the stack guard with something that
	// will trip any stack check and leaving a flag to tell newstack to die.
	gp.stackguard0 = stackPreempt
	gp.throwsplit = true

	// Leave SP around for GC and traceback.
	save(pc, sp, bp)
	gp.syscallsp = sp
	gp.syscallpc = pc
	gp.syscallbp = bp
	casgstatus(gp, _Grunning, _Gsyscall)
	if staticLockRanking {
		// When doing static lock ranking casgstatus can call
		// systemstack which clobbers g.sched.
		save(pc, sp, bp)
	}
	if gp.syscallsp < gp.stack.lo || gp.stack.hi < gp.syscallsp {
		systemstack(func() {
			print("entersyscall inconsistent sp ", hex(gp.syscallsp), " [", hex(gp.stack.lo), ",", hex(gp.stack.hi), "]\n")
			throw("entersyscall")
		})
	}
	if gp.syscallbp != 0 && gp.syscallbp < gp.stack.lo || gp.stack.hi < gp.syscallbp {
		systemstack(func() {
			print("entersyscall inconsistent bp ", hex(gp.syscallbp), " [", hex(gp.stack.lo), ",", hex(gp.stack.hi), "]\n")
			throw("entersyscall")
		})
	}

	if trace.ok() {
		systemstack(func() {
			trace.GoSysCall()
			traceRelease(trace)
		})
		// systemstack itself clobbers g.sched.{pc,sp} and we might
		// need them later when the G is genuinely blocked in a
		// syscall
		save(pc, sp, bp)
	}

	if sched.sysmonwait.Load() {
		systemstack(entersyscall_sysmon)
		save(pc, sp, bp)
	}

	if gp.m.p.ptr().runSafePointFn != 0 {
		// runSafePointFn may stack split if run on this stack
		systemstack(runSafePointFn)
		save(pc, sp, bp)
	}

	gp.m.syscalltick = gp.m.p.ptr().syscalltick
	pp := gp.m.p.ptr()
	pp.m = 0
	gp.m.oldp.set(pp)
	gp.m.p = 0
	atomic.Store(&pp.status, _Psyscall)
	if sched.gcwaiting.Load() {
		systemstack(entersyscall_gcwait)
		save(pc, sp, bp)
	}

	gp.m.locks--
}

// Standard syscall entry used by the go syscall library and normal cgo calls.
//
// This is exported via linkname to assembly in the syscall package and x/sys.
//
// Other packages should not be accessing entersyscall directly,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - gvisor.dev/gvisor
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:nosplit
//go:linkname entersyscall
func entersyscall() {
	// N.B. getcallerfp cannot be written directly as argument in the call
	// to reentersyscall because it forces spilling the other arguments to
	// the stack. This results in exceeding the nosplit stack requirements
	// on some platforms.
	fp := getcallerfp()
	reentersyscall(getcallerpc(), getcallersp(), fp)
}

func entersyscall_sysmon() {
	lock(&sched.lock)
	if sched.sysmonwait.Load() {
		sched.sysmonwait.Store(false)
		notewakeup(&sched.sysmonnote)
	}
	unlock(&sched.lock)
}

func entersyscall_gcwait() {
	gp := getg()
	pp := gp.m.oldp.ptr()

	lock(&sched.lock)
	trace := traceAcquire()
	if sched.stopwait > 0 && atomic.Cas(&pp.status, _Psyscall, _Pgcstop) {
		if trace.ok() {
			// This is a steal in the new tracer. While it's very likely
			// that we were the ones to put this P into _Psyscall, between
			// then and now it's totally possible it had been stolen and
			// then put back into _Psyscall for us to acquire here. In such
			// case ProcStop would be incorrect.
			//
			// TODO(mknyszek): Consider emitting a ProcStop instead when
			// gp.m.syscalltick == pp.syscalltick, since then we know we never
			// lost the P.
			trace.ProcSteal(pp, true)
			traceRelease(trace)
		}
		pp.gcStopTime = nanotime()
		pp.syscalltick++
		if sched.stopwait--; sched.stopwait == 0 {
			notewakeup(&sched.stopnote)
		}
	} else if trace.ok() {
		traceRelease(trace)
	}
	unlock(&sched.lock)
}

// The same as entersyscall(), but with a hint that the syscall is blocking.

// entersyscallblock should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - gvisor.dev/gvisor
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname entersyscallblock
//go:nosplit
func entersyscallblock() {
	gp := getg()

	gp.m.locks++ // see comment in entersyscall
	gp.throwsplit = true
	gp.stackguard0 = stackPreempt // see comment in entersyscall
	gp.m.syscalltick = gp.m.p.ptr().syscalltick
	gp.m.p.ptr().syscalltick++

	// Leave SP around for GC and traceback.
	pc := getcallerpc()
	sp := getcallersp()
	bp := getcallerfp()
	save(pc, sp, bp)
	gp.syscallsp = gp.sched.sp
	gp.syscallpc = gp.sched.pc
	gp.syscallbp = gp.sched.bp
	if gp.syscallsp < gp.stack.lo || gp.stack.hi < gp.syscallsp {
		sp1 := sp
		sp2 := gp.sched.sp
		sp3 := gp.syscallsp
		systemstack(func() {
			print("entersyscallblock inconsistent sp ", hex(sp1), " ", hex(sp2), " ", hex(sp3), " [", hex(gp.stack.lo), ",", hex(gp.stack.hi), "]\n")
			throw("entersyscallblock")
		})
	}
	casgstatus(gp, _Grunning, _Gsyscall)
	if gp.syscallsp < gp.stack.lo || gp.stack.hi < gp.syscallsp {
		systemstack(func() {
			print("entersyscallblock inconsistent sp ", hex(sp), " ", hex(gp.sched.sp), " ", hex(gp.syscallsp), " [", hex(gp.stack.lo), ",", hex(gp.stack.hi), "]\n")
			throw("entersyscallblock")
		})
	}
	if gp.syscallbp != 0 && gp.syscallbp < gp.stack.lo || gp.stack.hi < gp.syscallbp {
		systemstack(func() {
			print("entersyscallblock inconsistent bp ", hex(bp), " ", hex(gp.sched.bp), " ", hex(gp.syscallbp), " [", hex(gp.stack.lo), ",", hex(gp.stack.hi), "]\n")
			throw("entersyscallblock")
		})
	}

	systemstack(entersyscallblock_handoff)

	// Resave for traceback during blocked call.
	save(getcallerpc(), getcallersp(), getcallerfp())

	gp.m.locks--
}

func entersyscallblock_handoff() {
	trace := traceAcquire()
	if trace.ok() {
		trace.GoSysCall()
		traceRelease(trace)
	}
	handoffp(releasep())
}

// The goroutine g exited its system call.
// Arrange for it to run on a cpu again.
// This is called only from the go syscall library, not
// from the low-level system calls used by the runtime.
//
// Write barriers are not allowed because our P may have been stolen.
//
// This is exported via linkname to assembly in the syscall package.
//
// exitsyscall should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - gvisor.dev/gvisor
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:nosplit
//go:nowritebarrierrec
//go:linkname exitsyscall
func exitsyscall() {
	gp := getg()

	gp.m.locks++ // see comment in entersyscall
	if getcallersp() > gp.syscallsp {
		throw("exitsyscall: syscall frame is no longer valid")
	}

	gp.waitsince = 0
	oldp := gp.m.oldp.ptr()
	gp.m.oldp = 0
	if exitsyscallfast(oldp) {
		// When exitsyscallfast returns success, we have a P so can now use
		// write barriers
		if goroutineProfile.active {
			// Make sure that gp has had its stack written out to the goroutine
			// profile, exactly as it was when the goroutine profiler first
			// stopped the world.
			systemstack(func() {
				tryRecordGoroutineProfileWB(gp)
			})
		}
		trace := traceAcquire()
		if trace.ok() {
			lostP := oldp != gp.m.p.ptr() || gp.m.syscalltick != gp.m.p.ptr().syscalltick
			systemstack(func() {
				// Write out syscall exit eagerly.
				//
				// It's important that we write this *after* we know whether we
				// lost our P or not (determined by exitsyscallfast).
				trace.GoSysExit(lostP)
				if lostP {
					// We lost the P at some point, even though we got it back here.
					// Trace that we're starting again, because there was a traceGoSysBlock
					// call somewhere in exitsyscallfast (indicating that this goroutine
					// had blocked) and we're about to start running again.
					trace.GoStart()
				}
			})
		}
		// There's a cpu for us, so we can run.
		gp.m.p.ptr().syscalltick++
		// We need to cas the status and scan before resuming...
		casgstatus(gp, _Gsyscall, _Grunning)
		if trace.ok() {
			traceRelease(trace)
		}

		// Garbage collector isn't running (since we are),
		// so okay to clear syscallsp.
		gp.syscallsp = 0
		gp.m.locks--
		if gp.preempt {
			// restore the preemption request in case we've cleared it in newstack
			gp.stackguard0 = stackPreempt
		} else {
			// otherwise restore the real stackGuard, we've spoiled it in entersyscall/entersyscallblock
			gp.stackguard0 = gp.stack.lo + stackGuard
		}
		gp.throwsplit = false

		if sched.disable.user && !schedEnabled(gp) {
			// Scheduling of this goroutine is disabled.
			Gosched()
		}

		return
	}

	gp.m.locks--

	// Call the scheduler.
	mcall(exitsyscall0)

	// Scheduler returned, so we're allowed to run now.
	// Delete the syscallsp information that we left for
	// the garbage collector during the system call.
	// Must wait until now because until gosched returns
	// we don't know for sure that the garbage collector
	// is not running.
	gp.syscallsp = 0
	gp.m.p.ptr().syscalltick++
	gp.throwsplit = false
}

//go:nosplit
func exitsyscallfast(oldp *p) bool {
	// Freezetheworld sets stopwait but does not retake P's.
	if sched.stopwait == freezeStopWait {
		return false
	}

	// Try to re-acquire the last P.
	trace := traceAcquire()
	if oldp != nil && oldp.status == _Psyscall && atomic.Cas(&oldp.status, _Psyscall, _Pidle) {
		// There's a cpu for us, so we can run.
		wirep(oldp)
		exitsyscallfast_reacquired(trace)
		if trace.ok() {
			traceRelease(trace)
		}
		return true
	}
	if trace.ok() {
		traceRelease(trace)
	}

	// Try to get any other idle P.
	if sched.pidle != 0 {
		var ok bool
		systemstack(func() {
			ok = exitsyscallfast_pidle()
		})
		if ok {
			return true
		}
	}
	return false
}

// exitsyscallfast_reacquired is the exitsyscall path on which this G
// has successfully reacquired the P it was running on before the
// syscall.
//
//go:nosplit
func exitsyscallfast_reacquired(trace traceLocker) {
	gp := getg()
	if gp.m.syscalltick != gp.m.p.ptr().syscalltick {
		if trace.ok() {
			// The p was retaken and then enter into syscall again (since gp.m.syscalltick has changed).
			// traceGoSysBlock for this syscall was already emitted,
			// but here we effectively retake the p from the new syscall running on the same p.
			systemstack(func() {
				// We're stealing the P. It's treated
				// as if it temporarily stopped running. Then, start running.
				trace.ProcSteal(gp.m.p.ptr(), true)
				trace.ProcStart()
			})
		}
		gp.m.p.ptr().syscalltick++
	}
}

func exitsyscallfast_pidle() bool {
	lock(&sched.lock)
	pp, _ := pidleget(0)
	if pp != nil && sched.sysmonwait.Load() {
		sched.sysmonwait.Store(false)
		notewakeup(&sched.sysmonnote)
	}
	unlock(&sched.lock)
	if pp != nil {
		acquirep(pp)
		return true
	}
	return false
}

// exitsyscall slow path on g0.
// Failed to acquire P, enqueue gp as runnable.
//
// Called via mcall, so gp is the calling g from this M.
//
//go:nowritebarrierrec
func exitsyscall0(gp *g) {
	var trace traceLocker
	traceExitingSyscall()
	trace = traceAcquire()
	casgstatus(gp, _Gsyscall, _Grunnable)
	traceExitedSyscall()
	if trace.ok() {
		// Write out syscall exit eagerly.
		//
		// It's important that we write this *after* we know whether we
		// lost our P or not (determined by exitsyscallfast).
		trace.GoSysExit(true)
		traceRelease(trace)
	}
	dropg()
	lock(&sched.lock)
	var pp *p
	if schedEnabled(gp) {
		pp, _ = pidleget(0)
	}
	var locked bool
	if pp == nil {
		globrunqput(gp)

		// Below, we stoplockedm if gp is locked. globrunqput releases
		// ownership of gp, so we must check if gp is locked prior to
		// committing the release by unlocking sched.lock, otherwise we
		// could race with another M transitioning gp from unlocked to
		// locked.
		locked = gp.lockedm != 0
	} else if sched.sysmonwait.Load() {
		sched.sysmonwait.Store(false)
		notewakeup(&sched.sysmonnote)
	}
	unlock(&sched.lock)
	if pp != nil {
		acquirep(pp)
		execute(gp, false) // Never returns.
	}
	if locked {
		// Wait until another thread schedules gp and so m again.
		//
		// N.B. lockedm must be this M, as this g was running on this M
		// before entersyscall.
		stoplockedm()
		execute(gp, false) // Never returns.
	}
	stopm()
	schedule() // Never returns.
}

// Called from syscall package before fork.
//
// syscall_runtime_BeforeFork is for package syscall,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/containerd/containerd
//   - gvisor.dev/gvisor
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname syscall_runtime_BeforeFork syscall.runtime_BeforeFork
//go:nosplit
func syscall_runtime_BeforeFork() {
	gp := getg().m.curg

	// Block signals during a fork, so that the child does not run
	// a signal handler before exec if a signal is sent to the process
	// group. See issue #18600.
	gp.m.locks++
	sigsave(&gp.m.sigmask)
	sigblock(false)

	// This function is called before fork in syscall package.
	// Code between fork and exec must not allocate memory nor even try to grow stack.
	// Here we spoil g.stackguard0 to reliably detect any attempts to grow stack.
	// runtime_AfterFork will undo this in parent process, but not in child.
	gp.stackguard0 = stackFork
}

// Called from syscall package after fork in parent.
//
// syscall_runtime_AfterFork is for package syscall,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/containerd/containerd
//   - gvisor.dev/gvisor
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname syscall_runtime_AfterFork syscall.runtime_AfterFork
//go:nosplit
func syscall_runtime_AfterFork() {
	gp := getg().m.curg

	// See the comments in beforefork.
	gp.stackguard0 = gp.stack.lo + stackGuard

	msigrestore(gp.m.sigmask)

	gp.m.locks--
}

// inForkedChild is true while manipulating signals in the child process.
// This is used to avoid calling libc functions in case we are using vfork.
var inForkedChild bool

// Called from syscall package after fork in child.
// It resets non-sigignored signals to the default handler, and
// restores the signal mask in preparation for the exec.
//
// Because this might be called during a vfork, and therefore may be
// temporarily sharing address space with the parent process, this must
// not change any global variables or calling into C code that may do so.
//
// syscall_runtime_AfterForkInChild is for package syscall,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/containerd/containerd
//   - gvisor.dev/gvisor
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname syscall_runtime_AfterForkInChild syscall.runtime_AfterForkInChild
//go:nosplit
//go:nowritebarrierrec
func syscall_runtime_AfterForkInChild() {
	// It's OK to change the global variable inForkedChild here
	// because we are going to change it back. There is no race here,
	// because if we are sharing address space with the parent process,
	// then the parent process can not be running concurrently.
	inForkedChild = true

	clearSignalHandlers()

	// When we are the child we are the only thread running,
	// so we know that nothing else has changed gp.m.sigmask.
	msigrestore(getg().m.sigmask)

	inForkedChild = false
}

// pendingPreemptSignals is the number of preemption signals
// that have been sent but not received. This is only used on Darwin.
// For #41702.
var pendingPreemptSignals atomic.Int32

// Called from syscall package before Exec.
//
//go:linkname syscall_runtime_BeforeExec syscall.runtime_BeforeExec
func syscall_runtime_BeforeExec() {
	// Prevent thread creation during exec.
	execLock.lock()

	// On Darwin, wait for all pending preemption signals to
	// be received. See issue #41702.
	if GOOS == "darwin" || GOOS == "ios" {
		for pendingPreemptSignals.Load() > 0 {
			osyield()
		}
	}
}

// Called from syscall package after Exec.
//
//go:linkname syscall_runtime_AfterExec syscall.runtime_AfterExec
func syscall_runtime_AfterExec() {
	execLock.unlock()
}

// Allocate a new g, with a stack big enough for stacksize bytes.
// 分配一个新的goroutine(g)，栈大小足够容纳stacksize字节
func malg(stacksize int32) *g {
	// 创建一个新的goroutine结构体
	newg := new(g)
	if stacksize >= 0 {
		// 将栈大小向上取整到2的幂次方，并加上系统栈的大小
		stacksize = round2(stackSystem + stacksize)
		// 在系统栈上执行栈分配操作
		systemstack(func() {
			newg.stack = stackalloc(uint32(stacksize))
		})
		// 设置栈保护区域，防止栈溢出
		newg.stackguard0 = newg.stack.lo + stackGuard
		// 设置栈保护区域的上界为最大值
		newg.stackguard1 = ^uintptr(0)
		// Clear the bottom word of the stack. We record g
		// there on gsignal stack during VDSO on ARM and ARM64.
		// 清除栈底部的字。在ARM和ARM64架构上，我们在VDSO期间
		// 在gsignal栈上记录g的位置
		*(*uintptr)(unsafe.Pointer(newg.stack.lo)) = 0
	}
	return newg
}

// Create a new g running fn.
// Put it on the queue of g's waiting to run.
// The compiler turns a go statement into a call to this.
// 创建一个新的goroutine来运行fn函数
// 将其放入等待运行的goroutine队列中
// 编译器会将go语句转换为对此函数的调用
func newproc(fn *funcval) {
	// 获取当前goroutine
	gp := getg()
	// 获取调用者的程序计数器
	pc := getcallerpc()
	// 在系统栈上执行以下操作
	systemstack(func() {
		// 创建新的goroutine，传入函数、当前goroutine、调用者PC等信息
		newg := newproc1(fn, gp, pc, false, waitReasonZero)

		// 获取当前M的P
		pp := getg().m.p.ptr()
		// 将新创建的goroutine放入P的运行队列
		runqput(pp, newg, true)

		// 如果主goroutine已经启动，尝试唤醒一个P来运行新创建的goroutine
		if mainStarted {
			wakep()
		}
	})
}

// Create a new g in state _Grunnable (or _Gwaiting if parked is true), starting at fn.
// callerpc is the address of the go statement that created this. The caller is responsible
// for adding the new g to the scheduler. If parked is true, waitreason must be non-zero.
// 创建一个新的goroutine，初始状态为_Grunnable(如果parked为true则为_Gwaiting)，从fn开始执行
// callerpc是创建此goroutine的go语句的地址。调用者负责将新的goroutine添加到调度器中
// 如果parked为true，waitreason必须非零
func newproc1(fn *funcval, callergp *g, callerpc uintptr, parked bool, waitreason waitReason) *g {
	if fn == nil {
		fatal("go of nil func value")
	}

	mp := acquirem() // disable preemption because we hold M and P in local vars.
	// 获取当前M，禁用抢占，因为我们在局部变量中持有M和P
	pp := mp.p.ptr()
	newg := gfget(pp)
	if newg == nil {
		newg = malg(stackMin)
		casgstatus(newg, _Gidle, _Gdead)
		allgadd(newg) // publishes with a g->status of Gdead so GC scanner doesn't look at uninitialized stack.
		// 将goroutine添加到全局列表中，状态为Gdead，这样GC扫描器就不会查看未初始化的栈
	}
	if newg.stack.hi == 0 {
		throw("newproc1: newg missing stack")
	}

	if readgstatus(newg) != _Gdead {
		throw("newproc1: new g is not Gdead")
	}

	totalSize := uintptr(4*goarch.PtrSize + sys.MinFrameSize) // extra space in case of reads slightly beyond frame
	// 计算栈帧所需的总大小，包括4个指针大小的空间和最小帧大小
	// 额外空间用于处理略微超出帧范围的读取操作

	totalSize = alignUp(totalSize, sys.StackAlign)
	// 将总大小向上对齐到栈对齐要求

	sp := newg.stack.hi - totalSize
	// 计算新的栈指针位置，从栈顶减去所需的总大小

	if usesLR {
		// caller's LR
		// 如果使用链接寄存器(LR)
		*(*uintptr)(unsafe.Pointer(sp)) = 0
		// 将栈顶的链接寄存器值初始化为0
		prepGoExitFrame(sp)
		// 准备goroutine退出帧
	}
	if GOARCH == "arm64" {
		// caller's FP
		// 如果是ARM64架构
		*(*uintptr)(unsafe.Pointer(sp - goarch.PtrSize)) = 0
		// 将栈指针下方一个指针大小的位置(帧指针)初始化为0
	}

	// Clear the scheduling context of the new goroutine
	// 清除新goroutine的调度上下文
	memclrNoHeapPointers(unsafe.Pointer(&newg.sched), unsafe.Sizeof(newg.sched))

	// Set up the stack pointer and top of stack
	// 设置栈指针和栈顶位置
	newg.sched.sp = sp
	newg.stktopsp = sp

	// Set the program counter to goexit + PCQuantum
	// +PCQuantum ensures the previous instruction is in the same function
	// 设置程序计数器为goexit + PCQuantum
	// +PCQuantum确保前一条指令在同一函数中
	newg.sched.pc = abi.FuncPCABI0(goexit) + sys.PCQuantum

	// Set the goroutine pointer in the scheduling context
	// 在调度上下文中设置goroutine指针
	newg.sched.g = guintptr(unsafe.Pointer(newg))

	// Set up the function call in the scheduling context
	// 在调度上下文中设置函数调用
	gostartcallfn(&newg.sched, fn)

	// Set parent goroutine ID and caller PC
	// 设置父goroutine ID和调用者PC
	newg.parentGoid = callergp.goid
	newg.gopc = callerpc

	// Save ancestor information for stack traces
	// 保存用于栈跟踪的祖先信息
	newg.ancestors = saveAncestors(callergp)

	// Set the start PC to the function being called
	// 设置起始PC为被调用的函数
	newg.startpc = fn.fn

	// Handle system goroutine vs user goroutine setup
	// 处理系统goroutine和用户goroutine的设置
	if isSystemGoroutine(newg, false) {
		// Increment the count of system goroutines
		// 增加系统goroutine计数
		sched.ngsys.Add(1)
	} else {
		// Only user goroutines inherit pprof labels.
		// 只有用户goroutine继承pprof标签
		if mp.curg != nil {
			newg.labels = mp.curg.labels
		}
		if goroutineProfile.active {
			// A concurrent goroutine profile is running. It should include
			// exactly the set of goroutines that were alive when the goroutine
			// profiler first stopped the world. That does not include newg, so
			// mark it as not needing a profile before transitioning it from
			// _Gdead.
			// 并发goroutine分析正在运行。它应该只包含在goroutine分析器
			// 首次停止世界时存活的goroutine集合。这不包括newg，所以在从
			// _Gdead状态转换之前将其标记为不需要分析
			newg.goroutineProfiled.Store(goroutineProfileSatisfied)
		}
	}
	// Track initial transition?
	// 跟踪初始转换？
	newg.trackingSeq = uint8(cheaprand())
	if newg.trackingSeq%gTrackingPeriod == 0 {
		newg.tracking = true
	}
	// 将新goroutine的栈空间添加到可扫描栈列表中
	gcController.addScannableStack(pp, int64(newg.stack.hi-newg.stack.lo))

	// Get a goid and switch to runnable. Make all this atomic to the tracer.
	// 获取goroutine ID并切换到可运行状态。确保所有这些操作对追踪器来说是原子的。
	trace := traceAcquire()
	var status uint32 = _Grunnable
	if parked {
		status = _Gwaiting
		newg.waitreason = waitreason
	}
	// 将goroutine状态从_Gdead切换到目标状态
	casgstatus(newg, _Gdead, status)
	if pp.goidcache == pp.goidcacheend {
		// Sched.goidgen is the last allocated id,
		// this batch must be [sched.goidgen+1, sched.goidgen+GoidCacheBatch].
		// At startup sched.goidgen=0, so main goroutine receives goid=1.
		// Sched.goidgen是最后分配的ID，
		// 这一批ID必须是[sched.goidgen+1, sched.goidgen+GoidCacheBatch]。
		// 在启动时sched.goidgen=0，所以主goroutine获得goid=1。
		pp.goidcache = sched.goidgen.Add(_GoidCacheBatch)
		pp.goidcache -= _GoidCacheBatch - 1
		pp.goidcacheend = pp.goidcache + _GoidCacheBatch
	}
	// 分配新的goroutine ID并递增缓存
	newg.goid = pp.goidcache
	pp.goidcache++
	// 重置goroutine的追踪状态
	newg.trace.reset()
	if trace.ok() {
		// 记录goroutine创建事件
		trace.GoCreate(newg, newg.startpc, parked)
		traceRelease(trace)
	}

	// Set up race context.
	// 设置竞态检测上下文
	if raceenabled {
		// 如果启用了竞态检测，初始化新goroutine的竞态检测上下文
		newg.racectx = racegostart(callerpc)
		// 重置竞态检测忽略标志
		newg.raceignore = 0
		if newg.labels != nil {
			// See note in proflabel.go on labelSync's role in synchronizing
			// with the reads in the signal handler.
			// 如果goroutine有标签，释放并合并竞态检测状态
			// 注意：关于labelSync在同步信号处理器读取中的作用，请参见proflabel.go中的说明
			racereleasemergeg(newg, unsafe.Pointer(&labelSync))
		}
	}
	// 释放当前M的锁
	releasem(mp)

	return newg
}

// saveAncestors copies previous ancestors of the given caller g and
// includes info for the current caller into a new set of tracebacks for
// a g being created.
// saveAncestors函数复制给定调用者g的祖先信息，并将当前调用者的信息包含到
// 正在创建的g的新回溯集合中
func saveAncestors(callergp *g) *[]ancestorInfo {
	// Copy all prior info, except for the root goroutine (goid 0).
	// 复制所有先前的信息，除了根goroutine（goid为0）
	if debug.tracebackancestors <= 0 || callergp.goid == 0 {
		return nil
	}
	var callerAncestors []ancestorInfo
	if callergp.ancestors != nil {
		callerAncestors = *callergp.ancestors
	}
	// 计算需要的祖先信息数量，加1是为了包含当前调用者
	n := int32(len(callerAncestors)) + 1
	// 如果超过最大限制，则截断到限制值
	if n > debug.tracebackancestors {
		n = debug.tracebackancestors
	}
	// 创建新的祖先信息切片
	ancestors := make([]ancestorInfo, n)
	// 将调用者的祖先信息复制到新切片中，从索引1开始
	copy(ancestors[1:], callerAncestors)

	// 获取调用者的调用栈信息
	var pcs [tracebackInnerFrames]uintptr
	npcs := gcallers(callergp, 0, pcs[:])
	ipcs := make([]uintptr, npcs)
	copy(ipcs, pcs[:])
	// 在索引0位置存储当前调用者的信息
	ancestors[0] = ancestorInfo{
		pcs:  ipcs,          // 程序计数器数组
		goid: callergp.goid, // goroutine ID
		gopc: callergp.gopc, // goroutine的创建程序计数器
	}

	// 创建新的祖先信息切片指针并返回
	ancestorsp := new([]ancestorInfo)
	*ancestorsp = ancestors
	return ancestorsp
}

// Put on gfree list.
// If local list is too long, transfer a batch to the global list.
func gfput(pp *p, gp *g) {
	if readgstatus(gp) != _Gdead {
		throw("gfput: bad status (not Gdead)")
	}

	stksize := gp.stack.hi - gp.stack.lo

	if stksize != uintptr(startingStackSize) {
		// non-standard stack size - free it.
		stackfree(gp.stack)
		gp.stack.lo = 0
		gp.stack.hi = 0
		gp.stackguard0 = 0
	}

	pp.gFree.push(gp)
	pp.gFree.n++
	if pp.gFree.n >= 64 {
		var (
			inc      int32
			stackQ   gQueue
			noStackQ gQueue
		)
		for pp.gFree.n >= 32 {
			gp := pp.gFree.pop()
			pp.gFree.n--
			if gp.stack.lo == 0 {
				noStackQ.push(gp)
			} else {
				stackQ.push(gp)
			}
			inc++
		}
		lock(&sched.gFree.lock)
		sched.gFree.noStack.pushAll(noStackQ)
		sched.gFree.stack.pushAll(stackQ)
		sched.gFree.n += inc
		unlock(&sched.gFree.lock)
	}
}

// Get from gfree list.
// If local list is empty, grab a batch from global list.
// 从gfree列表中获取goroutine
// 如果本地列表为空，从全局列表中获取一批goroutine
func gfget(pp *p) *g {
retry:
	// 如果P的gfree列表为空，且全局gfree列表中有goroutine(有栈或无栈)
	if pp.gFree.empty() && (!sched.gFree.stack.empty() || !sched.gFree.noStack.empty()) {
		lock(&sched.gFree.lock) // 锁定全局gfree列表
		// Move a batch of free Gs to the P.
		// 将一批空闲的goroutine移动到P的本地列表中
		for pp.gFree.n < 32 { // 当P的gfree列表中的goroutine数量小于32时
			// Prefer Gs with stacks.
			// 优先获取有栈的goroutine
			gp := sched.gFree.stack.pop() // 尝试从全局有栈队列中获取goroutine
			if gp == nil {                // 如果没有有栈的goroutine
				gp := sched.gFree.noStack.pop() // 尝试从全局无栈队列中获取goroutine
				if gp == nil {                  // 如果也没有无栈的goroutine
					break // 跳出循环
				}
			}
			sched.gFree.n--   // 全局gfree计数器减1
			pp.gFree.push(gp) // 将获取到的goroutine放入P的本地gfree列表
			pp.gFree.n++      // P的gfree计数器加1
		}
		unlock(&sched.gFree.lock) // 解锁全局gfree列表
		goto retry                // 跳转到retry标签，重新尝试获取goroutine
	}
	gp := pp.gFree.pop() // 从P的本地gfree列表中获取一个goroutine
	if gp == nil {       // 如果没有获取到goroutine
		return nil // 返回nil
	}
	pp.gFree.n-- // 减少P的gfree计数器
	if gp.stack.lo != 0 && gp.stack.hi-gp.stack.lo != uintptr(startingStackSize) {
		// Deallocate old stack. We kept it in gfput because it was the
		// right size when the goroutine was put on the free list, but
		// the right size has changed since then.
		// 释放旧的栈。我们在gfput中保留它是因为当goroutine被放入空闲列表时它的大小是正确的，
		// 但自那时起正确的大小已经改变。
		systemstack(func() {
			stackfree(gp.stack) // 释放goroutine的栈
			gp.stack.lo = 0     // 清空栈的起始地址
			gp.stack.hi = 0     // 清空栈的结束地址
			gp.stackguard0 = 0  // 清空栈保护区域
		})
	}
	if gp.stack.lo == 0 {
		// Stack was deallocated in gfput or just above. Allocate a new one.
		// 栈在gfput中或上面被释放了。分配一个新的栈。
		systemstack(func() {
			gp.stack = stackalloc(startingStackSize) // 分配一个新的栈，大小为startingStackSize
		})
		gp.stackguard0 = gp.stack.lo + stackGuard // 设置栈保护区域
	} else {
		if raceenabled {
			// 如果启用了竞态检测，为栈内存注册竞态检测
			racemalloc(unsafe.Pointer(gp.stack.lo), gp.stack.hi-gp.stack.lo)
		}
		if msanenabled {
			// 如果启用了内存消毒，为栈内存注册内存消毒
			msanmalloc(unsafe.Pointer(gp.stack.lo), gp.stack.hi-gp.stack.lo)
		}
		if asanenabled {
			// 如果启用了地址消毒，取消栈内存的毒化标记
			asanunpoison(unsafe.Pointer(gp.stack.lo), gp.stack.hi-gp.stack.lo)
		}
	}
	return gp // 返回处理好的goroutine
}

// Purge all cached G's from gfree list to the global list.
// 将P的gfree列表中的所有缓存的G移动到全局列表中
func gfpurge(pp *p) {
	var (
		inc      int32  // 计数器，记录移动的G的数量
		stackQ   gQueue // 有栈的G队列
		noStackQ gQueue // 无栈的G队列
	)
	// 循环处理P的gfree列表中的所有G
	for !pp.gFree.empty() {
		gp := pp.gFree.pop()  // 从P的gfree列表中弹出一个G
		pp.gFree.n--          // P的gfree计数器减1
		if gp.stack.lo == 0 { // 如果G没有栈
			noStackQ.push(gp) // 将G加入无栈队列
		} else {
			stackQ.push(gp) // 将G加入有栈队列
		}
		inc++ // 计数器加1
	}
	// 获取全局gfree锁
	lock(&sched.gFree.lock)
	// 将无栈G队列中的所有G添加到全局gfree列表
	sched.gFree.noStack.pushAll(noStackQ)
	// 将有栈G队列中的所有G添加到全局gfree列表
	sched.gFree.stack.pushAll(stackQ)
	// 更新全局gfree计数器
	sched.gFree.n += inc
	// 释放全局gfree锁
	unlock(&sched.gFree.lock)
}

// Breakpoint executes a breakpoint trap.
func Breakpoint() {
	breakpoint()
}

// dolockOSThread is called by LockOSThread and lockOSThread below
// after they modify m.locked. Do not allow preemption during this call,
// or else the m might be different in this function than in the caller.
//
//go:nosplit
func dolockOSThread() {
	if GOARCH == "wasm" {
		return // no threads on wasm yet
	}
	gp := getg()
	gp.m.lockedg.set(gp)
	gp.lockedm.set(gp.m)
}

// LockOSThread wires the calling goroutine to its current operating system thread.
// The calling goroutine will always execute in that thread,
// and no other goroutine will execute in it,
// until the calling goroutine has made as many calls to
// [UnlockOSThread] as to LockOSThread.
// If the calling goroutine exits without unlocking the thread,
// the thread will be terminated.
//
// All init functions are run on the startup thread. Calling LockOSThread
// from an init function will cause the main function to be invoked on
// that thread.
//
// A goroutine should call LockOSThread before calling OS services or
// non-Go library functions that depend on per-thread state.
//
//go:nosplit
func LockOSThread() {
	if atomic.Load(&newmHandoff.haveTemplateThread) == 0 && GOOS != "plan9" {
		// If we need to start a new thread from the locked
		// thread, we need the template thread. Start it now
		// while we're in a known-good state.
		startTemplateThread()
	}
	gp := getg()
	gp.m.lockedExt++
	if gp.m.lockedExt == 0 {
		gp.m.lockedExt--
		panic("LockOSThread nesting overflow")
	}
	dolockOSThread()
}

//go:nosplit
func lockOSThread() {
	getg().m.lockedInt++
	dolockOSThread()
}

// dounlockOSThread is called by UnlockOSThread and unlockOSThread below
// after they update m->locked. Do not allow preemption during this call,
// or else the m might be in different in this function than in the caller.
//
//go:nosplit
func dounlockOSThread() {
	if GOARCH == "wasm" {
		return // no threads on wasm yet
	}
	gp := getg()
	if gp.m.lockedInt != 0 || gp.m.lockedExt != 0 {
		return
	}
	gp.m.lockedg = 0
	gp.lockedm = 0
}

// UnlockOSThread undoes an earlier call to LockOSThread.
// If this drops the number of active LockOSThread calls on the
// calling goroutine to zero, it unwires the calling goroutine from
// its fixed operating system thread.
// If there are no active LockOSThread calls, this is a no-op.
//
// Before calling UnlockOSThread, the caller must ensure that the OS
// thread is suitable for running other goroutines. If the caller made
// any permanent changes to the state of the thread that would affect
// other goroutines, it should not call this function and thus leave
// the goroutine locked to the OS thread until the goroutine (and
// hence the thread) exits.
//
//go:nosplit
func UnlockOSThread() {
	gp := getg()
	if gp.m.lockedExt == 0 {
		return
	}
	gp.m.lockedExt--
	dounlockOSThread()
}

//go:nosplit
func unlockOSThread() {
	gp := getg()
	if gp.m.lockedInt == 0 {
		systemstack(badunlockosthread)
	}
	gp.m.lockedInt--
	dounlockOSThread()
}

func badunlockosthread() {
	throw("runtime: internal error: misuse of lockOSThread/unlockOSThread")
}

func gcount() int32 {
	n := int32(atomic.Loaduintptr(&allglen)) - sched.gFree.n - sched.ngsys.Load()
	for _, pp := range allp {
		n -= pp.gFree.n
	}

	// All these variables can be changed concurrently, so the result can be inconsistent.
	// But at least the current goroutine is running.
	if n < 1 {
		n = 1
	}
	return n
}

func mcount() int32 {
	return int32(sched.mnext - sched.nmfreed)
}

var prof struct {
	signalLock atomic.Uint32

	// Must hold signalLock to write. Reads may be lock-free, but
	// signalLock should be taken to synchronize with changes.
	hz atomic.Int32
}

func _System()                    { _System() }
func _ExternalCode()              { _ExternalCode() }
func _LostExternalCode()          { _LostExternalCode() }
func _GC()                        { _GC() }
func _LostSIGPROFDuringAtomic64() { _LostSIGPROFDuringAtomic64() }
func _LostContendedRuntimeLock()  { _LostContendedRuntimeLock() }
func _VDSO()                      { _VDSO() }

// Called if we receive a SIGPROF signal.
// Called by the signal handler, may run during STW.
//
//go:nowritebarrierrec
func sigprof(pc, sp, lr uintptr, gp *g, mp *m) {
	if prof.hz.Load() == 0 {
		return
	}

	// If mp.profilehz is 0, then profiling is not enabled for this thread.
	// We must check this to avoid a deadlock between setcpuprofilerate
	// and the call to cpuprof.add, below.
	if mp != nil && mp.profilehz == 0 {
		return
	}

	// On mips{,le}/arm, 64bit atomics are emulated with spinlocks, in
	// internal/runtime/atomic. If SIGPROF arrives while the program is inside
	// the critical section, it creates a deadlock (when writing the sample).
	// As a workaround, create a counter of SIGPROFs while in critical section
	// to store the count, and pass it to sigprof.add() later when SIGPROF is
	// received from somewhere else (with _LostSIGPROFDuringAtomic64 as pc).
	if GOARCH == "mips" || GOARCH == "mipsle" || GOARCH == "arm" {
		if f := findfunc(pc); f.valid() {
			if stringslite.HasPrefix(funcname(f), "internal/runtime/atomic") {
				cpuprof.lostAtomic++
				return
			}
		}
		if GOARCH == "arm" && goarm < 7 && GOOS == "linux" && pc&0xffff0000 == 0xffff0000 {
			// internal/runtime/atomic functions call into kernel
			// helpers on arm < 7. See
			// internal/runtime/atomic/sys_linux_arm.s.
			cpuprof.lostAtomic++
			return
		}
	}

	// Profiling runs concurrently with GC, so it must not allocate.
	// Set a trap in case the code does allocate.
	// Note that on windows, one thread takes profiles of all the
	// other threads, so mp is usually not getg().m.
	// In fact mp may not even be stopped.
	// See golang.org/issue/17165.
	getg().m.mallocing++

	var u unwinder
	var stk [maxCPUProfStack]uintptr
	n := 0
	if mp.ncgo > 0 && mp.curg != nil && mp.curg.syscallpc != 0 && mp.curg.syscallsp != 0 {
		cgoOff := 0
		// Check cgoCallersUse to make sure that we are not
		// interrupting other code that is fiddling with
		// cgoCallers.  We are running in a signal handler
		// with all signals blocked, so we don't have to worry
		// about any other code interrupting us.
		if mp.cgoCallersUse.Load() == 0 && mp.cgoCallers != nil && mp.cgoCallers[0] != 0 {
			for cgoOff < len(mp.cgoCallers) && mp.cgoCallers[cgoOff] != 0 {
				cgoOff++
			}
			n += copy(stk[:], mp.cgoCallers[:cgoOff])
			mp.cgoCallers[0] = 0
		}

		// Collect Go stack that leads to the cgo call.
		u.initAt(mp.curg.syscallpc, mp.curg.syscallsp, 0, mp.curg, unwindSilentErrors)
	} else if usesLibcall() && mp.libcallg != 0 && mp.libcallpc != 0 && mp.libcallsp != 0 {
		// Libcall, i.e. runtime syscall on windows.
		// Collect Go stack that leads to the call.
		u.initAt(mp.libcallpc, mp.libcallsp, 0, mp.libcallg.ptr(), unwindSilentErrors)
	} else if mp != nil && mp.vdsoSP != 0 {
		// VDSO call, e.g. nanotime1 on Linux.
		// Collect Go stack that leads to the call.
		u.initAt(mp.vdsoPC, mp.vdsoSP, 0, gp, unwindSilentErrors|unwindJumpStack)
	} else {
		u.initAt(pc, sp, lr, gp, unwindSilentErrors|unwindTrap|unwindJumpStack)
	}
	n += tracebackPCs(&u, 0, stk[n:])

	if n <= 0 {
		// Normal traceback is impossible or has failed.
		// Account it against abstract "System" or "GC".
		n = 2
		if inVDSOPage(pc) {
			pc = abi.FuncPCABIInternal(_VDSO) + sys.PCQuantum
		} else if pc > firstmoduledata.etext {
			// "ExternalCode" is better than "etext".
			pc = abi.FuncPCABIInternal(_ExternalCode) + sys.PCQuantum
		}
		stk[0] = pc
		if mp.preemptoff != "" {
			stk[1] = abi.FuncPCABIInternal(_GC) + sys.PCQuantum
		} else {
			stk[1] = abi.FuncPCABIInternal(_System) + sys.PCQuantum
		}
	}

	if prof.hz.Load() != 0 {
		// Note: it can happen on Windows that we interrupted a system thread
		// with no g, so gp could nil. The other nil checks are done out of
		// caution, but not expected to be nil in practice.
		var tagPtr *unsafe.Pointer
		if gp != nil && gp.m != nil && gp.m.curg != nil {
			tagPtr = &gp.m.curg.labels
		}
		cpuprof.add(tagPtr, stk[:n])

		gprof := gp
		var mp *m
		var pp *p
		if gp != nil && gp.m != nil {
			if gp.m.curg != nil {
				gprof = gp.m.curg
			}
			mp = gp.m
			pp = gp.m.p.ptr()
		}
		traceCPUSample(gprof, mp, pp, stk[:n])
	}
	getg().m.mallocing--
}

// setcpuprofilerate sets the CPU profiling rate to hz times per second.
// If hz <= 0, setcpuprofilerate turns off CPU profiling.
func setcpuprofilerate(hz int32) {
	// Force sane arguments.
	if hz < 0 {
		hz = 0
	}

	// Disable preemption, otherwise we can be rescheduled to another thread
	// that has profiling enabled.
	gp := getg()
	gp.m.locks++

	// Stop profiler on this thread so that it is safe to lock prof.
	// if a profiling signal came in while we had prof locked,
	// it would deadlock.
	setThreadCPUProfiler(0)

	for !prof.signalLock.CompareAndSwap(0, 1) {
		osyield()
	}
	if prof.hz.Load() != hz {
		setProcessCPUProfiler(hz)
		prof.hz.Store(hz)
	}
	prof.signalLock.Store(0)

	lock(&sched.lock)
	sched.profilehz = hz
	unlock(&sched.lock)

	if hz != 0 {
		setThreadCPUProfiler(hz)
	}

	gp.m.locks--
}

// init initializes pp, which may be a freshly allocated p or a
// previously destroyed p, and transitions it to status _Pgcstop.
// init初始化pp，pp可能是一个新分配的p或者之前被销毁的p，并将其状态转换为_Pgcstop
func (pp *p) init(id int32) {
	pp.id = id
	pp.status = _Pgcstop
	pp.sudogcache = pp.sudogbuf[:0]
	pp.deferpool = pp.deferpoolbuf[:0]
	pp.wbBuf.reset()
	if pp.mcache == nil {
		if id == 0 {
			if mcache0 == nil {
				throw("missing mcache?")
			}
			// Use the bootstrap mcache0. Only one P will get
			// mcache0: the one with ID 0.
			// 使用引导阶段的mcache0。只有一个P会获得mcache0：ID为0的那个P
			pp.mcache = mcache0
		} else {
			pp.mcache = allocmcache()
		}
	}
	if raceenabled && pp.raceprocctx == 0 {
		if id == 0 {
			pp.raceprocctx = raceprocctx0
			raceprocctx0 = 0 // bootstrap
		} else {
			pp.raceprocctx = raceproccreate()
		}
	}
	lockInit(&pp.timers.mu, lockRankTimers)

	// This P may get timers when it starts running. Set the mask here
	// since the P may not go through pidleget (notably P 0 on startup).
	// 当P开始运行时可能会获得定时器。在这里设置掩码，
	// 因为P可能不会经过pidleget（特别是启动时的P 0）
	timerpMask.set(id)
	// Similarly, we may not go through pidleget before this P starts
	// running if it is P 0 on startup.
	// 类似地，如果这是启动时的P 0，在P开始运行之前可能不会经过pidleget
	idlepMask.clear(id)
}

// destroy releases all of the resources associated with pp and
// transitions it to status _Pdead.
//
// sched.lock must be held and the world must be stopped.
// destroy释放与pp关联的所有资源，并将其状态转换为_Pdead
//
// 调用此函数时必须持有sched.lock锁，且世界必须处于停止状态
func (pp *p) destroy() {
	assertLockHeld(&sched.lock)
	assertWorldStopped()

	// Move all runnable goroutines to the global queue
	// 将所有可运行的goroutine移动到全局队列
	for pp.runqhead != pp.runqtail {
		// Pop from tail of local queue
		// 从本地队列尾部弹出
		pp.runqtail--
		gp := pp.runq[pp.runqtail%uint32(len(pp.runq))].ptr()
		// Push onto head of global queue
		// 将goroutine推入全局队列头部
		globrunqputhead(gp)
	}
	if pp.runnext != 0 {
		// 如果存在下一个要运行的goroutine，也将其移动到全局队列
		globrunqputhead(pp.runnext.ptr())
		pp.runnext = 0
	}

	// Move all timers to the local P.
	// 将所有定时器移动到本地P
	getg().m.p.ptr().timers.take(&pp.timers)

	// Flush p's write barrier buffer.
	// 刷新P的写屏障缓冲区
	if gcphase != _GCoff {
		wbBufFlush1(pp)
		pp.gcw.dispose()
	}

	// Clear sudog buffer and cache
	// 清空sudog缓冲区和缓存
	for i := range pp.sudogbuf {
		pp.sudogbuf[i] = nil
	}
	pp.sudogcache = pp.sudogbuf[:0]
	pp.pinnerCache = nil

	// Clear defer pool buffer and cache
	// 清空defer池缓冲区和缓存
	for j := range pp.deferpoolbuf {
		pp.deferpoolbuf[j] = nil
	}
	pp.deferpool = pp.deferpoolbuf[:0]

	// Clean up span cache and page cache
	// 清理span缓存和页缓存
	systemstack(func() {
		for i := 0; i < pp.mspancache.len; i++ {
			// Safe to call since the world is stopped.
			// 由于世界已停止，可以安全调用
			mheap_.spanalloc.free(unsafe.Pointer(pp.mspancache.buf[i]))
		}
		pp.mspancache.len = 0
		lock(&mheap_.lock)
		pp.pcache.flush(&mheap_.pages)
		unlock(&mheap_.lock)
	})

	// Free mcache and clear related fields
	// 释放mcache并清除相关字段
	freemcache(pp.mcache)
	pp.mcache = nil
	gfpurge(pp)
	if raceenabled {
		if pp.timers.raceCtx != 0 {
			// The race detector code uses a callback to fetch
			// the proc context, so arrange for that callback
			// to see the right thing.
			// This hack only works because we are the only
			// thread running.
			// 竞态检测器代码使用回调来获取proc上下文，所以需要安排这个回调
			// 能看到正确的内容。
			// 这个hack之所以能工作是因为我们是唯一运行的线程。
			mp := getg().m
			phold := mp.p.ptr()
			mp.p.set(pp)

			racectxend(pp.timers.raceCtx)
			pp.timers.raceCtx = 0

			mp.p.set(phold)
		}
		raceprocdestroy(pp.raceprocctx)
		pp.raceprocctx = 0
	}
	pp.gcAssistTime = 0
	pp.status = _Pdead
}

// Change number of processors.
// 更改处理器数量
//
// sched.lock must be held, and the world must be stopped.
// 必须持有sched.lock锁，且世界必须处于停止状态
//
// gcworkbufs must not be being modified by either the GC or the write barrier
// code, so the GC must not be running if the number of Ps actually changes.
// gcworkbufs不能被GC或写屏障代码修改，因此如果P的数量实际发生变化，GC不能运行
//
// Returns list of Ps with local work, they need to be scheduled by the caller.
// 返回具有本地工作的P列表，它们需要由调用者调度
func procresize(nprocs int32) *p {
	// 确保持有调度器锁
	assertLockHeld(&sched.lock)
	// 确保世界处于停止状态
	assertWorldStopped()

	// 获取当前的处理器数量
	old := gomaxprocs
	// 检查参数有效性
	if old < 0 || nprocs <= 0 {
		throw("procresize: invalid arg")
	}
	// 获取跟踪器实例
	trace := traceAcquire()
	if trace.ok() {
		// 记录处理器数量变化事件
		trace.Gomaxprocs(nprocs)
		traceRelease(trace)
	}

	// update statistics
	// 更新统计信息
	now := nanotime()
	if sched.procresizetime != 0 {
		// 如果之前有记录处理器调整时间，计算总运行时间
		// 总时间 = 旧处理器数量 * (当前时间 - 上次调整时间)
		sched.totaltime += int64(old) * (now - sched.procresizetime)
	}
	// 更新处理器调整时间为当前时间
	sched.procresizetime = now

	// 计算需要的掩码位数
	// 每个uint32可以表示32个处理器，所以需要(nprocs + 31) / 32个uint32
	maskWords := (nprocs + 31) / 32

	// Grow allp if necessary.
	// 如果需要，扩展allp数组
	if nprocs > int32(len(allp)) {
		// Synchronize with retake, which could be running
		// concurrently since it doesn't run on a P.
		// 与retake同步，因为它不在P上运行，所以可能并发执行
		lock(&allpLock)
		if nprocs <= int32(cap(allp)) {
			// 如果新处理器数量不超过allp的容量，直接调整长度
			allp = allp[:nprocs]
		} else {
			// 如果新处理器数量超过allp的容量，需要创建新的切片
			nallp := make([]*p, nprocs)
			// Copy everything up to allp's cap so we
			// never lose old allocated Ps.
			// 复制allp中所有已分配的P，确保不会丢失已分配的P
			copy(nallp, allp[:cap(allp)])
			allp = nallp
		}

		if maskWords <= int32(cap(idlepMask)) {
			// 如果新的掩码位数不超过现有掩码的容量，直接调整长度
			idlepMask = idlepMask[:maskWords]
			timerpMask = timerpMask[:maskWords]
		} else {
			// 如果新的掩码位数超过现有掩码的容量，需要创建新的掩码数组
			nidlepMask := make([]uint32, maskWords)
			// No need to copy beyond len, old Ps are irrelevant.
			// 不需要复制超过len的部分，旧的P已经无关紧要
			copy(nidlepMask, idlepMask)
			idlepMask = nidlepMask

			ntimerpMask := make([]uint32, maskWords)
			copy(ntimerpMask, timerpMask)
			timerpMask = ntimerpMask
		}
		unlock(&allpLock)
	}

	// initialize new P's
	// 初始化新的处理器(P)
	for i := old; i < nprocs; i++ {
		pp := allp[i]
		if pp == nil {
			// 如果该位置的P为空，则创建一个新的P
			pp = new(p)
		}
		// 初始化P，设置其ID为i
		pp.init(i)
		// 使用原子操作将新创建的P存储到allp数组中
		// 这样可以确保并发安全
		atomicstorep(unsafe.Pointer(&allp[i]), unsafe.Pointer(pp))
	}

	gp := getg()
	if gp.m.p != 0 && gp.m.p.ptr().id < nprocs {
		// continue to use the current P
		// 继续使用当前的P
		gp.m.p.ptr().status = _Prunning
		gp.m.p.ptr().mcache.prepareForSweep()
	} else {
		// release the current P and acquire allp[0].
		// 释放当前的P并获取allp[0]
		//
		// We must do this before destroying our current P
		// because p.destroy itself has write barriers, so we
		// need to do that from a valid P.
		// 我们必须在销毁当前P之前执行此操作，
		// 因为p.destroy本身包含写屏障，所以我们需要在一个有效的P上执行此操作
		if gp.m.p != 0 {
			trace := traceAcquire()
			if trace.ok() {
				// Pretend that we were descheduled
				// and then scheduled again to keep
				// the trace consistent.
				// 假装我们被取消调度然后重新调度，
				// 以保持跟踪的一致性
				trace.GoSched()
				trace.ProcStop(gp.m.p.ptr())
				traceRelease(trace)
			}
			gp.m.p.ptr().m = 0
		}
		gp.m.p = 0
		pp := allp[0]
		pp.m = 0
		pp.status = _Pidle
		acquirep(pp)
		trace := traceAcquire()
		if trace.ok() {
			trace.GoStart()
			traceRelease(trace)
		}
	}

	// g.m.p is now set, so we no longer need mcache0 for bootstrapping.
	// g.m.p现在已经设置好了，所以我们不再需要mcache0用于引导过程
	mcache0 = nil

	// release resources from unused P's
	// 释放未使用的P的资源
	for i := nprocs; i < old; i++ {
		pp := allp[i]
		pp.destroy()
		// can't free P itself because it can be referenced by an M in syscall
		// 不能释放P本身，因为它可能被处于系统调用中的M引用
	}

	// Trim allp.
	// 裁剪allp数组
	if int32(len(allp)) != nprocs {
		lock(&allpLock)
		allp = allp[:nprocs]                // 将allp数组裁剪到nprocs长度
		idlepMask = idlepMask[:maskWords]   // 裁剪空闲P的掩码
		timerpMask = timerpMask[:maskWords] // 裁剪定时器P的掩码
		unlock(&allpLock)
	}

	// 创建一个指向可运行P的指针
	var runnablePs *p
	// 从后向前遍历所有P
	for i := nprocs - 1; i >= 0; i-- {
		pp := allp[i]
		// 跳过当前M正在使用的P
		if gp.m.p.ptr() == pp {
			continue
		}
		// 将P的状态设置为空闲
		pp.status = _Pidle
		if runqempty(pp) {
			// 如果P的运行队列为空，将其放入空闲P列表
			pidleput(pp, now)
		} else {
			// 如果P的运行队列不为空，为其分配一个M
			pp.m.set(mget())
			// 将P链接到可运行P链表中
			pp.link.set(runnablePs)
			runnablePs = pp
		}
	}
	// 重置工作窃取顺序
	stealOrder.reset(uint32(nprocs))
	// 创建一个指向gomaxprocs的指针，用于编译器检查gomaxprocs是否为int32类型
	var int32p *int32 = &gomaxprocs // make compiler check that gomaxprocs is an int32
	// 原子地更新gomaxprocs的值
	atomic.Store((*uint32)(unsafe.Pointer(int32p)), uint32(nprocs))
	if old != nprocs {
		// Notify the limiter that the amount of procs has changed.
		// 通知限制器处理器数量已发生变化
		gcCPULimiter.resetCapacity(now, nprocs)
	}
	// 返回可运行的P链表
	return runnablePs
}

// Associate p and the current m.
// 将P与当前M关联起来
//
// This function is allowed to have write barriers even if the caller
// isn't because it immediately acquires pp.
// 即使调用者不允许写屏障，此函数也允许写屏障，因为它立即获取了pp
//
//go:yeswritebarrierrec
func acquirep(pp *p) {
	// Do the part that isn't allowed to have write barriers.
	// 执行不允许写屏障的部分
	wirep(pp)

	// Have p; write barriers now allowed.
	// 已经获取了P，现在允许写屏障

	// Perform deferred mcache flush before this P can allocate
	// from a potentially stale mcache.
	// 在P可以从可能过时的mcache分配内存之前，执行延迟的mcache刷新
	pp.mcache.prepareForSweep()

	trace := traceAcquire()
	if trace.ok() {
		trace.ProcStart()
		traceRelease(trace)
	}
}

// wirep is the first step of acquirep, which actually associates the
// current M to pp. This is broken out so we can disallow write
// barriers for this part, since we don't yet have a P.
// wirep是acquirep的第一步，它实际上将当前M与pp关联起来。
// 这个函数被单独分离出来是为了在这个阶段禁用写屏障，因为此时我们还没有P。
//
//go:nowritebarrierrec
//go:nosplit
func wirep(pp *p) {
	gp := getg()

	if gp.m.p != 0 {
		// Call on the systemstack to avoid a nosplit overflow build failure
		// on some platforms when built with -N -l. See #64113.
		// 在系统栈上调用以避免在使用-N -l构建时在某些平台上出现nosplit溢出构建失败。
		// 参见 #64113。
		systemstack(func() {
			throw("wirep: already in go")
		})
	}
	if pp.m != 0 || pp.status != _Pidle {
		// Call on the systemstack to avoid a nosplit overflow build failure
		// on some platforms when built with -N -l. See #64113.
		// 在系统栈上调用以避免在使用-N -l构建时在某些平台上出现nosplit溢出构建失败。
		// 参见 #64113。
		systemstack(func() {
			id := int64(0)
			if pp.m != 0 {
				id = pp.m.ptr().id
			}
			print("wirep: p->m=", pp.m, "(", id, ") p->status=", pp.status, "\n")
			throw("wirep: invalid p state")
		})
	}
	// 将P与当前M关联
	gp.m.p.set(pp)
	// 将M与P关联
	pp.m.set(gp.m)
	// 设置P的状态为运行中
	pp.status = _Prunning
}

// Disassociate p and the current m.
// 解除当前M与P的关联
func releasep() *p {
	// 获取追踪器
	trace := traceAcquire()
	if trace.ok() {
		// 记录P停止的追踪事件
		trace.ProcStop(getg().m.p.ptr())
		// 释放追踪器
		traceRelease(trace)
	}
	// 调用不带追踪的releasep实现
	return releasepNoTrace()
}

// Disassociate p and the current m without tracing an event.
// 解除当前M与P的关联，但不记录追踪事件
func releasepNoTrace() *p {
	gp := getg()

	// 检查当前M是否关联了P
	// Check if current M has an associated P
	if gp.m.p == 0 {
		throw("releasep: invalid arg")
	}
	pp := gp.m.p.ptr()
	// 验证P的状态是否正确
	// Verify P's state is correct
	if pp.m.ptr() != gp.m || pp.status != _Prunning {
		print("releasep: m=", gp.m, " m->p=", gp.m.p.ptr(), " p->m=", hex(pp.m), " p->status=", pp.status, "\n")
		throw("releasep: invalid p state")
	}
	// 解除M与P的关联
	// Disassociate M from P
	gp.m.p = 0
	// 解除P与M的关联
	// Disassociate P from M
	pp.m = 0
	// 将P的状态设置为空闲
	// Set P's status to idle
	pp.status = _Pidle
	return pp
}

func incidlelocked(v int32) {
	lock(&sched.lock)
	sched.nmidlelocked += v
	if v > 0 {
		checkdead()
	}
	unlock(&sched.lock)
}

// Check for deadlock situation.
// The check is based on number of running M's, if 0 -> deadlock.
// sched.lock must be held.
func checkdead() {
	assertLockHeld(&sched.lock)

	// For -buildmode=c-shared or -buildmode=c-archive it's OK if
	// there are no running goroutines. The calling program is
	// assumed to be running.
	if islibrary || isarchive {
		return
	}

	// If we are dying because of a signal caught on an already idle thread,
	// freezetheworld will cause all running threads to block.
	// And runtime will essentially enter into deadlock state,
	// except that there is a thread that will call exit soon.
	if panicking.Load() > 0 {
		return
	}

	// If we are not running under cgo, but we have an extra M then account
	// for it. (It is possible to have an extra M on Windows without cgo to
	// accommodate callbacks created by syscall.NewCallback. See issue #6751
	// for details.)
	var run0 int32
	if !iscgo && cgoHasExtraM && extraMLength.Load() > 0 {
		run0 = 1
	}

	run := mcount() - sched.nmidle - sched.nmidlelocked - sched.nmsys
	if run > run0 {
		return
	}
	if run < 0 {
		print("runtime: checkdead: nmidle=", sched.nmidle, " nmidlelocked=", sched.nmidlelocked, " mcount=", mcount(), " nmsys=", sched.nmsys, "\n")
		unlock(&sched.lock)
		throw("checkdead: inconsistent counts")
	}

	grunning := 0
	forEachG(func(gp *g) {
		if isSystemGoroutine(gp, false) {
			return
		}
		s := readgstatus(gp)
		switch s &^ _Gscan {
		case _Gwaiting,
			_Gpreempted:
			grunning++
		case _Grunnable,
			_Grunning,
			_Gsyscall:
			print("runtime: checkdead: find g ", gp.goid, " in status ", s, "\n")
			unlock(&sched.lock)
			throw("checkdead: runnable g")
		}
	})
	if grunning == 0 { // possible if main goroutine calls runtime·Goexit()
		unlock(&sched.lock) // unlock so that GODEBUG=scheddetail=1 doesn't hang
		fatal("no goroutines (main called runtime.Goexit) - deadlock!")
	}

	// Maybe jump time forward for playground.
	if faketime != 0 {
		if when := timeSleepUntil(); when < maxWhen {
			faketime = when

			// Start an M to steal the timer.
			pp, _ := pidleget(faketime)
			if pp == nil {
				// There should always be a free P since
				// nothing is running.
				unlock(&sched.lock)
				throw("checkdead: no p for timer")
			}
			mp := mget()
			if mp == nil {
				// There should always be a free M since
				// nothing is running.
				unlock(&sched.lock)
				throw("checkdead: no m for timer")
			}
			// M must be spinning to steal. We set this to be
			// explicit, but since this is the only M it would
			// become spinning on its own anyways.
			sched.nmspinning.Add(1)
			mp.spinning = true
			mp.nextp.set(pp)
			notewakeup(&mp.park)
			return
		}
	}

	// There are no goroutines running, so we can look at the P's.
	for _, pp := range allp {
		if len(pp.timers.heap) > 0 {
			return
		}
	}

	unlock(&sched.lock) // unlock so that GODEBUG=scheddetail=1 doesn't hang
	fatal("all goroutines are asleep - deadlock!")
}

// forcegcperiod is the maximum time in nanoseconds between garbage
// collections. If we go this long without a garbage collection, one
// is forced to run.
//
// This is a variable for testing purposes. It normally doesn't change.
var forcegcperiod int64 = 2 * 60 * 1e9

// needSysmonWorkaround is true if the workaround for
// golang.org/issue/42515 is needed on NetBSD.
var needSysmonWorkaround bool = false

// haveSysmon indicates whether there is sysmon thread support.
//
// No threads on wasm yet, so no sysmon.
const haveSysmon = GOARCH != "wasm"

// Always runs without a P, so write barriers are not allowed.
//
//go:nowritebarrierrec
func sysmon() {
	lock(&sched.lock)
	sched.nmsys++
	checkdead()
	unlock(&sched.lock)

	lasttrace := int64(0)
	idle := 0 // how many cycles in succession we had not wokeup somebody
	delay := uint32(0)

	for {
		if idle == 0 { // start with 20us sleep...
			delay = 20
		} else if idle > 50 { // start doubling the sleep after 1ms...
			delay *= 2
		}
		if delay > 10*1000 { // up to 10ms
			delay = 10 * 1000
		}
		usleep(delay)

		// sysmon should not enter deep sleep if schedtrace is enabled so that
		// it can print that information at the right time.
		//
		// It should also not enter deep sleep if there are any active P's so
		// that it can retake P's from syscalls, preempt long running G's, and
		// poll the network if all P's are busy for long stretches.
		//
		// It should wakeup from deep sleep if any P's become active either due
		// to exiting a syscall or waking up due to a timer expiring so that it
		// can resume performing those duties. If it wakes from a syscall it
		// resets idle and delay as a bet that since it had retaken a P from a
		// syscall before, it may need to do it again shortly after the
		// application starts work again. It does not reset idle when waking
		// from a timer to avoid adding system load to applications that spend
		// most of their time sleeping.
		now := nanotime()
		if debug.schedtrace <= 0 && (sched.gcwaiting.Load() || sched.npidle.Load() == gomaxprocs) {
			lock(&sched.lock)
			if sched.gcwaiting.Load() || sched.npidle.Load() == gomaxprocs {
				syscallWake := false
				next := timeSleepUntil()
				if next > now {
					sched.sysmonwait.Store(true)
					unlock(&sched.lock)
					// Make wake-up period small enough
					// for the sampling to be correct.
					sleep := forcegcperiod / 2
					if next-now < sleep {
						sleep = next - now
					}
					shouldRelax := sleep >= osRelaxMinNS
					if shouldRelax {
						osRelax(true)
					}
					syscallWake = notetsleep(&sched.sysmonnote, sleep)
					if shouldRelax {
						osRelax(false)
					}
					lock(&sched.lock)
					sched.sysmonwait.Store(false)
					noteclear(&sched.sysmonnote)
				}
				if syscallWake {
					idle = 0
					delay = 20
				}
			}
			unlock(&sched.lock)
		}

		lock(&sched.sysmonlock)
		// Update now in case we blocked on sysmonnote or spent a long time
		// blocked on schedlock or sysmonlock above.
		now = nanotime()

		// trigger libc interceptors if needed
		if *cgo_yield != nil {
			asmcgocall(*cgo_yield, nil)
		}
		// poll network if not polled for more than 10ms
		lastpoll := sched.lastpoll.Load()
		if netpollinited() && lastpoll != 0 && lastpoll+10*1000*1000 < now {
			sched.lastpoll.CompareAndSwap(lastpoll, now)
			list, delta := netpoll(0) // non-blocking - returns list of goroutines
			if !list.empty() {
				// Need to decrement number of idle locked M's
				// (pretending that one more is running) before injectglist.
				// Otherwise it can lead to the following situation:
				// injectglist grabs all P's but before it starts M's to run the P's,
				// another M returns from syscall, finishes running its G,
				// observes that there is no work to do and no other running M's
				// and reports deadlock.
				incidlelocked(-1)
				injectglist(&list)
				incidlelocked(1)
				netpollAdjustWaiters(delta)
			}
		}
		if GOOS == "netbsd" && needSysmonWorkaround {
			// netpoll is responsible for waiting for timer
			// expiration, so we typically don't have to worry
			// about starting an M to service timers. (Note that
			// sleep for timeSleepUntil above simply ensures sysmon
			// starts running again when that timer expiration may
			// cause Go code to run again).
			//
			// However, netbsd has a kernel bug that sometimes
			// misses netpollBreak wake-ups, which can lead to
			// unbounded delays servicing timers. If we detect this
			// overrun, then startm to get something to handle the
			// timer.
			//
			// See issue 42515 and
			// https://gnats.netbsd.org/cgi-bin/query-pr-single.pl?number=50094.
			if next := timeSleepUntil(); next < now {
				startm(nil, false, false)
			}
		}
		if scavenger.sysmonWake.Load() != 0 {
			// Kick the scavenger awake if someone requested it.
			scavenger.wake()
		}
		// retake P's blocked in syscalls
		// and preempt long running G's
		if retake(now) != 0 {
			idle = 0
		} else {
			idle++
		}
		// check if we need to force a GC
		if t := (gcTrigger{kind: gcTriggerTime, now: now}); t.test() && forcegc.idle.Load() {
			lock(&forcegc.lock)
			forcegc.idle.Store(false)
			var list gList
			list.push(forcegc.g)
			injectglist(&list)
			unlock(&forcegc.lock)
		}
		if debug.schedtrace > 0 && lasttrace+int64(debug.schedtrace)*1000000 <= now {
			lasttrace = now
			schedtrace(debug.scheddetail > 0)
		}
		unlock(&sched.sysmonlock)
	}
}

type sysmontick struct {
	schedtick   uint32
	syscalltick uint32
	schedwhen   int64
	syscallwhen int64
}

// forcePreemptNS is the time slice given to a G before it is
// preempted.
const forcePreemptNS = 10 * 1000 * 1000 // 10ms

func retake(now int64) uint32 {
	n := 0
	// Prevent allp slice changes. This lock will be completely
	// uncontended unless we're already stopping the world.
	lock(&allpLock)
	// We can't use a range loop over allp because we may
	// temporarily drop the allpLock. Hence, we need to re-fetch
	// allp each time around the loop.
	for i := 0; i < len(allp); i++ {
		pp := allp[i]
		if pp == nil {
			// This can happen if procresize has grown
			// allp but not yet created new Ps.
			continue
		}
		pd := &pp.sysmontick
		s := pp.status
		sysretake := false
		if s == _Prunning || s == _Psyscall {
			// Preempt G if it's running on the same schedtick for
			// too long. This could be from a single long-running
			// goroutine or a sequence of goroutines run via
			// runnext, which share a single schedtick time slice.
			t := int64(pp.schedtick)
			if int64(pd.schedtick) != t {
				pd.schedtick = uint32(t)
				pd.schedwhen = now
			} else if pd.schedwhen+forcePreemptNS <= now {
				preemptone(pp)
				// In case of syscall, preemptone() doesn't
				// work, because there is no M wired to P.
				sysretake = true
			}
		}
		if s == _Psyscall {
			// Retake P from syscall if it's there for more than 1 sysmon tick (at least 20us).
			t := int64(pp.syscalltick)
			if !sysretake && int64(pd.syscalltick) != t {
				pd.syscalltick = uint32(t)
				pd.syscallwhen = now
				continue
			}
			// On the one hand we don't want to retake Ps if there is no other work to do,
			// but on the other hand we want to retake them eventually
			// because they can prevent the sysmon thread from deep sleep.
			if runqempty(pp) && sched.nmspinning.Load()+sched.npidle.Load() > 0 && pd.syscallwhen+10*1000*1000 > now {
				continue
			}
			// Drop allpLock so we can take sched.lock.
			unlock(&allpLock)
			// Need to decrement number of idle locked M's
			// (pretending that one more is running) before the CAS.
			// Otherwise the M from which we retake can exit the syscall,
			// increment nmidle and report deadlock.
			incidlelocked(-1)
			trace := traceAcquire()
			if atomic.Cas(&pp.status, s, _Pidle) {
				if trace.ok() {
					trace.ProcSteal(pp, false)
					traceRelease(trace)
				}
				n++
				pp.syscalltick++
				handoffp(pp)
			} else if trace.ok() {
				traceRelease(trace)
			}
			incidlelocked(1)
			lock(&allpLock)
		}
	}
	unlock(&allpLock)
	return uint32(n)
}

// Tell all goroutines that they have been preempted and they should stop.
// This function is purely best-effort. It can fail to inform a goroutine if a
// processor just started running it.
// No locks need to be held.
// Returns true if preemption request was issued to at least one goroutine.
func preemptall() bool {
	res := false
	for _, pp := range allp {
		if pp.status != _Prunning {
			continue
		}
		if preemptone(pp) {
			res = true
		}
	}
	return res
}

// Tell the goroutine running on processor P to stop.
// This function is purely best-effort. It can incorrectly fail to inform the
// goroutine. It can inform the wrong goroutine. Even if it informs the
// correct goroutine, that goroutine might ignore the request if it is
// simultaneously executing newstack.
// No lock needs to be held.
// Returns true if preemption request was issued.
// The actual preemption will happen at some point in the future
// and will be indicated by the gp->status no longer being
// Grunning
func preemptone(pp *p) bool {
	mp := pp.m.ptr()
	if mp == nil || mp == getg().m {
		return false
	}
	gp := mp.curg
	if gp == nil || gp == mp.g0 {
		return false
	}

	gp.preempt = true

	// Every call in a goroutine checks for stack overflow by
	// comparing the current stack pointer to gp->stackguard0.
	// Setting gp->stackguard0 to StackPreempt folds
	// preemption into the normal stack overflow check.
	gp.stackguard0 = stackPreempt

	// Request an async preemption of this P.
	if preemptMSupported && debug.asyncpreemptoff == 0 {
		pp.preempt = true
		preemptM(mp)
	}

	return true
}

var starttime int64

func schedtrace(detailed bool) {
	now := nanotime()
	if starttime == 0 {
		starttime = now
	}

	lock(&sched.lock)
	print("SCHED ", (now-starttime)/1e6, "ms: gomaxprocs=", gomaxprocs, " idleprocs=", sched.npidle.Load(), " threads=", mcount(), " spinningthreads=", sched.nmspinning.Load(), " needspinning=", sched.needspinning.Load(), " idlethreads=", sched.nmidle, " runqueue=", sched.runqsize)
	if detailed {
		print(" gcwaiting=", sched.gcwaiting.Load(), " nmidlelocked=", sched.nmidlelocked, " stopwait=", sched.stopwait, " sysmonwait=", sched.sysmonwait.Load(), "\n")
	}
	// We must be careful while reading data from P's, M's and G's.
	// Even if we hold schedlock, most data can be changed concurrently.
	// E.g. (p->m ? p->m->id : -1) can crash if p->m changes from non-nil to nil.
	for i, pp := range allp {
		mp := pp.m.ptr()
		h := atomic.Load(&pp.runqhead)
		t := atomic.Load(&pp.runqtail)
		if detailed {
			print("  P", i, ": status=", pp.status, " schedtick=", pp.schedtick, " syscalltick=", pp.syscalltick, " m=")
			if mp != nil {
				print(mp.id)
			} else {
				print("nil")
			}
			print(" runqsize=", t-h, " gfreecnt=", pp.gFree.n, " timerslen=", len(pp.timers.heap), "\n")
		} else {
			// In non-detailed mode format lengths of per-P run queues as:
			// [len1 len2 len3 len4]
			print(" ")
			if i == 0 {
				print("[")
			}
			print(t - h)
			if i == len(allp)-1 {
				print("]\n")
			}
		}
	}

	if !detailed {
		unlock(&sched.lock)
		return
	}

	for mp := allm; mp != nil; mp = mp.alllink {
		pp := mp.p.ptr()
		print("  M", mp.id, ": p=")
		if pp != nil {
			print(pp.id)
		} else {
			print("nil")
		}
		print(" curg=")
		if mp.curg != nil {
			print(mp.curg.goid)
		} else {
			print("nil")
		}
		print(" mallocing=", mp.mallocing, " throwing=", mp.throwing, " preemptoff=", mp.preemptoff, " locks=", mp.locks, " dying=", mp.dying, " spinning=", mp.spinning, " blocked=", mp.blocked, " lockedg=")
		if lockedg := mp.lockedg.ptr(); lockedg != nil {
			print(lockedg.goid)
		} else {
			print("nil")
		}
		print("\n")
	}

	forEachG(func(gp *g) {
		print("  G", gp.goid, ": status=", readgstatus(gp), "(", gp.waitreason.String(), ") m=")
		if gp.m != nil {
			print(gp.m.id)
		} else {
			print("nil")
		}
		print(" lockedm=")
		if lockedm := gp.lockedm.ptr(); lockedm != nil {
			print(lockedm.id)
		} else {
			print("nil")
		}
		print("\n")
	})
	unlock(&sched.lock)
}

// schedEnableUser enables or disables the scheduling of user
// goroutines.
//
// This does not stop already running user goroutines, so the caller
// should first stop the world when disabling user goroutines.
func schedEnableUser(enable bool) {
	lock(&sched.lock)
	if sched.disable.user == !enable {
		unlock(&sched.lock)
		return
	}
	sched.disable.user = !enable
	if enable {
		n := sched.disable.n
		sched.disable.n = 0
		globrunqputbatch(&sched.disable.runnable, n)
		unlock(&sched.lock)
		for ; n != 0 && sched.npidle.Load() != 0; n-- {
			startm(nil, false, false)
		}
	} else {
		unlock(&sched.lock)
	}
}

// schedEnabled reports whether gp should be scheduled. It returns
// false is scheduling of gp is disabled.
//
// sched.lock must be held.
func schedEnabled(gp *g) bool {
	assertLockHeld(&sched.lock)

	if sched.disable.user {
		return isSystemGoroutine(gp, true)
	}
	return true
}

// Put mp on midle list.
// sched.lock must be held.
// May run during STW, so write barriers are not allowed.
//
//go:nowritebarrierrec
func mput(mp *m) {
	assertLockHeld(&sched.lock)

	mp.schedlink = sched.midle
	sched.midle.set(mp)
	sched.nmidle++
	checkdead()
}

// Try to get an m from midle list.
// sched.lock must be held.
// May run during STW, so write barriers are not allowed.
//
// 尝试从空闲M列表中获取一个M
// 调用者必须持有调度器锁
// 可能在STW期间运行，所以不允许写屏障
//
//go:nowritebarrierrec
func mget() *m {
	assertLockHeld(&sched.lock)

	mp := sched.midle.ptr() // 获取空闲M列表中的第一个M
	if mp != nil {
		sched.midle = mp.schedlink // 更新空闲M列表头为下一个M
		sched.nmidle--             // 空闲M计数减1
	}
	return mp // 返回获取到的M，如果没有则返回nil
}

// Put gp on the global runnable queue.
// sched.lock must be held.
// May run during STW, so write barriers are not allowed.
//
//go:nowritebarrierrec
func globrunqput(gp *g) {
	assertLockHeld(&sched.lock)

	sched.runq.pushBack(gp)
	sched.runqsize++
}

// Put gp at the head of the global runnable queue.
// sched.lock must be held.
// May run during STW, so write barriers are not allowed.
//
//go:nowritebarrierrec
func globrunqputhead(gp *g) {
	assertLockHeld(&sched.lock)

	sched.runq.push(gp)
	sched.runqsize++
}

// Put a batch of runnable goroutines on the global runnable queue.
// This clears *batch.
// sched.lock must be held.
// May run during STW, so write barriers are not allowed.
//
//go:nowritebarrierrec
func globrunqputbatch(batch *gQueue, n int32) {
	assertLockHeld(&sched.lock)

	sched.runq.pushBackAll(*batch)
	sched.runqsize += n
	*batch = gQueue{}
}

// Try get a batch of G's from the global runnable queue.
// sched.lock must be held.
func globrunqget(pp *p, max int32) *g {
	assertLockHeld(&sched.lock)

	if sched.runqsize == 0 {
		return nil
	}

	n := sched.runqsize/gomaxprocs + 1
	if n > sched.runqsize {
		n = sched.runqsize
	}
	if max > 0 && n > max {
		n = max
	}
	if n > int32(len(pp.runq))/2 {
		n = int32(len(pp.runq)) / 2
	}

	sched.runqsize -= n

	gp := sched.runq.pop()
	n--
	for ; n > 0; n-- {
		gp1 := sched.runq.pop()
		runqput(pp, gp1, false)
	}
	return gp
}

// pMask is an atomic bitstring with one bit per P.
type pMask []uint32

// read returns true if P id's bit is set.
func (p pMask) read(id uint32) bool {
	word := id / 32
	mask := uint32(1) << (id % 32)
	return (atomic.Load(&p[word]) & mask) != 0
}

// set sets P id's bit.
func (p pMask) set(id int32) {
	word := id / 32
	mask := uint32(1) << (id % 32)
	atomic.Or(&p[word], mask)
}

// clear clears P id's bit.
func (p pMask) clear(id int32) {
	word := id / 32
	mask := uint32(1) << (id % 32)
	atomic.And(&p[word], ^mask)
}

// pidleput puts p on the _Pidle list. now must be a relatively recent call
// to nanotime or zero. Returns now or the current time if now was zero.
//
// This releases ownership of p. Once sched.lock is released it is no longer
// safe to use p.
//
// sched.lock must be held.
//
// May run during STW, so write barriers are not allowed.
//
// pidleput将P放入空闲P列表中。now参数必须是最近一次调用nanotime()的结果或零。
// 如果now为零，则返回当前时间，否则返回now。
//
// 这个函数会释放对p的所有权。一旦sched.lock被释放，继续使用p就不再安全。
//
// 调用此函数时必须持有sched.lock锁。
//
// 此函数可能在STW(Stop The World)期间运行，因此不允许写屏障。
//
//go:nowritebarrierrec
func pidleput(pp *p, now int64) int64 {
	// 确保持有调度器锁
	assertLockHeld(&sched.lock)

	// 检查P的运行队列是否为空
	if !runqempty(pp) {
		throw("pidleput: P has non-empty run queue")
	}
	// 如果now为0，获取当前时间
	if now == 0 {
		now = nanotime()
	}
	// 如果P没有定时器，清除其定时器掩码
	if pp.timers.len.Load() == 0 {
		timerpMask.clear(pp.id)
	}
	// 设置P的空闲掩码
	idlepMask.set(pp.id)
	// 将P链接到空闲P链表
	pp.link = sched.pidle
	sched.pidle.set(pp)
	// 增加空闲P计数
	sched.npidle.Add(1)
	// 启动P的空闲限制器事件
	if !pp.limiterEvent.start(limiterEventIdle, now) {
		throw("must be able to track idle limiter event")
	}
	return now
}

// pidleget tries to get a p from the _Pidle list, acquiring ownership.
// pidleget尝试从空闲P列表中获取一个P，并获取其所有权
//
// sched.lock must be held.
// 必须持有sched.lock锁
//
// May run during STW, so write barriers are not allowed.
// 可能在STW(Stop The World)期间运行，因此不允许写屏障
//
//go:nowritebarrierrec
func pidleget(now int64) (*p, int64) {
	// 确保持有调度器锁
	assertLockHeld(&sched.lock)

	// 从空闲P链表中获取一个P
	pp := sched.pidle.ptr()
	if pp != nil {
		// Timer may get added at any time now.
		// 现在可能随时添加定时器
		if now == 0 {
			// 如果now为0，获取当前时间
			now = nanotime()
		}
		// 设置P的定时器掩码
		timerpMask.set(pp.id)
		// 清除P的空闲掩码
		idlepMask.clear(pp.id)
		// 更新空闲P链表头
		sched.pidle = pp.link
		// 减少空闲P计数
		sched.npidle.Add(-1)
		// 停止P的空闲限制器事件
		pp.limiterEvent.stop(limiterEventIdle, now)
	}
	// 返回获取到的P和当前时间
	return pp, now
}

// pidlegetSpinning tries to get a p from the _Pidle list, acquiring ownership.
// This is called by spinning Ms (or callers than need a spinning M) that have
// found work. If no P is available, this must synchronized with non-spinning
// Ms that may be preparing to drop their P without discovering this work.
//
// sched.lock must be held.
//
// May run during STW, so write barriers are not allowed.
//
//go:nowritebarrierrec
func pidlegetSpinning(now int64) (*p, int64) {
	assertLockHeld(&sched.lock)

	pp, now := pidleget(now)
	if pp == nil {
		// See "Delicate dance" comment in findrunnable. We found work
		// that we cannot take, we must synchronize with non-spinning
		// Ms that may be preparing to drop their P.
		sched.needspinning.Store(1)
		return nil, now
	}

	return pp, now
}

// runqempty reports whether pp has no Gs on its local run queue.
// It never returns true spuriously.
// runqempty 检查本地运行队列是否为空
// 它永远不会错误地返回true
func runqempty(pp *p) bool {
	// Defend against a race where 1) pp has G1 in runqnext but runqhead == runqtail,
	// 2) runqput on pp kicks G1 to the runq, 3) runqget on pp empties runqnext.
	// Simply observing that runqhead == runqtail and then observing that runqnext == nil
	// does not mean the queue is empty.
	// 防止以下竞态条件:
	// 1) pp的runqnext中有G1但runqhead等于runqtail
	// 2) pp上的runqput将G1踢到runq中
	// 3) pp上的runqget清空runqnext
	// 简单地观察到runqhead等于runqtail并且runqnext为nil并不意味着队列为空
	for {
		// 原子地加载队列头部位置
		head := atomic.Load(&pp.runqhead)
		// 原子地加载队列尾部位置
		tail := atomic.Load(&pp.runqtail)
		// 原子地加载runnext指针
		runnext := atomic.Loaduintptr((*uintptr)(unsafe.Pointer(&pp.runnext)))
		// 再次检查尾部位置是否发生变化
		if tail == atomic.Load(&pp.runqtail) {
			// 只有当队列头部等于尾部且runnext为空时，才认为队列为空
			return head == tail && runnext == 0
		}
	}
}

// To shake out latent assumptions about scheduling order,
// we introduce some randomness into scheduling decisions
// when running with the race detector.
// The need for this was made obvious by changing the
// (deterministic) scheduling order in Go 1.5 and breaking
// many poorly-written tests.
// With the randomness here, as long as the tests pass
// consistently with -race, they shouldn't have latent scheduling
// assumptions.
const randomizeScheduler = raceenabled

// runqput tries to put g on the local runnable queue.
// If next is false, runqput adds g to the tail of the runnable queue.
// If next is true, runqput puts g in the pp.runnext slot.
// If the run queue is full, runnext puts g on the global queue.
// Executed only by the owner P.
func runqput(pp *p, gp *g, next bool) {
	if !haveSysmon && next {
		// A runnext goroutine shares the same time slice as the
		// current goroutine (inheritTime from runqget). To prevent a
		// ping-pong pair of goroutines from starving all others, we
		// depend on sysmon to preempt "long-running goroutines". That
		// is, any set of goroutines sharing the same time slice.
		//
		// If there is no sysmon, we must avoid runnext entirely or
		// risk starvation.
		next = false
	}
	if randomizeScheduler && next && randn(2) == 0 {
		next = false
	}

	if next {
	retryNext:
		oldnext := pp.runnext
		if !pp.runnext.cas(oldnext, guintptr(unsafe.Pointer(gp))) {
			goto retryNext
		}
		if oldnext == 0 {
			return
		}
		// Kick the old runnext out to the regular run queue.
		gp = oldnext.ptr()
	}

retry:
	h := atomic.LoadAcq(&pp.runqhead) // load-acquire, synchronize with consumers
	t := pp.runqtail
	if t-h < uint32(len(pp.runq)) {
		pp.runq[t%uint32(len(pp.runq))].set(gp)
		atomic.StoreRel(&pp.runqtail, t+1) // store-release, makes the item available for consumption
		return
	}
	if runqputslow(pp, gp, h, t) {
		return
	}
	// the queue is not full, now the put above must succeed
	goto retry
}

// Put g and a batch of work from local runnable queue on global queue.
// Executed only by the owner P.
func runqputslow(pp *p, gp *g, h, t uint32) bool {
	var batch [len(pp.runq)/2 + 1]*g

	// First, grab a batch from local queue.
	n := t - h
	n = n / 2
	if n != uint32(len(pp.runq)/2) {
		throw("runqputslow: queue is not full")
	}
	for i := uint32(0); i < n; i++ {
		batch[i] = pp.runq[(h+i)%uint32(len(pp.runq))].ptr()
	}
	if !atomic.CasRel(&pp.runqhead, h, h+n) { // cas-release, commits consume
		return false
	}
	batch[n] = gp

	if randomizeScheduler {
		for i := uint32(1); i <= n; i++ {
			j := cheaprandn(i + 1)
			batch[i], batch[j] = batch[j], batch[i]
		}
	}

	// Link the goroutines.
	for i := uint32(0); i < n; i++ {
		batch[i].schedlink.set(batch[i+1])
	}
	var q gQueue
	q.head.set(batch[0])
	q.tail.set(batch[n])

	// Now put the batch on global queue.
	lock(&sched.lock)
	globrunqputbatch(&q, int32(n+1))
	unlock(&sched.lock)
	return true
}

// runqputbatch tries to put all the G's on q on the local runnable queue.
// If the queue is full, they are put on the global queue; in that case
// this will temporarily acquire the scheduler lock.
// Executed only by the owner P.
func runqputbatch(pp *p, q *gQueue, qsize int) {
	h := atomic.LoadAcq(&pp.runqhead)
	t := pp.runqtail
	n := uint32(0)
	for !q.empty() && t-h < uint32(len(pp.runq)) {
		gp := q.pop()
		pp.runq[t%uint32(len(pp.runq))].set(gp)
		t++
		n++
	}
	qsize -= int(n)

	if randomizeScheduler {
		off := func(o uint32) uint32 {
			return (pp.runqtail + o) % uint32(len(pp.runq))
		}
		for i := uint32(1); i < n; i++ {
			j := cheaprandn(i + 1)
			pp.runq[off(i)], pp.runq[off(j)] = pp.runq[off(j)], pp.runq[off(i)]
		}
	}

	atomic.StoreRel(&pp.runqtail, t)
	if !q.empty() {
		lock(&sched.lock)
		globrunqputbatch(q, int32(qsize))
		unlock(&sched.lock)
	}
}

// Get g from local runnable queue.
// If inheritTime is true, gp should inherit the remaining time in the
// current time slice. Otherwise, it should start a new time slice.
// Executed only by the owner P.
func runqget(pp *p) (gp *g, inheritTime bool) {
	// If there's a runnext, it's the next G to run.
	next := pp.runnext
	// If the runnext is non-0 and the CAS fails, it could only have been stolen by another P,
	// because other Ps can race to set runnext to 0, but only the current P can set it to non-0.
	// Hence, there's no need to retry this CAS if it fails.
	if next != 0 && pp.runnext.cas(next, 0) {
		return next.ptr(), true
	}

	for {
		h := atomic.LoadAcq(&pp.runqhead) // load-acquire, synchronize with other consumers
		t := pp.runqtail
		if t == h {
			return nil, false
		}
		gp := pp.runq[h%uint32(len(pp.runq))].ptr()
		if atomic.CasRel(&pp.runqhead, h, h+1) { // cas-release, commits consume
			return gp, false
		}
	}
}

// runqdrain drains the local runnable queue of pp and returns all goroutines in it.
// Executed only by the owner P.
func runqdrain(pp *p) (drainQ gQueue, n uint32) {
	oldNext := pp.runnext
	if oldNext != 0 && pp.runnext.cas(oldNext, 0) {
		drainQ.pushBack(oldNext.ptr())
		n++
	}

retry:
	h := atomic.LoadAcq(&pp.runqhead) // load-acquire, synchronize with other consumers
	t := pp.runqtail
	qn := t - h
	if qn == 0 {
		return
	}
	if qn > uint32(len(pp.runq)) { // read inconsistent h and t
		goto retry
	}

	if !atomic.CasRel(&pp.runqhead, h, h+qn) { // cas-release, commits consume
		goto retry
	}

	// We've inverted the order in which it gets G's from the local P's runnable queue
	// and then advances the head pointer because we don't want to mess up the statuses of G's
	// while runqdrain() and runqsteal() are running in parallel.
	// Thus we should advance the head pointer before draining the local P into a gQueue,
	// so that we can update any gp.schedlink only after we take the full ownership of G,
	// meanwhile, other P's can't access to all G's in local P's runnable queue and steal them.
	// See https://groups.google.com/g/golang-dev/c/0pTKxEKhHSc/m/6Q85QjdVBQAJ for more details.
	for i := uint32(0); i < qn; i++ {
		gp := pp.runq[(h+i)%uint32(len(pp.runq))].ptr()
		drainQ.pushBack(gp)
		n++
	}
	return
}

// Grabs a batch of goroutines from pp's runnable queue into batch.
// Batch is a ring buffer starting at batchHead.
// Returns number of grabbed goroutines.
// Can be executed by any P.
func runqgrab(pp *p, batch *[256]guintptr, batchHead uint32, stealRunNextG bool) uint32 {
	for {
		h := atomic.LoadAcq(&pp.runqhead) // load-acquire, synchronize with other consumers
		t := atomic.LoadAcq(&pp.runqtail) // load-acquire, synchronize with the producer
		n := t - h
		n = n - n/2
		if n == 0 {
			if stealRunNextG {
				// Try to steal from pp.runnext.
				if next := pp.runnext; next != 0 {
					if pp.status == _Prunning {
						// Sleep to ensure that pp isn't about to run the g
						// we are about to steal.
						// The important use case here is when the g running
						// on pp ready()s another g and then almost
						// immediately blocks. Instead of stealing runnext
						// in this window, back off to give pp a chance to
						// schedule runnext. This will avoid thrashing gs
						// between different Ps.
						// A sync chan send/recv takes ~50ns as of time of
						// writing, so 3us gives ~50x overshoot.
						if !osHasLowResTimer {
							usleep(3)
						} else {
							// On some platforms system timer granularity is
							// 1-15ms, which is way too much for this
							// optimization. So just yield.
							osyield()
						}
					}
					if !pp.runnext.cas(next, 0) {
						continue
					}
					batch[batchHead%uint32(len(batch))] = next
					return 1
				}
			}
			return 0
		}
		if n > uint32(len(pp.runq)/2) { // read inconsistent h and t
			continue
		}
		for i := uint32(0); i < n; i++ {
			g := pp.runq[(h+i)%uint32(len(pp.runq))]
			batch[(batchHead+i)%uint32(len(batch))] = g
		}
		if atomic.CasRel(&pp.runqhead, h, h+n) { // cas-release, commits consume
			return n
		}
	}
}

// Steal half of elements from local runnable queue of p2
// and put onto local runnable queue of p.
// Returns one of the stolen elements (or nil if failed).
func runqsteal(pp, p2 *p, stealRunNextG bool) *g {
	t := pp.runqtail
	n := runqgrab(p2, &pp.runq, t, stealRunNextG)
	if n == 0 {
		return nil
	}
	n--
	gp := pp.runq[(t+n)%uint32(len(pp.runq))].ptr()
	if n == 0 {
		return gp
	}
	h := atomic.LoadAcq(&pp.runqhead) // load-acquire, synchronize with consumers
	if t-h+n >= uint32(len(pp.runq)) {
		throw("runqsteal: runq overflow")
	}
	atomic.StoreRel(&pp.runqtail, t+n) // store-release, makes the item available for consumption
	return gp
}

// A gQueue is a dequeue of Gs linked through g.schedlink. A G can only
// be on one gQueue or gList at a time.
type gQueue struct {
	head guintptr
	tail guintptr
}

// empty reports whether q is empty.
func (q *gQueue) empty() bool {
	return q.head == 0
}

// push adds gp to the head of q.
func (q *gQueue) push(gp *g) {
	gp.schedlink = q.head
	q.head.set(gp)
	if q.tail == 0 {
		q.tail.set(gp)
	}
}

// pushBack adds gp to the tail of q.
func (q *gQueue) pushBack(gp *g) {
	gp.schedlink = 0
	if q.tail != 0 {
		q.tail.ptr().schedlink.set(gp)
	} else {
		q.head.set(gp)
	}
	q.tail.set(gp)
}

// pushBackAll adds all Gs in q2 to the tail of q. After this q2 must
// not be used.
func (q *gQueue) pushBackAll(q2 gQueue) {
	if q2.tail == 0 {
		return
	}
	q2.tail.ptr().schedlink = 0
	if q.tail != 0 {
		q.tail.ptr().schedlink = q2.head
	} else {
		q.head = q2.head
	}
	q.tail = q2.tail
}

// pop removes and returns the head of queue q. It returns nil if
// q is empty.
func (q *gQueue) pop() *g {
	gp := q.head.ptr()
	if gp != nil {
		q.head = gp.schedlink
		if q.head == 0 {
			q.tail = 0
		}
	}
	return gp
}

// popList takes all Gs in q and returns them as a gList.
func (q *gQueue) popList() gList {
	stack := gList{q.head}
	*q = gQueue{}
	return stack
}

// A gList is a list of Gs linked through g.schedlink. A G can only be
// on one gQueue or gList at a time.
type gList struct {
	head guintptr
}

// empty reports whether l is empty.
func (l *gList) empty() bool {
	return l.head == 0
}

// push adds gp to the head of l.
func (l *gList) push(gp *g) {
	gp.schedlink = l.head
	l.head.set(gp)
}

// pushAll prepends all Gs in q to l.
func (l *gList) pushAll(q gQueue) {
	if !q.empty() {
		q.tail.ptr().schedlink = l.head
		l.head = q.head
	}
}

// pop removes and returns the head of l. If l is empty, it returns nil.
func (l *gList) pop() *g {
	gp := l.head.ptr()
	if gp != nil {
		l.head = gp.schedlink
	}
	return gp
}

//go:linkname setMaxThreads runtime/debug.setMaxThreads
func setMaxThreads(in int) (out int) {
	lock(&sched.lock)
	out = int(sched.maxmcount)
	if in > 0x7fffffff { // MaxInt32
		sched.maxmcount = 0x7fffffff
	} else {
		sched.maxmcount = int32(in)
	}
	checkmcount()
	unlock(&sched.lock)
	return
}

// procPin should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/bytedance/gopkg
//   - github.com/choleraehyq/pid
//   - github.com/songzhibin97/gkit
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname procPin
//go:nosplit
func procPin() int {
	gp := getg()
	mp := gp.m

	mp.locks++
	return int(mp.p.ptr().id)
}

// procUnpin should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/bytedance/gopkg
//   - github.com/choleraehyq/pid
//   - github.com/songzhibin97/gkit
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname procUnpin
//go:nosplit
func procUnpin() {
	gp := getg()
	gp.m.locks--
}

//go:linkname sync_runtime_procPin sync.runtime_procPin
//go:nosplit
func sync_runtime_procPin() int {
	return procPin()
}

//go:linkname sync_runtime_procUnpin sync.runtime_procUnpin
//go:nosplit
func sync_runtime_procUnpin() {
	procUnpin()
}

//go:linkname sync_atomic_runtime_procPin sync/atomic.runtime_procPin
//go:nosplit
func sync_atomic_runtime_procPin() int {
	return procPin()
}

//go:linkname sync_atomic_runtime_procUnpin sync/atomic.runtime_procUnpin
//go:nosplit
func sync_atomic_runtime_procUnpin() {
	procUnpin()
}

// Active spinning for sync.Mutex.
//
// sync_runtime_canSpin should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/livekit/protocol
//   - github.com/sagernet/gvisor
//   - gvisor.dev/gvisor
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname sync_runtime_canSpin sync.runtime_canSpin
//go:nosplit
func sync_runtime_canSpin(i int) bool {
	// sync.Mutex is cooperative, so we are conservative with spinning.
	// Spin only few times and only if running on a multicore machine and
	// GOMAXPROCS>1 and there is at least one other running P and local runq is empty.
	// As opposed to runtime mutex we don't do passive spinning here,
	// because there can be work on global runq or on other Ps.
	if i >= active_spin || ncpu <= 1 || gomaxprocs <= sched.npidle.Load()+sched.nmspinning.Load()+1 {
		return false
	}
	if p := getg().m.p.ptr(); !runqempty(p) {
		return false
	}
	return true
}

// sync_runtime_doSpin should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/livekit/protocol
//   - github.com/sagernet/gvisor
//   - gvisor.dev/gvisor
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname sync_runtime_doSpin sync.runtime_doSpin
//go:nosplit
func sync_runtime_doSpin() {
	procyield(active_spin_cnt)
}

var stealOrder randomOrder

// randomOrder/randomEnum are helper types for randomized work stealing.
// They allow to enumerate all Ps in different pseudo-random orders without repetitions.
// The algorithm is based on the fact that if we have X such that X and GOMAXPROCS
// are coprime, then a sequences of (i + X) % GOMAXPROCS gives the required enumeration.
type randomOrder struct {
	count    uint32
	coprimes []uint32
}

type randomEnum struct {
	i     uint32
	count uint32
	pos   uint32
	inc   uint32
}

func (ord *randomOrder) reset(count uint32) {
	ord.count = count
	ord.coprimes = ord.coprimes[:0]
	for i := uint32(1); i <= count; i++ {
		if gcd(i, count) == 1 {
			ord.coprimes = append(ord.coprimes, i)
		}
	}
}

func (ord *randomOrder) start(i uint32) randomEnum {
	return randomEnum{
		count: ord.count,
		pos:   i % ord.count,
		inc:   ord.coprimes[i/ord.count%uint32(len(ord.coprimes))],
	}
}

func (enum *randomEnum) done() bool {
	return enum.i == enum.count
}

func (enum *randomEnum) next() {
	enum.i++
	enum.pos = (enum.pos + enum.inc) % enum.count
}

func (enum *randomEnum) position() uint32 {
	return enum.pos
}

func gcd(a, b uint32) uint32 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// An initTask represents the set of initializations that need to be done for a package.
// Keep in sync with ../../test/noinit.go:initTask
type initTask struct {
	state uint32 // 0 = uninitialized, 1 = in progress, 2 = done
	nfns  uint32
	// followed by nfns pcs, uintptr sized, one per init function to run
}

// inittrace stores statistics for init functions which are
// updated by malloc and newproc when active is true.
var inittrace tracestat

type tracestat struct {
	active bool   // init tracing activation status
	id     uint64 // init goroutine id
	allocs uint64 // heap allocations
	bytes  uint64 // heap allocated bytes
}

func doInit(ts []*initTask) {
	for _, t := range ts {
		doInit1(t)
	}
}

func doInit1(t *initTask) {
	switch t.state {
	case 2: // fully initialized
		return
	case 1: // initialization in progress
		throw("recursive call during initialization - linker skew")
	default: // not initialized yet
		t.state = 1 // initialization in progress

		var (
			start  int64
			before tracestat
		)

		if inittrace.active {
			start = nanotime()
			// Load stats non-atomically since tracinit is updated only by this init goroutine.
			before = inittrace
		}

		if t.nfns == 0 {
			// We should have pruned all of these in the linker.
			throw("inittask with no functions")
		}

		firstFunc := add(unsafe.Pointer(t), 8)
		for i := uint32(0); i < t.nfns; i++ {
			p := add(firstFunc, uintptr(i)*goarch.PtrSize)
			f := *(*func())(unsafe.Pointer(&p))
			f()
		}

		if inittrace.active {
			end := nanotime()
			// Load stats non-atomically since tracinit is updated only by this init goroutine.
			after := inittrace

			f := *(*func())(unsafe.Pointer(&firstFunc))
			pkg := funcpkgpath(findfunc(abi.FuncPCABIInternal(f)))

			var sbuf [24]byte
			print("init ", pkg, " @")
			print(string(fmtNSAsMS(sbuf[:], uint64(start-runtimeInitTime))), " ms, ")
			print(string(fmtNSAsMS(sbuf[:], uint64(end-start))), " ms clock, ")
			print(string(itoa(sbuf[:], after.bytes-before.bytes)), " bytes, ")
			print(string(itoa(sbuf[:], after.allocs-before.allocs)), " allocs")
			print("\n")
		}

		t.state = 2 // initialization done
	}
}
