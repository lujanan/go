// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Page heap.
//
// See malloc.go for overview.

package runtime

import (
	"internal/cpu"
	"internal/goarch"
	"internal/runtime/atomic"
	"runtime/internal/sys"
	"unsafe"
)

const (
	// minPhysPageSize is a lower-bound on the physical page size. The
	// true physical page size may be larger than this. In contrast,
	// sys.PhysPageSize is an upper-bound on the physical page size.
	// minPhysPageSize 是物理页大小的下界。实际的物理页大小可能比这个值大。
	// 相比之下，sys.PhysPageSize 是物理页大小的上界。
	minPhysPageSize = 4096

	// maxPhysPageSize is the maximum page size the runtime supports.
	// maxPhysPageSize 是运行时支持的最大页大小。512kb
	maxPhysPageSize = 512 << 10

	// maxPhysHugePageSize sets an upper-bound on the maximum huge page size
	// that the runtime supports.
	// maxPhysHugePageSize 设置了运行时支持的最大大页大小的上界。
	maxPhysHugePageSize = pallocChunkBytes

	// pagesPerReclaimerChunk indicates how many pages to scan from the
	// pageInUse bitmap at a time. Used by the page reclaimer.
	//
	// Higher values reduce contention on scanning indexes (such as
	// h.reclaimIndex), but increase the minimum latency of the
	// operation.
	//
	// The time required to scan this many pages can vary a lot depending
	// on how many spans are actually freed. Experimentally, it can
	// scan for pages at ~300 GB/ms on a 2.6GHz Core i7, but can only
	// free spans at ~32 MB/ms. Using 512 pages bounds this at
	// roughly 100µs.
	//
	// Must be a multiple of the pageInUse bitmap element size and
	// must also evenly divide pagesPerArena.
	// pagesPerReclaimerChunk 表示页面回收器每次从 pageInUse 位图中扫描的页面数量。
	//
	// 较大的值可以减少扫描索引（如 h.reclaimIndex）的竞争，但会增加操作的最小延迟。
	//
	// 扫描这些页面所需的时间可能因实际释放的 spans 数量而有很大差异。
	// 实验表明，在 2.6GHz 的 Core i7 上，它可以以约 300 GB/ms 的速度扫描页面，
	// 但只能以约 32 MB/ms 的速度释放 spans。使用 512 页可以将此限制在约 100µs。
	//
	// 必须是 pageInUse 位图元素大小的倍数，并且必须能整除 pagesPerArena。
	pagesPerReclaimerChunk = 512

	// physPageAlignedStacks indicates whether stack allocations must be
	// physical page aligned. This is a requirement for MAP_STACK on
	// OpenBSD.
	// physPageAlignedStacks 表示栈分配是否必须按物理页对齐。
	// 这是 OpenBSD 上 MAP_STACK 的要求。
	physPageAlignedStacks = GOOS == "openbsd"
)

// Main malloc heap.
// The heap itself is the "free" and "scav" treaps,
// but all the other global data is here too.
//
// mheap must not be heap-allocated because it contains mSpanLists,
// which must not be heap-allocated.
// 主内存分配堆。
// 堆本身是 "free" 和 "scav" 树堆，
// 但所有其他全局数据也在这里。
//
// mheap 不能从堆上分配，因为它包含 mSpanLists，
// 而 mSpanLists 不能从堆上分配。
type mheap struct {
	_ sys.NotInHeap

	// lock must only be acquired on the system stack, otherwise a g
	// could self-deadlock if its stack grows with the lock held.
	// lock 只能在系统栈上获取，否则如果 g 的栈在持有锁的情况下增长，
	// 可能会导致自死锁。
	lock mutex

	pages pageAlloc // page allocation data structure // 页面分配数据结构

	sweepgen uint32 // sweep generation, see comment in mspan; written during STW // 扫描代，参见 mspan 中的注释；在 STW 期间写入

	// allspans is a slice of all mspans ever created. Each mspan
	// appears exactly once.
	//
	// The memory for allspans is manually managed and can be
	// reallocated and move as the heap grows.
	//
	// In general, allspans is protected by mheap_.lock, which
	// prevents concurrent access as well as freeing the backing
	// store. Accesses during STW might not hold the lock, but
	// must ensure that allocation cannot happen around the
	// access (since that may free the backing store).
	// allspans 是所有已创建的 mspan 的切片。每个 mspan 只出现一次。
	//
	// allspans 的内存是手动管理的，可以随着堆的增长而重新分配和移动。
	//
	// 通常，allspans 由 mheap_.lock 保护，这可以防止并发访问以及释放底层存储。
	// 在 STW 期间的访问可能不持有锁，但必须确保在访问期间不会发生分配
	//（因为这可能会释放底层存储）。
	allspans []*mspan // all spans out there // 所有的 span

	// Proportional sweep
	//
	// These parameters represent a linear function from gcController.heapLive
	// to page sweep count. The proportional sweep system works to
	// stay in the black by keeping the current page sweep count
	// above this line at the current gcController.heapLive.
	//
	// The line has slope sweepPagesPerByte and passes through a
	// basis point at (sweepHeapLiveBasis, pagesSweptBasis). At
	// any given time, the system is at (gcController.heapLive,
	// pagesSwept) in this space.
	//
	// It is important that the line pass through a point we
	// control rather than simply starting at a 0,0 origin
	// because that lets us adjust sweep pacing at any time while
	// accounting for current progress. If we could only adjust
	// the slope, it would create a discontinuity in debt if any
	// progress has already been made.
	// 比例扫描
	//
	// 这些参数表示从 gcController.heapLive 到页面扫描计数的线性函数。
	// 比例扫描系统通过保持当前页面扫描计数在当前 gcController.heapLive
	// 的这条线之上来保持盈余。
	//
	// 这条线有斜率 sweepPagesPerByte，并通过基点 (sweepHeapLiveBasis, pagesSweptBasis)。
	// 在任何给定时间，系统在这个空间中的位置是 (gcController.heapLive, pagesSwept)。
	//
	// 重要的是，这条线通过我们控制的一个点，而不是简单地从 0,0 原点开始，
	// 因为这让我们可以在考虑当前进度的同时随时调整扫描速度。
	// 如果我们只能调整斜率，那么如果已经取得任何进展，就会在债务中产生不连续性。
	pagesInUse         atomic.Uintptr // pages of spans in stats mSpanInUse // 处于 mSpanInUse 状态的 span 的页面数
	pagesSwept         atomic.Uint64  // pages swept this cycle // 本周期扫描的页面数
	pagesSweptBasis    atomic.Uint64  // pagesSwept to use as the origin of the sweep ratio // 用作扫描比例原点的 pagesSwept
	sweepHeapLiveBasis uint64         // value of gcController.heapLive to use as the origin of sweep ratio; written with lock, read without // 用作扫描比例原点的 gcController.heapLive 值；写入时加锁，读取时不加锁
	sweepPagesPerByte  float64        // proportional sweep ratio; written with lock, read without // 比例扫描比率；写入时加锁，读取时不加锁

	// Page reclaimer state
	// 页面回收器状态

	// reclaimIndex is the page index in allArenas of next page to
	// reclaim. Specifically, it refers to page (i %
	// pagesPerArena) of arena allArenas[i / pagesPerArena].
	//
	// If this is >= 1<<63, the page reclaimer is done scanning
	// the page marks.
	// reclaimIndex 是 allArenas 中下一个要回收的页面的索引。
	// 具体来说，它指向 arena allArenas[i / pagesPerArena] 中的
	// 页面 (i % pagesPerArena)。
	//
	// 如果这个值 >= 1<<63，页面回收器已完成扫描页面标记。
	reclaimIndex atomic.Uint64

	// reclaimCredit is spare credit for extra pages swept. Since
	// the page reclaimer works in large chunks, it may reclaim
	// more than requested. Any spare pages released go to this
	// credit pool.
	// reclaimCredit 是额外扫描页面的备用信用。
	// 由于页面回收器以大块工作，它可能会回收比请求更多的页面。
	// 任何释放的备用页面都会进入这个信用池。
	reclaimCredit atomic.Uintptr

	_ cpu.CacheLinePad // prevents false-sharing between arenas and preceding variables // 防止 arenas 和前面的变量之间的伪共享

	// arenas is the heap arena map. It points to the metadata for
	// the heap for every arena frame of the entire usable virtual
	// address space.
	//
	// Use arenaIndex to compute indexes into this array.
	//
	// For regions of the address space that are not backed by the
	// Go heap, the arena map contains nil.
	//
	// Modifications are protected by mheap_.lock. Reads can be
	// performed without locking; however, a given entry can
	// transition from nil to non-nil at any time when the lock
	// isn't held. (Entries never transitions back to nil.)
	//
	// In general, this is a two-level mapping consisting of an L1
	// map and possibly many L2 maps. This saves space when there
	// are a huge number of arena frames. However, on many
	// platforms (even 64-bit), arenaL1Bits is 0, making this
	// effectively a single-level map. In this case, arenas[0]
	// will never be nil.
	// arenas 是堆 arena 映射。它指向整个可用虚拟地址空间中
	// 每个 arena 帧的堆元数据。
	//
	// 使用 arenaIndex 计算这个数组的索引。
	//
	// 对于不由 Go 堆支持的地址空间区域，arena 映射包含 nil。
	//
	// 修改受 mheap_.lock 保护。读取可以在不加锁的情况下执行；
	// 但是，当不持有锁时，任何条目都可能随时从 nil 转换为非 nil。
	//（条目永远不会转换回 nil。）
	//
	// 通常，这是一个两级映射，由 L1 映射和可能的多个 L2 映射组成。
	// 当有大量 arena 帧时，这可以节省空间。然而，在许多平台（甚至是 64 位）上，
	// arenaL1Bits 为 0，使这实际上成为单级映射。在这种情况下，arenas[0] 永远不会是 nil。
	arenas [1 << arenaL1Bits]*[1 << arenaL2Bits]*heapArena

	// arenasHugePages indicates whether arenas' L2 entries are eligible
	// to be backed by huge pages.
	// arenasHugePages 表示 arenas 的 L2 条目是否有资格由大页支持。
	arenasHugePages bool

	// heapArenaAlloc is pre-reserved space for allocating heapArena
	// objects. This is only used on 32-bit, where we pre-reserve
	// this space to avoid interleaving it with the heap itself.
	// heapArenaAlloc 是预保留的用于分配 heapArena 对象的空间。
	// 这仅在 32 位系统上使用，我们预保留这个空间以避免它与堆本身交错。
	heapArenaAlloc linearAlloc

	// arenaHints is a list of addresses at which to attempt to
	// add more heap arenas. This is initially populated with a
	// set of general hint addresses, and grown with the bounds of
	// actual heap arena ranges.
	// arenaHints 是一个地址列表，用于尝试添加更多的堆 arena。
	// 它最初填充了一组通用的提示地址，并随着实际堆 arena 范围的边界而增长。
	arenaHints *arenaHint

	// arena is a pre-reserved space for allocating heap arenas
	// (the actual arenas). This is only used on 32-bit.
	// arena 是预保留的用于分配堆 arena（实际的 arena）的空间。
	// 这仅在 32 位系统上使用。
	arena linearAlloc

	// allArenas is the arenaIndex of every mapped arena. This can
	// be used to iterate through the address space.
	//
	// Access is protected by mheap_.lock. However, since this is
	// append-only and old backing arrays are never freed, it is
	// safe to acquire mheap_.lock, copy the slice header, and
	// then release mheap_.lock.
	// allArenas 是每个已映射 arena 的 arenaIndex。这可以用于遍历地址空间。
	//
	// 访问受 mheap_.lock 保护。然而，由于这是仅追加的，
	// 且旧的底层数组永远不会被释放，所以获取 mheap_.lock、
	// 复制切片头，然后释放 mheap_.lock 是安全的。
	allArenas []arenaIdx

	// sweepArenas is a snapshot of allArenas taken at the
	// beginning of the sweep cycle. This can be read safely by
	// simply blocking GC (by disabling preemption).
	// sweepArenas 是在清扫周期开始时获取的 allArenas 的快照。
	// 通过简单地阻止 GC（通过禁用抢占），可以安全地读取它。
	sweepArenas []arenaIdx

	// markArenas is a snapshot of allArenas taken at the beginning
	// of the mark cycle. Because allArenas is append-only, neither
	// this slice nor its contents will change during the mark, so
	// it can be read safely.
	// markArenas 是在标记周期开始时获取的 allArenas 的快照。
	// 由于 allArenas 是仅追加的，在标记期间这个切片及其内容都不会改变，
	// 所以可以安全地读取它。
	markArenas []arenaIdx

	// curArena is the arena that the heap is currently growing
	// into. This should always be physPageSize-aligned.
	// curArena 是堆当前正在扩展到的 arena。
	// 它应该始终按 physPageSize 对齐。
	curArena struct {
		base, end uintptr
	}

	// central free lists for small size classes.
	// the padding makes sure that the mcentrals are
	// spaced CacheLinePadSize bytes apart, so that each mcentral.lock
	// gets its own cache line.
	// central is indexed by spanClass.
	// 小尺寸类别的中央空闲列表。
	// 填充确保 mcentrals 之间间隔 CacheLinePadSize 字节，
	// 这样每个 mcentral.lock 都有自己的缓存行。
	// central 按 spanClass 索引。
	central [numSpanClasses]struct {
		mcentral mcentral
		pad      [(cpu.CacheLinePadSize - unsafe.Sizeof(mcentral{})%cpu.CacheLinePadSize) % cpu.CacheLinePadSize]byte
	}

	spanalloc fixalloc // allocator for span*
	// spanalloc 是用于分配 span* 的固定大小分配器
	cachealloc fixalloc // allocator for mcache*
	// cachealloc 是用于分配 mcache* 的固定大小分配器
	specialfinalizeralloc fixalloc // allocator for specialfinalizer*
	// specialfinalizeralloc 是用于分配 specialfinalizer* 的固定大小分配器
	specialprofilealloc fixalloc // allocator for specialprofile*
	// specialprofilealloc 是用于分配 specialprofile* 的固定大小分配器
	specialReachableAlloc fixalloc // allocator for specialReachable
	// specialReachableAlloc 是用于分配 specialReachable 的固定大小分配器
	specialPinCounterAlloc fixalloc // allocator for specialPinCounter
	// specialPinCounterAlloc 是用于分配 specialPinCounter 的固定大小分配器
	specialWeakHandleAlloc fixalloc // allocator for specialWeakHandle
	// specialWeakHandleAlloc 是用于分配 specialWeakHandle 的固定大小分配器
	speciallock mutex // lock for special record allocators.
	// speciallock 是用于保护特殊记录分配器的互斥锁
	arenaHintAlloc fixalloc // allocator for arenaHints
	// arenaHintAlloc 是用于分配 arenaHints 的固定大小分配器

	// User arena state.
	//
	// Protected by mheap_.lock.
	// 用户 arena 状态。
	//
	// 受 mheap_.lock 保护。
	userArena struct {
		// arenaHints is a list of addresses at which to attempt to
		// add more heap arenas for user arena chunks. This is initially
		// populated with a set of general hint addresses, and grown with
		// the bounds of actual heap arena ranges.
		// arenaHints 是一个地址列表，用于尝试为用户 arena 块添加更多的堆 arena。
		// 它最初填充了一组通用的提示地址，并随着实际堆 arena 范围的边界而增长。
		arenaHints *arenaHint

		// quarantineList is a list of user arena spans that have been set to fault, but
		// are waiting for all pointers into them to go away. Sweeping handles
		// identifying when this is true, and moves the span to the ready list.
		// quarantineList 是一个用户 arena span 的列表，这些 span 已被设置为故障，
		// 但正在等待所有指向它们的指针消失。清扫器负责识别这种情况，
		// 并将 span 移动到就绪列表。
		quarantineList mSpanList

		// readyList is a list of empty user arena spans that are ready for reuse.
		// readyList 是一个空用户 arena span 的列表，这些 span 已准备好重用。
		readyList mSpanList
	}

	unused *specialfinalizer // never set, just here to force the specialfinalizer type into DWARF
}

var mheap_ mheap

