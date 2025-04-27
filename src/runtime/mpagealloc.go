// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Page allocator.
//
// The page allocator manages mapped pages (defined by pageSize, NOT
// physPageSize) for allocation and re-use. It is embedded into mheap.
//
// Pages are managed using a bitmap that is sharded into chunks.
// In the bitmap, 1 means in-use, and 0 means free. The bitmap spans the
// process's address space. Chunks are managed in a sparse-array-style structure
// similar to mheap.arenas, since the bitmap may be large on some systems.
//
// The bitmap is efficiently searched by using a radix tree in combination
// with fast bit-wise intrinsics. Allocation is performed using an address-ordered
// first-fit approach.
//
// Each entry in the radix tree is a summary that describes three properties of
// a particular region of the address space: the number of contiguous free pages
// at the start and end of the region it represents, and the maximum number of
// contiguous free pages found anywhere in that region.
//
// Each level of the radix tree is stored as one contiguous array, which represents
// a different granularity of subdivision of the processes' address space. Thus, this
// radix tree is actually implicit in these large arrays, as opposed to having explicit
// dynamically-allocated pointer-based node structures. Naturally, these arrays may be
// quite large for system with large address spaces, so in these cases they are mapped
// into memory as needed. The leaf summaries of the tree correspond to a bitmap chunk.
//
// The root level (referred to as L0 and index 0 in pageAlloc.summary) has each
// summary represent the largest section of address space (16 GiB on 64-bit systems),
// with each subsequent level representing successively smaller subsections until we
// reach the finest granularity at the leaves, a chunk.
//
// More specifically, each summary in each level (except for leaf summaries)
// represents some number of entries in the following level. For example, each
// summary in the root level may represent a 16 GiB region of address space,
// and in the next level there could be 8 corresponding entries which represent 2
// GiB subsections of that 16 GiB region, each of which could correspond to 8
// entries in the next level which each represent 256 MiB regions, and so on.
//
// Thus, this design only scales to heaps so large, but can always be extended to
// larger heaps by simply adding levels to the radix tree, which mostly costs
// additional virtual address space. The choice of managing large arrays also means
// that a large amount of virtual address space may be reserved by the runtime.

// 页面分配器。
//
// 页面分配器管理映射页面（由 pageSize 定义，而不是 physPageSize）用于分配和重用。
// 它被嵌入到 mheap 中。
//
// 页面使用位图进行管理，位图被分片成多个块。
// 在位图中，1 表示正在使用，0 表示空闲。位图跨越进程的地址空间。
// 块使用类似 mheap.arenas 的稀疏数组结构进行管理，因为位图在某些系统上可能很大。
//
// 位图通过结合基数树和快速位操作内部函数进行高效搜索。
// 分配使用地址有序的首次适应方法进行。
//
// 基数树中的每个条目都是一个摘要，描述了地址空间特定区域的三个属性：
// 该区域开始和结束处的连续空闲页面数量，以及在该区域任何位置找到的最大连续空闲页面数量。
//
// 基数树的每一层都存储为一个连续数组，表示进程地址空间的不同粒度细分。
// 因此，这个基数树实际上隐含在这些大数组中，而不是使用显式的动态分配的基于指针的节点结构。
// 自然地，这些数组对于具有大地址空间的系统可能相当大，所以在这些情况下，
// 它们会根据需要映射到内存中。树的叶节点摘要对应于位图块。
//
// 根级别（在 pageAlloc.summary 中称为 L0 和索引 0）的每个摘要表示地址空间的最大部分
//（在 64 位系统上为 16 GiB），每个后续级别表示越来越小的子部分，直到我们在叶节点达到最细粒度，即一个块。
//
// 更具体地说，每个级别中的每个摘要（除了叶节点摘要）表示下一级别中的一些条目。
// 例如，根级别的每个摘要可能表示 16 GiB 的地址空间区域，在下一级别可能有 8 个对应条目，
// 每个表示该 16 GiB 区域的 2 GiB 子部分，每个子部分可能对应下一级别中的 8 个条目，
// 每个表示 256 MiB 区域，以此类推。
//
// 因此，这种设计只能扩展到一定大小的堆，但始终可以通过简单地向基数树添加级别来扩展到更大的堆，
// 这主要需要额外的虚拟地址空间。选择管理大型数组也意味着运行时可能会保留大量的虚拟地址空间。

package runtime

import (
	"internal/runtime/atomic"
	"unsafe"
)

const (
	// The size of a bitmap chunk, i.e. the amount of bits (that is, pages) to consider
	// in the bitmap at once.
	pallocChunkPages    = 1 << logPallocChunkPages
	pallocChunkBytes    = pallocChunkPages * pageSize // 512 x 8kb = 4m
	logPallocChunkPages = 9
	logPallocChunkBytes = logPallocChunkPages + pageShift

	// 位图块的大小，即一次要考虑的位数（即页面数）。
	// pallocChunkPages 表示每个块包含的页面数，为 2^9 = 512 页
	// pallocChunkBytes 表示每个块的字节大小，为 512 页 * 8KB = 4MB
	// logPallocChunkPages 是 pallocChunkPages 的以 2 为底的对数，值为 9
	// logPallocChunkBytes 是 pallocChunkBytes 的以 2 为底的对数，值为 9 + 13 = 22

	// The number of radix bits for each level.
	//
	// The value of 3 is chosen such that the block of summaries we need to scan at
	// each level fits in 64 bytes (2^3 summaries * 8 bytes per summary), which is
	// close to the L1 cache line width on many systems. Also, a value of 3 fits 4 tree
	// levels perfectly into the 21-bit pallocBits summary field at the root level.
	//
	// The following equation explains how each of the constants relate:
	// summaryL0Bits + (summaryLevels-1)*summaryLevelBits + logPallocChunkBytes = heapAddrBits
	//
	// summaryLevels is an architecture-dependent value defined in mpagealloc_*.go.
	summaryLevelBits = 3
	summaryL0Bits    = heapAddrBits - logPallocChunkBytes - (summaryLevels-1)*summaryLevelBits

	// 每一层的基数位数。
	//
	// 选择值 3 的原因是：我们需要在每个级别扫描的摘要块都能放入 64 字节
	//（2^3 个摘要 * 每个摘要 8 字节），这接近许多系统的 L1 缓存行宽度。
	// 另外，值 3 使得 4 个树级别完美地适应根级别的 21 位 pallocBits 摘要字段。
	//
	// 以下等式解释了各个常量之间的关系：
	// summaryL0Bits + (summaryLevels-1)*summaryLevelBits + logPallocChunkBytes = heapAddrBits
	//
	// summaryLevels 是在 mpagealloc_*.go 中定义的与架构相关的值。
	// summaryLevelBits 表示每个级别的基数位数，值为 3
	// summaryL0Bits 表示根级别的位数，通过上述等式计算得出

	// pallocChunksL2Bits is the number of bits of the chunk index number
	// covered by the second level of the chunks map.
	//
	// See (*pageAlloc).chunks for more details. Update the documentation
	// there should this change.
	pallocChunksL2Bits  = heapAddrBits - logPallocChunkBytes - pallocChunksL1Bits
	pallocChunksL1Shift = pallocChunksL2Bits

	// pallocChunksL2Bits 是块索引号中被 chunks 映射的第二层覆盖的位数。
	//
	// 有关更多详细信息，请参见 (*pageAlloc).chunks。如果这里发生变化，请更新那里的文档。
	// pallocChunksL2Bits 通过从总地址位数中减去块字节数的对数和第一层位数计算得出
	// pallocChunksL1Shift 用于计算第一层索引，值等于 pallocChunksL2Bits
)

