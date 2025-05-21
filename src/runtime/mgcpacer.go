// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/cpu"
	"internal/goexperiment"
	"internal/runtime/atomic"
	_ "unsafe" // for go:linkname
)

const (
	// gcGoalUtilization is the goal CPU utilization for
	// marking as a fraction of GOMAXPROCS.
	//
	// Increasing the goal utilization will shorten GC cycles as the GC
	// has more resources behind it, lessening costs from the write barrier,
	// but comes at the cost of increasing mutator latency.
	// gcGoalUtilization 是 GC 周期中标记阶段的目标 CPU 利用率，表示为 GOMAXPROCS 的一个分数。
	//
	// 增加目标利用率会缩短 GC 周期，因为 GC 拥有更多的资源，从而减少写屏障的成本，
	// 但会以增加 mutator 延迟为代价。
	gcGoalUtilization = gcBackgroundUtilization

	// gcBackgroundUtilization is the fixed CPU utilization for background
	// marking. It must be <= gcGoalUtilization. The difference between
	// gcGoalUtilization and gcBackgroundUtilization will be made up by
	// mark assists. The scheduler will aim to use within 50% of this
	// goal.
	//
	// As a general rule, there's little reason to set gcBackgroundUtilization
	// < gcGoalUtilization. One reason might be in mostly idle applications,
	// where goroutines are unlikely to assist at all, so the actual
	// utilization will be lower than the goal. But this is moot point
	// because the idle mark workers already soak up idle CPU resources.
	// These two values are still kept separate however because they are
	// distinct conceptually, and in previous iterations of the pacer the
	// distinction was more important.
	// gcBackgroundUtilization 是后台标记的固定 CPU 利用率。它必须小于或等于 gcGoalUtilization。
	// gcGoalUtilization 和 gcBackgroundUtilization 之间的差值将通过标记辅助来弥补。
	// 调度器将努力使用此目标的 50% 以内。
	//
	// 作为一般规则，几乎没有理由设置 gcBackgroundUtilization < gcGoalUtilization。
	// 一个原因可能是在主要空闲的应用程序中，goroutine 根本不太可能提供帮助，因此实际利用率将低于目标。
	// 但这是一个没有实际意义的点，因为空闲标记工作器已经吸收了空闲的 CPU 资源。
	// 然而，这两个值仍然是分开的，因为它们在概念上是不同的，并且在 pacer 的先前迭代中，这种区别更为重要。
	gcBackgroundUtilization = 0.25

	// gcCreditSlack is the amount of scan work credit that can
	// accumulate locally before updating gcController.heapScanWork and,
	// optionally, gcController.bgScanCredit. Lower values give a more
	// accurate assist ratio and make it more likely that assists will
	// successfully steal background credit. Higher values reduce memory
	// contention.
	// gcCreditSlack 是扫描工作 credit 的数量，这些 credit 可以在本地累积，
	// 然后更新 gcController.heapScanWork，并可以选择更新 gcController.bgScanCredit。
	// 较低的值可以提供更准确的辅助比率，并使辅助更有可能成功窃取后台 credit。
	// 较高的值会减少内存争用。
	gcCreditSlack = 2000

	// gcAssistTimeSlack is the nanoseconds of mutator assist time that
	// can accumulate on a P before updating gcController.assistTime.
	// gcAssistTimeSlack 是 mutator 辅助时间的纳秒数，这些时间可以在 P 上累积，
	// 然后更新 gcController.assistTime。
	gcAssistTimeSlack = 5000

	// gcOverAssistWork determines how many extra units of scan work a GC
	// assist does when an assist happens. This amortizes the cost of an
	// assist by pre-paying for this many bytes of future allocations.
	// gcOverAssistWork 确定在发生 GC 辅助时，GC 辅助执行的额外扫描工作单元的数量。
	// 这通过预先支付这么多字节的未来分配来分摊辅助的成本。
	gcOverAssistWork = 64 << 10

	// defaultHeapMinimum is the value of heapMinimum for GOGC==100.
	// defaultHeapMinimum 是 GOGC==100 时 heapMinimum 的值。
	defaultHeapMinimum = (goexperiment.HeapMinimum512KiBInt)*(512<<10) +
		(1-goexperiment.HeapMinimum512KiBInt)*(4<<20)

	// maxStackScanSlack is the bytes of stack space allocated or freed
	// that can accumulate on a P before updating gcController.stackSize.
	// maxStackScanSlack 是在更新 gcController.stackSize 之前，可以在 P 上累积的已分配或已释放堆栈空间的字节数。
	maxStackScanSlack = 8 << 10

	// memoryLimitMinHeapGoalHeadroom is the minimum amount of headroom the
	// pacer gives to the heap goal when operating in the memory-limited regime.
	// That is, it'll reduce the heap goal by this many extra bytes off of the
	// base calculation, at minimum.
	// memoryLimitMinHeapGoalHeadroom 是在内存受限状态下，pacer 给予堆目标 headroon 的最小值。
	// 也就是说，它将从基本计算中减少这么多额外的字节，至少。
	memoryLimitMinHeapGoalHeadroom = 1 << 20

	// memoryLimitHeapGoalHeadroomPercent is how headroom the memory-limit-based
	// heap goal should have as a percent of the maximum possible heap goal allowed
	// to maintain the memory limit.
	// memoryLimitHeapGoalHeadroomPercent 是基于内存限制的堆目标应该具有的 headroom 占允许的最大可能堆目标的百分比，
	// 以维持内存限制。
	memoryLimitHeapGoalHeadroomPercent = 3
)

// gcController implements the GC pacing controller that determines
// when to trigger concurrent garbage collection and how much marking
// work to do in mutator assists and background marking.
//
// It calculates the ratio between the allocation rate (in terms of CPU
// time) and the GC scan throughput to determine the heap size at which to
// trigger a GC cycle such that no GC assists are required to finish on time.
// This algorithm thus optimizes GC CPU utilization to the dedicated background
// mark utilization of 25% of GOMAXPROCS by minimizing GC assists.
// GOMAXPROCS. The high-level design of this algorithm is documented
// at https://github.com/golang/proposal/blob/master/design/44167-gc-pacer-redesign.md.
// See https://golang.org/s/go15gcpacing for additional historical context.
// gcController 实现了 GC 步调控制器，它决定了何时触发并发垃圾回收，以及在 mutator 辅助和后台标记中执行多少标记工作。
//
// 它计算分配率（以 CPU 时间衡量）和 GC 扫描吞吐量之间的比率，以确定触发 GC 周期的堆大小，
// 这样就不需要 GC 辅助来按时完成。
// 该算法因此优化了 GC CPU 利用率，使其达到 GOMAXPROCS 的 25% 的专用后台标记利用率，从而最大限度地减少 GC 辅助。
// GOMAXPROCS。该算法的高级设计记录在 https://github.com/golang/proposal/blob/master/design/44167-gc-pacer-redesign.md。
// 有关其他历史背景，请参见 https://golang.org/s/go15gcpacing。
var gcController gcControllerState

