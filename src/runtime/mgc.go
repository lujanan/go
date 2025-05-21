// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Garbage collector (GC).
//
// The GC runs concurrently with mutator threads, is type accurate (aka precise), allows multiple
// GC thread to run in parallel. It is a concurrent mark and sweep that uses a write barrier. It is
// non-generational and non-compacting. Allocation is done using size segregated per P allocation
// areas to minimize fragmentation while eliminating locks in the common case.
//
// The algorithm decomposes into several steps.
// This is a high level description of the algorithm being used. For an overview of GC a good
// place to start is Richard Jones' gchandbook.org.
//
// The algorithm's intellectual heritage includes Dijkstra's on-the-fly algorithm, see
// Edsger W. Dijkstra, Leslie Lamport, A. J. Martin, C. S. Scholten, and E. F. M. Steffens. 1978.
// On-the-fly garbage collection: an exercise in cooperation. Commun. ACM 21, 11 (November 1978),
// 966-975.
// For journal quality proofs that these steps are complete, correct, and terminate see
// Hudson, R., and Moss, J.E.B. Copying Garbage Collection without stopping the world.
// Concurrency and Computation: Practice and Experience 15(3-5), 2003.
//
// 垃圾收集器(GC)
//
// GC与mutator线程并发运行，是类型精确的(也称为精确的)，允许多个GC线程并行运行。
// 它是一个使用写屏障的并发标记清除算法。它是非分代和非压缩的。
// 分配使用按P分配区域的大小隔离来完成，以最小化碎片，同时在常见情况下消除锁。
//
// 该算法分解为几个步骤。
// 这是正在使用的算法的高级描述。对于GC的概述，Richard Jones的gchandbook.org是一个很好的起点。
//
// 该算法的知识遗产包括Dijkstra的on-the-fly算法，参见
// Edsger W. Dijkstra, Leslie Lamport, A. J. Martin, C. S. Scholten, and E. F. M. Steffens. 1978.
// On-the-fly garbage collection: an exercise in cooperation. Commun. ACM 21, 11 (November 1978),
// 966-975.
// 关于这些步骤的完整性、正确性和终止性的期刊质量证明，参见
// Hudson, R., and Moss, J.E.B. Copying Garbage Collection without stopping the world.
// Concurrency and Computation: Practice and Experience 15(3-5), 2003.
// 1. GC performs sweep termination.
//
//    a. Stop the world. This causes all Ps to reach a GC safe-point.
//
//    b. Sweep any unswept spans. There will only be unswept spans if
//    this GC cycle was forced before the expected time.
//
// 1. GC执行清扫终止阶段
//
//    a. 停止世界(Stop the world)。这会导致所有的P都到达一个GC安全点。
//       安全点是指程序执行到某个位置时，所有对象的状态都是已知的，
//       可以安全地进行垃圾回收操作。
//
//    b. 清扫所有未清扫的span。只有在GC周期被强制提前执行时，
//       才会存在未清扫的span。span是内存管理的基本单位，
//       包含了多个连续的内存页。
//
// 2. GC performs the mark phase.
//
//    a. Prepare for the mark phase by setting gcphase to _GCmark
//    (from _GCoff), enabling the write barrier, enabling mutator
//    assists, and enqueueing root mark jobs. No objects may be
//    scanned until all Ps have enabled the write barrier, which is
//    accomplished using STW.
//
//    b. Start the world. From this point, GC work is done by mark
//    workers started by the scheduler and by assists performed as
//    part of allocation. The write barrier shades both the
//    overwritten pointer and the new pointer value for any pointer
//    writes (see mbarrier.go for details). Newly allocated objects
//    are immediately marked black.
//
//    c. GC performs root marking jobs. This includes scanning all
//    stacks, shading all globals, and shading any heap pointers in
//    off-heap runtime data structures. Scanning a stack stops a
//    goroutine, shades any pointers found on its stack, and then
//    resumes the goroutine.
//
//    d. GC drains the work queue of grey objects, scanning each grey
//    object to black and shading all pointers found in the object
//    (which in turn may add those pointers to the work queue).
//
//    e. Because GC work is spread across local caches, GC uses a
//    distributed termination algorithm to detect when there are no
//    more root marking jobs or grey objects (see gcMarkDone). At this
//    point, GC transitions to mark termination.
//
// 2. GC执行标记阶段
//
//    a. 准备标记阶段：将gcphase从_GCoff设置为_GCmark，启用写屏障，
//       启用mutator辅助，并将根标记作业加入队列。在所有P都启用写屏障之前，
//       不能扫描任何对象，这是通过STW(Stop The World)来实现的。
//
//    b. 启动世界(Start the world)。从这时起，GC工作由调度器启动的标记工作线程
//       和作为分配一部分的辅助线程来完成。写屏障会对任何指针写入操作中的
//       被覆盖指针和新指针值都进行着色(详见mbarrier.go)。新分配的对象
//       会立即被标记为黑色。
//
//    c. GC执行根标记作业。这包括扫描所有栈，对所有全局变量进行着色，
//       以及对堆外运行时数据结构中的堆指针进行着色。扫描栈会暂停一个goroutine，
//       对其栈上找到的所有指针进行着色，然后恢复该goroutine的执行。
//
//    d. GC清空灰色对象的工作队列，将每个灰色对象扫描为黑色，
//       并对对象中找到的所有指针进行着色(这可能会将这些指针添加到工作队列中)。
//
//    e. 由于GC工作分布在本地缓存中，GC使用分布式终止算法来检测是否
//       没有更多的根标记作业或灰色对象(参见gcMarkDone)。此时，GC转换到标记终止阶段。
//
// 3. GC performs mark termination.
//
//    a. Stop the world.
//
//    b. Set gcphase to _GCmarktermination, and disable workers and
//    assists.
//
//    c. Perform housekeeping like flushing mcaches.
//
// 3. GC执行标记终止阶段
//
//    a. 停止世界(Stop The World)。暂停所有goroutine的执行。
//
//    b. 将gcphase设置为_GCmarktermination，并禁用所有工作线程和辅助线程。
//       这确保了在标记终止阶段不会有新的标记工作开始。
//
//    c. 执行清理工作，比如刷新所有mcache。这是为了确保所有缓存中的对象
//       都被正确处理，为下一个GC周期做准备。
//
// 4. GC performs the sweep phase.
//
//    a. Prepare for the sweep phase by setting gcphase to _GCoff,
//    setting up sweep state and disabling the write barrier.
//
//    b. Start the world. From this point on, newly allocated objects
//    are white, and allocating sweeps spans before use if necessary.
//
//    c. GC does concurrent sweeping in the background and in response
//    to allocation. See description below.
//
// 4. GC执行清扫阶段
//
//    a. 准备清扫阶段：将gcphase设置为_GCoff，设置清扫状态并禁用写屏障。
//       这标志着GC进入清扫阶段，此时不再需要写屏障来跟踪对象的变化。
//
//    b. 启动世界(Start the world)。从这时起，新分配的对象都是白色的，
//       并且在必要时会在使用前清扫span。这确保了新分配的对象能够被正确管理。
//
//    c. GC在后台并发执行清扫工作，并响应内存分配请求。清扫工作会与程序
//       的正常执行同时进行，以提高效率。具体实现细节见下文描述。
//
// 5. When sufficient allocation has taken place, replay the sequence
// starting with 1 above. See discussion of GC rate below.
//
// 5. 当内存分配达到一定量时，重新开始执行上述从步骤1开始的序列。
//    具体的内存分配触发阈值和GC频率的讨论见下文。

// Concurrent sweep.
//
// The sweep phase proceeds concurrently with normal program execution.
// The heap is swept span-by-span both lazily (when a goroutine needs another span)
// and concurrently in a background goroutine (this helps programs that are not CPU bound).
// At the end of STW mark termination all spans are marked as "needs sweeping".
//
// The background sweeper goroutine simply sweeps spans one-by-one.
//
// To avoid requesting more OS memory while there are unswept spans, when a
// goroutine needs another span, it first attempts to reclaim that much memory
// by sweeping. When a goroutine needs to allocate a new small-object span, it
// sweeps small-object spans for the same object size until it frees at least
// one object. When a goroutine needs to allocate large-object span from heap,
// it sweeps spans until it frees at least that many pages into heap. There is
// one case where this may not suffice: if a goroutine sweeps and frees two
// nonadjacent one-page spans to the heap, it will allocate a new two-page
// span, but there can still be other one-page unswept spans which could be
// combined into a two-page span.
//
// It's critical to ensure that no operations proceed on unswept spans (that would corrupt
// mark bits in GC bitmap). During GC all mcaches are flushed into the central cache,
// so they are empty. When a goroutine grabs a new span into mcache, it sweeps it.
// When a goroutine explicitly frees an object or sets a finalizer, it ensures that
// the span is swept (either by sweeping it, or by waiting for the concurrent sweep to finish).
// The finalizer goroutine is kicked off only when all spans are swept.
// When the next GC starts, it sweeps all not-yet-swept spans (if any).

// 并发清扫
//
// 清扫阶段与正常程序执行并发进行。堆的清扫是按span逐个进行的，包括两种方式：
// 1. 惰性清扫：当goroutine需要新的span时进行
// 2. 后台并发清扫：通过后台goroutine进行（这对非CPU密集型程序特别有帮助）
// 在STW标记终止阶段结束时，所有span都被标记为"需要清扫"状态。
//
// 后台清扫goroutine简单地一个接一个地清扫span。
//
// 为了避免在有未清扫span的情况下请求更多操作系统内存，当一个goroutine需要新的span时，
// 它会首先尝试通过清扫来回收足够的内存。具体来说：
// - 当需要分配新的小对象span时，它会清扫相同大小的span直到至少释放一个对象
// - 当需要从堆中分配大对象span时，它会清扫span直到至少释放相应数量的页面到堆中
// 这里有一个特殊情况：如果一个goroutine清扫并释放了两个不相邻的单页span到堆中，
// 它会分配一个新的两页span，但可能还存在其他未清扫的单页span可以合并成两页span。
//
// 确保不对未清扫的span进行操作是至关重要的（否则会破坏GC位图中的标记位）。
// 在GC期间，所有mcache都被刷新到central cache中，所以它们是空的。
// 当一个goroutine获取新的span到mcache时，它会先清扫这个span。
// 当一个goroutine显式释放对象或设置finalizer时，它会确保span被清扫
// （要么通过清扫它，要么等待并发清扫完成）。
// finalizer goroutine只有在所有span都被清扫后才会启动。
// 当下一次GC开始时，它会清扫所有尚未清扫的span（如果有的话）。

// GC rate.
// Next GC is after we've allocated an extra amount of memory proportional to
// the amount already in use. The proportion is controlled by GOGC environment variable
// (100 by default). If GOGC=100 and we're using 4M, we'll GC again when we get to 8M
// (this mark is computed by the gcController.heapGoal method). This keeps the GC cost in
// linear proportion to the allocation cost. Adjusting GOGC just changes the linear constant
// (and also the amount of extra memory used).

// GC触发率
// 下一次GC会在我们分配了与当前使用量成比例的额外内存后触发。
// 这个比例由GOGC环境变量控制（默认为100）。
// 例如：如果GOGC=100且当前使用了4M内存，那么当内存使用量达到8M时会再次触发GC
// （这个触发点由gcController.heapGoal方法计算）。
// 这种机制使得GC的成本与内存分配成本保持线性比例关系。
// 调整GOGC的值只会改变这个线性常数（同时也会改变额外使用的内存量）。

// Oblets
//
// In order to prevent long pauses while scanning large objects and to
// improve parallelism, the garbage collector breaks up scan jobs for
// objects larger than maxObletBytes into "oblets" of at most
// maxObletBytes. When scanning encounters the beginning of a large
// object, it scans only the first oblet and enqueues the remaining
// oblets as new scan jobs.

// Oblets（对象分片）
//
// 为了防止扫描大对象时出现长时间停顿并提高并行性，
// 垃圾收集器会将大于maxObletBytes的对象的扫描任务
// 分解成最大不超过maxObletBytes的"oblets"（对象分片）。
// 当扫描遇到一个大对象的开始位置时，它只扫描第一个分片，
// 并将剩余的分片作为新的扫描任务加入队列。

package runtime

import (
	"internal/cpu"
	"internal/runtime/atomic"
	"unsafe"
)

const (
	_DebugGC      = 0
	_FinBlockSize = 4 * 1024

	// concurrentSweep is a debug flag. Disabling this flag
	// ensures all spans are swept while the world is stopped.
	// concurrentSweep是一个调试标志。禁用此标志可以确保
	// 在程序暂停时完成所有span的清扫工作。
	concurrentSweep = true

	// debugScanConservative enables debug logging for stack
	// frames that are scanned conservatively.
	// debugScanConservative启用对保守式扫描的栈帧的调试日志记录。
	debugScanConservative = false

	// sweepMinHeapDistance is a lower bound on the heap distance
	// (in bytes) reserved for concurrent sweeping between GC
	// cycles.
	// sweepMinHeapDistance是在GC周期之间为并发清扫预留的
	// 堆内存距离（以字节为单位）的下限值。
	sweepMinHeapDistance = 1024 * 1024
)

// heapObjectsCanMove always returns false in the current garbage collector.
// It exists for go4.org/unsafe/assume-no-moving-gc, which is an
// unfortunate idea that had an even more unfortunate implementation.
// Every time a new Go release happened, the package stopped building,
// and the authors had to add a new file with a new //go:build line, and
// then the entire ecosystem of packages with that as a dependency had to
// explicitly update to the new version. Many packages depend on
// assume-no-moving-gc transitively, through paths like
// inet.af/netaddr -> go4.org/intern -> assume-no-moving-gc.
// This was causing a significant amount of friction around each new
// release, so we added this bool for the package to //go:linkname
// instead. The bool is still unfortunate, but it's not as bad as
// breaking the ecosystem on every new release.
//
// If the Go garbage collector ever does move heap objects, we can set
// this to true to break all the programs using assume-no-moving-gc.
//
// heapObjectsCanMove 在当前垃圾收集器中总是返回 false。
// 这个函数是为了 go4.org/unsafe/assume-no-moving-gc 包而存在的，
// 这是一个不幸的想法，其实现更加不幸。
// 每次发布新的 Go 版本时，这个包就会停止构建，
// 作者不得不添加一个带有新的 //go:build 行的新文件，
// 然后整个依赖这个包的生态系统都必须显式更新到新版本。
// 许多包通过类似 inet.af/netaddr -> go4.org/intern -> assume-no-moving-gc
// 这样的路径间接依赖 assume-no-moving-gc。
// 这导致每次新版本发布时都会产生大量的摩擦，
// 所以我们添加了这个布尔值供包通过 //go:linkname 使用。
// 这个布尔值仍然是不幸的，但比每次新版本发布都破坏生态系统要好。
//
// 如果 Go 垃圾收集器将来确实会移动堆对象，我们可以将其设置为 true
// 来破坏所有使用 assume-no-moving-gc 的程序。
//
//go:linkname heapObjectsCanMove
func heapObjectsCanMove() bool {
	return false
}