// maxSearchAddr returns the maximum searchAddr value, which indicates
// that the heap has no free space.
//
// This function exists just to make it clear that this is the maximum address
// for the page allocator's search space. See maxOffAddr for details.
//
// It's a function (rather than a variable) because it needs to be
// usable before package runtime's dynamic initialization is complete.
// See #51913 for details.
//
// maxSearchAddr 返回 searchAddr 的最大值，该值表示堆中没有可用空间。
//
// 这个函数的存在只是为了明确这是页面分配器搜索空间的最大地址。
// 有关详细信息，请参见 maxOffAddr。
//
// 它被实现为一个函数（而不是变量）是因为它需要在 runtime 包的动态初始化完成之前就能使用。
// 有关详细信息，请参见 #51913。
func maxSearchAddr() offAddr { return maxOffAddr }

// Global chunk index.
//
// Represents an index into the leaf level of the radix tree.
// Similar to arenaIndex, except instead of arenas, it divides the address
// space into chunks.
type chunkIdx uint

// chunkIndex returns the global index of the palloc chunk containing the
// pointer p.
func chunkIndex(p uintptr) chunkIdx {
	return chunkIdx((p - arenaBaseOffset) / pallocChunkBytes)
}

// chunkBase returns the base address of the palloc chunk at index ci.
func chunkBase(ci chunkIdx) uintptr {
	return uintptr(ci)*pallocChunkBytes + arenaBaseOffset
}

// chunkPageIndex computes the index of the page that contains p,
// relative to the chunk which contains p.
func chunkPageIndex(p uintptr) uint {
	return uint(p % pallocChunkBytes / pageSize)
}

// l1 returns the index into the first level of (*pageAlloc).chunks.
func (i chunkIdx) l1() uint {
	if pallocChunksL1Bits == 0 {
		// Let the compiler optimize this away if there's no
		// L1 map.
		return 0
	} else {
		return uint(i) >> pallocChunksL1Shift
	}
}

// l2 returns the index into the second level of (*pageAlloc).chunks.
func (i chunkIdx) l2() uint {
	if pallocChunksL1Bits == 0 {
		return uint(i)
	} else {
		return uint(i) & (1<<pallocChunksL2Bits - 1)
	}
}

// offAddrToLevelIndex converts an address in the offset address space
// to the index into summary[level] containing addr.
func offAddrToLevelIndex(level int, addr offAddr) int {
	return int((addr.a - arenaBaseOffset) >> levelShift[level])
}

// levelIndexToOffAddr converts an index into summary[level] into
// the corresponding address in the offset address space.
func levelIndexToOffAddr(level, idx int) offAddr {
	return offAddr{(uintptr(idx) << levelShift[level]) + arenaBaseOffset}
}

// addrsToSummaryRange converts base and limit pointers into a range
// of entries for the given summary level.
//
// The returned range is inclusive on the lower bound and exclusive on
// the upper bound.
func addrsToSummaryRange(level int, base, limit uintptr) (lo int, hi int) {
	// This is slightly more nuanced than just a shift for the exclusive
	// upper-bound. Note that the exclusive upper bound may be within a
	// summary at this level, meaning if we just do the obvious computation
	// hi will end up being an inclusive upper bound. Unfortunately, just
	// adding 1 to that is too broad since we might be on the very edge
	// of a summary's max page count boundary for this level
	// (1 << levelLogPages[level]). So, make limit an inclusive upper bound
	// then shift, then add 1, so we get an exclusive upper bound at the end.
	lo = int((base - arenaBaseOffset) >> levelShift[level])
	hi = int(((limit-1)-arenaBaseOffset)>>levelShift[level]) + 1
	return
}

// blockAlignSummaryRange aligns indices into the given level to that
// level's block width (1 << levelBits[level]). It assumes lo is inclusive
// and hi is exclusive, and so aligns them down and up respectively.
func blockAlignSummaryRange(level int, lo, hi int) (int, int) {
	e := uintptr(1) << levelBits[level]
	return int(alignDown(uintptr(lo), e)), int(alignUp(uintptr(hi), e))
}