// gcControllerState 是 gcController 的实现细节。
// 它包含所有用于计算和存储 GC 相关状态的变量。
// 这些变量在 GC 周期开始时初始化，并在周期结束时更新。
// 它们在 GC 控制器中使用，以确定何时触发 GC 以及执行多少标记工作。
type gcControllerState struct {
	// Initialized from GOGC. GOGC=off means no GC.
	// gcPercent 从 GOGC 初始化。GOGC=off 表示不进行 GC。
	gcPercent atomic.Int32

	// memoryLimit is the soft memory limit in bytes.
	//
	// Initialized from GOMEMLIMIT. GOMEMLIMIT=off is equivalent to MaxInt64
	// which means no soft memory limit in practice.
	//
	// This is an int64 instead of a uint64 to more easily maintain parity with
	// the SetMemoryLimit API, which sets a maximum at MaxInt64. This value
	// should never be negative.
	//
	// memoryLimit 是以字节为单位的软内存限制。
	//
	// 从 GOMEMLIMIT 初始化。GOMEMLIMIT=off 相当于 MaxInt64，实际上表示没有软内存限制。
	//
	// 这是一个 int64 而不是 uint64，以便更容易与 SetMemoryLimit API 保持一致，该 API 将最大值设置为 MaxInt64。
	// 此值不应为负数。
	memoryLimit atomic.Int64

	// heapMinimum is the minimum heap size at which to trigger GC.
	// For small heaps, this overrides the usual GOGC*live set rule.
	//
	// When there is a very small live set but a lot of allocation, simply
	// collecting when the heap reaches GOGC*live results in many GC
	// cycles and high total per-GC overhead. This minimum amortizes this
	// per-GC overhead while keeping the heap reasonably small.
	//
	// During initialization this is set to 4MB*GOGC/100. In the case of
	// GOGC==0, this will set heapMinimum to 0, resulting in constant
	// collection even when the heap size is small, which is useful for
	// debugging.
	//
	// heapMinimum 是触发 GC 的最小堆大小。对于小堆，这将覆盖通常的 GOGC*live set 规则。
	// 当 live set 非常小但分配量很大时，简单地在堆达到 GOGC*live 时进行收集会导致许多 GC 周期和较高的总每次 GC 开销。
	// 此最小值分摊了每次 GC 开销，同时保持堆相对较小。
	//
	// 在初始化期间，这设置为 4MB*GOGC/100。在 GOGC==0 的情况下，这将设置 heapMinimum 为 0，从而即使在堆大小很小时也进行持续收集，这对于调试很有用。
	heapMinimum uint64

	// runway is the amount of runway in heap bytes allocated by the
	// application that we want to give the GC once it starts.
	//
	// This is computed from consMark during mark termination.
	//
	// runway 是应用程序在 GC 开始时希望给予 GC 的堆字节量。
	//
	// 这是在标记终止期间从 consMark 计算得出的。
	runway atomic.Uint64

	// consMark is the estimated per-CPU consMark ratio for the application.
	//
	// It represents the ratio between the application's allocation
	// rate, as bytes allocated per CPU-time, and the GC's scan rate,
	// as bytes scanned per CPU-time.
	// The units of this ratio are (B / cpu-ns) / (B / cpu-ns).
	//
	// At a high level, this value is computed as the bytes of memory
	// allocated (cons) per unit of scan work completed (mark) in a GC
	// cycle, divided by the CPU time spent on each activity.
	//
	// Updated at the end of each GC cycle, in endCycle.
	// consMark 是应用程序在 GC 周期中每单位扫描工作完成的内存分配（cons）与扫描工作（mark）的比率。
	// 这个比率的单位是 (B / cpu-ns) / (B / cpu-ns)。
	//
	// 在每个 GC 周期的结束时更新，在 endCycle 中。
	consMark float64

	// lastConsMark is the computed cons/mark value for the previous 4 GC
	// cycles. Note that this is *not* the last value of consMark, but the
	// measured cons/mark value in endCycle.
	// lastConsMark 是前 4 个 GC 周期的计算 cons/mark 值。
	// 注意，这不是 lastConsMark 的最后一个值，而是 endCycle 中的测量 cons/mark 值。
	lastConsMark [4]float64

	// gcPercentHeapGoal is the goal heapLive for when next GC ends derived
	// from gcPercent.
	//
	// Set to ^uint64(0) if gcPercent is disabled.
	// gcPercentHeapGoal 是根据 gcPercent 计算的下一个 GC 结束时的 heapLive。
	//
	// 如果 gcPercent 被禁用，则设置为 ^uint64(0)。
	gcPercentHeapGoal atomic.Uint64

	// sweepDistMinTrigger is the minimum trigger to ensure a minimum
	// sweep distance.
	//
	// This bound is also special because it applies to both the trigger
	// *and* the goal (all other trigger bounds must be based *on* the goal).
	//
	// It is computed ahead of time, at commit time. The theory is that,
	// absent a sudden change to a parameter like gcPercent, the trigger
	// will be chosen to always give the sweeper enough headroom. However,
	// such a change might dramatically and suddenly move up the trigger,
	// in which case we need to ensure the sweeper still has enough headroom.
	//
	// sweepDistMinTrigger 是确保最小触发距离的最低触发值。
	//
	// 这个边界也是特殊的，因为它适用于触发器和目标（所有其他触发器边界必须基于目标）。
	//
	// 它在提交时预先计算，在 commit time 计算。理论是，
	// 如果没有突然改变像 gcPercent 这样的参数，触发器将被选择为始终给 sweeper 足够的 headroom。
	// 然而，这种变化可能会突然显著地向上移动触发器，在这种情况下，我们需要确保 sweeper 仍然有足够的 headroom。
	sweepDistMinTrigger atomic.Uint64

	// triggered is the point at which the current GC cycle actually triggered.
	// Only valid during the mark phase of a GC cycle, otherwise set to ^uint64(0).
	//
	// Updated while the world is stopped.
	//
	// triggered 是当前 GC 周期实际触发的点。
	// 仅在 GC 周期的标记阶段有效，否则设置为 ^uint64(0)。
	//
	// 在 world 停止时更新。
	triggered uint64

	// lastHeapGoal is the value of heapGoal at the moment the last GC
	// ended. Note that this is distinct from the last value heapGoal had,
	// because it could change if e.g. gcPercent changes.
	//
	// Read and written with the world stopped or with mheap_.lock held.
	//
	// lastHeapGoal 是最后一次 GC 结束时 heapGoal 的值。
	// 注意，这与 heapGoal 的最后一个值不同，因为如果 e.g. gcPercent 发生变化，它可能会改变。
	//
	// 在 world 停止时或持有 mheap_.lock 时读取和写入。
	lastHeapGoal uint64

	// heapLive is the number of bytes considered live by the GC.
	// That is: retained by the most recent GC plus allocated
	// since then. heapLive ≤ memstats.totalAlloc-memstats.totalFree, since
	// heapAlloc includes unmarked objects that have not yet been swept (and
	// hence goes up as we allocate and down as we sweep) while heapLive
	// excludes these objects (and hence only goes up between GCs).
	//
	// To reduce contention, this is updated only when obtaining a span
	// from an mcentral and at this point it counts all of the unallocated
	// slots in that span (which will be allocated before that mcache
	// obtains another span from that mcentral). Hence, it slightly
	// overestimates the "true" live heap size. It's better to overestimate
	// than to underestimate because 1) this triggers the GC earlier than
	// necessary rather than potentially too late and 2) this leads to a
	// conservative GC rate rather than a GC rate that is potentially too
	// low.
	//
	// Whenever this is updated, call traceHeapAlloc() and
	// this gcControllerState's revise() method.
	// heapLive 是 GC 认为的存活字节数。
	// 也就是说：最近一次 GC 保留的加上从那以后分配的。
	// heapLive ≤ memstats.totalAlloc-memstats.totalFree，因为 heapAlloc 包括尚未清除的未标记对象（因此随着我们的分配而增加，随着我们的清除而减少），而 heapLive 排除这些对象（因此仅在 GC 之间增加）。
	//
	// 为了减少争用，仅在从 mcentral 获取 span 时才更新此值，此时它会计算该 span 中所有未分配的插槽（这些插槽将在该 mcache 从该 mcentral 获取另一个 span 之前分配）。
	// 因此，它略微高估了“真实”的活动堆大小。
	// 高估比低估更好，因为 1) 这会比不必要地提前触发 GC，而不是可能太晚，并且 2) 这会导致保守的 GC 速率，而不是可能太低的 GC 速率。
	//
	// 每次更新此值时，调用 traceHeapAlloc() 和此 gcControllerState 的 revise() 方法。
	heapLive atomic.Uint64

	// heapScan is the number of bytes of "scannable" heap. This is the
	// live heap (as counted by heapLive), but omitting no-scan objects and
	// no-scan tails of objects.
	//
	// This value is fixed at the start of a GC cycle. It represents the
	// maximum scannable heap.
	//
	// heapScan 是“可扫描”堆的字节数。
	// 这是活动堆（由 heapLive 计算），但省略了不可扫描的对象和对象的不可扫描尾部。
	//
	// 此值在 GC 周期的开始时固定。它表示最大可扫描堆。
	heapScan atomic.Uint64

	// lastHeapScan is the number of bytes of heap that were scanned
	// last GC cycle. It is the same as heapMarked, but only
	// includes the "scannable" parts of objects.
	//
	// Updated when the world is stopped.
	//
	// lastHeapScan 是上次 GC 周期中扫描的堆字节数。
	// 它与 heapMarked 相同，但只包括对象的“可扫描”部分。
	//
	// 在 world 停止时更新。
	lastHeapScan uint64

	// lastStackScan is the number of bytes of stack that were scanned
	// last GC cycle.
	//
	// Updated when the world is stopped.
	//
	// lastStackScan 是上次 GC 周期中扫描的堆字节数。
	//
	// 在 world 停止时更新。
	lastStackScan atomic.Uint64

	// maxStackScan is the amount of allocated goroutine stack space in
	// use by goroutines.
	//
	// This number tracks allocated goroutine stack space rather than used
	// goroutine stack space (i.e. what is actually scanned) because used
	// goroutine stack space is much harder to measure cheaply. By using
	// allocated space, we make an overestimate; this is OK, it's better
	// to conservatively overcount than undercount.
	//
	// maxStackScan 是 goroutines 使用的分配的 goroutine 堆栈空间量。
	//
	// 这个数字跟踪分配的 goroutine 堆栈空间，而不是实际使用的堆栈空间（即实际扫描的内容），因为实际使用的堆栈空间更难廉价测量。
	// 使用分配的空间，我们高估了；这是可以的，保守地高估比低估更好。
	maxStackScan atomic.Uint64

	// globalsScan is the total amount of global variable space
	// that is scannable.
	// globalsScan 是可扫描的全局变量空间的总量。
	globalsScan atomic.Uint64

	// heapMarked is the number of bytes marked by the previous
	// GC. After mark termination, heapLive == heapMarked, but
	// unlike heapLive, heapMarked does not change until the
	// next mark termination.
	// heapMarked 是先前 GC 标记的字节数。标记终止后，heapLive == heapMarked，但与 heapLive 不同，heapMarked 在下一次标记终止之前不会更改。
	heapMarked uint64

	// heapScanWork is the total heap scan work performed this cycle.
	// stackScanWork is the total stack scan work performed this cycle.
	// globalsScanWork is the total globals scan work performed this cycle.
	//
	// These are updated atomically during the cycle. Updates occur in
	// bounded batches, since they are both written and read
	// throughout the cycle. At the end of the cycle, heapScanWork is how
	// much of the retained heap is scannable.
	//
	// Currently these are measured in bytes. For most uses, this is an
	// opaque unit of work, but for estimation the definition is important.
	//
	// Note that stackScanWork includes only stack space scanned, not all
	// of the allocated stack.
	// heapScanWork 是此周期中执行的堆扫描总工作量。
	// stackScanWork 是此周期中执行的堆栈扫描总工作量。
	// globalsScanWork 是此周期中执行的全局变量扫描总工作量。
	//
	// 这些在周期中以原子方式更新。更新以有界批量进行，因为它们在整个周期中都被写入和读取。
	// 在周期结束时，heapScanWork 是保留堆中有多少是可扫描的。
	//
	// 目前，这些以字节为单位进行测量。对于大多数用途，这是一个不透明的工作单元，但对于估计，定义很重要。
	//
	// 请注意，stackScanWork 仅包括扫描的堆栈空间，而不包括所有已分配的堆栈。
	heapScanWork    atomic.Int64 // 堆扫描工作量
	stackScanWork   atomic.Int64 // 栈扫描工作量
	globalsScanWork atomic.Int64 // 全局变量扫描工作量

	// bgScanCredit is the scan work credit accumulated by the concurrent
	// background scan. This credit is accumulated by the background scan
	// and stolen by mutator assists.  Updates occur in bounded batches,
	// since it is both written and read throughout the cycle.
	//
	// bgScanCredit 是并发扫描累积的扫描工作量信用。
	// 这种信用是由并发扫描累积的，并被 mutator assists 窃取。
	// 更新以有界批量进行，因为它是周期中被写入和读取的。
	bgScanCredit atomic.Int64

	// assistTime is the nanoseconds spent in mutator assists
	// during this cycle. This is updated atomically, and must also
	// be updated atomically even during a STW, because it is read
	// by sysmon. Updates occur in bounded batches, since it is both
	// written and read throughout the cycle.
	//
	// assistTime 是此周期中在 mutator assists 中花费的纳秒数。
	// 这个值是原子更新的，即使在 STW 期间也必须原子更新，因为它是被 sysmon 读取的。
	// 更新以有界批量进行，因为它是周期中被写入和读取的。
	assistTime atomic.Int64

	// dedicatedMarkTime is the nanoseconds spent in dedicated mark workers
	// during this cycle. This is updated at the end of the concurrent mark
	// phase.
	//
	// dedicatedMarkTime 是此周期中在专用标记工作者中花费的纳秒数。
	// 这个值在并发标记阶段结束时更新。
	dedicatedMarkTime atomic.Int64

	// fractionalMarkTime is the nanoseconds spent in the fractional mark
	// worker during this cycle. This is updated throughout the cycle and
	// will be up-to-date if the fractional mark worker is not currently
	// running.
	//
	// fractionalMarkTime 是此周期中在分片标记工作者中花费的纳秒数。
	// 这个值在整个周期中更新，如果分片标记工作者当前不运行，它将是最新的。
	//
	// fractionalMarkTime 是此周期中在分片标记工作者中花费的纳秒数。
	// 这个值在整个周期中更新，如果分片标记工作者当前不运行，它将是最新的。
	fractionalMarkTime atomic.Int64

	// idleMarkTime is the nanoseconds spent in idle marking during this
	// cycle. This is updated throughout the cycle.
	//
	// idleMarkTime 是此周期中在空闲标记中花费的纳秒数。
	// 这个值在整个周期中更新。
	idleMarkTime atomic.Int64

	// markStartTime is the absolute start time in nanoseconds
	// that assists and background mark workers started.
	//
	// markStartTime 是辅助和后台标记工作者开始的时间（以纳秒为单位）。
	markStartTime int64

	// dedicatedMarkWorkersNeeded is the number of dedicated mark workers
	// that need to be started. This is computed at the beginning of each
	// cycle and decremented as dedicated mark workers get started.
	//
	// dedicatedMarkWorkersNeeded 是必须启动的专用标记工作者数量。
	// 这个值在每个周期的开始时计算，并在专用标记工作者启动时递减。
	dedicatedMarkWorkersNeeded atomic.Int64

	// idleMarkWorkers is two packed int32 values in a single uint64.
	// These two values are always updated simultaneously.
	//
	// The bottom int32 is the current number of idle mark workers executing.
	//
	// The top int32 is the maximum number of idle mark workers allowed to
	// execute concurrently. Normally, this number is just gomaxprocs. However,
	// during periodic GC cycles it is set to 0 because the system is idle
	// anyway; there's no need to go full blast on all of GOMAXPROCS.
	//
	// The maximum number of idle mark workers is used to prevent new workers
	// from starting, but it is not a hard maximum. It is possible (but
	// exceedingly rare) for the current number of idle mark workers to
	// transiently exceed the maximum. This could happen if the maximum changes
	// just after a GC ends, and an M with no P.
	//
	// Note that if we have no dedicated mark workers, we set this value to
	// 1 in this case we only have fractional GC workers which aren't scheduled
	// strictly enough to ensure GC progress. As a result, idle-priority mark
	// workers are vital to GC progress in these situations.
	//
	// For example, consider a situation in which goroutines block on the GC
	// (such as via runtime.GOMAXPROCS) and only fractional mark workers are
	// scheduled (e.g. GOMAXPROCS=1). Without idle-priority mark workers, the
	// last running M might skip scheduling a fractional mark worker if its
	// utilization goal is met, such that once it goes to sleep (because there's
	// nothing to do), there will be nothing else to spin up a new M for the
	// fractional worker in the future, stalling GC progress and causing a
	// deadlock. However, idle-priority workers will *always* run when there is
	// nothing left to do, ensuring the GC makes progress.
	//
	// See github.com/golang/go/issues/44163 for more details.
	// idleMarkWorkers 是一个 uint64，其中包含两个打包的 int32 值。
	// 这两个值总是同时更新。
	//
	// 底部的 int32 是当前正在执行的空闲标记工作者的数量。
	//
	// 顶部的 int32 是允许并发执行的空闲标记工作者的最大数量。
	// 通常，这个数字就是 gomaxprocs。但是，在周期性 GC 周期中，它被设置为 0，
	// 因为系统无论如何都是空闲的；没有必要在所有 GOMAXPROCS 上全力以赴。
	//
	// 空闲标记工作者的最大数量用于阻止新工作者启动，但它不是一个硬性最大值。
	// 当前空闲标记工作者的数量可能会短暂地超过最大值（这种情况极其罕见）。
	// 如果最大值在 GC 结束后立即更改，并且 M 没有 P，则可能会发生这种情况。
	//
	// 请注意，如果我们没有专用的标记工作者，我们将此值设置为 1，
	// 在这种情况下，我们只有 fractional GC 工作者，它们的调度不够严格，
	// 无法确保 GC 进度。因此，在这些情况下，空闲优先级的标记工作者对于 GC 进度至关重要。
	//
	// 例如，考虑这样一种情况：goroutine 阻塞 GC（例如通过 runtime.GOMAXPROCS），
	// 并且只调度 fractional 标记工作者（例如 GOMAXPROCS=1）。
	// 如果没有空闲优先级的标记工作者，则最后一个运行的 M 可能会跳过调度 fractional 标记工作者
	// （如果其利用率目标已达到），这样一旦它进入睡眠状态（因为没有事可做），
	// 将不会有其他东西来为 fractional 工作者启动一个新的 M，从而导致 GC 进度停滞并导致死锁。
	// 但是，当没有剩余的事情可做时，空闲优先级的工作者将*始终*运行，从而确保 GC 取得进展。
	//
	// 更多详细信息，请参见 github.com/golang/go/issues/44163。
	idleMarkWorkers atomic.Uint64

	// assistWorkPerByte is the ratio of scan work to allocated
	// bytes that should be performed by mutator assists. This is
	// computed at the beginning of each cycle and updated every
	// time heapScan is updated.
	//
	// assistWorkPerByte 是 mutator assists 应该执行的扫描工作量与分配的字节数的比率。
	// 这个值在每个周期的开始时计算，并在每次更新 heapScan 时更新。
	assistWorkPerByte atomic.Float64

	// assistBytesPerWork is 1/assistWorkPerByte.
	//
	// Note that because this is read and written independently
	// from assistWorkPerByte users may notice a skew between
	// the two values, and such a state should be safe.
	//
	// assistBytesPerWork 是 1/assistWorkPerByte。
	//
	// 请注意，因为 this 是独立读取和写入的，所以 users 可能会注意到两个值之间存在偏差，
	// 并且这种状态应该是安全的。
	assistBytesPerWork atomic.Float64

	// fractionalUtilizationGoal is the fraction of wall clock
	// time that should be spent in the fractional mark worker on
	// each P that isn't running a dedicated worker.
	//
	// For example, if the utilization goal is 25% and there are
	// no dedicated workers, this will be 0.25. If the goal is
	// 25%, there is one dedicated worker, and GOMAXPROCS is 5,
	// this will be 0.05 to make up the missing 5%.
	//
	// If this is zero, no fractional workers are needed.
	//
	// fractionalUtilizationGoal 是应该在每个 P 上花费的墙钟时间的分数，
	// 该 P 没有运行专用的工作者。
	//
	// 例如，如果利用率目标是 25%，并且没有专用的工作者，这个值将是 0.25。
	// 如果目标是 25%，有一个专用的工作者，并且 GOMAXPROCS 是 5，这个值将是 0.05，
	// 以弥补缺失的 5%。
	//
	// 如果这个值为零，则不需要分片工作者。
	fractionalUtilizationGoal float64

	// These memory stats are effectively duplicates of fields from
	// memstats.heapStats but are updated atomically or with the world
	// stopped and don't provide the same consistency guarantees.
	// 这些内存统计数据实际上是 memstats.heapStats 中字段的重复，
	// 但是以原子方式或在停止 world 的情况下更新，并且不提供相同的
	// 一致性保证。
	//
	// Because the runtime is responsible for managing a memory limit, it's
	// useful to couple these stats more tightly to the gcController, which
	// is intimately connected to how that memory limit is maintained.
	// 因为运行时负责管理内存限制，所以将这些统计数据更紧密地
	// 耦合到 gcController 非常有用，gcController 与如何维护
	// 该内存限制密切相关。
	heapInUse    sysMemStat    // bytes in mSpanInUse spans，正在使用的堆内存，以 mSpanInUse spans 为单位
	heapReleased sysMemStat    // bytes released to the OS，已释放到操作系统的堆内存
	heapFree     sysMemStat    // bytes not in any span, but not released to the OS，不在任何 span 中但未释放到操作系统的堆内存
	totalAlloc   atomic.Uint64 // total bytes allocated，已分配的总字节数
	totalFree    atomic.Uint64 // total bytes freed，已释放的总字节数
	mappedReady  atomic.Uint64 // total virtual memory in the Ready state (see mem.go).，处于 Ready 状态的总虚拟内存（参见 mem.go）。

	// test indicates that this is a test-only copy of gcControllerState.
	// test 表示这是 gcControllerState 的仅用于测试的副本。
	test bool

	_ cpu.CacheLinePad
}

