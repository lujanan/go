// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Garbage collector: marking and scanning

package runtime

import (
	"internal/abi"
	"internal/goarch"
	"internal/runtime/atomic"
	"runtime/internal/sys"
	"unsafe"
)

const (
	// 固定根对象的类型枚举
	fixedRootFinalizers  = iota // 终结器根对象
	fixedRootFreeGStacks        // 空闲G栈根对象
	fixedRootCount              // 根对象类型计数

	// rootBlockBytes is the number of bytes to scan per data or
	// BSS root.
	// rootBlockBytes 定义了每次扫描数据段或BSS段根对象时的字节数
	rootBlockBytes = 256 << 10 // 256KB

	// maxObletBytes is the maximum bytes of an object to scan at
	// once. Larger objects will be split up into "oblets" of at
	// most this size. Since we can scan 1–2 MB/ms, 128 KB bounds
	// scan preemption at ~100 µs.
	//
	// This must be > _MaxSmallSize so that the object base is the
	// span base.
	// maxObletBytes 定义了单个对象扫描时的最大字节数。
	// 更大的对象会被分割成不超过这个大小的"oblets"。
	// 由于扫描速度约为1-2MB/ms，128KB的限制使得扫描抢占时间约为100微秒。
	// 这个值必须大于_MaxSmallSize，以确保对象基址就是span基址。
	maxObletBytes = 128 << 10 // 128KB

	// drainCheckThreshold specifies how many units of work to do
	// between self-preemption checks in gcDrain. Assuming a scan
	// rate of 1 MB/ms, this is ~100 µs. Lower values have higher
	// overhead in the scan loop (the scheduler check may perform
	// a syscall, so its overhead is nontrivial). Higher values
	// make the system less responsive to incoming work.
	// drainCheckThreshold 指定了在gcDrain中进行自抢占检查之间要完成的工作单元数。
	// 假设扫描速率为1MB/ms，这相当于约100微秒。
	// 较低的值会增加扫描循环的开销（调度器检查可能涉及系统调用，开销不小）。
	// 较高的值会降低系统对传入工作的响应性。
	drainCheckThreshold = 100000

	// pagesPerSpanRoot indicates how many pages to scan from a span root
	// at a time. Used by special root marking.
	//
	// Higher values improve throughput by increasing locality, but
	// increase the minimum latency of a marking operation.
	//
	// Must be a multiple of the pageInUse bitmap element size and
	// must also evenly divide pagesPerArena.
	// pagesPerSpanRoot 定义了每次从span根对象扫描的页数。用于特殊根对象标记。
	// 较高的值通过增加局部性来提高吞吐量，但会增加标记操作的最小延迟。
	// 必须是pageInUse位图元素大小的倍数，并且必须能整除pagesPerArena。
	pagesPerSpanRoot = 512
)

// gcMarkRootPrepare queues root scanning jobs (stacks, globals, and
// some miscellany) and initializes scanning-related state.
//
// The world must be stopped.
// gcMarkRootPrepare 函数用于准备根对象扫描工作，包括栈、全局变量和其他杂项，
// 并初始化与扫描相关的状态。调用此函数时，必须停止世界（STW）。
func gcMarkRootPrepare() {
	assertWorldStopped()

	// Compute how many data and BSS root blocks there are.
	// 计算数据段和BSS段根对象的块数
	nBlocks := func(bytes uintptr) int {
		return int(divRoundUp(bytes, rootBlockBytes))
	}

	// 初始化数据段和BSS段根对象计数
	work.nDataRoots = 0
	work.nBSSRoots = 0

	// Scan globals.
	// 扫描全局变量
	for _, datap := range activeModules() {
		// 计算数据段根对象块数
		nDataRoots := nBlocks(datap.edata - datap.data)
		if nDataRoots > work.nDataRoots {
			work.nDataRoots = nDataRoots
		}

		// 计算BSS段根对象块数
		nBSSRoots := nBlocks(datap.ebss - datap.bss)
		if nBSSRoots > work.nBSSRoots {
			work.nBSSRoots = nBSSRoots
		}
	}

	// Scan span roots for finalizer specials.
	//
	// We depend on addfinalizer to mark objects that get
	// finalizers after root marking.
	//
	// We're going to scan the whole heap (that was available at the time the
	// mark phase started, i.e. markArenas) for in-use spans which have specials.
	//
	// Break up the work into arenas, and further into chunks.
	//
	// Snapshot allArenas as markArenas. This snapshot is safe because allArenas
	// is append-only.
	// 扫描span根对象以查找终结器特殊对象
	// 我们依赖addfinalizer来标记在根对象标记之后获得终结器的对象
	// 我们将扫描整个堆（在标记阶段开始时可用的堆，即markArenas）中具有特殊对象的正在使用的span
	// 将工作分解为arena，并进一步分解为块
	// 将allArenas快照为markArenas。这个快照是安全的，因为allArenas是只追加的
	mheap_.markArenas = mheap_.allArenas[:len(mheap_.allArenas):len(mheap_.allArenas)]
	work.nSpanRoots = len(mheap_.markArenas) * (pagesPerArena / pagesPerSpanRoot)

	// Scan stacks.
	//
	// Gs may be created after this point, but it's okay that we
	// ignore them because they begin life without any roots, so
	// there's nothing to scan, and any roots they create during
	// the concurrent phase will be caught by the write barrier.
	// 扫描栈
	// 在此点之后可能会创建新的G，但我们可以忽略它们，因为它们开始时没有任何根对象，
	// 所以没有需要扫描的内容，它们在并发阶段创建的任何根对象都会被写屏障捕获
	work.stackRoots = allGsSnapshot()
	work.nStackRoots = len(work.stackRoots)

	// 初始化下一个要扫描的根对象索引
	work.markrootNext = 0
	// 计算总的根对象扫描任务数
	work.markrootJobs = uint32(fixedRootCount + work.nDataRoots + work.nBSSRoots + work.nSpanRoots + work.nStackRoots)

	// Calculate base indexes of each root type
	// 计算每种根对象类型的基准索引
	work.baseData = uint32(fixedRootCount)
	work.baseBSS = work.baseData + uint32(work.nDataRoots)
	work.baseSpans = work.baseBSS + uint32(work.nBSSRoots)
	work.baseStacks = work.baseSpans + uint32(work.nSpanRoots)
	work.baseEnd = work.baseStacks + uint32(work.nStackRoots)
}

// gcMarkRootCheck checks that all roots have been scanned. It is
// purely for debugging.
// gcMarkRootCheck 检查是否所有根对象都已被扫描。这个函数仅用于调试目的。
func gcMarkRootCheck() {
	// 检查是否所有根对象扫描任务都已完成
	// 如果还有未完成的扫描任务，则打印错误信息并抛出异常
	if work.markrootNext < work.markrootJobs {
		print(work.markrootNext, " of ", work.markrootJobs, " markroot jobs done\n")
		throw("left over markroot jobs")
	}

	// Check that stacks have been scanned.
	//
	// We only check the first nStackRoots Gs that we should have scanned.
	// Since we don't care about newer Gs (see comment in
	// gcMarkRootPrepare), no locking is required.
	// 检查所有栈是否已被扫描
	//
	// 我们只检查应该被扫描的前nStackRoots个G
	// 由于我们不关心新创建的G（参见gcMarkRootPrepare中的注释），所以不需要加锁
	i := 0
	forEachGRace(func(gp *g) {
		// 如果已经检查了足够数量的G，则返回
		if i >= work.nStackRoots {
			return
		}

		// 如果发现某个G的栈未被扫描，则打印该G的信息并抛出异常
		if !gp.gcscandone {
			println("gp", gp, "goid", gp.goid,
				"status", readgstatus(gp),
				"gcscandone", gp.gcscandone)
			throw("scan missed a g")
		}

		i++
	})
}

// ptrmask for an allocation containing a single pointer.
// oneptrmask 是一个包含单个指针的分配的位掩码
var oneptrmask = [...]uint8{1}

// markroot scans the i'th root.
//
// Preemption must be disabled (because this uses a gcWork).
//
// Returns the amount of GC work credit produced by the operation.
// If flushBgCredit is true, then that credit is also flushed
// to the background credit pool.
//
// nowritebarrier is only advisory here.
// markroot 扫描第 i 个根对象。
//
// 必须禁用抢占 (因为这使用了一个 gcWork)。
//
// 返回此操作产生的 GC 工作量。
// 如果 flushBgCredit 为 true，则该信用额度也会被刷新到后台信用池。
//
// nowritebarrier 在这里只是建议性的。
//
//go:nowritebarrier
func markroot(gcw *gcWork, i uint32, flushBgCredit bool) int64 {
	// Note: if you add a case here, please also update heapdump.go:dumproots.
	// 注意：如果在此处添加 case，请同时更新 heapdump.go:dumproots。
	var workDone int64
	var workCounter *atomic.Int64
	switch {
	case work.baseData <= i && i < work.baseBSS:
		// Scan data roots.
		// 扫描 data 根。
		workCounter = &gcController.globalsScanWork
		for _, datap := range activeModules() {
			// Mark root block for data segment.
			// 标记数据段的根块。
			workDone += markrootBlock(datap.data, datap.edata-datap.data, datap.gcdatamask.bytedata, gcw, int(i-work.baseData))
		}

	case work.baseBSS <= i && i < work.baseSpans:
		// Scan BSS roots.
		// 扫描 BSS 根。
		workCounter = &gcController.globalsScanWork
		for _, datap := range activeModules() {
			// Mark root block for BSS segment.
			// 标记 BSS 段的根块。
			workDone += markrootBlock(datap.bss, datap.ebss-datap.bss, datap.gcbssmask.bytedata, gcw, int(i-work.baseBSS))
		}

	case i == fixedRootFinalizers:
		// Scan finalizers.
		// 扫描 finalizer。
		for fb := allfin; fb != nil; fb = fb.alllink {
			cnt := uintptr(atomic.Load(&fb.cnt))
			scanblock(uintptr(unsafe.Pointer(&fb.fin[0])), cnt*unsafe.Sizeof(fb.fin[0]), &finptrmask[0], gcw, nil)
		}

	case i == fixedRootFreeGStacks:
		// Scan free G stacks.
		// 扫描空闲 G 栈。
		// Switch to the system stack so we can call
		// stackfree.
		// 切换到系统栈，以便可以调用 stackfree。
		systemstack(markrootFreeGStacks)

	case work.baseSpans <= i && i < work.baseStacks:
		// mark mspan.specials
		// 标记 mspan.specials
		markrootSpans(gcw, int(i-work.baseSpans))

	default:
		// the rest is scanning goroutine stacks
		// 其余情况是扫描 goroutine 栈
		workCounter = &gcController.stackScanWork
		// 如果 i 不在栈根的范围内，则抛出异常
		if i < work.baseStacks || work.baseEnd <= i {
			printlock()
			print("runtime: markroot index ", i, " not in stack roots range [", work.baseStacks, ", ", work.baseEnd, ")\n")
			throw("markroot: bad index")
		}
		// 获取要扫描的 goroutine
		gp := work.stackRoots[i-work.baseStacks]

		// remember when we've first observed the G blocked
		// needed only to output in traceback
		// 记录 G 阻塞的起始时间，用于 traceback
		status := readgstatus(gp) // We are not in a scan state
		// 如果 G 处于等待或 syscall 状态，并且 waitsince 为 0，则记录 waitsince
		if (status == _Gwaiting || status == _Gsyscall) && gp.waitsince == 0 {
			gp.waitsince = work.tstart
		}

		// scanstack must be done on the system stack in case
		// we're trying to scan our own stack.
		// 如果正在扫描自己的栈，必须在系统栈上执行 scanstack
		systemstack(func() {
			// If this is a self-scan, put the user G in
			// _Gwaiting to prevent self-deadlock. It may
			// already be in _Gwaiting if this is a mark
			// worker or we're in mark termination.
			// 如果这是自扫描，将用户 G 置于 _Gwaiting 以防止自死锁。
			// 如果这是标记工作程序或我们处于标记终止阶段，它可能已经处于 _Gwaiting 状态。
			userG := getg().m.curg                                     // 获取当前 M 的 G
			selfScan := gp == userG && readgstatus(userG) == _Grunning // 检查是否是自扫描，即当前 G 和要扫描的 G 是同一个，并且当前 G 处于 _Grunning 状态
			if selfScan {
				casGToWaitingForGC(userG, _Grunning, waitReasonGarbageCollectionScan) // 如果是自扫描，则将当前 G 的状态从 _Grunning 切换到 _Gwaiting，防止死锁
			}

			// TODO: suspendG blocks (and spins) until gp
			// stops, which may take a while for
			// running goroutines. Consider doing this in
			// two phases where the first is non-blocking:
			// we scan the stacks we can and ask running
			// goroutines to scan themselves; and the
			// second blocks.
			// TODO: suspendG 会阻塞（并自旋）直到 gp 停止，对于正在运行的 goroutine 来说，这可能需要一段时间。
			// 考虑分两个阶段进行，第一个阶段是非阻塞的：我们扫描可以扫描的栈，并要求正在运行的 goroutine 扫描它们自己；
			// 第二个阶段是阻塞的。
			// TODO: suspendG blocks (and spins) until gp stops, which may take a while for running goroutines. Consider doing this in two phases where the first is non-blocking: we scan the stacks we can and ask running goroutines to scan themselves; and the second blocks.
			// TODO: suspendG 阻塞并循环等待直到 gp 停止，对于正在运行的 goroutine 来说，这可能需要一段时间。
			// 考虑将其分为两个阶段，第一个阶段是非阻塞的：我们扫描可以扫描的栈，并要求正在运行的 goroutine 扫描它们自己；第二个阶段是阻塞的。
			// TODO: suspendG 阻塞（并自旋）直到 gp 停止，对于正在运行的 goroutine 来说，这可能需要一段时间。
			// 考虑分两个阶段进行，第一个阶段是非阻塞的：我们扫描可以扫描的栈，并要求正在运行的 goroutine 扫描它们自己；
			// 第二个阶段是阻塞的。
			stopped := suspendG(gp) // 暂停 G，直到 G 停止
			if stopped.dead {       // 如果 G 已经死亡
				gp.gcscandone = true // 标记 G 已经扫描完成
				return               // 直接返回
			}
			if gp.gcscandone { // 如果 G 已经扫描完成
				throw("g already scanned") // 抛出异常，表示 G 已经被扫描
			}
			workDone += scanstack(gp, gcw) // 扫描 G 的栈
			gp.gcscandone = true           // 标记 G 已经扫描完成
			resumeG(stopped)               // 恢复 G 的运行

			if selfScan { // 如果是自扫描
				casgstatus(userG, _Gwaiting, _Grunning) // 将当前 G 的状态从 _Gwaiting 切换回 _Grunning
			}
		})
	}
	if workCounter != nil && workDone != 0 {
		// If a work counter is provided and we did some work...
		// 如果提供了工作计数器并且我们完成了一些工作...
		workCounter.Add(workDone)
		// Add the work we did to the counter.
		// 将我们完成的工作添加到计数器。
		if flushBgCredit {
			// If we should flush background GC credit...
			// 如果我们应该刷新后台 GC 信用...
			gcFlushBgCredit(workDone)
			// Flush the background GC credit. This helps background
			// GC keep up with allocation.
			// 刷新后台 GC 信用。这有助于后台 GC 跟上分配。
		}
	}
	return workDone
	// Return the amount of work we did.
	// 返回我们完成的工作量。
}