// A heapArena stores metadata for a heap arena. heapArenas are stored
// outside of the Go heap and accessed via the mheap_.arenas index.
// heapArena 存储堆 arena 的元数据。heapArenas 存储在 Go 堆之外，
// 并通过 mheap_.arenas 索引访问。
type heapArena struct {
	_ sys.NotInHeap

	// spans maps from virtual address page ID within this arena to *mspan.
	// For allocated spans, their pages map to the span itself.
	// For free spans, only the lowest and highest pages map to the span itself.
	// Internal pages map to an arbitrary span.
	// For pages that have never been allocated, spans entries are nil.
	//
	// Modifications are protected by mheap.lock. Reads can be
	// performed without locking, but ONLY from indexes that are
	// known to contain in-use or stack spans. This means there
	// must not be a safe-point between establishing that an
	// address is live and looking it up in the spans array.
	// spans 将 arena 内的虚拟地址页面 ID 映射到 *mspan。
	// 对于已分配的 spans，它们的页面映射到 span 本身。
	// 对于空闲的 spans，只有最低和最高的页面映射到 span 本身。
	// 内部页面映射到任意的 span。
	// 对于从未分配过的页面，spans 条目为 nil。
	//
	// 修改受 mheap.lock 保护。读取可以在不加锁的情况下进行，
	// 但只能从已知包含使用中或栈 spans 的索引进行。
	// 这意味着在确定地址是活动的和从 spans 数组中查找它之间不能有安全点。
	spans [pagesPerArena]*mspan

	// pageInUse is a bitmap that indicates which spans are in
	// state mSpanInUse. This bitmap is indexed by page number,
	// but only the bit corresponding to the first page in each
	// span is used.
	//
	// Reads and writes are atomic.
	// pageInUse 是一个位图，指示哪些 spans 处于 mSpanInUse 状态。
	// 这个位图按页面编号索引，但只使用每个 span 中第一页对应的位。
	//
	// 读取和写入是原子的。
	pageInUse [pagesPerArena / 8]uint8

	// pageMarks is a bitmap that indicates which spans have any
	// marked objects on them. Like pageInUse, only the bit
	// corresponding to the first page in each span is used.
	//
	// Writes are done atomically during marking. Reads are
	// non-atomic and lock-free since they only occur during
	// sweeping (and hence never race with writes).
	//
	// This is used to quickly find whole spans that can be freed.
	//
	// TODO(austin): It would be nice if this was uint64 for
	// faster scanning, but we don't have 64-bit atomic bit
	// operations.
	// pageMarks 是一个位图，指示哪些 spans 上有任何标记的对象。
	// 与 pageInUse 类似，只使用每个 span 中第一页对应的位。
	//
	// 在标记期间写入是原子的。读取是非原子的且无锁的，
	// 因为它们只在清扫期间发生（因此永远不会与写入竞争）。
	//
	// 这用于快速找到可以释放的整个 spans。
	//
	// TODO(austin): 如果这是 uint64 会更好，可以更快地扫描，
	// 但我们没有 64 位原子位操作。
	pageMarks [pagesPerArena / 8]uint8

	// pageSpecials is a bitmap that indicates which spans have
	// specials (finalizers or other). Like pageInUse, only the bit
	// corresponding to the first page in each span is used.
	//
	// Writes are done atomically whenever a special is added to
	// a span and whenever the last special is removed from a span.
	// Reads are done atomically to find spans containing specials
	// during marking.
	// pageSpecials 是一个位图，指示哪些 spans 有特殊对象（终结器或其他）。
	// 与 pageInUse 类似，只使用每个 span 中第一页对应的位。
	//
	// 每当向 span 添加特殊对象或从 span 中移除最后一个特殊对象时，
	// 写入是原子的。在标记期间，读取是原子的，用于查找包含特殊对象的 spans。
	pageSpecials [pagesPerArena / 8]uint8

	// checkmarks stores the debug.gccheckmark state. It is only
	// used if debug.gccheckmark > 0.
	// checkmarks 存储 debug.gccheckmark 状态。
	// 仅在 debug.gccheckmark > 0 时使用。
	checkmarks *checkmarksMap

	// zeroedBase marks the first byte of the first page in this
	// arena which hasn't been used yet and is therefore already
	// zero. zeroedBase is relative to the arena base.
	// Increases monotonically until it hits heapArenaBytes.
	//
	// This field is sufficient to determine if an allocation
	// needs to be zeroed because the page allocator follows an
	// address-ordered first-fit policy.
	//
	// Read atomically and written with an atomic CAS.
	// zeroedBase 标记此 arena 中第一个未使用页面的第一个字节，
	// 因此它已经是零。zeroedBase 相对于 arena 基址。
	// 单调递增直到达到 heapArenaBytes。
	//
	// 这个字段足以确定分配是否需要清零，因为页面分配器遵循
	// 地址有序的首次适应策略。
	//
	// 原子读取并使用原子 CAS 写入。
	zeroedBase uintptr
}

// arenaHint is a hint for where to grow the heap arenas. See
// mheap_.arenaHints.
// arenaHint 是一个提示，用于指示堆 arena 应该在哪里增长。参见 mheap_.arenaHints。
type arenaHint struct {
	_    sys.NotInHeap // 表示这个结构体不应该从堆上分配
	addr uintptr       // 建议的 arena 起始地址
	down bool          // 如果为 true，表示应该从 addr 向下增长；如果为 false，表示应该从 addr 向上增长
	next *arenaHint    // 指向下一个 arenaHint 的指针，形成一个链表
}

// An mspan is a run of pages.
//
// When a mspan is in the heap free treap, state == mSpanFree
// and heapmap(s->start) == span, heapmap(s->start+s->npages-1) == span.
// If the mspan is in the heap scav treap, then in addition to the
// above scavenged == true. scavenged == false in all other cases.
//
// When a mspan is allocated, state == mSpanInUse or mSpanManual
// and heapmap(i) == span for all s->start <= i < s->start+s->npages.

// Every mspan is in one doubly-linked list, either in the mheap's
// busy list or one of the mcentral's span lists.

// An mspan representing actual memory has state mSpanInUse,
// mSpanManual, or mSpanFree. Transitions between these states are
// constrained as follows:
//
//   - A span may transition from free to in-use or manual during any GC
//     phase.
//
//   - During sweeping (gcphase == _GCoff), a span may transition from
//     in-use to free (as a result of sweeping) or manual to free (as a
//     result of stacks being freed).
//
//   - During GC (gcphase != _GCoff), a span *must not* transition from
//     manual or in-use to free. Because concurrent GC may read a pointer
//     and then look up its span, the span state must be monotonic.
//
// Setting mspan.state to mSpanInUse or mSpanManual must be done
// atomically and only after all other span fields are valid.
// Likewise, if inspecting a span is contingent on it being
// mSpanInUse, the state should be loaded atomically and checked
// before depending on other fields. This allows the garbage collector
// to safely deal with potentially invalid pointers, since resolving
// such pointers may race with a span being allocated.

// mspan 是一组连续的页面。
//
// 当 mspan 在堆的空闲树堆中时，state == mSpanFree
// 且 heapmap(s->start) == span，heapmap(s->start+s->npages-1) == span。
// 如果 mspan 在堆的回收树堆中，则除了上述条件外，scavenged == true。
// 在其他所有情况下，scavenged == false。
//
// 当 mspan 被分配时，state == mSpanInUse 或 mSpanManual，
// 且对于所有 s->start <= i < s->start+s->npages，heapmap(i) == span。
//
// 每个 mspan 都在一个双向链表中，要么在 mheap 的忙碌列表中，
// 要么在某个 mcentral 的 span 列表中。
//
// 表示实际内存的 mspan 具有状态 mSpanInUse、mSpanManual 或 mSpanFree。
// 这些状态之间的转换受到以下约束：
//
//   - 在任何 GC 阶段，span 可以从空闲状态转换为使用中或手动管理状态。
//
//   - 在清扫阶段（gcphase == _GCoff），span 可以从使用中转换为空闲状态
//     （作为清扫的结果）或从手动管理转换为空闲状态（作为栈被释放的结果）。
//
//   - 在 GC 期间（gcphase != _GCoff），span *不得*从手动管理或使用中状态
//     转换为空闲状态。因为并发 GC 可能会读取一个指针然后查找其 span，
//     span 状态必须是单调的。
//
// 将 mspan.state 设置为 mSpanInUse 或 mSpanManual 必须是原子的，
// 并且只能在所有其他 span 字段有效之后进行。
// 同样，如果检查 span 依赖于它处于 mSpanInUse 状态，
// 则应该原子地加载状态并在依赖其他字段之前进行检查。
// 这允许垃圾收集器安全地处理可能无效的指针，
// 因为解析这些指针可能与 span 的分配竞争。
type mSpanState uint8

const (
	mSpanDead   mSpanState = iota
	mSpanInUse             // allocated for garbage collected heap
	mSpanManual            // allocated for manual management (e.g., stack allocator)
	// 已死亡的 span 状态
	// 正在使用的 span 状态，用于垃圾回收堆的分配
	// 手动管理的 span 状态，用于手动内存管理（例如栈分配器）
)

// mSpanStateNames are the names of the span states, indexed by
// mSpanState.
// mSpanStateNames 是 span 状态的名称，按 mSpanState 索引。
var mSpanStateNames = []string{
	"mSpanDead",   // 已死亡的 span
	"mSpanInUse",  // 正在使用的 span
	"mSpanManual", // 手动管理的 span
}

// mSpanStateBox holds an atomic.Uint8 to provide atomic operations on
// an mSpanState. This is a separate type to disallow accidental comparison
// or assignment with mSpanState.
// mSpanStateBox 持有一个 atomic.Uint8 以提供对 mSpanState 的原子操作。
// 这是一个单独的类型，用于防止与 mSpanState 的意外比较或赋值。
type mSpanStateBox struct {
	s atomic.Uint8
}

// It is nosplit to match get, below.
// 使用 nosplit 以匹配下面的 get 函数

//go:nosplit
func (b *mSpanStateBox) set(s mSpanState) {
	b.s.Store(uint8(s))
}

// It is nosplit because it's called indirectly by typedmemclr,
// which must not be preempted.
// 使用 nosplit 是因为它被 typedmemclr 间接调用，
// 而 typedmemclr 不能被抢占

//go:nosplit
func (b *mSpanStateBox) get() mSpanState {
	return mSpanState(b.s.Load())
}

// mSpanList heads a linked list of spans.
// mSpanList 是一个 span 链表的头节点
type mSpanList struct {
	_     sys.NotInHeap
	first *mspan // first span in list, or nil if none
	last  *mspan // last span in list, or nil if none
}

type mspan struct {
	_    sys.NotInHeap
	next *mspan     // next span in list, or nil if none
	prev *mspan     // previous span in list, or nil if none
	list *mSpanList // For debugging.
	// 下一个 span 的指针，如果没有则为 nil
	// 上一个 span 的指针，如果没有则为 nil
	// 用于调试的 span 列表

	startAddr uintptr // address of first byte of span aka s.base()
	npages    uintptr // number of pages in span
	// span 第一个字节的地址，即 s.base()
	// span 中的页数

	manualFreeList gclinkptr // list of free objects in mSpanManual spans
	// mSpanManual 类型 span 中的空闲对象列表

	// freeindex is the slot index between 0 and nelems at which to begin scanning
	// for the next free object in this span.
	// Each allocation scans allocBits starting at freeindex until it encounters a 0
	// indicating a free object. freeindex is then adjusted so that subsequent scans begin
	// just past the newly discovered free object.
	//
	// If freeindex == nelem, this span has no free objects.
	//
	// allocBits is a bitmap of objects in this span.
	// If n >= freeindex and allocBits[n/8] & (1<<(n%8)) is 0
	// then object n is free;
	// otherwise, object n is allocated. Bits starting at nelem are
	// undefined and should never be referenced.
	//
	// Object n starts at address n*elemsize + (start << pageShift).
	freeindex uint16
	// freeindex 是在 0 到 nelems 之间的槽索引，用于开始扫描
	// span 中的下一个空闲对象。每次分配从 freeindex 开始
	// 扫描 allocBits，直到遇到 0 表示空闲对象。然后调整
	// freeindex 使后续扫描从新发现的对象之后开始。
	//
	// 如果 freeindex == nelem，表示这个 span 没有空闲对象。
	//
	// allocBits 是这个 span 中对象的位图。
	// 如果 n >= freeindex 且 allocBits[n/8] & (1<<(n%8)) 为 0，
	// 则对象 n 是空闲的；否则对象 n 已分配。
	// 从 nelem 开始的位是未定义的，不应该被引用。
	//
	// 对象 n 的起始地址是 n*elemsize + (start << pageShift)。

	// TODO: Look up nelems from sizeclass and remove this field if it
	// helps performance.
	nelems uint16 // number of object in the span.
	// TODO: 从 sizeclass 查找 nelems 并移除这个字段，如果
	// 这有助于性能的话。
	// span 中的对象数量

	// freeIndexForScan is like freeindex, except that freeindex is
	// used by the allocator whereas freeIndexForScan is used by the
	// GC scanner. They are two fields so that the GC sees the object
	// is allocated only when the object and the heap bits are
	// initialized (see also the assignment of freeIndexForScan in
	// mallocgc, and issue 54596).
	freeIndexForScan uint16
	// freeIndexForScan 类似于 freeindex，但 freeindex 用于分配器，
	// 而 freeIndexForScan 用于 GC 扫描器。它们分为两个字段，
	// 这样 GC 只有在对象和堆位图都初始化后才能看到对象已分配
	// （另见 mallocgc 中 freeIndexForScan 的赋值和 issue 54596）。

	// Cache of the allocBits at freeindex. allocCache is shifted
	// such that the lowest bit corresponds to the bit freeindex.
	// allocCache holds the complement of allocBits, thus allowing
	// ctz (count trailing zero) to use it directly.
	// allocCache may contain bits beyond s.nelems; the caller must ignore
	// these.
	allocCache uint64
	// freeindex 处 allocBits 的缓存。allocCache 被移位，
	// 使得最低位对应于 freeindex 位。
	// allocCache 保存 allocBits 的补码，因此允许 ctz
	// （计算尾随零）直接使用它。
	// allocCache 可能包含超出 s.nelems 的位；调用者必须忽略这些位。

	// allocBits and gcmarkBits hold pointers to a span's mark and
	// allocation bits. The pointers are 8 byte aligned.
	// There are three arenas where this data is held.
	// free: Dirty arenas that are no longer accessed
	//       and can be reused.
	// next: Holds information to be used in the next GC cycle.
	// current: Information being used during this GC cycle.
	// previous: Information being used during the last GC cycle.
	// A new GC cycle starts with the call to finishsweep_m.
	// finishsweep_m moves the previous arena to the free arena,
	// the current arena to the previous arena, and
	// the next arena to the current arena.
	// The next arena is populated as the spans request
	// memory to hold gcmarkBits for the next GC cycle as well
	// as allocBits for newly allocated spans.
	//
	// The pointer arithmetic is done "by hand" instead of using
	// arrays to avoid bounds checks along critical performance
	// paths.
	// The sweep will free the old allocBits and set allocBits to the
	// gcmarkBits. The gcmarkBits are replaced with a fresh zeroed
	// out memory.
	allocBits  *gcBits
	gcmarkBits *gcBits
	pinnerBits *gcBits // bitmap for pinned objects; accessed atomically
	// allocBits 和 gcmarkBits 保存指向 span 的标记和分配位的指针。
	// 这些指针是 8 字节对齐的。
	// 这些数据保存在三个区域中：
	// free: 不再访问且可以重用的脏区域
	// next: 保存将在下一个 GC 周期使用的信息
	// current: 当前 GC 周期中使用的信息
	// previous: 上一个 GC 周期中使用的信息
	// 新的 GC 周期从调用 finishsweep_m 开始。
	// finishsweep_m 将 previous 区域移动到 free 区域，
	// current 区域移动到 previous 区域，
	// next 区域移动到 current 区域。
	// 当 spans 请求内存来保存下一个 GC 周期的 gcmarkBits
	// 以及新分配 spans 的 allocBits 时，填充 next 区域。
	//
	// 使用"手动"指针算术而不是数组，以避免在关键性能路径上
	// 进行边界检查。
	// 清扫将释放旧的 allocBits 并将 allocBits 设置为 gcmarkBits。
	// gcmarkBits 被替换为新的清零内存。
	// 固定对象的位图；原子访问

	// sweep generation:
	// if sweepgen == h->sweepgen - 2, the span needs sweeping
	// if sweepgen == h->sweepgen - 1, the span is currently being swept
	// if sweepgen == h->sweepgen, the span is swept and ready to use
	// if sweepgen == h->sweepgen + 1, the span was cached before sweep began and is still cached, and needs sweeping
	// if sweepgen == h->sweepgen + 3, the span was swept and then cached and is still cached
	// h->sweepgen is incremented by 2 after every GC
	sweepgen uint32
	// 清扫代：
	// 如果 sweepgen == h->sweepgen - 2，span 需要清扫
	// 如果 sweepgen == h->sweepgen - 1，span 正在被清扫
	// 如果 sweepgen == h->sweepgen，span 已清扫并可以使用
	// 如果 sweepgen == h->sweepgen + 1，span 在清扫开始前被缓存且仍被缓存，需要清扫
	// 如果 sweepgen == h->sweepgen + 3，span 被清扫后缓存且仍被缓存
	// h->sweepgen 在每次 GC 后增加 2

	divMul uint32 // for divide by elemsize
	// 用于除以 elemsize 的乘数，用于快速除法运算
	allocCount uint16 // number of allocated objects
	// 已分配对象的数量
	spanclass spanClass // size class and noscan (uint8)
	// 大小类和是否不需要扫描标记（uint8）
	state mSpanStateBox // mSpanInUse etc; accessed atomically (get/set methods)
	// span 的状态（如 mSpanInUse 等）；通过原子操作访问（get/set 方法）
	needzero uint8 // needs to be zeroed before allocation
	// 在分配前是否需要清零
	isUserArenaChunk bool // whether or not this span represents a user arena
	// 该 span 是否表示用户 arena
	allocCountBeforeCache uint16 // a copy of allocCount that is stored just before this span is cached
	// 在 span 被缓存前存储的 allocCount 副本
	elemsize uintptr // computed from sizeclass or from npages
	// 从 sizeclass 或 npages 计算得到的元素大小
	limit uintptr // end of data in span
	// span 中数据的结束位置
	speciallock mutex // guards specials list and changes to pinnerBits
	// 保护 specials 列表和 pinnerBits 的修改
	specials *special // linked list of special records sorted by offset.
	// 按偏移量排序的特殊记录链表
	userArenaChunkFree addrRange // interval for managing chunk allocation
	// 用于管理 chunk 分配的范围
	largeType *_type // malloc header for large objects.
	// 大对象的 malloc 头部
}

// base returns the base address of the span.
// base 返回 span 的基地址
func (s *mspan) base() uintptr {
	return s.startAddr
}

// layout returns the size of the span, the number of elements it can hold,
// and the total size in bytes.
// layout 返回 span 的大小、可以容纳的元素数量以及总字节数
func (s *mspan) layout() (size, n, total uintptr) {
	// Calculate total size in bytes by multiplying number of pages by page size
	// 通过将页数乘以页大小来计算总字节数
	total = s.npages << _PageShift
	// Get the size of each element
	// 获取每个元素的大小
	size = s.elemsize
	// If element size is greater than 0, calculate number of elements
	// 如果元素大小大于0，计算可以容纳的元素数量
	if size > 0 {
		n = total / size
	}
	return
}