// init initializes the GC controller state.
//
// Must be called with the world stopped.
//
// The world must be stopped, or mheap_.lock must be held.
// init 函数初始化 GC 控制器状态。
func (c *gcControllerState) init(gcPercent int32, memoryLimit int64) {
	// Initialize the GC controller state.
	// 初始化 GC 控制器状态。

	c.heapMinimum = defaultHeapMinimum
	// Set the minimum heap size.
	// 设置最小堆大小。

	c.triggered = ^uint64(0)
	// Initialize the trigger to a maximum value, so the first GC
	// cycle is always triggered.
	// 将触发器初始化为最大值，以便始终触发第一个 GC 周期。

	c.setGCPercent(gcPercent)
	// Set the GC percentage.
	// 设置 GC 百分比。

	c.setMemoryLimit(memoryLimit)
	// Set the memory limit.
	// 设置内存限制。

	c.commit(true) // No sweep phase in the first GC cycle.
	// Commit the GC controller state. Since this is the first GC
	// cycle, there is no sweep phase.
	// 提交 GC 控制器状态。由于这是第一个 GC 周期，因此没有清除阶段。

	// N.B. Don't bother calling traceHeapGoal. Tracing is never enabled at
	// initialization time.
	// 注意：不要费心调用 traceHeapGoal。在初始化时永远不会启用跟踪。

	// N.B. No need to call revise; there's no GC enabled during
	// initialization.
	// 注意：无需调用 revise；在初始化期间未启用 GC。
}

// startCycle resets the GC controller's state and computes estimates
// for a new GC cycle. The caller must hold worldsema and the world
// must be stopped.
// startCycle 重置 GC 控制器的状态并计算新 GC 周期的估计值。
// 调用者必须持有 worldsema 并且 world 必须停止。
func (c *gcControllerState) startCycle(markStartTime int64, procs int, trigger gcTrigger) {
	c.heapScanWork.Store(0)
	// Reset heap scan work counter.
	// 重置堆扫描工作计数器。
	c.stackScanWork.Store(0)
	// Reset stack scan work counter.
	// 重置栈扫描工作计数器。
	c.globalsScanWork.Store(0)
	// Reset globals scan work counter.
	// 重置全局变量扫描工作计数器。
	c.bgScanCredit.Store(0)
	// Reset background scan credit.
	// 重置后台扫描信用。
	c.assistTime.Store(0)
	// Reset assist time.
	// 重置辅助时间。
	c.dedicatedMarkTime.Store(0)
	// Reset dedicated mark time.
	// 重置专用标记时间。
	c.fractionalMarkTime.Store(0)
	// Reset fractional mark time.
	// 重置部分标记时间。
	c.idleMarkTime.Store(0)
	// Reset idle mark time.
	// 重置空闲标记时间。
	c.markStartTime = markStartTime
	// Record mark start time.
	// 记录标记开始时间。
	c.triggered = c.heapLive.Load()
	// Set trigger to current heap live.
	// 将触发器设置为当前堆活动大小。

	// Compute the background mark utilization goal. In general,
	// this may not come out exactly. We round the number of
	// dedicated workers so that the utilization is closest to
	// 25%. For small GOMAXPROCS, this would introduce too much
	// error, so we add fractional workers in that case.
	// 计算后台标记利用率目标。一般来说，这可能不会完全准确。
	// 我们对专用 worker 的数量进行四舍五入，以使利用率最接近 25%。
	// 对于小的 GOMAXPROCS，这会引入太多的误差，因此在这种情况下我们会添加 fractional worker。
	totalUtilizationGoal := float64(procs) * gcBackgroundUtilization
	dedicatedMarkWorkersNeeded := int64(totalUtilizationGoal + 0.5)
	utilError := float64(dedicatedMarkWorkersNeeded)/totalUtilizationGoal - 1
	const maxUtilError = 0.3
	if utilError < -maxUtilError || utilError > maxUtilError {
		// Rounding put us more than 30% off our goal. With
		// gcBackgroundUtilization of 25%, this happens for
		// GOMAXPROCS<=3 or GOMAXPROCS=6. Enable fractional
		// workers to compensate.
		// 四舍五入使我们偏离目标超过 30%。对于 25% 的 gcBackgroundUtilization，
		// 这发生在 GOMAXPROCS<=3 或 GOMAXPROCS=6 时。启用 fractional worker 进行补偿。
		if float64(dedicatedMarkWorkersNeeded) > totalUtilizationGoal {
			// Too many dedicated workers.
			// 专用 worker 太多。
			dedicatedMarkWorkersNeeded--
		}
		c.fractionalUtilizationGoal = (totalUtilizationGoal - float64(dedicatedMarkWorkersNeeded)) / float64(procs)
	} else {
		c.fractionalUtilizationGoal = 0
	}

	// In STW mode, we just want dedicated workers.
	// 在 STW 模式下，我们只需要专用 worker。
	if debug.gcstoptheworld > 0 {
		dedicatedMarkWorkersNeeded = int64(procs)
		c.fractionalUtilizationGoal = 0
	}

	// Clear per-P state
	// 清除每个 P 的状态。
	for _, p := range allp {
		p.gcAssistTime = 0
		// 清除 P 的 GC 辅助时间。
		p.gcFractionalMarkTime = 0
		// 清除 P 的 GC 部分标记时间。
	}

	if trigger.kind == gcTriggerTime {
		// During a periodic GC cycle, reduce the number of idle mark workers
		// required. However, we need at least one dedicated mark worker or
		// idle GC worker to ensure GC progress in some scenarios (see comment
		// on maxIdleMarkWorkers).
		// 在周期性 GC 循环期间，减少所需的空闲标记 worker 数量。
		// 但是，我们需要至少一个专用标记 worker 或空闲 GC worker，
		// 以确保在某些情况下 GC 能够取得进展（请参阅 maxIdleMarkWorkers 上的注释）。
		if dedicatedMarkWorkersNeeded > 0 {
			c.setMaxIdleMarkWorkers(0)
			// 如果需要专用标记 worker，则不允许空闲标记 worker。
		} else {
			// TODO(mknyszek): The fundamental reason why we need this is because
			// we can't count on the fractional mark worker to get scheduled.
			// Fix that by ensuring it gets scheduled according to its quota even
			// if the rest of the application is idle.
			// TODO(mknyszek)：我们需要这样做的根本原因是，我们不能指望 fractional mark worker 被调度。
			// 通过确保即使应用程序的其余部分处于空闲状态，它也会根据其配额进行调度来解决此问题。
			c.setMaxIdleMarkWorkers(1)
			// 否则，允许一个空闲标记 worker 以确保 GC 取得进展。
		}
	} else {
		// N.B. gomaxprocs and dedicatedMarkWorkersNeeded are guaranteed not to
		// change during a GC cycle.
		// 注意：gomaxprocs 和 dedicatedMarkWorkersNeeded 保证在 GC 循环期间不会更改。
		c.setMaxIdleMarkWorkers(int32(procs) - int32(dedicatedMarkWorkersNeeded))
		// 设置最大空闲标记 worker 数量。
	}

	// Compute initial values for controls that are updated
	// throughout the cycle.
	// 计算在整个循环中更新的控件的初始值。
	c.dedicatedMarkWorkersNeeded.Store(dedicatedMarkWorkersNeeded)
	// 存储所需的专用标记 worker 数量。
	c.revise()
	// 根据当前状态调整 GC 速率。

	if debug.gcpacertrace > 0 {
		// If gcpacertrace is enabled, print out some debugging information.
		// 如果启用了 gcpacertrace，则打印一些调试信息。
		heapGoal := c.heapGoal()
		// Get the heap goal.
		// 获取堆目标。
		assistRatio := c.assistWorkPerByte.Load()
		// Get the assist ratio.
		// 获取辅助比例。
		print("pacer: assist ratio=", assistRatio,
			" (scan ", gcController.heapScan.Load()>>20, " MB in ",
			work.initialHeapLive>>20, "->",
			heapGoal>>20, " MB)",
			" workers=", dedicatedMarkWorkersNeeded,
			"+", c.fractionalUtilizationGoal, "\n")
		// Print out the assist ratio, the amount of scanning work, the initial heap live size,
		// the heap goal, the number of dedicated mark workers, and the fractional utilization goal.
		// 打印辅助比例、扫描工作量、初始堆活动大小、堆目标、专用标记 worker 的数量和部分利用率目标。
	}
}