func gcinit() {
	// 检查 workbuf 结构体的大小是否符合预期
	// 如果大小不等于预定义的 _WorkbufSize，则抛出异常
	if unsafe.Sizeof(workbuf{}) != _WorkbufSize {
		throw("size of Workbuf is suboptimal")
	}
	// No sweep on the first cycle.
	// 在第一个 GC 周期不进行清扫
	// 将清扫状态设置为已排空状态
	sweep.active.state.Store(sweepDrainedMask)

	// Initialize GC pacer state.
	// Use the environment variable GOGC for the initial gcPercent value.
	// Use the environment variable GOMEMLIMIT for the initial memoryLimit value.
	// 初始化 GC 控制器状态
	// 使用环境变量 GOGC 作为初始的 gcPercent 值
	// 使用环境变量 GOMEMLIMIT 作为初始的 memoryLimit 值
	gcController.init(readGOGC(), readGOMEMLIMIT())

	// 初始化工作信号量，用于控制 GC 的启动和完成
	work.startSema = 1
	work.markDoneSema = 1

	// 初始化各种锁，用于保护并发访问
	// 初始化清扫等待者队列的锁
	lockInit(&work.sweepWaiters.lock, lockRankSweepWaiters)
	// 初始化辅助队列的锁
	lockInit(&work.assistQueue.lock, lockRankAssistQueue)
	// 初始化强引用到弱引用队列的锁
	lockInit(&work.strongFromWeak.lock, lockRankStrongFromWeakQueue)
	// 初始化工作缓冲区 span 的锁
	lockInit(&work.wbufSpans.lock, lockRankWbufSpans)
}

// gcenable is called after the bulk of the runtime initialization,
// just before we're about to start letting user code run.
// It kicks off the background sweeper goroutine, the background
// scavenger goroutine, and enables GC.
//
// gcenable 在运行时初始化的大部分工作完成后被调用，
// 就在即将开始运行用户代码之前。
// 它启动后台清扫器 goroutine、后台回收器 goroutine，
// 并启用垃圾回收器。
func gcenable() {
	// Kick off sweeping and scavenging.
	c := make(chan int, 2)   // 创建一个容量为2的通道，用于同步后台goroutine的启动
	go bgsweep(c)            // 启动后台清扫器goroutine，用于清理未使用的内存
	go bgscavenge(c)         // 启动后台回收器goroutine，用于回收空闲内存
	<-c                      // 等待第一个goroutine就绪
	<-c                      // 等待第二个goroutine就绪
	memstats.enablegc = true // 现在运行时已经初始化完成，可以启用GC了
}

// Garbage collector phase.
// Indicates to write barrier and synchronization task to perform.
// 垃圾回收器阶段
// 指示写屏障和同步任务要执行的操作
var gcphase uint32

// The compiler knows about this variable.
// If you change it, you must change builtin/runtime.go, too.
// If you change the first four bytes, you must also change the write
// barrier insertion code.
//
// writeBarrier should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/bytedance/sonic
//   - github.com/cloudwego/frugal
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
// 编译器知道这个变量
// 如果修改它，你必须同时修改 builtin/runtime.go
// 如果修改前四个字节，你还必须修改写屏障插入代码
//
// writeBarrier 应该是一个内部实现细节
// 但是很多包通过 linkname 访问它
// 不光彩的包包括：
//   - github.com/bytedance/sonic
//   - github.com/cloudwego/frugal
//
// 不要删除或改变类型签名
// 参见 go.dev/issue/67401
//
//go:linkname writeBarrier
var writeBarrier struct {
	enabled bool    // compiler emits a check of this before calling write barrier
	pad     [3]byte // compiler uses 32-bit load for "enabled" field
	alignme uint64  // guarantee alignment so that compiler can use a 32 or 64-bit load
	// enabled: 编译器在调用写屏障前会检查这个字段
	// pad: 编译器使用32位加载来读取"enabled"字段
	// alignme: 保证对齐，这样编译器可以使用32位或64位加载
}

// gcBlackenEnabled is 1 if mutator assists and background mark
// workers are allowed to blacken objects. This must only be set when
// gcphase == _GCmark.
// gcBlackenEnabled 为1时表示允许 mutator 辅助标记和后台标记工作器
// 将对象标记为黑色。这个标志只能在 gcphase == _GCmark 时设置。
var gcBlackenEnabled uint32

const (
	// GC未运行；后台进行清扫，写屏障禁用
	_GCoff = iota // GC not running; sweeping in background, write barrier disabled
	// GC正在标记根对象和工作缓冲区：分配黑色对象，写屏障启用
	_GCmark // GC marking roots and workbufs: allocate black, write barrier ENABLED
	// GC标记终止阶段：分配黑色对象，P协助GC，写屏障启用
	_GCmarktermination // GC mark termination: allocate black, P's help GC, write barrier ENABLED
)

// setGCPhase sets the current GC phase and updates the write barrier state.
// setGCPhase 设置当前GC阶段并更新写屏障状态
//
//go:nosplit
func setGCPhase(x uint32) {
	// Store the new GC phase atomically
	// 原子地存储新的GC阶段
	atomic.Store(&gcphase, x)

	// Enable write barrier if we're in mark or mark termination phase
	// 如果处于标记阶段或标记终止阶段，则启用写屏障
	writeBarrier.enabled = gcphase == _GCmark || gcphase == _GCmarktermination
}

// gcMarkWorkerMode represents the mode that a concurrent mark worker
// should operate in.
//
// Concurrent marking happens through four different mechanisms. One
// is mutator assists, which happen in response to allocations and are
// not scheduled. The other three are variations in the per-P mark
// workers and are distinguished by gcMarkWorkerMode.
//
// gcMarkWorkerMode 表示并发标记工作器应该以什么模式运行
//
// 并发标记通过四种不同的机制进行：
// 1. mutator assists（mutator辅助标记）：在响应内存分配时发生，不需要调度
// 2. 其他三种机制是每个P的标记工作器的变体，通过gcMarkWorkerMode来区分
type gcMarkWorkerMode int

const (
	// gcMarkWorkerNotWorker indicates that the next scheduled G is not
	// starting work and the mode should be ignored.
	// gcMarkWorkerNotWorker 表示下一个被调度的G不会开始工作，
	// 这个模式应该被忽略。
	gcMarkWorkerNotWorker gcMarkWorkerMode = iota

	// gcMarkWorkerDedicatedMode indicates that the P of a mark
	// worker is dedicated to running that mark worker. The mark
	// worker should run without preemption.
	// gcMarkWorkerDedicatedMode 表示标记工作器的P专门用于运行该标记工作器。
	// 标记工作器应该在不被抢占的情况下运行。
	gcMarkWorkerDedicatedMode

	// gcMarkWorkerFractionalMode indicates that a P is currently
	// running the "fractional" mark worker. The fractional worker
	// is necessary when GOMAXPROCS*gcBackgroundUtilization is not
	// an integer and using only dedicated workers would result in
	// utilization too far from the target of gcBackgroundUtilization.
	// The fractional worker should run until it is preempted and
	// will be scheduled to pick up the fractional part of
	// GOMAXPROCS*gcBackgroundUtilization.
	// gcMarkWorkerFractionalMode 表示P当前正在运行"分数"标记工作器。
	// 当GOMAXPROCS*gcBackgroundUtilization不是整数时，需要分数工作器，
	// 因为仅使用专用工作器会导致利用率与gcBackgroundUtilization的目标相差太远。
	// 分数工作器应该运行到被抢占为止，并将被调度来获取GOMAXPROCS*gcBackgroundUtilization的分数部分。
	gcMarkWorkerFractionalMode

	// gcMarkWorkerIdleMode indicates that a P is running the mark
	// worker because it has nothing else to do. The idle worker
	// should run until it is preempted and account its time
	// against gcController.idleMarkTime.
	// gcMarkWorkerIdleMode 表示P正在运行标记工作器是因为它没有其他事情可做。
	// 空闲工作器应该运行到被抢占为止，并将其时间计入gcController.idleMarkTime。
	gcMarkWorkerIdleMode
)

// gcMarkWorkerModeStrings are the strings labels of gcMarkWorkerModes
// to use in execution traces.
// gcMarkWorkerModeStrings 是 gcMarkWorkerModes 的字符串标签数组，
// 用于在执行跟踪中标识不同的标记工作器模式。
// 数组中的每个字符串对应一个标记工作器模式：
// - "Not worker": 非工作器模式
// - "GC (dedicated)": 专用标记工作器模式
// - "GC (fractional)": 分数标记工作器模式
// - "GC (idle)": 空闲标记工作器模式
var gcMarkWorkerModeStrings = [...]string{
	"Not worker",
	"GC (dedicated)",
	"GC (fractional)",
	"GC (idle)",
}

// pollFractionalWorkerExit reports whether a fractional mark worker
// should self-preempt. It assumes it is called from the fractional
// worker.
// pollFractionalWorkerExit 报告分数标记工作器是否应该自我抢占。
// 该函数假设它是由分数工作器调用的。
func pollFractionalWorkerExit() bool {
	// This should be kept in sync with the fractional worker
	// scheduler logic in findRunnableGCWorker.
	// 这应该与 findRunnableGCWorker 中的分数工作器调度逻辑保持同步。
	now := nanotime()
	delta := now - gcController.markStartTime
	if delta <= 0 {
		return true
	}
	p := getg().m.p.ptr()
	selfTime := p.gcFractionalMarkTime + (now - p.gcMarkWorkerStartTime)
	// Add some slack to the utilization goal so that the
	// fractional worker isn't behind again the instant it exits.
	// 在利用率目标上添加一些余量，这样分数工作器在退出的瞬间不会再次落后。
	return float64(selfTime)/float64(delta) > 1.2*gcController.fractionalUtilizationGoal
}

var work workType

type workType struct {
	// full: 存储已满的workbuf块的无锁链表
	full lfstack // lock-free list of full blocks workbuf
	// _: CPU缓存行填充，防止full和empty之间的伪共享
	_ cpu.CacheLinePad // prevents false-sharing between full and empty
	// empty: 存储空的workbuf块的无锁链表
	empty lfstack // lock-free list of empty blocks workbuf
	// _: CPU缓存行填充，防止empty和nproc/nwait之间的伪共享
	_ cpu.CacheLinePad // prevents false-sharing between empty and nproc/nwait

	// wbufSpans: 管理workbuf的内存span
	wbufSpans struct {
		// lock: 互斥锁，保护对span列表的访问
		lock mutex
		// free is a list of spans dedicated to workbufs, but
		// that don't currently contain any workbufs.
		// free: 专门用于workbuf但目前不包含任何workbuf的span列表
		free mSpanList
		// busy is a list of all spans containing workbufs on
		// one of the workbuf lists.
		// busy: 包含在workbuf列表中的workbuf的所有span列表
		busy mSpanList
	}

	// Restore 64-bit alignment on 32-bit.
	// _: 在32位系统上恢复64位对齐
	_ uint32

	// bytesMarked is the number of bytes marked this cycle. This
	// includes bytes blackened in scanned objects, noscan objects
	// that go straight to black, and permagrey objects scanned by
	// markroot during the concurrent scan phase. This is updated
	// atomically during the cycle. Updates may be batched
	// arbitrarily, since the value is only read at the end of the
	// cycle.
	//
	// Because of benign races during marking, this number may not
	// be the exact number of marked bytes, but it should be very
	// close.
	//
	// Put this field here because it needs 64-bit atomic access
	// (and thus 8-byte alignment even on 32-bit architectures).
	// bytesMarked 表示本轮GC周期中被标记的字节数。这个数值包括：
	// 1. 在扫描对象中被标记为黑色的字节
	// 2. 直接标记为黑色的noscan对象（不需要扫描的对象）
	// 3. 在并发扫描阶段由markroot扫描的永久灰色对象
	// 这个值在GC周期中会被原子地更新。由于这个值只在周期结束时被读取，
	// 所以更新操作可以任意批量进行。
	//
	// 由于标记过程中存在良性竞争，这个数字可能不是被标记字节的精确数量，
	// 但应该非常接近实际值。
	//
	// 将这个字段放在这里是因为它需要64位原子访问（因此在32位架构上
	// 需要8字节对齐）。
	bytesMarked uint64

	// markrootNext: 下一个要处理的markroot任务索引
	markrootNext uint32 // next markroot job
	// markrootJobs: markroot任务的总数量
	markrootJobs uint32 // number of markroot jobs

	// nproc: 当前参与GC的P(处理器)的数量
	nproc uint32
	// tstart: GC开始的时间戳
	tstart int64
	// nwait: 等待GC完成的goroutine数量
	nwait uint32

	// Number of roots of various root types. Set by gcMarkRootPrepare.
	//
	// nStackRoots == len(stackRoots), but we have nStackRoots for
	// consistency.
	// 各种根类型的数量。由gcMarkRootPrepare设置。
	//
	// nStackRoots等于stackRoots的长度，但为了保持一致性，
	// 我们仍然使用nStackRoots变量。
	//
	// nDataRoots: 数据段根对象的数量
	// nBSSRoots: BSS段根对象的数量
	// nSpanRoots: span根对象的数量
	// nStackRoots: 栈根对象的数量
	nDataRoots, nBSSRoots, nSpanRoots, nStackRoots int

	// Base indexes of each root type. Set by gcMarkRootPrepare.
	// 每种根类型的基准索引。由gcMarkRootPrepare设置。
	// baseData: 数据段根对象的起始索引
	// baseBSS: BSS段根对象的起始索引
	// baseSpans: span根对象的起始索引
	// baseStacks: 栈根对象的起始索引
	// baseEnd: 所有根对象索引的结束位置
	baseData, baseBSS, baseSpans, baseStacks, baseEnd uint32

	// stackRoots is a snapshot of all of the Gs that existed
	// before the beginning of concurrent marking. The backing
	// store of this must not be modified because it might be
	// shared with allgs.
	// stackRoots 是在并发标记开始前存在的所有 G(goroutine) 的快照。
	// 这个切片的底层存储不能被修改，因为它可能与 allgs 共享。
	stackRoots []*g

	// Each type of GC state transition is protected by a lock.
	// Since multiple threads can simultaneously detect the state
	// transition condition, any thread that detects a transition
	// condition must acquire the appropriate transition lock,
	// re-check the transition condition and return if it no
	// longer holds or perform the transition if it does.
	// Likewise, any transition must invalidate the transition
	// condition before releasing the lock. This ensures that each
	// transition is performed by exactly one thread and threads
	// that need the transition to happen block until it has
	// happened.
	//
	// 每种GC状态转换都由一个锁来保护。
	// 由于多个线程可以同时检测到状态转换条件，
	// 任何检测到转换条件的线程都必须获取适当的转换锁，
	// 重新检查转换条件，如果条件不再成立则返回，
	// 如果条件仍然成立则执行转换。
	// 同样，任何转换在释放锁之前都必须使转换条件失效。
	// 这确保了每个转换只由一个线程执行，
	// 需要转换发生的线程会阻塞直到转换完成。
	//
	// startSema protects the transition from "off" to mark or
	// mark termination.
	// startSema 保护从"关闭"状态到标记或标记终止状态的转换。
	startSema uint32
	// markDoneSema protects transitions from mark to mark termination.
	// markDoneSema 保护从标记状态到标记终止状态的转换。
	markDoneSema uint32

	// bgMarkDone: 用于标记后台标记完成的信号量
	// 当达到后台标记完成点时，通过CAS操作将其设置为1
	bgMarkDone uint32 // cas to 1 when at a background mark completion point
	// Background mark completion signaling

	// mode is the concurrency mode of the current GC cycle.
	// mode: 当前GC周期的并发模式
	// 决定了GC如何与程序并发执行
	mode gcMode

	// userForced indicates the current GC cycle was forced by an
	// explicit user call.
	// userForced: 表示当前GC周期是否由用户显式调用触发
	// 当用户通过runtime.GC()等函数强制触发GC时，此标志为true
	userForced bool

	// initialHeapLive is the value of gcController.heapLive at the
	// beginning of this GC cycle.
	// initialHeapLive: 当前GC周期开始时gcController.heapLive的值
	// 用于记录GC开始时的堆内存使用量，作为GC周期的基准值
	initialHeapLive uint64

	// assistQueue is a queue of assists that are blocked because
	// there was neither enough credit to steal or enough work to
	// do.
	// assistQueue: 用于存储被阻塞的辅助标记协程的队列
	// 当协程既没有足够的信用值可以窃取，也没有足够的工作可做时，
	// 它们会被放入这个队列中等待
	assistQueue struct {
		lock mutex  // 保护队列访问的互斥锁
		q    gQueue // 存储被阻塞的goroutine的队列
	}

	// sweepWaiters is a list of blocked goroutines to wake when
	// we transition from mark termination to sweep.
	// sweepWaiters: 存储需要被唤醒的阻塞协程的列表
	// 当GC从标记终止阶段转换到清扫阶段时，
	// 这些协程将被唤醒以执行清扫工作
	sweepWaiters struct {
		lock mutex // 保护列表访问的互斥锁
		list gList // 存储等待被唤醒的goroutine的列表
	}

	// strongFromWeak controls how the GC interacts with weak->strong
	// pointer conversions.
	// strongFromWeak 控制垃圾回收器如何处理弱引用到强引用的转换
	strongFromWeak struct {
		// block is a flag set during mark termination that prevents
		// new weak->strong conversions from executing by blocking the
		// goroutine and enqueuing it onto q.
		//
		// Mutated only by one goroutine at a time in gcMarkDone,
		// with globally-synchronizing events like forEachP and
		// stopTheWorld.
		// block: 在标记终止阶段设置的标志位，用于阻止新的弱引用到强引用的转换
		// 通过阻塞goroutine并将其加入队列q来实现
		// 该标志位只能在gcMarkDone中由单个goroutine修改，
		// 并且需要与forEachP和stopTheWorld等全局同步事件配合使用
		block bool

		// q is a queue of goroutines that attempted to perform a
		// weak->strong conversion during mark termination.
		//
		// Protected by lock.
		// q: 存储那些在标记终止阶段尝试执行弱引用到强引用转换的goroutine的队列
		// 由lock互斥锁保护
		lock mutex
		q    gQueue
	}

	// cycles is the number of completed GC cycles, where a GC
	// cycle is sweep termination, mark, mark termination, and
	// sweep. This differs from memstats.numgc, which is
	// incremented at mark termination.
	// cycles: 已完成的GC周期数
	// 一个完整的GC周期包括：清扫终止、标记、标记终止和清扫四个阶段
	// 注意：这与memstats.numgc不同，后者在标记终止阶段就增加计数
	cycles atomic.Uint32

	// Timing/utilization stats for this cycle.
	// 当前GC周期的时间统计和利用率统计
	// stwprocs: 在STW(Stop The World)期间停止的P的数量
	// maxprocs: 系统中最大的P的数量
	stwprocs, maxprocs int32
	// tSweepTerm, tMark, tMarkTerm, tEnd: 各个GC阶段开始的时间点（纳秒时间戳）
	tSweepTerm, tMark, tMarkTerm, tEnd int64 // nanotime() of phase start

	// pauseNS is the total STW time this cycle, measured as the time between
	// when stopping began (just before trying to stop Ps) and just after the
	// world started again.
	// pauseNS: 当前GC周期中STW(Stop The World)的总时间
	// 从开始停止Ps之前到世界重新启动之后的时间间隔
	pauseNS int64

	// debug.gctrace heap sizes for this cycle.
	// 当前GC周期中堆内存大小的统计信息，用于debug.gctrace
	// heap0: GC开始时的堆大小
	// heap1: GC标记阶段结束时的堆大小
	// heap2: GC结束时的堆大小
	heap0, heap1, heap2 uint64

	// Cumulative estimated CPU usage.
	// 累计估计的CPU使用情况统计
	cpuStats
}