// recordspan adds a newly allocated span to h.allspans.
//
// This only happens the first time a span is allocated from
// mheap.spanalloc (it is not called when a span is reused).
//
// Write barriers are disallowed here because it can be called from
// gcWork when allocating new workbufs. However, because it's an
// indirect call from the fixalloc initializer, the compiler can't see
// this.
//
// The heap lock must be held.
//
// recordspan 将新分配的 span 添加到 h.allspans 中。
//
// 这只在第一次从 mheap.spanalloc 分配 span 时发生
// （当 span 被重用时不会调用此函数）。
//
// 这里禁止写屏障，因为它可能在分配新的 workbufs 时从 gcWork 调用。
// 然而，由于它是从 fixalloc 初始化器的间接调用，
// 编译器无法看到这一点。
//
// 必须持有堆锁。
//
//go:nowritebarrierrec
func recordspan(vh unsafe.Pointer, p unsafe.Pointer) {
	h := (*mheap)(vh)
	s := (*mspan)(p)

	assertLockHeld(&h.lock)

	// 检查是否需要扩容 allspans 切片
	if len(h.allspans) >= cap(h.allspans) {
		// 计算新的容量，至少为 64KB/指针大小
		n := 64 * 1024 / goarch.PtrSize
		// 如果当前容量的 1.5 倍大于 n，则使用当前容量的 1.5 倍
		if n < cap(h.allspans)*3/2 {
			n = cap(h.allspans) * 3 / 2
		}
		// 创建新的切片
		var new []*mspan
		sp := (*slice)(unsafe.Pointer(&new))
		// 分配新的内存空间
		sp.array = sysAlloc(uintptr(n)*goarch.PtrSize, &memstats.other_sys)
		if sp.array == nil {
			throw("runtime: cannot allocate memory")
		}
		// 设置新切片的长度和容量
		sp.len = len(h.allspans)
		sp.cap = n
		// 如果原切片不为空，则复制数据
		if len(h.allspans) > 0 {
			copy(new, h.allspans)
		}
		// 保存旧切片引用
		oldAllspans := h.allspans
		// 将新切片赋值给 h.allspans
		*(*notInHeapSlice)(unsafe.Pointer(&h.allspans)) = *(*notInHeapSlice)(unsafe.Pointer(&new))
		// 释放旧切片的内存
		if len(oldAllspans) != 0 {
			sysFree(unsafe.Pointer(&oldAllspans[0]), uintptr(cap(oldAllspans))*unsafe.Sizeof(oldAllspans[0]), &memstats.other_sys)
		}
	}
	// 扩展切片长度并添加新的 span
	h.allspans = h.allspans[:len(h.allspans)+1]
	h.allspans[len(h.allspans)-1] = s
}

// A spanClass represents the size class and noscan-ness of a span.
//
// Each size class has a noscan spanClass and a scan spanClass. The
// noscan spanClass contains only noscan objects, which do not contain
// pointers and thus do not need to be scanned by the garbage
// collector.
// spanClass 表示 span 的大小类和是否需要扫描。
//
// 每个大小类都有一个不需要扫描的 spanClass 和一个需要扫描的 spanClass。
// 不需要扫描的 spanClass 只包含不需要扫描的对象，这些对象不包含指针，
// 因此不需要被垃圾收集器扫描。
type spanClass uint8

const (
	// 总 spanClass 数量是大小类数量的两倍，因为每个大小类都有
	// 需要扫描和不需要扫描两种类型
	numSpanClasses = _NumSizeClasses << 1
	// tinySpanClass 是用于小对象分配的 spanClass，
	// 它被标记为不需要扫描（通过 | 1 操作）
	tinySpanClass = spanClass(tinySizeClass<<1 | 1)
)

// makeSpanClass 根据大小类和是否需要扫描创建 spanClass
// sizeclass: 大小类
// noscan: 是否不需要扫描
func makeSpanClass(sizeclass uint8, noscan bool) spanClass {
	// 将大小类左移 1 位，为 noscan 标志位留出空间
	// 如果不需要扫描，则将最低位置 1
	return spanClass(sizeclass<<1) | spanClass(bool2int(noscan))
}

// sizeclass 返回 spanClass 对应的大小类
// 通过右移 1 位去除 noscan 标志位
//
//go:nosplit
func (sc spanClass) sizeclass() int8 {
	return int8(sc >> 1)
}

// noscan 返回 spanClass 是否不需要扫描
// 通过检查最低位是否为 1 来判断
//
//go:nosplit
func (sc spanClass) noscan() bool {
	return sc&1 != 0
}

// arenaIndex returns the index into mheap_.arenas of the arena
// containing metadata for p. This index combines of an index into the
// L1 map and an index into the L2 map and should be used as
// mheap_.arenas[ai.l1()][ai.l2()].
//
// If p is outside the range of valid heap addresses, either l1() or
// l2() will be out of bounds.
//
// It is nosplit because it's called by spanOf and several other
// nosplit functions.
//
// arenaIndex 返回包含 p 的 arena 在 mheap_.arenas 中的索引。
// 这个索引结合了 L1 映射和 L2 映射的索引，应该用作
// mheap_.arenas[ai.l1()][ai.l2()]。
//
// 如果 p 超出了有效堆地址的范围，l1() 或 l2() 将越界。
//
// 使用 nosplit 是因为它被 spanOf 和其他几个 nosplit 函数调用。
//
//go:nosplit
func arenaIndex(p uintptr) arenaIdx {
	return arenaIdx((p - arenaBaseOffset) / heapArenaBytes)
}

// arenaBase returns the low address of the region covered by heap
// arena i.
//
// arenaBase 返回堆 arena i 覆盖区域的低地址。
func arenaBase(i arenaIdx) uintptr {
	return uintptr(i)*heapArenaBytes + arenaBaseOffset
}

// arenaIdx is a type that represents an index into the heap arenas.
// It is used to efficiently locate metadata for a given heap address.
//
// arenaIdx 是一个表示堆 arena 索引的类型。
// 它用于高效地定位给定堆地址的元数据。
type arenaIdx uint

// l1 returns the "l1" portion of an arenaIdx.
//
// Marked nosplit because it's called by spanOf and other nosplit
// functions.
//
// l1 返回 arenaIdx 的 "l1" 部分。
// 标记为 nosplit 是因为它被 spanOf 和其他 nosplit 函数调用。
//
//go:nosplit
func (i arenaIdx) l1() uint {
	if arenaL1Bits == 0 {
		// Let the compiler optimize this away if there's no
		// L1 map.
		// 如果没有 L1 映射，让编译器优化掉这个分支
		return 0
	} else {
		// 通过右移 arenaL1Shift 位来获取 L1 索引
		return uint(i) >> arenaL1Shift
	}
}

// l2 returns the "l2" portion of an arenaIdx.
//
// Marked nosplit because it's called by spanOf and other nosplit funcs.
// functions.
//
// l2 返回 arenaIdx 的 "l2" 部分。
// 标记为 nosplit 是因为它被 spanOf 和其他 nosplit 函数调用。
//
//go:nosplit
func (i arenaIdx) l2() uint {
	if arenaL1Bits == 0 {
		// 如果没有 L1 映射，直接返回整个索引作为 L2 索引
		return uint(i)
	} else {
		// 使用掩码 (1<<arenaL2Bits - 1) 来获取 L2 索引
		return uint(i) & (1<<arenaL2Bits - 1)
	}
}

// inheap reports whether b is a pointer into a (potentially dead) heap object.
// It returns false for pointers into mSpanManual spans.
// Non-preemptible because it is used by write barriers.
//
// inheap 报告 b 是否是指向（可能已死亡的）堆对象的指针。
// 对于指向 mSpanManual span 的指针返回 false。
// 不可抢占是因为它被写屏障使用。
//
//go:nowritebarrier
//go:nosplit
func inheap(b uintptr) bool {
	// 通过检查 spanOfHeap 是否返回非 nil 来判断指针是否在堆中
	return spanOfHeap(b) != nil
}

// inHeapOrStack is a variant of inheap that returns true for pointers
// into any allocated heap span.
//
// inHeapOrStack 是 inheap 的一个变体，对于指向任何已分配堆 span 的指针返回 true。
// 它检查指针是否在堆或栈中，包括手动管理的 span。
//
//go:nowritebarrier
//go:nosplit
func inHeapOrStack(b uintptr) bool {
	// Get the span containing the pointer
	// 获取包含该指针的 span
	s := spanOf(b)
	// If the span doesn't exist or the pointer is before the span's base address,
	// it's not in the heap or stack
	// 如果 span 不存在或指针在 span 的基地址之前，说明不在堆或栈中
	if s == nil || b < s.base() {
		return false
	}
	// Check the span's state
	// 检查 span 的状态
	switch s.state.get() {
	case mSpanInUse, mSpanManual:
		// If the span is in use or manually managed, check if the pointer
		// is within the span's bounds
		// 如果 span 正在使用或手动管理，检查指针是否在 span 的边界内
		return b < s.limit
	default:
		// For any other state, the pointer is not in the heap or stack
		// 对于其他任何状态，指针都不在堆或栈中
		return false
	}
}

// spanOf returns the span of p. If p does not point into the heap
// arena or no span has ever contained p, spanOf returns nil.
//
// If p does not point to allocated memory, this may return a non-nil
// span that does *not* contain p. If this is a possibility, the
// caller should either call spanOfHeap or check the span bounds
// explicitly.
//
// Must be nosplit because it has callers that are nosplit.
//
// spanOf 返回 p 所在的 span。如果 p 不指向堆 arena 或者没有 span 包含 p，
// spanOf 返回 nil。
//
// 如果 p 不指向已分配的内存，这个函数可能返回一个非 nil 但不包含 p 的 span。
// 如果存在这种可能性，调用者应该要么调用 spanOfHeap，要么显式检查 span 的边界。
//
// 必须使用 nosplit 因为它的调用者都是 nosplit 的。
//
//go:nosplit
func spanOf(p uintptr) *mspan {
	// This function looks big, but we use a lot of constant
	// folding around arenaL1Bits to get it under the inlining
	// budget. Also, many of the checks here are safety checks
	// that Go needs to do anyway, so the generated code is quite
	// short.
	//
	// 这个函数看起来很大，但我们使用了很多围绕 arenaL1Bits 的常量折叠
	// 来使其符合内联预算。此外，这里的许多检查是 Go 无论如何都需要做的
	// 安全检查，所以生成的代码实际上相当短。
	ri := arenaIndex(p)
	if arenaL1Bits == 0 {
		// If there's no L1, then ri.l1() can't be out of bounds but ri.l2() can.
		// 如果没有 L1，那么 ri.l1() 不会越界，但 ri.l2() 可能会越界
		if ri.l2() >= uint(len(mheap_.arenas[0])) {
			return nil
		}
	} else {
		// If there's an L1, then ri.l1() can be out of bounds but ri.l2() can't.
		// 如果有 L1，那么 ri.l1() 可能会越界，但 ri.l2() 不会越界
		if ri.l1() >= uint(len(mheap_.arenas)) {
			return nil
		}
	}
	l2 := mheap_.arenas[ri.l1()]
	if arenaL1Bits != 0 && l2 == nil { // Should never happen if there's no L1.
		return nil
	}
	ha := l2[ri.l2()]
	if ha == nil {
		return nil
	}
	return ha.spans[(p/pageSize)%pagesPerArena]
}

// spanOfUnchecked is equivalent to spanOf, but the caller must ensure
// that p points into an allocated heap arena.
//
// Must be nosplit because it has callers that are nosplit.
//
// spanOfUnchecked 等同于 spanOf，但调用者必须确保 p 指向已分配的堆 arena。
//
// 必须使用 nosplit 因为它的调用者都是 nosplit 的。
//
//go:nosplit
func spanOfUnchecked(p uintptr) *mspan {
	// 获取 p 的 arena 索引
	ai := arenaIndex(p)
	// 直接访问 arena 数组获取对应的 span
	// 这里假设 p 一定指向有效的堆内存，所以不需要进行边界检查
	return mheap_.arenas[ai.l1()][ai.l2()].spans[(p/pageSize)%pagesPerArena]
}

// spanOfHeap is like spanOf, but returns nil if p does not point to a
// heap object.
//
// Must be nosplit because it has callers that are nosplit.
//
// spanOfHeap 类似于 spanOf，但如果 p 不指向堆对象则返回 nil。
//
// 必须使用 nosplit 因为它的调用者都是 nosplit 的。
//
//go:nosplit
func spanOfHeap(p uintptr) *mspan {
	s := spanOf(p)
	// s is nil if it's never been allocated. Otherwise, we check
	// its state first because we don't trust this pointer, so we
	// have to synchronize with span initialization. Then, it's
	// still possible we picked up a stale span pointer, so we
	// have to check the span's bounds.
	//
	// 如果 s 从未被分配过，则为 nil。否则，我们首先检查其状态，
	// 因为我们不信任这个指针，所以必须与 span 初始化同步。
	// 然后，我们可能获取了一个过时的 span 指针，所以必须检查
	// span 的边界。
	if s == nil || s.state.get() != mSpanInUse || p < s.base() || p >= s.limit {
		return nil
	}
	return s
}

// pageIndexOf returns the arena, page index, and page mask for pointer p.
// The caller must ensure p is in the heap.
// pageIndexOf 返回指针 p 对应的 arena、页面索引和页面掩码。
// 调用者必须确保 p 在堆中。
func pageIndexOf(p uintptr) (arena *heapArena, pageIdx uintptr, pageMask uint8) {
	// 获取 p 的 arena 索引
	ai := arenaIndex(p)
	// 通过 arena 索引获取对应的 arena
	arena = mheap_.arenas[ai.l1()][ai.l2()]
	// 计算页面在位图中的索引：
	// 1. p/pageSize 得到页面号
	// 2. 除以 8 得到在 pageInUse 数组中的索引
	// 3. 对 pageInUse 长度取模确保索引在有效范围内
	pageIdx = ((p / pageSize) / 8) % uintptr(len(arena.pageInUse))
	// 计算页面在位图中的掩码：
	// 1. p/pageSize 得到页面号
	// 2. 对 8 取模得到在位图中的位置
	// 3. 左移 1 得到对应的位掩码
	pageMask = byte(1 << ((p / pageSize) % 8))
	return
}

// Initialize the heap.
// 初始化堆
func (h *mheap) init() {
	// 初始化堆锁和特殊锁
	lockInit(&h.lock, lockRankMheap)
	lockInit(&h.speciallock, lockRankMheapSpecial)

	// 初始化各种分配器
	// 1. span 分配器，用于分配 mspan 对象
	h.spanalloc.init(unsafe.Sizeof(mspan{}), recordspan, unsafe.Pointer(h), &memstats.mspan_sys)
	// 2. cache 分配器，用于分配 mcache 对象
	h.cachealloc.init(unsafe.Sizeof(mcache{}), nil, nil, &memstats.mcache_sys)
	// 3. 特殊对象分配器，用于分配各种特殊对象
	h.specialfinalizeralloc.init(unsafe.Sizeof(specialfinalizer{}), nil, nil, &memstats.other_sys)
	h.specialprofilealloc.init(unsafe.Sizeof(specialprofile{}), nil, nil, &memstats.other_sys)
	h.specialReachableAlloc.init(unsafe.Sizeof(specialReachable{}), nil, nil, &memstats.other_sys)
	h.specialPinCounterAlloc.init(unsafe.Sizeof(specialPinCounter{}), nil, nil, &memstats.other_sys)
	h.specialWeakHandleAlloc.init(unsafe.Sizeof(specialWeakHandle{}), nil, nil, &memstats.gcMiscSys)
	// 4. arena 提示分配器，用于分配 arena 提示对象
	h.arenaHintAlloc.init(unsafe.Sizeof(arenaHint{}), nil, nil, &memstats.other_sys)

	// Don't zero mspan allocations. Background sweeping can
	// inspect a span concurrently with allocating it, so it's
	// important that the span's sweepgen survive across freeing
	// and re-allocating a span to prevent background sweeping
	// from improperly cas'ing it from 0.
	//
	// This is safe because mspan contains no heap pointers.
	// 不要清零 mspan 分配。后台清扫可以在分配 span 的同时检查它，
	// 所以重要的是 span 的 sweepgen 在释放和重新分配 span 时保持存活，
	// 以防止后台清扫错误地将其从 0 开始 cas。
	//
	// 这是安全的，因为 mspan 不包含堆指针。
	h.spanalloc.zero = false

	// h->mapcache needs no init
	// h->mapcache 不需要初始化

	// 初始化所有中心缓存
	for i := range h.central {
		h.central[i].mcentral.init(spanClass(i))
	}

	// 初始化页面分配器
	h.pages.init(&h.lock, &memstats.gcMiscSys, false)
}

// reclaim sweeps and reclaims at least npage pages into the heap.
// It is called before allocating npage pages to keep growth in check.
//
// reclaim implements the page-reclaimer half of the sweeper.
//
// h.lock must NOT be held.
// reclaim 清扫并回收至少 npage 页到堆中。
// 它在分配 npage 页之前被调用，以控制增长。
//
// reclaim 实现了清扫器的页面回收部分。
//
// 调用时不能持有 h.lock。
func (h *mheap) reclaim(npage uintptr) {
	// TODO(austin): Half of the time spent freeing spans is in
	// locking/unlocking the heap (even with low contention). We
	// could make the slow path here several times faster by
	// batching heap frees.
	// TODO(austin): 释放 spans 的时间有一半花在锁定/解锁堆上
	// （即使在低竞争情况下）。我们可以通过批量释放堆来使这里的
	// 慢路径快几倍。

	// Bail early if there's no more reclaim work.
	// 如果没有更多回收工作，提前返回。
	if h.reclaimIndex.Load() >= 1<<63 {
		return
	}

	// Disable preemption so the GC can't start while we're
	// sweeping, so we can read h.sweepArenas, and so
	// traceGCSweepStart/Done pair on the P.
	// 禁用抢占，这样在我们清扫时 GC 不能启动，
	// 这样我们可以读取 h.sweepArenas，并且
	// 在 P 上配对 traceGCSweepStart/Done。
	mp := acquirem()

	trace := traceAcquire()
	if trace.ok() {
		trace.GCSweepStart()
		traceRelease(trace)
	}

	arenas := h.sweepArenas
	locked := false
	for npage > 0 {
		// Pull from accumulated credit first.
		// 首先从累积的信用中提取。
		if credit := h.reclaimCredit.Load(); credit > 0 {
			take := credit
			if take > npage {
				// Take only what we need.
				// 只取我们需要的数量。
				take = npage
			}
			if h.reclaimCredit.CompareAndSwap(credit, credit-take) {
				npage -= take
			}
			continue
		}

		// Claim a chunk of work.
		// 认领一块工作。
		idx := uintptr(h.reclaimIndex.Add(pagesPerReclaimerChunk) - pagesPerReclaimerChunk)
		if idx/pagesPerArena >= uintptr(len(arenas)) {
			// Page reclaiming is done.
			// 页面回收完成。
			h.reclaimIndex.Store(1 << 63)
			break
		}

		if !locked {
			// Lock the heap for reclaimChunk.
			// 为 reclaimChunk 锁定堆。
			lock(&h.lock)
			locked = true
		}

		// Scan this chunk.
		// 扫描这个块。
		nfound := h.reclaimChunk(arenas, idx, pagesPerReclaimerChunk)
		if nfound <= npage {
			npage -= nfound
		} else {
			// Put spare pages toward global credit.
			// 将多余的页面计入全局信用。
			h.reclaimCredit.Add(nfound - npage)
			npage = 0
		}
	}
	if locked {
		unlock(&h.lock)
	}

	trace = traceAcquire()
	if trace.ok() {
		trace.GCSweepDone()
		traceRelease(trace)
	}
	releasem(mp)
}