// revise updates the assist ratio during the GC cycle to account for
// improved estimates. This should be called whenever gcController.heapScan,
// gcController.heapLive, or if any inputs to gcController.heapGoal are
// updated. It is safe to call concurrently, but it may race with other
// calls to revise.
//
// The result of this race is that the two assist ratio values may not line
// up or may be stale. In practice this is OK because the assist ratio
// moves slowly throughout a GC cycle, and the assist ratio is a best-effort
// heuristic anyway. Furthermore, no part of the heuristic depends on
// the two assist ratio values being exact reciprocals of one another, since
// the two values are used to convert values from different sources.
//
// The worst case result of this raciness is that we may miss a larger shift
// in the ratio (say, if we decide to pace more aggressively against the
// hard heap goal) but even this "hard goal" is best-effort (see #40460).
// The dedicated GC should ensure we don't exceed the hard goal by too much
// in the rare case we do exceed it.
//
// It should only be called when gcBlackenEnabled != 0 (because this
// is when assists are enabled and the necessary statistics are
// available).
// revise 在 GC 循环期间更新辅助比例，以考虑改进的估计。
// 应该在 gcController.heapScan、gcController.heapLive 或 gcController.heapGoal 的任何输入更新时调用。
// 可以安全地并发调用，但它可能会与对 revise 的其他调用竞争。
//
// 这种竞争的结果是，两个辅助比率值可能不一致或可能过时。
// 实际上，这是可以的，因为辅助比率在整个 GC 周期中移动缓慢，并且辅助比率无论如何都是一种尽力而为的启发式方法。
// 此外，启发式的任何部分都不依赖于两个辅助比率值是彼此的精确倒数，因为这两个值用于转换来自不同来源的值。
//
// 这种竞争的最坏情况结果是，我们可能会错过比率的更大变化（例如，如果我们决定更积极地针对硬堆目标进行调整），
// 但即使这个“硬目标”也是尽力而为的（参见 #40460）。
// 专用 GC 应确保在极少数情况下，我们不会过度超出硬目标。
//
// 只有在 gcBlackenEnabled != 0 时才应调用它（因为这是启用辅助功能并且必要的统计信息可用时）。
func (c *gcControllerState) revise() {
	gcPercent := c.gcPercent.Load()
	if gcPercent < 0 {
		// If GC is disabled but we're running a forced GC,
		// act like GOGC is huge for the below calculations.
		gcPercent = 100000
		// 如果 GC 被禁用，但我们正在运行强制 GC，则假装 GOGC 很大以进行以下计算。
		// 将 gcPercent 设置为一个很大的值，以便在后续的计算中，即使 GC 被禁用，也能按照一定的比例进行垃圾回收。
	}
	live := c.heapLive.Load()
	// live 表示当前堆的活动大小。
	scan := c.heapScan.Load()
	// scan 表示当前已经扫描的堆大小。
	work := c.heapScanWork.Load() + c.stackScanWork.Load() + c.globalsScanWork.Load()
	// work 表示已经完成的扫描工作量，包括堆扫描、栈扫描和全局变量扫描。

	// Assume we're under the soft goal. Pace GC to complete at
	// heapGoal assuming the heap is in steady-state.
	heapGoal := int64(c.heapGoal())
	// 假设我们低于软目标。假设堆处于稳定状态，则调整 GC 以在 heapGoal 完成。
	// heapGoal 是一个软目标，表示期望的堆大小。

	// The expected scan work is computed as the amount of bytes scanned last
	// GC cycle (both heap and stack), plus our estimate of globals work for this cycle.
	scanWorkExpected := int64(c.lastHeapScan + c.lastStackScan.Load() + c.globalsScan.Load())
	// 预期的扫描工作量计算为上次 GC 循环中扫描的字节数（堆和栈），加上我们对本次循环的全局变量工作量的估计。
	// scanWorkExpected 表示预期的扫描工作量，用于后续的计算。

	// maxScanWork is a worst-case estimate of the amount of scan work that
	// needs to be performed in this GC cycle. Specifically, it represents
	// the case where *all* scannable memory turns out to be live, and
	// *all* allocated stack space is scannable.
	// maxScanWork 是本次 GC 周期中需要执行的扫描工作量的最坏情况估计。
	// 具体来说，它表示 *所有* 可扫描内存最终都是 live 的情况，并且 *所有* 已分配的栈空间都是可扫描的。
	maxStackScan := c.maxStackScan.Load()
	// maxStackScan 是最大栈扫描大小。
	maxScanWork := int64(scan + maxStackScan + c.globalsScan.Load())
	// maxScanWork 是最大扫描工作量，等于当前已扫描的堆大小加上最大栈扫描大小加上全局变量扫描大小。
	if work > scanWorkExpected {
		// We've already done more scan work than expected. Because our expectation
		// is based on a steady-state scannable heap size, we assume this means our
		// heap is growing. Compute a new heap goal that takes our existing runway
		// computed for scanWorkExpected and extrapolates it to maxScanWork, the worst-case
		// scan work. This keeps our assist ratio stable if the heap continues to grow.
		// 我们已经完成了比预期更多扫描工作。因为我们的期望是基于稳态可扫描堆大小，所以我们假设这意味着我们的堆正在增长。
		// 计算一个新的堆目标，该目标采用为 scanWorkExpected 计算的现有 runway，并将其外推到 maxScanWork，即最坏情况的扫描工作。
		// 如果堆继续增长，这将使我们的辅助比率保持稳定。
		//
		// The effect of this mechanism is that assists stay flat in the face of heap
		// growths. It's OK to use more memory this cycle to scan all the live heap,
		// because the next GC cycle is inevitably going to use *at least* that much
		// memory anyway.
		// 这种机制的效果是，在堆增长的情况下，辅助保持不变。
		// 可以在此周期中使用更多内存来扫描所有 live 堆，因为下一个 GC 周期无论如何都不可避免地会使用 *至少* 那么多内存。
		extHeapGoal := int64(float64(heapGoal-int64(c.triggered))/float64(scanWorkExpected)*float64(maxScanWork)) + int64(c.triggered)
		// extHeapGoal 是外推的堆目标，基于已完成的工作量和最大工作量计算得出。
		scanWorkExpected = maxScanWork
		// scanWorkExpected 更新为 maxScanWork，以便在后续的计算中使用。

		// hardGoal is a hard limit on the amount that we're willing to push back the
		// heap goal, and that's twice the heap goal (i.e. if GOGC=100 and the heap and/or
		// stacks and/or globals grow to twice their size, this limits the current GC cycle's
		// growth to 4x the original live heap's size).
		// hardGoal 是我们愿意推迟堆目标的量的硬性限制，它是堆目标的两倍（即，如果 GOGC=100 并且堆和/或栈和/或全局变量增长到其大小的两倍，
		// 这会将当前 GC 周期的增长限制为原始 live 堆大小的 4 倍）。
		//
		// This maintains the invariant that we use no more memory than the next GC cycle
		// will anyway.
		// 这保持了我们使用的内存不会超过下一个 GC 周期无论如何都会使用的内存的不变性。
		hardGoal := int64((1.0 + float64(gcPercent)/100.0) * float64(heapGoal))
		// hardGoal 是硬目标，表示堆大小的上限。
		if extHeapGoal > hardGoal {
			// 如果外推的堆目标大于硬目标，则将其设置为硬目标。
			extHeapGoal = hardGoal
		}
		heapGoal = extHeapGoal
		// heapGoal 更新为外推的堆目标。
	}
	if int64(live) > heapGoal {
		// We're already past our heap goal, even the extrapolated one.
		// Leave ourselves some extra runway, so in the worst case we
		// finish by that point.
		// 如果当前 live 大小已经超过了堆目标，即使是外推的堆目标，也需要留出一些额外的空间，以便在最坏的情况下也能完成 GC。
		const maxOvershoot = 1.1
		heapGoal = int64(float64(heapGoal) * maxOvershoot)
		// 将堆目标乘以一个略大于 1 的系数，以留出一些额外的空间。

		// Compute the upper bound on the scan work remaining.
		// 计算剩余扫描工作的上限。
		scanWorkExpected = maxScanWork
		// 将预期扫描工作量设置为最大扫描工作量。
	}

	// Compute the remaining scan work estimate.
	//
	// Note that we currently count allocations during GC as both
	// scannable heap (heapScan) and scan work completed
	// (scanWork), so allocation will change this difference
	// slowly in the soft regime and not at all in the hard
	// regime.
	// 计算剩余扫描工作量的估计值。
	//
	// 注意，我们目前将 GC 期间的分配计为可扫描堆（heapScan）和已完成的扫描工作（scanWork），
	// 因此分配将在软模式下缓慢改变这种差异，而在硬模式下则不会改变。
	scanWorkRemaining := scanWorkExpected - work
	// 剩余扫描工作量等于预期扫描工作量减去已完成的扫描工作量。
	if scanWorkRemaining < 1000 {
		// We set a somewhat arbitrary lower bound on
		// remaining scan work since if we aim a little high,
		// we can miss by a little.
		//
		// We *do* need to enforce that this is at least 1,
		// since marking is racy and double-scanning objects
		// may legitimately make the remaining scan work
		// negative, even in the hard goal regime.
		// 我们对剩余扫描工作量设置了一个有些武断的下限，因为如果我们的目标稍微高一点，我们可能会错过一点。
		//
		// 我们*确实*需要强制执行此值至少为 1，因为标记是竞争的，并且重复扫描对象可能会使剩余扫描工作量变为负数，即使在硬目标模式下也是如此。
		scanWorkRemaining = 1000
		// 如果剩余扫描工作量小于 1000，则将其设置为 1000。
	}

	// Compute the heap distance remaining.
	// 计算剩余堆距离。
	heapRemaining := heapGoal - int64(live)
	// 剩余堆距离等于堆目标减去当前 live 大小。
	if heapRemaining <= 0 {
		// This shouldn't happen, but if it does, avoid
		// dividing by zero or setting the assist negative.
		// 这不应该发生，但如果发生，请避免除以零或将辅助设置为负数。
		heapRemaining = 1
		// 如果剩余堆距离小于等于 0，则将其设置为 1。
	}

	// Compute the mutator assist ratio so by the time the mutator
	// allocates the remaining heap bytes up to heapGoal, it will
	// have done (or stolen) the remaining amount of scan work.
	// Note that the assist ratio values are updated atomically
	// but not together. This means there may be some degree of
	// skew between the two values. This is generally OK as the
	// values shift relatively slowly over the course of a GC
	// cycle.
	// 计算 mutator 辅助比率，以便在 mutator 将剩余堆字节分配到 heapGoal 之前，它将完成（或窃取）剩余的扫描工作量。
	// 请注意，辅助比率值是原子更新的，但不是一起更新的。这意味着两个值之间可能存在一定程度的偏差。
	// 这通常是可以的，因为这些值在 GC 周期中相对缓慢地变化。
	assistWorkPerByte := float64(scanWorkRemaining) / float64(heapRemaining)
	// 每个字节的辅助工作量等于剩余扫描工作量除以剩余堆距离。
	assistBytesPerWork := float64(heapRemaining) / float64(scanWorkRemaining)
	// 每个工作量的辅助字节数等于剩余堆距离除以剩余扫描工作量。
	c.assistWorkPerByte.Store(assistWorkPerByte)
	// 存储每个字节的辅助工作量。
	c.assistBytesPerWork.Store(assistBytesPerWork)
	// 存储每个工作量的辅助字节数。
}

// endCycle computes the consMark estimate for the next cycle.
// userForced indicates whether the current GC cycle was forced
// by the application.
// endCycle 函数计算下一个周期的 consMark 估计值。
// userForced 表示当前 GC 周期是否由应用程序强制执行。
func (c *gcControllerState) endCycle(now int64, procs int, userForced bool) {
	// Record last heap goal for the scavenger.
	// We'll be updating the heap goal soon.
	gcController.lastHeapGoal = c.heapGoal()
	// 记录 scavenger 的最后一个堆目标。
	// 我们将很快更新堆目标。

	// Compute the duration of time for which assists were turned on.
	assistDuration := now - c.markStartTime
	// 计算辅助开启的持续时间。

	// Assume background mark hit its utilization goal.
	utilization := gcBackgroundUtilization
	// 假设后台标记达到了其利用率目标。
	// Add assist utilization; avoid divide by zero.
	if assistDuration > 0 {
		utilization += float64(c.assistTime.Load()) / float64(assistDuration*int64(procs))
	}
	// 添加辅助利用率；避免除以零。

	if c.heapLive.Load() <= c.triggered {
		// Shouldn't happen, but let's be very safe about this in case the
		// GC is somehow extremely short.
		//
		// In this case though, the only reasonable value for c.heapLive-c.triggered
		// would be 0, which isn't really all that useful, i.e. the GC was so short
		// that it didn't matter.
		//
		// Ignore this case and don't update anything.
		return
	}
	// 如果当前堆大小小于等于触发的堆大小，则返回。
	// 这不应该发生，但为了安全起见，以防 GC 非常短。
	// 在这种情况下，c.heapLive-c.triggered 的唯一合理值将为 0，这实际上并没有什么用处，即 GC 太短了，无关紧要。
	// 忽略这种情况，不要更新任何内容。
	idleUtilization := 0.0
	if assistDuration > 0 {
		idleUtilization = float64(c.idleMarkTime.Load()) / float64(assistDuration*int64(procs))
	}
	// 计算空闲利用率。
	// Determine the cons/mark ratio.
	//
	// The units we want for the numerator and denominator are both B / cpu-ns.
	// We get this by taking the bytes allocated or scanned, and divide by the amount of
	// CPU time it took for those operations. For allocations, that CPU time is
	//
	//    assistDuration * procs * (1 - utilization)
	//
	// Where utilization includes just background GC workers and assists. It does *not*
	// include idle GC work time, because in theory the mutator is free to take that at
	// any point.
	//
	// For scanning, that CPU time is
	//
	//    assistDuration * procs * (utilization + idleUtilization)
	//
	// In this case, we *include* idle utilization, because that is additional CPU time that
	// the GC had available to it.
	//
	// In effect, idle GC time is sort of double-counted here, but it's very weird compared
	// to other kinds of GC work, because of how fluid it is. Namely, because the mutator is
	// *always* free to take it.
	//
	// So this calculation is really:
	//     (heapLive-trigger) / (assistDuration * procs * (1-utilization)) /
	//         (scanWork) / (assistDuration * procs * (utilization+idleUtilization))
	//
	// Note that because we only care about the ratio, assistDuration and procs cancel out.
	// 确定 cons/mark 比率。
	//
	// 我们想要的分子和分母的单位都是 B / cpu-ns。
	// 我们通过获取已分配或扫描的字节数，然后除以这些操作所花费的 CPU 时间来获得这个值。对于分配，CPU 时间是
	//
	//    assistDuration * procs * (1 - utilization)
	//
	// 其中 utilization 仅包括后台 GC worker 和 assists。它*不*包括空闲 GC 工作时间，因为理论上 mutator 可以随时占用它。
	//
	// 对于扫描，CPU 时间是
	//
	//    assistDuration * procs * (utilization + idleUtilization)
	//
	// 在这种情况下，我们*包括*空闲利用率，因为这是 GC 可用的额外 CPU 时间。
	//
	// 实际上，空闲 GC 时间在这里有点重复计算，但与其他类型的 GC 工作相比，它非常奇怪，因为它非常不稳定。也就是说，因为 mutator *总是*可以自由地占用它。
	//
	// 所以这个计算实际上是：
	//     (heapLive-trigger) / (assistDuration * procs * (1-utilization)) /
	//         (scanWork) / (assistDuration * procs * (utilization+idleUtilization))
	//
	// 请注意，因为我们只关心比率，所以 assistDuration 和 procs 会被抵消。
	scanWork := c.heapScanWork.Load() + c.stackScanWork.Load() + c.globalsScanWork.Load()
	// 计算总的扫描工作量，包括堆、栈和全局变量的扫描工作量。
	// Calculate the total scan work, including heap, stack, and global variables.

	currentConsMark := (float64(c.heapLive.Load()-c.triggered) * (utilization + idleUtilization)) /
		(float64(scanWork) * (1 - utilization))
	// 计算当前的 cons/mark 比率。这个比率用于调整 GC 的步调。
	// 公式为 (heapLive - trigger) * (utilization + idleUtilization) / (scanWork * (1 - utilization))
	// Calculate the current cons/mark ratio. This ratio is used to adjust the GC pacing.
	// The formula is (heapLive - trigger) * (utilization + idleUtilization) / (scanWork * (1 - utilization))

	// Update our cons/mark estimate. This is the maximum of the value we just computed and the last
	// 4 cons/mark values we measured. The reason we take the maximum here is to bias a noisy
	// cons/mark measurement toward fewer assists at the expense of additional GC cycles (starting
	// earlier).
	// 更新 cons/mark 的估计值。这是我们刚刚计算的值和我们测量的最后 4 个 cons/mark 值的最大值。
	// 我们在这里取最大值的原因是为了使有噪声的 cons/mark 测量偏向于更少的 assists，以牺牲额外的 GC 周期（提前启动）为代价。
	// Update our cons/mark estimate. This is the maximum of the value we just computed and the last
	// 4 cons/mark values we measured. The reason we take the maximum here is to bias a noisy
	// cons/mark measurement toward fewer assists at the expense of additional GC cycles (starting
	// earlier).
	oldConsMark := c.consMark
	// 保存旧的 consMark 值。
	// Save the old consMark value.
	c.consMark = currentConsMark
	// 将当前的 consMark 值设置为计算出的值。
	// Set the current consMark value to the calculated value.
	for i := range c.lastConsMark {
		if c.lastConsMark[i] > c.consMark {
			c.consMark = c.lastConsMark[i]
		}
	}
	// 遍历 lastConsMark 数组，如果数组中的任何值大于当前的 consMark 值，则更新 consMark 值。
	// Iterate over the lastConsMark array, and if any value in the array is greater than the current consMark value, update the consMark value.
	copy(c.lastConsMark[:], c.lastConsMark[1:])
	// 将 lastConsMark 数组中的所有值向前移动一位。
	// Move all values in the lastConsMark array forward by one position.
	c.lastConsMark[len(c.lastConsMark)-1] = currentConsMark
	// 将当前的 consMark 值添加到 lastConsMark 数组的末尾。
	// Add the current consMark value to the end of the lastConsMark array.

	if debug.gcpacertrace > 0 {
		printlock()
		goal := gcGoalUtilization * 100
		print("pacer: ", int(utilization*100), "% CPU (", int(goal), " exp.) for ")
		print(c.heapScanWork.Load(), "+", c.stackScanWork.Load(), "+", c.globalsScanWork.Load(), " B work (", c.lastHeapScan+c.lastStackScan.Load()+c.globalsScan.Load(), " B exp.) ")
		live := c.heapLive.Load()
		print("in ", c.triggered, " B -> ", live, " B (∆goal ", int64(live)-int64(c.lastHeapGoal), ", cons/mark ", oldConsMark, ")")
		println()
		printunlock()
	}
	// 如果启用了 gcpacertrace，则打印 GC 步调器的调试信息。
	// If gcpacertrace is enabled, print GC pacer debugging information.
}