// GC runs a garbage collection and blocks the caller until the
// garbage collection is complete. It may also block the entire
// program.
// GC执行垃圾回收并阻塞调用者直到垃圾回收完成。它也可能阻塞整个程序。
func GC() {
	// We consider a cycle to be: sweep termination, mark, mark
	// termination, and sweep. This function shouldn't return
	// until a full cycle has been completed, from beginning to
	// end. Hence, we always want to finish up the current cycle
	// and start a new one. That means:
	//
	// 1. In sweep termination, mark, or mark termination of cycle
	// N, wait until mark termination N completes and transitions
	// to sweep N.
	//
	// 2. In sweep N, help with sweep N.
	//
	// At this point we can begin a full cycle N+1.
	//
	// 3. Trigger cycle N+1 by starting sweep termination N+1.
	//
	// 4. Wait for mark termination N+1 to complete.
	//
	// 5. Help with sweep N+1 until it's done.
	//
	// This all has to be written to deal with the fact that the
	// GC may move ahead on its own. For example, when we block
	// until mark termination N, we may wake up in cycle N+2.

	// 我们认为一个完整的GC周期包括：清扫终止、标记、标记终止和清扫四个阶段。
	// 这个函数不应该返回，直到一个完整的周期从开始到结束都完成。
	// 因此，我们总是希望完成当前周期并开始一个新的周期。这意味着：
	//
	// 1. 在周期N的清扫终止、标记或标记终止阶段，等待直到标记终止N完成并转换到清扫N。
	//
	// 2. 在清扫N阶段，协助完成清扫N。
	//
	// 此时我们可以开始一个完整的周期N+1。
	//
	// 3. 通过启动清扫终止N+1来触发周期N+1。
	//
	// 4. 等待标记终止N+1完成。
	//
	// 5. 协助完成清扫N+1直到它完成。
	//
	// 所有这些都必须考虑到GC可能会自行前进的事实。
	// 例如，当我们阻塞直到标记终止N时，我们可能会在周期N+2中醒来。

	// Wait until the current sweep termination, mark, and mark
	// termination complete.
	// 等待直到当前的清扫终止、标记和标记终止完成。
	n := work.cycles.Load() // 获取当前GC周期数
	gcWaitOnMark(n)         // 等待第n个标记阶段完成

	// We're now in sweep N or later. Trigger GC cycle N+1, which
	// will first finish sweep N if necessary and then enter sweep
	// termination N+1.
	// 我们现在处于清扫N或更晚的阶段。触发GC周期N+1，
	// 如果需要的话，它会先完成清扫N，然后进入清扫终止N+1。
	gcStart(gcTrigger{kind: gcTriggerCycle, n: n + 1})

	// Wait for mark termination N+1 to complete.
	// 等待标记终止N+1完成。
	gcWaitOnMark(n + 1)

	// Finish sweep N+1 before returning. We do this both to
	// complete the cycle and because runtime.GC() is often used
	// as part of tests and benchmarks to get the system into a
	// relatively stable and isolated state.
	// 在返回之前完成清扫N+1。我们这样做既是为了完成周期，
	// 也是因为runtime.GC()经常被用作测试和基准测试的一部分，
	// 以使系统进入相对稳定和隔离的状态。
	for work.cycles.Load() == n+1 && sweepone() != ^uintptr(0) {
		Gosched()
	}

	// Callers may assume that the heap profile reflects the
	// just-completed cycle when this returns (historically this
	// happened because this was a STW GC), but right now the
	// profile still reflects mark termination N, not N+1.
	//
	// As soon as all of the sweep frees from cycle N+1 are done,
	// we can go ahead and publish the heap profile.
	//
	// First, wait for sweeping to finish. (We know there are no
	// more spans on the sweep queue, but we may be concurrently
	// sweeping spans, so we have to wait.)

	// 调用者可能认为当函数返回时堆配置文件反映了刚刚完成的周期
	// (历史上这是因为这是一个STW GC)，但现在配置文件仍然反映的是
	// 标记终止N，而不是N+1。
	//
	// 一旦周期N+1的所有清扫释放完成，我们就可以发布堆配置文件。
	//
	// 首先，等待清扫完成。(我们知道清扫队列中没有更多的span了，
	// 但我们可能正在并发地清扫span，所以我们必须等待。)
	for work.cycles.Load() == n+1 && !isSweepDone() {
		Gosched()
	}

	// Now we're really done with sweeping, so we can publish the
	// stable heap profile. Only do this if we haven't already hit
	// another mark termination.

	// 现在清扫真的完成了，所以我们可以发布稳定的堆配置文件。
	// 只有在还没有进入另一个标记终止阶段时才这样做。
	mp := acquirem()
	cycle := work.cycles.Load()
	if cycle == n+1 || (gcphase == _GCmark && cycle == n+2) {
		mProf_PostSweep()
	}
	releasem(mp)
}

// gcWaitOnMark blocks until GC finishes the Nth mark phase. If GC has
// already completed this mark phase, it returns immediately.
// gcWaitOnMark 会阻塞直到GC完成第N个标记阶段。如果GC已经完成了这个标记阶段，
// 它会立即返回。
func gcWaitOnMark(n uint32) {
	for {
		// Disable phase transitions.
		// 禁用阶段转换
		lock(&work.sweepWaiters.lock)
		nMarks := work.cycles.Load()
		if gcphase != _GCmark {
			// We've already completed this cycle's mark.
			// 我们已经完成了这个周期的标记阶段
			nMarks++
		}
		if nMarks > n {
			// We're done.
			// 我们已经完成了
			unlock(&work.sweepWaiters.lock)
			return
		}

		// Wait until sweep termination, mark, and mark
		// termination of cycle N complete.
		// 等待直到周期N的清扫终止、标记和标记终止完成
		work.sweepWaiters.list.push(getg())
		goparkunlock(&work.sweepWaiters.lock, waitReasonWaitForGCCycle, traceBlockUntilGCEnds, 1)
	}
}

// gcMode indicates how concurrent a GC cycle should be.
// gcMode 表示GC周期的并发程度
type gcMode int

const (
	// 并发GC和清扫模式
	gcBackgroundMode gcMode = iota // concurrent GC and sweep
	// 立即进行STW(stop-the-world)GC，但并发清扫
	gcForceMode // stop-the-world GC now, concurrent sweep
	// 立即进行STW GC和STW清扫(由用户强制触发)
	gcForceBlockMode // stop-the-world GC now and STW sweep (forced by user)
)

// A gcTrigger is a predicate for starting a GC cycle. Specifically,
// it is an exit condition for the _GCoff phase.
// gcTrigger 是触发GC周期的谓词。具体来说，它是 _GCoff 阶段的退出条件。
type gcTrigger struct {
	kind gcTriggerKind // 触发GC的类型
	// gcTriggerTime: 当前时间，用于基于时间的GC触发
	now int64 // gcTriggerTime: current time
	// gcTriggerCycle: 要启动的GC周期编号
	n uint32 // gcTriggerCycle: cycle number to start
}

type gcTriggerKind int

const (
	// gcTriggerHeap indicates that a cycle should be started when
	// the heap size reaches the trigger heap size computed by the
	// controller.
	// gcTriggerHeap 表示当堆大小达到由控制器计算的触发堆大小时，
	// 应该启动一个GC周期
	gcTriggerHeap gcTriggerKind = iota

	// gcTriggerTime indicates that a cycle should be started when
	// it's been more than forcegcperiod nanoseconds since the
	// previous GC cycle.
	// gcTriggerTime 表示自上一个GC周期以来，如果已经过去了超过
	// forcegcperiod纳秒的时间，就应该启动一个新的GC周期
	gcTriggerTime

	// gcTriggerCycle indicates that a cycle should be started if
	// we have not yet started cycle number gcTrigger.n (relative
	// to work.cycles).
	// gcTriggerCycle 表示如果我们还没有启动周期号gcTrigger.n
	// (相对于work.cycles)，就应该启动一个新的GC周期
	gcTriggerCycle
)

// test reports whether the trigger condition is satisfied, meaning
// that the exit condition for the _GCoff phase has been met. The exit
// condition should be tested when allocating.
// test 函数用于检查触发条件是否满足，即 _GCoff 阶段的退出条件是否已满足。
// 这个退出条件应该在分配内存时进行测试。
func (t gcTrigger) test() bool {
	// 如果GC被禁用、正在panic或不在GC关闭阶段，则不触发GC
	if !memstats.enablegc || panicking.Load() != 0 || gcphase != _GCoff {
		return false
	}
	switch t.kind {
	case gcTriggerHeap:
		// 基于堆大小的触发条件：
		// 获取触发阈值，并检查当前堆大小是否超过该阈值
		trigger, _ := gcController.trigger()
		return gcController.heapLive.Load() >= trigger
	case gcTriggerTime:
		// 基于时间的触发条件：
		// 如果GC百分比为负，则不触发
		if gcController.gcPercent.Load() < 0 {
			return false
		}
		// 获取上次GC的时间，检查是否已经超过了强制GC周期
		lastgc := int64(atomic.Load64(&memstats.last_gc_nanotime))
		return lastgc != 0 && t.now-lastgc > forcegcperiod
	case gcTriggerCycle:
		// 基于GC周期的触发条件：
		// 检查是否已经到达指定的GC周期
		// 注意：这里考虑了计数器溢出的情况
		return int32(t.n-work.cycles.Load()) > 0
	}
	return true
}

// gcStart starts the GC. It transitions from _GCoff to _GCmark (if
// debug.gcstoptheworld == 0) or performs all of GC (if
// debug.gcstoptheworld != 0).
//
// This may return without performing this transition in some cases,
// such as when called on a system stack or with locks held.
// gcStart 启动垃圾回收。它会从 _GCoff 状态转换到 _GCmark 状态（如果
// debug.gcstoptheworld == 0），或者执行完整的GC（如果
// debug.gcstoptheworld != 0）。
//
// 在某些情况下，这个函数可能会直接返回而不执行状态转换，比如当它在
// 系统栈上被调用或者持有锁的情况下。