type pageAlloc struct {
	// Radix tree of summaries.
	//
	// Each slice's cap represents the whole memory reservation.
	// Each slice's len reflects the allocator's maximum known
	// mapped heap address for that level.
	//
	// The backing store of each summary level is reserved in init
	// and may or may not be committed in grow (small address spaces
	// may commit all the memory in init).
	//
	// The purpose of keeping len <= cap is to enforce bounds checks
	// on the top end of the slice so that instead of an unknown
	// runtime segmentation fault, we get a much friendlier out-of-bounds
	// error.
	//
	// To iterate over a summary level, use inUse to determine which ranges
	// are currently available. Otherwise one might try to access
	// memory which is only Reserved which may result in a hard fault.
	//
	// We may still get segmentation faults < len since some of that
	// memory may not be committed yet.
	// 摘要的基数树。
	//
	// 每个切片的 cap 表示整个内存预留。
	// 每个切片的 len 反映了分配器在该级别已知的最大映射堆地址。
	//
	// 每个摘要级别的后备存储都在 init 中预留，
	// 并且可能在 grow 中提交（小地址空间可能在 init 中提交所有内存）。
	//
	// 保持 len <= cap 的目的是在切片的上端强制执行边界检查，
	// 这样我们就能得到一个更友好的越界错误，而不是未知的运行时段错误。
	//
	// 要遍历摘要级别，使用 inUse 来确定当前可用的范围。
	// 否则可能会尝试访问仅预留的内存，这可能导致硬故障。
	//
	// 由于某些内存可能尚未提交，我们仍然可能获得 < len 的段错误。
	summary [summaryLevels][]pallocSum

	// chunks is a slice of bitmap chunks.
	//
	// The total size of chunks is quite large on most 64-bit platforms
	// (O(GiB) or more) if flattened, so rather than making one large mapping
	// (which has problems on some platforms, even when PROT_NONE) we use a
	// two-level sparse array approach similar to the arena index in mheap.
	//
	// To find the chunk containing a memory address `a`, do:
	//   chunkOf(chunkIndex(a))
	//
	// Below is a table describing the configuration for chunks for various
	// heapAddrBits supported by the runtime.
	//
	// heapAddrBits | L1 Bits | L2 Bits | L2 Entry Size
	// ------------------------------------------------
	// 32           | 0       | 10      | 128 KiB
	// 33 (iOS)     | 0       | 11      | 256 KiB
	// 48           | 13      | 13      | 1 MiB
	//
	// There's no reason to use the L1 part of chunks on 32-bit, the
	// address space is small so the L2 is small. For platforms with a
	// 48-bit address space, we pick the L1 such that the L2 is 1 MiB
	// in size, which is a good balance between low granularity without
	// making the impact on BSS too high (note the L1 is stored directly
	// in pageAlloc).
	//
	// To iterate over the bitmap, use inUse to determine which ranges
	// are currently available. Otherwise one might iterate over unused
	// ranges.
	//
	// Protected by mheapLock.
	//
	// TODO(mknyszek): Consider changing the definition of the bitmap
	// such that 1 means free and 0 means in-use so that summaries and
	// the bitmaps align better on zero-values.
	//
	// chunks 是位图块的切片。
	//
	// 在大多数64位平台上，如果展平的话，chunks的总大小相当大（O(GiB)或更多），
	// 因此我们使用类似于mheap中arena索引的两级稀疏数组方法，而不是创建一个大的映射
	//（这在某些平台上会有问题，即使使用PROT_NONE）。
	//
	// 要找到包含内存地址`a`的块，执行：
	//   chunkOf(chunkIndex(a))
	//
	// 下表描述了运行时支持的各种heapAddrBits的chunks配置。
	//
	// heapAddrBits | L1位数 | L2位数 | L2条目大小
	// ------------------------------------------------
	// 32           | 0      | 10     | 128 KiB
	// 33 (iOS)     | 0      | 11     | 256 KiB
	// 48           | 13     | 13     | 1 MiB
	//
	// 在32位系统上没有理由使用chunks的L1部分，因为地址空间较小，所以L2也较小。
	// 对于具有48位地址空间的平台，我们选择L1使得L2大小为1 MiB，
	// 这是在低粒度和不过度影响BSS之间的良好平衡（注意L1直接存储在pageAlloc中）。
	//
	// 要遍历位图，使用inUse来确定当前可用的范围。否则可能会遍历未使用的范围。
	//
	// 由mheapLock保护。
	//
	// TODO(mknyszek): 考虑更改位图的定义，使1表示空闲，0表示使用中，
	// 这样摘要和位图在零值上能更好地对齐。
	chunks [1 << pallocChunksL1Bits]*[1 << pallocChunksL2Bits]pallocData

	// The address to start an allocation search with. It must never
	// point to any memory that is not contained in inUse, i.e.
	// inUse.contains(searchAddr.addr()) must always be true. The one
	// exception to this rule is that it may take on the value of
	// maxOffAddr to indicate that the heap is exhausted.
	//
	// We guarantee that all valid heap addresses below this value
	// are allocated and not worth searching.
	//
	// 用于开始分配搜索的地址。它绝不能指向不在 inUse 范围内的任何内存，
	// 即 inUse.contains(searchAddr.addr()) 必须始终为 true。
	// 这个规则的一个例外是它可以取 maxOffAddr 的值来表示堆已耗尽。
	//
	// 我们保证所有低于此值的有效堆地址都已分配，不值得搜索。
	searchAddr offAddr

	// start and end represent the chunk indices
	// which pageAlloc knows about. It assumes
	// chunks in the range [start, end) are
	// currently ready to use.
	//
	// start 和 end 表示 pageAlloc 已知的块索引。
	// 它假设范围 [start, end) 内的块当前可以使用。
	start, end chunkIdx

	// inUse is a slice of ranges of address space which are
	// known by the page allocator to be currently in-use (passed
	// to grow).
	//
	// We care much more about having a contiguous heap in these cases
	// and take additional measures to ensure that, so in nearly all
	// cases this should have just 1 element.
	//
	// All access is protected by the mheapLock.
	//
	// inUse 是地址空间范围的切片，页面分配器知道这些范围当前正在使用中
	//（传递给 grow 函数）。
	//
	// 在这些情况下，我们更关心拥有连续的堆，并采取额外的措施来确保这一点，
	// 因此在几乎所有情况下，这应该只有一个元素。
	//
	// 所有访问都由 mheapLock 保护。
	inUse addrRanges

	// scav stores the scavenger state.
	// scav 存储了内存回收器的状态
	scav struct {
		// index is an efficient index of chunks that have pages available to
		// scavenge.
		// index 是一个高效的索引，用于标记哪些内存块有可回收的页面
		index scavengeIndex

		// releasedBg is the amount of memory released in the background this
		// scavenge cycle.
		// releasedBg 是在当前回收周期中通过后台回收释放的内存量
		releasedBg atomic.Uintptr

		// releasedEager is the amount of memory released eagerly this scavenge
		// cycle.
		// releasedEager 是在当前回收周期中通过主动回收释放的内存量
		releasedEager atomic.Uintptr
	}

	// mheap_.lock. This level of indirection makes it possible
	// to test pageAlloc independently of the runtime allocator.
	// mheap_.lock。这种间接引用使得可以独立于运行时分配器测试 pageAlloc。
	mheapLock *mutex

	// sysStat is the runtime memstat to update when new system
	// memory is committed by the pageAlloc for allocation metadata.
	// sysStat 是运行时内存统计，当 pageAlloc 为分配元数据提交新的系统内存时需要更新它。
	sysStat *sysMemStat

	// summaryMappedReady is the number of bytes mapped in the Ready state
	// in the summary structure. Used only for testing currently.
	//
	// Protected by mheapLock.
	// summaryMappedReady 是摘要结构中处于 Ready 状态的已映射字节数。
	// 目前仅用于测试。
	//
	// 由 mheapLock 保护。
	summaryMappedReady uintptr

	// chunkHugePages indicates whether page bitmap chunks should be backed
	// by huge pages.
	// chunkHugePages 指示页面位图块是否应该由大页支持。
	chunkHugePages bool

	// Whether or not this struct is being used in tests.
	// 指示此结构体是否在测试中使用。
	test bool
}