// markrootBlock scans the shard'th shard of the block of memory [b0,
// b0+n0), with the given pointer mask.
//
// Returns the amount of work done.
// markrootBlock扫描内存块[b0, b0+n0)的第shard个分片，使用给定的指针掩码。
//
// 返回完成的工作量。
// 例如，如果b0指向一块大小为n0的内存区域的起始地址，
// 并且ptrmask0是指向该内存区域的指针掩码的指针，
// shard指定了将该内存区域划分成的多个分片中的一个分片，
// 则此函数将扫描该分片并返回扫描期间完成的工作量。
// 该函数通常在垃圾回收期间用于扫描根对象。
// b0: 指向要扫描的内存块的起始地址。
// n0: 要扫描的内存块的大小。
// ptrmask0: 指向内存块的指针掩码的指针。指针掩码用于指示内存块中哪些字包含指针。
// gcw: 指向gcWork结构体的指针，该结构体用于管理垃圾回收工作。
// shard: 要扫描的分片的索引。
// 返回值：完成的工作量。
//
//go:nowritebarrier
func markrootBlock(b0, n0 uintptr, ptrmask0 *uint8, gcw *gcWork, shard int) int64 {
	// markrootBlock扫描内存块[b0, b0+n0)的第shard个分片，使用给定的指针掩码。
	//
	// 返回完成的工作量。

	if rootBlockBytes%(8*goarch.PtrSize) != 0 {
		// This is necessary to pick byte offsets in ptrmask0.
		// 这是为了选择ptrmask0中的字节偏移量。
		throw("rootBlockBytes must be a multiple of 8*ptrSize")
	}

	// Note that if b0 is toward the end of the address space,
	// then b0 + rootBlockBytes might wrap around.
	// These tests are written to avoid any possible overflow.
	// 注意，如果b0接近地址空间的末尾，那么b0 + rootBlockBytes可能会回绕。
	// 这些测试是为了避免任何可能的溢出而编写的。
	off := uintptr(shard) * rootBlockBytes
	// Calculate the offset for the current shard.
	// 计算当前分片的偏移量。
	if off >= n0 {
		return 0
	}
	// If the offset is beyond the size of the block, return 0 (no work to do).
	// 如果偏移量超过块的大小，则返回 0（没有工作要做）。
	b := b0 + off
	// Calculate the starting address of the current shard.
	// 计算当前分片的起始地址。
	ptrmask := (*uint8)(add(unsafe.Pointer(ptrmask0), uintptr(shard)*(rootBlockBytes/(8*goarch.PtrSize))))
	// Calculate the pointer mask for the current shard.
	// 计算当前分片的指针掩码。
	n := uintptr(rootBlockBytes)
	// Set the size of the current shard to the default rootBlockBytes.
	// 将当前分片的大小设置为默认的 rootBlockBytes。
	if off+n > n0 {
		n = n0 - off
	}
	// If the current shard extends beyond the end of the block, truncate it.
	// 如果当前分片超出块的末尾，则截断它。

	// Scan this shard.
	// 扫描这个分片。
	scanblock(b, n, ptrmask, gcw, nil)
	return int64(n)
}

// markrootFreeGStacks frees stacks of dead Gs.
//
// This does not free stacks of dead Gs cached on Ps, but having a few
// cached stacks around isn't a problem.
// markrootFreeGStacks 释放死亡 G 的堆栈。
//
// 这不会释放 P 上缓存的死亡 G 的堆栈，但是保留一些缓存的堆栈没有问题。
func markrootFreeGStacks() {
	// Take list of dead Gs with stacks.
	// 获取带有堆栈的死亡 G 的列表。
	lock(&sched.gFree.lock)
	list := sched.gFree.stack
	sched.gFree.stack = gList{}
	unlock(&sched.gFree.lock)
	if list.empty() {
		return
	}

	// Free stacks.
	// 释放堆栈。
	q := gQueue{list.head, list.head}
	for gp := list.head.ptr(); gp != nil; gp = gp.schedlink.ptr() {
		stackfree(gp.stack)
		gp.stack.lo = 0
		gp.stack.hi = 0
		// Manipulate the queue directly since the Gs are
		// already all linked the right way.
		// 直接操作队列，因为 Gs 已经以正确的方式链接在一起。
		q.tail.set(gp)
	}

	// Put Gs back on the free list.
	// 将 Gs 放回空闲列表。
	lock(&sched.gFree.lock)
	sched.gFree.noStack.pushAll(q)
	unlock(&sched.gFree.lock)
}

// markrootSpans marks roots for one shard of markArenas.
// markrootSpans 标记 markArenas 的一个分片中的根。
//
//go:nowritebarrier
func markrootSpans(gcw *gcWork, shard int) {
	// Objects with finalizers have two GC-related invariants:
	//
	// 1) Everything reachable from the object must be marked.
	// This ensures that when we pass the object to its finalizer,
	// everything the finalizer can reach will be retained.
	//
	// 2) Finalizer specials (which are not in the garbage
	// collected heap) are roots. In practice, this means the fn
	// field must be scanned.
	//
	// Objects with weak handles have only one invariant related
	// to this function: weak handle specials (which are not in the
	// garbage collected heap) are roots. In practice, this means
	// the handle field must be scanned. Note that the value the
	// handle pointer referenced does *not* need to be scanned. See
	// the definition of specialWeakHandle for details.

	// 带有 finalizer 的对象有两个与 GC 相关的约束：
	//
	// 1) 从对象可达的所有内容都必须被标记。
	// 这确保了当我们把对象传递给它的 finalizer 时，
	// finalizer 可以访问的所有内容都将被保留。
	//
	// 2) Finalizer specials（不在垃圾回收堆中）是根。
	// 实际上，这意味着必须扫描 fn 字段。
	//
	// 带有 weak handle 的对象只有一个与此函数相关的约束：
	// weak handle specials（不在垃圾回收堆中）是根。
	// 实际上，这意味着必须扫描 handle 字段。
	// 注意，handle 指针引用的值 *不需要* 被扫描。
	// 参见 specialWeakHandle 的定义以了解详情。
	sg := mheap_.sweepgen

	// Find the arena and page index into that arena for this shard.
	// 找到 arena 和该 arena 中对应于此分片的页索引。
	ai := mheap_.markArenas[shard/(pagesPerArena/pagesPerSpanRoot)]
	ha := mheap_.arenas[ai.l1()][ai.l2()]
	arenaPage := uint(uintptr(shard) * pagesPerSpanRoot % pagesPerArena)

	// Construct slice of bitmap which we'll iterate over.
	// 构建我们将要迭代的位图切片。
	specialsbits := ha.pageSpecials[arenaPage/8:]
	specialsbits = specialsbits[:pagesPerSpanRoot/8]
	for i := range specialsbits {
		// Find set bits, which correspond to spans with specials.
		// 查找设置的位，这些位对应于具有 specials 的 spans。
		specials := atomic.Load8(&specialsbits[i])
		if specials == 0 {
			continue
		}
		for j := uint(0); j < 8; j++ {
			if specials&(1<<j) == 0 {
				continue
			}
			// Find the span for this bit.
			// 找到此位的 span。
			//
			// This value is guaranteed to be non-nil because having
			// specials implies that the span is in-use, and since we're
			// currently marking we can be sure that we don't have to worry
			// about the span being freed and re-used.
			// 此值保证为非 nil，因为具有 specials 意味着 span 正在使用中，并且由于我们当前正在标记，
			// 我们可以确定不必担心 span 被释放和重用。
			s := ha.spans[arenaPage+uint(i)*8+j]

			// The state must be mSpanInUse if the specials bit is set, so
			// sanity check that.
			// 如果设置了 specials 位，则状态必须为 mSpanInUse，因此进行完整性检查。
			if state := s.state.get(); state != mSpanInUse {
				print("s.state = ", state, "\n")
				throw("non in-use span found with specials bit set")
			}
			// Check that this span was swept (it may be cached or uncached).
			// 检查此 span 是否已被清除（它可能是缓存的或未缓存的）。
			if !useCheckmark && !(s.sweepgen == sg || s.sweepgen == sg+3) {
				// sweepgen was updated (+2) during non-checkmark GC pass
				// sweepgen 在非 checkmark GC 期间已更新 (+2)
				print("sweep ", s.sweepgen, " ", sg, "\n")
				throw("gc: unswept span")
			}

			// Lock the specials to prevent a special from being
			// removed from the list while we're traversing it.
			// 锁定 specials 以防止在遍历列表时从列表中删除 special。
			lock(&s.speciallock)
			for sp := s.specials; sp != nil; sp = sp.next {
				switch sp.kind {
				case _KindSpecialFinalizer:
					// don't mark finalized object, but scan it so we
					// retain everything it points to.
					// 不要标记已完成的对象，但扫描它，以便我们保留它指向的所有内容。
					spf := (*specialfinalizer)(unsafe.Pointer(sp))
					// A finalizer can be set for an inner byte of an object, find object beginning.
					// 可以为对象的内部字节设置 finalizer，找到对象开头。
					p := s.base() + uintptr(spf.special.offset)/s.elemsize*s.elemsize

					// Mark everything that can be reached from
					// the object (but *not* the object itself or
					// we'll never collect it).
					// 标记可以从对象访问的所有内容（但 *不* 标记对象本身，否则我们永远不会收集它）。
					if !s.spanclass.noscan() {
						scanobject(p, gcw)
					}

					// The special itself is a root.
					// special 本身是一个根。
					scanblock(uintptr(unsafe.Pointer(&spf.fn)), goarch.PtrSize, &oneptrmask[0], gcw, nil)
				case _KindSpecialWeakHandle:
					// The special itself is a root.
					// special 本身是一个根。
					spw := (*specialWeakHandle)(unsafe.Pointer(sp))
					scanblock(uintptr(unsafe.Pointer(&spw.handle)), goarch.PtrSize, &oneptrmask[0], gcw, nil)
				}
			}
			unlock(&s.speciallock)
		}
	}
}