// reclaimChunk sweeps unmarked spans that start at page indexes [pageIdx, pageIdx+n).
// It returns the number of pages returned to the heap.
//
// h.lock must be held and the caller must be non-preemptible. Note: h.lock may be
// temporarily unlocked and re-locked in order to do sweeping or if tracing is
// enabled.
// reclaimChunk 扫描从页面索引 [pageIdx, pageIdx+n) 开始的未标记的 spans。
// 它返回返回到堆的页面数量。
//
// 必须持有 h.lock 且调用者必须是非抢占式的。注意：h.lock 可能会
// 临时解锁并重新锁定，以便进行清扫或启用跟踪。
func (h *mheap) reclaimChunk(arenas []arenaIdx, pageIdx, n uintptr) uintptr {
	// The heap lock must be held because this accesses the
	// heapArena.spans arrays using potentially non-live pointers.
	// In particular, if a span were freed and merged concurrently
	// with this probing heapArena.spans, it would be possible to
	// observe arbitrary, stale span pointers.
	// 必须持有堆锁，因为这会使用可能非存活的指针访问 heapArena.spans 数组。
	// 特别是，如果 span 被释放并与探测 heapArena.spans 并发合并，
	// 可能会观察到任意的、过时的 span 指针。
	assertLockHeld(&h.lock)

	n0 := n
	var nFreed uintptr
	sl := sweep.active.begin()
	if !sl.valid {
		return 0
	}
	for n > 0 {
		ai := arenas[pageIdx/pagesPerArena]
		ha := h.arenas[ai.l1()][ai.l2()]

		// Get a chunk of the bitmap to work on.
		// 获取要处理的位图块
		arenaPage := uint(pageIdx % pagesPerArena)
		inUse := ha.pageInUse[arenaPage/8:]
		marked := ha.pageMarks[arenaPage/8:]
		if uintptr(len(inUse)) > n/8 {
			inUse = inUse[:n/8]
			marked = marked[:n/8]
		}

		// Scan this bitmap chunk for spans that are in-use
		// but have no marked objects on them.
		// 扫描这个位图块，查找正在使用但上面没有标记对象的 spans
		for i := range inUse {
			inUseUnmarked := atomic.Load8(&inUse[i]) &^ marked[i]
			if inUseUnmarked == 0 {
				continue
			}

			for j := uint(0); j < 8; j++ {
				if inUseUnmarked&(1<<j) != 0 {
					s := ha.spans[arenaPage+uint(i)*8+j]
					if s, ok := sl.tryAcquire(s); ok {
						npages := s.npages
						unlock(&h.lock)
						if s.sweep(false) {
							nFreed += npages
						}
						lock(&h.lock)
						// Reload inUse. It's possible nearby
						// spans were freed when we dropped the
						// lock and we don't want to get stale
						// pointers from the spans array.
						// 重新加载 inUse。当我们释放锁时，附近的 spans
						// 可能已被释放，我们不想从 spans 数组获取过时的指针。
						inUseUnmarked = atomic.Load8(&inUse[i]) &^ marked[i]
					}
				}
			}
		}

		// Advance.
		// 前进
		pageIdx += uintptr(len(inUse) * 8)
		n -= uintptr(len(inUse) * 8)
	}
	sweep.active.end(sl)
	trace := traceAcquire()
	if trace.ok() {
		unlock(&h.lock)
		// Account for pages scanned but not reclaimed.
		// 计算已扫描但未回收的页面
		trace.GCSweepSpan((n0 - nFreed) * pageSize)
		traceRelease(trace)
		lock(&h.lock)
	}

	assertLockHeld(&h.lock) // Must be locked on return.
	// 返回时必须持有锁
	return nFreed
}

// spanAllocType represents the type of allocation to make, or
// the type of allocation to be freed.
type spanAllocType uint8

const (
	spanAllocHeap          spanAllocType = iota // heap span
	spanAllocStack                              // stack span
	spanAllocPtrScalarBits                      // unrolled GC prog bitmap span
	spanAllocWorkBuf                            // work buf span
)

// manual returns true if the span allocation is manually managed.
func (s spanAllocType) manual() bool {
	return s != spanAllocHeap
}

// alloc allocates a new span of npage pages from the GC'd heap.
//
// spanclass indicates the span's size class and scannability.
//
// Returns a span that has been fully initialized. span.needzero indicates
// whether the span has been zeroed. Note that it may not be.
//
// alloc 从 GC 堆中分配一个新的包含 npage 页的 span。
//
// spanclass 参数指定了 span 的大小类和可扫描性。
//
// 返回一个完全初始化的 span。span.needzero 指示
// span 是否已被清零。注意，它可能没有被清零。
func (h *mheap) alloc(npages uintptr, spanclass spanClass) *mspan {
	// Don't do any operations that lock the heap on the G stack.
	// It might trigger stack growth, and the stack growth code needs
	// to be able to allocate heap.
	//
	// 不要在 G 栈上执行任何需要锁定堆的操作。
	// 这可能会触发栈增长，而栈增长代码需要能够分配堆内存。
	var s *mspan
	systemstack(func() {
		// To prevent excessive heap growth, before allocating n pages
		// we need to sweep and reclaim at least n pages.
		//
		// 为了防止堆过度增长，在分配 n 页之前，
		// 我们需要清扫并回收至少 n 页。
		if !isSweepDone() {
			h.reclaim(npages)
		}
		s = h.allocSpan(npages, spanAllocHeap, spanclass)
	})
	return s
}

// allocManual allocates a manually-managed span of npage pages.
// allocManual returns nil if allocation fails.
//
// allocManual adds the bytes used to *stat, which should be a
// memstats in-use field. Unlike allocations in the GC'd heap, the
// allocation does *not* count toward heapInUse.
//
// The memory backing the returned span may not be zeroed if
// span.needzero is set.
//
// allocManual must be called on the system stack because it may
// acquire the heap lock via allocSpan. See mheap for details.
//
// If new code is written to call allocManual, do NOT use an
// existing spanAllocType value and instead declare a new one.
//
// allocManual 分配一个手动管理的 npage 页的 span。
// 如果分配失败，allocManual 返回 nil。
//
// allocManual 将使用的字节数添加到 *stat，这应该是一个
// memstats 的使用中字段。与 GC 堆中的分配不同，
// 此分配*不*计入 heapInUse。
//
// 如果设置了 span.needzero，返回的 span 的内存可能未被清零。
//
// allocManual 必须在系统栈上调用，因为它可能通过 allocSpan
// 获取堆锁。有关详细信息，请参阅 mheap。
//
// 如果编写新代码来调用 allocManual，请*不要*使用现有的
// spanAllocType 值，而是声明一个新的值。
//
//go:systemstack
func (h *mheap) allocManual(npages uintptr, typ spanAllocType) *mspan {
	if !typ.manual() {
		throw("manual span allocation called with non-manually-managed type")
	}
	return h.allocSpan(npages, typ, 0)
}

// setSpans modifies the span map so [spanOf(base), spanOf(base+npage*pageSize))
// is s.
// setSpans 修改 span 映射，使得 [spanOf(base), spanOf(base+npage*pageSize)) 范围内的
// 所有页面都指向 span s。
func (h *mheap) setSpans(base, npage uintptr, s *mspan) {
	// 计算起始页面索引
	p := base / pageSize
	// 获取起始地址对应的 arena 索引
	ai := arenaIndex(base)
	// 获取对应的 heapArena
	ha := h.arenas[ai.l1()][ai.l2()]
	// 遍历所有需要设置的页面
	for n := uintptr(0); n < npage; n++ {
		// 计算当前页面在 arena 中的索引
		i := (p + n) % pagesPerArena
		// 如果跨越了 arena 边界，需要更新 arena 信息
		if i == 0 {
			ai = arenaIndex(base + n*pageSize)
			ha = h.arenas[ai.l1()][ai.l2()]
		}
		// 设置当前页面对应的 span
		ha.spans[i] = s
	}
}

// allocNeedsZero checks if the region of address space [base, base+npage*pageSize),
// assumed to be allocated, needs to be zeroed, updating heap arena metadata for
// future allocations.
//
// This must be called each time pages are allocated from the heap, even if the page
// allocator can otherwise prove the memory it's allocating is already zero because
// they're fresh from the operating system. It updates heapArena metadata that is
// critical for future page allocations.
//
// There are no locking constraints on this method.
//
// allocNeedsZero 检查地址空间区域 [base, base+npage*pageSize) 是否需要清零，
// 并更新堆 arena 元数据以供未来分配使用。
//
// 每次从堆分配页面时都必须调用此函数，即使页面分配器可以证明它正在分配的内存
// 已经为零（因为它们刚从操作系统获得）。它更新对未来的页面分配至关重要的
// heapArena 元数据。
//
// 此方法没有锁定约束。
func (h *mheap) allocNeedsZero(base, npage uintptr) (needZero bool) {
	for npage > 0 {
		// 获取当前基地址对应的 arena 索引和 arena 对象
		ai := arenaIndex(base)
		ha := h.arenas[ai.l1()][ai.l2()]

		// 获取当前 arena 的已清零基地址
		zeroedBase := atomic.Loaduintptr(&ha.zeroedBase)
		// 计算当前基地址在 arena 内的偏移
		arenaBase := base % heapArenaBytes
		if arenaBase < zeroedBase {
			// We extended into the non-zeroed part of the
			// arena, so this region needs to be zeroed before use.
			//
			// zeroedBase is monotonically increasing, so if we see this now then
			// we can be sure we need to zero this memory region.
			//
			// We still need to update zeroedBase for this arena, and
			// potentially more arenas.
			//
			// 我们扩展到了 arena 的未清零部分，所以这个区域在使用前需要清零。
			//
			// zeroedBase 是单调递增的，所以如果我们现在看到这个情况，
			// 我们可以确定需要清零这个内存区域。
			//
			// 我们仍然需要更新这个 arena 的 zeroedBase，可能还需要更新更多 arena。
			needZero = true
		}
		// We may observe arenaBase > zeroedBase if we're racing with one or more
		// allocations which are acquiring memory directly before us in the address
		// space. But, because we know no one else is acquiring *this* memory, it's
		// still safe to not zero.
		//
		// 如果我们在地址空间中与一个或多个分配竞争，这些分配直接在我们之前获取内存，
		// 我们可能会观察到 arenaBase > zeroedBase。但是，因为我们知道没有其他人
		// 在获取*这个*内存，所以不进行清零仍然是安全的。

		// Compute how far into the arena we extend into, capped
		// at heapArenaBytes.
		//
		// 计算我们在 arena 中扩展的距离，上限为 heapArenaBytes。
		arenaLimit := arenaBase + npage*pageSize
		if arenaLimit > heapArenaBytes {
			arenaLimit = heapArenaBytes
		}
		// Increase ha.zeroedBase so it's >= arenaLimit.
		// We may be racing with other updates.
		//
		// 增加 ha.zeroedBase 使其 >= arenaLimit。
		// 我们可能正在与其他更新竞争。
		for arenaLimit > zeroedBase {
			if atomic.Casuintptr(&ha.zeroedBase, zeroedBase, arenaLimit) {
				break
			}
			zeroedBase = atomic.Loaduintptr(&ha.zeroedBase)
			// Double check basic conditions of zeroedBase.
			//
			// 再次检查 zeroedBase 的基本条件。
			if zeroedBase <= arenaLimit && zeroedBase > arenaBase {
				// The zeroedBase moved into the space we were trying to
				// claim. That's very bad, and indicates someone allocated
				// the same region we did.
				//
				// zeroedBase 移动到了我们试图声明的空间中。这非常糟糕，
				// 表明有人分配了我们正在分配的相同区域。
				throw("potentially overlapping in-use allocations detected")
			}
		}

		// Move base forward and subtract from npage to move into
		// the next arena, or finish.
		//
		// 向前移动 base 并从 npage 中减去以移动到下一个 arena，或完成。
		base += arenaLimit - arenaBase
		npage -= (arenaLimit - arenaBase) / pageSize
	}
	return
}

// tryAllocMSpan attempts to allocate an mspan object from
// the P-local cache, but may fail.
//
// h.lock need not be held.
//
// This caller must ensure that its P won't change underneath
// it during this function. Currently to ensure that we enforce
// that the function is run on the system stack, because that's
// the only place it is used now. In the future, this requirement
// may be relaxed if its use is necessary elsewhere.
//
// tryAllocMSpan 尝试从 P 本地缓存中分配一个 mspan 对象，但可能会失败。
//
// 不需要持有 h.lock。
//
// 调用者必须确保在函数执行期间其 P 不会改变。目前为了确保这一点，
// 我们强制该函数在系统栈上运行，因为这是它现在唯一使用的地方。
// 将来，如果需要在其他地方使用，这个要求可能会放宽。
//
//go:systemstack
func (h *mheap) tryAllocMSpan() *mspan {
	pp := getg().m.p.ptr()
	// If we don't have a p or the cache is empty, we can't do
	// anything here.
	// 如果我们没有 p 或者缓存为空，我们在这里什么也做不了。
	if pp == nil || pp.mspancache.len == 0 {
		return nil
	}
	// Pull off the last entry in the cache.
	// 从缓存中取出最后一个条目。
	s := pp.mspancache.buf[pp.mspancache.len-1]
	pp.mspancache.len--
	return s
}

// allocMSpanLocked allocates an mspan object.
//
// h.lock must be held.
//
// allocMSpanLocked must be called on the system stack because
// its caller holds the heap lock. See mheap for details.
// Running on the system stack also ensures that we won't
// switch Ps during this function. See tryAllocMSpan for details.
//
// allocMSpanLocked 分配一个 mspan 对象。
//
// 必须持有 h.lock。
//
// allocMSpanLocked 必须在系统栈上调用，因为其调用者持有堆锁。
// 详见 mheap 的说明。在系统栈上运行也确保了我们不会在函数执行期间切换 P。
// 详见 tryAllocMSpan 的说明。
//
//go:systemstack
func (h *mheap) allocMSpanLocked() *mspan {
	assertLockHeld(&h.lock)

	pp := getg().m.p.ptr()
	if pp == nil {
		// We don't have a p so just do the normal thing.
		// 如果没有 P，就执行正常的分配操作。
		return (*mspan)(h.spanalloc.alloc())
	}
	// Refill the cache if necessary.
	// 如果需要，重新填充缓存。
	if pp.mspancache.len == 0 {
		const refillCount = len(pp.mspancache.buf) / 2
		for i := 0; i < refillCount; i++ {
			pp.mspancache.buf[i] = (*mspan)(h.spanalloc.alloc())
		}
		pp.mspancache.len = refillCount
	}
	// Pull off the last entry in the cache.
	// 从缓存中取出最后一个条目。
	s := pp.mspancache.buf[pp.mspancache.len-1]
	pp.mspancache.len--
	return s
}

// freeMSpanLocked free an mspan object.
//
// h.lock must be held.
//
// freeMSpanLocked must be called on the system stack because
// its caller holds the heap lock. See mheap for details.
// Running on the system stack also ensures that we won't
// switch Ps during this function. See tryAllocMSpan for details.
//
// freeMSpanLocked 释放一个 mspan 对象。
//
// 必须持有 h.lock。
//
// freeMSpanLocked 必须在系统栈上调用，因为其调用者持有堆锁。
// 详见 mheap 的说明。在系统栈上运行也确保了我们不会在函数执行期间切换 P。
// 详见 tryAllocMSpan 的说明。
//
//go:systemstack
func (h *mheap) freeMSpanLocked(s *mspan) {
	assertLockHeld(&h.lock)

	pp := getg().m.p.ptr()
	// First try to free the mspan directly to the cache.
	// 首先尝试将 mspan 直接释放到缓存中。
	if pp != nil && pp.mspancache.len < len(pp.mspancache.buf) {
		pp.mspancache.buf[pp.mspancache.len] = s
		pp.mspancache.len++
		return
	}
	// Failing that (or if we don't have a p), just free it to
	// the heap.
	// 如果失败（或者我们没有 P），就直接释放到堆中。
	h.spanalloc.free(unsafe.Pointer(s))
}