func (p *pageAlloc) init(mheapLock *mutex, sysStat *sysMemStat, test bool) {
	if levelLogPages[0] > logMaxPackedValue {
		// We can't represent 1<<levelLogPages[0] pages, the maximum number
		// of pages we need to represent at the root level, in a summary, which
		// is a big problem. Throw.
		print("runtime: root level max pages = ", 1<<levelLogPages[0], "\n")
		print("runtime: summary max pages = ", maxPackedValue, "\n")
		throw("root level max pages doesn't fit in summary")
	}
	p.sysStat = sysStat

	// Initialize p.inUse.
	p.inUse.init(sysStat)

	// System-dependent initialization.
	p.sysInit(test)

	// Start with the searchAddr in a state indicating there's no free memory.
	p.searchAddr = maxSearchAddr()

	// Set the mheapLock.
	p.mheapLock = mheapLock

	// Initialize the scavenge index.
	p.summaryMappedReady += p.scav.index.init(test, sysStat)

	// Set if we're in a test.
	p.test = test
}

// tryChunkOf returns the bitmap data for the given chunk.
//
// Returns nil if the chunk data has not been mapped.
func (p *pageAlloc) tryChunkOf(ci chunkIdx) *pallocData {
	l2 := p.chunks[ci.l1()]
	if l2 == nil {
		return nil
	}
	return &l2[ci.l2()]
}

// chunkOf returns the chunk at the given chunk index.
//
// The chunk index must be valid or this method may throw.
func (p *pageAlloc) chunkOf(ci chunkIdx) *pallocData {
	return &p.chunks[ci.l1()][ci.l2()]
}

// grow sets up the metadata for the address range [base, base+size).
// It may allocate metadata, in which case *p.sysStat will be updated.
//
// p.mheapLock must be held.
func (p *pageAlloc) grow(base, size uintptr) {
	assertLockHeld(p.mheapLock)

	// Round up to chunks, since we can't deal with increments smaller
	// than chunks. Also, sysGrow expects aligned values.
	limit := alignUp(base+size, pallocChunkBytes)
	base = alignDown(base, pallocChunkBytes)

	// Grow the summary levels in a system-dependent manner.
	// We just update a bunch of additional metadata here.
	p.sysGrow(base, limit)

	// Grow the scavenge index.
	p.summaryMappedReady += p.scav.index.grow(base, limit, p.sysStat)

	// Update p.start and p.end.
	// If no growth happened yet, start == 0. This is generally
	// safe since the zero page is unmapped.
	firstGrowth := p.start == 0
	start, end := chunkIndex(base), chunkIndex(limit)
	if firstGrowth || start < p.start {
		p.start = start
	}
	if end > p.end {
		p.end = end
	}
	// Note that [base, limit) will never overlap with any existing
	// range inUse because grow only ever adds never-used memory
	// regions to the page allocator.
	p.inUse.add(makeAddrRange(base, limit))

	// A grow operation is a lot like a free operation, so if our
	// chunk ends up below p.searchAddr, update p.searchAddr to the
	// new address, just like in free.
	if b := (offAddr{base}); b.lessThan(p.searchAddr) {
		p.searchAddr = b
	}

	// Add entries into chunks, which is sparse, if needed. Then,
	// initialize the bitmap.
	//
	// Newly-grown memory is always considered scavenged.
	// Set all the bits in the scavenged bitmaps high.
	for c := chunkIndex(base); c < chunkIndex(limit); c++ {
		if p.chunks[c.l1()] == nil {
			// Create the necessary l2 entry.
			const l2Size = unsafe.Sizeof(*p.chunks[0])
			r := sysAlloc(l2Size, p.sysStat)
			if r == nil {
				throw("pageAlloc: out of memory")
			}
			if !p.test {
				// Make the chunk mapping eligible or ineligible
				// for huge pages, depending on what our current
				// state is.
				if p.chunkHugePages {
					sysHugePage(r, l2Size)
				} else {
					sysNoHugePage(r, l2Size)
				}
			}
			// Store the new chunk block but avoid a write barrier.
			// grow is used in call chains that disallow write barriers.
			*(*uintptr)(unsafe.Pointer(&p.chunks[c.l1()])) = uintptr(r)
		}
		p.chunkOf(c).scavenged.setRange(0, pallocChunkPages)
	}

	// Update summaries accordingly. The grow acts like a free, so
	// we need to ensure this newly-free memory is visible in the
	// summaries.
	p.update(base, size/pageSize, true, false)
}