// gcAssistAlloc performs GC work to make gp's assist debt positive.
// gp must be the calling user goroutine.
//
// This must be called with preemption enabled.
// gcAssistAlloc 执行 GC 工作以使 gp 的 assist 债务为正。
// gp 必须是调用用户 goroutine。
//
// 必须在抢占启用的情况下调用。
func gcAssistAlloc(gp *g) {
	// Don't assist in non-preemptible contexts. These are
	// generally fragile and won't allow the assist to block.
	// 不要在不可抢占的上下文中进行辅助。这些上下文通常很脆弱，不允许辅助阻塞。
	if getg() == gp.m.g0 {
		// The current G is the system stack.
		// 当前的 G 是系统堆栈。
		return
	}
	if mp := getg().m; mp.locks > 0 || mp.preemptoff != "" {
		// The current M has locks held or preemption is disabled.
		// 当前的 M 持有锁或禁用了抢占。
		return
	}

	// This extremely verbose boolean indicates whether we've
	// entered mark assist from the perspective of the tracer.
	//
	// In the tracer, this is just before we call gcAssistAlloc1
	// *regardless* of whether tracing is enabled. This is because
	// the tracer allows for tracing to begin (and advance
	// generations) in the middle of a GC mark phase, so we need to
	// record some state so that the tracer can pick it up to ensure
	// a consistent trace result.
	//
	// TODO(mknyszek): Hide the details of inMarkAssist in tracer
	// functions and simplify all the state tracking. This is a lot.
	//
	// enteredMarkAssistForTracing 是一个非常冗长的布尔值，用于指示我们是否从 tracer 的角度进入了 mark assist。
	//
	// 在 tracer 中，这正好在我们调用 gcAssistAlloc1 之前，*无论* 是否启用了 tracing。
	// 这是因为 tracer 允许在 GC 标记阶段的中间开始（并推进 generations）tracing，
	// 因此我们需要记录一些状态，以便 tracer 可以获取它以确保一致的 trace 结果。
	//
	// TODO(mknyszek): 将 inMarkAssist 的细节隐藏在 tracer 函数中，并简化所有状态跟踪。这太多了。
	// enteredMarkAssistForTracing 变量用于在 tracing 的角度上，标记是否进入了 mark assist 阶段。
	// 它的作用是在 tracer 中，当 tracing 可以在 GC 标记阶段的中间开始时，记录状态，以确保 trace 结果的一致性。
	// 即使 tracing 没有启用，这个状态也会被跟踪，它只被新的 tracer 使用。
	// 参见 enteredMarkAssistForTracing 上的注释。
	// 这个变量在 gcAssistAlloc1 调用之前被设置，用于指示是否进入了 mark assist 阶段。
	// 它的值会被 tracer 使用，以确保 trace 结果的一致性。
	// TODO(mknyszek): 考虑将 inMarkAssist 的细节隐藏在 tracer 函数中，并简化所有状态跟踪。
	// 当前的状态跟踪方式过于复杂。
	// 初始化 enteredMarkAssistForTracing 变量为 false，表示尚未进入 mark assist 阶段。
	enteredMarkAssistForTracing := false
retry:
	if gcCPULimiter.limiting() {
		// If the CPU limiter is enabled, intentionally don't
		// assist to reduce the amount of CPU time spent in the GC.
		// 如果启用了 CPU 限制器，则故意不进行辅助，以减少在 GC 中花费的 CPU 时间。
		if enteredMarkAssistForTracing {
			// If we entered mark assist for tracing, we need to clean up the tracing state.
			// 如果我们为了追踪的目的进入了 mark assist，我们需要清理追踪状态。
			trace := traceAcquire()
			// Acquire the trace context.
			// 获取追踪上下文。
			if trace.ok() {
				// If tracing is enabled.
				// 如果启用了追踪。
				trace.GCMarkAssistDone()
				// Mark the end of the GC mark assist.
				// 标记 GC mark assist 的结束。
				// Set this *after* we trace the end to make sure
				// that we emit an in-progress event if this is
				// the first event for the goroutine in the trace
				// or trace generation. Also, do this between
				// acquire/release because this is part of the
				// goroutine's trace state, and it must be atomic
				// with respect to the tracer.
				// 在我们追踪结束后设置此值，以确保如果这是 goroutine 在追踪或追踪生成中的第一个事件，我们会发出一个正在进行的事件。
				// 此外，在 acquire/release 之间执行此操作，因为这是 goroutine 追踪状态的一部分，并且必须相对于追踪器是原子的。
				gp.inMarkAssist = false
				// Reset the inMarkAssist flag.
				// 重置 inMarkAssist 标志。
				traceRelease(trace)
				// Release the trace context.
				// 释放追踪上下文。
			} else {
				// This state is tracked even if tracing isn't enabled.
				// It's only used by the new tracer.
				// See the comment on enteredMarkAssistForTracing.
				// 即使未启用追踪，也会跟踪此状态。它仅由新的追踪器使用。
				// 请参阅 enteredMarkAssistForTracing 上的注释。
				gp.inMarkAssist = false
				// Reset the inMarkAssist flag.
				// 重置 inMarkAssist 标志。
			}
		}
		return
		// Return without assisting.
		// 不进行辅助直接返回。
	}
	// Compute the amount of scan work we need to do to make the
	// balance positive. When the required amount of work is low,
	// we over-assist to build up credit for future allocations
	// and amortize the cost of assisting.
	// 计算我们需要做的扫描工作量，以使余额为正。当所需的工作量较低时，我们会过度辅助以建立未来分配的信用并分摊辅助成本。
	assistWorkPerByte := gcController.assistWorkPerByte.Load()
	// Load the amount of work per byte of allocation.
	// 加载每字节分配的工作量。
	assistBytesPerWork := gcController.assistBytesPerWork.Load()
	// Load the number of bytes allocated per unit of work.
	// 加载每个工作单元分配的字节数。
	debtBytes := -gp.gcAssistBytes
	// Calculate the amount of debt the current goroutine has.
	// 计算当前 goroutine 的债务量。
	scanWork := int64(assistWorkPerByte * float64(debtBytes))
	// Calculate the amount of scan work required to pay off the debt.
	// 计算偿还债务所需的扫描工作量。
	if scanWork < gcOverAssistWork {
		// If the amount of scan work is less than the over-assist threshold,
		// increase the amount of scan work to the over-assist threshold.
		// 如果扫描工作量小于过度辅助阈值，则将扫描工作量增加到过度辅助阈值。
		scanWork = gcOverAssistWork
		debtBytes = int64(assistBytesPerWork * float64(scanWork))
		// Recalculate the amount of debt based on the increased scan work.
		// 根据增加的扫描工作量重新计算债务量。
	}

	// Steal as much credit as we can from the background GC's
	// scan credit. This is racy and may drop the background
	// credit below 0 if two mutators steal at the same time. This
	// will just cause steals to fail until credit is accumulated
	// again, so in the long run it doesn't really matter, but we
	// do have to handle the negative credit case.
	// 尽可能从后台 GC 的扫描信用中窃取信用。这是有竞争条件的，如果两个 mutator 同时窃取，可能会导致后台信用低于 0。
	// 这只会导致窃取失败，直到再次积累信用，所以从长远来看，这并不重要，但我们必须处理负信用情况。
	bgScanCredit := gcController.bgScanCredit.Load()
	// Load the current background scan credit.
	// 加载当前后台扫描信用。
	stolen := int64(0)
	if bgScanCredit > 0 {
		// If there is background scan credit available.
		// 如果有可用的后台扫描信用。
		if bgScanCredit < scanWork {
			// If the background scan credit is less than the required scan work.
			// 如果后台扫描信用小于所需的扫描工作量。
			stolen = bgScanCredit
			// Steal all the available background scan credit.
			// 窃取所有可用的后台扫描信用。
			gp.gcAssistBytes += 1 + int64(assistBytesPerWork*float64(stolen))
			// Update the goroutine's assist bytes based on the stolen credit.
			// 根据窃取的信用更新 goroutine 的辅助字节。
		} else {
			// If the background scan credit is enough to cover the required scan work.
			// 如果后台扫描信用足以支付所需的扫描工作量。
			stolen = scanWork
			// Steal the required amount of scan work from the background scan credit.
			// 从后台扫描信用中窃取所需的扫描工作量。
			gp.gcAssistBytes += debtBytes
			// Update the goroutine's assist bytes based on the debt.
			// 根据债务更新 goroutine 的辅助字节。
		}
		gcController.bgScanCredit.Add(-stolen)
		// Reduce the background scan credit by the amount stolen.
		// 将后台扫描信用减少窃取的数量。

		scanWork -= stolen
		// Reduce the required scan work by the amount stolen.
		// 将所需的扫描工作量减少窃取的数量。

		if scanWork == 0 {
			// We were able to steal all of the credit we
			// needed.
			// 如果我们能够窃取所有需要的信用。
			if enteredMarkAssistForTracing {
				// If we entered mark assist for tracing.
				// 如果我们为了追踪而进入了标记辅助。
				trace := traceAcquire()
				if trace.ok() {
					trace.GCMarkAssistDone()
					// Set this *after* we trace the end to make sure
					// that we emit an in-progress event if this is
					// the first event for the goroutine in the trace
					// or trace generation. Also, do this between
					// acquire/release because this is part of the
					// goroutine's trace state, and it must be atomic
					// with respect to the tracer.
					// 在我们追踪结束之后设置这个，以确保如果这是 goroutine 在追踪或追踪生成中的第一个事件，我们会发出一个正在进行的事件。
					// 此外，在 acquire/release 之间执行此操作，因为这是 goroutine 追踪状态的一部分，并且对于追踪器必须是原子的。
					gp.inMarkAssist = false
					// Indicate that the goroutine is no longer in mark assist.
					// 指示 goroutine 不再处于标记辅助状态。
					traceRelease(trace)
					// Release the trace.
					// 释放追踪。
				} else {
					// This state is tracked even if tracing isn't enabled.
					// It's only used by the new tracer.
					// See the comment on enteredMarkAssistForTracing.
					// 即使未启用追踪，也会追踪此状态。它仅由新的追踪器使用。
					// 请参阅 enteredMarkAssistForTracing 上的注释。
					gp.inMarkAssist = false
					// Indicate that the goroutine is no longer in mark assist.
					// 指示 goroutine 不再处于标记辅助状态。
				}
			}
			return
		}
	}
	if !enteredMarkAssistForTracing {
		// If we haven't entered mark assist for tracing yet.
		// 如果我们尚未进入标记辅助进行追踪。
		trace := traceAcquire()
		// Acquire a trace.
		// 获取一个追踪。
		if trace.ok() {
			// If tracing is enabled.
			// 如果启用了追踪。
			trace.GCMarkAssistStart()
			// Trace the start of mark assist.
			// 追踪标记辅助的开始。
			// Set this *after* we trace the start, otherwise we may
			// emit an in-progress event for an assist we're about to start.
			// 在我们追踪开始之后设置这个，否则我们可能会为一个即将开始的辅助发出一个正在进行的事件。
			gp.inMarkAssist = true
			// Indicate that the goroutine is in mark assist.
			// 指示 goroutine 正在进行标记辅助。
			traceRelease(trace)
			// Release the trace.
			// 释放追踪。
		} else {
			gp.inMarkAssist = true
			// Indicate that the goroutine is in mark assist.
			// 指示 goroutine 正在进行标记辅助。
		}
		// In the new tracer, set enter mark assist tracing if we
		// ever pass this point, because we must manage inMarkAssist
		// correctly.
		// 在新的追踪器中，如果我们通过此点，则设置进入标记辅助追踪，因为我们必须正确管理 inMarkAssist。
		//
		// See the comment on enteredMarkAssistForTracing.
		// 请参阅 enteredMarkAssistForTracing 上的注释。
		enteredMarkAssistForTracing = true
		// Indicate that we have entered mark assist for tracing.
		// 指示我们已经进入标记辅助进行追踪。
	}

	// Perform assist work
	// 执行辅助工作
	systemstack(func() {
		gcAssistAlloc1(gp, scanWork)
		// The user stack may have moved, so this can't touch
		// anything on it until it returns from systemstack.
		// 用户堆栈可能已经移动，所以在从 systemstack 返回之前，不能接触它上面的任何东西。
	})

	completed := gp.param != nil
	// Check if the assist work is completed.
	// 检查辅助工作是否完成。
	gp.param = nil
	// Clear the parameter.
	// 清除参数。
	if completed {
		gcMarkDone()
		// Mark the garbage collection as done.
		// 标记垃圾回收已完成。
	}

	if gp.gcAssistBytes < 0 {
		// We were unable steal enough credit or perform
		// enough work to pay off the assist debt. We need to
		// do one of these before letting the mutator allocate
		// more to prevent over-allocation.
		// 我们无法窃取足够的信用或执行足够的工作来偿还辅助债务。在允许 mutator 分配更多内存以防止过度分配之前，我们需要执行其中一项操作。
		//
		// If this is because we were preempted, reschedule
		// and try some more.
		// 如果这是因为我们被抢占了，重新调度并尝试更多。
		if gp.preempt {
			Gosched()
			goto retry
		}

		// Add this G to an assist queue and park. When the GC
		// has more background credit, it will satisfy queued
		// assists before flushing to the global credit pool.
		// 将此 G 添加到辅助队列并停放。当 GC 具有更多后台信用时，它将满足排队的辅助，然后再刷新到全局信用池。
		//
		// Note that this does *not* get woken up when more
		// work is added to the work list. The theory is that
		// there wasn't enough work to do anyway, so we might
		// as well let background marking take care of the
		// work that is available.
		// 请注意，当更多工作添加到工作列表时，这不会被唤醒。理论是无论如何都没有足够的工作要做，所以我们不妨让后台标记来处理可用的工作。
		if !gcParkAssist() {
			goto retry
		}

		// At this point either background GC has satisfied
		// this G's assist debt, or the GC cycle is over.
		// 此时，后台 GC 要么已经满足了此 G 的辅助债务，要么 GC 周期已经结束。
	}
	if enteredMarkAssistForTracing {
		// If mark assist tracing was entered.
		// 如果进入了标记辅助跟踪。
		trace := traceAcquire()
		// Acquire the trace context.
		// 获取跟踪上下文。
		if trace.ok() {
			// If tracing is enabled.
			// 如果启用了跟踪。
			trace.GCMarkAssistDone()
			// Record that the mark assist is done.
			// 记录标记辅助已完成。
			// Set this *after* we trace the end to make sure
			// that we emit an in-progress event if this is
			// the first event for the goroutine in the trace
			// or trace generation. Also, do this between
			// acquire/release because this is part of the
			// goroutine's trace state, and it must be atomic
			// with respect to the tracer.
			// 在我们跟踪结束之后设置此项，以确保如果这是 goroutine 在跟踪或跟踪生成中的第一个事件，我们会发出一个正在进行的事件。
			// 此外，在 acquire/release 之间执行此操作，因为这是 goroutine 的跟踪状态的一部分，并且对于跟踪器必须是原子的。
			gp.inMarkAssist = false
			// Clear the inMarkAssist flag.
			// 清除 inMarkAssist 标志。
			traceRelease(trace)
			// Release the trace context.
			// 释放跟踪上下文。
		} else {
			// This state is tracked even if tracing isn't enabled.
			// It's only used by the new tracer.
			// See the comment on enteredMarkAssistForTracing.
			// 即使未启用跟踪，也会跟踪此状态。它仅由新的跟踪器使用。
			// 请参阅 enteredMarkAssistForTracing 上的注释。
			gp.inMarkAssist = false
			// Clear the inMarkAssist flag.
			// 清除 inMarkAssist 标志。
		}
	}
}