// enlistWorker encourages another dedicated mark worker to start on
// another P if there are spare worker slots. It is used by putfull
// when more work is made available.
//
// enlistWorker 鼓励另一个专用标记 worker 在有空闲 worker 槽位时开始在另一个 P 上工作。
// 当有更多工作可用时，它由 putfull 使用。
//
//go:nowritebarrier
func (c *gcControllerState) enlistWorker() {
	// If there are idle Ps, wake one so it will run an idle worker.
	// NOTE: This is suspected of causing deadlocks. See golang.org/issue/19112.
	//
	//	if sched.npidle.Load() != 0 && sched.nmspinning.Load() == 0 {
	//		wakep()
	//		return
	//	}
	// 如果存在空闲的 P，则唤醒一个 P 以便它运行一个空闲的 worker。
	// 注意：这被怀疑会导致死锁。请参阅 golang.org/issue/19112。

	// There are no idle Ps. If we need more dedicated workers,
	// try to preempt a running P so it will switch to a worker.
	// 如果没有空闲的 P，我们需要更多专用的 worker，尝试抢占一个正在运行的 P，以便它可以切换到一个 worker。
	if c.dedicatedMarkWorkersNeeded.Load() <= 0 {
		return
	}
	// Pick a random other P to preempt.
	// 随机选择另一个 P 进行抢占。
	if gomaxprocs <= 1 {
		return
	}
	gp := getg()
	if gp == nil || gp.m == nil || gp.m.p == 0 {
		return
	}
	myID := gp.m.p.ptr().id              // 获取当前 P 的 ID。Get the ID of the current P.
	for tries := 0; tries < 5; tries++ { // 尝试 5 次抢占其他 P。Try to preempt other Ps 5 times.
		id := int32(cheaprandn(uint32(gomaxprocs - 1))) // 生成一个随机的 P ID。Generate a random P ID.
		if id >= myID {                                 // 如果随机 ID 大于或等于当前 P 的 ID，则增加 ID，以避免抢占当前 P。If the random ID is greater than or equal to the current P's ID, increment the ID to avoid preempting the current P.
			id++
		}
		p := allp[id]              // 获取 P 结构体。Get the P structure.
		if p.status != _Prunning { // 如果 P 没有运行，则继续下一次尝试。If the P is not running, continue to the next attempt.
			continue
		}
		if preemptone(p) { // 尝试抢占 P。Try to preempt the P.
			return // 如果抢占成功，则返回。If preemption is successful, return.
		}
	}
}

// findRunnableGCWorker returns a background mark worker for pp if it
// should be run. This must only be called when gcBlackenEnabled != 0.
//
// findRunnableGCWorker 函数返回一个后台标记 worker，如果它应该运行，则返回。
// 这必须在 gcBlackenEnabled != 0 时才能调用。
func (c *gcControllerState) findRunnableGCWorker(pp *p, now int64) (*g, int64) {
	if gcBlackenEnabled == 0 {
		throw("gcControllerState.findRunnable: blackening not enabled")
	}

	// Since we have the current time, check if the GC CPU limiter
	// hasn't had an update in a while. This check is necessary in
	// case the limiter is on but hasn't been checked in a while and
	// so may have left sufficient headroom to turn off again.
	// 由于我们有当前时间，检查 GC CPU 限制器是否有一段时间没有更新了。
	// 这种检查是必要的，以防限制器开启了，但有一段时间没有检查，
	// 因此可能留下了足够的空间来再次关闭。
	if now == 0 {
		now = nanotime()
	}
	if gcCPULimiter.needUpdate(now) {
		gcCPULimiter.update(now)
	}

	if !gcMarkWorkAvailable(pp) {
		// No work to be done right now. This can happen at
		// the end of the mark phase when there are still
		// assists tapering off. Don't bother running a worker
		// now because it'll just return immediately.
		// 现在没有工作要做。这可能发生在标记阶段的末尾，
		// 当仍然有 assists 逐渐减少时。现在不要费心运行 worker，
		// 因为它会立即返回。
		return nil, now
	}

	// Grab a worker before we commit to running below.
	// 在我们提交到下面运行之前，先获取一个 worker。
	node := (*gcBgMarkWorkerNode)(gcBgMarkWorkerPool.pop())
	if node == nil {
		// There is at least one worker per P, so normally there are
		// enough workers to run on all Ps, if necessary. However, once
		// a worker enters gcMarkDone it may park without rejoining the
		// pool, thus freeing a P with no corresponding worker.
		// gcMarkDone never depends on another worker doing work, so it
		// is safe to simply do nothing here.
		//
		// If gcMarkDone bails out without completing the mark phase,
		// it will always do so with queued global work. Thus, that P
		// will be immediately eligible to re-run the worker G it was
		// just using, ensuring work can complete.
		// 每个 P 至少有一个 worker，所以通常有足够的 worker 在所有 P 上运行（如果需要）。
		// 然而，一旦一个 worker 进入 gcMarkDone，它可能会 park 而不重新加入池，
		// 从而释放一个没有相应 worker 的 P。
		// gcMarkDone 从不依赖于另一个 worker 做工作，所以在这里简单地什么都不做是安全的。
		//
		// 如果 gcMarkDone 在没有完成标记阶段的情况下退出，它总是会带着排队的全局工作这样做。
		// 因此，该 P 将立即有资格重新运行它刚刚使用的 worker G，从而确保工作可以完成。
		return nil, now
	}

	decIfPositive := func(val *atomic.Int64) bool {
		// Decrement the value pointed to by val if it's positive.
		// 如果 val 指向的值是正数，则将其递减。
		for {
			v := val.Load() // Load the current value of the atomic int64. 加载 atomic int64 的当前值。
			if v <= 0 {     // If the value is not positive, return false. 如果该值不是正数，则返回 false。
				return false
			}

			if val.CompareAndSwap(v, v-1) { // Atomically decrement the value if it hasn't changed. 如果该值没有改变，则原子地递减该值。
				return true // Successfully decremented. 成功递减。
			}
		}
	}

	if decIfPositive(&c.dedicatedMarkWorkersNeeded) {
		// This P is now dedicated to marking until the end of
		// the concurrent mark phase.
		pp.gcMarkWorkerMode = gcMarkWorkerDedicatedMode
		// 如果成功将 dedicatedMarkWorkersNeeded 减 1，则表示此 P 现在专用于标记，直到并发标记阶段结束。
		// 设置 P 的 gcMarkWorkerMode 为 gcMarkWorkerDedicatedMode，表示该 P 专用于标记。
	} else if c.fractionalUtilizationGoal == 0 {
		// No need for fractional workers.
		gcBgMarkWorkerPool.push(&node.node)
		return nil, now
		// 如果 fractionalUtilizationGoal 为 0，则表示不需要 fractional worker。
		// 将 worker 节点放回 gcBgMarkWorkerPool。
		// 返回 nil 和当前时间，表示没有要运行的 G。
	} else {
		// Is this P behind on the fractional utilization
		// goal?
		//
		// This should be kept in sync with pollFractionalWorkerExit.
		delta := now - c.markStartTime
		if delta > 0 && float64(pp.gcFractionalMarkTime)/float64(delta) > c.fractionalUtilizationGoal {
			// Nope. No need to run a fractional worker.
			gcBgMarkWorkerPool.push(&node.node)
			return nil, now
			// 如果此 P 的 fractional utilization 落后于目标，则不需要运行 fractional worker。
			// 计算自标记阶段开始以来的时间差 delta。
			// 如果 delta 大于 0 且 (pp.gcFractionalMarkTime / delta) 大于 fractionalUtilizationGoal，则表示不需要运行 fractional worker。
			// 将 worker 节点放回 gcBgMarkWorkerPool。
			// 返回 nil 和当前时间，表示没有要运行的 G。
		}
		// Run a fractional worker.
		pp.gcMarkWorkerMode = gcMarkWorkerFractionalMode
		// 运行 fractional worker。
		// 设置 P 的 gcMarkWorkerMode 为 gcMarkWorkerFractionalMode，表示该 P 运行 fractional worker。
	}

	// Run the background mark worker.
	// 运行后台标记 worker。
	gp := node.gp.ptr()                   // Get the G from the node. 从节点获取 G。
	trace := traceAcquire()               // Acquire a trace context. 获取一个跟踪上下文。
	casgstatus(gp, _Gwaiting, _Grunnable) // Change the G's status from _Gwaiting to _Grunnable. 将 G 的状态从 _Gwaiting 更改为 _Grunnable。
	if trace.ok() {                       // If tracing is enabled... 如果启用了跟踪...
		trace.GoUnpark(gp, 0) // Trace that the G is unparked. 跟踪 G 被唤醒。
		traceRelease(trace)   // Release the trace context. 释放跟踪上下文。
	}
	return gp, now // Return the G and the current time. 返回 G 和当前时间。
}

// resetLive sets up the controller state for the next mark phase after the end
// of the previous one. Must be called after endCycle and before commit, before
// the world is started.
//
// The world must be stopped.
// resetLive 在前一个标记阶段结束后，为下一个标记阶段设置控制器状态。
// 必须在 endCycle 之后和 commit 之前，在 world 启动之前调用。
//
// 必须停止 world。
func (c *gcControllerState) resetLive(bytesMarked uint64) {
	c.heapMarked = bytesMarked                            // Store the number of bytes marked in this cycle. 存储在此循环中标记的字节数。
	c.heapLive.Store(bytesMarked)                         // Set the initial value of heapLive to the number of bytes marked. 将 heapLive 的初始值设置为标记的字节数。
	c.heapScan.Store(uint64(c.heapScanWork.Load()))       // Set the initial value of heapScan to the amount of scan work remaining. 将 heapScan 的初始值设置为剩余的扫描工作量。
	c.lastHeapScan = uint64(c.heapScanWork.Load())        // Remember the amount of heap scan work remaining at the beginning of the cycle. 记住循环开始时剩余的堆扫描工作量。
	c.lastStackScan.Store(uint64(c.stackScanWork.Load())) // Remember the amount of stack scan work remaining at the beginning of the cycle. 记住循环开始时剩余的堆栈扫描工作量。
	c.triggered = ^uint64(0)                              // Reset triggered. 重置触发器。

	// heapLive was updated, so emit a trace event.
	// heapLive 已更新，因此发出跟踪事件。
	trace := traceAcquire() // Acquire a trace context. 获取跟踪上下文。
	if trace.ok() {         // If tracing is enabled... 如果启用了跟踪...
		trace.HeapAlloc(bytesMarked) // Trace the amount of memory allocated on the heap. 跟踪堆上分配的内存量。
		traceRelease(trace)          // Release the trace context. 释放跟踪上下文。
	}
}

// markWorkerStop must be called whenever a mark worker stops executing.
//
// It updates mark work accounting in the controller by a duration of
// work in nanoseconds and other bookkeeping.
//
// Safe to execute at any time.
// markWorkerStop 必须在任何标记 worker 停止执行时调用。
//
// 它通过一个以纳秒为单位的 work 持续时间和其他簿记信息来更新控制器中的标记 work 记账。
//
// 可以在任何时候安全地执行。
func (c *gcControllerState) markWorkerStop(mode gcMarkWorkerMode, duration int64) {
	switch mode {
	case gcMarkWorkerDedicatedMode:
		// Dedicated mode mark worker stopped.
		// 专用模式标记 worker 停止。
		c.dedicatedMarkTime.Add(duration)
		c.dedicatedMarkWorkersNeeded.Add(1)
	case gcMarkWorkerFractionalMode:
		// Fractional mode mark worker stopped.
		// 分数模式标记 worker 停止。
		c.fractionalMarkTime.Add(duration)
	case gcMarkWorkerIdleMode:
		// Idle mode mark worker stopped.
		// 空闲模式标记 worker 停止。
		c.idleMarkTime.Add(duration)
		c.removeIdleMarkWorker()
	default:
		throw("markWorkerStop: unknown mark worker mode")
	}
}

func (c *gcControllerState) update(dHeapLive, dHeapScan int64) {
	// update updates the controller state with the given heap growth and scan work.
	// update 函数使用给定的堆增长和扫描工作量来更新控制器状态。
	if dHeapLive != 0 {
		// If the heap live size changed... 如果堆 live 大小发生了变化...
		trace := traceAcquire()                      // Acquire a trace context. 获取跟踪上下文。
		live := gcController.heapLive.Add(dHeapLive) // Atomically add dHeapLive to heapLive and get the new value. 原子地将 dHeapLive 加到 heapLive 并获取新值。
		if trace.ok() {
			// If tracing is enabled... 如果启用了跟踪...
			// gcController.heapLive changed.
			trace.HeapAlloc(live) // Trace the new heap size. 跟踪新的堆大小。
			traceRelease(trace)   // Release the trace context. 释放跟踪上下文。
		}
	}
	if gcBlackenEnabled == 0 {
		// Update heapScan when we're not in a current GC. It is fixed
		// at the beginning of a cycle.
		// 当我们不在当前的 GC 中时，更新 heapScan。它在循环开始时是固定的。
		if dHeapScan != 0 {
			// If the heap scan work changed... 如果堆扫描工作量发生了变化...
			gcController.heapScan.Add(dHeapScan) // Atomically add dHeapScan to heapScan. 原子地将 dHeapScan 加到 heapScan。
		}
	} else {
		// gcController.heapLive changed.
		// If we're in a current GC, revise the pacing based on the new heap size.
		// 如果我们处于当前的 GC 中，则根据新的堆大小修改步调。
		c.revise() // Revise the GC pacing. 修改 GC 步调。
	}
}

func (c *gcControllerState) addScannableStack(pp *p, amount int64) {
	// addScannableStack adds the amount of scannable stack to the global counter.
	// addScannableStack 将可扫描堆栈的数量添加到全局计数器。
	if pp == nil {
		// If pp is nil, add the amount to the global maxStackScan.
		// 如果 pp 为 nil，则将数量添加到全局 maxStackScan。
		c.maxStackScan.Add(amount)
		return
	}
	// Otherwise, add the amount to the p's maxStackScanDelta.
	// 否则，将数量添加到 p 的 maxStackScanDelta。
	pp.maxStackScanDelta += amount
	// If the p's maxStackScanDelta is greater than or equal to maxStackScanSlack or less than or equal to -maxStackScanSlack,
	// add the p's maxStackScanDelta to the global maxStackScan and reset the p's maxStackScanDelta.
	// 如果 p 的 maxStackScanDelta 大于或等于 maxStackScanSlack 或小于或等于 -maxStackScanSlack，
	// 则将 p 的 maxStackScanDelta 添加到全局 maxStackScan 并重置 p 的 maxStackScanDelta。
	if pp.maxStackScanDelta >= maxStackScanSlack || pp.maxStackScanDelta <= -maxStackScanSlack {
		c.maxStackScan.Add(pp.maxStackScanDelta)
		pp.maxStackScanDelta = 0
	}
}

func (c *gcControllerState) addGlobals(amount int64) {
	// addGlobals adds the amount of scannable global variables to the global counter.
	// addGlobals 将可扫描全局变量的数量添加到全局计数器。
	c.globalsScan.Add(amount)
}

// heapGoal returns the current heap goal.
// heapGoal 返回当前的堆目标。
func (c *gcControllerState) heapGoal() uint64 {
	// heapGoal returns the current heap goal.
	// heapGoal 返回当前的堆目标。
	goal, _ := c.heapGoalInternal()
	return goal
}