// enableChunkHugePages enables huge pages for the chunk bitmap mappings (disabled by default).
//
// This function is idempotent.
//
// A note on latency: for sufficiently small heaps (<10s of GiB) this function will take constant
// time, but may take time proportional to the size of the mapped heap beyond that.
//
// The heap lock must not be held over this operation, since it will briefly acquire
// the heap lock.
//
// Must be called on the system stack because it acquires the heap lock.
//
//go:systemstack
func (p *pageAlloc) enableChunkHugePages() {
	// Grab the heap lock to turn on huge pages for new chunks and clone the current
	// heap address space ranges.
	//
	// After the lock is released, we can be sure that bitmaps for any new chunks may
	// be backed with huge pages, and we have the address space for the rest of the
	// chunks. At the end of this function, all chunk metadata should be backed by huge
	// pages.
	lock(&mheap_.lock)
	if p.chunkHugePages {
		unlock(&mheap_.lock)
		return
	}
	p.chunkHugePages = true
	var inUse addrRanges
	inUse.sysStat = p.sysStat
	p.inUse.cloneInto(&inUse)
	unlock(&mheap_.lock)

	// This might seem like a lot of work, but all these loops are for generality.
	//
	// For a 1 GiB contiguous heap, a 48-bit address space, 13 L1 bits, a palloc chunk size
	// of 4 MiB, and adherence to the default set of heap address hints, this will result in
	// exactly 1 call to sysHugePage.
	for _, r := range p.inUse.ranges {
		for i := chunkIndex(r.base.addr()).l1(); i < chunkIndex(r.limit.addr()-1).l1(); i++ {
			// N.B. We can assume that p.chunks[i] is non-nil and in a mapped part of p.chunks
			// because it's derived from inUse, which never shrinks.
			sysHugePage(unsafe.Pointer(p.chunks[i]), unsafe.Sizeof(*p.chunks[0]))
		}
	}
}

// update updates heap metadata. It must be called each time the bitmap
// is updated.
//
// If contig is true, update does some optimizations assuming that there was
// a contiguous allocation or free between addr and addr+npages. alloc indicates
// whether the operation performed was an allocation or a free.
//
// p.mheapLock must be held.
func (p *pageAlloc) update(base, npages uintptr, contig, alloc bool) {
	assertLockHeld(p.mheapLock)

	// base, limit, start, and end are inclusive.
	limit := base + npages*pageSize - 1
	sc, ec := chunkIndex(base), chunkIndex(limit)

	// Handle updating the lowest level first.
	if sc == ec {
		// Fast path: the allocation doesn't span more than one chunk,
		// so update this one and if the summary didn't change, return.
		x := p.summary[len(p.summary)-1][sc]
		y := p.chunkOf(sc).summarize()
		if x == y {
			return
		}
		p.summary[len(p.summary)-1][sc] = y
	} else if contig {
		// Slow contiguous path: the allocation spans more than one chunk
		// and at least one summary is guaranteed to change.
		summary := p.summary[len(p.summary)-1]

		// Update the summary for chunk sc.
		summary[sc] = p.chunkOf(sc).summarize()

		// Update the summaries for chunks in between, which are
		// either totally allocated or freed.
		whole := p.summary[len(p.summary)-1][sc+1 : ec]
		if alloc {
			clear(whole)
		} else {
			for i := range whole {
				whole[i] = freeChunkSum
			}
		}

		// Update the summary for chunk ec.
		summary[ec] = p.chunkOf(ec).summarize()
	} else {
		// Slow general path: the allocation spans more than one chunk
		// and at least one summary is guaranteed to change.
		//
		// We can't assume a contiguous allocation happened, so walk over
		// every chunk in the range and manually recompute the summary.
		summary := p.summary[len(p.summary)-1]
		for c := sc; c <= ec; c++ {
			summary[c] = p.chunkOf(c).summarize()
		}
	}

	// Walk up the radix tree and update the summaries appropriately.
	changed := true
	for l := len(p.summary) - 2; l >= 0 && changed; l-- {
		// Update summaries at level l from summaries at level l+1.
		changed = false

		// "Constants" for the previous level which we
		// need to compute the summary from that level.
		logEntriesPerBlock := levelBits[l+1]
		logMaxPages := levelLogPages[l+1]

		// lo and hi describe all the parts of the level we need to look at.
		lo, hi := addrsToSummaryRange(l, base, limit+1)

		// Iterate over each block, updating the corresponding summary in the less-granular level.
		for i := lo; i < hi; i++ {
			children := p.summary[l+1][i<<logEntriesPerBlock : (i+1)<<logEntriesPerBlock]
			sum := mergeSummaries(children, logMaxPages)
			old := p.summary[l][i]
			if old != sum {
				changed = true
				p.summary[l][i] = sum
			}
		}
	}
}

// allocRange marks the range of memory [base, base+npages*pageSize) as
// allocated. It also updates the summaries to reflect the newly-updated
// bitmap.
//
// Returns the amount of scavenged memory in bytes present in the
// allocated range.
//
// p.mheapLock must be held.
func (p *pageAlloc) allocRange(base, npages uintptr) uintptr {
	assertLockHeld(p.mheapLock)

	limit := base + npages*pageSize - 1
	sc, ec := chunkIndex(base), chunkIndex(limit)
	si, ei := chunkPageIndex(base), chunkPageIndex(limit)

	scav := uint(0)
	if sc == ec {
		// The range doesn't cross any chunk boundaries.
		chunk := p.chunkOf(sc)
		scav += chunk.scavenged.popcntRange(si, ei+1-si)
		chunk.allocRange(si, ei+1-si)
		p.scav.index.alloc(sc, ei+1-si)
	} else {
		// The range crosses at least one chunk boundary.
		chunk := p.chunkOf(sc)
		scav += chunk.scavenged.popcntRange(si, pallocChunkPages-si)
		chunk.allocRange(si, pallocChunkPages-si)
		p.scav.index.alloc(sc, pallocChunkPages-si)
		for c := sc + 1; c < ec; c++ {
			chunk := p.chunkOf(c)
			scav += chunk.scavenged.popcntRange(0, pallocChunkPages)
			chunk.allocAll()
			p.scav.index.alloc(c, pallocChunkPages)
		}
		chunk = p.chunkOf(ec)
		scav += chunk.scavenged.popcntRange(0, ei+1)
		chunk.allocRange(0, ei+1)
		p.scav.index.alloc(ec, ei+1)
	}
	p.update(base, npages, true, true)
	return uintptr(scav) * pageSize
}