// gcAssistAlloc1 is the part of gcAssistAlloc that runs on the system
// stack. This is a separate function to make it easier to see that
// we're not capturing anything from the user stack, since the user
// stack may move while we're in this function.
//
// gcAssistAlloc1 indicates whether this assist completed the mark
// phase by setting gp.param to non-nil. This can't be communicated on
// the stack since it may move.
//
// gcAssistAlloc1 是 gcAssistAlloc 的一部分，它在系统堆栈上运行。
// 这是一个单独的函数，可以更容易地看到我们没有从用户堆栈中捕获任何内容，
// 因为用户堆栈可能在此函数中移动。
//
// gcAssistAlloc1 通过将 gp.param 设置为非 nil 来指示此辅助是否完成了标记阶段。
// 这无法在堆栈上传达，因为它可能会移动。
//
//go:systemstack
//go:systemstack
//go:systemstack 指示此函数必须在系统堆栈上运行。
func gcAssistAlloc1(gp *g, scanWork int64) {
	// Clear the flag indicating that this assist completed the
	// mark phase.
	// 清除标志，表明此辅助已完成标记阶段。
	gp.param = nil

	if atomic.Load(&gcBlackenEnabled) == 0 {
		// The gcBlackenEnabled check in malloc races with the
		// store that clears it but an atomic check in every malloc
		// would be a performance hit.
		// Instead we recheck it here on the non-preemptible system
		// stack to determine if we should perform an assist.
		// malloc 中的 gcBlackenEnabled 检查与清除它的存储发生竞争，但是在每个 malloc 中进行原子检查会降低性能。
		// 相反，我们在此处在不可抢占的系统堆栈上重新检查它，以确定是否应执行辅助。

		// GC is done, so ignore any remaining debt.
		// GC 已完成，因此忽略任何剩余的债务。
		gp.gcAssistBytes = 0
		return
	}
	// Track time spent in this assist. Since we're on the
	// system stack, this is non-preemptible, so we can
	// just measure start and end time.
	//
	// Limiter event tracking might be disabled if we end up here
	// while on a mark worker.
	// 跟踪在此辅助中花费的时间。由于我们在系统堆栈上，因此它是不可抢占的，因此我们可以只测量开始和结束时间。
	//
	// 如果我们最终在标记工作器上，则可能会禁用限制器事件跟踪。
	startTime := nanotime()
	trackLimiterEvent := gp.m.p.ptr().limiterEvent.start(limiterEventMarkAssist, startTime)

	// Decrement the number of waiting workers.
	// 减少等待工作程序的数量。
	decnwait := atomic.Xadd(&work.nwait, -1)
	// If this was the last worker, then something is wrong.
	// 如果这是最后一个工作程序，则说明有问题。
	if decnwait == work.nproc {
		println("runtime: work.nwait =", decnwait, "work.nproc=", work.nproc)
		throw("nwait > work.nprocs")
	}

	// gcDrainN requires the caller to be preemptible.
	casGToWaitingForGC(gp, _Grunning, waitReasonGCAssistMarking) // 将 G 状态更改为 _Gwaiting，以便可以进行抢占。
	// 将 G 状态更改为 _Gwaiting，以便可以进行抢占。

	// drain own cached work first in the hopes that it
	// will be more cache friendly.
	gcw := &getg().m.p.ptr().gcw        // 获取当前 P 的 gcw 缓存
	workDone := gcDrainN(gcw, scanWork) // 从 gcw 中消耗一些扫描工作

	casgstatus(gp, _Gwaiting, _Grunning) // 将 G 状态改回 _Grunning
	// 将 G 状态改回 _Grunning

	// Record that we did this much scan work.
	// 记录我们完成了多少扫描工作。
	//
	// Back out the number of bytes of assist credit that
	// this scan work counts for. The "1+" is a poor man's
	// round-up, to ensure this adds credit even if
	// assistBytesPerWork is very low.
	assistBytesPerWork := gcController.assistBytesPerWork.Load()        // 获取每个工作单元的辅助字节数
	gp.gcAssistBytes += 1 + int64(assistBytesPerWork*float64(workDone)) // 增加 G 的 gcAssistBytes 计数器
	// 增加 G 的 gcAssistBytes 计数器

	// If this is the last worker and we ran out of work,
	// signal a completion point.
	// 如果这是最后一个工作程序并且我们用完了工作，则发出完成信号。
	incnwait := atomic.Xadd(&work.nwait, +1) // 增加等待工作程序的数量
	// 增加等待工作程序的数量
	if incnwait > work.nproc { // 如果等待的工作程序数量大于工作程序总数，则抛出错误
		println("runtime: work.nwait=", incnwait,
			"work.nproc=", work.nproc)
		throw("work.nwait > work.nproc")
	}

	if incnwait == work.nproc && !gcMarkWorkAvailable(nil) {
		// This has reached a background completion point. Set
		// gp.param to a non-nil value to indicate this. It
		// doesn't matter what we set it to (it just has to be
		// a valid pointer).
		gp.param = unsafe.Pointer(gp) // 设置 gp.param 为非 nil 值，表示已达到后台完成点
	}
	now := nanotime()           // 获取当前时间
	duration := now - startTime // 计算辅助标记持续时间
	pp := gp.m.p.ptr()          // 获取当前 P
	pp.gcAssistTime += duration // 累加 P 的 gcAssistTime
	if trackLimiterEvent {
		pp.limiterEvent.stop(limiterEventMarkAssist, now) // 停止限制器事件跟踪
	}
	if pp.gcAssistTime > gcAssistTimeSlack {
		gcController.assistTime.Add(pp.gcAssistTime) // 将 P 的 gcAssistTime 添加到全局 gcController.assistTime
		gcCPULimiter.update(now)                     // 更新 CPU 限制器
		pp.gcAssistTime = 0                          // 重置 P 的 gcAssistTime
	}
}

// gcWakeAllAssists wakes all currently blocked assists. This is used
// at the end of a GC cycle. gcBlackenEnabled must be false to prevent
// new assists from going to sleep after this point.
// gcWakeAllAssists 唤醒所有当前被阻塞的 assists。这在 GC 周期结束时使用。gcBlackenEnabled 必须为 false，以防止新的 assists 在此之后进入睡眠状态。
func gcWakeAllAssists() {
	lock(&work.assistQueue.lock) // 获取 assistQueue 的锁
	// 获取 assistQueue 的锁
	list := work.assistQueue.q.popList() // 从 assistQueue 中弹出一个 G 链表
	// 从 assistQueue 中弹出一个 G 链表
	injectglist(&list) // 将 G 链表注入到全局运行队列中，使其可以被调度执行
	// 将 G 链表注入到全局运行队列中，使其可以被调度执行
	unlock(&work.assistQueue.lock) // 释放 assistQueue 的锁
	// 释放 assistQueue 的锁
}

// gcParkAssist puts the current goroutine on the assist queue and parks.
//
// gcParkAssist reports whether the assist is now satisfied. If it
// returns false, the caller must retry the assist.
// gcParkAssist 将当前 goroutine 放入 assist 队列并 park。
// gcParkAssist 报告 assist 是否已满足。如果返回 false，调用者必须重试 assist。
func gcParkAssist() bool {
	lock(&work.assistQueue.lock) // 获取 assistQueue 的锁
	// 获取 assistQueue 的锁
	// If the GC cycle finished while we were getting the lock,
	// exit the assist. The cycle can't finish while we hold the
	// lock.
	// 如果在获取锁时 GC 周期完成，则退出 assist。在持有锁时，周期无法完成。
	if atomic.Load(&gcBlackenEnabled) == 0 { // 检查 gcBlackenEnabled 是否为 0，表示 GC 周期已完成
		unlock(&work.assistQueue.lock) // 释放 assistQueue 的锁
		return true                    // 返回 true，表示 assist 已满足
	}

	gp := getg()                    // 获取当前 goroutine 的 G 结构体
	oldList := work.assistQueue.q   // 保存当前的 assist 队列
	work.assistQueue.q.pushBack(gp) // 将当前 goroutine 的 G 结构体添加到 assist 队列的末尾

	// Recheck for background credit now that this G is in
	// the queue, but can still back out. This avoids a
	// race in case background marking has flushed more
	// credit since we checked above.
	// 现在重新检查后台 credit，因为此 G 在队列中，但仍然可以退出。这避免了在后台标记刷新更多 credit 时出现竞争。
	if gcController.bgScanCredit.Load() > 0 { // 检查是否有足够的后台扫描 credit
		work.assistQueue.q = oldList // 恢复到旧的 assist 队列，相当于把当前G从队列中移除
		if oldList.tail != 0 {
			oldList.tail.ptr().schedlink.set(nil)
		}
		unlock(&work.assistQueue.lock) // 释放 assistQueue 的锁
		return false                   // 返回 false，表示 assist 未满足，需要重试
	}
	// Park.
	goparkunlock(&work.assistQueue.lock, waitReasonGCAssistWait, traceBlockGCMarkAssist, 2) // 将当前 goroutine park，等待被唤醒
	return true                                                                             // 返回 true，表示 assist 已满足（实际上是被唤醒后才返回）
}

// gcFlushBgCredit flushes scanWork units of background scan work
// credit. This first satisfies blocked assists on the
// work.assistQueue and then flushes any remaining credit to
// gcController.bgScanCredit.
//
// Write barriers are disallowed because this is used by gcDrain after
// it has ensured that all work is drained and this must preserve that
// condition.
// gcFlushBgCredit 刷新后台扫描工作 credit 的 scanWork 单位。
// 首先满足 work.assistQueue 上阻塞的 assists，然后将任何剩余的 credit 刷新到 gcController.bgScanCredit。
//
// 禁止写屏障，因为它被 gcDrain 使用，在 gcDrain 确保所有工作都已耗尽后，这必须保持该条件。
//
// 该函数的作用是将后台扫描的 credit 刷新到全局的 credit 池中，
// 并且在刷新之前，会先尝试唤醒 assistQueue 中等待的 goroutine。
// 这样做的目的是为了平衡前台 assist 和后台扫描的工作量。
// 如果 assistQueue 中有等待的 goroutine，那么就优先唤醒它们，
// 否则就将 credit 刷新到全局的 credit 池中。
// 这样可以避免前台 assist 一直等待，而后台扫描却一直在进行的情况。
//
//go:nowritebarrierrec
//go:nowritebarrierrec 表示该函数在写屏障下是安全的。
func gcFlushBgCredit(scanWork int64) {
	if work.assistQueue.q.empty() {
		// Fast path; there are no blocked assists. There's a
		// small window here where an assist may add itself to
		// the blocked queue and park. If that happens, we'll
		// just get it on the next flush.
		// 快速路径；没有阻塞的 assists。这里有一个很小的窗口，一个 assist 可能会将自己添加到阻塞队列并 park。
		// 如果发生这种情况，我们将在下一次刷新时获取它。
		gcController.bgScanCredit.Add(scanWork) // 将 scanWork 添加到后台扫描 credit
		return
	}

	assistBytesPerWork := gcController.assistBytesPerWork.Load() // 获取每个 work 的 assist 字节数
	scanBytes := int64(float64(scanWork) * assistBytesPerWork)   // 计算扫描的字节数

	lock(&work.assistQueue.lock) // 获取 assistQueue 的锁
	for !work.assistQueue.q.empty() && scanBytes > 0 {
		gp := work.assistQueue.q.pop() // 从 assistQueue 中弹出一个 G
		// Note that gp.gcAssistBytes is negative because gp
		// is in debt. Think carefully about the signs below.
		// 注意 gp.gcAssistBytes 是负数，因为 gp 欠债。仔细考虑下面的符号。
		if scanBytes+gp.gcAssistBytes >= 0 {
			// Satisfy this entire assist debt.
			// 满足整个 assist 债务。
			scanBytes += gp.gcAssistBytes // 偿还 gp 的债务
			gp.gcAssistBytes = 0          // gp 不再欠债
			// It's important that we *not* put gp in
			// runnext. Otherwise, it's possible for user
			// code to exploit the GC worker's high
			// scheduler priority to get itself always run
			// before other goroutines and always in the
			// fresh quantum started by GC.
			// 重要的是我们*不*将 gp 放入 runnext。否则，用户代码可能会利用 GC worker 的高调度器优先级，
			// 使其始终在其他 goroutine 之前运行，并且始终在 GC 启动的新量子中运行。
			ready(gp, 0, false) // 唤醒 gp
		} else {
			// Partially satisfy this assist.
			// 部分满足此 assist。
			gp.gcAssistBytes += scanBytes // 部分偿还 gp 的债务
			scanBytes = 0                 // 没有剩余的 scanBytes
			// As a heuristic, we move this assist to the
			// back of the queue so that large assists
			// can't clog up the assist queue and
			// substantially delay small assists.
			// 作为一种启发式方法，我们将此 assist 移到队列的后面，这样大的 assists 就不会阻塞 assist 队列并大大延迟小的 assists。
			work.assistQueue.q.pushBack(gp) // 将 gp 移到队列的末尾
			break
		}
	}

	if scanBytes > 0 {
		// Convert from scan bytes back to work.
		// 从扫描字节转换回 work。
		assistWorkPerByte := gcController.assistWorkPerByte.Load() // 获取每个字节的 assist work
		scanWork = int64(float64(scanBytes) * assistWorkPerByte)   // 计算剩余的 scanWork
		gcController.bgScanCredit.Add(scanWork)                    // 将剩余的 scanWork 添加到后台扫描 credit
	}
	unlock(&work.assistQueue.lock) // 释放 assistQueue 的锁
}