func gcStart(trigger gcTrigger) {
	// Since this is called from malloc and malloc is called in
	// the guts of a number of libraries that might be holding
	// locks, don't attempt to start GC in non-preemptible or
	// potentially unstable situations.
	// 由于这个函数是从 malloc 调用的，而 malloc 又可能被许多持有锁的
	// 库函数调用，所以在不可抢占或潜在不稳定的情况下不要尝试启动GC。

	// 获取当前M（机器）的引用
	mp := acquirem()
	// 检查当前G（goroutine）的状态
	if gp := getg(); gp == mp.g0 || mp.locks > 1 || mp.preemptoff != "" {
		// 如果当前G是系统G，或者M持有多个锁，或者M被标记为不可抢占
		// 则释放M并直接返回，不执行GC
		releasem(mp)
		return
	}
	// 释放M的引用
	releasem(mp)
	// 清空M引用
	mp = nil

	// Pick up the remaining unswept/not being swept spans concurrently
	//
	// This shouldn't happen if we're being invoked in background
	// mode since proportional sweep should have just finished
	// sweeping everything, but rounding errors, etc, may leave a
	// few spans unswept. In forced mode, this is necessary since
	// GC can be forced at any point in the sweeping cycle.
	//
	// We check the transition condition continuously here in case
	// this G gets delayed in to the next GC cycle.

	// 并发清理剩余的未清扫/正在清扫的span
	// 在后台模式下，这种情况不应该发生，因为比例清扫应该已经完成了所有清扫工作
	// 但是由于舍入误差等原因，可能会留下一些未清扫的span
	// 在强制模式下，这是必要的，因为GC可以在清扫周期的任何时刻被强制触发
	// 我们在这里持续检查转换条件，以防这个G被延迟到下一个GC周期
	for trigger.test() && sweepone() != ^uintptr(0) {
	}

	// Perform GC initialization and the sweep termination
	// transition.
	// 执行GC初始化和清扫终止转换
	semacquire(&work.startSema)
	// Re-check transition condition under transition lock.
	// 在转换锁下重新检查转换条件
	if !trigger.test() {
		semrelease(&work.startSema)
		return
	}

	// In gcstoptheworld debug mode, upgrade the mode accordingly.
	// We do this after re-checking the transition condition so
	// that multiple goroutines that detect the heap trigger don't
	// start multiple STW GCs.
	// 在gcstoptheworld调试模式下，相应地升级模式
	// 我们在重新检查转换条件后执行此操作，这样多个检测到堆触发器的goroutine
	// 就不会启动多个STW GC
	mode := gcBackgroundMode
	if debug.gcstoptheworld == 1 {
		mode = gcForceMode
	} else if debug.gcstoptheworld == 2 {
		mode = gcForceBlockMode
	}

	// Ok, we're doing it! Stop everybody else
	// 好的，我们开始执行！停止所有其他操作
	semacquire(&gcsema)    // 获取GC信号量，防止多个GC同时运行
	semacquire(&worldsema) // 获取世界信号量，准备停止世界

	// For stats, check if this GC was forced by the user.
	// Update it under gcsema to avoid gctrace getting wrong values.
	// 为了统计，检查这次GC是否由用户强制触发
	// 在gcsema下更新，以避免gctrace获取错误的值
	work.userForced = trigger.kind == gcTriggerCycle

	// 获取跟踪器并记录GC开始事件
	trace := traceAcquire()
	if trace.ok() {
		trace.GCStart()     // 记录GC开始事件
		traceRelease(trace) // 释放跟踪器
	}

	// Check that all Ps have finished deferred mcache flushes.
	// 检查所有P是否都完成了延迟的mcache刷新
	for _, p := range allp {
		// 获取当前P的mcache刷新代数值，并与全局清扫代数值比较
		// 如果不相等，说明该P的mcache还没有被正确刷新
		if fg := p.mcache.flushGen.Load(); fg != mheap_.sweepgen {
			println("runtime: p", p.id, "flushGen", fg, "!= sweepgen", mheap_.sweepgen)
			throw("p mcache not flushed")
		}
	}

	// Start background mark workers.
	// 启动后台标记工作协程
	gcBgMarkStartWorkers()

	// Reset mark state.
	// 重置标记状态
	systemstack(gcResetMarkState)

	// 设置STW(Stop The World)和最大处理器数量
	// 将当前可用的处理器数量赋值给STW处理器数和最大处理器数
	work.stwprocs, work.maxprocs = gomaxprocs, gomaxprocs
	if work.stwprocs > ncpu {
		// This is used to compute CPU time of the STW phases,
		// so it can't be more than ncpu, even if GOMAXPROCS is.
		// 用于计算STW阶段的CPU时间，即使GOMAXPROCS设置得更大，
		// 也不能超过实际的CPU核心数(ncpu)
		work.stwprocs = ncpu
	}
	// 记录GC开始时的堆内存使用量
	work.heap0 = gcController.heapLive.Load()
	// 初始化暂停时间为0
	work.pauseNS = 0
	// 设置GC模式
	work.mode = mode

	// 获取当前时间戳
	now := nanotime()
	// 记录清扫阶段开始的时间
	work.tSweepTerm = now
	var stw worldStop
	// 在系统栈上执行停止世界操作，进入清扫终止阶段
	systemstack(func() {
		stw = stopTheWorldWithSema(stwGCSweepTerm)
	})

	// Accumulate fine-grained stopping time.
	// 累积精确的停止时间
	// 将STW阶段的CPU时间添加到GC暂停时间统计中
	work.cpuStats.accumulateGCPauseTime(stw.stoppingCPUTime, 1)

	// Finish sweep before we start concurrent scan.
	// 在开始并发扫描之前完成清扫工作
	// 在系统栈上执行清扫完成操作
	systemstack(func() {
		finishsweep_m()
	})

	// clearpools before we start the GC. If we wait the memory will not be
	// reclaimed until the next GC cycle.
	// 在开始GC之前清空内存池。如果等待的话，内存将不会在下一个GC周期之前被回收
	clearpools()

	// 增加GC周期计数
	work.cycles.Add(1)

	// Assists and workers can start the moment we start
	// the world.
	// 辅助标记和标记工作协程可以在我们启动世界时立即开始
	gcController.startCycle(now, int(gomaxprocs), trigger)

	// Notify the CPU limiter that assists may begin.
	// 通知CPU限制器辅助标记可以开始了
	gcCPULimiter.startGCTransition(true, now)

	// In STW mode, disable scheduling of user Gs. This may also
	// disable scheduling of this goroutine, so it may block as
	// soon as we start the world again.
	// 在STW模式下，禁用用户G的调度。这也可能会禁用当前goroutine的调度，
	// 所以一旦我们重新启动世界，它可能会被阻塞
	if mode != gcBackgroundMode {
		schedEnableUser(false)
	}

	// Enter concurrent mark phase and enable
	// write barriers.
	//
	// Because the world is stopped, all Ps will
	// observe that write barriers are enabled by
	// the time we start the world and begin
	// scanning.
	//
	// Write barriers must be enabled before assists are
	// enabled because they must be enabled before
	// any non-leaf heap objects are marked. Since
	// allocations are blocked until assists can
	// happen, we want to enable assists as early as
	// possible.

	// 进入并发标记阶段并启用写屏障
	// 由于此时世界已停止，所有的P都会在重新启动世界并开始扫描时观察到写屏障已启用
	// 写屏障必须在启用辅助标记之前启用，因为它们必须在标记任何非叶子堆对象之前启用
	// 由于分配操作会被阻塞直到辅助标记可以开始，我们希望尽早启用辅助标记
	setGCPhase(_GCmark)

	// 准备后台标记工作
	// 必须在启用辅助标记之前完成
	gcBgMarkPrepare() // Must happen before assists are enabled.

	// 准备标记根对象
	// 扫描并标记所有根对象，包括栈、全局变量等
	gcMarkRootPrepare()

	// Mark all active tinyalloc blocks. Since we're
	// allocating from these, they need to be black like
	// other allocations. The alternative is to blacken
	// the tiny block on every allocation from it, which
	// would slow down the tiny allocator.
	// 标记所有活跃的tinyalloc块。由于我们正在从这些块中分配内存，
	// 它们需要像其他分配一样被标记为黑色。另一种选择是在每次分配时
	// 将tiny块标记为黑色，但这会降低tiny分配器的性能。
	gcMarkTinyAllocs()

	// At this point all Ps have enabled the write
	// barrier, thus maintaining the no white to
	// black invariant. Enable mutator assists to
	// put back-pressure on fast allocating
	// mutators.
	// 此时所有的P都已启用写屏障，从而维持了"没有白色到黑色"的不变量。
	// 启用mutator辅助标记，以对快速分配内存的mutator施加反压力。
	atomic.Store(&gcBlackenEnabled, 1)

	// In STW mode, we could block the instant systemstack
	// returns, so make sure we're not preemptible.
	// 在STW模式下，我们可能会在systemstack返回时立即被阻塞，
	// 所以确保当前goroutine不可被抢占。
	mp = acquirem()

	// Update the CPU stats pause time.
	//
	// Use maxprocs instead of stwprocs here because the total time
	// computed in the CPU stats is based on maxprocs, and we want them
	// to be comparable.
	// 更新CPU统计信息中的暂停时间。
	// 这里使用maxprocs而不是stwprocs，因为CPU统计信息中计算的总时间
	// 是基于maxprocs的，我们希望它们具有可比性。
	work.cpuStats.accumulateGCPauseTime(nanotime()-stw.finishedStopping, work.maxprocs)

	// Concurrent mark.
	// 并发标记阶段
	systemstack(func() {
		// 重新启动世界并获取当前时间
		// 0表示不等待所有P都准备好
		now = startTheWorldWithSema(0, stw)
		// 累加GC暂停时间
		work.pauseNS += now - stw.startedStopping
		// 记录标记阶段开始时间
		work.tMark = now

		// Release the CPU limiter.
		// 释放CPU限制器，允许更多CPU资源用于GC
		gcCPULimiter.finishGCTransition(now)
	})

	// Release the world sema before Gosched() in STW mode
	// because we will need to reacquire it later but before
	// this goroutine becomes runnable again, and we could
	// self-deadlock otherwise.
	// 在STW模式下，在Gosched()之前释放world信号量
	// 因为我们需要在goroutine再次变为可运行状态之前重新获取它
	// 否则可能会发生自死锁
	semrelease(&worldsema)
	releasem(mp)

	// Make sure we block instead of returning to user code
	// in STW mode.
	// 确保在STW模式下阻塞而不是返回到用户代码
	if mode != gcBackgroundMode {
		Gosched()
	}

	// 释放startSema信号量，允许其他goroutine继续执行
	semrelease(&work.startSema)
}

// gcMarkDoneFlushed counts the number of P's with flushed work.
//
// Ideally this would be a captured local in gcMarkDone, but forEachP
// escapes its callback closure, so it can't capture anything.
//
// This is protected by markDoneSema.
// gcMarkDoneFlushed 统计已经刷新工作的P的数量
//
// 理想情况下这应该是gcMarkDone中的一个局部变量，但是forEachP
// 会逃逸其回调闭包，所以不能捕获任何变量
//
// 这个变量由markDoneSema保护
var gcMarkDoneFlushed uint32

// gcDebugMarkDone contains fields used to debug/test mark termination.
// gcDebugMarkDone 包含用于调试/测试标记终止的字段
var gcDebugMarkDone struct {
	// spinAfterRaggedBarrier forces gcMarkDone to spin after it executes
	// the ragged barrier.
	// spinAfterRaggedBarrier 强制gcMarkDone在执行完ragged barrier后自旋
	spinAfterRaggedBarrier atomic.Bool

	// restartedDueTo27993 indicates that we restarted mark termination
	// due to the bug described in issue #27993.
	//
	// Protected by worldsema.
	// restartedDueTo27993 表示我们由于issue #27993中描述的bug
	// 而重新启动了标记终止
	//
	// 由worldsema保护
	restartedDueTo27993 bool
}

// gcMarkDone transitions the GC from mark to mark termination if all
// reachable objects have been marked (that is, there are no grey
// objects and can be no more in the future). Otherwise, it flushes
// all local work to the global queues where it can be discovered by
// other workers.
//
// This should be called when all local mark work has been drained and
// there are no remaining workers. Specifically, when
//
//	work.nwait == work.nproc && !gcMarkWorkAvailable(p)
//
// The calling context must be preemptible.
//
// Flushing local work is important because idle Ps may have local
// work queued. This is the only way to make that work visible and
// drive GC to completion.
//
// It is explicitly okay to have write barriers in this function. If
// it does transition to mark termination, then all reachable objects
// have been marked, so the write barrier cannot shade any more
// objects.