// findMappedAddr returns the smallest mapped offAddr that is
// >= addr. That is, if addr refers to mapped memory, then it is
// returned. If addr is higher than any mapped region, then
// it returns maxOffAddr.
//
// p.mheapLock must be held.
func (p *pageAlloc) findMappedAddr(addr offAddr) offAddr {
	assertLockHeld(p.mheapLock)

	// If we're not in a test, validate first by checking mheap_.arenas.
	// This is a fast path which is only safe to use outside of testing.
	ai := arenaIndex(addr.addr())
	if p.test || mheap_.arenas[ai.l1()] == nil || mheap_.arenas[ai.l1()][ai.l2()] == nil {
		vAddr, ok := p.inUse.findAddrGreaterEqual(addr.addr())
		if ok {
			return offAddr{vAddr}
		} else {
			// The candidate search address is greater than any
			// known address, which means we definitely have no
			// free memory left.
			return maxOffAddr
		}
	}
	return addr
}

// find searches for the first (address-ordered) contiguous free region of
// npages in size and returns a base address for that region.
//
// It uses p.searchAddr to prune its search and assumes that no palloc chunks
// below chunkIndex(p.searchAddr) contain any free memory at all.
//
// find also computes and returns a candidate p.searchAddr, which may or
// may not prune more of the address space than p.searchAddr already does.
// This candidate is always a valid p.searchAddr.
//
// find represents the slow path and the full radix tree search.
//
// Returns a base address of 0 on failure, in which case the candidate
// searchAddr returned is invalid and must be ignored.
//
// p.mheapLock must be held.
func (p *pageAlloc) find(npages uintptr) (uintptr, offAddr) {
	assertLockHeld(p.mheapLock)

	// Search algorithm.
	//
	// This algorithm walks each level l of the radix tree from the root level
	// to the leaf level. It iterates over at most 1 << levelBits[l] of entries
	// in a given level in the radix tree, and uses the summary information to
	// find either:
	//  1) That a given subtree contains a large enough contiguous region, at
	//     which point it continues iterating on the next level, or
	//  2) That there are enough contiguous boundary-crossing bits to satisfy
	//     the allocation, at which point it knows exactly where to start
	//     allocating from.
	//
	// i tracks the index into the current level l's structure for the
	// contiguous 1 << levelBits[l] entries we're actually interested in.
	//
	// NOTE: Technically this search could allocate a region which crosses
	// the arenaBaseOffset boundary, which when arenaBaseOffset != 0, is
	// a discontinuity. However, the only way this could happen is if the
	// page at the zero address is mapped, and this is impossible on
	// every system we support where arenaBaseOffset != 0. So, the
	// discontinuity is already encoded in the fact that the OS will never
	// map the zero page for us, and this function doesn't try to handle
	// this case in any way.

	// i is the beginning of the block of entries we're searching at the
	// current level.
	i := 0

	// firstFree is the region of address space that we are certain to
	// find the first free page in the heap. base and bound are the inclusive
	// bounds of this window, and both are addresses in the linearized, contiguous
	// view of the address space (with arenaBaseOffset pre-added). At each level,
	// this window is narrowed as we find the memory region containing the
	// first free page of memory. To begin with, the range reflects the
	// full process address space.
	//
	// firstFree is updated by calling foundFree each time free space in the
	// heap is discovered.
	//
	// At the end of the search, base.addr() is the best new
	// searchAddr we could deduce in this search.
	firstFree := struct {
		base, bound offAddr
	}{
		base:  minOffAddr,
		bound: maxOffAddr,
	}
	// foundFree takes the given address range [addr, addr+size) and
	// updates firstFree if it is a narrower range. The input range must
	// either be fully contained within firstFree or not overlap with it
	// at all.
	//
	// This way, we'll record the first summary we find with any free
	// pages on the root level and narrow that down if we descend into
	// that summary. But as soon as we need to iterate beyond that summary
	// in a level to find a large enough range, we'll stop narrowing.
	foundFree := func(addr offAddr, size uintptr) {
		if firstFree.base.lessEqual(addr) && addr.add(size-1).lessEqual(firstFree.bound) {
			// This range fits within the current firstFree window, so narrow
			// down the firstFree window to the base and bound of this range.
			firstFree.base = addr
			firstFree.bound = addr.add(size - 1)
		} else if !(addr.add(size-1).lessThan(firstFree.base) || firstFree.bound.lessThan(addr)) {
			// This range only partially overlaps with the firstFree range,
			// so throw.
			print("runtime: addr = ", hex(addr.addr()), ", size = ", size, "\n")
			print("runtime: base = ", hex(firstFree.base.addr()), ", bound = ", hex(firstFree.bound.addr()), "\n")
			throw("range partially overlaps")
		}
	}

	// lastSum is the summary which we saw on the previous level that made us
	// move on to the next level. Used to print additional information in the
	// case of a catastrophic failure.
	// lastSumIdx is that summary's index in the previous level.
	lastSum := packPallocSum(0, 0, 0)
	lastSumIdx := -1

nextLevel:
	for l := 0; l < len(p.summary); l++ {
		// For the root level, entriesPerBlock is the whole level.
		entriesPerBlock := 1 << levelBits[l]
		logMaxPages := levelLogPages[l]

		// We've moved into a new level, so let's update i to our new
		// starting index. This is a no-op for level 0.
		i <<= levelBits[l]

		// Slice out the block of entries we care about.
		entries := p.summary[l][i : i+entriesPerBlock]

		// Determine j0, the first index we should start iterating from.
		// The searchAddr may help us eliminate iterations if we followed the
		// searchAddr on the previous level or we're on the root level, in which
		// case the searchAddr should be the same as i after levelShift.
		j0 := 0
		if searchIdx := offAddrToLevelIndex(l, p.searchAddr); searchIdx&^(entriesPerBlock-1) == i {
			j0 = searchIdx & (entriesPerBlock - 1)
		}

		// Run over the level entries looking for
		// a contiguous run of at least npages either
		// within an entry or across entries.
		//
		// base contains the page index (relative to
		// the first entry's first page) of the currently
		// considered run of consecutive pages.
		//
		// size contains the size of the currently considered
		// run of consecutive pages.
		var base, size uint
		for j := j0; j < len(entries); j++ {
			sum := entries[j]
			if sum == 0 {
				// A full entry means we broke any streak and
				// that we should skip it altogether.
				size = 0
				continue
			}

			// We've encountered a non-zero summary which means
			// free memory, so update firstFree.
			foundFree(levelIndexToOffAddr(l, i+j), (uintptr(1)<<logMaxPages)*pageSize)

			s := sum.start()
			if size+s >= uint(npages) {
				// If size == 0 we don't have a run yet,
				// which means base isn't valid. So, set
				// base to the first page in this block.
				if size == 0 {
					base = uint(j) << logMaxPages
				}
				// We hit npages; we're done!
				size += s
				break
			}
			if sum.max() >= uint(npages) {
				// The entry itself contains npages contiguous
				// free pages, so continue on the next level
				// to find that run.
				i += j
				lastSumIdx = i
				lastSum = sum
				continue nextLevel
			}
			if size == 0 || s < 1<<logMaxPages {
				// We either don't have a current run started, or this entry
				// isn't totally free (meaning we can't continue the current
				// one), so try to begin a new run by setting size and base
				// based on sum.end.
				size = sum.end()
				base = uint(j+1)<<logMaxPages - size
				continue
			}
			// The entry is completely free, so continue the run.
			size += 1 << logMaxPages
		}
		if size >= uint(npages) {
			// We found a sufficiently large run of free pages straddling
			// some boundary, so compute the address and return it.
			addr := levelIndexToOffAddr(l, i).add(uintptr(base) * pageSize).addr()
			return addr, p.findMappedAddr(firstFree.base)
		}
		if l == 0 {
			// We're at level zero, so that means we've exhausted our search.
			return 0, maxSearchAddr()
		}

		// We're not at level zero, and we exhausted the level we were looking in.
		// This means that either our calculations were wrong or the level above
		// lied to us. In either case, dump some useful state and throw.
		print("runtime: summary[", l-1, "][", lastSumIdx, "] = ", lastSum.start(), ", ", lastSum.max(), ", ", lastSum.end(), "\n")
		print("runtime: level = ", l, ", npages = ", npages, ", j0 = ", j0, "\n")
		print("runtime: p.searchAddr = ", hex(p.searchAddr.addr()), ", i = ", i, "\n")
		print("runtime: levelShift[level] = ", levelShift[l], ", levelBits[level] = ", levelBits[l], "\n")
		for j := 0; j < len(entries); j++ {
			sum := entries[j]
			print("runtime: summary[", l, "][", i+j, "] = (", sum.start(), ", ", sum.max(), ", ", sum.end(), ")\n")
		}
		throw("bad summary data")
	}

	// Since we've gotten to this point, that means we haven't found a
	// sufficiently-sized free region straddling some boundary (chunk or larger).
	// This means the last summary we inspected must have had a large enough "max"
	// value, so look inside the chunk to find a suitable run.
	//
	// After iterating over all levels, i must contain a chunk index which
	// is what the final level represents.
	ci := chunkIdx(i)
	j, searchIdx := p.chunkOf(ci).find(npages, 0)
	if j == ^uint(0) {
		// We couldn't find any space in this chunk despite the summaries telling
		// us it should be there. There's likely a bug, so dump some state and throw.
		sum := p.summary[len(p.summary)-1][i]
		print("runtime: summary[", len(p.summary)-1, "][", i, "] = (", sum.start(), ", ", sum.max(), ", ", sum.end(), ")\n")
		print("runtime: npages = ", npages, "\n")
		throw("bad summary data")
	}

	// Compute the address at which the free space starts.
	addr := chunkBase(ci) + uintptr(j)*pageSize

	// Since we actually searched the chunk, we may have
	// found an even narrower free window.
	searchAddr := chunkBase(ci) + uintptr(searchIdx)*pageSize
	foundFree(offAddr{searchAddr}, chunkBase(ci+1)-searchAddr)
	return addr, p.findMappedAddr(firstFree.base)
}