// scanstack scans gp's stack, greying all pointers found on the stack.
//
// Returns the amount of scan work performed, but doesn't update
// gcController.stackScanWork or flush any credit. Any background credit produced
// by this function should be flushed by its caller. scanstack itself can't
// safely flush because it may result in trying to wake up a goroutine that
// was just scanned, resulting in a self-deadlock.
//
// scanstack will also shrink the stack if it is safe to do so. If it
// is not, it schedules a stack shrink for the next synchronous safe
// point.
//
// scanstack is marked go:systemstack because it must not be preempted
// while using a workbuf.
//
// scanstack 扫描 gp 的堆栈，找到堆栈上的所有指针并将它们标记为灰色。
//
// 返回执行的扫描工作量，但不更新 gcController.stackScanWork 或刷新任何 credit。
// 此函数产生的任何后台 credit 都应由其调用者刷新。
// scanstack 本身无法安全地刷新，因为它可能导致尝试唤醒刚刚扫描的 goroutine，从而导致自死锁。
//
// 如果安全，scanstack 还会缩小堆栈。如果不是，它会安排在下一个同步安全点缩小堆栈。
//
// scanstack 被标记为 go:systemstack，因为它在使用 workbuf 时不得被抢占。
//
//go:nowritebarrier
//go:systemstack
func scanstack(gp *g, gcw *gcWork) int64 {
	// Check that the goroutine is in a scanning state.
	// 检查 goroutine 是否处于扫描状态。
	if readgstatus(gp)&_Gscan == 0 {
		print("runtime:scanstack: gp=", gp, ", goid=", gp.goid, ", gp->atomicstatus=", hex(readgstatus(gp)), "\n")
		throw("scanstack - bad status")
	}

	// Check the goroutine's status and handle it accordingly.
	// 检查 goroutine 的状态并相应地处理它。
	switch readgstatus(gp) &^ _Gscan {
	default:
		// Unexpected status.
		// 意外的状态。
		print("runtime: gp=", gp, ", goid=", gp.goid, ", gp->atomicstatus=", readgstatus(gp), "\n")
		throw("mark - bad status")
	case _Gdead:
		// Goroutine is dead, nothing to scan.
		// Goroutine 已经死亡，无需扫描。
		return 0
	case _Grunning:
		// Goroutine is running, which is unexpected during scanning.
		// Goroutine 正在运行，这在扫描期间是意外的。
		print("runtime: gp=", gp, ", goid=", gp.goid, ", gp->atomicstatus=", readgstatus(gp), "\n")
		throw("scanstack: goroutine not stopped")
	case _Grunnable, _Gsyscall, _Gwaiting:
		// Goroutine is in a valid state for scanning.
		// Goroutine 处于扫描的有效状态。
		// ok
	}

	if gp == getg() {
		throw("can't scan our own stack")
	}

	// scannedSize is the amount of work we'll be reporting.
	//
	// It is less than the allocated size (which is hi-lo).
	// scannedSize 是我们将要报告的工作量。
	// 它小于分配的大小 (即 hi-lo)。
	var sp uintptr
	if gp.syscallsp != 0 {
		sp = gp.syscallsp // If in a system call this is the stack pointer (gp.sched.sp can be 0 in this case on Windows).
		// 如果在系统调用中，这是堆栈指针 (在这种情况下，Windows 上的 gp.sched.sp 可以为 0)。
	} else {
		sp = gp.sched.sp
	}
	scannedSize := gp.stack.hi - sp // 计算扫描的大小

	// Keep statistics for initial stack size calculation.
	// Note that this accumulates the scanned size, not the allocated size.
	// 保留初始堆栈大小计算的统计信息。
	// 请注意，这会累积扫描的大小，而不是分配的大小。
	p := getg().m.p.ptr()
	p.scannedStackSize += uint64(scannedSize) // 累加扫描的堆栈大小
	p.scannedStacks++                         // 增加扫描的堆栈数量

	if isShrinkStackSafe(gp) {
		// Shrink the stack if not much of it is being used.
		// 如果没有使用太多堆栈，则缩小堆栈。
		shrinkstack(gp)
	} else {
		// Otherwise, shrink the stack at the next sync safe point.
		// 否则，在下一个同步安全点缩小堆栈。
		gp.preemptShrink = true
	}

	// var state stackScanState
	// state.stack = gp.stack
	var state stackScanState // 定义一个 stackScanState 类型的变量 state，用于保存扫描堆栈的状态
	state.stack = gp.stack   // 将当前 Goroutine 的堆栈信息赋值给 state.stack

	// if stackTraceDebug {
	// 	println("stack trace goroutine", gp.goid)
	// }
	if stackTraceDebug { // 如果启用了堆栈跟踪调试
		println("stack trace goroutine", gp.goid) // 打印当前 Goroutine 的 ID
	}

	// if debugScanConservative && gp.asyncSafePoint {
	// 	print("scanning async preempted goroutine ", gp.goid, " stack [", hex(gp.stack.lo), ",", hex(gp.stack.hi), ")\n")
	// }
	if debugScanConservative && gp.asyncSafePoint { // 如果启用了保守扫描调试并且当前 Goroutine 处于异步安全点
		print("scanning async preempted goroutine ", gp.goid, " stack [", hex(gp.stack.lo), ",", hex(gp.stack.hi), ")\n") // 打印正在扫描的异步抢占 Goroutine 的 ID 和堆栈范围
	}

	// Scan the saved context register. This is effectively a live
	// register that gets moved back and forth between the
	// register and sched.ctxt without a write barrier.
	// if gp.sched.ctxt != nil {
	// 	scanblock(uintptr(unsafe.Pointer(&gp.sched.ctxt)), goarch.PtrSize, &oneptrmask[0], gcw, &state)
	// }
	// 扫描保存的上下文寄存器。这实际上是一个活动寄存器，在寄存器和 sched.ctxt 之间来回移动，没有写屏障。
	if gp.sched.ctxt != nil { // 如果 Goroutine 的调度上下文中存在数据
		scanblock(uintptr(unsafe.Pointer(&gp.sched.ctxt)), goarch.PtrSize, &oneptrmask[0], gcw, &state) // 扫描该上下文中的指针
	}

	// Scan the stack. Accumulate a list of stack objects.
	// var u unwinder
	// for u.init(gp, 0); u.valid(); u.next() {
	// 	scanframeworker(&u.frame, &state, gcw)
	// }
	// 扫描堆栈。累积堆栈对象的列表。
	var u unwinder                           // 定义一个 unwinder 类型的变量 u，用于展开堆栈
	for u.init(gp, 0); u.valid(); u.next() { // 初始化 unwinder，并在堆栈中迭代
		scanframeworker(&u.frame, &state, gcw) // 扫描当前帧中的指针
	}

	// Find additional pointers that point into the stack from the heap.
	// Currently this includes defers and panics. See also function copystack.
	// 查找从堆指向堆栈的其他指针。
	// 目前包括 defers 和 panics。另请参见函数 copystack。

	// Find and trace other pointers in defer records.
	// 查找并跟踪 defer 记录中的其他指针。
	for d := gp._defer; d != nil; d = d.link { // 遍历 Goroutine 的 _defer 链表
		if d.fn != nil {
			// Scan the func value, which could be a stack allocated closure.
			// See issue 30453.
			// 扫描 func 值，它可能是一个堆栈分配的闭包。
			// 参见 issue 30453。
			scanblock(uintptr(unsafe.Pointer(&d.fn)), goarch.PtrSize, &oneptrmask[0], gcw, &state) // 扫描 defer 结构体中的 fn 字段，该字段可能指向一个闭包
		}
		if d.link != nil {
			// The link field of a stack-allocated defer record might point
			// to a heap-allocated defer record. Keep that heap record live.
			// 堆栈分配的 defer 记录的 link 字段可能指向堆分配的 defer 记录。保持该堆记录处于活动状态。
			scanblock(uintptr(unsafe.Pointer(&d.link)), goarch.PtrSize, &oneptrmask[0], gcw, &state) // 扫描 defer 结构体中的 link 字段，该字段指向下一个 defer 结构体
		}
		// Retain defers records themselves.
		// Defer records might not be reachable from the G through regular heap
		// tracing because the defer linked list might weave between the stack and the heap.
		// 保留 defers 记录本身。
		// Defer 记录可能无法通过常规堆跟踪从 G 访问，因为 defer 链表可能在堆栈和堆之间交织。
		if d.heap { // 如果 defer 结构体分配在堆上
			scanblock(uintptr(unsafe.Pointer(&d)), goarch.PtrSize, &oneptrmask[0], gcw, &state) // 扫描 defer 结构体本身
		}
	}
	if gp._panic != nil {
		// Panics are always stack allocated.
		// Panics 总是堆栈分配的。
		state.putPtr(uintptr(unsafe.Pointer(gp._panic)), false) // 将 panic 结构体的指针添加到待扫描的指针队列中
	}

	// Find and scan all reachable stack objects.
	//
	// The state's pointer queue prioritizes precise pointers over
	// conservative pointers so that we'll prefer scanning stack
	// objects precisely.
	state.buildIndex() // 构建栈对象索引，用于快速查找指针指向的栈对象
	// 查找并扫描所有可达的栈对象。
	//
	// state 的指针队列优先处理精确指针，而不是保守指针，
	// 因此我们更倾向于精确地扫描栈对象。
	// 构建索引，方便后续查找栈对象
	for {
		p, conservative := state.getPtr() // 获取下一个待处理的指针，以及是否是保守指针
		if p == 0 {                       // 如果没有更多指针了
			break // 退出循环
		}
		obj := state.findObject(p) // 查找指针 p 指向的栈对象
		if obj == nil {            // 如果找不到对应的栈对象
			continue // 继续下一个指针
		}
		r := obj.r // 获取栈对象的类型信息
		if r == nil {
			// We've already scanned this object.
			// 我们已经扫描过这个对象了。
			continue // 继续下一个指针
		}
		obj.setRecord(nil) // Don't scan it again.
		// 设置对象记录为 nil，防止重复扫描
		if stackTraceDebug {
			printlock()
			print("  live stkobj at", hex(state.stack.lo+uintptr(obj.off)), "of size", obj.size)
			if conservative {
				print(" (conservative)")
			}
			println()
			printunlock()
		}
		gcdata := r.gcdata() // 获取 GC 元数据
		var s *mspan
		if r.useGCProg() {
			// This path is pretty unlikely, an object large enough
			// to have a GC program allocated on the stack.
			// We need some space to unpack the program into a straight
			// bitmask, which we allocate/free here.
			// TODO: it would be nice if there were a way to run a GC
			// program without having to store all its bits. We'd have
			// to change from a Lempel-Ziv style program to something else.
			// Or we can forbid putting objects on stacks if they require
			// a gc program (see issue 27447).
			// 如果类型信息 r 使用了 GC 程序
			// 这种情况不太可能发生，只有足够大的对象才会在栈上分配 GC 程序。
			// 我们需要一些空间来将程序解包成一个简单的位掩码，这里我们分配和释放这些空间。
			// TODO: 如果有一种方法可以在不存储所有位的情况下运行 GC 程序，那就太好了。
			// 我们必须从 Lempel-Ziv 风格的程序改为其他风格。
			// 或者我们可以禁止将需要 GC 程序的对象放在栈上 (参见 issue 27447)。
			s = materializeGCProg(r.ptrdata(), gcdata)    // 物化 GC 程序，将其转换为位掩码
			gcdata = (*byte)(unsafe.Pointer(s.startAddr)) // 将 gcdata 指针指向位掩码的起始地址
		}

		b := state.stack.lo + uintptr(obj.off) // 计算对象的起始地址
		if conservative {
			scanConservative(b, r.ptrdata(), gcdata, gcw, &state) // 保守地扫描对象
		} else {
			scanblock(b, r.ptrdata(), gcdata, gcw, &state) // 精确地扫描对象
		}

		if s != nil {
			dematerializeGCProg(s) // 释放 GC 程序占用的空间
		}
	}

	// Deallocate object buffers.
	// (Pointer buffers were all deallocated in the loop above.)
	// 释放对象缓冲区。
	// （指针缓冲区已在上面的循环中全部释放。）
	for state.head != nil {
		x := state.head     // 获取当前缓冲区
		state.head = x.next // 移动到下一个缓冲区
		if stackTraceDebug {
			for i := 0; i < x.nobj; i++ {
				obj := &x.obj[i]  // 获取缓冲区中的对象
				if obj.r == nil { // reachable
					continue // 如果对象可达，则跳过
				}
				println("  dead stkobj at", hex(gp.stack.lo+uintptr(obj.off)), "of size", obj.r.size)
				// Note: not necessarily really dead - only reachable-from-ptr dead.
				// 注意：不一定真的死了 - 只是从指针可达的角度来看是死的。
			}
		}
		x.nobj = 0                              // 重置缓冲区中的对象数量
		putempty((*workbuf)(unsafe.Pointer(x))) // 将缓冲区放回空闲列表
	}
	if state.buf != nil || state.cbuf != nil || state.freeBuf != nil {
		throw("remaining pointer buffers") // 如果还有剩余的指针缓冲区，则抛出错误
	}
	return int64(scannedSize) // 返回扫描的大小
}