// gcMarkDone 函数用于将GC从标记阶段转换到标记终止阶段，前提是所有可达对象都已被标记
// (也就是说，没有灰色对象，且将来也不会有)。否则，它会将所有本地工作刷新到全局队列中，
// 以便其他工作线程可以发现。
//
// 当所有本地标记工作都已完成且没有剩余的工作线程时，应该调用此函数。具体来说，当满足以下条件时：
//
//	work.nwait == work.nproc && !gcMarkWorkAvailable(p)
//
// 调用上下文必须是可抢占的。
//
// 刷新本地工作很重要，因为空闲的P可能有本地工作排队。这是使这些工作可见并推动GC完成的唯一方式。
//
// 在这个函数中使用写屏障是明确允许的。如果确实转换到标记终止阶段，那么所有可达对象都已被标记，
// 因此写屏障不会使任何对象变灰。
func gcMarkDone() {
	// Ensure only one thread is running the ragged barrier at a
	// time.
	// 确保只有一个线程在运行ragged barrier
	semacquire(&work.markDoneSema)

top:
	// Re-check transition condition under transition lock.
	//
	// It's critical that this checks the global work queues are
	// empty before performing the ragged barrier. Otherwise,
	// there could be global work that a P could take after the P
	// has passed the ragged barrier.
	// 在转换锁下重新检查转换条件
	//
	// 在执行ragged barrier之前检查全局工作队列是否为空是至关重要的。
	// 否则，可能会有全局工作在一个P通过ragged barrier后被该P获取。
	if !(gcphase == _GCmark && work.nwait == work.nproc && !gcMarkWorkAvailable(nil)) {
		semrelease(&work.markDoneSema)
		return
	}

	// forEachP needs worldsema to execute, and we'll need it to
	// stop the world later, so acquire worldsema now.
	// forEachP需要worldsema来执行，而且我们稍后需要它来停止世界，
	// 所以现在就获取worldsema
	semacquire(&worldsema)

	// Prevent weak->strong conversions from generating additional
	// GC work. forEachP will guarantee that it is observed globally.
	// 防止弱引用到强引用的转换产生额外的GC工作。
	// forEachP将确保这个设置被全局观察到
	work.strongFromWeak.block = true

	// Flush all local buffers and collect flushedWork flags.
	// 刷新所有本地缓冲区并收集flushedWork标志
	gcMarkDoneFlushed = 0
	forEachP(waitReasonGCMarkTermination, func(pp *p) {
		// Flush the write barrier buffer, since this may add
		// work to the gcWork.
		// 刷新写屏障缓冲区，因为这可能会向gcWork添加工作
		wbBufFlush1(pp)

		// Flush the gcWork, since this may create global work
		// and set the flushedWork flag.
		//
		// TODO(austin): Break up these workbufs to
		// better distribute work.
		// 刷新gcWork，因为这可能会创建全局工作并设置flushedWork标志
		// TODO(austin): 将这些workbufs拆分以更好地分配工作
		pp.gcw.dispose()
		// Collect the flushedWork flag.
		// 收集flushedWork标志
		if pp.gcw.flushedWork {
			atomic.Xadd(&gcMarkDoneFlushed, 1)
			pp.gcw.flushedWork = false
		}
	})

	if gcMarkDoneFlushed != 0 {
		// More grey objects were discovered since the
		// previous termination check, so there may be more
		// work to do. Keep going. It's possible the
		// transition condition became true again during the
		// ragged barrier, so re-check it.
		// 自上次终止检查以来发现了更多的灰色对象，所以可能还有更多工作要做。
		// 继续执行。在ragged barrier期间，转换条件可能再次变为true，所以重新检查它。
		semrelease(&worldsema)
		goto top
	}

	// For debugging/testing.
	// 用于调试/测试
	for gcDebugMarkDone.spinAfterRaggedBarrier.Load() {
	}

	// There was no global work, no local work, and no Ps
	// communicated work since we took markDoneSema. Therefore
	// there are no grey objects and no more objects can be
	// shaded. Transition to mark termination.
	// 自从我们获取markDoneSema以来，没有全局工作、没有本地工作，也没有P之间的工作通信。
	// 因此没有灰色对象，也不会有更多对象被标记。转换到标记终止阶段。
	now := nanotime()
	work.tMarkTerm = now
	getg().m.preemptoff = "gcing"
	var stw worldStop
	systemstack(func() {
		stw = stopTheWorldWithSema(stwGCMarkTerm)
	})
	// The gcphase is _GCmark, it will transition to _GCmarktermination
	// below. The important thing is that the wb remains active until
	// all marking is complete. This includes writes made by the GC.
	// gcphase当前是_GCmark，它将在下面转换到_GCmarktermination。
	// 重要的是写屏障(wb)在标记完成之前保持活跃。
	// 这包括GC执行的写入操作。

	// Accumulate fine-grained stopping time.
	// 累积细粒度的停止时间
	work.cpuStats.accumulateGCPauseTime(stw.stoppingCPUTime, 1)

	// There is sometimes work left over when we enter mark termination due
	// to write barriers performed after the completion barrier above.
	// Detect this and resume concurrent mark. This is obviously
	// unfortunate.
	//
	// See issue #27993 for details.
	//
	// Switch to the system stack to call wbBufFlush1, though in this case
	// it doesn't matter because we're non-preemptible anyway.
	// 有时在进入标记终止阶段时，由于完成屏障之后执行的写屏障操作，会遗留一些工作。
	// 检测这种情况并恢复并发标记。这显然是不幸的。
	//
	// 详情请参见issue #27993。
	//
	// 切换到系统栈来调用wbBufFlush1，虽然在这种情况下这并不重要，因为我们本来就是不可抢占的。
	restart := false
	systemstack(func() {
		for _, p := range allp {
			wbBufFlush1(p)
			if !p.gcw.empty() {
				restart = true
				break
			}
		}
	})
	if restart {
		gcDebugMarkDone.restartedDueTo27993 = true

		getg().m.preemptoff = ""
		systemstack(func() {
			// Accumulate the time we were stopped before we had to start again.
			// 累积我们在不得不重新开始之前停止的时间
			work.cpuStats.accumulateGCPauseTime(nanotime()-stw.finishedStopping, work.maxprocs)

			// Start the world again.
			// 重新启动世界
			now := startTheWorldWithSema(0, stw)
			work.pauseNS += now - stw.startedStopping
		})
		semrelease(&worldsema)
		goto top
	}

	// 计算GC开始时的栈大小
	gcComputeStartingStackSize()

	// Disable assists and background workers. We must do
	// this before waking blocked assists.
	// 禁用辅助标记和后台工作器。我们必须在唤醒被阻塞的辅助标记之前完成这个操作。
	atomic.Store(&gcBlackenEnabled, 0)

	// Notify the CPU limiter that GC assists will now cease.
	// 通知CPU限制器GC辅助标记现在将停止
	gcCPULimiter.startGCTransition(false, now)

	// Wake all blocked assists. These will run when we
	// start the world again.
	// 唤醒所有被阻塞的辅助标记。这些将在我们重新启动世界时运行。
	gcWakeAllAssists()

	// Wake all blocked weak->strong conversions. These will run
	// when we start the world again.
	// 唤醒所有被阻塞的弱引用到强引用的转换。这些将在我们重新启动世界时运行。
	work.strongFromWeak.block = false
	gcWakeAllStrongFromWeak()

	// Likewise, release the transition lock. Blocked
	// workers and assists will run when we start the
	// world again.
	// 同样，释放转换锁。被阻塞的工作器和辅助标记将在我们重新启动世界时运行。
	semrelease(&work.markDoneSema)

	// In STW mode, re-enable user goroutines. These will be
	// queued to run after we start the world.
	// 在STW模式下，重新启用用户goroutine。这些将在我们重新启动世界后排队运行。
	schedEnableUser(true)

	// endCycle depends on all gcWork cache stats being flushed.
	// The termination algorithm above ensured that up to
	// allocations since the ragged barrier.
	// endCycle依赖于所有gcWork缓存统计信息被刷新。
	// 上面的终止算法确保了从粗糙屏障以来的所有分配都被处理。
	gcController.endCycle(now, int(gomaxprocs), work.userForced)

	// Perform mark termination. This will restart the world.
	// 执行标记终止。这将重新启动世界。
	gcMarkTermination(stw)
}

// World must be stopped and mark assists and background workers must be
// disabled.
// 世界必须停止，并且标记辅助和后台工作器必须被禁用
func gcMarkTermination(stw worldStop) {
	// Start marktermination (write barrier remains enabled for now).
	// 开始标记终止阶段（写屏障暂时保持启用状态）
	setGCPhase(_GCmarktermination)

	// 记录当前堆的活跃内存大小
	work.heap1 = gcController.heapLive.Load()
	// 记录开始时间
	startTime := nanotime()

	// 获取当前M（机器）
	mp := acquirem()
	// 设置抢占标志为"gcing"，表示正在进行GC
	mp.preemptoff = "gcing"
	// 设置回溯级别为2，用于调试
	mp.traceback = 2
	// 获取当前G（goroutine）
	curgp := mp.curg
	// N.B. The execution tracer is not aware of this status
	// transition and handles it specially based on the
	// wait reason.
	// 注意：执行跟踪器不知道这个状态转换，它基于等待原因特殊处理
	// 将当前G的状态从运行中(_Grunning)转换为等待GC(_Gwaiting)
	casGToWaitingForGC(curgp, _Grunning, waitReasonGarbageCollection)

	// Run gc on the g0 stack. We do this so that the g stack
	// we're currently running on will no longer change. Cuts
	// the root set down a bit (g0 stacks are not scanned, and
	// we don't need to scan gc's internal state).  We also
	// need to switch to g0 so we can shrink the stack.
	// 在g0栈上运行GC。这样做是为了确保我们当前运行的g栈不会改变。
	// 这样可以减少根集合的大小（g0栈不会被扫描，我们也不需要扫描GC的内部状态）。
	// 同时，我们需要切换到g0以便可以收缩栈。
	systemstack(func() {
		gcMark(startTime)
		// Must return immediately.
		// The outer function's stack may have moved
		// during gcMark (it shrinks stacks, including the
		// outer function's stack), so we must not refer
		// to any of its variables. Return back to the
		// non-system stack to pick up the new addresses
		// before continuing.
		// 必须立即返回。
		// 外层函数的栈可能在gcMark期间发生移动（它会收缩栈，包括外层函数的栈），
		// 所以我们不能引用它的任何变量。在继续之前，需要返回到非系统栈以获取新的地址。
	})

	// 声明一个布尔变量，用于标记是否完成了STW（Stop-The-World）清扫
	var stwSwept bool
	systemstack(func() {
		// 记录标记阶段结束时堆的活跃内存大小
		work.heap2 = work.bytesMarked
		if debug.gccheckmark > 0 {
			// Run a full non-parallel, stop-the-world
			// mark using checkmark bits, to check that we
			// didn't forget to mark anything during the
			// concurrent mark process.
			// 使用检查标记位运行一个完整的非并行、停止世界的标记过程，
			// 以检查我们在并发标记过程中是否遗漏了任何对象的标记
			startCheckmarks()
			// 重置标记状态
			gcResetMarkState()
			// 获取当前P的GC工作队列
			gcw := &getg().m.p.ptr().gcw
			// 执行标记过程，直到队列为空
			gcDrain(gcw, 0)
			// 刷新写屏障缓冲区
			wbBufFlush1(getg().m.p.ptr())
			// 清理GC工作队列
			gcw.dispose()
			// 结束检查标记过程
			endCheckmarks()
		}

		// marking is complete so we can turn the write barrier off
		// 标记阶段已完成，可以关闭写屏障
		setGCPhase(_GCoff)
		// 执行清扫阶段，并记录是否完成
		stwSwept = gcSweep(work.mode)
	})

	// Reset traceback flag to 0
	mp.traceback = 0
	// 重置回溯标志为0，表示GC完成后不再需要回溯

	// Change goroutine status from waiting to running
	casgstatus(curgp, _Gwaiting, _Grunning)
	// 将当前goroutine的状态从等待(_Gwaiting)改为运行(_Grunning)

	// Acquire trace for GC completion event
	trace := traceAcquire()
	if trace.ok() {
		trace.GCDone()
		traceRelease(trace)
	}
	// 获取追踪器并记录GC完成事件
	// 如果追踪器可用，则记录GC完成事件并释放追踪器

	// all done
	mp.preemptoff = ""
	// 清除抢占标志，表示GC完成后可以正常调度

	// Verify GC phase is off
	if gcphase != _GCoff {
		throw("gc done but gcphase != _GCoff")
	}
	// 验证GC阶段是否已关闭
	// 如果GC阶段不是_GCoff，则抛出异常

	// Record heapInUse for scavenger.
	memstats.lastHeapInUse = gcController.heapInUse.load()
	// 记录当前堆使用量，供内存清理器使用
	// 从GC控制器加载当前堆使用量并保存到内存统计信息中

	// Update GC trigger and pacing, as well as downstream consumers
	// of this pacing information, for the next cycle.
	systemstack(gcControllerCommit)
	// 更新GC触发条件和节奏控制参数，为下一个GC周期做准备
	// 在系统栈上执行GC控制器的提交操作，更新相关参数

	// Update timing memstats
	// 更新内存统计信息中的时间相关数据
	now := nanotime()                                       // 获取当前纳秒级时间戳
	sec, nsec, _ := time_now()                              // 获取当前Unix时间戳的秒和纳秒部分
	unixNow := sec*1e9 + int64(nsec)                        // 将秒和纳秒转换为纳秒级的Unix时间戳
	work.pauseNS += now - stw.startedStopping               // 累加GC暂停时间
	work.tEnd = now                                         // 记录GC结束时间
	atomic.Store64(&memstats.last_gc_unix, uint64(unixNow)) // must be Unix time to make sense to user
	// 存储最后一次GC的Unix时间戳，使用原子操作确保线程安全
	atomic.Store64(&memstats.last_gc_nanotime, uint64(now)) // monotonic time for us
	// 存储最后一次GC的单调时间戳，用于内部计算
	memstats.pause_ns[memstats.numgc%uint32(len(memstats.pause_ns))] = uint64(work.pauseNS)
	// 在循环数组中记录本次GC的暂停时间
	memstats.pause_end[memstats.numgc%uint32(len(memstats.pause_end))] = uint64(unixNow)
	// 在循环数组中记录本次GC的结束时间
	memstats.pause_total_ns += uint64(work.pauseNS)
	// 累加所有GC暂停的总时间

	// Accumulate CPU stats.
	//
	// Use maxprocs instead of stwprocs for GC pause time because the total time
	// computed in the CPU stats is based on maxprocs, and we want them to be
	// comparable.
	//
	// Pass gcMarkPhase=true to accumulate so we can get all the latest GC CPU stats
	// in there too.
	// 累积CPU统计信息
	// 使用maxprocs而不是stwprocs来计算GC暂停时间，因为CPU统计信息中的总时间是基于maxprocs计算的
	// 我们希望这些时间是可比较的
	// 传入gcMarkPhase=true进行累积，这样我们可以获取所有最新的GC CPU统计信息
	work.cpuStats.accumulateGCPauseTime(now-stw.finishedStopping, work.maxprocs)
	work.cpuStats.accumulate(now, true)

	// Compute overall GC CPU utilization.
	// Omit idle marking time from the overall utilization here since it's "free".
	// 计算总体GC CPU利用率
	// 这里从总体利用率中省略空闲标记时间，因为它是"免费的"
	memstats.gc_cpu_fraction = float64(work.cpuStats.GCTotalTime-work.cpuStats.GCIdleTime) / float64(work.cpuStats.TotalTime)

	// Reset assist time and background time stats.
	//
	// Do this now, instead of at the start of the next GC cycle, because
	// these two may keep accumulating even if the GC is not active.
	// 重置辅助时间和后台时间统计
	// 现在执行重置，而不是在下一个GC周期开始时，因为即使GC不活跃
	// 这两个统计值也可能继续累积
	scavenge.assistTime.Store(0)
	scavenge.backgroundTime.Store(0)

	// Reset idle time stat.
	// 重置空闲时间统计
	sched.idleTime.Store(0)

	if work.userForced {
		memstats.numforcedgc++
	}

	// Bump GC cycle count and wake goroutines waiting on sweep.
	// 增加GC周期计数并唤醒等待清扫的goroutines
	lock(&work.sweepWaiters.lock)
	memstats.numgc++                     // 增加GC计数器
	injectglist(&work.sweepWaiters.list) // 将等待清扫的goroutines注入到调度器中
	unlock(&work.sweepWaiters.lock)

	// Increment the scavenge generation now.
	//
	// This moment represents peak heap in use because we're
	// about to start sweeping.
	// 增加清扫代计数
	// 这个时刻代表堆使用量的峰值，因为即将开始清扫
	mheap_.pages.scav.index.nextGen()

	// Release the CPU limiter.
	// 释放CPU限制器
	gcCPULimiter.finishGCTransition(now)

	// Finish the current heap profiling cycle and start a new
	// heap profiling cycle. We do this before starting the world
	// so events don't leak into the wrong cycle.
	// 结束当前堆分析周期并开始新的堆分析周期
	// 在启动世界之前执行此操作，以确保事件不会泄漏到错误的周期中
	mProf_NextCycle()

	// There may be stale spans in mcaches that need to be swept.
	// Those aren't tracked in any sweep lists, so we need to
	// count them against sweep completion until we ensure all
	// those spans have been forced out.
	//
	// If gcSweep fully swept the heap (for example if the sweep
	// is not concurrent due to a GODEBUG setting), then we expect
	// the sweepLocker to be invalid, since sweeping is done.
	//
	// N.B. Below we might duplicate some work from gcSweep; this is
	// fine as all that work is idempotent within a GC cycle, and
	// we're still holding worldsema so a new cycle can't start.
	// 在mcaches中可能存在需要清扫的过期spans
	// 这些spans没有被任何清扫列表跟踪，所以我们需要
	// 将它们计入清扫完成度，直到确保所有这些spans都被强制清理出去
	//
	// 如果gcSweep完全清扫了堆（例如由于GODEBUG设置导致清扫不是并发的），
	// 那么我们希望sweepLocker是无效的，因为清扫已经完成
	//
	// 注意：下面我们可能会重复gcSweep中的一些工作；这是没问题的，
	// 因为所有这些工作在GC周期内都是幂等的，而且我们仍然持有worldsema，
	// 所以新的周期无法开始
	sl := sweep.active.begin()
	if !stwSwept && !sl.valid {
		throw("failed to set sweep barrier")
	} else if stwSwept && sl.valid {
		throw("non-concurrent sweep failed to drain all sweep queues")
	}

	systemstack(func() {
		// The memstats updated above must be updated with the world
		// stopped to ensure consistency of some values, such as
		// sched.idleTime and sched.totaltime. memstats also include
		// the pause time (work,pauseNS), forcing computation of the
		// total pause time before the pause actually ends.
		//
		// Here we reuse the same now for start the world so that the
		// time added to /sched/pauses/total/gc:seconds will be
		// consistent with the value in memstats.

		// 上面更新的内存统计信息必须在世界停止时更新，
		// 以确保某些值的一致性，比如sched.idleTime和sched.totaltime。
		// 内存统计信息还包括暂停时间(work.pauseNS)，
		// 这迫使我们在暂停实际结束之前计算总暂停时间。
		//
		// 这里我们重用相同的now来启动世界，这样添加到
		// /sched/pauses/total/gc:seconds的时间将与memstats中的值保持一致。
		startTheWorldWithSema(now, stw)
	})

	// Flush the heap profile so we can start a new cycle next GC.
	// This is relatively expensive, so we don't do it with the
	// world stopped.
	// 刷新堆内存分析数据，以便在下次GC时开始新的周期
	// 这个操作相对耗时，所以我们不在世界停止时执行它
	mProf_Flush()

	// Prepare workbufs for freeing by the sweeper. We do this
	// asynchronously because it can take non-trivial time.
	// 为清扫器准备要释放的工作缓冲区
	// 我们异步执行这个操作，因为它可能需要相当长的时间
	prepareFreeWorkbufs()

	// Free stack spans. This must be done between GC cycles.
	// 释放栈span。这个操作必须在GC周期之间完成
	systemstack(freeStackSpans)

	// Ensure all mcaches are flushed. Each P will flush its own
	// mcache before allocating, but idle Ps may not. Since this
	// is necessary to sweep all spans, we need to ensure all
	// mcaches are flushed before we start the next GC cycle.
	//
	// While we're here, flush the page cache for idle Ps to avoid
	// having pages get stuck on them. These pages are hidden from
	// the scavenger, so in small idle heaps a significant amount
	// of additional memory might be held onto.
	//
	// Also, flush the pinner cache, to avoid leaking that memory
	// indefinitely.
	// 确保所有mcache都被刷新。每个P在分配前都会刷新自己的mcache，
	// 但空闲的P可能不会这样做。由于这是清扫所有span所必需的，
	// 我们需要确保在开始下一个GC周期之前所有mcache都被刷新。
	//
	// 同时，刷新空闲P的页面缓存，以避免页面卡在它们上面。
	// 这些页面对scavenger是隐藏的，所以在小的空闲堆中可能会
	// 持有大量额外的内存。
	//
	// 另外，刷新pinner缓存，以避免无限期地泄漏该内存。
	forEachP(waitReasonFlushProcCaches, func(pp *p) {
		// 准备清扫该P的mcache
		pp.mcache.prepareForSweep()
		if pp.status == _Pidle {
			// 如果是空闲的P，在系统栈上执行页面缓存刷新
			systemstack(func() {
				lock(&mheap_.lock)
				// 将页面缓存刷新回堆的页面池
				pp.pcache.flush(&mheap_.pages)
				unlock(&mheap_.lock)
			})
		}
		// 清空pinner缓存
		pp.pinnerCache = nil
	})
	if sl.valid {
		// Now that we've swept stale spans in mcaches, they don't
		// count against unswept spans.
		//
		// Note: this sweepLocker may not be valid if sweeping had
		// already completed during the STW. See the corresponding
		// begin() call that produced sl.
		// 现在我们已经清扫了mcache中的过期span，它们不再计入未清扫的span。
		//
		// 注意：如果在STW期间清扫已经完成，这个sweepLocker可能无效。
		// 参见产生sl的相应begin()调用。
		sweep.active.end(sl)
	}

	// Print gctrace before dropping worldsema. As soon as we drop
	// worldsema another cycle could start and smash the stats
	// we're trying to print.
	// 在释放worldsema之前打印GC跟踪信息。一旦我们释放了worldsema，
	// 另一个GC周期可能会开始并破坏我们正在尝试打印的统计信息。
	if debug.gctrace > 0 {
		// 计算GC CPU使用率百分比
		util := int(memstats.gc_cpu_fraction * 100)

		// 创建一个缓冲区用于格式化数字
		var sbuf [24]byte
		printlock()
		// 打印GC编号和开始时间
		print("gc ", memstats.numgc,
			" @", string(itoaDiv(sbuf[:], uint64(work.tSweepTerm-runtimeInitTime)/1e6, 3)), "s ",
			util, "%: ")
		prev := work.tSweepTerm
		// 打印各个GC阶段的时间
		for i, ns := range []int64{work.tMark, work.tMarkTerm, work.tEnd} {
			if i != 0 {
				print("+")
			}
			print(string(fmtNSAsMS(sbuf[:], uint64(ns-prev))))
			prev = ns
		}
		print(" ms clock, ")
		// 打印CPU时间统计，包括STW时间、辅助标记时间、专用标记时间等
		for i, ns := range []int64{
			int64(work.stwprocs) * (work.tMark - work.tSweepTerm),                          // STW时间
			gcController.assistTime.Load(),                                                 // 辅助标记时间
			gcController.dedicatedMarkTime.Load() + gcController.fractionalMarkTime.Load(), // 专用标记时间
			gcController.idleMarkTime.Load(),                                               // 空闲标记时间
			int64(work.stwprocs) * (work.tEnd - work.tMarkTerm),                            // 最终STW时间
		} {
			if i == 2 || i == 3 {
				// 用/分隔标记时间组件
				print("/")
			} else if i != 0 {
				print("+")
			}
			print(string(fmtNSAsMS(sbuf[:], uint64(ns))))
		}
		// 打印内存统计信息，包括堆大小变化、目标堆大小、栈扫描大小等
		print(" ms cpu, ",
			work.heap0>>20, "->", work.heap1>>20, "->", work.heap2>>20, " MB, ", // 堆大小变化
			gcController.lastHeapGoal>>20, " MB goal, ", // 目标堆大小
			gcController.lastStackScan.Load()>>20, " MB stacks, ", // 栈扫描大小
			gcController.globalsScan.Load()>>20, " MB globals, ", // 全局变量扫描大小
			work.maxprocs, " P") // P的数量
		if work.userForced {
			print(" (forced)") // 如果是用户强制触发的GC，打印(forced)
		}
		print("\n")
		printunlock()
	}

	// Set any arena chunks that were deferred to fault.
	// 设置所有被延迟到fault状态的arena块
	lock(&userArenaState.lock)
	faultList := userArenaState.fault
	userArenaState.fault = nil
	unlock(&userArenaState.lock)
	for _, lc := range faultList {
		lc.mspan.setUserArenaChunkToFault()
	}

	// Enable huge pages on some metadata if we cross a heap threshold.
	// 如果堆大小超过阈值，为某些元数据启用大页
	if gcController.heapGoal() > minHeapForMetadataHugePages {
		systemstack(func() {
			mheap_.enableMetadataHugePages()
		})
	}

	semrelease(&worldsema)
	semrelease(&gcsema)
	// Careful: another GC cycle may start now.
	// 注意：此时可能会开始另一个GC周期

	releasem(mp)
	mp = nil

	// now that gc is done, kick off finalizer thread if needed
	// 现在GC已完成，如果需要的话启动finalizer线程
	if !concurrentSweep {
		// give the queued finalizers, if any, a chance to run
		// 让队列中的finalizers有机会运行
		Gosched()
	}
}