// alloc allocates npages worth of memory from the page heap, returning the base
// address for the allocation and the amount of scavenged memory in bytes
// contained in the region [base address, base address + npages*pageSize).
//
// Returns a 0 base address on failure, in which case other returned values
// should be ignored.
//
// p.mheapLock must be held.
//
// Must run on the system stack because p.mheapLock must be held.
//
//go:systemstack
func (p *pageAlloc) alloc(npages uintptr) (addr uintptr, scav uintptr) {
	assertLockHeld(p.mheapLock)

	// If the searchAddr refers to a region which has a higher address than
	// any known chunk, then we know we're out of memory.
	if chunkIndex(p.searchAddr.addr()) >= p.end {
		return 0, 0
	}

	// If npages has a chance of fitting in the chunk where the searchAddr is,
	// search it directly.
	searchAddr := minOffAddr
	if pallocChunkPages-chunkPageIndex(p.searchAddr.addr()) >= uint(npages) {
		// npages is guaranteed to be no greater than pallocChunkPages here.
		i := chunkIndex(p.searchAddr.addr())
		if max := p.summary[len(p.summary)-1][i].max(); max >= uint(npages) {
			j, searchIdx := p.chunkOf(i).find(npages, chunkPageIndex(p.searchAddr.addr()))
			if j == ^uint(0) {
				print("runtime: max = ", max, ", npages = ", npages, "\n")
				print("runtime: searchIdx = ", chunkPageIndex(p.searchAddr.addr()), ", p.searchAddr = ", hex(p.searchAddr.addr()), "\n")
				throw("bad summary data")
			}
			addr = chunkBase(i) + uintptr(j)*pageSize
			searchAddr = offAddr{chunkBase(i) + uintptr(searchIdx)*pageSize}
			goto Found
		}
	}
	// We failed to use a searchAddr for one reason or another, so try
	// the slow path.
	addr, searchAddr = p.find(npages)
	if addr == 0 {
		if npages == 1 {
			// We failed to find a single free page, the smallest unit
			// of allocation. This means we know the heap is completely
			// exhausted. Otherwise, the heap still might have free
			// space in it, just not enough contiguous space to
			// accommodate npages.
			p.searchAddr = maxSearchAddr()
		}
		return 0, 0
	}
Found:
	// Go ahead and actually mark the bits now that we have an address.
	scav = p.allocRange(addr, npages)

	// If we found a higher searchAddr, we know that all the
	// heap memory before that searchAddr in an offset address space is
	// allocated, so bump p.searchAddr up to the new one.
	if p.searchAddr.lessThan(searchAddr) {
		p.searchAddr = searchAddr
	}
	return addr, scav
}