// allocSpan allocates an mspan which owns npages worth of memory.
//
// If typ.manual() == false, allocSpan allocates a heap span of class spanclass
// and updates heap accounting. If manual == true, allocSpan allocates a
// manually-managed span (spanclass is ignored), and the caller is
// responsible for any accounting related to its use of the span. Either
// way, allocSpan will atomically add the bytes in the newly allocated
// span to *sysStat.
//
// The returned span is fully initialized.
//
// h.lock must not be held.
//
// allocSpan must be called on the system stack both because it acquires
// the heap lock and because it must block GC transitions.
//
// allocSpan 分配一个拥有 npages 页内存的 mspan。
//
// 如果 typ.manual() == false，allocSpan 分配一个 spanclass 类的堆 span
// 并更新堆的统计信息。如果 manual == true，allocSpan 分配一个手动管理的 span
// （spanclass 被忽略），调用者负责与其使用 span 相关的任何统计信息。
// 无论哪种情况，allocSpan 都会原子地将新分配的 span 中的字节数添加到 *sysStat。
//
// 返回的 span 是完全初始化的。
//
// 不能持有 h.lock。
//
// allocSpan 必须在系统栈上调用，因为它需要获取堆锁并且必须阻止 GC 转换。
//
//go:systemstack
func (h *mheap) allocSpan(npages uintptr, typ spanAllocType, spanclass spanClass) (s *mspan) {
	// Function-global state.
	// 函数全局状态
	gp := getg()
	base, scav := uintptr(0), uintptr(0)
	growth := uintptr(0)

	// On some platforms we need to provide physical page aligned stack
	// allocations. Where the page size is less than the physical page
	// size, we already manage to do this by default.
	// 在某些平台上，我们需要提供物理页面对齐的栈分配。
	// 当页面大小小于物理页面大小时，我们默认已经处理了这种情况。
	needPhysPageAlign := physPageAlignedStacks && typ == spanAllocStack && pageSize < physPageSize

	// If the allocation is small enough, try the page cache!
	// The page cache does not support aligned allocations, so we cannot use
	// it if we need to provide a physical page aligned stack allocation.
	// 如果分配的大小足够小，尝试使用页面缓存！
	// 页面缓存不支持对齐分配，所以如果我们需要提供物理页面对齐的栈分配，
	// 就不能使用它。
	pp := gp.m.p.ptr()
	if !needPhysPageAlign && pp != nil && npages < pageCachePages/4 {
		c := &pp.pcache

		// If the cache is empty, refill it.
		// 如果缓存为空，重新填充它。
		if c.empty() {
			lock(&h.lock)
			*c = h.pages.allocToCache()
			unlock(&h.lock)
		}

		// Try to allocate from the cache.
		// 尝试从缓存中分配。
		base, scav = c.alloc(npages)
		if base != 0 {
			s = h.tryAllocMSpan()
			if s != nil {
				goto HaveSpan
			}
			// We have a base but no mspan, so we need
			// to lock the heap.
			// 我们有 base 但没有 mspan，所以需要锁定堆。
		}
	}

	// For one reason or another, we couldn't get the
	// whole job done without the heap lock.
	// 由于某种原因，我们无法在不获取堆锁的情况下
	// 完成整个工作。
	lock(&h.lock)

	if needPhysPageAlign {
		// Overallocate by a physical page to allow for later alignment.
		// 多分配一个物理页面以允许后续对齐
		extraPages := physPageSize / pageSize

		// Find a big enough region first, but then only allocate the
		// aligned portion. We can't just allocate and then free the
		// edges because we need to account for scavenged memory, and
		// that's difficult with alloc.
		//
		// Note that we skip updates to searchAddr here. It's OK if
		// it's stale and higher than normal; it'll operate correctly,
		// just come with a performance cost.
		// 首先找到一个足够大的区域，但只分配对齐的部分。
		// 我们不能先分配然后释放边缘，因为我们需要考虑已回收的内存，
		// 这在 alloc 中很难处理。
		//
		// 注意我们在这里跳过了对 searchAddr 的更新。
		// 如果它过时且高于正常值也没关系；它仍然可以正确运行，
		// 只是会带来性能成本。
		base, _ = h.pages.find(npages + extraPages)
		if base == 0 {
			var ok bool
			// Try to grow the heap to accommodate the requested pages plus extra pages for alignment.
			// 尝试扩展堆以容纳请求的页面加上用于对齐的额外页面
			growth, ok = h.grow(npages + extraPages)
			if !ok {
				// If we couldn't grow the heap, unlock and return nil.
				// 如果无法扩展堆，解锁并返回 nil
				unlock(&h.lock)
				return nil
			}
			// After growing, try to find the requested space again.
			// 扩展后，再次尝试查找请求的空间
			base, _ = h.pages.find(npages + extraPages)
			if base == 0 {
				// This should never happen - we just grew the heap but can't find space.
				// 这不应该发生 - 我们刚刚扩展了堆但找不到空间
				throw("grew heap, but no adequate free space found")
			}
		}
		// 将 base 向上对齐到物理页面大小
		base = alignUp(base, physPageSize)
		// 分配指定范围的页面并返回回收的内存大小
		scav = h.pages.allocRange(base, npages)
	}

	if base == 0 {
		// Try to acquire a base address.
		// 尝试获取一个基地址
		base, scav = h.pages.alloc(npages)
		if base == 0 {
			var ok bool
			// Try to grow the heap to accommodate the requested pages.
			// 尝试扩展堆以容纳请求的页面
			growth, ok = h.grow(npages)
			if !ok {
				// If we couldn't grow the heap, unlock and return nil.
				// 如果无法扩展堆，解锁并返回 nil
				unlock(&h.lock)
				return nil
			}
			// After growing, try to allocate the pages again.
			// 扩展后，再次尝试分配页面
			base, scav = h.pages.alloc(npages)
			if base == 0 {
				// This should never happen - we just grew the heap but can't find space.
				// 这不应该发生 - 我们刚刚扩展了堆但找不到空间
				throw("grew heap, but no adequate free space found")
			}
		}
	}
	if s == nil {
		// We failed to get an mspan earlier, so grab
		// one now that we have the heap lock.
		// 我们之前未能获取 mspan，所以现在在持有堆锁的情况下获取一个
		s = h.allocMSpanLocked()
	}
	unlock(&h.lock)

HaveSpan:
	// Decide if we need to scavenge in response to what we just allocated.
	// Specifically, we track the maximum amount of memory to scavenge of all
	// the alternatives below, assuming that the maximum satisfies *all*
	// conditions we check (e.g. if we need to scavenge X to satisfy the
	// memory limit and Y to satisfy heap-growth scavenging, and Y > X, then
	// it's fine to pick Y, because the memory limit is still satisfied).
	//
	// It's fine to do this after allocating because we expect any scavenged
	// pages not to get touched until we return. Simultaneously, it's important
	// to do this before calling sysUsed because that may commit address space.
	//
	// 决定是否需要根据我们刚刚分配的内容进行内存回收。
	// 具体来说，我们跟踪所有备选方案中需要回收的最大内存量，
	// 假设最大值满足我们检查的*所有*条件（例如，如果我们需要回收 X 来满足
	// 内存限制，回收 Y 来满足堆增长回收，且 Y > X，那么选择 Y 是可以的，
	// 因为内存限制仍然得到满足）。
	//
	// 在分配后执行此操作是可以的，因为我们期望任何被回收的页面在返回之前
	// 不会被触及。同时，在调用 sysUsed 之前执行此操作很重要，因为那可能会
	// 提交地址空间。
	bytesToScavenge := uintptr(0)
	forceScavenge := false
	if limit := gcController.memoryLimit.Load(); !gcCPULimiter.limiting() {
		// Assist with scavenging to maintain the memory limit by the amount
		// that we expect to page in.
		// 通过协助回收来维持内存限制，回收量是我们预期要换入的量
		inuse := gcController.mappedReady.Load()
		// Be careful about overflow, especially with uintptrs. Even on 32-bit platforms
		// someone can set a really big memory limit that isn't maxInt64.
		// 注意溢出，特别是对于 uintptrs。即使在 32 位平台上，
		// 也可能设置一个非常大的内存限制，而不是 maxInt64
		if uint64(scav)+inuse > uint64(limit) {
			bytesToScavenge = uintptr(uint64(scav) + inuse - uint64(limit))
			forceScavenge = true
		}
	}
	if goal := scavenge.gcPercentGoal.Load(); goal != ^uint64(0) && growth > 0 {
		// We just caused a heap growth, so scavenge down what will soon be used.
		// By scavenging inline we deal with the failure to allocate out of
		// memory fragments by scavenging the memory fragments that are least
		// likely to be re-used.
		//
		// Only bother with this because we're not using a memory limit. We don't
		// care about heap growths as long as we're under the memory limit, and the
		// previous check for scaving already handles that.
		// 我们刚刚导致了堆增长，所以回收即将使用的内存。
		// 通过内联回收，我们通过回收最不可能被重用的内存碎片来处理分配失败。
		//
		// 只有在没有使用内存限制时才需要这样做。只要在内存限制之下，
		// 我们就不关心堆增长，之前的回收检查已经处理了这种情况。
		if retained := heapRetained(); retained+uint64(growth) > goal {
			// The scavenging algorithm requires the heap lock to be dropped so it
			// can acquire it only sparingly. This is a potentially expensive operation
			// so it frees up other goroutines to allocate in the meanwhile. In fact,
			// they can make use of the growth we just created.
			// 回收算法需要释放堆锁，这样它才能偶尔获取锁。
			// 这是一个潜在的高成本操作，所以它释放其他 goroutine 在此期间进行分配。
			// 事实上，它们可以利用我们刚刚创建的增长。
			todo := growth
			if overage := uintptr(retained + uint64(growth) - goal); todo > overage {
				todo = overage
			}
			if todo > bytesToScavenge {
				bytesToScavenge = todo
			}
		}
	}
	// There are a few very limited circumstances where we won't have a P here.
	// It's OK to simply skip scavenging in these cases. Something else will notice
	// and pick up the tab.
	// 在极少数情况下，我们可能没有 P 在这里。
	// 在这种情况下简单地跳过回收是可以的。其他东西会注意到并处理这个问题。
	var now int64
	if pp != nil && bytesToScavenge > 0 {
		// Measure how long we spent scavenging and add that measurement to the assist
		// time so we can track it for the GC CPU limiter.
		//
		// Limiter event tracking might be disabled if we end up here
		// while on a mark worker.
		// 测量我们花费在回收上的时间，并将这个测量添加到协助时间中，
		// 这样我们就可以为 GC CPU 限制器跟踪它。
		//
		// 如果我们在标记工作线程中，限制器事件跟踪可能会被禁用。
		start := nanotime()
		track := pp.limiterEvent.start(limiterEventScavengeAssist, start)

		// Scavenge, but back out if the limiter turns on.
		// 进行回收，但如果限制器开启则退出。
		released := h.pages.scavenge(bytesToScavenge, func() bool {
			return gcCPULimiter.limiting()
		}, forceScavenge)

		mheap_.pages.scav.releasedEager.Add(released)

		// Finish up accounting.
		// 完成统计
		now = nanotime()
		if track {
			pp.limiterEvent.stop(limiterEventScavengeAssist, now)
		}
		scavenge.assistTime.Add(now - start)
	}

	// Initialize the span.
	// 初始化 span
	h.initSpan(s, typ, spanclass, base, npages)

	// Commit and account for any scavenged memory that the span now owns.
	// 提交并统计 span 现在拥有的任何已回收内存
	nbytes := npages * pageSize
	if scav != 0 {
		// sysUsed all the pages that are actually available
		// in the span since some of them might be scavenged.
		// 使用 span 中实际可用的所有页面，因为其中一些可能已被回收
		sysUsed(unsafe.Pointer(base), nbytes, scav)
		gcController.heapReleased.add(-int64(scav))
	}
	// Update stats.
	// 更新统计信息
	gcController.heapFree.add(-int64(nbytes - scav))
	if typ == spanAllocHeap {
		gcController.heapInUse.add(int64(nbytes))
	}
	// Update consistent stats.
	// 更新一致性统计信息
	stats := memstats.heapStats.acquire()
	// Add the scavenged memory to committed memory and subtract it from released memory
	// 将回收的内存添加到已提交内存中，并从已释放内存中减去
	atomic.Xaddint64(&stats.committed, int64(scav))
	atomic.Xaddint64(&stats.released, -int64(scav))
	switch typ {
	case spanAllocHeap:
		// Update heap memory statistics
		// 更新堆内存统计信息
		atomic.Xaddint64(&stats.inHeap, int64(nbytes))
	case spanAllocStack:
		// Update stack memory statistics
		// 更新栈内存统计信息
		atomic.Xaddint64(&stats.inStacks, int64(nbytes))
	case spanAllocPtrScalarBits:
		// Update pointer/scalar bits memory statistics
		// 更新指针/标量位内存统计信息
		atomic.Xaddint64(&stats.inPtrScalarBits, int64(nbytes))
	case spanAllocWorkBuf:
		// Update work buffer memory statistics
		// 更新工作缓冲区内存统计信息
		atomic.Xaddint64(&stats.inWorkBufs, int64(nbytes))
	}
	memstats.heapStats.release()

	// Trace the span alloc.
	// 跟踪 span 的分配。
	if traceAllocFreeEnabled() {
		// 尝试获取跟踪器
		trace := traceTryAcquire()
		if trace.ok() {
			// 记录 span 分配事件
			trace.SpanAlloc(s)
			// 释放跟踪器
			traceRelease(trace)
		}
	}
	return s
}

// initSpan initializes a blank span s which will represent the range
// [base, base+npages*pageSize). typ is the type of span being allocated.
// initSpan 初始化一个空白 span s，它将表示范围 [base, base+npages*pageSize)。
// typ 是正在分配的 span 的类型。
func (h *mheap) initSpan(s *mspan, typ spanAllocType, spanclass spanClass, base, npages uintptr) {
	// At this point, both s != nil and base != 0, and the heap
	// lock is no longer held. Initialize the span.
	// 此时，s != nil 且 base != 0，且不再持有堆锁。初始化 span。
	s.init(base, npages)
	if h.allocNeedsZero(base, npages) {
		s.needzero = 1
	}
	nbytes := npages * pageSize
	if typ.manual() {
		// 手动管理的 span 初始化
		s.manualFreeList = 0
		s.nelems = 0
		s.limit = s.base() + s.npages*pageSize
		s.state.set(mSpanManual)
	} else {
		// We must set span properties before the span is published anywhere
		// since we're not holding the heap lock.
		// 由于我们没有持有堆锁，必须在 span 发布到任何地方之前设置 span 属性。
		s.spanclass = spanclass
		if sizeclass := spanclass.sizeclass(); sizeclass == 0 {
			// 大对象 span 初始化
			s.elemsize = nbytes
			s.nelems = 1
			s.divMul = 0
		} else {
			// 小对象 span 初始化
			s.elemsize = uintptr(class_to_size[sizeclass])
			if !s.spanclass.noscan() && heapBitsInSpan(s.elemsize) {
				// 为指针/扫描位图预留空间
				s.nelems = uint16((nbytes - (nbytes / goarch.PtrSize / 8)) / s.elemsize)
			} else {
				s.nelems = uint16(nbytes / s.elemsize)
			}
			s.divMul = class_to_divmagic[sizeclass]
		}

		// Initialize mark and allocation structures.
		// 初始化标记和分配结构
		s.freeindex = 0
		s.freeIndexForScan = 0
		s.allocCache = ^uint64(0) // all 1s indicating all free.
		s.gcmarkBits = newMarkBits(uintptr(s.nelems))
		s.allocBits = newAllocBits(uintptr(s.nelems))

		// It's safe to access h.sweepgen without the heap lock because it's
		// only ever updated with the world stopped and we run on the
		// systemstack which blocks a STW transition.
		// 在没有堆锁的情况下访问 h.sweepgen 是安全的，因为它只在世界停止时更新，
		// 而且我们在 systemstack 上运行，这会阻止 STW 转换。
		atomic.Store(&s.sweepgen, h.sweepgen)

		// Now that the span is filled in, set its state. This
		// is a publication barrier for the other fields in
		// the span. While valid pointers into this span
		// should never be visible until the span is returned,
		// if the garbage collector finds an invalid pointer,
		// access to the span may race with initialization of
		// the span. We resolve this race by atomically
		// setting the state after the span is fully
		// initialized, and atomically checking the state in
		// any situation where a pointer is suspect.
		// 现在 span 已填充，设置其状态。这是 span 中其他字段的发布屏障。
		// 虽然有效的指针在 span 返回之前不应该可见，但如果垃圾收集器
		// 发现无效指针，访问 span 可能会与 span 的初始化竞争。
		// 我们通过在 span 完全初始化后原子地设置状态，并在任何指针
		// 可疑的情况下原子地检查状态来解决这个竞争。
		s.state.set(mSpanInUse)
	}

	// Publish the span in various locations.
	// 在各种位置发布 span

	// This is safe to call without the lock held because the slots
	// related to this span will only ever be read or modified by
	// this thread until pointers into the span are published (and
	// we execute a publication barrier at the end of this function
	// before that happens) or pageInUse is updated.
	// 在没有持有锁的情况下调用是安全的，因为与此 span 相关的槽位
	// 只会被此线程读取或修改，直到指向 span 的指针被发布
	//（并且我们在此函数结束前执行发布屏障）或 pageInUse 被更新。
	h.setSpans(s.base(), npages, s)

	if !typ.manual() {
		// Mark in-use span in arena page bitmap.
		//
		// This publishes the span to the page sweeper, so
		// it's imperative that the span be completely initialized
		// prior to this line.
		// 在 arena 页面位图中标记正在使用的 span。
		//
		// 这将 span 发布到页面清扫器，因此在此行之前
		// span 必须完全初始化。
		arena, pageIdx, pageMask := pageIndexOf(s.base())
		atomic.Or8(&arena.pageInUse[pageIdx], pageMask)

		// Update related page sweeper stats.
		// 更新相关的页面清扫器统计信息
		h.pagesInUse.Add(npages)
	}

	// Make sure the newly allocated span will be observed
	// by the GC before pointers into the span are published.
	// 确保在指向 span 的指针发布之前，新分配的 span 会被 GC 观察到
	publicationBarrier()
}

// Try to add at least npage pages of memory to the heap,
// returning how much the heap grew by and whether it worked.
//
// h.lock must be held.
// 尝试向堆中添加至少 npage 页的内存，
// 返回堆增长了多少以及是否成功。
//
// 必须持有 h.lock。
func (h *mheap) grow(npage uintptr) (uintptr, bool) {
	assertLockHeld(&h.lock)

	// We must grow the heap in whole palloc chunks.
	// We call sysMap below but note that because we
	// round up to pallocChunkPages which is on the order
	// of MiB (generally >= to the huge page size) we
	// won't be calling it too much.
	// 我们必须以完整的 palloc 块为单位增长堆。
	// 我们在下面调用 sysMap，但要注意因为我们
	// 向上取整到 pallocChunkPages（通常在 MiB 级别，
	// 一般 >= 大页大小），所以我们不会调用太多次。
	ask := alignUp(npage, pallocChunkPages) * pageSize

	totalGrowth := uintptr(0)
	// This may overflow because ask could be very large
	// and is otherwise unrelated to h.curArena.base.
	// 这可能会溢出，因为 ask 可能非常大，
	// 而且与 h.curArena.base 无关。
	end := h.curArena.base + ask
	nBase := alignUp(end, physPageSize)
	if nBase > h.curArena.end || /* overflow */ end < h.curArena.base {
		// Not enough room in the current arena. Allocate more
		// arena space. This may not be contiguous with the
		// current arena, so we have to request the full ask.
		// 当前 arena 空间不足。分配更多 arena 空间。
		// 这可能与当前 arena 不连续，所以我们必须请求完整的 ask。
		av, asize := h.sysAlloc(ask, &h.arenaHints, true)
		if av == nil {
			inUse := gcController.heapFree.load() + gcController.heapReleased.load() + gcController.heapInUse.load()
			print("runtime: out of memory: cannot allocate ", ask, "-byte block (", inUse, " in use)\n")
			return 0, false
		}

		if uintptr(av) == h.curArena.end {
			// The new space is contiguous with the old
			// space, so just extend the current space.
			// 新空间与旧空间连续，所以只需扩展当前空间。
			h.curArena.end = uintptr(av) + asize
		} else {
			// The new space is discontiguous. Track what
			// remains of the current space and switch to
			// the new space. This should be rare.
			// 新空间不连续。跟踪当前空间的剩余部分并切换到新空间。
			// 这种情况应该很少见。
			if size := h.curArena.end - h.curArena.base; size != 0 {
				// Transition this space from Reserved to Prepared and mark it
				// as released since we'll be able to start using it after updating
				// the page allocator and releasing the lock at any time.
				// 将此空间从 Reserved 转换为 Prepared 并标记为已释放，
				// 因为我们在更新页面分配器并释放锁后随时可以开始使用它。
				sysMap(unsafe.Pointer(h.curArena.base), size, &gcController.heapReleased)
				// Update stats.
				// 更新统计信息
				stats := memstats.heapStats.acquire()
				atomic.Xaddint64(&stats.released, int64(size))
				memstats.heapStats.release()
				// Update the page allocator's structures to make this
				// space ready for allocation.
				// 更新页面分配器的结构，使此空间准备好进行分配。
				h.pages.grow(h.curArena.base, size)
				totalGrowth += size
			}
			// Switch to the new space.
			// 切换到新空间
			h.curArena.base = uintptr(av)
			h.curArena.end = uintptr(av) + asize
		}

		// Recalculate nBase.
		// We know this won't overflow, because sysAlloc returned
		// a valid region starting at h.curArena.base which is at
		// least ask bytes in size.
		// 重新计算 nBase。
		// 我们知道这不会溢出，因为 sysAlloc 返回了一个
		// 从 h.curArena.base 开始的有效区域，大小至少为 ask 字节。
		nBase = alignUp(h.curArena.base+ask, physPageSize)
	}

	// Grow into the current arena.
	// 在当前 arena 中增长
	v := h.curArena.base
	h.curArena.base = nBase

	// Transition the space we're going to use from Reserved to Prepared.
	//
	// The allocation is always aligned to the heap arena
	// size which is always > physPageSize, so its safe to
	// just add directly to heapReleased.
	// 将我们要使用的空间从 Reserved 转换为 Prepared。
	//
	// 分配总是对齐到堆 arena 大小，这总是 > physPageSize，
	// 所以直接添加到 heapReleased 是安全的。
	sysMap(unsafe.Pointer(v), nBase-v, &gcController.heapReleased)

	// The memory just allocated counts as both released
	// and idle, even though it's not yet backed by spans.
	// 刚分配的内存既算作已释放也算作空闲，
	// 即使它还没有被 spans 支持。
	stats := memstats.heapStats.acquire()
	atomic.Xaddint64(&stats.released, int64(nBase-v))
	memstats.heapStats.release()

	// Update the page allocator's structures to make this
	// space ready for allocation.
	// 更新页面分配器的结构，使此空间准备好进行分配。
	h.pages.grow(v, nBase-v)
	totalGrowth += nBase - v
	return totalGrowth, true
}