// heapGoalInternal is the implementation of heapGoal which returns additional
// information that is necessary for computing the trigger.
//
// The returned minTrigger is always <= goal.
// heapGoalInternal 是 heapGoal 的实现，它返回计算触发器所需的额外信息。
// 返回的 minTrigger 始终 <= goal。
func (c *gcControllerState) heapGoalInternal() (goal, minTrigger uint64) {
	// Start with the goal calculated for gcPercent.
	// 从为 gcPercent 计算的目标开始。
	goal = c.gcPercentHeapGoal.Load() // 加载基于 GOGC 百分比计算的堆目标。

	// Check if the memory-limit-based goal is smaller, and if so, pick that.
	// 检查基于内存限制的目标是否较小，如果是，则选择该目标。
	if newGoal := c.memoryLimitHeapGoal(); newGoal < goal {
		goal = newGoal // 如果内存限制目标较小，则使用它。
	} else {
		// We're not limited by the memory limit goal, so perform a series of
		// adjustments that might move the goal forward in a variety of circumstances.
		// 我们不受内存限制目标的限制，因此在各种情况下执行一系列调整，可能会使目标前进。

		sweepDistTrigger := c.sweepDistMinTrigger.Load() // 加载基于扫描距离的最小触发器。
		if sweepDistTrigger > goal {
			// Set the goal to maintain a minimum sweep distance since
			// the last call to commit. Note that we never want to do this
			// if we're in the memory limit regime, because it could push
			// the goal up.
			// 设置目标以维持自上次调用 commit 以来的最小扫描距离。请注意，如果我们在内存限制模式下，我们永远不想这样做，因为它可能会将目标向上推。
			goal = sweepDistTrigger // 如果扫描距离触发器大于当前目标，则更新目标。
		}
		// Since we ignore the sweep distance trigger in the memory
		// limit regime, we need to ensure we don't propagate it to
		// the trigger, because it could cause a violation of the
		// invariant that the trigger < goal.
		// 由于我们在内存限制模式下忽略扫描距离触发器，因此我们需要确保不将其传播到触发器，因为它可能导致违反触发器 < 目标的约束。
		minTrigger = sweepDistTrigger // 将最小触发器设置为扫描距离触发器。

		// Ensure that the heap goal is at least a little larger than
		// the point at which we triggered. This may not be the case if GC
		// start is delayed or if the allocation that pushed gcController.heapLive
		// over trigger is large or if the trigger is really close to
		// GOGC. Assist is proportional to this distance, so enforce a
		// minimum distance, even if it means going over the GOGC goal
		// by a tiny bit.
		//
		// Ignore this if we're in the memory limit regime: we'd prefer to
		// have the GC respond hard about how close we are to the goal than to
		// push the goal back in such a manner that it could cause us to exceed
		// the memory limit.
		// 确保堆目标至少比我们触发的点大一点。如果 GC 启动延迟，或者将 gcController.heapLive 推到触发器之上的分配很大，或者触发器非常接近 GOGC，则可能不是这种情况。Assist 与此距离成正比，因此即使这意味着稍微超过 GOGC 目标，也要强制执行最小距离。
		//
		// 如果我们在内存限制模式下，请忽略此设置：我们希望 GC 强烈响应我们与目标的接近程度，而不是以可能导致我们超过内存限制的方式将目标向后推。
		const minRunway = 64 << 10 // 最小安全距离，单位为字节。
		if c.triggered != ^uint64(0) && goal < c.triggered+minRunway {
			goal = c.triggered + minRunway // 确保目标至少比触发值大 minRunway。
		}
	}
	return // 返回计算出的堆目标和最小触发器。
}

// memoryLimitHeapGoal returns a heap goal derived from memoryLimit.
// memoryLimitHeapGoal 返回基于 memoryLimit 的堆目标。
func (c *gcControllerState) memoryLimitHeapGoal() uint64 {
	// Start by pulling out some values we'll need. Be careful about overflow.
	// 首先，提取一些我们需要的值。注意不要溢出。
	var heapFree, heapAlloc, mappedReady uint64
	for {
		heapFree = c.heapFree.load() // Free and unscavenged memory.
		// 空闲和未扫描的内存。
		heapAlloc = c.totalAlloc.Load() - c.totalFree.Load() // Heap object bytes in use.
		// 堆对象正在使用的字节数。
		mappedReady = c.mappedReady.Load() // Total unreleased mapped memory.
		// 总的未释放的映射内存。
		if heapFree+heapAlloc <= mappedReady {
			break
		}
		// It is impossible for total unreleased mapped memory to exceed heap memory, but
		// because these stats are updated independently, we may observe a partial update
		// including only some values. Thus, we appear to break the invariant. However,
		// this condition is necessarily transient, so just try again. In the case of a
		// persistent accounting error, we'll deadlock here.
		// 总的未释放的映射内存不可能超过堆内存，但是因为这些统计数据是独立更新的，
		// 我们可能会观察到一个只包含一些值的部分更新。因此，我们似乎打破了不变性。
		// 然而，这种情况必然是短暂的，所以再试一次。如果出现持久的记账错误，我们将会在这里死锁。
	}

	// Below we compute a goal from memoryLimit. There are a few things to be aware of.
	// Firstly, the memoryLimit does not easily compare to the heap goal: the former
	// is total mapped memory by the runtime that hasn't been released, while the latter is
	// only heap object memory. Intuitively, the way we convert from one to the other is to
	// subtract everything from memoryLimit that both contributes to the memory limit (so,
	// ignore scavenged memory) and doesn't contain heap objects. This isn't quite what
	// lines up with reality, but it's a good starting point.
	//
	// In practice this computation looks like the following:
	//
	//    goal := memoryLimit - ((mappedReady - heapFree - heapAlloc) + max(mappedReady - memoryLimit, 0))
	//                    ^1                                    ^2
	//    goal -= goal / 100 * memoryLimitHeapGoalHeadroomPercent
	//    ^3
	//
	// Let's break this down.
	//
	// The first term (marker 1) is everything that contributes to the memory limit and isn't
	// or couldn't become heap objects. It represents, broadly speaking, non-heap overheads.
	// One oddity you may have noticed is that we also subtract out heapFree, i.e. unscavenged
	// memory that may contain heap objects in the future.
	//
	// Let's take a step back. In an ideal world, this term would look something like just
	// the heap goal. That is, we "reserve" enough space for the heap to grow to the heap
	// goal, and subtract out everything else. This is of course impossible; the definition
	// is circular! However, this impossible definition contains a key insight: the amount
	// we're *going* to use matters just as much as whatever we're currently using.
	//
	// Consider if the heap shrinks to 1/10th its size, leaving behind lots of free and
	// unscavenged memory. mappedReady - heapAlloc will be quite large, because of that free
	// and unscavenged memory, pushing the goal down significantly.
	//
	// heapFree is also safe to exclude from the memory limit because in the steady-state, it's
	// just a pool of memory for future heap allocations, and making new allocations from heapFree
	// memory doesn't increase overall memory use. In transient states, the scavenger and the
	// allocator actively manage the pool of heapFree memory to maintain the memory limit.
	//
	// The second term (marker 2) is the amount of memory we've exceeded the limit by, and is
	// intended to help recover from such a situation. By pushing the heap goal down, we also
	// push the trigger down, triggering and finishing a GC sooner in order to make room for
	// other memory sources. Note that since we're effectively reducing the heap goal by X bytes,
	// we're actually giving more than X bytes of headroom back, because the heap goal is in
	// terms of heap objects, but it takes more than X bytes (e.g. due to fragmentation) to store
	// X bytes worth of objects.
	//
	// The final adjustment (marker 3) reduces the maximum possible memory limit heap goal by
	// memoryLimitHeapGoalPercent. As the name implies, this is to provide additional headroom in
	// the face of pacing inaccuracies, and also to leave a buffer of unscavenged memory so the
	// allocator isn't constantly scavenging. The reduction amount also has a fixed minimum
	// (memoryLimitMinHeapGoalHeadroom, not pictured) because the aforementioned pacing inaccuracies
	// disproportionately affect small heaps: as heaps get smaller, the pacer's inputs get fuzzier.
	// Shorter GC cycles and less GC work means noisy external factors like the OS scheduler have a
	// greater impact.

	// 下面我们从 memoryLimit 计算目标。 有几件事需要注意。
	// 首先，memoryLimit 不容易与堆目标进行比较：前者是运行时尚未释放的总映射内存，而后者仅是堆对象内存。
	// 直观地，我们将一个转换为另一个的方法是从 memoryLimit 中减去既有助于内存限制（因此，忽略已清除的内存）又不包含堆对象的所有内容。
	// 这与现实不太符，但这是一个很好的起点。
	//
	// 实际上，此计算如下所示：
	//
	//    goal := memoryLimit - ((mappedReady - heapFree - heapAlloc) + max(mappedReady - memoryLimit, 0))
	//                    ^1                                    ^2
	//    goal -= goal / 100 * memoryLimitHeapGoalHeadroomPercent
	//    ^3
	//
	// 让我们分解一下。
	//
	// 第一个术语（标记 1）是所有有助于内存限制且不是或不可能成为堆对象的内容。
	// 广义上讲，它代表非堆开销。 您可能已经注意到一个奇怪之处，我们还减去了 heapFree，即将来可能包含堆对象的未清除内存。
	//
	// 让我们退一步。 在理想的世界中，该术语看起来就像堆目标一样。 也就是说，我们“保留”足够的空间供堆增长到堆目标，然后减去其他所有内容。
	// 这当然是不可能的。定义是循环的！ 但是，这个不可能的定义包含一个关键的见解：我们*将要*使用的数量与我们当前使用的数量同等重要。
	//
	// 考虑一下，如果堆缩小到其大小的 1/10，则会留下大量空闲和未清除的内存。 由于该空闲和未清除的内存，mappedReady - heapAlloc 将非常大，从而显着降低了目标。
	//
	// heapFree 也可以安全地从内存限制中排除，因为在稳定状态下，它只是用于将来堆分配的内存池，并且从 heapFree 内存进行新的分配不会增加总体内存使用量。
	// 在瞬态状态下，清除器和分配器会主动管理 heapFree 内存池，以维持内存限制。
	//
	// 第二个术语（标记 2）是我们超出限制的内存量，旨在帮助从这种情况中恢复。
	// 通过降低堆目标，我们还会降低触发器，从而更快地触发和完成 GC，以便为其他内存源腾出空间。
	// 请注意，由于我们实际上是将堆目标减少了 X 字节，因此我们实际上提供了超过 X 字节的额外空间，
	// 因为堆目标是根据堆对象来衡量的，但是需要超过 X 字节（例如，由于碎片）来存储价值 X 字节的对象。
	//
	// 最终调整（标记 3）将最大可能的内存限制堆目标减少了 memoryLimitHeapGoalPercent。
	// 顾名思义，这是为了在面对步调不准确的情况下提供额外的空间，并留出未清除内存的缓冲区，以便分配器不会不断地进行清除。
	// 减少量也有一个固定的最小值（memoryLimitMinHeapGoalHeadroom，未图示），因为上述步调不准确对小堆的影响不成比例：
	// 随着堆越来越小，pacer 的输入变得越来越模糊。 较短的 GC 周期和较少的 GC 工作意味着嘈杂的外部因素（如 OS 调度程序）会产生更大的影响。

	memoryLimit := uint64(c.memoryLimit.Load())
	// 从原子变量中加载内存限制。
	// Load the memory limit from the atomic variable.

	// Compute term 1.
	nonHeapMemory := mappedReady - heapFree - heapAlloc
	// 计算公式中的第一项：非堆内存。
	// 这表示所有有助于内存限制且不是或不可能成为堆对象的内容。
	// Calculate term 1 in the formula: non-heap memory.
	// This represents everything that contributes to the memory limit and is not or cannot be a heap object.

	// Compute term 2.
	var overage uint64
	if mappedReady > memoryLimit {
		overage = mappedReady - memoryLimit
	}
	// 计算公式中的第二项：超出内存限制的部分。
	// Calculate term 2 in the formula: the amount exceeding the memory limit.

	if nonHeapMemory+overage >= memoryLimit {
		// We're at a point where non-heap memory exceeds the memory limit on its own.
		// There's honestly not much we can do here but just trigger GCs continuously
		// and let the CPU limiter reign that in. Something has to give at this point.
		// Set it to heapMarked, the lowest possible goal.
		return c.heapMarked
	}
	// 如果非堆内存加上超出部分已经超过了内存限制，
	// 那么我们能做的就只有持续触发GC，并让CPU限制器来控制。
	// 在这种情况下，将目标设置为heapMarked，即可能的最低目标。
	// If non-heap memory plus the overage already exceeds the memory limit,
	// then all we can do is continuously trigger GCs and let the CPU limiter control it.
	// In this case, set the goal to heapMarked, the lowest possible goal.

	// Compute the goal.
	goal := memoryLimit - (nonHeapMemory + overage)
	// 计算堆目标。
	// Calculate the heap goal.

	// Apply some headroom to the goal to account for pacing inaccuracies and to reduce
	// the impact of scavenging at allocation time in response to a high allocation rate
	// when GOGC=off. See issue #57069. Also, be careful about small limits.
	headroom := goal / 100 * memoryLimitHeapGoalHeadroomPercent
	// 应用一些额外空间到目标，以应对步调不准确的情况，
	// 并减少在高分配率时，由于GOGC=off而导致在分配时进行清除的影响。
	// 还要注意小限制。
	// Apply some headroom to the goal to account for pacing inaccuracies,
	// and to reduce the impact of scavenging at allocation time in response to a high allocation rate
	// when GOGC=off. Also, be careful about small limits.

	if headroom < memoryLimitMinHeapGoalHeadroom {
		// Set a fixed minimum to deal with the particularly large effect pacing inaccuracies
		// have for smaller heaps.
		headroom = memoryLimitMinHeapGoalHeadroom
	}
	// 如果额外空间小于最小堆目标额外空间，则将其设置为最小堆目标额外空间。
	// If the headroom is less than the minimum heap goal headroom, set it to the minimum heap goal headroom.

	if goal < headroom || goal-headroom < headroom {
		goal = headroom
	} else {
		goal = goal - headroom
	}
	// 确保目标不会太小。
	// Ensure the goal is not too small.

	// Don't let us go below the live heap. A heap goal below the live heap doesn't make sense.
	if goal < c.heapMarked {
		goal = c.heapMarked
	}
	// 不要让我们低于活动堆。低于活动堆的堆目标没有意义。
	// Don't let us go below the live heap. A heap goal below the live heap doesn't make sense.

	return goal
	// 返回计算出的堆目标。
	// Return the calculated heap goal.
}