// Scan a stack frame: local variables and function arguments/results.
// 扫描堆栈帧：局部变量和函数参数/结果。
//
//go:nowritebarrier
func scanframeworker(frame *stkframe, state *stackScanState, gcw *gcWork) {
	if _DebugGC > 1 && frame.continpc != 0 {
		print("scanframe ", funcname(frame.fn), "\n")
	}

	// isAsyncPreempt reports whether frame is the async preempt function.
	// isAsyncPreempt 报告帧是否是异步抢占函数。
	isAsyncPreempt := frame.fn.valid() && frame.fn.funcID == abi.FuncID_asyncPreempt
	// isDebugCall reports whether frame is the debugCallV2 function.
	// isDebugCall 报告帧是否是 debugCallV2 函数。
	isDebugCall := frame.fn.valid() && frame.fn.funcID == abi.FuncID_debugCallV2
	if state.conservative || isAsyncPreempt || isDebugCall {
		if debugScanConservative {
			println("conservatively scanning function", funcname(frame.fn), "at PC", hex(frame.continpc))
		}

		// Conservatively scan the frame. Unlike the precise
		// case, this includes the outgoing argument space
		// since we may have stopped while this function was
		// setting up a call.
		// 保守地扫描帧。与精确扫描不同，这包括传出的参数空间，
		// 因为我们可能在函数设置调用时停止了。
		//
		// TODO: We could narrow this down if the compiler
		// produced a single map per function of stack slots
		// and registers that ever contain a pointer.
		// TODO: 如果编译器为每个函数生成一个堆栈槽和寄存器的映射，
		// 我们可以缩小这个范围，这些堆栈槽和寄存器总是包含一个指针。
		if frame.varp != 0 {
			size := frame.varp - frame.sp
			if size > 0 {
				scanConservative(frame.sp, size, nil, gcw, state)
			}
		}

		// Scan arguments to this frame.
		// 扫描此帧的参数。
		if n := frame.argBytes(); n != 0 {
			// TODO: We could pass the entry argument map
			// to narrow this down further.
			// TODO: 我们可以传递入口参数映射来进一步缩小这个范围。
			scanConservative(frame.argp, n, nil, gcw, state)
		}

		if isAsyncPreempt || isDebugCall {
			// This function's frame contained the
			// registers for the asynchronously stopped
			// parent frame. Scan the parent
			// conservatively.
			// 此函数的帧包含异步停止的父帧的寄存器。
			// 保守地扫描父帧。
			state.conservative = true
		} else {
			// We only wanted to scan those two frames
			// conservatively. Clear the flag for future
			// frames.
			// 我们只想保守地扫描这两个帧。
			// 清除未来帧的标志。
			state.conservative = false
		}
		return
	}

	locals, args, objs := frame.getStackMap(false)
	// 获取堆栈映射，包括局部变量、参数和栈对象。
	// 如果栈帧已分配，则扫描局部变量。
	// 扫描参数。
	// 将所有栈对象添加到栈对象列表中。

	// Scan local variables if stack frame has been allocated.
	// 如果栈帧已分配，则扫描局部变量。
	if locals.n > 0 {
		size := uintptr(locals.n) * goarch.PtrSize
		scanblock(frame.varp-size, size, locals.bytedata, gcw, state)
	}

	// Scan arguments.
	// 扫描参数。
	if args.n > 0 {
		scanblock(frame.argp, uintptr(args.n)*goarch.PtrSize, args.bytedata, gcw, state)
	}

	// Add all stack objects to the stack object list.
	// 将所有栈对象添加到栈对象列表中。
	if frame.varp != 0 {
		// varp is 0 for defers, where there are no locals.
		// In that case, there can't be a pointer to its args, either.
		// (And all args would be scanned above anyway.)
		// varp 对于没有局部变量的延迟调用 (defer) 为 0。
		// 在这种情况下，也不可能存在指向其参数的指针。
		// （并且所有参数无论如何都会在上面扫描。）
		for i := range objs {
			obj := &objs[i]
			off := obj.off
			base := frame.varp // locals base pointer
			// base 是局部变量的基址指针
			if off >= 0 {
				base = frame.argp // arguments and return values base pointer
				// base 是参数和返回值的基址指针
			}
			ptr := base + uintptr(off)
			if ptr < frame.sp {
				// object hasn't been allocated in the frame yet.
				// 对象尚未在帧中分配。
				continue
			}
			if stackTraceDebug {
				println("stkobj at", hex(ptr), "of size", obj.size)
			}
			state.addObject(ptr, obj)
		}
	}
}

type gcDrainFlags int

const (
	// gcDrain returns when g.preempt is set.
	// gcDrain在g.preempt被设置时返回。
	gcDrainUntilPreempt gcDrainFlags = 1 << iota
	// gcDrain flushes scan work credit to gcController.bgScanCredit every gcCreditSlack units of scan work.
	// gcDrain每隔gcCreditSlack个扫描工作单元，将扫描工作信用刷新到gcController.bgScanCredit。
	gcDrainFlushBgCredit
	// gcDrain returns when there is other work to do.
	// 当有其他工作要做时，gcDrain返回。
	gcDrainIdle
	// gcDrain self-preempts when pollFractionalWorkerExit() returns true. This implies gcDrainNoBlock.
	// 当pollFractionalWorkerExit()返回true时，gcDrain会自我抢占。这意味着gcDrainNoBlock。
	gcDrainFractional
)

// gcDrainMarkWorkerIdle is a wrapper for gcDrain that exists to better account
// mark time in profiles.
// gcDrainMarkWorkerIdle 是 gcDrain 的一个包装器，它的存在是为了更好地在 profiles 中记录标记时间。
func gcDrainMarkWorkerIdle(gcw *gcWork) {
	// 调用 gcDrain 函数，并传入相应的标志位。
	// gcDrainIdle: 当有其他工作要做时，gcDrain 返回。
	// gcDrainUntilPreempt: gcDrain 在 g.preempt 被设置时返回。
	// gcDrainFlushBgCredit: gcDrain 每隔 gcCreditSlack 个扫描工作单元，将扫描工作信用刷新到 gcController.bgScanCredit。
	gcDrain(gcw, gcDrainIdle|gcDrainUntilPreempt|gcDrainFlushBgCredit)
}

// gcDrainMarkWorkerDedicated is a wrapper for gcDrain that exists to better account
// mark time in profiles.
// gcDrainMarkWorkerDedicated 是 gcDrain 的一个包装器，它的存在是为了更好地在 profiles 中记录标记时间。
func gcDrainMarkWorkerDedicated(gcw *gcWork, untilPreempt bool) {
	// 初始化标志位，默认包含 gcDrainFlushBgCredit，表示每隔一定扫描工作单元刷新背景信用。
	flags := gcDrainFlushBgCredit
	// 如果 untilPreempt 为 true，则添加 gcDrainUntilPreempt 标志，表示直到可以被抢占时才返回。
	if untilPreempt {
		flags |= gcDrainUntilPreempt
	}
	// 调用 gcDrain 函数，传入工作区和标志位。
	gcDrain(gcw, flags)
}

// gcDrainMarkWorkerFractional is a wrapper for gcDrain that exists to better account
// mark time in profiles.
// gcDrainMarkWorkerFractional 是 gcDrain 的一个包装器，它的存在是为了更好地在 profiles 中记录标记时间。
func gcDrainMarkWorkerFractional(gcw *gcWork) {
	// 调用 gcDrain 函数，传入工作区和标志位。
	// gcDrainFractional: 当 pollFractionalWorkerExit() 返回 true 时，gcDrain 会自我抢占。
	// gcDrainUntilPreempt: gcDrain 在 g.preempt 被设置时返回。
	// gcDrainFlushBgCredit: gcDrain 每隔 gcCreditSlack 个扫描工作单元，将扫描工作信用刷新到 gcController.bgScanCredit。
	gcDrain(gcw, gcDrainFractional|gcDrainUntilPreempt|gcDrainFlushBgCredit)
}

// gcDrain scans roots and objects in work buffers, blackening grey
// objects until it is unable to get more work. It may return before
// GC is done; it's the caller's responsibility to balance work from
// other Ps.
//
// If flags&gcDrainUntilPreempt != 0, gcDrain returns when g.preempt
// is set.
//
// If flags&gcDrainIdle != 0, gcDrain returns when there is other work
// to do.
//
// If flags&gcDrainFractional != 0, gcDrain self-preempts when
// pollFractionalWorkerExit() returns true. This implies
// gcDrainNoBlock.
//
// If flags&gcDrainFlushBgCredit != 0, gcDrain flushes scan work
// credit to gcController.bgScanCredit every gcCreditSlack units of
// scan work.
//
// gcDrain will always return if there is a pending STW or forEachP.
//
// Disabling write barriers is necessary to ensure that after we've
// confirmed that we've drained gcw, that we don't accidentally end
// up flipping that condition by immediately adding work in the form
// of a write barrier buffer flush.
//
// Don't set nowritebarrierrec because it's safe for some callees to
// have write barriers enabled.
//
// gcDrain 扫描工作缓冲区中的根对象和对象，将灰色对象着色为黑色，直到无法获得更多工作。
// 它可能会在 GC 完成之前返回；调用者有责任平衡来自其他 P 的工作。
//
// 如果 flags&gcDrainUntilPreempt != 0，则当 g.preempt 被设置时，gcDrain 返回。
//
// 如果 flags&gcDrainIdle != 0，则当有其他工作要做时，gcDrain 返回。
//
// 如果 flags&gcDrainFractional != 0，则当 pollFractionalWorkerExit() 返回 true 时，gcDrain 会自我抢占。
// 这意味着 gcDrainNoBlock。
//
// 如果 flags&gcDrainFlushBgCredit != 0，则 gcDrain 每隔 gcCreditSlack 个扫描工作单元，将扫描工作信用刷新到 gcController.bgScanCredit。
//
// 如果有待处理的 STW 或 forEachP，gcDrain 将始终返回。
//
// 禁用写屏障是必要的，以确保在我们确认已经耗尽 gcw 之后，我们不会因为立即添加写屏障缓冲区刷新形式的工作而意外地翻转该条件。
//
// 不要设置 nowritebarrierrec，因为对于某些被调用者来说，启用写屏障是安全的。
//
//go:nowritebarrier
func gcDrain(gcw *gcWork, flags gcDrainFlags) {
	if !writeBarrier.enabled {
		throw("gcDrain phase incorrect")
	}
	// 如果写屏障未启用，则抛出错误，因为gcDrain阶段不正确。
	// If the write barrier is not enabled, throw an error because the gcDrain phase is incorrect.

	// N.B. We must be running in a non-preemptible context, so it's
	// safe to hold a reference to our P here.
	gp := getg().m.curg
	pp := gp.m.p.ptr()
	preemptible := flags&gcDrainUntilPreempt != 0
	flushBgCredit := flags&gcDrainFlushBgCredit != 0
	idle := flags&gcDrainIdle != 0
	// 注意：我们必须在不可抢占的上下文中运行，因此在这里持有对我们的 P 的引用是安全的。
	// 获取当前 G 和 P 的指针。
	// 根据传入的 flags 计算是否可抢占、是否刷新后台信用以及是否空闲。
	// N.B. We must be running in a non-preemptible context, so it's safe to hold a reference to our P here.
	// Get the current G and P pointers.
	// Calculate whether it is preemptible, whether to flush background credit, and whether it is idle according to the incoming flags.

	initScanWork := gcw.heapScanWork
	// 记录初始的堆扫描工作量。
	// Record the initial heap scan workload.

	// checkWork is the scan work before performing the next
	// self-preempt check.
	checkWork := int64(1<<63 - 1)
	var check func() bool
	if flags&(gcDrainIdle|gcDrainFractional) != 0 {
		checkWork = initScanWork + drainCheckThreshold
		if idle {
			check = pollWork
		} else if flags&gcDrainFractional != 0 {
			check = pollFractionalWorkerExit
		}
	}
	// checkWork 是在执行下一次自我抢占检查之前的扫描工作量。
	// 如果设置了 gcDrainIdle 或 gcDrainFractional 标志，则设置 checkWork 和 check 函数。
	// 如果是空闲状态，则使用 pollWork 函数进行检查。
	// 如果是部分状态，则使用 pollFractionalWorkerExit 函数进行检查。
	// checkWork is the scan work before performing the next self-preempt check.
	// If the gcDrainIdle or gcDrainFractional flag is set, set the checkWork and check functions.
	// If it is idle, use the pollWork function to check.
	// If it is a partial state, use the pollFractionalWorkerExit function to check.

	// Drain root marking jobs.
	// 处理根标记任务。
	if work.markrootNext < work.markrootJobs {
		// Stop if we're preemptible, if someone wants to STW, or if
		// someone is calling forEachP.
		// 如果可以被抢占、有人想要 STW（Stop-The-World）或者有人调用 forEachP，则停止。
		for !(gp.preempt && (preemptible || sched.gcwaiting.Load() || pp.runSafePointFn != 0)) {
			job := atomic.Xadd(&work.markrootNext, +1) - 1
			// 获取下一个根标记任务的索引，并原子性地增加计数器。
			// Get the index of the next root marking job and atomically increment the counter.
			if job >= work.markrootJobs {
				break
			}
			// 如果所有根标记任务都已完成，则跳出循环。
			// If all root marking jobs are completed, break out of the loop.
			markroot(gcw, job, flushBgCredit)
			// 执行根标记任务。
			// Execute the root marking job.
			if check != nil && check() {
				goto done
			}
			// 如果需要进行抢占检查并且检查函数返回 true，则跳转到 done 标签。
			// If a preemption check is needed and the check function returns true, jump to the done label.
		}
	}

	// Drain heap marking jobs.
	// 处理堆标记任务。
	//
	// Stop if we're preemptible, if someone wants to STW, or if
	// someone is calling forEachP.
	// 如果可以被抢占、有人想要 STW（Stop-The-World）或者有人调用 forEachP，则停止。
	//
	// TODO(mknyszek): Consider always checking gp.preempt instead
	// of having the preempt flag, and making an exception for certain
	// mark workers in retake. That might be simpler than trying to
	// enumerate all the reasons why we might want to preempt, even
	// if we're supposed to be mostly non-preemptible.
	// TODO(mknyszek): 考虑始终检查 gp.preempt 而不是使用 preempt 标志，并在 retake 中为某些标记 worker 做出例外。
	// 这可能比尝试枚举我们可能想要抢占的所有原因更简单，即使我们应该在很大程度上是不可抢占的。
	for !(gp.preempt && (preemptible || sched.gcwaiting.Load() || pp.runSafePointFn != 0)) {
		// Try to keep work available on the global queue. We used to
		// check if there were waiting workers, but it's better to
		// just keep work available than to make workers wait. In the
		// worst case, we'll do O(log(_WorkbufSize)) unnecessary
		// balances.
		// 尝试保持全局队列上的工作可用。我们过去常常检查是否有等待的 worker，但最好是保持工作可用，而不是让 worker 等待。
		// 在最坏的情况下，我们将执行 O(log(_WorkbufSize)) 次不必要的平衡操作。
		if work.full == 0 {
			gcw.balance()
		}

		b := gcw.tryGetFast()
		if b == 0 {
			b = gcw.tryGet()
			if b == 0 {
				// Flush the write barrier
				// buffer; this may create
				// more work.
				// 刷新写屏障缓冲区；这可能会创建更多工作。
				wbBufFlush()
				b = gcw.tryGet()
			}
		}
		if b == 0 {
			// Unable to get work.
			// 无法获取工作。
			break
		}
		scanobject(b, gcw)

		// Flush background scan work credit to the global
		// account if we've accumulated enough locally so
		// mutator assists can draw on it.
		// 将后台扫描工作信用刷新到全局帐户，以便 mutator 辅助可以使用它。
		// If we've accumulated enough heap scan work, flush it to the global account.
		// 如果我们积累了足够的堆扫描工作量，则将其刷新到全局帐户。
		if gcw.heapScanWork >= gcCreditSlack {
			// Add the local heap scan work to the global heap scan work.
			// 将本地堆扫描工作量添加到全局堆扫描工作量。
			gcController.heapScanWork.Add(gcw.heapScanWork)
			// If we should flush the background credit, do so.
			// 如果我们应该刷新后台信用，请执行此操作。
			if flushBgCredit {
				gcFlushBgCredit(gcw.heapScanWork - initScanWork)
				initScanWork = 0
			}
			// Update the check work.
			// 更新检查工作。
			checkWork -= gcw.heapScanWork
			// Reset the local heap scan work.
			// 重置本地堆扫描工作。
			gcw.heapScanWork = 0

			// Check if we need to perform a drain check.
			// 检查是否需要执行 drain 检查。
			if checkWork <= 0 {
				checkWork += drainCheckThreshold
				if check != nil && check() {
					break
				}
			}
		}
	}

done:
	// Flush remaining scan work credit.
	// 刷新剩余的扫描工作信用。
	if gcw.heapScanWork > 0 {
		// Add any remaining heap scan work to the global counter.
		// 将所有剩余的堆扫描工作添加到全局计数器。
		gcController.heapScanWork.Add(gcw.heapScanWork)
		// If background credit flushing is enabled, flush the remaining credit.
		// 如果启用了后台信用刷新，则刷新剩余的信用。
		if flushBgCredit {
			gcFlushBgCredit(gcw.heapScanWork - initScanWork)
		}
		// Reset the local heap scan work counter.
		// 重置本地堆扫描工作计数器。
		gcw.heapScanWork = 0
	}
}