// gcBgMarkStartWorkers prepares background mark worker goroutines. These
// goroutines will not run until the mark phase, but they must be started while
// the work is not stopped and from a regular G stack. The caller must hold
// worldsema.
// gcBgMarkStartWorkers 准备后台标记工作协程。这些协程在标记阶段之前不会运行，
// 但它们必须在工作未停止时从常规G栈启动。调用者必须持有worldsema。
func gcBgMarkStartWorkers() {
	// Background marking is performed by per-P G's. Ensure that each P has
	// a background GC G.
	//
	// Worker Gs don't exit if gomaxprocs is reduced. If it is raised
	// again, we can reuse the old workers; no need to create new workers.
	// 后台标记由每个P的G执行。确保每个P都有一个后台GC G。
	// 如果gomaxprocs减少，工作G不会退出。如果再次增加，
	// 我们可以重用旧的工作G；不需要创建新的工作G。
	if gcBgMarkWorkerCount >= gomaxprocs {
		return
	}

	// Increment mp.locks when allocating. We are called within gcStart,
	// and thus must not trigger another gcStart via an allocation. gcStart
	// bails when allocating with locks held, so simulate that for these
	// allocations.
	//
	// TODO(prattmic): cleanup gcStart to use a more explicit "in gcStart"
	// check for bailing.
	// 分配时增加mp.locks。我们在gcStart中被调用，
	// 因此不能通过分配触发另一个gcStart。gcStart在持有锁时分配会退出，
	// 所以为这些分配模拟这种情况。
	mp := acquirem()
	ready := make(chan struct{}, 1)
	releasem(mp)

	for gcBgMarkWorkerCount < gomaxprocs {
		mp := acquirem() // See above, we allocate a closure here.
		// 见上文，我们在这里分配一个闭包
		go gcBgMarkWorker(ready)
		releasem(mp)

		// N.B. we intentionally wait on each goroutine individually
		// rather than starting all in a batch and then waiting once
		// afterwards. By running one goroutine at a time, we can take
		// advantage of runnext to bounce back and forth between
		// workers and this goroutine. In an overloaded application,
		// this can reduce GC start latency by prioritizing these
		// goroutines rather than waiting on the end of the run queue.
		// 注意：我们有意地等待每个goroutine单独完成，
		// 而不是批量启动所有goroutine然后一次性等待。
		// 通过一次运行一个goroutine，我们可以利用runnext在
		// 工作goroutine和这个goroutine之间来回切换。
		// 在过载的应用程序中，这可以通过优先处理这些goroutine
		// 而不是等待运行队列末尾来减少GC启动延迟。
		<-ready
		// The worker is now guaranteed to be added to the pool before
		// its P's next findRunnableGCWorker.
		// 现在可以保证工作goroutine在其P的下一次findRunnableGCWorker之前被添加到池中

		gcBgMarkWorkerCount++
	}
}

// gcBgMarkPrepare sets up state for background marking.
// Mutator assists must not yet be enabled.
// gcBgMarkPrepare 设置后台标记的状态。
// 此时还不能启用 Mutator 辅助标记。
func gcBgMarkPrepare() {
	// Background marking will stop when the work queues are empty
	// and there are no more workers (note that, since this is
	// concurrent, this may be a transient state, but mark
	// termination will clean it up). Between background workers
	// and assists, we don't really know how many workers there
	// will be, so we pretend to have an arbitrarily large number
	// of workers, almost all of which are "waiting". While a
	// worker is working it decrements nwait. If nproc == nwait,
	// there are no workers.
	// 当工作队列为空且没有更多工作线程时，后台标记将停止
	//（注意：由于这是并发的，这可能是一个临时状态，但标记终止会清理它）。
	// 在后台工作线程和辅助标记之间，我们实际上并不知道会有多少工作线程，
	// 所以我们假装有任意数量的工作线程，其中几乎都是"等待"状态。
	// 当工作线程工作时，它会减少 nwait。如果 nproc == nwait，
	// 则表示没有工作线程在工作。
	work.nproc = ^uint32(0) // 设置一个极大值，表示工作线程总数
	work.nwait = ^uint32(0) // 设置一个极大值，表示等待中的工作线程数
}

// gcBgMarkWorkerNode is an entry in the gcBgMarkWorkerPool. It points to a single
// gcBgMarkWorker goroutine.
// gcBgMarkWorkerNode 是 gcBgMarkWorkerPool 中的一个条目。它指向一个单独的
// gcBgMarkWorker goroutine。
type gcBgMarkWorkerNode struct {
	// Unused workers are managed in a lock-free stack. This field must be first.
	// 未使用的工作线程在无锁栈中管理。这个字段必须是第一个。
	node lfnode

	// The g of this worker.
	// 这个工作线程对应的 goroutine
	gp guintptr

	// Release this m on park. This is used to communicate with the unlock
	// function, which cannot access the G's stack. It is unused outside of
	// gcBgMarkWorker().
	// 在 park 时释放这个 m。这用于与 unlock 函数通信，
	// 因为 unlock 函数无法访问 G 的栈。这个字段在 gcBgMarkWorker() 之外不会被使用。
	m muintptr
}