const (
	// These constants determine the bounds on the GC trigger as a fraction
	// of heap bytes allocated between the start of a GC (heapLive == heapMarked)
	// and the end of a GC (heapLive == heapGoal).
	//
	// The constants are obscured in this way for efficiency. The denominator
	// of the fraction is always a power-of-two for a quick division, so that
	// the numerator is a single constant integer multiplication.
	// 这些常量决定了GC触发的边界，它表示从GC开始（heapLive == heapMarked）
	// 到GC结束（heapLive == heapGoal）之间分配的堆字节数的分数。
	//
	// 为了效率，这些常量以这种方式被模糊化。分母始终是2的幂，以便快速除法，
	// 这样分子就是一个简单的常数整数乘法。
	triggerRatioDen = 64

	// The minimum trigger constant was chosen empirically: given a sufficiently
	// fast/scalable allocator with 48 Ps that could drive the trigger ratio
	// to <0.05, this constant causes applications to retain the same peak
	// RSS compared to not having this allocator.
	// 最小触发常量是根据经验选择的：给定一个足够快/可扩展的分配器，
	// 具有48个Ps，可以将触发比率驱动到<0.05，此常量使应用程序保持与
	// 没有此分配器时相同的峰值RSS。
	minTriggerRatioNum = 45 // ~0.7

	// The maximum trigger constant is chosen somewhat arbitrarily, but the
	// current constant has served us well over the years.
	// 最大触发常量是随意选择的，但是当前的常量多年来一直为我们服务。
	maxTriggerRatioNum = 61 // ~0.95
)

// trigger returns the current point at which a GC should trigger along with
// the heap goal.
//
// The returned value may be compared against heapLive to determine whether
// the GC should trigger. Thus, the GC trigger condition should be (but may
// not be, in the case of small movements for efficiency) checked whenever
// the heap goal may change.
// trigger 返回当前GC应该触发的点以及堆目标。
//
// 返回值可以与heapLive进行比较，以确定是否应该触发GC。因此，每当堆目标可能改变时，
// 都应该检查GC触发条件（但为了效率，在小变动的情况下可能不会检查）。
func (c *gcControllerState) trigger() (uint64, uint64) {
	// 获取堆目标和最小触发值。
	// Get the heap goal and minimum trigger.
	goal, minTrigger := c.heapGoalInternal()

	// 不变性：触发器必须始终小于堆目标。
	// Invariant: the trigger must always be less than the heap goal.
	//
	// 请注意，内存限制对我们的堆目标设置了一个硬性上限，但活动堆可能会超出它。
	// Note that the memory limit sets a hard maximum on our heap goal,
	// but the live heap may grow beyond it.

	// 如果堆标记大于等于堆目标，则执行以下操作。
	// If the heap marked is greater than or equal to the heap goal, then do the following.
	if c.heapMarked >= goal {
		// 目标不应小于 heapMarked，但让我们对此保持防御性。
		// The goal should never be smaller than heapMarked, but let's be
		// defensive about it.
		// 这里唯一合理的触发器是在 heapMarked 处导致连续的 GC 循环，
		// 但如果目标小于该值，则尊重目标。
		// The only reasonable trigger here is one that
		// causes a continuous GC cycle at heapMarked, but respect the goal
		// if it came out as smaller than that.
		return goal, goal
	}

	// 在此之下，c.heapMarked < goal。
	// Below this point, c.heapMarked < goal.

	// heapMarked 是我们的绝对最小值，并且我们从 heapGoalinternal 获得的触发边界可能小于该值。
	// heapMarked is our absolute minimum, and it's possible the trigger
	// bound we get from heapGoalinternal is less than that.
	if minTrigger < c.heapMarked {
		// 如果最小触发值小于堆标记，则将最小触发值设置为堆标记。
		// If the minimum trigger is less than the heap marked, then set the minimum trigger to the heap marked.
		minTrigger = c.heapMarked
	}

	// If we let the trigger go too low, then if the application
	// is allocating very rapidly we might end up in a situation
	// where we're allocating black during a nearly always-on GC.
	// The result of this is a growing heap and ultimately an
	// increase in RSS. By capping us at a point >0, we're essentially
	// saying that we're OK using more CPU during the GC to prevent
	// this growth in RSS.
	// 如果我们让触发点过低，那么如果应用程序分配速度非常快，我们最终可能会处于一种
	// 在几乎总是开启的GC期间分配黑色对象的情况。
	// 这样做的结果是堆的增长，并最终导致RSS的增加。通过将我们限制在>0的点，我们实际上
	// 是在说，我们可以接受在GC期间使用更多的CPU来防止RSS的增长。
	triggerLowerBound := ((goal-c.heapMarked)/triggerRatioDen)*minTriggerRatioNum + c.heapMarked
	if minTrigger < triggerLowerBound {
		minTrigger = triggerLowerBound
	}

	// For small heaps, set the max trigger point at maxTriggerRatio of the way
	// from the live heap to the heap goal. This ensures we always have *some*
	// headroom when the GC actually starts. For larger heaps, set the max trigger
	// point at the goal, minus the minimum heap size.
	//
	// This choice follows from the fact that the minimum heap size is chosen
	// to reflect the costs of a GC with no work to do. With a large heap but
	// very little scan work to perform, this gives us exactly as much runway
	// as we would need, in the worst case.
	// 对于小堆，将最大触发点设置为从活动堆到堆目标之间的maxTriggerRatio。
	// 这确保了当GC实际启动时，我们总是有*一些*余量。对于较大的堆，将最大触发点
	// 设置为目标值，减去最小堆大小。
	//
	// 这种选择源于这样一个事实：最小堆大小的选择是为了反映没有工作可做的GC的成本。
	// 对于一个大堆，但扫描工作量非常小，这给了我们正好足够的跑道，在最坏的情况下。
	maxTrigger := ((goal-c.heapMarked)/triggerRatioDen)*maxTriggerRatioNum + c.heapMarked
	if goal > defaultHeapMinimum && goal-defaultHeapMinimum > maxTrigger {
		maxTrigger = goal - defaultHeapMinimum
	}
	maxTrigger = max(maxTrigger, minTrigger)

	// Compute the trigger from our bounds and the runway stored by commit.
	// 从我们的边界和commit存储的runway计算触发器。
	// Compute the trigger from our bounds and the runway stored by commit.
	// 从我们的边界和commit存储的runway计算触发器。
	var trigger uint64        // 声明触发器变量
	runway := c.runway.Load() // 从 c.runway 加载 runway 的值。runway 表示 GC 期望的剩余工作量。
	if runway > goal {        // 如果 runway 大于目标堆大小
		trigger = minTrigger // 触发器设置为最小触发值。这意味着 GC 应该尽快开始，因为 runway 已经超过了目标。
	} else { // 如果 runway 小于或等于目标堆大小
		trigger = goal - runway // 触发器设置为目标堆大小减去 runway。这表示在达到目标之前，还可以分配多少内存。
	}
	trigger = max(trigger, minTrigger) // 确保触发器不小于最小触发值。
	trigger = min(trigger, maxTrigger) // 确保触发器不大于最大触发值。
	if trigger > goal {                // 如果计算出的触发器大于目标堆大小，则表示存在错误。
		print("trigger=", trigger, " heapGoal=", goal, "\n")               // 打印触发器和目标堆大小的值。
		print("minTrigger=", minTrigger, " maxTrigger=", maxTrigger, "\n") // 打印最小和最大触发器的值。
		throw("produced a trigger greater than the heap goal")             // 抛出一个 panic，表明生成了一个大于堆目标的触发器。
	}
	return trigger, goal // 返回计算出的触发器和目标堆大小。
}

// commit recomputes all pacing parameters needed to derive the
// trigger and the heap goal. Namely, the gcPercent-based heap goal,
// and the amount of runway we want to give the GC this cycle.
//
// This can be called any time. If GC is the in the middle of a
// concurrent phase, it will adjust the pacing of that phase.
//
// isSweepDone should be the result of calling isSweepDone(),
// unless we're testing or we know we're executing during a GC cycle.
//
// This depends on gcPercent, gcController.heapMarked, and
// gcController.heapLive. These must be up to date.
//
// Callers must call gcControllerState.revise after calling this
// function if the GC is enabled.
//
// mheap_.lock must be held or the world must be stopped.
// commit 重新计算所有用于推导触发器和堆目标的步调参数。
// 也就是基于 gcPercent 的堆目标，以及我们希望在本周期给 GC 的 runway 量。
//
// 可以在任何时候调用。如果 GC 处于并发阶段的中间，它将调整该阶段的步调。
//
// isSweepDone 应该是调用 isSweepDone() 的结果，除非我们正在测试或者我们知道我们正在 GC 周期中执行。
//
// 这取决于 gcPercent、gcController.heapMarked 和 gcController.heapLive。这些值必须是最新的。
//
// 如果启用了 GC，调用者必须在调用此函数后调用 gcControllerState.revise。
//
// 必须持有 mheap_.lock 或停止 world。
func (c *gcControllerState) commit(isSweepDone bool) {
	if !c.test {
		assertWorldStoppedOrLockHeld(&mheap_.lock)
	}
	// 如果不是测试环境，则断言 world 停止或持有 mheap_.lock 锁。
	// If not a test environment, assert that the world is stopped or the mheap_.lock is held.

	if isSweepDone {
		// The sweep is done, so there aren't any restrictions on the trigger
		// we need to think about.
		c.sweepDistMinTrigger.Store(0)
	} else {
		// Concurrent sweep happens in the heap growth
		// from gcController.heapLive to trigger. Make sure we
		// give the sweeper some runway if it doesn't have enough.
		c.sweepDistMinTrigger.Store(c.heapLive.Load() + sweepMinHeapDistance)
	}
	// 如果清扫已完成，则对触发器没有任何限制。
	// 否则，并发清扫发生在堆增长中，从 gcController.heapLive 到触发器。
	// 确保我们给清扫器一些 runway，如果它没有足够的 runway。
	// If the sweep is done, there are no restrictions on the trigger.
	// Otherwise, concurrent sweep happens in the heap growth from gcController.heapLive to trigger.
	// Make sure we give the sweeper some runway if it doesn't have enough.

	// Compute the next GC goal, which is when the allocated heap
	// has grown by GOGC/100 over where it started the last cycle,
	// plus additional runway for non-heap sources of GC work.
	gcPercentHeapGoal := ^uint64(0)
	if gcPercent := c.gcPercent.Load(); gcPercent >= 0 {
		gcPercentHeapGoal = c.heapMarked + (c.heapMarked+c.lastStackScan.Load()+c.globalsScan.Load())*uint64(gcPercent)/100
	}
	// Apply the minimum heap size here. It's defined in terms of gcPercent
	// and is only updated by functions that call commit.
	if gcPercentHeapGoal < c.heapMinimum {
		gcPercentHeapGoal = c.heapMinimum
	}
	c.gcPercentHeapGoal.Store(gcPercentHeapGoal)
	// 计算下一个 GC 目标，即已分配的堆比上一个周期开始时增长了 GOGC/100，
	// 加上来自非堆 GC 来源的额外 runway。
	// 在这里应用最小堆大小。它根据 gcPercent 定义，并且仅由调用 commit 的函数更新。
	// Compute the next GC goal, which is when the allocated heap has grown by GOGC/100 over where it started the last cycle,
	// plus additional runway for non-heap sources of GC work.
	// Apply the minimum heap size here. It's defined in terms of gcPercent and is only updated by functions that call commit.

	// Compute the amount of runway we want the GC to have by using our
	// estimate of the cons/mark ratio.
	//
	// The idea is to take our expected scan work, and multiply it by
	// the cons/mark ratio to determine how long it'll take to complete
	// that scan work in terms of bytes allocated. This gives us our GC's
	// runway.
	//
	// However, the cons/mark ratio is a ratio of rates per CPU-second, but
	// here we care about the relative rates for some division of CPU
	// resources among the mutator and the GC.
	//
	// To summarize, we have B / cpu-ns, and we want B / ns. We get that
	// by multiplying by our desired division of CPU resources. We choose
	// to express CPU resources as GOMAPROCS*fraction. Note that because
	// we're working with a ratio here, we can omit the number of CPU cores,
	// because they'll appear in the numerator and denominator and cancel out.
	// As a result, this is basically just "weighing" the cons/mark ratio by
	// our desired division of resources.
	//
	// Furthermore, by setting the runway so that CPU resources are divided
	// this way, assuming that the cons/mark ratio is correct, we make that
	// division a reality.
	// 计算我们希望 GC 拥有的 runway 大小，通过使用我们对 cons/mark 比率的估计。
	//
	// 我们的想法是将我们预期的扫描工作量乘以 cons/mark 比率，以确定完成该扫描工作量需要多长时间（以分配的字节为单位）。这为我们提供了 GC 的 runway。
	//
	// 然而，cons/mark 比率是每个 CPU 秒的速率比率，但在这里我们关心的是 mutator 和 GC 之间 CPU 资源划分的相对速率。
	//
	// 总结一下，我们有 B / cpu-ns，我们想要 B / ns。我们通过乘以我们期望的 CPU 资源划分来实现这一点。我们选择将 CPU 资源表示为 GOMAPROCS*fraction。
	// 请注意，因为我们在这里处理的是比率，所以我们可以省略 CPU 核心的数量，因为它们将出现在分子和分母中并相互抵消。
	// 因此，这基本上只是“衡量” cons/mark 比率，通过我们期望的资源划分。
	//
	// 此外，通过设置 runway 以便以这种方式划分 CPU 资源，假设 cons/mark 比率是正确的，我们使该划分成为现实。
	c.runway.Store(uint64((c.consMark * (1 - gcGoalUtilization) / (gcGoalUtilization)) * float64(c.lastHeapScan+c.lastStackScan.Load()+c.globalsScan.Load())))
}