// free returns npages worth of memory starting at base back to the page heap.
//
// p.mheapLock must be held.
//
// Must run on the system stack because p.mheapLock must be held.
//
//go:systemstack
func (p *pageAlloc) free(base, npages uintptr) {
	assertLockHeld(p.mheapLock)

	// If we're freeing pages below the p.searchAddr, update searchAddr.
	if b := (offAddr{base}); b.lessThan(p.searchAddr) {
		p.searchAddr = b
	}
	limit := base + npages*pageSize - 1
	if npages == 1 {
		// Fast path: we're clearing a single bit, and we know exactly
		// where it is, so mark it directly.
		i := chunkIndex(base)
		pi := chunkPageIndex(base)
		p.chunkOf(i).free1(pi)
		p.scav.index.free(i, pi, 1)
	} else {
		// Slow path: we're clearing more bits so we may need to iterate.
		sc, ec := chunkIndex(base), chunkIndex(limit)
		si, ei := chunkPageIndex(base), chunkPageIndex(limit)

		if sc == ec {
			// The range doesn't cross any chunk boundaries.
			p.chunkOf(sc).free(si, ei+1-si)
			p.scav.index.free(sc, si, ei+1-si)
		} else {
			// The range crosses at least one chunk boundary.
			p.chunkOf(sc).free(si, pallocChunkPages-si)
			p.scav.index.free(sc, si, pallocChunkPages-si)
			for c := sc + 1; c < ec; c++ {
				p.chunkOf(c).freeAll()
				p.scav.index.free(c, 0, pallocChunkPages)
			}
			p.chunkOf(ec).free(0, ei+1)
			p.scav.index.free(ec, 0, ei+1)
		}
	}
	p.update(base, npages, true, false)
}

const (
	pallocSumBytes = unsafe.Sizeof(pallocSum(0))

	// maxPackedValue is the maximum value that any of the three fields in
	// the pallocSum may take on.
	maxPackedValue    = 1 << logMaxPackedValue
	logMaxPackedValue = logPallocChunkPages + (summaryLevels-1)*summaryLevelBits

	freeChunkSum = pallocSum(uint64(pallocChunkPages) |
		uint64(pallocChunkPages<<logMaxPackedValue) |
		uint64(pallocChunkPages<<(2*logMaxPackedValue)))
)

// pallocSum is a packed summary type which packs three numbers: start, max,
// and end into a single 8-byte value. Each of these values are a summary of
// a bitmap and are thus counts, each of which may have a maximum value of
// 2^21 - 1, or all three may be equal to 2^21. The latter case is represented
// by just setting the 64th bit.
// pallocSum 是一个压缩的摘要类型，它将三个数字：start、max 和 end 打包到一个 8 字节的值中。
// 这些值中的每一个都是位图的摘要，因此都是计数值，每个值可能的最大值为 2^21 - 1，
// 或者所有三个值都可能等于 2^21。后一种情况通过设置第 64 位来表示。
type pallocSum uint64

// packPallocSum takes a start, max, and end value and produces a pallocSum.
func packPallocSum(start, max, end uint) pallocSum {
	if max == maxPackedValue {
		return pallocSum(uint64(1 << 63))
	}
	return pallocSum((uint64(start) & (maxPackedValue - 1)) |
		((uint64(max) & (maxPackedValue - 1)) << logMaxPackedValue) |
		((uint64(end) & (maxPackedValue - 1)) << (2 * logMaxPackedValue)))
}

// start extracts the start value from a packed sum.
func (p pallocSum) start() uint {
	if uint64(p)&uint64(1<<63) != 0 {
		return maxPackedValue
	}
	return uint(uint64(p) & (maxPackedValue - 1))
}

// max extracts the max value from a packed sum.
func (p pallocSum) max() uint {
	if uint64(p)&uint64(1<<63) != 0 {
		return maxPackedValue
	}
	return uint((uint64(p) >> logMaxPackedValue) & (maxPackedValue - 1))
}

// end extracts the end value from a packed sum.
func (p pallocSum) end() uint {
	if uint64(p)&uint64(1<<63) != 0 {
		return maxPackedValue
	}
	return uint((uint64(p) >> (2 * logMaxPackedValue)) & (maxPackedValue - 1))
}

// unpack unpacks all three values from the summary.
func (p pallocSum) unpack() (uint, uint, uint) {
	if uint64(p)&uint64(1<<63) != 0 {
		return maxPackedValue, maxPackedValue, maxPackedValue
	}
	return uint(uint64(p) & (maxPackedValue - 1)),
		uint((uint64(p) >> logMaxPackedValue) & (maxPackedValue - 1)),
		uint((uint64(p) >> (2 * logMaxPackedValue)) & (maxPackedValue - 1))
}

// mergeSummaries merges consecutive summaries which may each represent at
// most 1 << logMaxPagesPerSum pages each together into one.
func mergeSummaries(sums []pallocSum, logMaxPagesPerSum uint) pallocSum {
	// Merge the summaries in sums into one.
	//
	// We do this by keeping a running summary representing the merged
	// summaries of sums[:i] in start, most, and end.
	start, most, end := sums[0].unpack()
	for i := 1; i < len(sums); i++ {
		// Merge in sums[i].
		si, mi, ei := sums[i].unpack()

		// Merge in sums[i].start only if the running summary is
		// completely free, otherwise this summary's start
		// plays no role in the combined sum.
		if start == uint(i)<<logMaxPagesPerSum {
			start += si
		}

		// Recompute the max value of the running sum by looking
		// across the boundary between the running sum and sums[i]
		// and at the max sums[i], taking the greatest of those two
		// and the max of the running sum.
		most = max(most, end+si, mi)

		// Merge in end by checking if this new summary is totally
		// free. If it is, then we want to extend the running sum's
		// end by the new summary. If not, then we have some alloc'd
		// pages in there and we just want to take the end value in
		// sums[i].
		if ei == 1<<logMaxPagesPerSum {
			end += 1 << logMaxPagesPerSum
		} else {
			end = ei
		}
	}
	return packPallocSum(start, most, end)
}