func gcBgMarkWorker(ready chan struct{}) {
	gp := getg()

	// We pass node to a gopark unlock function, so it can't be on
	// the stack (see gopark). Prevent deadlock from recursively
	// starting GC by disabling preemption.
	// 我们将 node 传递给 gopark unlock 函数，所以它不能在栈上（参见 gopark）。
	// 通过禁用抢占来防止递归启动 GC 导致的死锁。
	gp.m.preemptoff = "GC worker init"
	node := new(gcBgMarkWorkerNode)
	gp.m.preemptoff = ""

	node.gp.set(gp)

	node.m.set(acquirem())

	ready <- struct{}{}
	// After this point, the background mark worker is generally scheduled
	// cooperatively by gcController.findRunnableGCWorker. While performing
	// work on the P, preemption is disabled because we are working on
	// P-local work buffers. When the preempt flag is set, this puts itself
	// into _Gwaiting to be woken up by gcController.findRunnableGCWorker
	// at the appropriate time.
	//
	// When preemption is enabled (e.g., while in gcMarkDone), this worker
	// may be preempted and schedule as a _Grunnable G from a runq. That is
	// fine; it will eventually gopark again for further scheduling via
	// findRunnableGCWorker.
	//
	// Since we disable preemption before notifying ready, we guarantee that
	// this G will be in the worker pool for the next findRunnableGCWorker.
	// This isn't strictly necessary, but it reduces latency between
	// _GCmark starting and the workers starting.
	// 从这一点开始，后台标记工作线程通常由 gcController.findRunnableGCWorker 协作调度。
	// 在 P 上执行工作时，由于我们在处理 P 本地的工作缓冲区，所以禁用抢占。
	// 当设置抢占标志时，它会将自身置于 _Gwaiting 状态，等待 gcController.findRunnableGCWorker
	// 在适当的时机唤醒它。
	//
	// 当启用抢占时（例如，在 gcMarkDone 期间），这个工作线程可能会被抢占，
	// 并作为 _Grunnable G 从运行队列中调度。这是没问题的；它最终会再次通过
	// findRunnableGCWorker 进行 gopark 以进行进一步的调度。
	//
	// 由于我们在通知 ready 之前禁用了抢占，我们保证这个 G 将在下一个
	// findRunnableGCWorker 调用时在工作线程池中。这不是严格必需的，
	// 但它减少了 _GCmark 启动和工作线程启动之间的延迟。

	for {
		// Go to sleep until woken by
		// gcController.findRunnableGCWorker.
		// 进入睡眠状态，直到被 gcController.findRunnableGCWorker 唤醒
		gopark(func(g *g, nodep unsafe.Pointer) bool {
			node := (*gcBgMarkWorkerNode)(nodep)

			if mp := node.m.ptr(); mp != nil {
				// The worker G is no longer running; release
				// the M.
				//
				// N.B. it is _safe_ to release the M as soon
				// as we are no longer performing P-local mark
				// work.
				//
				// However, since we cooperatively stop work
				// when gp.preempt is set, if we releasem in
				// the loop then the following call to gopark
				// would immediately preempt the G. This is
				// also safe, but inefficient: the G must
				// schedule again only to enter gopark and park
				// again. Thus, we defer the release until
				// after parking the G.
				// 工作 G 不再运行；释放 M
				//
				// 注意：一旦我们不再执行 P 本地的标记工作，立即释放 M 是安全的
				//
				// 但是，由于我们在 gp.preempt 被设置时会协作式地停止工作，
				// 如果我们在循环中释放 M，那么接下来的 gopark 调用会立即抢占 G。
				// 这也是安全的，但效率不高：G 必须重新调度，只是为了再次进入 gopark 并再次休眠。
				// 因此，我们将释放操作推迟到 G 休眠之后。
				releasem(mp)
			}

			// Release this G to the pool.
			// 将这个 G 释放回池中
			gcBgMarkWorkerPool.push(&node.node)
			// Note that at this point, the G may immediately be
			// rescheduled and may be running.
			// 注意：此时，G 可能会立即被重新调度并运行
			return true
		}, unsafe.Pointer(node), waitReasonGCWorkerIdle, traceBlockSystemGoroutine, 0)

		// Preemption must not occur here, or another G might see
		// p.gcMarkWorkerMode.

		// Disable preemption so we can use the gcw. If the
		// scheduler wants to preempt us, we'll stop draining,
		// dispose the gcw, and then preempt.
		// 这里不能发生抢占，否则其他 G 可能会看到 p.gcMarkWorkerMode

		// 禁用抢占以便我们可以使用 gcw。如果调度器想要抢占我们，
		// 我们会停止标记工作，释放 gcw，然后允许抢占
		node.m.set(acquirem())
		pp := gp.m.p.ptr() // P can't change with preemption disabled.
		// 获取当前 P，由于禁用了抢占，P 不会改变

		if gcBlackenEnabled == 0 {
			println("worker mode", pp.gcMarkWorkerMode)
			throw("gcBgMarkWorker: blackening not enabled")
		}
		// 如果标记阶段未启用，打印工作模式并抛出异常

		if pp.gcMarkWorkerMode == gcMarkWorkerNotWorker {
			throw("gcBgMarkWorker: mode not set")
		}
		// 如果工作模式未设置，抛出异常

		startTime := nanotime()
		pp.gcMarkWorkerStartTime = startTime
		var trackLimiterEvent bool
		if pp.gcMarkWorkerMode == gcMarkWorkerIdleMode {
			trackLimiterEvent = pp.limiterEvent.start(limiterEventIdleMarkWork, startTime)
		}
		// 记录开始时间，如果是在空闲模式下工作，则启动限制器事件跟踪

		// Decrease the number of waiting workers by 1
		// 将等待中的工作协程数量减1
		decnwait := atomic.Xadd(&work.nwait, -1)
		// Check if the number of waiting workers equals the total number of processors
		// 检查等待中的工作协程数量是否等于处理器总数
		if decnwait == work.nproc {
			println("runtime: work.nwait=", decnwait, "work.nproc=", work.nproc)
			// If true, throw an error as this indicates an invalid state
			// 如果是，则抛出错误，因为这表示一个无效的状态
			throw("work.nwait was > work.nproc")
		}

		systemstack(func() {
			// Mark our goroutine preemptible so its stack
			// can be scanned. This lets two mark workers
			// scan each other (otherwise, they would
			// deadlock). We must not modify anything on
			// the G stack. However, stack shrinking is
			// disabled for mark workers, so it is safe to
			// read from the G stack.
			//
			// N.B. The execution tracer is not aware of this status
			// transition and handles it specially based on the
			// wait reason.
			// 将我们的 goroutine 标记为可抢占，这样它的栈就可以被扫描。
			// 这允许两个标记工作协程互相扫描（否则它们会死锁）。
			// 我们不能修改 G 栈上的任何内容。但是，对于标记工作协程，
			// 栈收缩是禁用的，所以从 G 栈读取是安全的。
			//
			// 注意：执行跟踪器不知道这个状态转换，
			// 它基于等待原因特殊处理这种情况。
			casGToWaitingForGC(gp, _Grunning, waitReasonGCWorkerActive)
			switch pp.gcMarkWorkerMode {
			default:
				throw("gcBgMarkWorker: unexpected gcMarkWorkerMode")
			case gcMarkWorkerDedicatedMode:
				// 专用模式：执行标记工作，允许抢占
				gcDrainMarkWorkerDedicated(&pp.gcw, true)
				if gp.preempt {
					// We were preempted. This is
					// a useful signal to kick
					// everything out of the run
					// queue so it can run
					// somewhere else.
					// 我们被抢占了。这是一个有用的信号，
					// 可以将运行队列中的所有内容踢出，
					// 让它们在其他地方运行。
					if drainQ, n := runqdrain(pp); n > 0 {
						lock(&sched.lock)
						globrunqputbatch(&drainQ, int32(n))
						unlock(&sched.lock)
					}
				}
				// Go back to draining, this time
				// without preemption.
				// 继续执行标记工作，这次不允许抢占
				gcDrainMarkWorkerDedicated(&pp.gcw, false)
			case gcMarkWorkerFractionalMode:
				// 部分模式：执行部分标记工作
				gcDrainMarkWorkerFractional(&pp.gcw)
			case gcMarkWorkerIdleMode:
				// 空闲模式：在系统空闲时执行标记工作
				gcDrainMarkWorkerIdle(&pp.gcw)
			}
			// 将 goroutine 状态从等待改为运行
			casgstatus(gp, _Gwaiting, _Grunning)
		})

		// Account for time and mark us as stopped.
		// 计算时间并标记我们已停止
		now := nanotime()
		// Calculate the duration of this mark worker's execution
		// 计算这个标记工作协程的执行时间
		duration := now - startTime

		// Notify the GC controller that this worker has stopped
		// 通知 GC 控制器这个工作协程已经停止
		gcController.markWorkerStop(pp.gcMarkWorkerMode, duration)

		// If we're tracking limiter events, stop tracking this one
		// 如果正在跟踪限制器事件，则停止跟踪当前事件
		if trackLimiterEvent {
			pp.limiterEvent.stop(limiterEventIdleMarkWork, now)
		}

		// For fractional mode workers, accumulate their execution time
		// 对于部分模式的工作协程，累加它们的执行时间
		if pp.gcMarkWorkerMode == gcMarkWorkerFractionalMode {
			atomic.Xaddint64(&pp.gcFractionalMarkTime, duration)
		}

		// Was this the last worker and did we run out
		// of work?
		// 这是最后一个工作协程吗？我们是否已经完成了所有工作？
		incnwait := atomic.Xadd(&work.nwait, +1)
		if incnwait > work.nproc {
			println("runtime: p.gcMarkWorkerMode=", pp.gcMarkWorkerMode,
				"work.nwait=", incnwait, "work.nproc=", work.nproc)
			throw("work.nwait > work.nproc")
		}

		// We'll releasem after this point and thus this P may run
		// something else. We must clear the worker mode to avoid
		// attributing the mode to a different (non-worker) G in
		// traceGoStart.
		// 在这个点之后我们将释放 M，因此这个 P 可能会运行其他内容。
		// 我们必须清除工作模式，以避免在 traceGoStart 中将模式错误地
		// 归因于一个不同的（非工作）G。
		pp.gcMarkWorkerMode = gcMarkWorkerNotWorker

		// If this worker reached a background mark completion
		// point, signal the main GC goroutine.
		// 如果这个工作协程达到了后台标记的完成点，通知主 GC 协程。
		if incnwait == work.nproc && !gcMarkWorkAvailable(nil) {
			// We don't need the P-local buffers here, allow
			// preemption because we may schedule like a regular
			// goroutine in gcMarkDone (block on locks, etc).
			// 在这里我们不需要 P 本地缓冲区，允许抢占，因为我们在 gcMarkDone 中
			// 可能会像普通协程一样被调度（比如在锁上阻塞等）。
			releasem(node.m.ptr())
			node.m.set(nil)

			gcMarkDone()
		}
	}
}

// gcMarkWorkAvailable reports whether executing a mark worker
// on p is potentially useful. p may be nil, in which case it only
// checks the global sources of work.
// gcMarkWorkAvailable 报告在 p 上执行标记工作协程是否可能有用。
// p 可能为 nil，在这种情况下它只检查全局工作源。
func gcMarkWorkAvailable(p *p) bool {
	// 如果 p 不为空且其本地工作缓存不为空，说明有本地工作可做
	if p != nil && !p.gcw.empty() {
		return true
	}
	// 如果全局工作队列不为空，说明有全局工作可做
	if !work.full.empty() {
		return true // global work available
	}
	// 如果还有根扫描工作未完成，说明有根扫描工作可做
	if work.markrootNext < work.markrootJobs {
		return true // root scan work available
	}
	// 如果以上条件都不满足，说明没有工作可做
	return false
}

// gcMark runs the mark (or, for concurrent GC, mark termination)
// All gcWork caches must be empty.
// STW is in effect at this point.
// gcMark 执行标记阶段（对于并发 GC 来说，是标记终止阶段）
// 所有的 gcWork 缓存必须为空
// 此时处于 STW（Stop The World）状态
func gcMark(startTime int64) {
	// 确保当前处于标记终止阶段
	if gcphase != _GCmarktermination {
		throw("in gcMark expecting to see gcphase as _GCmarktermination")
	}
	// 记录标记阶段的开始时间
	work.tstart = startTime

	// Check that there's no marking work remaining.
	// 检查是否还有剩余的标记工作
	if work.full != 0 || work.markrootNext < work.markrootJobs {
		// 打印详细的调试信息，包括各种根对象的数量
		print("runtime: full=", hex(work.full), " next=", work.markrootNext, " jobs=", work.markrootJobs, " nDataRoots=", work.nDataRoots, " nBSSRoots=", work.nBSSRoots, " nSpanRoots=", work.nSpanRoots, " nStackRoots=", work.nStackRoots, "\n")
		panic("non-empty mark queue after concurrent mark")
	}

	if debug.gccheckmark > 0 {
		// This is expensive when there's a large number of
		// Gs, so only do it if checkmark is also enabled.
		// 当 G 的数量很大时，这个操作会很昂贵，
		// 所以只有在启用了 checkmark 时才执行
		gcMarkRootCheck()
	}

	// Drop allg snapshot. allgs may have grown, in which case
	// this is the only reference to the old backing store and
	// there's no need to keep it around.
	// 丢弃 allg 快照。allgs 可能已经增长，在这种情况下，
	// 这是对旧后备存储的唯一引用，不需要保留它
	work.stackRoots = nil

	// Clear out buffers and double-check that all gcWork caches
	// are empty. This should be ensured by gcMarkDone before we
	// enter mark termination.
	//
	// TODO: We could clear out buffers just before mark if this
	// has a non-negligible impact on STW time.
	// 清空缓冲区并再次检查所有 gcWork 缓存是否为空。
	// 这应该在进入标记终止阶段之前由 gcMarkDone 确保。
	//
	// TODO: 如果这对 STW 时间有显著影响，我们可以在标记之前清空缓冲区。
	for _, p := range allp {
		// The write barrier may have buffered pointers since
		// the gcMarkDone barrier. However, since the barrier
		// ensured all reachable objects were marked, all of
		// these must be pointers to black objects. Hence we
		// can just discard the write barrier buffer.
		// 自 gcMarkDone 屏障以来，写屏障可能已经缓冲了指针。
		// 但是，由于屏障确保了所有可达对象都被标记，
		// 这些指针必须都指向黑色对象。因此我们可以直接丢弃写屏障缓冲区。
		if debug.gccheckmark > 0 {
			// For debugging, flush the buffer and make
			// sure it really was all marked.
			// 用于调试，刷新缓冲区并确保所有内容确实都被标记了。
			wbBufFlush1(p)
		} else {
			p.wbBuf.reset()
		}

		gcw := &p.gcw
		if !gcw.empty() {
			printlock()
			print("runtime: P ", p.id, " flushedWork ", gcw.flushedWork)
			if gcw.wbuf1 == nil {
				print(" wbuf1=<nil>")
			} else {
				print(" wbuf1.n=", gcw.wbuf1.nobj)
			}
			if gcw.wbuf2 == nil {
				print(" wbuf2=<nil>")
			} else {
				print(" wbuf2.n=", gcw.wbuf2.nobj)
			}
			print("\n")
			throw("P has cached GC work at end of mark termination")
		}
		// There may still be cached empty buffers, which we
		// need to flush since we're going to free them. Also,
		// there may be non-zero stats because we allocated
		// black after the gcMarkDone barrier.
		// 可能仍然存在缓存的空缓冲区，我们需要刷新它们因为我们要释放它们。
		// 另外，由于我们在 gcMarkDone 屏障之后分配了黑色对象，
		// 所以可能存在非零的统计信息。
		gcw.dispose()
	}

	// Flush scanAlloc from each mcache since we're about to modify
	// heapScan directly. If we were to flush this later, then scanAlloc
	// might have incorrect information.
	//
	// Note that it's not important to retain this information; we know
	// exactly what heapScan is at this point via scanWork.
	// 清空每个 mcache 中的 scanAlloc，因为我们即将直接修改 heapScan。
	// 如果稍后再清空，那么 scanAlloc 可能会包含不正确的信息。
	//
	// 注意：保留这些信息并不重要；通过 scanWork 我们知道此时 heapScan 的确切值。
	for _, p := range allp {
		c := p.mcache
		if c == nil {
			continue
		}
		c.scanAlloc = 0 // 将每个 P 的 mcache 中的 scanAlloc 重置为 0
	}

	// Reset controller state.
	// 重置控制器状态
	gcController.resetLive(work.bytesMarked) // 根据已标记的字节数重置 GC 控制器的存活对象计数
}