// gcDrainN blackens grey objects until it has performed roughly
// scanWork units of scan work or the G is preempted. This is
// best-effort, so it may perform less work if it fails to get a work
// buffer. Otherwise, it will perform at least n units of work, but
// may perform more because scanning is always done in whole object
// increments. It returns the amount of scan work performed.
//
// The caller goroutine must be in a preemptible state (e.g.,
// _Gwaiting) to prevent deadlocks during stack scanning. As a
// consequence, this must be called on the system stack.
//
// gcDrainN 将灰色对象着色为黑色，直到它执行了大约 scanWork 单位的扫描工作，或者 G 被抢占。
// 这是尽力而为，因此如果它无法获取工作缓冲区，则可能执行较少的工作。
// 否则，它将至少执行 n 个单位的工作，但由于扫描总是以整个对象增量完成，因此可能会执行更多的工作。
// 它返回执行的扫描工作量。
//
// 调用者 goroutine 必须处于可抢占状态（例如，_Gwaiting），以防止在堆栈扫描期间发生死锁。
// 因此，必须在系统堆栈上调用此函数。
//
//go:nowritebarrier
//go:systemstack
func gcDrainN(gcw *gcWork, scanWork int64) int64 {
	if !writeBarrier.enabled {
		throw("gcDrainN phase incorrect")
	}
	// 如果写屏障未启用，则抛出错误，因为 gcDrainN 阶段不正确。
	// If the write barrier is not enabled, throw an error because the gcDrainN phase is incorrect.

	// There may already be scan work on the gcw, which we don't
	// want to claim was done by this call.
	workFlushed := -gcw.heapScanWork
	// 可能在 gcw 上已经有一些扫描工作，我们不想声明这些工作是由这次调用完成的。
	// There may already be some scan work on the gcw, which we don't want to claim was done by this call.
	// 因此，将 workFlushed 初始化为 -gcw.heapScanWork，以便稍后可以正确计算此调用完成的工作量。
	// Therefore, initialize workFlushed to -gcw.heapScanWork so that the amount of work done by this call can be calculated correctly later.

	// In addition to backing out because of a preemption, back out
	// if the GC CPU limiter is enabled.
	// 除了由于抢占而退出外，如果启用了 GC CPU 限制器，则退出。
	// In addition to backing out because of a preemption, back out if the GC CPU limiter is enabled.
	gp := getg().m.curg
	for !gp.preempt && !gcCPULimiter.limiting() && workFlushed+gcw.heapScanWork < scanWork {
		// See gcDrain comment.
		// 参见 gcDrain 注释。
		if work.full == 0 {
			// Try to get more work.
			// 尝试获取更多工作。
			gcw.balance()
		}

		// Try to get a buffer from the fast path.
		// 尝试从快速路径获取缓冲区。
		b := gcw.tryGetFast()
		if b == 0 {
			// Try to get a buffer from the global queue.
			// 尝试从全局队列获取缓冲区。
			b = gcw.tryGet()
			if b == 0 {
				// Flush the write barrier buffer;
				// this may create more work.
				// 刷新写屏障缓冲区；这可能会创建更多工作。
				wbBufFlush()
				b = gcw.tryGet()
			}
		}

		if b == 0 {
			// Try to do a root job.
			// 尝试执行根作业。
			if work.markrootNext < work.markrootJobs {
				job := atomic.Xadd(&work.markrootNext, +1) - 1
				if job < work.markrootJobs {
					workFlushed += markroot(gcw, job, false)
					continue
				}
			}
			// No heap or root jobs.
			// 没有堆或根作业。
			break
		}

		// Scan the object.
		// 扫描对象。
		scanobject(b, gcw)

		// Flush background scan work credit.
		// 刷新后台扫描工作信用。
		if gcw.heapScanWork >= gcCreditSlack {
			gcController.heapScanWork.Add(gcw.heapScanWork)
			workFlushed += gcw.heapScanWork
			gcw.heapScanWork = 0
		}
	}

	// Unlike gcDrain, there's no need to flush remaining work
	// here because this never flushes to bgScanCredit and
	// gcw.dispose will flush any remaining work to scanWork.
	// 与 gcDrain 不同，这里不需要刷新剩余的工作，因为此函数从不刷新到 bgScanCredit，
	// 并且 gcw.dispose 将剩余的工作刷新到 scanWork。

	return workFlushed + gcw.heapScanWork
}

// scanblock scans b as scanobject would, but using an explicit
// pointer bitmap instead of the heap bitmap.
//
// This is used to scan non-heap roots, so it does not update
// gcw.bytesMarked or gcw.heapScanWork.
//
// If stk != nil, possible stack pointers are also reported to stk.putPtr.
//
// scanblock 扫描 b，就像 scanobject 一样，但是使用显式的指针位图而不是堆位图。
// 这用于扫描非堆根，因此它不会更新 gcw.bytesMarked 或 gcw.heapScanWork。
// 如果 stk != nil，可能的栈指针也会报告给 stk.putPtr。
//
//go:nowritebarrier
func scanblock(b0, n0 uintptr, ptrmask *uint8, gcw *gcWork, stk *stackScanState) {
	// Use local copies of original parameters, so that a stack trace
	// due to one of the throws below shows the original block
	// base and extent.
	// 使用原始参数的本地副本，以便由于下面的抛出而产生的堆栈跟踪显示原始块的基址和范围。
	b := b0
	n := n0

	for i := uintptr(0); i < n; {
		// Find bits for the next word.
		// 查找下一个字的位。
		bits := uint32(*addb(ptrmask, i/(goarch.PtrSize*8)))
		if bits == 0 {
			i += goarch.PtrSize * 8
			continue
		}
		for j := 0; j < 8 && i < n; j++ {
			if bits&1 != 0 {
				// Same work as in scanobject; see comments there.
				// 与 scanobject 中的工作相同；请参阅那里的注释。
				p := *(*uintptr)(unsafe.Pointer(b + i)) // Read the word at the current offset. 读取当前偏移量处的字。
				if p != 0 {                             // If it's non-zero, it might be a pointer. 如果它非零，则可能是一个指针。
					if obj, span, objIndex := findObject(p, b, i); obj != 0 { // Check if it points to a valid heap object. 检查它是否指向一个有效的堆对象。
						greyobject(obj, b, i, span, gcw, objIndex) // If so, mark the object as grey. 如果是，则将该对象标记为灰色。
					} else if stk != nil && p >= stk.stack.lo && p < stk.stack.hi { // Otherwise, if we have stack information and it looks like a stack pointer... 否则，如果我们有堆栈信息，并且它看起来像一个堆栈指针...
						stk.putPtr(p, false) // ... then record it for stack scanning. ...然后记录它以进行堆栈扫描。
					}
				}
			}
			bits >>= 1
			i += goarch.PtrSize
		}
	}
}

// scanobject scans the object starting at b, adding pointers to gcw.
// b must point to the beginning of a heap object or an oblet.
// scanobject consults the GC bitmap for the pointer mask and the
// spans for the size of the object.
// scanobject 扫描从 b 开始的对象，并将指针添加到 gcw。
// b 必须指向堆对象或 oblet 的开头。
// scanobject 查询 GC 位图以获取指针掩码，并查询 span 以获取对象的大小。
//
//go:nowritebarrier
func scanobject(b uintptr, gcw *gcWork) {
	// Prefetch object before we scan it.
	//
	// This will overlap fetching the beginning of the object with initial
	// setup before we start scanning the object.
	sys.Prefetch(b) // 预取对象，在扫描之前。这将使获取对象的开始部分与开始扫描对象之前的初始设置重叠。

	// Find the bits for b and the size of the object at b.
	//
	// b is either the beginning of an object, in which case this
	// is the size of the object to scan, or it points to an
	// oblet, in which case we compute the size to scan below.
	s := spanOfUnchecked(b) // 获取 b 对应的 span。span 包含了对象的大小和类型信息。
	n := s.elemsize         // 获取 span 中元素的大小。对于堆对象，这通常是对象的大小。
	if n == 0 {
		throw("scanobject n == 0") // 如果大小为 0，则抛出异常。这表明 span 可能不正确。
	}
	if s.spanclass.noscan() {
		// Correctness-wise this is ok, but it's inefficient
		// if noscan objects reach here.
		throw("scanobject of a noscan object") // 如果对象是 noscan 对象，则抛出异常。这表明代码尝试扫描一个不应该被扫描的对象。从正确性的角度来看，这是可以的，但如果 noscan 对象到达这里，效率会很低。
	}

	var tp typePointers
	if n > maxObletBytes {
		// Large object. Break into oblets for better
		// parallelism and lower latency.
		// 大对象。分成 oblet 以获得更好的并行性和更低的延迟。
		if b == s.base() {
			// Enqueue the other oblets to scan later.
			// Some oblets may be in b's scalar tail, but
			// these will be marked as "no more pointers",
			// so we'll drop out immediately when we go to
			// scan those.
			// 将其他 oblet 排队以供稍后扫描。
			// 有些 oblet 可能在 b 的标量尾部，但
			// 这些将被标记为“没有更多指针”，
			// 所以当我们去扫描它们时，我们会立即退出。
			for oblet := b + maxObletBytes; oblet < s.base()+s.elemsize; oblet += maxObletBytes {
				if !gcw.putFast(oblet) {
					gcw.put(oblet)
				}
			}
		}

		// Compute the size of the oblet. Since this object
		// must be a large object, s.base() is the beginning
		// of the object.
		// 计算 oblet 的大小。由于此对象
		// 必须是一个大对象，s.base() 是
		// 对象的开头。
		n = s.base() + s.elemsize - b
		n = min(n, maxObletBytes)
		tp = s.typePointersOfUnchecked(s.base())
		tp = tp.fastForward(b-tp.addr, b+n)
	} else {
		tp = s.typePointersOfUnchecked(b)
	}

	var scanSize uintptr
	for {
		var addr uintptr
		// Get the next pointer using the fast path.
		// 使用快速路径获取下一个指针。
		if tp, addr = tp.nextFast(); addr == 0 {
			// Fast path failed, try the slower path.
			// 快速路径失败，尝试较慢的路径。
			if tp, addr = tp.next(b + n); addr == 0 {
				// No more pointers in this block.
				// 此块中没有更多指针。
				break
			}
		}

		// Keep track of farthest pointer we found, so we can
		// update heapScanWork. TODO: is there a better metric,
		// now that we can skip scalar portions pretty efficiently?
		scanSize = addr - b + goarch.PtrSize

		// Work here is duplicated in scanblock and above.
		// If you make changes here, make changes there too.
		obj := *(*uintptr)(unsafe.Pointer(addr))

		// At this point we have extracted the next potential pointer.
		// Quickly filter out nil and pointers back to the current object.
		// 在这一点上，我们已经提取了下一个潜在的指针。
		// 快速过滤掉 nil 和指向当前对象的指针。
		if obj != 0 && obj-b >= n {
			// Test if obj points into the Go heap and, if so,
			// mark the object.
			// 测试 obj 是否指向 Go 堆，如果是，
			// 标记该对象。
			//
			// Note that it's possible for findObject to
			// fail if obj points to a just-allocated heap
			// object because of a race with growing the
			// heap. In this case, we know the object was
			// just allocated and hence will be marked by
			// allocation itself.
			// 请注意，如果 obj 指向一个刚刚分配的堆
			// 对象，findObject 可能会失败，因为与增长
			// 堆存在竞争。在这种情况下，我们知道该对象
			// 刚刚被分配，因此将被分配本身标记。
			if obj, span, objIndex := findObject(obj, b, addr-b); obj != 0 {
				greyobject(obj, b, addr-b, span, gcw, objIndex)
			}
		}
	}
	gcw.bytesMarked += uint64(n)
	gcw.heapScanWork += int64(scanSize)
}