// Free the span back into the heap.
// 将 span 释放回堆中
func (h *mheap) freeSpan(s *mspan) {
	systemstack(func() {
		// Trace the span free.
		// 跟踪 span 的释放
		if traceAllocFreeEnabled() {
			trace := traceTryAcquire()
			if trace.ok() {
				trace.SpanFree(s)
				traceRelease(trace)
			}
		}

		lock(&h.lock)
		if msanenabled {
			// Tell msan that this entire span is no longer in use.
			// 告诉 msan 这个 span 不再使用
			base := unsafe.Pointer(s.base())
			bytes := s.npages << _PageShift
			msanfree(base, bytes)
		}
		if asanenabled {
			// Tell asan that this entire span is no longer in use.
			// 告诉 asan 这个 span 不再使用
			base := unsafe.Pointer(s.base())
			bytes := s.npages << _PageShift
			asanpoison(base, bytes)
		}
		h.freeSpanLocked(s, spanAllocHeap)
		unlock(&h.lock)
	})
}

// freeManual frees a manually-managed span returned by allocManual.
// typ must be the same as the spanAllocType passed to the allocManual that
// allocated s.
//
// This must only be called when gcphase == _GCoff. See mSpanState for
// an explanation.
//
// freeManual must be called on the system stack because it acquires
// the heap lock. See mheap for details.
//
// freeManual 释放由 allocManual 返回的手动管理的 span。
// typ 必须与分配 s 时传递给 allocManual 的 spanAllocType 相同。
//
// 这只能在 gcphase == _GCoff 时调用。有关说明，请参见 mSpanState。
//
// freeManual 必须在系统栈上调用，因为它会获取堆锁。有关详细信息，请参见 mheap。
//
//go:systemstack
func (h *mheap) freeManual(s *mspan, typ spanAllocType) {
	// Trace the span free.
	// 跟踪 span 的释放
	if traceAllocFreeEnabled() {
		trace := traceTryAcquire()
		if trace.ok() {
			trace.SpanFree(s)
			traceRelease(trace)
		}
	}

	// 标记 span 需要清零
	s.needzero = 1
	// 获取堆锁
	lock(&h.lock)
	// 释放 span
	h.freeSpanLocked(s, typ)
	// 释放堆锁
	unlock(&h.lock)
}

func (h *mheap) freeSpanLocked(s *mspan, typ spanAllocType) {
	// 确保持有堆锁
	assertLockHeld(&h.lock)

	// 根据 span 的状态进行不同的处理
	switch s.state.get() {
	case mSpanManual:
		// 对于手动管理的 span，确保没有分配的对象
		if s.allocCount != 0 {
			throw("mheap.freeSpanLocked - invalid stack free")
		}
	case mSpanInUse:
		// 对于正在使用的 span，进行一系列检查
		if s.isUserArenaChunk {
			throw("mheap.freeSpanLocked - invalid free of user arena chunk")
		}
		// 确保 span 没有被分配且清扫代与当前代相同
		if s.allocCount != 0 || s.sweepgen != h.sweepgen {
			print("mheap.freeSpanLocked - span ", s, " ptr ", hex(s.base()), " allocCount ", s.allocCount, " sweepgen ", s.sweepgen, "/", h.sweepgen, "\n")
			throw("mheap.freeSpanLocked - invalid free")
		}
		// 减少正在使用的页面计数
		h.pagesInUse.Add(-s.npages)

		// Clear in-use bit in arena page bitmap.
		// 清除 arena 页面位图中的使用位
		arena, pageIdx, pageMask := pageIndexOf(s.base())
		atomic.And8(&arena.pageInUse[pageIdx], ^pageMask)
	default:
		throw("mheap.freeSpanLocked - invalid span state")
	}

	// Update stats.
	//
	// Mirrors the code in allocSpan.
	// 更新统计信息
	// 这部分代码与 allocSpan 中的代码相对应
	nbytes := s.npages * pageSize
	gcController.heapFree.add(int64(nbytes))
	if typ == spanAllocHeap {
		gcController.heapInUse.add(-int64(nbytes))
	}
	// Update consistent stats.
	// 更新一致性统计信息
	stats := memstats.heapStats.acquire()
	switch typ {
	case spanAllocHeap:
		atomic.Xaddint64(&stats.inHeap, -int64(nbytes))
	case spanAllocStack:
		atomic.Xaddint64(&stats.inStacks, -int64(nbytes))
	case spanAllocPtrScalarBits:
		atomic.Xaddint64(&stats.inPtrScalarBits, -int64(nbytes))
	case spanAllocWorkBuf:
		atomic.Xaddint64(&stats.inWorkBufs, -int64(nbytes))
	}
	memstats.heapStats.release()

	// Mark the space as free.
	// 将空间标记为空闲
	h.pages.free(s.base(), s.npages)

	// Free the span structure. We no longer have a use for it.
	// 释放 span 结构体，我们不再需要它
	s.state.set(mSpanDead)
	h.freeMSpanLocked(s)
}

// scavengeAll acquires the heap lock (blocking any additional
// manipulation of the page allocator) and iterates over the whole
// heap, scavenging every free page available.
//
// Must run on the system stack because it acquires the heap lock.
//
// scavengeAll 获取堆锁（阻止对页面分配器的任何额外操作）并遍历整个堆，
// 回收所有可用的空闲页面。
//
// 必须在系统栈上运行，因为它获取了堆锁。
//
//go:systemstack
func (h *mheap) scavengeAll() {
	// Disallow malloc or panic while holding the heap lock. We do
	// this here because this is a non-mallocgc entry-point to
	// the mheap API.
	// 在持有堆锁时禁止 malloc 或 panic。我们在这里这样做是因为
	// 这是 mheap API 的一个非 mallocgc 入口点。
	gp := getg()
	gp.m.mallocing++

	// Force scavenge everything.
	// 强制回收所有内容
	released := h.pages.scavenge(^uintptr(0), nil, true)

	gp.m.mallocing--

	if debug.scavtrace > 0 {
		printScavTrace(0, released, true)
	}
}

//go:linkname runtime_debug_freeOSMemory runtime/debug.freeOSMemory
func runtime_debug_freeOSMemory() {
	GC()
	systemstack(func() { mheap_.scavengeAll() })
}

// Initialize a new span with the given start and npages.
// 使用给定的起始地址和页面数量初始化一个新的 span
func (span *mspan) init(base uintptr, npages uintptr) {
	// span is *not* zeroed.
	// span 不会被清零
	span.next = nil                                   // 下一个 span 指针
	span.prev = nil                                   // 上一个 span 指针
	span.list = nil                                   // 所属的 span 列表
	span.startAddr = base                             // span 的起始地址
	span.npages = npages                              // span 包含的页面数量
	span.allocCount = 0                               // 已分配的对象数量
	span.spanclass = 0                                // span 的大小类别
	span.elemsize = 0                                 // 每个对象的大小
	span.speciallock.key = 0                          // 特殊对象的锁
	span.specials = nil                               // 特殊对象列表
	span.needzero = 0                                 // 是否需要清零
	span.freeindex = 0                                // 下一个空闲对象的索引
	span.freeIndexForScan = 0                         // 用于扫描的空闲对象索引
	span.allocBits = nil                              // 分配位图
	span.gcmarkBits = nil                             // GC 标记位图
	span.pinnerBits = nil                             // 固定位图
	span.state.set(mSpanDead)                         // 设置 span 状态为死亡
	lockInit(&span.speciallock, lockRankMspanSpecial) // 初始化特殊对象的锁
}

func (span *mspan) inList() bool {
	return span.list != nil
}

// Initialize an empty doubly-linked list.
func (list *mSpanList) init() {
	list.first = nil
	list.last = nil
}

func (list *mSpanList) remove(span *mspan) {
	// Check if the span is actually in this list.
	// 检查 span 是否确实在这个列表中
	if span.list != list {
		print("runtime: failed mSpanList.remove span.npages=", span.npages,
			" span=", span, " prev=", span.prev, " span.list=", span.list, " list=", list, "\n")
		throw("mSpanList.remove")
	}

	// Update the first pointer if removing the first span.
	// 如果移除的是第一个 span，更新 first 指针
	if list.first == span {
		list.first = span.next
	} else {
		// Otherwise, update the previous span's next pointer.
		// 否则，更新前一个 span 的 next 指针
		span.prev.next = span.next
	}

	// Update the last pointer if removing the last span.
	// 如果移除的是最后一个 span，更新 last 指针
	if list.last == span {
		list.last = span.prev
	} else {
		// Otherwise, update the next span's previous pointer.
		// 否则，更新下一个 span 的 prev 指针
		span.next.prev = span.prev
	}

	// Clear the span's list pointers.
	// 清除 span 的列表指针
	span.next = nil
	span.prev = nil
	span.list = nil
}

func (list *mSpanList) isEmpty() bool {
	return list.first == nil
}

func (list *mSpanList) insert(span *mspan) {
	if span.next != nil || span.prev != nil || span.list != nil {
		println("runtime: failed mSpanList.insert", span, span.next, span.prev, span.list)
		throw("mSpanList.insert")
	}
	span.next = list.first
	if list.first != nil {
		// The list contains at least one span; link it in.
		// The last span in the list doesn't change.
		// 列表至少包含一个 span；将其链接进去。
		// 列表中的最后一个 span 不会改变。
		list.first.prev = span
	} else {
		// The list contains no spans, so this is also the last span.
		// 列表不包含任何 span，所以这也是最后一个 span。
		list.last = span
	}
	list.first = span
	span.list = list
}

func (list *mSpanList) insertBack(span *mspan) {
	// Check if the span is already in a list
	// 检查 span 是否已经在某个列表中
	if span.next != nil || span.prev != nil || span.list != nil {
		println("runtime: failed mSpanList.insertBack", span, span.next, span.prev, span.list)
		throw("mSpanList.insertBack")
	}
	// Set the previous pointer to the current last span
	// 将前一个指针设置为当前最后一个 span
	span.prev = list.last
	if list.last != nil {
		// The list contains at least one span.
		// 列表至少包含一个 span
		list.last.next = span
	} else {
		// The list contains no spans, so this is also the first span.
		// 列表不包含任何 span，所以这也是第一个 span
		list.first = span
	}
	// Update the last pointer to point to the new span
	// 更新最后一个指针指向新的 span
	list.last = span
	// Set the list pointer to the current list
	// 设置列表指针指向当前列表
	span.list = list
}

// takeAll removes all spans from other and inserts them at the front
// of list.
// takeAll 从 other 中移除所有 spans 并将它们插入到 list 的前面
func (list *mSpanList) takeAll(other *mSpanList) {
	// If other is empty, there's nothing to do
	// 如果 other 为空，则无需做任何操作
	if other.isEmpty() {
		return
	}

	// Reparent everything in other to list.
	// 将 other 中的所有 span 重新关联到 list
	for s := other.first; s != nil; s = s.next {
		s.list = list
	}

	// Concatenate the lists.
	// 连接两个列表
	if list.isEmpty() {
		// If list is empty, just copy everything from other
		// 如果 list 为空，直接复制 other 的所有内容
		*list = *other
	} else {
		// Neither list is empty. Put other before list.
		// 两个列表都不为空。将 other 放在 list 前面
		// 1. 将 other 的最后一个元素连接到 list 的第一个元素
		other.last.next = list.first
		// 2. 将 list 的第一个元素的前驱指向 other 的最后一个元素
		list.first.prev = other.last
		// 3. 更新 list 的第一个元素为 other 的第一个元素
		list.first = other.first
	}

	// Clear other list
	// 清空 other 列表
	other.first, other.last = nil, nil
}

const (
	// _KindSpecialFinalizer is for tracking finalizers.
	// _KindSpecialFinalizer 用于跟踪终结器
	_KindSpecialFinalizer = 1
	// _KindSpecialWeakHandle is used for creating weak pointers.
	// _KindSpecialWeakHandle 用于创建弱指针
	_KindSpecialWeakHandle = 2
	// _KindSpecialProfile is for memory profiling.
	// _KindSpecialProfile 用于内存分析
	_KindSpecialProfile = 3
	// _KindSpecialReachable is a special used for tracking
	// reachability during testing.
	// _KindSpecialReachable 是一个特殊的类型，用于在测试期间跟踪可达性
	_KindSpecialReachable = 4
	// _KindSpecialPinCounter is a special used for objects that are pinned
	// multiple times
	// _KindSpecialPinCounter 是一个特殊的类型，用于被多次固定的对象
	_KindSpecialPinCounter = 5
)

type special struct {
	_    sys.NotInHeap
	next *special // linked list in span
	// next 指向 span 中的下一个 special 对象，形成链表
	offset uint16 // span offset of object
	// offset 表示对象在 span 中的偏移量
	kind byte // kind of special
	// kind 表示 special 的类型，对应上面的常量值
}

// spanHasSpecials marks a span as having specials in the arena bitmap.
// spanHasSpecials 在 arena 位图中标记 span 包含特殊对象
func spanHasSpecials(s *mspan) {
	// Calculate which page in the arena this span starts on
	// 计算这个 span 在 arena 中开始的页面
	arenaPage := (s.base() / pageSize) % pagesPerArena
	// Get the arena index for this span
	// 获取这个 span 的 arena 索引
	ai := arenaIndex(s.base())
	// Get the heap arena containing this span
	// 获取包含这个 span 的堆 arena
	ha := mheap_.arenas[ai.l1()][ai.l2()]
	// Set the corresponding bit in the pageSpecials bitmap
	// 在 pageSpecials 位图中设置对应的位
	atomic.Or8(&ha.pageSpecials[arenaPage/8], uint8(1)<<(arenaPage%8))
}

// spanHasNoSpecials marks a span as having no specials in the arena bitmap.
// spanHasNoSpecials 在 arena 位图中标记 span 不包含特殊对象
func spanHasNoSpecials(s *mspan) {
	// Calculate which page in the arena this span starts on
	// 计算这个 span 在 arena 中开始的页面
	arenaPage := (s.base() / pageSize) % pagesPerArena
	// Get the arena index for this span
	// 获取这个 span 的 arena 索引
	ai := arenaIndex(s.base())
	// Get the heap arena containing this span
	// 获取包含这个 span 的堆 arena
	ha := mheap_.arenas[ai.l1()][ai.l2()]
	// Clear the corresponding bit in the pageSpecials bitmap
	// 在 pageSpecials 位图中清除对应的位
	atomic.And8(&ha.pageSpecials[arenaPage/8], ^(uint8(1) << (arenaPage % 8)))
}

// Adds the special record s to the list of special records for
// the object p. All fields of s should be filled in except for
// offset & next, which this routine will fill in.
// Returns true if the special was successfully added, false otherwise.
// (The add will fail only if a record with the same p and s->kind
// already exists.)
// 将特殊记录 s 添加到对象 p 的特殊记录列表中。
// s 的所有字段都应该被填充，除了 offset 和 next，这两个字段将由本函数填充。
// 如果特殊记录成功添加则返回 true，否则返回 false。
// （只有当具有相同 p 和 s->kind 的记录已存在时，添加才会失败。）
func addspecial(p unsafe.Pointer, s *special) bool {
	// 获取包含指针 p 的 span
	span := spanOfHeap(uintptr(p))
	if span == nil {
		throw("addspecial on invalid pointer")
	}

	// Ensure that the span is swept.
	// Sweeping accesses the specials list w/o locks, so we have
	// to synchronize with it. And it's just much safer.
	// 确保 span 已被清扫。
	// 清扫过程会无锁访问 specials 列表，所以我们需要与之同步。
	// 这样做更安全。
	mp := acquirem()
	span.ensureSwept()

	// 计算对象在 span 中的偏移量
	offset := uintptr(p) - span.base()
	kind := s.kind

	// 锁定 span 的特殊记录列表
	lock(&span.speciallock)

	// Find splice point, check for existing record.
	// 查找插入点，检查是否已存在记录
	iter, exists := span.specialFindSplicePoint(offset, kind)
	if !exists {
		// Splice in record, fill in offset.
		// 插入记录，填充偏移量
		s.offset = uint16(offset)
		s.next = *iter
		*iter = s
		// 标记 span 包含特殊记录
		spanHasSpecials(span)
	}

	unlock(&span.speciallock)
	releasem(mp)
	return !exists // already exists
}