// setGCPercent updates gcPercent. commit must be called after.
// Returns the old value of gcPercent.
//
// The world must be stopped, or mheap_.lock must be held.
// setGCPercent 更新 gcPercent。之后必须调用 commit。
// 返回 gcPercent 的旧值。
//
// 必须停止 world，或者必须持有 mheap_.lock。
func (c *gcControllerState) setGCPercent(in int32) int32 {
	// If we're not in a test environment, assert that the world is stopped or the heap lock is held.
	// 如果我们不在测试环境中，则断言 world 已停止或持有堆锁。
	if !c.test {
		assertWorldStoppedOrLockHeld(&mheap_.lock)
	}

	// Load the old value of gcPercent.
	// 加载 gcPercent 的旧值。
	out := c.gcPercent.Load()
	// If the input is negative, set it to -1 (disable GC).
	// 如果输入为负数，则将其设置为 -1（禁用 GC）。
	if in < 0 {
		in = -1
	}
	// Update the minimum heap size based on the new gcPercent.
	// 根据新的 gcPercent 更新最小堆大小。
	c.heapMinimum = defaultHeapMinimum * uint64(in) / 100
	// Store the new value of gcPercent.
	// 存储 gcPercent 的新值。
	c.gcPercent.Store(in)

	// Return the old value of gcPercent.
	// 返回 gcPercent 的旧值。
	return out
}

//go:linkname setGCPercent runtime/debug.setGCPercent
func setGCPercent(in int32) (out int32) {
	// Run on the system stack since we grab the heap lock.
	// 在系统栈上运行，因为我们需要获取堆锁。
	systemstack(func() {
		lock(&mheap_.lock)
		// Set the GC percent.
		// 设置 GC 百分比。
		out = gcController.setGCPercent(in)
		// Commit the GC controller state.
		// 提交 GC 控制器状态。
		gcControllerCommit()
		unlock(&mheap_.lock)
	})

	// If we just disabled GC, wait for any concurrent GC mark to
	// finish so we always return with no GC running.
	// 如果我们刚刚禁用了 GC，等待任何并发的 GC 标记完成，以便我们始终在没有 GC 运行的情况下返回。
	if in < 0 {
		gcWaitOnMark(work.cycles.Load())
	}

	return out
}

func readGOGC() int32 {
	// Read the GOGC environment variable.
	// 读取 GOGC 环境变量。
	p := gogetenv("GOGC")
	// If GOGC is "off", disable GC.
	// 如果 GOGC 是 "off"，则禁用 GC。
	if p == "off" {
		return -1
	}
	// If GOGC is a valid integer, use it as the GC percentage.
	// 如果 GOGC 是一个有效的整数，则将其用作 GC 百分比。
	if n, ok := atoi32(p); ok {
		return n
	}
	// Otherwise, use the default GC percentage of 100.
	// 否则，使用默认的 GC 百分比 100。
	return 100
}

// setMemoryLimit updates memoryLimit. commit must be called after
// Returns the old value of memoryLimit.
//
// The world must be stopped, or mheap_.lock must be held.
// setMemoryLimit 更新 memoryLimit。之后必须调用 commit。
// 返回 memoryLimit 的旧值。
//
// 必须停止 world，或者必须持有 mheap_.lock。
func (c *gcControllerState) setMemoryLimit(in int64) int64 {
	// If we're not in a test environment, assert that the world is stopped or the heap lock is held.
	// 如果我们不在测试环境中，断言 world 已停止或持有堆锁。
	if !c.test {
		assertWorldStoppedOrLockHeld(&mheap_.lock)
	}

	// Load the old memory limit.
	// 加载旧的内存限制。
	out := c.memoryLimit.Load()
	// If the input is non-negative, store it as the new memory limit.
	// 如果输入为非负数，则将其存储为新的内存限制。
	if in >= 0 {
		c.memoryLimit.Store(in)
	}

	// Return the old memory limit.
	// 返回旧的内存限制。
	return out
}

//go:linkname setMemoryLimit runtime/debug.setMemoryLimit
func setMemoryLimit(in int64) (out int64) {
	// Run on the system stack since we grab the heap lock.
	// 在系统栈上运行，因为我们要获取堆锁。
	systemstack(func() {
		// Acquire the heap lock.
		// 获取堆锁。
		lock(&mheap_.lock)
		// Set the memory limit using the gcController.
		// 使用 gcController 设置内存限制。
		out = gcController.setMemoryLimit(in)
		// If the input is negative or the old value is the same as the new value,
		// there's no need to commit the change.
		// 如果输入为负数或者旧值与新值相同，则无需提交更改。
		if in < 0 || out == in {
			// If we're just checking the value or not changing
			// it, there's no point in doing the rest.
			// 如果我们只是检查该值或者不更改它，则无需执行其余操作。
			unlock(&mheap_.lock)
			return
		}
		// Commit the change to the gcController.
		// 将更改提交到 gcController。
		gcControllerCommit()
		// Release the heap lock.
		// 释放堆锁。
		unlock(&mheap_.lock)
	})
	// Return the old memory limit.
	// 返回旧的内存限制。
	return out
}

// readGOMEMLIMIT reads the GOMEMLIMIT environment variable.
// It returns the value of GOMEMLIMIT, or maxInt64 if GOMEMLIMIT is
// unset or "off". If GOMEMLIMIT is malformed, it throws.
func readGOMEMLIMIT() int64 {
	// Get the value of the GOMEMLIMIT environment variable.
	// 获取 GOMEMLIMIT 环境变量的值。
	p := gogetenv("GOMEMLIMIT")
	// If GOMEMLIMIT is not set or is set to "off", return maxInt64.
	// 如果 GOMEMLIMIT 未设置或设置为 "off"，则返回 maxInt64。
	if p == "" || p == "off" {
		return maxInt64
	}
	// Parse the value of GOMEMLIMIT as a byte count.
	// 将 GOMEMLIMIT 的值解析为字节计数。
	n, ok := parseByteCount(p)
	// If the value of GOMEMLIMIT is malformed, throw an error.
	// 如果 GOMEMLIMIT 的值格式不正确，则抛出错误。
	if !ok {
		print("GOMEMLIMIT=", p, "\n")
		throw("malformed GOMEMLIMIT; see `go doc runtime/debug.SetMemoryLimit`")
	}
	// Return the parsed value of GOMEMLIMIT.
	// 返回 GOMEMLIMIT 的解析值。
	return n
}

// addIdleMarkWorker attempts to add a new idle mark worker.
//
// If this returns true, the caller must become an idle mark worker unless
// there's no background mark worker goroutines in the pool. This case is
// harmless because there are already background mark workers running.
// If this returns false, the caller must NOT become an idle mark worker.
//
// nosplit because it may be called without a P.
//
// addIdleMarkWorker 尝试添加一个新的空闲标记 worker。
//
// 如果此函数返回 true，则调用者必须成为一个空闲标记 worker，除非工作池中没有后台标记 worker goroutine。
// 这种情况是无害的，因为已经有后台标记 worker 在运行。
// 如果此函数返回 false，则调用者不得成为空闲标记 worker。
//
// 因为它可能在没有 P 的情况下被调用，所以使用 nosplit。
//
//go:nosplit
func (c *gcControllerState) addIdleMarkWorker() bool {
	// 尝试添加一个新的空闲标记 worker。
	//
	// 如果返回 true，则调用者必须成为一个空闲标记 worker，除非池中没有后台标记 worker goroutine。
	// 这种情况是无害的，因为已经有后台标记 worker 在运行。
	// 如果返回 false，则调用者不得成为空闲标记 worker。
	//
	// 因为它可能在没有 P 的情况下被调用，所以 nosplit。
	for {
		// Load the current value of idleMarkWorkers.
		// 加载 idleMarkWorkers 的当前值。
		old := c.idleMarkWorkers.Load()
		// Extract the number of idle mark workers and the maximum number of idle mark workers from the loaded value.
		// 从加载的值中提取空闲标记 worker 的数量和空闲标记 worker 的最大数量。
		n, max := int32(old&uint64(^uint32(0))), int32(old>>32)
		// If the number of idle mark workers is greater than or equal to the maximum number of idle mark workers, return false.
		// 如果空闲标记 worker 的数量大于或等于空闲标记 worker 的最大数量，则返回 false。
		if n >= max {
			// See the comment on idleMarkWorkers for why
			// n > max is tolerated.
			return false
		}
		// If the number of idle mark workers is negative, throw an error.
		// 如果空闲标记 worker 的数量为负数，则抛出错误。
		if n < 0 {
			print("n=", n, " max=", max, "\n")
			throw("negative idle mark workers")
		}
		// Calculate the new value of idleMarkWorkers.
		// 计算 idleMarkWorkers 的新值。
		new := uint64(uint32(n+1)) | (uint64(max) << 32)
		// Atomically compare and swap the value of idleMarkWorkers.
		// 原子地比较和交换 idleMarkWorkers 的值。
		if c.idleMarkWorkers.CompareAndSwap(old, new) {
			// If the compare and swap was successful, return true.
			// 如果比较和交换成功，则返回 true。
			return true
		}
	}
}

// needIdleMarkWorker is a hint as to whether another idle mark worker is needed.
//
// The caller must still call addIdleMarkWorker to become one. This is mainly
// useful for a quick check before an expensive operation.
//
// nosplit because it may be called without a P.
//
// needIdleMarkWorker 是一个提示，用于判断是否需要另一个空闲标记 worker。
//
// 调用者必须仍然调用 addIdleMarkWorker 才能成为其中一个。这主要用于在昂贵的操作之前快速检查。
//
// 因为它可能在没有 P 的情况下被调用，所以使用 nosplit。
//
//go:nosplit
//go:nosplit
func (c *gcControllerState) needIdleMarkWorker() bool {
	// 加载当前空闲标记 worker 的数量和最大数量。
	p := c.idleMarkWorkers.Load()
	// 从加载的值中提取空闲标记 worker 的数量和空闲标记 worker 的最大数量。
	n, max := int32(p&uint64(^uint32(0))), int32(p>>32)
	// 如果当前空闲标记 worker 的数量小于最大数量，则返回 true，否则返回 false。
	return n < max
}

// removeIdleMarkWorker must be called when a new idle mark worker stops executing.
//
// 当一个新的空闲标记 worker 停止执行时，必须调用 removeIdleMarkWorker。
//
// 因为它可能在没有 P 的情况下被调用，所以使用 nosplit。
//
//go:nosplit
func (c *gcControllerState) removeIdleMarkWorker() {
	for {
		// 加载当前空闲标记 worker 的数量和最大数量。
		old := c.idleMarkWorkers.Load()
		// 从加载的值中提取空闲标记 worker 的数量和空闲标记 worker 的最大数量。
		n, max := int32(old&uint64(^uint32(0))), int32(old>>32)
		// 如果空闲标记 worker 的数量为负数，则抛出错误。
		if n-1 < 0 {
			print("n=", n, " max=", max, "\n")
			throw("negative idle mark workers")
		}
		new := uint64(uint32(n-1)) | (uint64(max) << 32)
		// 原子地比较和交换 idleMarkWorkers 的值。
		if c.idleMarkWorkers.CompareAndSwap(old, new) {
			// 如果比较和交换成功，则返回。
			return
		}
	}
}

// setMaxIdleMarkWorkers sets the maximum number of idle mark workers allowed.
//
// This method is optimistic in that it does not wait for the number of
// idle mark workers to reduce to max before returning; it assumes the workers
// will deschedule themselves.
//
// setMaxIdleMarkWorkers 设置允许的最大空闲标记 worker 数量。
//
// 此方法假设工作线程会自行调度，因此在返回之前不等待空闲标记 worker 数量减少到 max 以下。
//
// 因为它可能在没有 P 的情况下被调用，所以使用 nosplit。
func (c *gcControllerState) setMaxIdleMarkWorkers(max int32) {
	for {
		// Load the current value of idleMarkWorkers.
		// 加载 idleMarkWorkers 的当前值。
		old := c.idleMarkWorkers.Load()
		// Extract the current number of idle mark workers.
		// 提取当前空闲标记 worker 的数量。
		n := int32(old & uint64(^uint32(0)))
		// Check if the number of idle mark workers is negative.
		// 检查空闲标记 worker 的数量是否为负数。
		if n < 0 {
			print("n=", n, " max=", max, "\n")
			throw("negative idle mark workers")
		}
		// Create a new value with the updated maximum number of idle mark workers.
		// 创建一个新值，其中包含更新后的最大空闲标记 worker 数量。
		new := uint64(uint32(n)) | (uint64(max) << 32)
		// Atomically compare and swap the value of idleMarkWorkers.
		// 原子地比较和交换 idleMarkWorkers 的值。
		if c.idleMarkWorkers.CompareAndSwap(old, new) {
			return
		}
	}
}

// gcControllerCommit is gcController.commit, but passes arguments from live
// (non-test) data. It also updates any consumers of the GC pacing, such as
// sweep pacing and the background scavenger.
//
// Calls gcController.commit.
//
// The heap lock must be held, so this must be executed on the system stack.
//
// gcControllerCommit 是 gcController.commit 的一个包装，但传递的是来自 live（非测试）数据中的参数。
// 它还会更新 GC 步调的任何使用者，例如 sweep 步调和后台 scavenger。
//
// 调用 gcController.commit。
//
// 必须持有堆锁，因此必须在系统堆栈上执行此操作。
//
//go:systemstack
func gcControllerCommit() {
	assertWorldStoppedOrLockHeld(&mheap_.lock)
	// Assert that the world is stopped or the heap lock is held.
	// 断言世界已停止或持有堆锁。

	gcController.commit(isSweepDone())
	// Commit the current GC state.
	// 提交当前的 GC 状态。

	// Update mark pacing.
	// 更新标记步调。
	if gcphase != _GCoff {
		gcController.revise()
	}
	// If the GC phase is not off, revise the GC controller.
	// 如果 GC 阶段未关闭，则修改 GC 控制器。

	// TODO(mknyszek): This isn't really accurate any longer because the heap
	// goal is computed dynamically. Still useful to snapshot, but not as useful.
	trace := traceAcquire()
	// Acquire a trace.
	// 获取一个跟踪。
	if trace.ok() {
		trace.HeapGoal()
		// Record the heap goal in the trace.
		// 在跟踪中记录堆目标。
		traceRelease(trace)
	}
	// Release the trace.
	// 释放跟踪。

	trigger, heapGoal := gcController.trigger()
	// Get the GC trigger and heap goal from the GC controller.
	// 从 GC 控制器获取 GC 触发器和堆目标。
	gcPaceSweeper(trigger)
	// Pace the sweeper based on the GC trigger.
	// 根据 GC 触发器调整 sweeper 的速度。
	gcPaceScavenger(gcController.memoryLimit.Load(), heapGoal, gcController.lastHeapGoal)
	// Pace the scavenger based on the memory limit, heap goal, and last heap goal.
	// 根据内存限制、堆目标和上一个堆目标调整 scavenger 的速度。
}