// scanConservative scans block [b, b+n) conservatively, treating any
// pointer-like value in the block as a pointer.
//
// If ptrmask != nil, only words that are marked in ptrmask are
// considered as potential pointers.
//
// If state != nil, it's assumed that [b, b+n) is a block in the stack
// and may contain pointers to stack objects.
// scanConservative 保守地扫描 [b, b+n) 块，将块中任何类似指针的值都视为指针。
//
// 如果 ptrmask != nil，则只有在 ptrmask 中标记的字被视为潜在的指针。
//
// 如果 state != nil，则假定 [b, b+n) 是堆栈中的一个块，并且可能包含指向堆栈对象的指针。
func scanConservative(b, n uintptr, ptrmask *uint8, gcw *gcWork, state *stackScanState) {
	if debugScanConservative {
		printlock()
		print("conservatively scanning [", hex(b), ",", hex(b+n), ")\n")
		hexdumpWords(b, b+n, func(p uintptr) byte {
			if ptrmask != nil {
				word := (p - b) / goarch.PtrSize
				bits := *addb(ptrmask, word/8)
				if (bits>>(word%8))&1 == 0 {
					return '$'
				}
			}

			val := *(*uintptr)(unsafe.Pointer(p))
			if state != nil && state.stack.lo <= val && val < state.stack.hi {
				return '@'
			}

			span := spanOfHeap(val)
			if span == nil {
				return ' '
			}
			idx := span.objIndex(val)
			if span.isFree(idx) {
				return ' '
			}
			return '*'
		})
		printunlock()
	}

	for i := uintptr(0); i < n; i += goarch.PtrSize {
		if ptrmask != nil {
			// ptrmask 不为空，表示存在指针掩码，需要根据掩码来判断是否扫描
			word := i / goarch.PtrSize
			// 计算当前偏移 i 对应的字在 ptrmask 中的索引
			bits := *addb(ptrmask, word/8)
			// 获取 ptrmask 中对应字节的值
			if bits == 0 {
				// 如果该字节的值为 0，表示该字节对应的 8 个字都不包含指针
				// Skip 8 words (the loop increment will do the 8th)
				// 跳过 8 个字（循环增量将处理第 8 个字）
				//
				// This must be the first time we've
				// seen this word of ptrmask, so i
				// must be 8-word-aligned, but check
				// our reasoning just in case.
				// 这必须是我们第一次看到 ptrmask 的这个字，所以 i 必须是 8 字对齐的，但为了以防万一，请检查我们的推理。
				if i%(goarch.PtrSize*8) != 0 {
					throw("misaligned mask")
				}
				i += goarch.PtrSize*8 - goarch.PtrSize
				// 将 i 增加 7 个字的大小，因为循环还会增加一个字的大小
				continue
				// 继续下一次循环
			}
			if (bits>>(word%8))&1 == 0 {
				// 如果该字在 ptrmask 中对应的位为 0，表示该字不包含指针
				continue
				// 继续下一次循环
			}
		}

		val := *(*uintptr)(unsafe.Pointer(b + i))
		// val 获取当前扫描地址的值，即 *(*uintptr)(unsafe.Pointer(b + i))

		// Check if val points into the stack.
		// 检查 val 是否指向栈内存
		if state != nil && state.stack.lo <= val && val < state.stack.hi {
			// val may point to a stack object. This
			// object may be dead from last cycle and
			// hence may contain pointers to unallocated
			// objects, but unlike heap objects we can't
			// tell if it's already dead. Hence, if all
			// pointers to this object are from
			// conservative scanning, we have to scan it
			// defensively, too.
			// val 可能指向一个栈对象。这个对象可能在上一个周期中已经死亡，
			// 因此可能包含指向未分配对象的指针，但与堆对象不同，我们无法判断它是否已经死亡。
			// 因此，如果指向此对象的所有指针都来自保守扫描，我们也必须防御性地扫描它。
			state.putPtr(val, true)
			// 将 val 添加到待扫描的指针列表中，并标记为保守扫描
			continue
			// 继续下一次循环
		}

		// Check if val points to a heap span.
		// 检查 val 是否指向堆 span
		span := spanOfHeap(val)
		// span 获取 val 指向的 span
		if span == nil {
			// 如果 span 为 nil，表示 val 不指向堆内存
			continue
			// 继续下一次循环
		}

		// Check if val points to an allocated object.
		// 检查 val 是否指向一个已分配的对象。
		idx := span.objIndex(val)
		// idx 获取 val 在 span 中的对象索引。
		if span.isFree(idx) {
			// 如果 span 中索引为 idx 的对象是空闲的，表示 val 指向的是一个空闲对象。
			continue
			// 继续下一次循环。
		}

		// val points to an allocated object. Mark it.
		// val 指向一个已分配的对象，标记它。
		obj := span.base() + idx*span.elemsize
		// obj 获取对象的起始地址。
		greyobject(obj, b, i, span, gcw, idx)
		// greyobject 将对象标记为灰色，并将其添加到待处理的灰色对象队列中。
	}
}

// Shade the object if it isn't already.
// The object is not nil and known to be in the heap.
// Preemption must be disabled.
// 如果对象尚未着色，则对其进行着色。
// 该对象不为 nil，并且已知在堆中。
// 必须禁用抢占。
//
//go:nowritebarrier
func shade(b uintptr) {
	// findObject 查找 b 指向的对象，如果找到，则返回对象地址、span 和对象索引。
	if obj, span, objIndex := findObject(b, 0, 0); obj != 0 {
		// 获取当前 P 的 gcWork 结构体指针。
		gcw := &getg().m.p.ptr().gcw
		// greyobject 将对象标记为灰色，并将其添加到待处理的灰色对象队列中。
		greyobject(obj, 0, 0, span, gcw, objIndex)
	}
}

// obj is the start of an object with mark mbits.
// If it isn't already marked, mark it and enqueue into gcw.
// base and off are for debugging only and could be removed.
//
// See also wbBufFlush1, which partially duplicates this logic.
//
// greyobject 将对象 obj 标记为灰色，如果尚未标记，则将其标记并加入到 gcw 队列中。
// obj 是带有 mark mbits 的对象的起始地址。
// 如果它尚未被标记，则标记它并将其加入 gcw 队列。
// base 和 off 仅用于调试，可以删除。
// 另请参见 wbBufFlush1，它部分重复了此逻辑。
//
//go:nowritebarrierrec
func greyobject(obj, base, off uintptr, span *mspan, gcw *gcWork, objIndex uintptr) {
	// obj should be start of allocation, and so must be at least pointer-aligned.
	// obj 应该是分配的起始地址，因此必须至少是指针对齐的。
	if obj&(goarch.PtrSize-1) != 0 {
		// 如果 obj 没有按照指针大小对齐，则抛出异常。
		throw("greyobject: obj not pointer-aligned")
	}
	mbits := span.markBitsForIndex(objIndex)
	// mbits 获取对象 obj 在 span 中的 mark bits。

	if useCheckmark {
		// 如果正在使用 checkmark，则调用 setCheckmark 设置标记。
		if setCheckmark(obj, base, off, mbits) {
			// 如果已经标记，则返回。
			return
		}
	} else {
		if debug.gccheckmark > 0 && span.isFree(objIndex) {
			// debug.gccheckmark > 0 并且 span 对应的对象是空闲的，则抛出异常
			print("runtime: marking free object ", hex(obj), " found at *(", hex(base), "+", hex(off), ")\n")
			gcDumpObject("base", base, off)
			gcDumpObject("obj", obj, ^uintptr(0))
			getg().m.traceback = 2
			throw("marking free object")
		}

		// If marked we have nothing to do.
		// 如果已经被标记，则直接返回
		if mbits.isMarked() {
			return
		}
		mbits.setMarked()
		// 设置 mbits 标记，表示对象已经被标记

		// Mark span.
		// 标记 span
		arena, pageIdx, pageMask := pageIndexOf(span.base())
		// 获取 span 对应的 arena、pageIdx 和 pageMask
		if arena.pageMarks[pageIdx]&pageMask == 0 {
			// 如果 arena.pageMarks[pageIdx]&pageMask 为 0，表示该页尚未被标记
			atomic.Or8(&arena.pageMarks[pageIdx], pageMask)
			// 使用原子操作将 arena.pageMarks[pageIdx] 的 pageMask 位设置为 1，表示该页已经被标记
		}

		// If this is a noscan object, fast-track it to black
		// instead of greying it.
		// 如果这是一个 noscan 对象，则直接将其标记为黑色，而不是将其标记为灰色
		if span.spanclass.noscan() {
			gcw.bytesMarked += uint64(span.elemsize)
			// 增加已标记的字节数
			return
		}
	}

	// We're adding obj to P's local workbuf, so it's likely
	// this object will be processed soon by the same P.
	// Even if the workbuf gets flushed, there will likely still be
	// some benefit on platforms with inclusive shared caches.
	//
	// 将 obj 添加到 P 的本地 workbuf 中，因此很可能
	// 此对象将很快被同一个 P 处理。
	// 即使 workbuf 被刷新，在具有包含性共享缓存的平台上
	// 仍然可能有一些好处。
	sys.Prefetch(obj)
	// Queue the obj for scanning.
	// 将 obj 排队以进行扫描。
	if !gcw.putFast(obj) {
		// 如果无法快速放入，则放入到正常的队列中
		gcw.put(obj)
	}
}

// gcDumpObject dumps the contents of obj for debugging and marks the
// field at byte offset off in obj.
//
// 为调试目的转储 obj 的内容，并标记 obj 中偏移量为 off 的字段。
func gcDumpObject(label string, obj, off uintptr) {
	s := spanOf(obj) // 获取对象 obj 对应的 span
	// get the span of object obj

	print(label, "=", hex(obj)) // 打印标签和对象的十六进制地址
	// print the label and the hexadecimal address of the object

	if s == nil { // 如果 span 为 nil，则打印 "s=nil" 并返回
		// if the span is nil, print "s=nil" and return
		print(" s=nil\n")
		return
	}
	print(" s.base()=", hex(s.base()), " s.limit=", hex(s.limit), " s.spanclass=", s.spanclass, " s.elemsize=", s.elemsize, " s.state=") // 打印 span 的 base、limit、spanclass、elemsize 和 state
	// print the base, limit, spanclass, elemsize, and state of the span

	if state := s.state.get(); 0 <= state && int(state) < len(mSpanStateNames) { // 获取 span 的状态，并判断是否在 mSpanStateNames 范围内
		// get the state of the span and check if it is within the range of mSpanStateNames
		print(mSpanStateNames[state], "\n") // 打印 span 的状态名称
		// print the name of the span's state
	} else {
		print("unknown(", state, ")\n") // 如果 span 的状态不在 mSpanStateNames 范围内，则打印 "unknown" 和状态值
		// if the state of the span is not within the range of mSpanStateNames, print "unknown" and the state value
	}

	skipped := false
	size := s.elemsize // size is the element size of the span.
	// size 是 span 的元素大小。
	if s.state.get() == mSpanManual && size == 0 {
		// We're printing something from a stack frame. We
		// don't know how big it is, so just show up to an
		// including off.
		// 我们正在打印堆栈帧中的内容。我们不知道它有多大，所以只显示到包括 off 的位置。
		size = off + goarch.PtrSize // Set size to at least off + pointer size.
		// 将 size 设置为至少 off + 指针大小。
	}
	for i := uintptr(0); i < size; i += goarch.PtrSize {
		// For big objects, just print the beginning (because
		// that usually hints at the object's type) and the
		// fields around off.
		// 对于大型对象，只打印开头部分（因为这通常暗示对象的类型）以及 off 附近的字段。
		if !(i < 128*goarch.PtrSize || off-16*goarch.PtrSize < i && i < off+16*goarch.PtrSize) {
			skipped = true
			continue
		}
		if skipped {
			print(" ...\n")
			skipped = false
		}
		print(" *(", label, "+", i, ") = ", hex(*(*uintptr)(unsafe.Pointer(obj + i))))
		if i == off {
			print(" <==")
		}
		print("\n")
	}
	if skipped {
		print(" ...\n")
	}
}

// gcmarknewobject marks a newly allocated object black. obj must
// not contain any non-nil pointers.
// gcmarknewobject 将新分配的对象标记为黑色。obj 必须不包含任何非 nil 指针。
//
// This is nosplit so it can manipulate a gcWork without preemption.
// 这是 nosplit，因此它可以操作 gcWork 而无需抢占。
//
// gcmarknewobject marks a newly allocated object as black.
// span: The span containing the object.
// obj: The address of the newly allocated object.
// gcmarknewobject 将新分配的对象标记为黑色。
// span: 包含该对象的 span。
// obj: 新分配对象的地址。
//
//go:nowritebarrier
//go:nosplit
func gcmarknewobject(span *mspan, obj uintptr) {
	// If checkmark is enabled, it indicates an error because the world should be stopped.
	// 如果启用了 checkmark，则表示出现错误，因为此时世界应该停止。
	if useCheckmark { // The world should be stopped so this should not happen.
		throw("gcmarknewobject called while doing checkmark")
	}

	// Mark object.
	// Get the index of the object within the span.
	// 获取对象在 span 中的索引。
	objIndex := span.objIndex(obj)
	// Set the corresponding mark bit to indicate that the object is marked (black).
	// 设置相应的标记位，以指示该对象已被标记（黑色）。
	span.markBitsForIndex(objIndex).setMarked()

	// Mark span.
	// Get the arena, page index, and page mask for the span's base address.
	// 获取 span 的基本地址的 arena、页面索引和页面掩码。
	arena, pageIdx, pageMask := pageIndexOf(span.base())
	// If the page is not already marked, mark it.
	// 如果页面尚未标记，则标记它。
	if arena.pageMarks[pageIdx]&pageMask == 0 {
		atomic.Or8(&arena.pageMarks[pageIdx], pageMask)
	}

	// Update the number of bytes marked by the garbage collector.
	// 更新垃圾收集器标记的字节数。
	gcw := &getg().m.p.ptr().gcw
	gcw.bytesMarked += uint64(span.elemsize)
}

// gcMarkTinyAllocs greys all active tiny alloc blocks.
//
// The world must be stopped.
// gcMarkTinyAllocs 将所有活动的微分配块标记为灰色。
//
// 必须停止世界。
func gcMarkTinyAllocs() {
	assertWorldStopped() // 确保世界已停止

	for _, p := range allp { // 遍历所有处理器
		c := p.mcache                // 获取处理器的 mcache
		if c == nil || c.tiny == 0 { // 如果 mcache 为 nil 或者没有微分配，则跳过
			continue
		}
		_, span, objIndex := findObject(c.tiny, 0, 0) // 查找微分配对象所在的 span 和索引
		gcw := &p.gcw                                 // 获取处理器的 gcWork
		greyobject(c.tiny, 0, 0, span, gcw, objIndex) // 将微分配对象标记为灰色
	}
}