// Removes the Special record of the given kind for the object p.
// Returns the record if the record existed, nil otherwise.
// The caller must FixAlloc_Free the result.
// 移除对象 p 的指定类型的特殊记录。
// 如果记录存在则返回该记录，否则返回 nil。
// 调用者必须使用 FixAlloc_Free 释放返回的结果。
func removespecial(p unsafe.Pointer, kind uint8) *special {
	// 获取包含指针 p 的 span
	span := spanOfHeap(uintptr(p))
	if span == nil {
		throw("removespecial on invalid pointer")
	}

	// Ensure that the span is swept.
	// Sweeping accesses the specials list w/o locks, so we have
	// to synchronize with it. And it's just much safer.
	// 确保 span 已被清扫。
	// 清扫过程会无锁访问 specials 列表，所以我们需要与之同步。
	// 这样做更安全。
	mp := acquirem()
	span.ensureSwept()

	// 计算对象在 span 中的偏移量
	offset := uintptr(p) - span.base()

	var result *special
	// 锁定 span 的特殊记录列表
	lock(&span.speciallock)

	// 查找并移除指定类型的特殊记录
	iter, exists := span.specialFindSplicePoint(offset, kind)
	if exists {
		// 从链表中移除记录
		s := *iter
		*iter = s.next
		result = s
	}
	// 如果特殊记录列表为空，更新 span 状态
	if span.specials == nil {
		spanHasNoSpecials(span)
	}
	unlock(&span.speciallock)
	releasem(mp)
	return result
}

// Find a splice point in the sorted list and check for an already existing
// record. Returns a pointer to the next-reference in the list predecessor.
// Returns true, if the referenced item is an exact match.
// 在已排序的列表中查找插入点并检查是否已存在记录。
// 返回列表中前驱节点的 next 指针的地址。
// 如果找到完全匹配的项则返回 true。
func (span *mspan) specialFindSplicePoint(offset uintptr, kind byte) (**special, bool) {
	// Find splice point, check for existing record.
	// 查找插入点，检查是否存在记录
	iter := &span.specials
	found := false
	for {
		s := *iter
		if s == nil {
			break
		}
		// 如果找到完全匹配的记录（相同的偏移量和类型）
		if offset == uintptr(s.offset) && kind == s.kind {
			found = true
			break
		}
		// 如果当前记录的偏移量大于目标偏移量，
		// 或者偏移量相同但类型更大，则找到了插入点
		if offset < uintptr(s.offset) || (offset == uintptr(s.offset) && kind < s.kind) {
			break
		}
		// 继续遍历下一个节点
		iter = &s.next
	}
	return iter, found
}

// The described object has a finalizer set for it.
//
// specialfinalizer is allocated from non-GC'd memory, so any heap
// pointers must be specially handled.
// 描述的对象设置了终结器。
//
// specialfinalizer 是从非 GC 内存中分配的，因此任何堆指针都必须特殊处理。
type specialfinalizer struct {
	_       sys.NotInHeap // 标记此类型不在堆上分配
	special special       // 基础特殊记录结构
	fn      *funcval      // May be a heap pointer. 可能是堆指针，指向终结器函数
	nret    uintptr       // 终结器函数的返回值数量
	fint    *_type        // May be a heap pointer, but always live. 可能是堆指针，但总是存活的，指向终结器函数的输入类型
	ot      *ptrtype      // May be a heap pointer, but always live. 可能是堆指针，但总是存活的，指向终结器函数的输出类型
}

// Adds a finalizer to the object p. Returns true if it succeeded.
// 为对象 p 添加终结器。如果成功则返回 true。
func addfinalizer(p unsafe.Pointer, f *funcval, nret uintptr, fint *_type, ot *ptrtype) bool {
	// 分配一个新的 specialfinalizer 结构体
	lock(&mheap_.speciallock)
	s := (*specialfinalizer)(mheap_.specialfinalizeralloc.alloc())
	unlock(&mheap_.speciallock)

	// 设置终结器的属性
	s.special.kind = _KindSpecialFinalizer
	s.fn = f
	s.nret = nret
	s.fint = fint
	s.ot = ot

	// 尝试将终结器添加到对象的特殊记录中
	if addspecial(p, &s.special) {
		// This is responsible for maintaining the same
		// GC-related invariants as markrootSpans in any
		// situation where it's possible that markrootSpans
		// has already run but mark termination hasn't yet.
		// 这负责维护与 markrootSpans 相同的 GC 相关不变量，
		// 在任何可能 markrootSpans 已经运行但标记终止尚未发生的情况下。
		if gcphase != _GCoff {
			// 查找对象及其所属的 span
			base, span, _ := findObject(uintptr(p), 0, 0)
			mp := acquirem()
			gcw := &mp.p.ptr().gcw

			// Mark everything reachable from the object
			// so it's retained for the finalizer.
			// 标记从对象可达的所有内容，以便为终结器保留它们
			if !span.spanclass.noscan() {
				scanobject(base, gcw)
			}

			// Mark the finalizer itself, since the
			// special isn't part of the GC'd heap.
			// 标记终结器本身，因为 special 不是 GC 堆的一部分
			scanblock(uintptr(unsafe.Pointer(&s.fn)), goarch.PtrSize, &oneptrmask[0], gcw, nil)
			releasem(mp)
		}
		return true
	}

	// There was an old finalizer
	// 已经存在旧的终结器
	lock(&mheap_.speciallock)
	mheap_.specialfinalizeralloc.free(unsafe.Pointer(s))
	unlock(&mheap_.speciallock)
	return false
}

// Removes the finalizer (if any) from the object p.
// 从对象 p 中移除终结器（如果存在的话）
func removefinalizer(p unsafe.Pointer) {
	// 尝试移除对象的特殊终结器记录
	s := (*specialfinalizer)(unsafe.Pointer(removespecial(p, _KindSpecialFinalizer)))
	if s == nil {
		return // there wasn't a finalizer to remove
		// 如果没有终结器需要移除，直接返回
	}
	// 获取特殊分配器的锁，安全地释放终结器结构体
	lock(&mheap_.speciallock)
	mheap_.specialfinalizeralloc.free(unsafe.Pointer(s))
	unlock(&mheap_.speciallock)
}

// The described object has a weak pointer.
//
// Weak pointers in the GC have the following invariants:
//
//   - Strong-to-weak conversions must ensure the strong pointer
//     remains live until the weak handle is installed. This ensures
//     that creating a weak pointer cannot fail.
//
//   - Weak-to-strong conversions require the weakly-referenced
//     object to be swept before the conversion may proceed. This
//     ensures that weak-to-strong conversions cannot resurrect
//     dead objects by sweeping them before that happens.
//
//   - Weak handles are unique and canonical for each byte offset into
//     an object that a strong pointer may point to, until an object
//     becomes unreachable.
//
//   - Weak handles contain nil as soon as an object becomes unreachable
//     the first time, before a finalizer makes it reachable again. New
//     weak handles created after resurrection are newly unique.
//
// specialWeakHandle is allocated from non-GC'd memory, so any heap
// pointers must be specially handled.

// 描述的对象有一个弱指针。
//
// GC 中的弱指针有以下不变性：
//
//   - 强到弱的转换必须确保强指针在弱句柄安装之前保持存活。
//     这确保了创建弱指针不会失败。
//
//   - 弱到强的转换要求弱引用对象在转换进行之前被清扫。
//     这确保了弱到强的转换不能通过提前清扫来复活死对象。
//
//   - 弱句柄对于强指针可能指向的对象中的每个字节偏移量都是唯一且规范的，
//     直到对象变得不可达。
//
//   - 一旦对象第一次变得不可达，弱句柄立即包含 nil，
//     在终结器使其再次可达之前。复活后创建的新弱句柄是新的唯一句柄。
//
// specialWeakHandle 是从非 GC 内存分配的，因此任何堆指针都必须特殊处理。
type specialWeakHandle struct {
	_       sys.NotInHeap
	special special
	// handle is a reference to the actual weak pointer.
	// It is always heap-allocated and must be explicitly kept
	// live so long as this special exists.
	// handle 是对实际弱指针的引用。
	// 它总是堆分配的，只要这个 special 存在就必须显式保持存活。
	handle *atomic.Uintptr
}

// Register a weak pointer for the given pointer p.
// Returns a handle to the weak pointer that can be used to convert it back to a strong pointer.
// 为给定的指针 p 注册一个弱指针。
// 返回一个弱指针的句柄，可用于将其转换回强指针。
//
//go:linkname internal_weak_runtime_registerWeakPointer internal/weak.runtime_registerWeakPointer
func internal_weak_runtime_registerWeakPointer(p unsafe.Pointer) unsafe.Pointer {
	return unsafe.Pointer(getOrAddWeakHandle(unsafe.Pointer(p)))
}

//go:linkname internal_weak_runtime_makeStrongFromWeak internal/weak.runtime_makeStrongFromWeak
func internal_weak_runtime_makeStrongFromWeak(u unsafe.Pointer) unsafe.Pointer {
	handle := (*atomic.Uintptr)(u)

	// Prevent preemption. We want to make sure that another GC cycle can't start
	// and that work.strongFromWeak.block can't change out from under us.
	// 防止抢占。我们想要确保另一个 GC 周期不能开始，
	// 并且 work.strongFromWeak.block 不能在我们下面改变。
	mp := acquirem()

	// Yield to the GC if necessary.
	// 如果需要，让出给 GC
	if work.strongFromWeak.block {
		releasem(mp)

		// Try to park and wait for mark termination.
		// N.B. gcParkStrongFromWeak calls acquirem before returning.
		// 尝试暂停并等待标记终止。
		// 注意：gcParkStrongFromWeak 在返回前会调用 acquirem。
		mp = gcParkStrongFromWeak()
	}

	p := handle.Load()
	if p == 0 {
		releasem(mp)
		return nil
	}
	// Be careful. p may or may not refer to valid memory anymore, as it could've been
	// swept and released already. It's always safe to ensure a span is swept, though,
	// even if it's just some random span.
	// 要小心。p 可能不再引用有效内存，因为它可能已经被清扫和释放了。
	// 不过，确保一个 span 被清扫总是安全的，即使它只是某个随机的 span。
	span := spanOfHeap(p)
	if span == nil {
		// The span probably got swept and released.
		// span 可能已经被清扫和释放了。
		releasem(mp)
		return nil
	}
	// Ensure the span is swept.
	// 确保 span 被清扫
	span.ensureSwept()

	// Now we can trust whatever we get from handle, so make a strong pointer.
	//
	// Even if we just swept some random span that doesn't contain this object, because
	// this object is long dead and its memory has since been reused, we'll just observe nil.
	// 现在我们可以信任从 handle 获取的任何内容，所以创建一个强指针。
	//
	// 即使我们刚刚清扫了一些不包含此对象的随机 span，
	// 因为这个对象早就死了，它的内存已经被重用，我们只会观察到 nil。
	ptr := unsafe.Pointer(handle.Load())

	// This is responsible for maintaining the same GC-related
	// invariants as the Yuasa part of the write barrier. During
	// the mark phase, it's possible that we just created the only
	// valid pointer to the object pointed to by ptr. If it's only
	// ever referenced from our stack, and our stack is blackened
	// already, we could fail to mark it. So, mark it now.
	// 这负责维护与写屏障的 Yuasa 部分相同的 GC 相关不变性。
	// 在标记阶段，我们可能刚刚创建了指向 ptr 所指向对象的唯一有效指针。
	// 如果它只从我们的栈中引用，而我们的栈已经被染黑，
	// 我们可能会无法标记它。所以现在标记它。
	if gcphase != _GCoff {
		shade(uintptr(ptr))
	}
	releasem(mp)

	// Explicitly keep ptr alive. This seems unnecessary since we return ptr,
	// but let's be explicit since it's important we keep ptr alive across the
	// call to shade.
	// 显式保持 ptr 存活。这看起来是不必要的，因为我们返回 ptr，
	// 但让我们显式地这样做，因为保持 ptr 在调用 shade 期间存活很重要。
	KeepAlive(ptr)
	return ptr
}

// gcParkStrongFromWeak puts the current goroutine on the weak->strong queue and parks.
// gcParkStrongFromWeak 将当前 goroutine 放入 weak->strong 队列并暂停
func gcParkStrongFromWeak() *m {
	// Prevent preemption as we check strongFromWeak, so it can't change out from under us.
	// 防止在我们检查 strongFromWeak 时被抢占，这样它就不会在我们下面改变
	mp := acquirem()

	for work.strongFromWeak.block {
		lock(&work.strongFromWeak.lock)
		releasem(mp) // N.B. Holding the lock prevents preemption.
		// 注意：持有锁可以防止抢占

		// Queue ourselves up.
		// 将我们自己加入队列
		work.strongFromWeak.q.pushBack(getg())

		// Park.
		// 暂停当前 goroutine
		goparkunlock(&work.strongFromWeak.lock, waitReasonGCWeakToStrongWait, traceBlockGCWeakToStrongWait, 2)

		// Re-acquire the current M since we're going to check the condition again.
		// 重新获取当前 M，因为我们要再次检查条件
		mp = acquirem()

		// Re-check condition. We may have awoken in the next GC's mark termination phase.
		// 重新检查条件。我们可能在下一次 GC 的标记终止阶段被唤醒
	}
	return mp
}

// gcWakeAllStrongFromWeak wakes all currently blocked weak->strong
// conversions. This is used at the end of a GC cycle.
//
// work.strongFromWeak.block must be false to prevent woken goroutines
// from immediately going back to sleep.
// gcWakeAllStrongFromWeak 唤醒所有当前被阻塞的 weak->strong 转换。
// 这用于 GC 周期的结束。
//
// work.strongFromWeak.block 必须为 false，以防止被唤醒的 goroutines
// 立即重新进入睡眠状态。
func gcWakeAllStrongFromWeak() {
	// 获取锁以保护对 weak->strong 队列的访问
	lock(&work.strongFromWeak.lock)
	// 从队列中弹出所有等待的 goroutines
	list := work.strongFromWeak.q.popList()
	// 将弹出的 goroutines 注入到调度器中，使它们可以继续执行
	injectglist(&list)
	// 释放锁
	unlock(&work.strongFromWeak.lock)
}

// Retrieves or creates a weak pointer handle for the object p.
// 获取或创建对象 p 的弱指针句柄
func getOrAddWeakHandle(p unsafe.Pointer) *atomic.Uintptr {
	// First try to retrieve without allocating.
	// 首先尝试在不分配的情况下获取
	if handle := getWeakHandle(p); handle != nil {
		// Keep p alive for the duration of the function to ensure
		// that it cannot die while we're trying to do this.
		// 在函数执行期间保持 p 存活，以确保在我们尝试这样做时它不会死亡
		KeepAlive(p)
		return handle
	}

	// 分配一个新的特殊弱句柄对象
	lock(&mheap_.speciallock)
	s := (*specialWeakHandle)(mheap_.specialWeakHandleAlloc.alloc())
	unlock(&mheap_.speciallock)

	// 创建并初始化新的弱句柄
	handle := new(atomic.Uintptr)
	s.special.kind = _KindSpecialWeakHandle
	s.handle = handle
	handle.Store(uintptr(p))
	if addspecial(p, &s.special) {
		// This is responsible for maintaining the same
		// GC-related invariants as markrootSpans in any
		// situation where it's possible that markrootSpans
		// has already run but mark termination hasn't yet.
		// 这负责在任何 markrootSpans 可能已经运行但标记终止尚未完成的情况下，
		// 维护与 markrootSpans 相同的 GC 相关不变量
		if gcphase != _GCoff {
			mp := acquirem()
			gcw := &mp.p.ptr().gcw
			// Mark the weak handle itself, since the
			// special isn't part of the GC'd heap.
			// 标记弱句柄本身，因为 special 不是 GC 堆的一部分
			scanblock(uintptr(unsafe.Pointer(&s.handle)), goarch.PtrSize, &oneptrmask[0], gcw, nil)
			releasem(mp)
		}

		// Keep p alive for the duration of the function to ensure
		// that it cannot die while we're trying to do this.
		//
		// Same for handle, which is only stored in the special.
		// There's a window where it might die if we don't keep it
		// alive explicitly. Returning it here is probably good enough,
		// but let's be defensive and explicit. See #70455.
		// 在函数执行期间保持 p 和 handle 存活，以确保它们不会死亡。
		// 虽然在这里返回它可能已经足够，但为了防御性和明确性，
		// 我们还是显式地保持它们存活。参见 #70455
		KeepAlive(p)
		KeepAlive(handle)
		return handle
	}

	// There was an existing handle. Free the special
	// and try again. We must succeed because we're explicitly
	// keeping p live until the end of this function. Either
	// we, or someone else, must have succeeded, because we can
	// only fail in the event of a race, and p will still be
	// be valid no matter how much time we spend here.
	// 存在现有的句柄。释放特殊对象并重试。
	// 我们必须成功，因为我们显式地保持 p 存活直到函数结束。
	// 要么我们，要么其他人必须成功，因为我们只能在竞争的情况下失败，
	// 而且无论我们在这里花费多少时间，p 都仍然有效
	lock(&mheap_.speciallock)
	mheap_.specialWeakHandleAlloc.free(unsafe.Pointer(s))
	unlock(&mheap_.speciallock)

	handle = getWeakHandle(p)
	if handle == nil {
		throw("failed to get or create weak handle")
	}

	// Keep p alive for the duration of the function to ensure
	// that it cannot die while we're trying to do this.
	//
	// Same for handle, just to be defensive.
	// 在函数执行期间保持 p 和 handle 存活，以确保它们不会死亡。
	// 对 handle 也这样做，只是为了防御性
	KeepAlive(p)
	KeepAlive(handle)
	return handle
}

func getWeakHandle(p unsafe.Pointer) *atomic.Uintptr {
	// Ensure that the span is swept.
	// Sweeping accesses the specials list w/o locks, so we have
	// to synchronize with it. And it's just much safer.
	// 确保 span 已被清扫。
	// 清扫操作会在没有锁的情况下访问 specials 列表，所以我们需要
	// 与它同步。这样做也更安全。
	span := spanOfHeap(uintptr(p))
	if span == nil {
		throw("getWeakHandle on invalid pointer")
	}

	// 获取当前 M 并确保 span 已被清扫
	mp := acquirem()
	span.ensureSwept()

	// 计算对象在 span 中的偏移量
	offset := uintptr(p) - span.base()

	// 加锁以安全地访问 specials 列表
	lock(&span.speciallock)

	// Find the existing record and return the handle if one exists.
	// 查找现有记录，如果存在则返回句柄
	var handle *atomic.Uintptr
	iter, exists := span.specialFindSplicePoint(offset, _KindSpecialWeakHandle)
	if exists {
		handle = ((*specialWeakHandle)(unsafe.Pointer(*iter))).handle
	}
	unlock(&span.speciallock)
	releasem(mp)

	// Keep p alive for the duration of the function to ensure
	// that it cannot die while we're trying to do this.
	// 在函数执行期间保持 p 存活，以确保它不会死亡
	KeepAlive(p)
	return handle
}