// gcSweep must be called on the system stack because it acquires the heap
// lock. See mheap for details.
//
// Returns true if the heap was fully swept by this function.
//
// The world must be stopped.
//
// gcSweep 必须在系统栈上调用，因为它会获取堆锁。详见 mheap。
//
// 如果堆被此函数完全清扫，则返回 true。
//
// 调用此函数时，世界必须处于停止状态。
//
//go:systemstack
func gcSweep(mode gcMode) bool {
	// 确保世界已停止
	assertWorldStopped()

	// 确保当前处于 GC 关闭阶段
	if gcphase != _GCoff {
		throw("gcSweep being done but phase is not GCoff")
	}

	// 获取堆锁并初始化清扫状态
	lock(&mheap_.lock)
	mheap_.sweepgen += 2                  // 增加清扫代数
	sweep.active.reset()                  // 重置活动清扫状态
	mheap_.pagesSwept.Store(0)            // 重置已清扫页数
	mheap_.sweepArenas = mheap_.allArenas // 设置要清扫的 arena 列表
	mheap_.reclaimIndex.Store(0)          // 重置回收索引
	mheap_.reclaimCredit.Store(0)         // 重置回收信用
	unlock(&mheap_.lock)

	// 清除中央索引
	sweep.centralIndex.clear()

	if !concurrentSweep || mode == gcForceBlockMode {
		// 特殊情况的同步清扫
		// 记录不需要按比例进行清扫
		lock(&mheap_.lock)
		mheap_.sweepPagesPerByte = 0
		unlock(&mheap_.lock)

		// 刷新所有 mcache
		for _, pp := range allp {
			pp.mcache.prepareForSweep()
		}

		// 急切地清扫所有 spans
		for sweepone() != ^uintptr(0) {
		}

		// 急切地释放 workbufs
		prepareFreeWorkbufs()
		for freeSomeWbufs(false) {
		}

		// 这个标记/清扫周期的所有"释放"事件现在都已发生，
		// 所以我们可以立即使这个分析周期可用
		mProf_NextCycle()
		mProf_Flush()
		return true
	}

	// 后台清扫
	lock(&sweep.lock)
	if sweep.parked {
		sweep.parked = false
		ready(sweep.g, 0, true) // 唤醒清扫 goroutine
	}
	unlock(&sweep.lock)
	return false
}

// gcResetMarkState resets global state prior to marking (concurrent
// or STW) and resets the stack scan state of all Gs.
//
// This is safe to do without the world stopped because any Gs created
// during or after this will start out in the reset state.
//
// gcResetMarkState must be called on the system stack because it acquires
// the heap lock. See mheap for details.
//
//go:systemstack

// gcResetMarkState 在标记阶段(并发或STW)之前重置全局状态，
// 并重置所有 G 的栈扫描状态。
//
// 即使在不停止世界的情况下执行也是安全的，因为在此期间或之后
// 创建的任何 G 都将以重置状态开始。
//
// gcResetMarkState 必须在系统栈上调用，因为它会获取堆锁。
// 有关详细信息，请参见 mheap。
//
//go:systemstack
func gcResetMarkState() {
	// This may be called during a concurrent phase, so lock to make sure
	// allgs doesn't change.
	// 这可能在并发阶段被调用，所以需要加锁以确保 allgs 不会改变
	forEachG(func(gp *g) {
		gp.gcscandone = false // set to true in gcphasework
		gp.gcAssistBytes = 0
	})

	// Clear page marks. This is just 1MB per 64GB of heap, so the
	// time here is pretty trivial.
	// 清除页面标记。这大约是每64GB堆内存占用1MB，所以这里的时间开销很小
	lock(&mheap_.lock)
	arenas := mheap_.allArenas
	unlock(&mheap_.lock)
	for _, ai := range arenas {
		ha := mheap_.arenas[ai.l1()][ai.l2()]
		clear(ha.pageMarks[:])
	}

	// 重置标记字节计数
	work.bytesMarked = 0
	// 记录当前堆的活跃内存大小，作为标记阶段的基准值
	work.initialHeapLive = gcController.heapLive.Load()
}

// Hooks for other packages

// poolcleanup 是一个函数变量，用于清理 sync.Pool 中的对象
var poolcleanup func()

// boringCaches 存储了 crypto/internal/boring 包中需要清理的缓存指针
// 这些缓存用于加密操作
var boringCaches []unsafe.Pointer // for crypto/internal/boring

// uniqueMapCleanup 是一个通道，用于触发 unique 包的清理操作
// 当需要清理时，向该通道发送信号
var uniqueMapCleanup chan struct{} // for unique

// sync_runtime_registerPoolCleanup should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/bytedance/gopkg
//   - github.com/songzhibin97/gkit
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
// sync_runtime_registerPoolCleanup 本应该是一个内部实现细节，
// 但是很多包通过 linkname 来访问它。
// 一些不规范的包包括：
//   - github.com/bytedance/gopkg
//   - github.com/songzhibin97/gkit
//
// 不要删除或更改类型签名。
// 详见 go.dev/issue/67401。
//
//go:linkname sync_runtime_registerPoolCleanup sync.runtime_registerPoolCleanup
func sync_runtime_registerPoolCleanup(f func()) {
	poolcleanup = f
}

// boring_registerCache 用于注册加密相关的缓存
// 将传入的缓存指针添加到 boringCaches 切片中
//
//go:linkname boring_registerCache crypto/internal/boring/bcache.registerCache
func boring_registerCache(p unsafe.Pointer) {
	boringCaches = append(boringCaches, p)
}

// unique_runtime_registerUniqueMapCleanup 用于注册 unique 包的清理函数
// 在运行时启动一个 goroutine 来执行清理操作
//
//go:linkname unique_runtime_registerUniqueMapCleanup unique.runtime_registerUniqueMapCleanup
func unique_runtime_registerUniqueMapCleanup(f func()) {
	// Start the goroutine in the runtime so it's counted as a system goroutine.
	// 在运行时启动 goroutine，这样它会被计为系统 goroutine
	uniqueMapCleanup = make(chan struct{}, 1)
	go func(cleanup func()) {
		for {
			<-uniqueMapCleanup
			cleanup()
		}
	}(f)
}

func clearpools() {
	// clear sync.Pools
	// 清理 sync.Pools
	if poolcleanup != nil {
		poolcleanup()
	}

	// clear boringcrypto caches
	// 清理加密相关的缓存
	for _, p := range boringCaches {
		atomicstorep(p, nil)
	}

	// clear unique maps
	// 清理 unique maps
	if uniqueMapCleanup != nil {
		select {
		case uniqueMapCleanup <- struct{}{}:
		default:
		}
	}

	// Clear central sudog cache.
	// Leave per-P caches alone, they have strictly bounded size.
	// Disconnect cached list before dropping it on the floor,
	// so that a dangling ref to one entry does not pin all of them.
	// 清理中央 sudog 缓存
	// 保留每个 P 的缓存，因为它们有严格的大小限制
	// 在丢弃缓存列表之前断开连接，
	// 这样单个条目的悬空引用就不会固定所有条目
	lock(&sched.sudoglock)
	var sg, sgnext *sudog
	for sg = sched.sudogcache; sg != nil; sg = sgnext {
		sgnext = sg.next
		sg.next = nil
	}
	sched.sudogcache = nil
	unlock(&sched.sudoglock)

	// Clear central defer pool.
	// Leave per-P pools alone, they have strictly bounded size.
	lock(&sched.deferlock)
	// disconnect cached list before dropping it on the floor,
	// so that a dangling ref to one entry does not pin all of them.
	// 清理中央 defer 池
	// 保留每个 P 的池，因为它们有严格的大小限制
	// 在丢弃缓存列表之前断开连接，
	// 这样单个条目的悬空引用就不会固定所有条目
	var d, dlink *_defer
	for d = sched.deferpool; d != nil; d = dlink {
		dlink = d.link
		d.link = nil
	}
	sched.deferpool = nil
	unlock(&sched.deferlock)
}

// Timing

// itoaDiv formats val/(10**dec) into buf.
// itoaDiv 将 val/(10**dec) 格式化为字符串并写入 buf
func itoaDiv(buf []byte, val uint64, dec int) []byte {
	// 从 buf 的末尾开始写入
	i := len(buf) - 1
	// 计算小数点的位置
	idec := i - dec
	// 当 val >= 10 或者还没到小数点位置时继续循环
	for val >= 10 || i >= idec {
		// 将当前数字转换为 ASCII 并写入 buf
		buf[i] = byte(val%10 + '0')
		i--
		// 如果到达小数点位置，写入小数点
		if i == idec {
			buf[i] = '.'
			i--
		}
		// 将 val 除以 10 继续处理下一位
		val /= 10
	}
	// 写入最后一位数字
	buf[i] = byte(val + '0')
	// 返回实际写入的部分
	return buf[i:]
}

// fmtNSAsMS nicely formats ns nanoseconds as milliseconds.
// fmtNSAsMS 将纳秒格式化为毫秒
func fmtNSAsMS(buf []byte, ns uint64) []byte {
	if ns >= 10e6 {
		// Format as whole milliseconds.
		// 如果纳秒数大于等于 10 毫秒，直接格式化为整数毫秒
		return itoaDiv(buf, ns/1e6, 0)
	}
	// Format two digits of precision, with at most three decimal places.
	// 格式化为最多保留三位小数的毫秒数
	x := ns / 1e3
	if x == 0 {
		// 如果小于 1 微秒，直接返回 "0"
		buf[0] = '0'
		return buf[:1]
	}
	dec := 3
	// 根据数值大小动态调整小数位数
	// 当数值大于等于 100 时，减少小数位数
	for x >= 100 {
		x /= 10
		dec--
	}
	return itoaDiv(buf, x, dec)
}

// Helpers for testing GC.

// gcTestMoveStackOnNextCall causes the stack to be moved on a call
// immediately following the call to this. It may not work correctly
// if any other work appears after this call (such as returning).
// Typically the following call should be marked go:noinline so it
// performs a stack check.
//
// In rare cases this may not cause the stack to move, specifically if
// there's a preemption between this call and the next.

// gcTestMoveStackOnNextCall 用于在下一个调用时强制移动栈
// 这个函数会导致紧接着的下一次调用时移动栈
// 如果在这次调用之后有其他操作（比如返回），可能无法正常工作
// 通常下一个调用应该标记为 go:noinline，这样它就会执行栈检查
//
// 在极少数情况下，这个函数可能不会导致栈移动，特别是如果
// 在这次调用和下一次调用之间发生了抢占
func gcTestMoveStackOnNextCall() {
	// 获取当前 goroutine
	gp := getg()
	// 设置栈保护值为强制移动栈的值
	gp.stackguard0 = stackForceMove
}

// gcTestIsReachable performs a GC and returns a bit set where bit i
// is set if ptrs[i] is reachable.
// gcTestIsReachable 执行一次 GC 并返回一个位集合，其中第 i 位被设置表示 ptrs[i] 是可访问的
func gcTestIsReachable(ptrs ...unsafe.Pointer) (mask uint64) {
	// This takes the pointers as unsafe.Pointers in order to keep
	// them live long enough for us to attach specials. After
	// that, we drop our references to them.
	// 使用 unsafe.Pointer 类型接收指针，以便在附加 specials 时保持它们存活
	// 之后我们会释放对这些指针的引用

	if len(ptrs) > 64 {
		panic("too many pointers for uint64 mask")
	}

	// Block GC while we attach specials and drop our references
	// to ptrs. Otherwise, if a GC is in progress, it could mark
	// them reachable via this function before we have a chance to
	// drop them.
	// 在附加 specials 和释放指针引用期间阻塞 GC
	// 否则，如果 GC 正在进行，它可能会在我们有机会释放指针之前
	// 通过这个函数将它们标记为可访问
	semacquire(&gcsema)

	// Create reachability specials for ptrs.
	// 为每个指针创建可达性特殊标记
	specials := make([]*specialReachable, len(ptrs))
	for i, p := range ptrs {
		lock(&mheap_.speciallock)
		s := (*specialReachable)(mheap_.specialReachableAlloc.alloc())
		unlock(&mheap_.speciallock)
		s.special.kind = _KindSpecialReachable
		if !addspecial(p, &s.special) {
			throw("already have a reachable special (duplicate pointer?)")
		}
		specials[i] = s
		// Make sure we don't retain ptrs.
		// 确保我们不保留对指针的引用
		ptrs[i] = nil
	}

	semrelease(&gcsema)

	// Force a full GC and sweep.
	// 强制执行一次完整的 GC 和清扫
	GC()

	// Process specials.
	// 处理特殊标记
	for i, s := range specials {
		// 检查对象是否已被清扫
		// 如果对象未被清扫，说明 GC 过程出现问题
		if !s.done {
			printlock()
			println("runtime: object", i, "was not swept")
			throw("IsReachable failed")
		}
		// 如果对象可达，则在掩码中设置对应位
		// 使用位运算将第 i 位设置为 1
		if s.reachable {
			mask |= 1 << i
		}
		// 获取堆锁，防止并发访问
		lock(&mheap_.speciallock)
		// 释放特殊标记对象的内存
		mheap_.specialReachableAlloc.free(unsafe.Pointer(s))
		// 释放堆锁
		unlock(&mheap_.speciallock)
	}

	// 返回可达性掩码
	// 掩码中每个位表示对应指针是否可达
	return mask
}

// gcTestPointerClass returns the category of what p points to, one of:
// "heap", "stack", "data", "bss", "other". This is useful for checking
// that a test is doing what it's intended to do.
//
// This is nosplit simply to avoid extra pointer shuffling that may
// complicate a test.
//
//go:nosplit
func gcTestPointerClass(p unsafe.Pointer) string {
	// 将指针转换为 uintptr 类型，并使用 noescape 确保指针不会逃逸
	p2 := uintptr(noescape(p))
	// 获取当前 goroutine
	gp := getg()
	// 检查指针是否指向栈内存区域
	if gp.stack.lo <= p2 && p2 < gp.stack.hi {
		return "stack"
	}
	// 检查指针是否指向堆内存区域
	if base, _, _ := findObject(p2, 0, 0); base != 0 {
		return "heap"
	}
	// 遍历所有活动模块
	for _, datap := range activeModules() {
		// 检查指针是否指向数据段（data segment）
		// 包括普通数据段和不可指针追踪的数据段
		if datap.data <= p2 && p2 < datap.edata || datap.noptrdata <= p2 && p2 < datap.enoptrdata {
			return "data"
		}
		// 检查指针是否指向 BSS 段
		// 包括普通 BSS 段和不可指针追踪的 BSS 段
		if datap.bss <= p2 && p2 < datap.ebss || datap.noptrbss <= p2 && p2 <= datap.enoptrbss {
			return "bss"
		}
	}
	// 确保指针 p 在函数返回前不会被垃圾回收
	KeepAlive(p)
	// 如果指针不属于以上任何类别，则返回 "other"
	return "other"
}