// The described object is being heap profiled.
// 描述的对象正在进行堆分析
type specialprofile struct {
	_       sys.NotInHeap // 确保结构体不会在堆上分配
	special special       // 基础特殊记录结构
	b       *bucket       // 关联的堆分析桶
}

// Set the heap profile bucket associated with addr to b.
// 将地址 addr 关联的堆分析桶设置为 b
func setprofilebucket(p unsafe.Pointer, b *bucket) {
	// 加锁以安全地分配 specialprofile
	lock(&mheap_.speciallock)
	s := (*specialprofile)(mheap_.specialprofilealloc.alloc())
	unlock(&mheap_.speciallock)

	// 设置特殊记录的类型和关联的桶
	s.special.kind = _KindSpecialProfile
	s.b = b

	// 尝试将特殊记录添加到对象中，如果已存在则抛出异常
	if !addspecial(p, &s.special) {
		throw("setprofilebucket: profile already set")
	}
}

// specialReachable tracks whether an object is reachable on the next
// GC cycle. This is used by testing.
// specialReachable 用于跟踪对象在下一个 GC 周期是否可达。
// 这个类型主要用于测试目的。
type specialReachable struct {
	special   special // 基础特殊记录结构
	done      bool    // 标记是否已完成可达性检查
	reachable bool    // 标记对象是否可达
}

// specialPinCounter tracks whether an object is pinned multiple times.
// specialPinCounter 用于跟踪对象被固定(pin)的次数。
type specialPinCounter struct {
	special special // 基础特殊记录结构
	counter uintptr // 记录对象被固定的次数
}

// specialsIter helps iterate over specials lists.
// specialsIter 用于帮助遍历特殊记录列表。
type specialsIter struct {
	pprev **special // 指向前一个特殊记录的指针的指针
	s     *special  // 当前特殊记录
}

// newSpecialsIter creates a new iterator for traversing the specials list of a span.
// newSpecialsIter 创建一个新的迭代器用于遍历 span 的特殊记录列表
func newSpecialsIter(span *mspan) specialsIter {
	return specialsIter{&span.specials, span.specials}
}

// valid returns whether the iterator is pointing to a valid special.
// valid 返回迭代器是否指向一个有效的特殊记录
func (i *specialsIter) valid() bool {
	return i.s != nil
}

// next advances the iterator to the next special in the list.
// next 将迭代器前进到列表中的下一个特殊记录
func (i *specialsIter) next() {
	i.pprev = &i.s.next
	i.s = *i.pprev
}

// unlinkAndNext removes the current special from the list and moves
// the iterator to the next special. It returns the unlinked special.
// unlinkAndNext 从列表中移除当前特殊记录，并将迭代器移动到下一个特殊记录。
// 它返回被移除的特殊记录。
func (i *specialsIter) unlinkAndNext() *special {
	// 保存当前特殊记录
	cur := i.s
	// 将迭代器移动到下一个特殊记录
	i.s = cur.next
	// 更新前一个特殊记录的 next 指针，跳过当前记录
	*i.pprev = i.s
	// 返回被移除的特殊记录
	return cur
}

// freeSpecial performs any cleanup on special s and deallocates it.
// s must already be unlinked from the specials list.
// freeSpecial 对特殊记录 s 执行清理并释放它。
// s 必须已经从特殊记录列表中移除。
func freeSpecial(s *special, p unsafe.Pointer, size uintptr) {
	switch s.kind {
	case _KindSpecialFinalizer:
		// 处理终结器类型的特殊记录
		sf := (*specialfinalizer)(unsafe.Pointer(s))
		// 将终结器加入队列
		queuefinalizer(p, sf.fn, sf.nret, sf.fint, sf.ot)
		// 获取特殊记录锁
		lock(&mheap_.speciallock)
		// 释放终结器特殊记录的内存
		mheap_.specialfinalizeralloc.free(unsafe.Pointer(sf))
		unlock(&mheap_.speciallock)
	case _KindSpecialWeakHandle:
		// 处理弱引用句柄类型的特殊记录
		sw := (*specialWeakHandle)(unsafe.Pointer(s))
		// 将句柄值设为0
		sw.handle.Store(0)
		// 获取特殊记录锁
		lock(&mheap_.speciallock)
		// 释放弱引用句柄特殊记录的内存
		mheap_.specialWeakHandleAlloc.free(unsafe.Pointer(s))
		unlock(&mheap_.speciallock)
	case _KindSpecialProfile:
		// 处理性能分析类型的特殊记录
		sp := (*specialprofile)(unsafe.Pointer(s))
		// 释放性能分析数据
		mProf_Free(sp.b, size)
		// 获取特殊记录锁
		lock(&mheap_.speciallock)
		// 释放性能分析特殊记录的内存
		mheap_.specialprofilealloc.free(unsafe.Pointer(sp))
		unlock(&mheap_.speciallock)
	case _KindSpecialReachable:
		// 处理可达性类型的特殊记录
		sp := (*specialReachable)(unsafe.Pointer(s))
		// 标记为已完成
		sp.done = true
		// The creator frees these.
		// 由创建者负责释放这些记录
	case _KindSpecialPinCounter:
		// 处理固定计数器类型的特殊记录
		lock(&mheap_.speciallock)
		// 释放固定计数器特殊记录的内存
		mheap_.specialPinCounterAlloc.free(unsafe.Pointer(s))
		unlock(&mheap_.speciallock)
	default:
		// 处理未知类型的特殊记录
		throw("bad special kind")
		panic("not reached")
	}
}

// gcBits is an alloc/mark bitmap. This is always used as gcBits.x.
// gcBits 是一个分配/标记位图。它总是通过 gcBits.x 来使用。
type gcBits struct {
	_ sys.NotInHeap // 标记此类型不在堆上分配
	x uint8         // 实际的位图数据
}

// bytep returns a pointer to the n'th byte of b.
// bytep 返回指向 b 的第 n 个字节的指针。
func (b *gcBits) bytep(n uintptr) *uint8 {
	return addb(&b.x, n) // 通过偏移量 n 计算并返回对应字节的指针
}

// bitp returns a pointer to the byte containing bit n and a mask for
// selecting that bit from *bytep.
// bitp 返回包含第 n 位的字节的指针，以及用于从 *bytep 中选择该位的掩码。
func (b *gcBits) bitp(n uintptr) (bytep *uint8, mask uint8) {
	return b.bytep(n / 8), 1 << (n % 8) // 计算字节位置和位掩码
}

// gcBitsChunkBytes is the size of a gcBitsArena in bytes.
// gcBitsChunkBytes 是 gcBitsArena 的大小，以字节为单位。
const gcBitsChunkBytes = uintptr(64 << 10)

// gcBitsHeaderBytes is the size of the gcBitsHeader structure.
// gcBitsHeaderBytes 是 gcBitsHeader 结构体的大小。
const gcBitsHeaderBytes = unsafe.Sizeof(gcBitsHeader{})

// gcBitsHeader is a header for a gcBitsArena.
// gcBitsHeader 是 gcBitsArena 的头部结构。
type gcBitsHeader struct {
	free uintptr // free is the index into bits of the next free byte.
	next uintptr // *gcBits triggers recursive type bug. (issue 14620)
	// free 是下一个空闲字节在位图中的索引。
	// next 指向下一个 gcBitsArena，使用 uintptr 而不是 *gcBits 是为了避免递归类型错误（issue 14620）。
}

// gcBitsArena is an arena for allocating gcBits.
// gcBitsArena 是用于分配 gcBits 的 arena。
type gcBitsArena struct {
	_ sys.NotInHeap // 标记此类型不在堆上分配
	// gcBitsHeader // side step recursive type bug (issue 14620) by including fields by hand.
	free uintptr // free is the index into bits of the next free byte; read/write atomically
	next *gcBitsArena
	bits [gcBitsChunkBytes - gcBitsHeaderBytes]gcBits
	// free 是下一个空闲字节在位图中的索引，可以原子地读写。
	// next 指向下一个 gcBitsArena。
	// bits 是实际的 gcBits 数组，大小为 gcBitsChunkBytes 减去头部大小。
}

// gcBitsArenas manages a collection of gcBitsArena.
// gcBitsArenas 管理一组 gcBitsArena。
var gcBitsArenas struct {
	lock     mutex
	free     *gcBitsArena
	next     *gcBitsArena // Read atomically. Write atomically under lock.
	current  *gcBitsArena
	previous *gcBitsArena
	// lock 用于保护对 gcBitsArenas 的并发访问。
	// free 指向空闲的 gcBitsArena 列表。
	// next 指向下一个可用的 gcBitsArena，可以原子地读取，但在锁下写入。
	// current 指向当前正在使用的 gcBitsArena。
	// previous 指向前一个 gcBitsArena。
}

// tryAlloc allocates from b or returns nil if b does not have enough room.
// This is safe to call concurrently.
// tryAlloc 从 b 中分配内存，如果 b 没有足够的空间则返回 nil。
// 这个函数可以安全地并发调用。
func (b *gcBitsArena) tryAlloc(bytes uintptr) *gcBits {
	// Check if the arena is nil or if there's not enough space.
	// 检查 arena 是否为 nil 或者是否有足够的空间。
	if b == nil || atomic.Loaduintptr(&b.free)+bytes > uintptr(len(b.bits)) {
		return nil
	}
	// Try to allocate from this block.
	// 尝试从这个块中分配内存。
	end := atomic.Xadduintptr(&b.free, bytes)
	if end > uintptr(len(b.bits)) {
		return nil
	}
	// There was enough room.
	// 有足够的空间。
	start := end - bytes
	return &b.bits[start]
}

// newMarkBits returns a pointer to 8 byte aligned bytes
// to be used for a span's mark bits.
// newMarkBits 返回一个指向 8 字节对齐的字节的指针，
// 用于 span 的标记位。
func newMarkBits(nelems uintptr) *gcBits {
	// Calculate the number of 64-bit blocks needed and the total bytes required.
	// 计算需要的 64 位块数和所需的总字节数。
	blocksNeeded := (nelems + 63) / 64
	bytesNeeded := blocksNeeded * 8

	// Try directly allocating from the current head arena.
	// 尝试直接从当前头部 arena 分配。
	head := (*gcBitsArena)(atomic.Loadp(unsafe.Pointer(&gcBitsArenas.next)))
	if p := head.tryAlloc(bytesNeeded); p != nil {
		return p
	}

	// There's not enough room in the head arena. We may need to
	// allocate a new arena.
	// 头部 arena 没有足够的空间。我们可能需要分配一个新的 arena。
	lock(&gcBitsArenas.lock)
	// Try the head arena again, since it may have changed. Now
	// that we hold the lock, the list head can't change, but its
	// free position still can.
	// 再次尝试头部 arena，因为它可能已经改变。现在我们持有锁，
	// 列表头部不能改变，但其空闲位置仍然可以改变。
	if p := gcBitsArenas.next.tryAlloc(bytesNeeded); p != nil {
		unlock(&gcBitsArenas.lock)
		return p
	}

	// Allocate a new arena. This may temporarily drop the lock.
	// 分配一个新的 arena。这可能会暂时释放锁。
	fresh := newArenaMayUnlock()
	// If newArenaMayUnlock dropped the lock, another thread may
	// have put a fresh arena on the "next" list. Try allocating
	// from next again.
	// 如果 newArenaMayUnlock 释放了锁，另一个线程可能已经
	// 在 "next" 列表上放置了一个新的 arena。再次尝试从 next 分配。
	if p := gcBitsArenas.next.tryAlloc(bytesNeeded); p != nil {
		// Put fresh back on the free list.
		// TODO: Mark it "already zeroed"
		// 将新的 arena 放回空闲列表。
		// TODO: 标记它为 "already zeroed"
		fresh.next = gcBitsArenas.free
		gcBitsArenas.free = fresh
		unlock(&gcBitsArenas.lock)
		return p
	}

	// Allocate from the fresh arena. We haven't linked it in yet, so
	// this cannot race and is guaranteed to succeed.
	// 从新的 arena 分配。我们还没有将其链接进来，所以
	// 这不会发生竞争，并且保证会成功。
	p := fresh.tryAlloc(bytesNeeded)
	if p == nil {
		throw("markBits overflow")
	}

	// Add the fresh arena to the "next" list.
	// 将新的 arena 添加到 "next" 列表。
	fresh.next = gcBitsArenas.next
	atomic.StorepNoWB(unsafe.Pointer(&gcBitsArenas.next), unsafe.Pointer(fresh))

	unlock(&gcBitsArenas.lock)
	return p
}

// newAllocBits returns a pointer to 8 byte aligned bytes
// to be used for this span's alloc bits.
// newAllocBits is used to provide newly initialized spans
// allocation bits. For spans not being initialized the
// mark bits are repurposed as allocation bits when
// the span is swept.
// newAllocBits 返回一个 8 字节对齐的字节指针，用于此 span 的分配位。
// newAllocBits 用于为新初始化的 span 提供分配位。
// 对于未初始化的 span，当 span 被清扫时，标记位会被重新用作分配位。
func newAllocBits(nelems uintptr) *gcBits {
	return newMarkBits(nelems)
}

// nextMarkBitArenaEpoch establishes a new epoch for the arenas
// holding the mark bits. The arenas are named relative to the
// current GC cycle which is demarcated by the call to finishweep_m.
//
// All current spans have been swept.
// During that sweep each span allocated room for its gcmarkBits in
// gcBitsArenas.next block. gcBitsArenas.next becomes the gcBitsArenas.current
// where the GC will mark objects and after each span is swept these bits
// will be used to allocate objects.
// gcBitsArenas.current becomes gcBitsArenas.previous where the span's
// gcAllocBits live until all the spans have been swept during this GC cycle.
// The span's sweep extinguishes all the references to gcBitsArenas.previous
// by pointing gcAllocBits into the gcBitsArenas.current.
// The gcBitsArenas.previous is released to the gcBitsArenas.free list.

// nextMarkBitArenaEpoch 为持有标记位的 arenas 建立一个新的纪元。
// arenas 的命名是相对于当前 GC 周期的，该周期由 finishweep_m 调用界定。
//
// 所有当前的 spans 都已被清扫。
// 在清扫过程中，每个 span 在 gcBitsArenas.next 块中为其 gcmarkBits 分配了空间。
// gcBitsArenas.next 变成 gcBitsArenas.current，GC 将在这里标记对象，
// 并且在每个 span 被清扫后，这些位将被用于分配对象。
// gcBitsArenas.current 变成 gcBitsArenas.previous，span 的 gcAllocBits 将在这里存活，
// 直到在此 GC 周期中所有 spans 都被清扫。
// span 的清扫通过将 gcAllocBits 指向 gcBitsArenas.current 来消除对 gcBitsArenas.previous 的所有引用。
// gcBitsArenas.previous 被释放到 gcBitsArenas.free 列表中。
func nextMarkBitArenaEpoch() {
	// Lock the gcBitsArenas to prevent concurrent access
	// 锁定 gcBitsArenas 以防止并发访问
	lock(&gcBitsArenas.lock)

	// Handle the previous arenas if they exist
	// 如果存在前一个 arenas，则处理它们
	if gcBitsArenas.previous != nil {
		if gcBitsArenas.free == nil {
			// If free list is empty, directly add previous to free list
			// 如果空闲列表为空，直接将 previous 添加到空闲列表
			gcBitsArenas.free = gcBitsArenas.previous
		} else {
			// Find end of previous arenas.
			// 找到前一个 arenas 的末尾
			last := gcBitsArenas.previous
			for last = gcBitsArenas.previous; last.next != nil; last = last.next {
			}
			// Append free list to the end of previous arenas
			// 将空闲列表追加到前一个 arenas 的末尾
			last.next = gcBitsArenas.free
			// Make previous arenas the new free list
			// 将前一个 arenas 设为新的空闲列表
			gcBitsArenas.free = gcBitsArenas.previous
		}
	}

	// Update arena pointers for the new epoch
	// 更新新纪元的 arena 指针
	gcBitsArenas.previous = gcBitsArenas.current
	gcBitsArenas.current = gcBitsArenas.next
	// Clear next pointer - new arenas will be allocated as needed
	// 清除 next 指针 - 新的 arenas 将在需要时分配
	atomic.StorepNoWB(unsafe.Pointer(&gcBitsArenas.next), nil) // newMarkBits calls newArena when needed

	// Release the lock
	// 释放锁
	unlock(&gcBitsArenas.lock)
}

// newArenaMayUnlock allocates and zeroes a gcBits arena.
// The caller must hold gcBitsArena.lock. This may temporarily release it.
// newArenaMayUnlock 分配并清零一个 gcBits arena。
// 调用者必须持有 gcBitsArena.lock。该函数可能会临时释放这个锁。
func newArenaMayUnlock() *gcBitsArena {
	var result *gcBitsArena
	if gcBitsArenas.free == nil {
		// 如果没有可用的空闲 arena，需要分配新的内存
		unlock(&gcBitsArenas.lock)
		result = (*gcBitsArena)(sysAlloc(gcBitsChunkBytes, &memstats.gcMiscSys))
		if result == nil {
			throw("runtime: cannot allocate memory")
		}
		lock(&gcBitsArenas.lock)
	} else {
		// 从空闲列表中获取一个 arena 并清零
		result = gcBitsArenas.free
		gcBitsArenas.free = gcBitsArenas.free.next
		memclrNoHeapPointers(unsafe.Pointer(result), gcBitsChunkBytes)
	}
	result.next = nil
	// If result.bits is not 8 byte aligned adjust index so
	// that &result.bits[result.free] is 8 byte aligned.
	// 如果 result.bits 不是 8 字节对齐的，调整索引以使
	// &result.bits[result.free] 是 8 字节对齐的。
	if unsafe.Offsetof(gcBitsArena{}.bits)&7 == 0 {
		result.free = 0
	} else {
		result.free = 8 - (uintptr(unsafe.Pointer(&result.bits[0])) & 7)
	}
	return result
}
