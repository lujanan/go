// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Memory allocator.
//
// This was originally based on tcmalloc, but has diverged quite a bit.
// http://goog-perftools.sourceforge.net/doc/tcmalloc.html

// 内存分配器。
//
// 最初基于 tcmalloc，但已经发生了很大的变化。
// http://goog-perftools.sourceforge.net/doc/tcmalloc.html

// The main allocator works in runs of pages.
// Small allocation sizes (up to and including 32 kB) are
// rounded to one of about 70 size classes, each of which
// has its own free set of objects of exactly that size.
// Any free page of memory can be split into a set of objects
// of one size class, which are then managed using a free bitmap.
//
// 主分配器以页为单位工作。
// 小的分配大小（最多包括 32 kB）会被四舍五入到大约 70 个大小类之一，
// 每个大小类都有自己的一组空闲对象，大小正好是该类的大小。
// 任何空闲的内存页都可以被分割成一组特定大小类的对象，
// 然后使用空闲位图来管理这些对象。

// The allocator's data structures are:
//
//	fixalloc: a free-list allocator for fixed-size off-heap objects,
//		used to manage storage used by the allocator.
//	mheap: the malloc heap, managed at page (8192-byte) granularity.
//	mspan: a run of in-use pages managed by the mheap.
//	mcentral: collects all spans of a given size class.
//	mcache: a per-P cache of mspans with free space.
//	mstats: allocation statistics.
//
// 分配器的数据结构包括：
//
//	fixalloc: 固定大小的堆外对象自由列表分配器，
//		用于管理分配器使用的存储空间。
//	mheap: malloc 堆，以页（8192字节）为粒度进行管理。
//	mspan: 由 mheap 管理的一组正在使用的页。
//	mcentral: 收集特定大小类的所有 spans。
//	mcache: 每个 P 的 mspans 缓存，包含空闲空间。
//	mstats: 分配统计信息。

// Allocating a small object proceeds up a hierarchy of caches:
//
//	1. Round the size up to one of the small size classes
//	   and look in the corresponding mspan in this P's mcache.
//	   Scan the mspan's free bitmap to find a free slot.
//	   If there is a free slot, allocate it.
//	   This can all be done without acquiring a lock.
//
//	2. If the mspan has no free slots, obtain a new mspan
//	   from the mcentral's list of mspans of the required size
//	   class that have free space.
//	   Obtaining a whole span amortizes the cost of locking
//	   the mcentral.
//
//	3. If the mcentral's mspan list is empty, obtain a run
//	   of pages from the mheap to use for the mspan.
//
//	4. If the mheap is empty or has no page runs large enough,
//	   allocate a new group of pages (at least 1MB) from the
//	   operating system. Allocating a large run of pages
//	   amortizes the cost of talking to the operating system.
//
// 分配小对象时，会按照以下缓存层次结构进行：
//
//	1. 将大小四舍五入到小大小类之一，
//	   并在当前 P 的 mcache 中查找对应的 mspan。
//	   扫描 mspan 的空闲位图以找到空闲槽。
//	   如果有空闲槽，就分配它。
//	   所有这些操作都可以在不获取锁的情况下完成。
//
//	2. 如果 mspan 没有空闲槽，从 mcentral 的列表中获取一个新的 mspan，
//	   该 mspan 必须具有所需大小类且有可用空间。
//	   获取整个 span 可以分摊锁定 mcentral 的成本。
//
//	3. 如果 mcentral 的 mspan 列表为空，从 mheap 获取一组页用于 mspan。
//
//	4. 如果 mheap 为空或没有足够大的页组，
//	   从操作系统分配一组新的页（至少 1MB）。
//	   分配大量页可以分摊与操作系统通信的成本。

// Sweeping an mspan and freeing objects on it proceeds up a similar
// hierarchy:
//
//	1. If the mspan is being swept in response to allocation, it
//	   is returned to the mcache to satisfy the allocation.
//
//	2. Otherwise, if the mspan still has allocated objects in it,
//	   it is placed on the mcentral free list for the mspan's size
//	   class.
//
//	3. Otherwise, if all objects in the mspan are free, the mspan's
//	   pages are returned to the mheap and the mspan is now dead.
//
// 清理 mspan 并释放其上的对象也遵循类似的层次结构：
//
//	1. 如果 mspan 正在被清理以响应分配请求，
//	   它会被返回到 mcache 以满足分配需求。
//
//	2. 否则，如果 mspan 中仍有已分配的对象，
//	   它会被放置在 mcentral 的对应大小类的空闲列表中。
//
//	3. 否则，如果 mspan 中的所有对象都是空闲的，
//	   mspan 的页会被返回到 mheap，mspan 现在处于死亡状态。

// Allocating and freeing a large object uses the mheap
// directly, bypassing the mcache and mcentral.
//
// 分配和释放大对象直接使用 mheap，绕过 mcache 和 mcentral。

// If mspan.needzero is false, then free object slots in the mspan are
// already zeroed. Otherwise if needzero is true, objects are zeroed as
// they are allocated. There are various benefits to delaying zeroing
// this way:
//
//	1. Stack frame allocation can avoid zeroing altogether.
//
//	2. It exhibits better temporal locality, since the program is
//	   probably about to write to the memory.
//
//	3. We don't zero pages that never get reused.
//
// 如果 mspan.needzero 为 false，则 mspan 中的空闲对象槽已经被清零。
// 否则如果 needzero 为 true，对象在分配时会被清零。
// 延迟清零有以下几个好处：
//
//	1. 栈帧分配可以完全避免清零。
//
//	2. 它表现出更好的时间局部性，因为程序可能即将写入该内存。
//
//	3. 我们不会对永远不会被重用的页进行清零。

// Virtual memory layout
//
// The heap consists of a set of arenas, which are 64MB on 64-bit and
// 4MB on 32-bit (heapArenaBytes). Each arena's start address is also
// aligned to the arena size.
//
// Each arena has an associated heapArena object that stores the
// metadata for that arena: the heap bitmap for all words in the arena
// and the span map for all pages in the arena. heapArena objects are
// themselves allocated off-heap.
//
// Since arenas are aligned, the address space can be viewed as a
// series of arena frames. The arena map (mheap_.arenas) maps from
// arena frame number to *heapArena, or nil for parts of the address
// space not backed by the Go heap. The arena map is structured as a
// two-level array consisting of a "L1" arena map and many "L2" arena
// maps; however, since arenas are large, on many architectures, the
// arena map consists of a single, large L2 map.
//
// The arena map covers the entire possible address space, allowing
// the Go heap to use any part of the address space. The allocator
// attempts to keep arenas contiguous so that large spans (and hence
// large objects) can cross arenas.
//
// 虚拟内存布局
//
// 堆由一组 arena 组成，在 64 位系统上为 64MB，在 32 位系统上为 4MB（heapArenaBytes）。
// 每个 arena 的起始地址也与 arena 大小对齐。
//
// 每个 arena 都有一个关联的 heapArena 对象，用于存储该 arena 的元数据：
// arena 中所有字的堆位图和 arena 中所有页的 span 映射。
// heapArena 对象本身是在堆外分配的。
//
// 由于 arena 是对齐的，地址空间可以被视为一系列 arena 帧。
// arena 映射（mheap_.arenas）将 arena 帧号映射到 *heapArena，
// 对于未被 Go 堆支持的地址空间部分则为 nil。
// arena 映射被构造为一个两级数组，由"L1" arena 映射和多个"L2" arena 映射组成；
// 然而，由于 arena 很大，在许多架构上，arena 映射由一个单一的、大的 L2 映射组成。
//
// arena 映射覆盖整个可能的地址空间，允许 Go 堆使用地址空间的任何部分。
// 分配器尝试保持 arena 连续，以便大 spans（以及大对象）可以跨越多个 arena。

package runtime

import (
	"internal/goarch"
	"internal/goos"
	"internal/runtime/atomic"
	"runtime/internal/math"
	"runtime/internal/sys"
	"unsafe"
)

const (
	maxTinySize   = _TinySize      // 微小对象分配器的最大大小
	tinySizeClass = _TinySizeClass // 微小对象的大小类别
	maxSmallSize  = _MaxSmallSize  // 小对象的最大大小

	pageShift = _PageShift // 页大小的位移量
	pageSize  = _PageSize  // 页大小

	_PageSize = 1 << _PageShift // 页大小，等于2的_PageShift次方
	_PageMask = _PageSize - 1   // 页掩码，用于页对齐

	// _64bit = 1 on 64-bit systems, 0 on 32-bit systems
	_64bit = 1 << (^uintptr(0) >> 63) / 2 // 64位系统为1，32位系统为0

	// Tiny allocator parameters, see "Tiny allocator" comment in malloc.go.
	_TinySize      = 16      // 微小对象的最大大小
	_TinySizeClass = int8(2) // 微小对象的大小类别

	_FixAllocChunk = 16 << 10 // Chunk size for FixAlloc // FixAlloc的块大小，16KB

	// Per-P, per order stack segment cache size.
	_StackCacheSize = 32 * 1024 // 每个P的每个阶的栈段缓存大小，32KB

	// Number of orders that get caching. Order 0 is FixedStack
	// and each successive order is twice as large.
	// We want to cache 2KB, 4KB, 8KB, and 16KB stacks. Larger stacks
	// will be allocated directly.
	// Since FixedStack is different on different systems, we
	// must vary NumStackOrders to keep the same maximum cached size.
	//   OS               | FixedStack | NumStackOrders
	//   -----------------+------------+---------------
	//   linux/darwin/bsd | 2KB        | 4
	//   windows/32       | 4KB        | 3
	//   windows/64       | 8KB        | 2
	//   plan9            | 4KB        | 3
	_NumStackOrders = 4 - goarch.PtrSize/4*goos.IsWindows - 1*goos.IsPlan9 // 获取缓存的阶数
	// 阶0是FixedStack，每个后续阶是前一个的两倍大
	// 我们想要缓存2KB、4KB、8KB和16KB的栈。更大的栈将直接分配
	// 由于FixedStack在不同系统上不同，我们必须改变NumStackOrders以保持相同的最大缓存大小

	// heapAddrBits is the number of bits in a heap address. On
	// amd64, addresses are sign-extended beyond heapAddrBits. On
	// other arches, they are zero-extended.
	//
	// On most 64-bit platforms, we limit this to 48 bits based on a
	// combination of hardware and OS limitations.
	//
	// amd64 hardware limits addresses to 48 bits, sign-extended
	// to 64 bits. Addresses where the top 16 bits are not either
	// all 0 or all 1 are "non-canonical" and invalid. Because of
	// these "negative" addresses, we offset addresses by 1<<47
	// (arenaBaseOffset) on amd64 before computing indexes into
	// the heap arenas index. In 2017, amd64 hardware added
	// support for 57 bit addresses; however, currently only Linux
	// supports this extension and the kernel will never choose an
	// address above 1<<47 unless mmap is called with a hint
	// address above 1<<47 (which we never do).
	//
	// arm64 hardware (as of ARMv8) limits user addresses to 48
	// bits, in the range [0, 1<<48).
	//
	// ppc64, mips64, and s390x support arbitrary 64 bit addresses
	// in hardware. On Linux, Go leans on stricter OS limits. Based
	// on Linux's processor.h, the user address space is limited as
	// follows on 64-bit architectures:
	//
	// Architecture  Name              Maximum Value (exclusive)
	// ---------------------------------------------------------------------
	// amd64         TASK_SIZE_MAX     0x007ffffffff000 (47 bit addresses)
	// arm64         TASK_SIZE_64      0x01000000000000 (48 bit addresses)
	// ppc64{,le}    TASK_SIZE_USER64  0x00400000000000 (46 bit addresses)
	// mips64{,le}   TASK_SIZE64       0x00010000000000 (40 bit addresses)
	// s390x         TASK_SIZE         1<<64 (64 bit addresses)
	//
	// These limits may increase over time, but are currently at
	// most 48 bits except on s390x. On all architectures, Linux
	// starts placing mmap'd regions at addresses that are
	// significantly below 48 bits, so even if it's possible to
	// exceed Go's 48 bit limit, it's extremely unlikely in
	// practice.
	//
	// On 32-bit platforms, we accept the full 32-bit address
	// space because doing so is cheap.
	// mips32 only has access to the low 2GB of virtual memory, so
	// we further limit it to 31 bits.
	//
	// On ios/arm64, although 64-bit pointers are presumably
	// available, pointers are truncated to 33 bits in iOS <14.
	// Furthermore, only the top 4 GiB of the address space are
	// actually available to the application. In iOS >=14, more
	// of the address space is available, and the OS can now
	// provide addresses outside of those 33 bits. Pick 40 bits
	// as a reasonable balance between address space usage by the
	// page allocator, and flexibility for what mmap'd regions
	// we'll accept for the heap. We can't just move to the full
	// 48 bits because this uses too much address space for older
	// iOS versions.
	// TODO(mknyszek): Once iOS <14 is deprecated, promote ios/arm64
	// to a 48-bit address space like every other arm64 platform.
	//
	// WebAssembly currently has a limit of 4GB linear memory.

	// heapAddrBits 是堆地址的位数。在 amd64 架构上，地址会进行符号扩展超出 heapAddrBits。
	// 在其他架构上，地址会进行零扩展。
	//
	// 在大多数 64 位平台上，我们基于硬件和操作系统的限制将其限制为 48 位。
	//
	// amd64 硬件将地址限制为 48 位，并进行符号扩展到 64 位。如果高 16 位不全为 0 或 1，
	// 则这些地址是"非规范"的且无效。由于这些"负"地址，我们在计算堆 arena 索引之前，
	// 在 amd64 上会将地址偏移 1<<47 (arenaBaseOffset)。在 2017 年，amd64 硬件添加了对
	// 57 位地址的支持；然而，目前只有 Linux 支持此扩展，并且内核永远不会选择高于 1<<47
	// 的地址，除非 mmap 被调用时提供了高于 1<<47 的提示地址（我们从不这样做）。
	//
	// arm64 硬件（从 ARMv8 开始）将用户地址限制为 48 位，范围在 [0, 1<<48)。
	//
	// ppc64、mips64 和 s390x 在硬件上支持任意 64 位地址。在 Linux 上，Go 依赖于更严格的
	// 操作系统限制。根据 Linux 的 processor.h，64 位架构上的用户地址空间限制如下：
	//
	// 架构        名称                最大值（不包含）
	// ---------------------------------------------------------------------
	// amd64        TASK_SIZE_MAX     0x007ffffffff000 (47 位地址)
	// arm64        TASK_SIZE_64      0x01000000000000 (48 位地址)
	// ppc64{,le}   TASK_SIZE_USER64  0x00400000000000 (46 位地址)
	// mips64{,le}  TASK_SIZE64       0x00010000000000 (40 位地址)
	// s390x        TASK_SIZE         1<<64 (64 位地址)
	//
	// 这些限制可能会随着时间的推移而增加，但目前除了 s390x 外，最多为 48 位。在所有架构上，
	// Linux 开始将 mmap 区域放置在远低于 48 位的地址，因此即使可能超过 Go 的 48 位限制，
	// 在实践中也极不可能。
	//
	// 在 32 位平台上，我们接受完整的 32 位地址空间，因为这样做成本较低。
	// mips32 只能访问虚拟内存的低 2GB，因此我们进一步将其限制为 31 位。
	//
	// 在 ios/arm64 上，虽然 64 位指针理论上可用，但在 iOS <14 中指针被截断为 33 位。
	// 此外，只有地址空间的高 4 GiB 实际上可供应用程序使用。在 iOS >=14 中，更多的地址空间
	// 可用，操作系统现在可以提供超出这 33 位的地址。选择 40 位作为页面分配器地址空间使用和
	// 堆接受的 mmap 区域灵活性之间的合理平衡。我们不能直接使用完整的 48 位，因为这会为
	// 较旧的 iOS 版本使用过多的地址空间。
	// TODO(mknyszek): 一旦 iOS <14 被弃用，将 ios/arm64 提升到 48 位地址空间，就像其他
	// arm64 平台一样。
	//
	// WebAssembly 目前有 4GB 线性内存的限制。
	// arm64 平台下 heapAddrBits = 48
	//
	heapAddrBits = (_64bit*(1-goarch.IsWasm)*(1-goos.IsIos*goarch.IsArm64))*48 + (1-_64bit+goarch.IsWasm)*(32-(goarch.IsMips+goarch.IsMipsle)) + 40*goos.IsIos*goarch.IsArm64

	// maxAlloc is the maximum size of an allocation. On 64-bit,
	// it's theoretically possible to allocate 1<<heapAddrBits bytes. On
	// 32-bit, however, this is one less than 1<<32 because the
	// number of bytes in the address space doesn't actually fit
	// in a uintptr.
	// maxAlloc 是单个分配的最大大小。在 64 位系统上，
	// 理论上可以分配 1<<heapAddrBits 字节。在 32 位系统上，
	// 这个值比 1<<32 小 1，因为地址空间中的字节数实际上
	// 无法完全放入一个 uintptr 中。
	maxAlloc = (1 << heapAddrBits) - (1-_64bit)*1

	// The number of bits in a heap address, the size of heap
	// arenas, and the L1 and L2 arena map sizes are related by
	//
	//   (1 << addr bits) = arena size * L1 entries * L2 entries
	//
	// Currently, we balance these as follows:
	//
	//       Platform  Addr bits  Arena size  L1 entries   L2 entries
	// --------------  ---------  ----------  ----------  -----------
	//       */64-bit         48        64MB           1    4M (32MB)
	// windows/64-bit         48         4MB          64    1M  (8MB)
	//      ios/arm64         33         4MB           1  2048  (8KB)
	//       */32-bit         32         4MB           1  1024  (4KB)
	//     */mips(le)         31         4MB           1   512  (2KB)

	// 堆地址位数、堆 arena 大小以及 L1 和 L2 arena 映射表大小之间的关系如下：
	//
	//   (1 << 地址位数) = arena 大小 * L1 条目数 * L2 条目数
	//
	// 目前，我们在不同平台上平衡这些参数如下：
	//
	//       平台        地址位数  Arena 大小  L1 条目数    L2 条目数
	// --------------  ---------  ----------  ----------  -----------
	//       */64-bit         48        64MB           1    4M (32MB)
	// windows/64-bit         48         4MB          64    1M  (8MB)
	//      ios/arm64         33         4MB           1  2048  (8KB)
	//       */32-bit         32         4MB           1  1024  (4KB)
	//     */mips(le)         31         4MB           1   512  (2KB)

	// heapArenaBytes is the size of a heap arena. The heap
	// consists of mappings of size heapArenaBytes, aligned to
	// heapArenaBytes. The initial heap mapping is one arena.
	//
	// This is currently 64MB on 64-bit non-Windows and 4MB on
	// 32-bit and on Windows. We use smaller arenas on Windows
	// because all committed memory is charged to the process,
	// even if it's not touched. Hence, for processes with small
	// heaps, the mapped arena space needs to be commensurate.
	// This is particularly important with the race detector,
	// since it significantly amplifies the cost of committed
	// memory.

	// heapArenaBytes 是一个堆 arena 的大小。堆由大小为 heapArenaBytes 的映射组成，
	// 并且这些映射都对齐到 heapArenaBytes。初始的堆映射就是一个 arena。
	//
	// 目前在 64 位非 Windows 系统上是 64MB，在 32 位系统和 Windows 上是 4MB。
	// 我们在 Windows 上使用较小的 arena 是因为所有已提交的内存都会被计入进程，
	// 即使这些内存没有被访问。因此，对于堆较小的进程，映射的 arena 空间需要相应调整。
	// 这对于竞态检测器特别重要，因为它会显著放大已提交内存的成本。
	heapArenaBytes = 1 << logHeapArenaBytes

	heapArenaWords = heapArenaBytes / goarch.PtrSize

	// logHeapArenaBytes is log_2 of heapArenaBytes. For clarity,
	// prefer using heapArenaBytes where possible (we need the
	// constant to compute some other constants).
	// heapArenaBytes 的以2为底的对数。为了清晰起见，
	// 尽可能使用 heapArenaBytes（我们需要这个常量来计算其他常量）。
	// 这个值根据不同的平台有不同的计算方式：
	// - 64位非Windows、非Wasm、非iOS/arm64: 6+20=26 (64MB)
	// - 64位Windows: 2+20=22 (4MB)
	// - 32位系统: 2+20=22 (4MB)
	// - Wasm: 2+20=22 (4MB)
	// - iOS/arm64: 2+20=22 (4MB)
	logHeapArenaBytes = (6+20)*(_64bit*(1-goos.IsWindows)*(1-goarch.IsWasm)*(1-goos.IsIos*goarch.IsArm64)) + (2+20)*(_64bit*goos.IsWindows) + (2+20)*(1-_64bit) + (2+20)*goarch.IsWasm + (2+20)*goos.IsIos*goarch.IsArm64

	// heapArenaBitmapWords is the size of each heap arena's bitmap in uintptrs.
	// heapArenaBitmapWords 是每个堆 arena 位图的大小，以 uintptr 为单位。
	// 每个 arena 的位图用于跟踪该 arena 中每个字的分配状态。
	// 由于每个 uintptr 可以表示 8*goarch.PtrSize 个字的分配状态，
	// 所以总大小是 heapArenaWords 除以 8*goarch.PtrSize。
	heapArenaBitmapWords = heapArenaWords / (8 * goarch.PtrSize)

	// pagesPerArena 是每个 arena 包含的页数。
	// 由于 arena 大小是 heapArenaBytes，而每页大小是 pageSize，
	// 所以每个 arena 包含的页数就是 heapArenaBytes/pageSize。
	pagesPerArena = heapArenaBytes / pageSize

	// arenaL1Bits is the number of bits of the arena number
	// covered by the first level arena map.
	//
	// This number should be small, since the first level arena
	// map requires PtrSize*(1<<arenaL1Bits) of space in the
	// binary's BSS. It can be zero, in which case the first level
	// index is effectively unused. There is a performance benefit
	// to this, since the generated code can be more efficient,
	// but comes at the cost of having a large L2 mapping.
	//
	// We use the L1 map on 64-bit Windows because the arena size
	// is small, but the address space is still 48 bits, and
	// there's a high cost to having a large L2.
	//
	// arenaL1Bits 是第一级 arena 映射覆盖的 arena 编号的位数。
	// 这个数字应该很小，因为第一级 arena 映射需要在二进制文件的 BSS 段中
	// 占用 PtrSize*(1<<arenaL1Bits) 的空间。它可以为零，在这种情况下
	// 第一级索引实际上未被使用。这样做有性能优势，因为生成的代码可以更高效，
	// 但代价是必须有一个大的 L2 映射。
	//
	// 我们在 64 位 Windows 上使用 L1 映射，因为 arena 大小较小，
	// 但地址空间仍然是 48 位，而且拥有大的 L2 映射的成本很高。
	arenaL1Bits = 6 * (_64bit * goos.IsWindows)

	// arenaL2Bits is the number of bits of the arena number
	// covered by the second level arena index.
	//
	// The size of each arena map allocation is proportional to
	// 1<<arenaL2Bits, so it's important that this not be too
	// large. 48 bits leads to 32MB arena index allocations, which
	// is about the practical threshold.
	//
	// arenaL2Bits 是第二级 arena 索引覆盖的 arena 编号的位数。
	// 每个 arena 映射分配的大小与 1<<arenaL2Bits 成正比，
	// 所以这个值不能太大。48 位会导致 32MB 的 arena 索引分配，
	// 这大约是实际使用的阈值。
	arenaL2Bits = heapAddrBits - logHeapArenaBytes - arenaL1Bits

	// arenaL1Shift is the number of bits to shift an arena frame
	// number by to compute an index into the first level arena map.
	//
	// arenaL1Shift 是计算第一级 arena 映射索引时需要将 arena 帧号
	// 右移的位数。这个值等于 arenaL2Bits，因为我们需要将 arena 帧号
	// 的高位（arenaL1Bits）和低位（arenaL2Bits）分开。
	arenaL1Shift = arenaL2Bits

	// arenaBits is the total bits in a combined arena map index.
	// This is split between the index into the L1 arena map and
	// the L2 arena map.
	//
	// arenaBits 是组合 arena 映射索引中的总位数。
	// 这个值被分为 L1 arena 映射的索引和 L2 arena 映射的索引两部分。
	// 它等于 arenaL1Bits 和 arenaL2Bits 的和，表示整个 arena 映射
	// 索引的位数。
	arenaBits = arenaL1Bits + arenaL2Bits

	// arenaBaseOffset is the pointer value that corresponds to
	// index 0 in the heap arena map.
	//
	// On amd64, the address space is 48 bits, sign extended to 64
	// bits. This offset lets us handle "negative" addresses (or
	// high addresses if viewed as unsigned).
	//
	// On aix/ppc64, this offset allows to keep the heapAddrBits to
	// 48. Otherwise, it would be 60 in order to handle mmap addresses
	// (in range 0x0a00000000000000 - 0x0afffffffffffff). But in this
	// case, the memory reserved in (s *pageAlloc).init for chunks
	// is causing important slowdowns.
	//
	// On other platforms, the user address space is contiguous
	// and starts at 0, so no offset is necessary.
	//
	// arenaBaseOffset 是堆 arena 映射中索引 0 对应的指针值。
	//
	// 在 amd64 架构上，地址空间是 48 位，符号扩展到 64 位。
	// 这个偏移量让我们能够处理"负"地址（如果被视为无符号数，则是高地址）。
	//
	// 在 aix/ppc64 架构上，这个偏移量允许将 heapAddrBits 保持在 48 位。
	// 否则，为了处理 mmap 地址（在 0x0a00000000000000 - 0x0afffffffffffff 范围内），
	// 它需要是 60 位。但在这种情况下，(s *pageAlloc).init 中为 chunks 保留的内存
	// 会导致严重的性能下降。
	//
	// 在其他平台上，用户地址空间是连续的且从 0 开始，所以不需要偏移量。
	arenaBaseOffset = 0xffff800000000000*goarch.IsAmd64 + 0x0a00000000000000*goos.IsAix
	// A typed version of this constant that will make it into DWARF (for viewcore).
	// 这个常量的类型化版本，将进入 DWARF（用于 viewcore）。
	arenaBaseOffsetUintptr = uintptr(arenaBaseOffset)

	// Max number of threads to run garbage collection.
	// 2, 3, and 4 are all plausible maximums depending
	// on the hardware details of the machine. The garbage
	// collector scales well to 32 cpus.
	//
	// 运行垃圾回收的最大线程数。
	// 根据机器的硬件细节，2、3 和 4 都是合理的最大值。
	// 垃圾回收器可以很好地扩展到 32 个 CPU。
	_MaxGcproc = 32

	// minLegalPointer is the smallest possible legal pointer.
	// This is the smallest possible architectural page size,
	// since we assume that the first page is never mapped.
	//
	// This should agree with minZeroPage in the compiler.
	//
	// minLegalPointer 是最小的合法指针。
	// 这是最小的架构页大小，因为我们假设第一页永远不会被映射。
	//
	// 这应该与编译器中的 minZeroPage 一致。
	minLegalPointer uintptr = 4096

	// minHeapForMetadataHugePages sets a threshold on when certain kinds of
	// heap metadata, currently the arenas map L2 entries and page alloc bitmap
	// mappings, are allowed to be backed by huge pages. If the heap goal ever
	// exceeds this threshold, then huge pages are enabled.
	//
	// These numbers are chosen with the assumption that huge pages are on the
	// order of a few MiB in size.
	//
	// The kind of metadata this applies to has a very low overhead when compared
	// to address space used, but their constant overheads for small heaps would
	// be very high if they were to be backed by huge pages (e.g. a few MiB makes
	// a huge difference for an 8 MiB heap, but barely any difference for a 1 GiB
	// heap). The benefit of huge pages is also not worth it for small heaps,
	// because only a very, very small part of the metadata is used for small heaps.
	//
	// N.B. If the heap goal exceeds the threshold then shrinks to a very small size
	// again, then huge pages will still be enabled for this mapping. The reason is that
	// there's no point unless we're also returning the physical memory for these
	// metadata mappings back to the OS. That would be quite complex to do in general
	// as the heap is likely fragmented after a reduction in heap size.
	//
	// minHeapForMetadataHugePages 设置了一个阈值，用于决定何时允许某些类型的堆元数据
	//（目前包括 arenas 映射的 L2 条目和页分配位图映射）使用大页（huge pages）作为支持。
	// 如果堆目标大小超过这个阈值，就会启用大页。
	//
	// 这些数值的选择基于大页大小在几 MiB 数量级的假设。
	//
	// 这种元数据相比使用的地址空间来说开销非常低，但如果对小堆使用大页支持，
	// 它们的固定开销会非常高（例如，几 MiB 对 8 MiB 的堆来说影响很大，
	// 但对 1 GiB 的堆几乎没有任何影响）。对小堆来说，使用大页的好处也不值得，
	// 因为只有非常非常小的一部分元数据用于小堆。
	//
	// 注意：如果堆目标大小超过阈值后又缩小到非常小的尺寸，大页仍然会保持启用状态。
	// 原因是除非我们同时将这些元数据映射的物理内存返回给操作系统，否则禁用大页没有意义。
	// 由于堆在大小减少后很可能变得碎片化，这通常很难实现。
	minHeapForMetadataHugePages = 1 << 30
)

// physPageSize is the size in bytes of the OS's physical pages.
// Mapping and unmapping operations must be done at multiples of
// physPageSize.
//
// This must be set by the OS init code (typically in osinit) before
// mallocinit.
//
// physPageSize 是操作系统物理页的大小（以字节为单位）。
// 映射和取消映射操作必须在 physPageSize 的倍数处进行。
//
// 这必须在 mallocinit 之前由操作系统初始化代码（通常在 osinit 中）设置。
var physPageSize uintptr

// physHugePageSize is the size in bytes of the OS's default physical huge
// page size whose allocation is opaque to the application. It is assumed
// and verified to be a power of two.
//
// If set, this must be set by the OS init code (typically in osinit) before
// mallocinit. However, setting it at all is optional, and leaving the default
// value is always safe (though potentially less efficient).
//
// Since physHugePageSize is always assumed to be a power of two,
// physHugePageShift is defined as physHugePageSize == 1 << physHugePageShift.
// The purpose of physHugePageShift is to avoid doing divisions in
// performance critical functions.
//
// physHugePageSize 是操作系统默认物理大页的大小（以字节为单位），
// 其分配对应用程序是不透明的。它被假设并验证为 2 的幂。
//
// 如果设置，这必须在 mallocinit 之前由操作系统初始化代码（通常在 osinit 中）设置。
// 然而，完全设置它是可选的，保留默认值总是安全的（尽管可能效率较低）。
//
// 由于 physHugePageSize 总是被假设为 2 的幂，
// physHugePageShift 被定义为 physHugePageSize == 1 << physHugePageShift。
// physHugePageShift 的目的是避免在性能关键函数中进行除法运算。
var (
	physHugePageSize  uintptr
	physHugePageShift uint
)

func mallocinit() {
	// 检查 tiny 大小类的正确性
	if class_to_size[_TinySizeClass] != _TinySize {
		throw("bad TinySizeClass")
	}

	// 检查 heapArenaBitmapWords 是否为 2 的幂
	// heapBits 期望位图地址的模运算能够正常工作
	if heapArenaBitmapWords&(heapArenaBitmapWords-1) != 0 {
		// heapBits expects modular arithmetic on bitmap
		// addresses to work.
		throw("heapArenaBitmapWords not a power of 2")
	}

	// Check physPageSize.
	// 检查物理页大小
	if physPageSize == 0 {
		// The OS init code failed to fetch the physical page size.
		// 操作系统初始化代码未能获取物理页大小
		throw("failed to get system page size")
	}
	if physPageSize > maxPhysPageSize {
		// 物理页大小超过了系统支持的最大页大小
		print("system page size (", physPageSize, ") is larger than maximum page size (", maxPhysPageSize, ")\n")
		throw("bad system page size")
	}
	if physPageSize < minPhysPageSize {
		// 物理页大小小于系统支持的最小页大小
		print("system page size (", physPageSize, ") is smaller than minimum page size (", minPhysPageSize, ")\n")
		throw("bad system page size")
	}
	if physPageSize&(physPageSize-1) != 0 {
		// 物理页大小必须是2的幂次方，这里通过位运算检查
		print("system page size (", physPageSize, ") must be a power of 2\n")
		throw("bad system page size")
	}

	// 检查大页大小
	if physHugePageSize&(physHugePageSize-1) != 0 {
		print("system huge page size (", physHugePageSize, ") must be a power of 2\n")
		throw("bad system huge page size")
	}
	if physHugePageSize > maxPhysHugePageSize {
		// physHugePageSize is greater than the maximum supported huge page size.
		// Don't throw here, like in the other cases, since a system configured
		// in this way isn't wrong, we just don't have the code to support them.
		// Instead, silently set the huge page size to zero.
		// 如果大页大小超过最大支持大小，不抛出错误，而是将大页大小设置为 0
		// 因为这样配置的系统并没有错，只是我们没有代码来支持它们
		physHugePageSize = 0
	}
	if physHugePageSize != 0 {
		// Since physHugePageSize is a power of 2, it suffices to increase
		// physHugePageShift until 1<<physHugePageShift == physHugePageSize.
		// 计算大页大小的位移值，用于避免在性能关键函数中进行除法运算
		for 1<<physHugePageShift != physHugePageSize {
			physHugePageShift++
		}
	}
	// Check arena and span related constants
	// 检查 arena 和 span 相关的常量设置
	// pagesPerArena 必须能被 pagesPerSpanRoot 整除，确保每个 arena 可以完整地划分为多个 span
	if pagesPerArena%pagesPerSpanRoot != 0 {
		print("pagesPerArena (", pagesPerArena, ") is not divisible by pagesPerSpanRoot (", pagesPerSpanRoot, ")\n")
		throw("bad pagesPerSpanRoot")
	}
	// pagesPerArena 必须能被 pagesPerReclaimerChunk 整除，确保每个 arena 可以完整地划分为多个回收器块
	if pagesPerArena%pagesPerReclaimerChunk != 0 {
		print("pagesPerArena (", pagesPerArena, ") is not divisible by pagesPerReclaimerChunk (", pagesPerReclaimerChunk, ")\n")
		throw("bad pagesPerReclaimerChunk")
	}

	// Check that the minimum size (exclusive) for a malloc header is also
	// a size class boundary. This is important to making sure checks align
	// across different parts of the runtime.
	// 检查 malloc header 的最小大小是否也是大小类的边界
	// 这对于确保运行时不同部分的检查对齐很重要
	minSizeForMallocHeaderIsSizeClass := false
	for i := 0; i < len(class_to_size); i++ {
		if minSizeForMallocHeader == uintptr(class_to_size[i]) {
			minSizeForMallocHeaderIsSizeClass = true
			break
		}
	}
	if !minSizeForMallocHeaderIsSizeClass {
		throw("min size of malloc header is not a size class boundary")
	}

	// Check that the pointer bitmap for all small sizes without a malloc header
	// fits in a word.
	// 检查没有 malloc header 的小对象的指针位图是否适合一个字
	if minSizeForMallocHeader/goarch.PtrSize > 8*goarch.PtrSize {
		throw("max pointer/scan bitmap size for headerless objects is too large")
	}

	// 检查标记指针的位数是否足够
	if minTagBits > taggedPointerBits {
		throw("taggedPointerbits too small")
	}

	// Initialize the heap.
	// 初始化堆
	mheap_.init()
	// 为 mcache0 分配内存，mcache0 是全局的 mcache 实例
	// 用于在没有 P 的情况下进行内存分配
	mcache0 = allocmcache()
	// 初始化 GC 位图 arenas 的锁
	// 用于保护 GC 位图 arenas 的并发访问
	lockInit(&gcBitsArenas.lock, lockRankGcBitsArenas)
	// 初始化性能分析插入锁
	// 用于保护性能分析数据的插入操作
	lockInit(&profInsertLock, lockRankProfInsert)
	// 初始化性能分析块锁
	// 用于保护性能分析块的操作
	lockInit(&profBlockLock, lockRankProfBlock)
	// 初始化活跃内存性能分析锁
	// 用于保护活跃内存性能分析数据的访问
	lockInit(&profMemActiveLock, lockRankProfMemActive)
	// 初始化未来内存性能分析锁数组
	// 用于保护未来内存性能分析数据的访问
	for i := range profMemFutureLock {
		lockInit(&profMemFutureLock[i], lockRankProfMemFuture)
	}
	// 初始化全局分配器互斥锁
	// 用于保护全局分配器的并发访问
	lockInit(&globalAlloc.mutex, lockRankGlobalAlloc)

	// Create initial arena growth hints.
	if goarch.PtrSize == 8 {
		// On a 64-bit machine, we pick the following hints
		// because:
		//
		// 1. Starting from the middle of the address space
		// makes it easier to grow out a contiguous range
		// without running in to some other mapping.
		//
		// 2. This makes Go heap addresses more easily
		// recognizable when debugging.
		//
		// 3. Stack scanning in gccgo is still conservative,
		// so it's important that addresses be distinguishable
		// from other data.
		//
		// Starting at 0x00c0 means that the valid memory addresses
		// will begin 0x00c0, 0x00c1, ...
		// In little-endian, that's c0 00, c1 00, ... None of those are valid
		// UTF-8 sequences, and they are otherwise as far away from
		// ff (likely a common byte) as possible. If that fails, we try other 0xXXc0
		// addresses. An earlier attempt to use 0x11f8 caused out of memory errors
		// on OS X during thread allocations.  0x00c0 causes conflicts with
		// AddressSanitizer which reserves all memory up to 0x0100.
		// These choices reduce the odds of a conservative garbage collector
		// not collecting memory because some non-pointer block of memory
		// had a bit pattern that matched a memory address.
		//
		// However, on arm64, we ignore all this advice above and slam the
		// allocation at 0x40 << 32 because when using 4k pages with 3-level
		// translation buffers, the user address space is limited to 39 bits
		// On ios/arm64, the address space is even smaller.
		//
		// On AIX, mmaps starts at 0x0A00000000000000 for 64-bit.
		// processes.
		//
		// Space mapped for user arenas comes immediately after the range
		// originally reserved for the regular heap when race mode is not
		// enabled because user arena chunks can never be used for regular heap
		// allocations and we want to avoid fragmenting the address space.
		//
		// In race mode we have no choice but to just use the same hints because
		// the race detector requires that the heap be mapped contiguously.

		// 在64位机器上，我们选择以下地址提示的原因：
		//
		// 1. 从地址空间的中间开始分配，可以更容易地扩展出连续的范围，
		//    而不会与其他映射发生冲突。
		//
		// 2. 这使得Go堆地址在调试时更容易识别。
		//
		// 3. gccgo中的栈扫描仍然是保守的，因此地址必须能够与其他数据区分开来。
		//
		// 从0x00c0开始意味着有效内存地址将以0x00c0、0x00c1等开始。
		// 在小端序中，这些地址表示为c0 00、c1 00等。这些都不是有效的UTF-8序列，
		// 并且它们尽可能地远离ff（可能是一个常见字节）。如果这个地址失败，
		// 我们会尝试其他0xXXc0地址。之前尝试使用0x11f8在OS X上的线程分配期间
		// 导致了内存不足错误。0x00c0与AddressSanitizer冲突，它保留了0x0100之前的所有内存。
		// 这些选择降低了保守垃圾收集器因为某些非指针内存块具有匹配内存地址的位模式
		// 而不收集内存的可能性。
		//
		// 然而，在arm64上，我们忽略上述所有建议，直接将分配地址设置为0x40 << 32，
		// 因为在使用4k页和3级转换缓冲区时，用户地址空间被限制为39位。
		// 在ios/arm64上，地址空间甚至更小。
		//
		// 在AIX上，64位进程的mmap从0x0A00000000000000开始。
		//
		// 当未启用race模式时，用户arena映射的空间紧跟在最初为常规堆保留的范围之后，
		// 因为用户arena块永远不能用于常规堆分配，我们希望避免地址空间碎片化。
		//
		// 在race模式下，我们别无选择，只能使用相同的提示，因为race检测器要求堆必须连续映射。
		for i := 0x7f; i >= 0; i-- {
			var p uintptr
			switch {
			case raceenabled:
				// The TSAN runtime requires the heap
				// to be in the range [0x00c000000000,
				// 0x00e000000000).
				// TSAN运行时要求堆必须在[0x00c000000000, 0x00e000000000)范围内
				p = uintptr(i)<<32 | uintptrMask&(0x00c0<<32)
				if p >= uintptrMask&0x00e000000000 {
					continue
				}
			case GOARCH == "arm64" && GOOS == "ios":
				// 在iOS/arm64平台上，使用特定的地址范围
				// 0x0013<<28 是iOS/arm64平台特定的基地址
				p = uintptr(i)<<40 | uintptrMask&(0x0013<<28)
			case GOARCH == "arm64":
				// 在arm64平台上，使用0x0040<<32作为基地址
				// 这是为了适应arm64的地址空间限制
				p = uintptr(i)<<40 | uintptrMask&(0x0040<<32)
			case GOOS == "aix":
				if i == 0 {
					// We don't use addresses directly after 0x0A00000000000000
					// to avoid collisions with others mmaps done by non-go programs.
					// 避免使用0x0A00000000000000之后的地址，以防止与非Go程序的mmap冲突
					continue
				}
				// AIX平台使用0xa0<<52作为基地址
				p = uintptr(i)<<40 | uintptrMask&(0xa0<<52)
			default:
				// 默认情况下使用0x00c0<<32作为基地址
				// 这是大多数64位平台的标准选择
				p = uintptr(i)<<40 | uintptrMask&(0x00c0<<32)
			}
			// Switch to generating hints for user arenas if we've gone
			// through about half the hints. In race mode, take only about
			// a quarter; we don't have very much space to work with.
			// 当遍历到大约一半的提示时，切换到为用户arena生成提示
			// 在race模式下，只使用大约四分之一的提示，因为可用空间有限
			hintList := &mheap_.arenaHints
			if (!raceenabled && i > 0x3f) || (raceenabled && i > 0x5f) {
				hintList = &mheap_.userArena.arenaHints
			}
			// 分配一个新的arenaHint并设置其地址
			// 将新的hint添加到hintList的头部
			hint := (*arenaHint)(mheap_.arenaHintAlloc.alloc())
			hint.addr = p
			hint.next, *hintList = *hintList, hint
		}
	} else {
		// On a 32-bit machine, we're much more concerned
		// about keeping the usable heap contiguous.
		// Hence:
		//
		// 1. We reserve space for all heapArenas up front so
		// they don't get interleaved with the heap. They're
		// ~258MB, so this isn't too bad. (We could reserve a
		// smaller amount of space up front if this is a
		// problem.)
		//
		// 2. We hint the heap to start right above the end of
		// the binary so we have the best chance of keeping it
		// contiguous.
		//
		// 3. We try to stake out a reasonably large initial
		// heap reservation.

		// 在32位机器上，我们更关注保持可用堆的连续性。
		// 因此：
		//
		// 1. 我们预先为所有heapArenas保留空间，这样它们就不会与堆交错。
		//    它们大约258MB，所以这还不错。（如果这是个问题，我们可以预先保留更小的空间。）
		//
		// 2. 我们提示堆从二进制文件结束处开始，这样我们就有最好的机会保持它的连续性。
		//
		// 3. 我们尝试预留一个合理大小的初始堆空间。

		// Calculate the total size needed for all heap arena metadata
		// 计算所有堆arena元数据所需的总大小
		const arenaMetaSize = (1 << arenaBits) * unsafe.Sizeof(heapArena{})
		// Reserve virtual memory space for heap arena metadata
		// 为堆arena元数据预留虚拟内存空间
		meta := uintptr(sysReserve(nil, arenaMetaSize))
		if meta != 0 {
			// Initialize the heap arena allocator with the reserved space
			// 使用预留的空间初始化堆arena分配器
			mheap_.heapArenaAlloc.init(meta, arenaMetaSize, true)
		}

		// We want to start the arena low, but if we're linked
		// against C code, it's possible global constructors
		// have called malloc and adjusted the process' brk.
		// Query the brk so we can avoid trying to map the
		// region over it (which will cause the kernel to put
		// the region somewhere else, likely at a high
		// address).
		// 我们希望从低地址开始分配arena，但如果链接了C代码，
		// 全局构造函数可能已经调用了malloc并调整了进程的brk。
		// 查询brk以便我们可以避免尝试映射该区域（这会导致内核将区域放在其他地方，可能在高地址）。
		procBrk := sbrk0()

		// If we ask for the end of the data segment but the
		// operating system requires a little more space
		// before we can start allocating, it will give out a
		// slightly higher pointer. Except QEMU, which is
		// buggy, as usual: it won't adjust the pointer
		// upward. So adjust it upward a little bit ourselves:
		// 1/4 MB to get away from the running binary image.
		// 如果我们请求数据段的末尾，但操作系统在开始分配之前需要更多空间，
		// 它会给出一个稍高的指针。除了QEMU，它像往常一样有bug：它不会向上调整指针。
		// 所以我们自己稍微向上调整一下：1/4 MB以远离正在运行的二进制映像。
		p := firstmoduledata.end
		if p < procBrk {
			p = procBrk
		}
		if mheap_.heapArenaAlloc.next <= p && p < mheap_.heapArenaAlloc.end {
			p = mheap_.heapArenaAlloc.end
		}
		p = alignUp(p+(256<<10), heapArenaBytes)
		// Because we're worried about fragmentation on
		// 32-bit, we try to make a large initial reservation.
		// 因为我们担心32位上的碎片化，我们尝试进行大的初始预留。
		// Define a set of arena sizes to try reserving, starting with the largest
		// 定义一组要尝试预留的arena大小，从最大的开始
		arenaSizes := []uintptr{
			512 << 20, // 512MB
			256 << 20, // 256MB
			128 << 20, // 128MB
		}
		// Try to reserve memory for each arena size until successful
		// 尝试为每个arena大小预留内存，直到成功
		for _, arenaSize := range arenaSizes {
			// Attempt to reserve aligned memory for the arena
			// 尝试为arena预留对齐的内存
			a, size := sysReserveAligned(unsafe.Pointer(p), arenaSize, heapArenaBytes)
			if a != nil {
				// Initialize the arena with the reserved memory
				// 使用预留的内存初始化arena
				mheap_.arena.init(uintptr(a), size, false)
				p = mheap_.arena.end // For hint below
				break
			}
		}
		// Create a new arena hint and add it to the hint list
		// 创建一个新的arena提示并将其添加到提示列表中
		hint := (*arenaHint)(mheap_.arenaHintAlloc.alloc())
		hint.addr = p
		hint.next, mheap_.arenaHints = mheap_.arenaHints, hint

		// Place the hint for user arenas just after the large reservation.
		//
		// While this potentially competes with the hint above, in practice we probably
		// aren't going to be getting this far anyway on 32-bit platforms.
		// 将用户arena的提示放在大预留之后。
		// 虽然这可能与上面的提示竞争，但在实践中，在32位平台上我们可能不会走到这一步。
		userArenaHint := (*arenaHint)(mheap_.arenaHintAlloc.alloc())
		userArenaHint.addr = p
		userArenaHint.next, mheap_.userArena.arenaHints = mheap_.userArena.arenaHints, userArenaHint
	}
	// Initialize the memory limit here because the allocator is going to look at it
	// but we haven't called gcinit yet and we're definitely going to allocate memory before then.
	// 在这里初始化内存限制，因为分配器将要查看它，
	// 但我们还没有调用gcinit，而且在那之前我们肯定会分配内存。
	gcController.memoryLimit.Store(maxInt64)
}

// sysAlloc allocates heap arena space for at least n bytes. The
// returned pointer is always heapArenaBytes-aligned and backed by
// h.arenas metadata. The returned size is always a multiple of
// heapArenaBytes. sysAlloc returns nil on failure.
// There is no corresponding free function.
//
// hintList is a list of hint addresses for where to allocate new
// heap arenas. It must be non-nil.
//
// register indicates whether the heap arena should be registered
// in allArenas.
//
// sysAlloc returns a memory region in the Reserved state. This region must
// be transitioned to Prepared and then Ready before use.
//
// h must be locked.
//
// sysAlloc 为堆 arena 分配至少 n 字节的空间。
// 返回的指针总是 heapArenaBytes 对齐的，并且由 h.arenas 元数据支持。
// 返回的大小总是 heapArenaBytes 的倍数。失败时返回 nil。
// 没有对应的释放函数。
//
// hintList 是用于分配新堆 arena 的提示地址列表。必须非空。
//
// register 表示是否应该在 allArenas 中注册堆 arena。
//
// sysAlloc 返回一个处于 Reserved 状态的内存区域。
// 在使用之前，这个区域必须转换为 Prepared 状态，然后再转换为 Ready 状态。
//
// h 必须被锁定。
func (h *mheap) sysAlloc(n uintptr, hintList **arenaHint, register bool) (v unsafe.Pointer, size uintptr) {
	assertLockHeld(&h.lock)

	// 将 n 向上对齐到 heapArenaBytes
	n = alignUp(n, heapArenaBytes)

	if hintList == &h.arenaHints {
		// First, try the arena pre-reservation.
		// Newly-used mappings are considered released.
		//
		// Only do this if we're using the regular heap arena hints.
		// This behavior is only for the heap.
		//
		// 首先，尝试使用预预留的 arena。
		// 新使用的映射被视为已释放。
		//
		// 只有当使用常规堆 arena 提示时才这样做。
		// 这种行为仅适用于堆。
		v = h.arena.alloc(n, heapArenaBytes, &gcController.heapReleased)
		if v != nil {
			size = n
			goto mapped
		}
	}

	// Try to grow the heap at a hint address.
	// 尝试在提示地址处扩展堆。
	for *hintList != nil {
		hint := *hintList
		p := hint.addr
		if hint.down {
			p -= n
		}
		if p+n < p {
			// We can't use this, so don't ask.
			// 我们不能使用这个地址，所以不要请求。
			v = nil
		} else if arenaIndex(p+n-1) >= 1<<arenaBits {
			// Outside addressable heap. Can't use.
			// 超出可寻址堆范围。不能使用。
			v = nil
		} else {
			v = sysReserve(unsafe.Pointer(p), n)
		}
		if p == uintptr(v) {
			// Success. Update the hint.
			// 成功。更新提示地址。
			if !hint.down {
				p += n
			}
			hint.addr = p
			size = n
			break
		}
		// Failed. Discard this hint and try the next.
		// 失败。丢弃这个提示并尝试下一个。
		//
		// TODO: This would be cleaner if sysReserve could be
		// told to only return the requested address. In
		// particular, this is already how Windows behaves, so
		// it would simplify things there.
		// TODO: 如果 sysReserve 可以被指示只返回请求的地址，
		// 这样会更清晰。特别是，Windows 已经是这样工作的，
		// 所以这会简化那里的情况。
		if v != nil {
			sysFreeOS(v, n)
		}
		*hintList = hint.next
		h.arenaHintAlloc.free(unsafe.Pointer(hint))
	}

	if size == 0 {
		if raceenabled {
			// The race detector assumes the heap lives in
			// [0x00c000000000, 0x00e000000000), but we
			// just ran out of hints in this region. Give
			// a nice failure.
			// 竞态检测器假设堆位于 [0x00c000000000, 0x00e000000000) 范围内，
			// 但我们在这个区域已经用完了提示地址。给出一个友好的错误提示。
			throw("too many address space collisions for -race mode")
		}

		// All of the hints failed, so we'll take any
		// (sufficiently aligned) address the kernel will give
		// us.
		// 所有的提示地址都失败了，所以我们将接受内核提供的任何
		// （足够对齐的）地址。
		v, size = sysReserveAligned(nil, n, heapArenaBytes)
		if v == nil {
			return nil, 0
		}

		// Create new hints for extending this region.
		// 为扩展这个区域创建新的提示地址。
		// 创建两个提示地址：一个指向区域的开始（向下扩展），
		// 一个指向区域的结束（向上扩展）。
		hint := (*arenaHint)(h.arenaHintAlloc.alloc())
		hint.addr, hint.down = uintptr(v), true
		hint.next, mheap_.arenaHints = mheap_.arenaHints, hint
		hint = (*arenaHint)(h.arenaHintAlloc.alloc())
		hint.addr = uintptr(v) + size
		hint.next, mheap_.arenaHints = mheap_.arenaHints, hint
	}

	// Check for bad pointers or pointers we can't use.
	// 检查无效指针或无法使用的指针。
	{
		var bad string
		p := uintptr(v)
		if p+size < p {
			// 检查地址溢出：如果 p+size < p，说明发生了地址回绕
			bad = "region exceeds uintptr range"
		} else if arenaIndex(p) >= 1<<arenaBits {
			// 检查起始地址是否超出可用地址空间
			bad = "base outside usable address space"
		} else if arenaIndex(p+size-1) >= 1<<arenaBits {
			// 检查结束地址是否超出可用地址空间
			bad = "end outside usable address space"
		}
		if bad != "" {
			// This should be impossible on most architectures,
			// but it would be really confusing to debug.
			// 在大多数架构上这应该是不可能的，
			// 但如果发生这种情况，调试会非常困难。
			print("runtime: memory allocated by OS [", hex(p), ", ", hex(p+size), ") not in usable address space: ", bad, "\n")
			throw("memory reservation exceeds address space limit")
		}
	}

	// 检查地址是否按照 heapArenaBytes 对齐
	// 如果不是，说明分配时出现了对齐错误
	if uintptr(v)&(heapArenaBytes-1) != 0 {
		throw("misrounded allocation in sysAlloc")
	}

mapped:
	// Create arena metadata.
	// 创建 arena 元数据。
	for ri := arenaIndex(uintptr(v)); ri <= arenaIndex(uintptr(v)+size-1); ri++ {
		l2 := h.arenas[ri.l1()]
		if l2 == nil {
			// Allocate an L2 arena map.
			//
			// Use sysAllocOS instead of sysAlloc or persistentalloc because there's no
			// statistic we can comfortably account for this space in. With this structure,
			// we rely on demand paging to avoid large overheads, but tracking which memory
			// is paged in is too expensive. Trying to account for the whole region means
			// that it will appear like an enormous memory overhead in statistics, even though
			// it is not.
			// 分配 L2 arena 映射。
			//
			// 使用 sysAllocOS 而不是 sysAlloc 或 persistentalloc，因为我们无法
			// 在统计信息中合理地计算这个空间。通过这种结构，我们依赖按需分页
			// 来避免大的开销，但跟踪哪些内存被分页进来太昂贵了。尝试计算整个区域
			// 意味着它会在统计信息中显示为巨大的内存开销，即使实际上并非如此。
			l2 = (*[1 << arenaL2Bits]*heapArena)(sysAllocOS(unsafe.Sizeof(*l2)))
			if l2 == nil {
				throw("out of memory allocating heap arena map")
			}
			if h.arenasHugePages {
				sysHugePage(unsafe.Pointer(l2), unsafe.Sizeof(*l2))
			} else {
				sysNoHugePage(unsafe.Pointer(l2), unsafe.Sizeof(*l2))
			}
			atomic.StorepNoWB(unsafe.Pointer(&h.arenas[ri.l1()]), unsafe.Pointer(l2))
		}
		// Check if the arena is already initialized
		// 检查 arena 是否已经被初始化
		if l2[ri.l2()] != nil {
			throw("arena already initialized")
		}
		var r *heapArena
		// Try to allocate heap arena metadata from the heap arena allocator first
		// 首先尝试从堆 arena 分配器分配堆 arena 元数据
		r = (*heapArena)(h.heapArenaAlloc.alloc(unsafe.Sizeof(*r), goarch.PtrSize, &memstats.gcMiscSys))
		if r == nil {
			// If heap arena allocator fails, try persistent allocator as fallback
			// 如果堆 arena 分配器失败，尝试使用持久分配器作为备选方案
			r = (*heapArena)(persistentalloc(unsafe.Sizeof(*r), goarch.PtrSize, &memstats.gcMiscSys))
			if r == nil {
				throw("out of memory allocating heap arena metadata")
			}
		}

		// Register the arena in allArenas if requested.
		// 如果需要，在 allArenas 中注册 arena。
		if register {
			if len(h.allArenas) == cap(h.allArenas) {
				size := 2 * uintptr(cap(h.allArenas)) * goarch.PtrSize
				if size == 0 {
					size = physPageSize
				}
				newArray := (*notInHeap)(persistentalloc(size, goarch.PtrSize, &memstats.gcMiscSys))
				if newArray == nil {
					throw("out of memory allocating allArenas")
				}
				oldSlice := h.allArenas
				*(*notInHeapSlice)(unsafe.Pointer(&h.allArenas)) = notInHeapSlice{newArray, len(h.allArenas), int(size / goarch.PtrSize)}
				copy(h.allArenas, oldSlice)
				// Do not free the old backing array because
				// there may be concurrent readers. Since we
				// double the array each time, this can lead
				// to at most 2x waste.
				// 不要释放旧的底层数组，因为可能有并发读取者。
				// 由于我们每次都将数组大小翻倍，这最多可能导致 2 倍的浪费。
			}
			h.allArenas = h.allArenas[:len(h.allArenas)+1]
			h.allArenas[len(h.allArenas)-1] = ri
		}

		// Store atomically just in case an object from the
		// new heap arena becomes visible before the heap lock
		// is released (which shouldn't happen, but there's
		// little downside to this).
		// 原子地存储，以防在堆锁释放之前新堆 arena 中的对象变得可见
		//（这不应该发生，但这样做几乎没有坏处）。
		atomic.StorepNoWB(unsafe.Pointer(&l2[ri.l2()]), unsafe.Pointer(r))
	}

	// Tell the race detector about the new heap memory.
	if raceenabled {
		racemapshadow(v, size)
	}

	return
}

// sysReserveAligned is like sysReserve, but the returned pointer is
// aligned to align bytes. It may reserve either n or n+align bytes,
// so it returns the size that was reserved.
// sysReserveAligned 类似于 sysReserve，但返回的指针会按照 align 字节对齐。
// 它可能会保留 n 或 n+align 字节，因此返回实际保留的大小。
func sysReserveAligned(v unsafe.Pointer, size, align uintptr) (unsafe.Pointer, uintptr) {
	// Since the alignment is rather large in uses of this
	// function, we're not likely to get it by chance, so we ask
	// for a larger region and remove the parts we don't need.
	// 由于这个函数使用的对齐值通常很大，我们不太可能偶然得到对齐的内存，
	// 所以我们会请求一个更大的区域，然后移除不需要的部分。
	retries := 0
retry:
	p := uintptr(sysReserve(v, size+align))
	switch {
	case p == 0:
		return nil, 0
	case p&(align-1) == 0:
		// 如果地址已经是对齐的，直接返回
		return unsafe.Pointer(p), size + align
	case GOOS == "windows":
		// On Windows we can't release pieces of a
		// reservation, so we release the whole thing and
		// re-reserve the aligned sub-region. This may race,
		// so we may have to try again.
		// 在 Windows 上，我们不能释放部分保留的内存，
		// 所以先释放整个区域，然后重新保留对齐的子区域。
		// 这可能会发生竞争，所以可能需要重试。
		sysFreeOS(unsafe.Pointer(p), size+align)
		p = alignUp(p, align)
		p2 := sysReserve(unsafe.Pointer(p), size)
		if p != uintptr(p2) {
			// Must have raced. Try again.
			// 一定是发生了竞争，重试
			sysFreeOS(p2, size)
			if retries++; retries == 100 {
				throw("failed to allocate aligned heap memory; too many retries")
			}
			goto retry
		}
		// Success.
		// 成功
		return p2, size
	default:
		// Trim off the unaligned parts.
		// 裁剪掉未对齐的部分
		pAligned := alignUp(p, align)
		sysFreeOS(unsafe.Pointer(p), pAligned-p)
		end := pAligned + size
		endLen := (p + size + align) - end
		if endLen > 0 {
			sysFreeOS(unsafe.Pointer(end), endLen)
		}
		return unsafe.Pointer(pAligned), size
	}
}

// enableMetadataHugePages enables huge pages for various sources of heap metadata.
//
// A note on latency: for sufficiently small heaps (<10s of GiB) this function will take constant
// time, but may take time proportional to the size of the mapped heap beyond that.
//
// This function is idempotent.
//
// The heap lock must not be held over this operation, since it will briefly acquire
// the heap lock.
//
// Must be called on the system stack because it acquires the heap lock.
//
// 为各种堆元数据源启用大页。
//
// 关于延迟的说明：对于足够小的堆（<10s of GiB），此函数将花费恒定时间，
// 但在此之后，时间可能与映射堆的大小成正比。
//
// 此函数是幂等的。
//
// 堆锁不能在此操作期间持有，因为它会短暂获取堆锁。
//
// 必须在系统栈上调用，因为它会获取堆锁。
//
//go:systemstack
func (h *mheap) enableMetadataHugePages() {
	// Enable huge pages for page structure.
	// 为页结构启用大页
	h.pages.enableChunkHugePages()

	// Grab the lock and set arenasHugePages if it's not.
	//
	// Once arenasHugePages is set, all new L2 entries will be eligible for
	// huge pages. We'll set all the old entries after we release the lock.
	// 获取锁并设置 arenasHugePages（如果尚未设置）
	//
	// 一旦设置了 arenasHugePages，所有新的 L2 条目都将有资格使用大页。
	// 我们将在释放锁后设置所有旧条目。
	lock(&h.lock)
	if h.arenasHugePages {
		unlock(&h.lock)
		return
	}
	h.arenasHugePages = true
	unlock(&h.lock)

	// N.B. The arenas L1 map is quite small on all platforms, so it's fine to
	// just iterate over the whole thing.
	// 注意：在所有平台上，arenas L1 映射都相当小，所以直接遍历整个映射是可以的。
	for i := range h.arenas {
		l2 := (*[1 << arenaL2Bits]*heapArena)(atomic.Loadp(unsafe.Pointer(&h.arenas[i])))
		if l2 == nil {
			continue
		}
		sysHugePage(unsafe.Pointer(l2), unsafe.Sizeof(*l2))
	}
}

// base address for all 0-byte allocations
// 所有0字节分配的基地址
// 这个变量用于存储所有0字节内存分配的基地址
// 当请求分配0字节内存时，会返回这个地址
// 这样可以避免为0字节分配实际的内存空间
var zerobase uintptr

// nextFreeFast returns the next free object if one is quickly available.
// Otherwise it returns 0.
// nextFreeFast 快速返回下一个可用的空闲对象。
// 如果没有可用的空闲对象，则返回 0。
func nextFreeFast(s *mspan) gclinkptr {
	// Is there a free object in the allocCache?
	// 检查 allocCache 中是否有空闲对象
	theBit := sys.TrailingZeros64(s.allocCache)
	if theBit < 64 {
		// 计算空闲对象在 span 中的索引
		result := s.freeindex + uint16(theBit)
		if result < s.nelems {
			// 计算下一个空闲对象的索引
			freeidx := result + 1
			// 如果下一个空闲对象是 allocCache 的最后一个对象，
			// 且不是 span 的最后一个对象，则返回 0
			// 这样调用者就知道需要重新加载 allocCache
			if freeidx%64 == 0 && freeidx != s.nelems {
				return 0
			}
			// 更新 allocCache，移除已分配的对象
			s.allocCache >>= uint(theBit + 1)
			// 更新 freeindex 为下一个空闲对象的索引
			s.freeindex = freeidx
			// 增加已分配对象的计数
			s.allocCount++
			// 返回空闲对象的地址
			return gclinkptr(uintptr(result)*s.elemsize + s.base())
		}
	}
	return 0
}

// nextFree returns the next free object from the cached span if one is available.
// Otherwise it refills the cache with a span with an available object and
// returns that object along with a flag indicating that this was a heavy
// weight allocation. If it is a heavy weight allocation the caller must
// determine whether a new GC cycle needs to be started or if the GC is active
// whether this goroutine needs to assist the GC.
//
// Must run in a non-preemptible context since otherwise the owner of
// c could change.
//
// nextFree 从缓存的 span 中返回下一个可用的空闲对象。
// 如果没有可用的对象，它会用一个新的有可用对象的 span 重新填充缓存，
// 并返回该对象以及一个标志，表明这是一个重量级分配。
// 如果是重量级分配，调用者必须确定是否需要启动新的 GC 周期，
// 或者如果 GC 正在运行，当前 goroutine 是否需要协助 GC。
//
// 必须在不可抢占的上下文中运行，否则 c 的所有者可能会改变。
func (c *mcache) nextFree(spc spanClass) (v gclinkptr, s *mspan, shouldhelpgc bool) {
	// 获取指定大小类的 span
	s = c.alloc[spc]
	// 默认不需要协助 GC
	shouldhelpgc = false
	// 获取下一个空闲对象的索引
	freeIndex := s.nextFreeIndex()
	if freeIndex == s.nelems {
		// The span is full.
		// span 已满
		if s.allocCount != s.nelems {
			println("runtime: s.allocCount=", s.allocCount, "s.nelems=", s.nelems)
			throw("s.allocCount != s.nelems && freeIndex == s.nelems")
		}
		// 重新填充缓存
		c.refill(spc)
		// 标记需要协助 GC
		shouldhelpgc = true
		// 获取新的 span
		s = c.alloc[spc]

		// 获取新 span 的下一个空闲对象索引
		freeIndex = s.nextFreeIndex()
	}

	// 检查索引是否有效
	if freeIndex >= s.nelems {
		throw("freeIndex is not valid")
	}

	// 计算并返回空闲对象的地址
	v = gclinkptr(uintptr(freeIndex)*s.elemsize + s.base())
	// 增加已分配对象计数
	s.allocCount++
	// 检查分配计数是否超过总对象数
	if s.allocCount > s.nelems {
		println("s.allocCount=", s.allocCount, "s.nelems=", s.nelems)
		throw("s.allocCount > s.nelems")
	}
	return
}

// Allocate an object of size bytes.
// Small objects are allocated from the per-P cache's free lists.
// Large objects (> 32 kB) are allocated straight from the heap.
//
// mallocgc should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/bytedance/gopkg
//   - github.com/bytedance/sonic
//   - github.com/cloudwego/frugal
//   - github.com/cockroachdb/cockroach
//   - github.com/cockroachdb/pebble
//   - github.com/ugorji/go/codec
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
// 分配一个大小为 size 字节的对象。
// 小对象从每个 P 的缓存空闲列表中分配。
// 大对象（> 32 kB）直接从堆中分配。
//
// mallocgc 应该是一个内部实现细节，
// 但许多包通过 linkname 访问它。
// 以下是使用此功能的知名包（不推荐这样做）：
//   - github.com/bytedance/gopkg
//   - github.com/bytedance/sonic
//   - github.com/cloudwego/frugal
//   - github.com/cockroachdb/cockroach
//   - github.com/cockroachdb/pebble
//   - github.com/ugorji/go/codec
//
// 不要移除或更改类型签名。
// 参见 go.dev/issue/67401。
//
//go:linkname mallocgc
func mallocgc(size uintptr, typ *_type, needzero bool) unsafe.Pointer {
	// 检查是否在 GC 标记终止阶段，如果是则抛出异常
	// 因为在 GC 标记终止阶段不应该进行内存分配
	if gcphase == _GCmarktermination {
		throw("mallocgc called with gcphase == _GCmarktermination")
	}

	// 如果请求分配的大小为 0，返回一个特殊的零大小对象的地址
	// 所有零大小对象都共享这个地址
	if size == 0 {
		return unsafe.Pointer(&zerobase)
	}

	// It's possible for any malloc to trigger sweeping, which may in
	// turn queue finalizers. Record this dynamic lock edge.
	// 任何 malloc 操作都可能触发清扫，而清扫可能会将对象加入终结器队列
	// 记录这个动态锁边，用于死锁检测
	lockRankMayQueueFinalizer()

	// 保存用户请求的原始大小
	userSize := size
	// 如果启用了 ASAN（Address Sanitizer）检查
	if asanenabled {
		// Refer to ASAN runtime library, the malloc() function allocates extra memory,
		// the redzone, around the user requested memory region. And the redzones are marked
		// as unaddressable. We perform the same operations in Go to detect the overflows or
		// underflows.
		// 参考 ASAN 运行时库，malloc() 函数会在用户请求的内存区域周围分配额外的内存（红区）
		// 红区被标记为不可寻址。我们在 Go 中执行相同的操作来检测溢出或下溢
		size += computeRZlog(size)
	}

	if debug.malloc {
		if debug.sbrk != 0 {
			align := uintptr(16)
			if typ != nil {
				// TODO(austin): This should be just
				//   align = uintptr(typ.align)
				// but that's only 4 on 32-bit platforms,
				// even if there's a uint64 field in typ (see #599).
				// This causes 64-bit atomic accesses to panic.
				// Hence, we use stricter alignment that matches
				// the normal allocator better.
				// 这里计算内存对齐方式
				// 理想情况下应该直接使用 typ.align，但在32位平台上这只有4字节
				// 即使类型中包含 uint64 字段也是如此（参见 #599）
				// 这会导致64位原子访问 panic
				// 因此我们使用更严格的对齐方式，更好地匹配普通分配器
				if size&7 == 0 {
					align = 8 // 如果大小是8的倍数，使用8字节对齐
				} else if size&3 == 0 {
					align = 4 // 如果大小是4的倍数，使用4字节对齐
				} else if size&1 == 0 {
					align = 2 // 如果大小是2的倍数，使用2字节对齐
				} else {
					align = 1 // 否则使用1字节对齐
				}
			}
			return persistentalloc(size, align, &memstats.other_sys)
		}

		if inittrace.active && inittrace.id == getg().goid {
			// Init functions are executed sequentially in a single goroutine.
			// 初始化函数在单个 goroutine 中顺序执行
			inittrace.allocs += 1 // 记录分配次数
		}
	}

	// assistG is the G to charge for this allocation, or nil if
	// GC is not currently active.
	// assistG 是负责此次分配的 G，如果 GC 当前未激活则为 nil
	assistG := deductAssistCredit(size)

	// Set mp.mallocing to keep from being preempted by GC.
	// 设置 mp.mallocing 以防止被 GC 抢占
	mp := acquirem()
	if mp.mallocing != 0 {
		throw("malloc deadlock")
	}
	if mp.gsignal == getg() {
		throw("malloc during signal")
	}
	mp.mallocing = 1

	shouldhelpgc := false // 是否需要帮助 GC
	dataSize := userSize  // 用户请求的原始大小
	c := getMCache(mp)    // 获取当前 P 的 mcache
	if c == nil {
		throw("mallocgc called without a P or outside bootstrapping")
	}
	var span *mspan                         // 内存 span
	var header **_type                      // 类型信息
	var x unsafe.Pointer                    // 分配的内存指针
	noscan := typ == nil || !typ.Pointers() // 判断对象是否包含指针
	// In some cases block zeroing can profitably (for latency reduction purposes)
	// be delayed till preemption is possible; delayedZeroing tracks that state.
	// 在某些情况下，为了减少延迟，内存清零操作可以延迟到可能被抢占时进行
	// delayedZeroing 用于跟踪这种状态
	delayedZeroing := false
	// Determine if it's a 'small' object that goes into a size-classed span.
	// 判断是否是一个"小"对象，需要放入大小分类的 span 中
	//
	// Note: This comparison looks a little strange, but it exists to smooth out
	// the crossover between the largest size class and large objects that have
	// their own spans. The small window of object sizes between maxSmallSize-mallocHeaderSize
	// and maxSmallSize will be considered large, even though they might fit in
	// a size class. In practice this is completely fine, since the largest small
	// size class has a single object in it already, precisely to make the transition
	// to large objects smooth.
	// 注意：这个比较看起来有点奇怪，但它存在是为了平滑处理最大大小类别和拥有自己 span 的大对象之间的过渡。
	// 在 maxSmallSize-mallocHeaderSize 和 maxSmallSize 之间的对象大小窗口会被视为大对象，
	// 即使它们可能适合某个大小类别。在实践中这完全没问题，因为最大的小对象大小类别已经只包含一个对象，
	// 这正是为了使向大对象的过渡更加平滑。
	if size <= maxSmallSize-mallocHeaderSize {
		if noscan && size < maxTinySize {
			// Tiny allocator.
			//
			// Tiny allocator combines several tiny allocation requests
			// into a single memory block. The resulting memory block
			// is freed when all subobjects are unreachable. The subobjects
			// must be noscan (don't have pointers), this ensures that
			// the amount of potentially wasted memory is bounded.
			//
			// Size of the memory block used for combining (maxTinySize) is tunable.
			// Current setting is 16 bytes, which relates to 2x worst case memory
			// wastage (when all but one subobjects are unreachable).
			// 8 bytes would result in no wastage at all, but provides less
			// opportunities for combining.
			// 32 bytes provides more opportunities for combining,
			// but can lead to 4x worst case wastage.
			// The best case winning is 8x regardless of block size.
			//
			// Objects obtained from tiny allocator must not be freed explicitly.
			// So when an object will be freed explicitly, we ensure that
			// its size >= maxTinySize.
			//
			// SetFinalizer has a special case for objects potentially coming
			// from tiny allocator, it such case it allows to set finalizers
			// for an inner byte of a memory block.
			//
			// The main targets of tiny allocator are small strings and
			// standalone escaping variables. On a json benchmark
			// the allocator reduces number of allocations by ~12% and
			// reduces heap size by ~20%.

			// 微型分配器
			//
			// 微型分配器将多个微型分配请求合并到单个内存块中。
			// 当所有子对象都不可达时，该内存块会被释放。
			// 子对象必须是 noscan 类型（不包含指针），这确保了潜在的内存浪费是有限的。
			//
			// 用于合并的内存块大小(maxTinySize)是可调的。
			// 当前设置为 16 字节，这会导致最坏情况下 2 倍的内存浪费
			//（当除一个子对象外其他都不可达时）。
			// 8 字节设置将完全不会造成浪费，但提供更少的合并机会。
			// 32 字节提供更多的合并机会，但可能导致 4 倍的最坏情况浪费。
			// 无论块大小如何，最佳情况下的收益都是 8 倍。
			//
			// 从微型分配器获得的对象不能显式释放。
			// 因此当对象需要显式释放时，我们确保其大小 >= maxTinySize。
			//
			// SetFinalizer 对可能来自微型分配器的对象有特殊处理，
			// 在这种情况下它允许为内存块内部的字节设置终结器。
			//
			// 微型分配器的主要目标是小型字符串和独立的逃逸变量。
			// 在 json 基准测试中，该分配器减少了约 12% 的分配次数，
			// 并减少了约 20% 的堆大小。

			// tiny 内存块中，从 offset 往后有空闲位置
			off := c.tinyoffset
			// Align tiny pointer for required (conservative) alignment.
			// 根据所需的（保守）对齐要求对齐微型指针
			if size&7 == 0 {
				// 如果大小为 5 ~ 8 B，size 会被调整为 8 B，此时 8 & 7 == 0，会走进此分支
				// 将 offset 补齐到 8 B 倍数的位置
				off = alignUp(off, 8)
			} else if goarch.PtrSize == 4 && size == 12 {
				// Conservatively align 12-byte objects to 8 bytes on 32-bit
				// systems so that objects whose first field is a 64-bit
				// value is aligned to 8 bytes and does not cause a fault on
				// atomic access. See issue 37262.
				// TODO(mknyszek): Remove this workaround if/when issue 36606
				// is resolved.
				// 在 32 位系统上保守地将 12 字节对象对齐到 8 字节，
				// 这样第一个字段是 64 位值的对象将按 8 字节对齐，
				// 不会在原子访问时导致错误。参见 issue 37262。
				// TODO(mknyszek): 如果/当 issue 36606 解决时移除这个变通方案。
				off = alignUp(off, 8)
			} else if size&3 == 0 {
				// 如果大小为 3 ~ 4 B，size 会被调整为 4 B，此时 4 & 3 == 0，会走进此分支
				// 将 offset 补齐到 4 B 倍数的位置
				off = alignUp(off, 4)
			} else if size&1 == 0 {
				// 如果大小为 1 ~ 2 B，size 会被调整为 2 B，此时 2 & 1 == 0，会走进此分支
				// 将 offset 补齐到 2 B 倍数的位置
				off = alignUp(off, 2)
			}
			if off+size <= maxTinySize && c.tiny != 0 {
				// The object fits into existing tiny block.
				// 对象适合放入现有的微型块中
				x = unsafe.Pointer(c.tiny + off)
				c.tinyoffset = off + size
				c.tinyAllocs++
				mp.mallocing = 0
				releasem(mp)
				return x
			}
			// Allocate a new maxTinySize block.
			// 分配一个新的 maxTinySize 大小的块
			span = c.alloc[tinySpanClass]
			v := nextFreeFast(span)
			if v == 0 {
				v, span, shouldhelpgc = c.nextFree(tinySpanClass)
			}
			x = unsafe.Pointer(v)
			(*[2]uint64)(x)[0] = 0
			(*[2]uint64)(x)[1] = 0
			// See if we need to replace the existing tiny block with the new one
			// based on amount of remaining free space.
			// 根据剩余可用空间判断是否需要用新块替换现有的微型块
			if !raceenabled && (size < c.tinyoffset || c.tiny == 0) {
				// Note: disabled when race detector is on, see comment near end of this function.
				// 注意：当竞态检测器开启时禁用，参见函数末尾的注释
				c.tiny = uintptr(x)
				c.tinyoffset = size
			}
			size = maxTinySize
		} else {
			// Check if we need a header for this allocation.
			// 检查这个分配是否需要头部信息
			hasHeader := !noscan && !heapBitsInSpan(size)
			if hasHeader {
				size += mallocHeaderSize
			}
			// Determine the size class for this allocation.
			// 确定这个分配的大小类别
			var sizeclass uint8
			if size <= smallSizeMax-8 {
				// For small allocations, use the size_to_class8 lookup table.
				// 对于小分配，使用 size_to_class8 查找表
				sizeclass = size_to_class8[divRoundUp(size, smallSizeDiv)]
			} else {
				// For larger allocations, use the size_to_class128 lookup table.
				// 对于较大的分配，使用 size_to_class128 查找表
				sizeclass = size_to_class128[divRoundUp(size-smallSizeMax, largeSizeDiv)]
			}
			// Get the actual size for this size class.
			// 获取这个大小类别的实际大小
			size = uintptr(class_to_size[sizeclass])
			// Create a span class combining size class and noscan flag.
			// 创建一个结合了大小类别和 noscan 标志的 span 类别
			spc := makeSpanClass(sizeclass, noscan)
			span = c.alloc[spc]
			// Try to get a free object quickly.
			// 尝试快速获取一个空闲对象
			v := nextFreeFast(span)
			if v == 0 {
				// If no free object is available, get one through the slow path.
				// 如果没有可用的空闲对象，通过慢路径获取一个
				v, span, shouldhelpgc = c.nextFree(spc)
			}
			x = unsafe.Pointer(v)
			// Zero the memory if needed.
			// 如果需要，将内存清零
			if needzero && span.needzero != 0 {
				memclrNoHeapPointers(x, size)
			}
			if hasHeader {
				// Set up the header and adjust the pointer and size.
				// 设置头部并调整指针和大小
				header = (**_type)(x)
				x = add(x, mallocHeaderSize)
				size -= mallocHeaderSize
			}
		}
	} else {
		shouldhelpgc = true
		// For large allocations, keep track of zeroed state so that
		// bulk zeroing can be happen later in a preemptible context.
		// 对于大对象分配，跟踪清零状态，这样批量清零操作可以在可抢占的上下文中稍后进行
		span = c.allocLarge(size, noscan)
		span.freeindex = 1
		span.allocCount = 1
		size = span.elemsize
		x = unsafe.Pointer(span.base())
		if needzero && span.needzero != 0 {
			delayedZeroing = true
		}
		if !noscan {
			// Tell the GC not to look at this yet.
			// 告诉 GC 暂时不要查看这个对象
			span.largeType = nil
			header = &span.largeType
		}
	}
	if !noscan && !delayedZeroing {
		// 如果对象包含指针且不需要延迟清零，设置对象的类型信息
		// 并更新扫描分配计数器
		c.scanAlloc += heapSetType(uintptr(x), dataSize, typ, header, span)
	}

	// Ensure that the stores above that initialize x to
	// type-safe memory and set the heap bits occur before
	// the caller can make x observable to the garbage
	// collector. Otherwise, on weakly ordered machines,
	// the garbage collector could follow a pointer to x,
	// but see uninitialized memory or stale heap bits.
	// 确保上面的初始化 x 为类型安全内存和设置堆位的操作
	// 在调用者使 x 对垃圾收集器可见之前发生。
	// 否则，在弱序机器上，垃圾收集器可能会跟随指向 x 的指针，
	// 但看到未初始化的内存或过时的堆位。
	publicationBarrier()
	// As x and the heap bits are initialized, update
	// freeIndexForScan now so x is seen by the GC
	// (including conservative scan) as an allocated object.
	// While this pointer can't escape into user code as a
	// _live_ pointer until we return, conservative scanning
	// may find a dead pointer that happens to point into this
	// object. Delaying this update until now ensures that
	// conservative scanning considers this pointer dead until
	// this point.
	// 由于 x 和堆位已经初始化，现在更新 freeIndexForScan，
	// 这样 x 就被 GC（包括保守扫描）视为已分配对象。
	// 虽然这个指针在我们返回之前不能作为 _live_ 指针逃逸到用户代码中，
	// 但保守扫描可能会发现一个恰好指向这个对象的死指针。
	// 延迟这个更新直到现在，确保保守扫描在这个点之前认为这个指针是死的。
	span.freeIndexForScan = span.freeindex

	// Allocate black during GC.
	// All slots hold nil so no scanning is needed.
	// This may be racing with GC so do it atomically if there can be
	// a race marking the bit.
	// 在 GC 期间分配黑色对象。
	// 所有槽位都持有 nil，所以不需要扫描。
	// 这可能与 GC 竞争，所以如果有标记位的竞争，就原子性地执行。
	if gcphase != _GCoff {
		gcmarknewobject(span, uintptr(x))
	}

	if raceenabled {
		racemalloc(x, size)
	}

	if msanenabled {
		msanmalloc(x, size)
	}

	if asanenabled {
		// We should only read/write the memory with the size asked by the user.
		// The rest of the allocated memory should be poisoned, so that we can report
		// errors when accessing poisoned memory.
		// The allocated memory is larger than required userSize, it will also include
		// redzone and some other padding bytes.
		// 我们应该只读写用户请求大小的内存。
		// 其余分配的内存应该被标记为有毒，这样我们可以在访问有毒内存时报告错误。
		// 分配的内存比用户请求的 userSize 大，它还包括红区和其他填充字节。
		rzBeg := unsafe.Add(x, userSize)
		asanpoison(rzBeg, size-userSize)
		asanunpoison(x, userSize)
	}

	// TODO(mknyszek): We should really count the header as part
	// of gc_sys or something. The code below just pretends it is
	// internal fragmentation and matches the GC's accounting by
	// using the whole allocation slot.
	// TODO(mknyszek): 我们真的应该将头部计算为 gc_sys 的一部分。
	// 下面的代码只是假装它是内部碎片，并通过使用整个分配槽来匹配 GC 的记账。
	fullSize := span.elemsize
	if rate := MemProfileRate; rate > 0 {
		// Note cache c only valid while m acquired; see #47302
		//
		// N.B. Use the full size because that matches how the GC
		// will update the mem profile on the "free" side.
		// 注意：缓存 c 仅在获取 m 时有效；参见 #47302
		//
		// 注意：使用完整大小是因为这与 GC 在"释放"端更新内存配置文件的方式相匹配。
		if rate != 1 && fullSize < c.nextSample {
			c.nextSample -= fullSize
		} else {
			profilealloc(mp, x, fullSize)
		}
	}
	mp.mallocing = 0
	releasem(mp)

	// Objects can be zeroed late in a context where preemption can occur.
	// If the object contains pointers, its pointer data must be cleared
	// or otherwise indicate that the GC shouldn't scan it.
	// x will keep the memory alive.
	// 对象可以在可能发生抢占的上下文中延迟清零。
	// 如果对象包含指针，必须清除其指针数据或表明 GC 不应该扫描它。
	// x 将保持内存存活。
	if delayedZeroing {
		// N.B. size == fullSize always in this case.
		// 注意：在这种情况下 size 总是等于 fullSize。
		memclrNoHeapPointersChunked(size, x) // This is a possible preemption point: see #47302
		// 这是一个可能的抢占点：参见 #47302

		// Finish storing the type information for this case.
		// 完成存储这种情况下的类型信息。
		if !noscan {
			mp := acquirem()
			getMCache(mp).scanAlloc += heapSetType(uintptr(x), dataSize, typ, header, span)

			// Publish the type information with the zeroed memory.
			// 发布带有清零内存的类型信息。
			publicationBarrier()
			releasem(mp)
		}
	}

	if debug.malloc {
		if inittrace.active && inittrace.id == getg().goid {
			// Init functions are executed sequentially in a single goroutine.
			// 初始化函数在单个 goroutine 中顺序执行。
			inittrace.bytes += uint64(fullSize)
		}

		if traceAllocFreeEnabled() {
			trace := traceAcquire()
			if trace.ok() {
				trace.HeapObjectAlloc(uintptr(x), typ)
				traceRelease(trace)
			}
		}
	}

	if assistG != nil {
		// Account for internal fragmentation in the assist
		// debt now that we know it.
		//
		// N.B. Use the full size because that's how the rest
		// of the GC accounts for bytes marked.
		// 现在我们知道内部碎片了，在辅助债务中计入它。
		//
		// 注意：使用完整大小是因为这是 GC 其余部分计算标记字节的方式。
		assistG.gcAssistBytes -= int64(fullSize - dataSize)
	}

	if shouldhelpgc {
		if t := (gcTrigger{kind: gcTriggerHeap}); t.test() {
			gcStart(t)
		}
	}

	if raceenabled && noscan && dataSize < maxTinySize {
		// Pad tinysize allocations so they are aligned with the end
		// of the tinyalloc region. This ensures that any arithmetic
		// that goes off the top end of the object will be detectable
		// by checkptr (issue 38872).
		// Note that we disable tinyalloc when raceenabled for this to work.
		// TODO: This padding is only performed when the race detector
		// is enabled. It would be nice to enable it if any package
		// was compiled with checkptr, but there's no easy way to
		// detect that (especially at compile time).
		// TODO: enable this padding for all allocations, not just
		// tinyalloc ones. It's tricky because of pointer maps.
		// Maybe just all noscan objects?
		// 填充 tiny 大小的分配，使它们与 tinyalloc 区域的末尾对齐。
		// 这确保任何超出对象顶部的算术运算都可以被 checkptr 检测到（问题 38872）。
		// 注意：我们禁用了 raceenabled 时的 tinyalloc 以使这能正常工作。
		// TODO：这种填充仅在启用竞态检测器时执行。如果任何包是用 checkptr 编译的，
		// 启用它会很好，但没有简单的方法来检测这一点（特别是在编译时）。
		// TODO：为所有分配启用这种填充，而不仅仅是 tinyalloc 的分配。
		// 由于指针映射的原因，这很棘手。也许只对所有 noscan 对象？
		x = add(x, size-dataSize)
	}

	return x
}

// deductAssistCredit reduces the current G's assist credit
// by size bytes, and assists the GC if necessary.
//
// Caller must be preemptible.
//
// Returns the G for which the assist credit was accounted.
//
// deductAssistCredit 减少当前 G 的辅助信用额度 size 字节，
// 并在必要时协助 GC。
//
// 调用者必须是可以被抢占的。
//
// 返回被计入辅助信用的 G。
func deductAssistCredit(size uintptr) *g {
	var assistG *g
	if gcBlackenEnabled != 0 {
		// Charge the current user G for this allocation.
		// 将此次分配计入当前用户 G
		assistG = getg()
		if assistG.m.curg != nil {
			assistG = assistG.m.curg
		}
		// Charge the allocation against the G. We'll account
		// for internal fragmentation at the end of mallocgc.
		// 将分配计入 G。我们将在 mallocgc 结束时
		// 计算内部碎片。
		assistG.gcAssistBytes -= int64(size)

		if assistG.gcAssistBytes < 0 {
			// This G is in debt. Assist the GC to correct
			// this before allocating. This must happen
			// before disabling preemption.
			// 这个 G 处于负债状态。在分配之前协助 GC
			// 来纠正这种情况。这必须在禁用抢占之前发生。
			gcAssistAlloc(assistG)
		}
	}
	return assistG
}

// memclrNoHeapPointersChunked repeatedly calls memclrNoHeapPointers
// on chunks of the buffer to be zeroed, with opportunities for preemption
// along the way.  memclrNoHeapPointers contains no safepoints and also
// cannot be preemptively scheduled, so this provides a still-efficient
// block copy that can also be preempted on a reasonable granularity.
//
// Use this with care; if the data being cleared is tagged to contain
// pointers, this allows the GC to run before it is all cleared.
//
// memclrNoHeapPointersChunked 分块调用 memclrNoHeapPointers 来清零缓冲区，
// 在清零过程中提供了抢占的机会。memclrNoHeapPointers 不包含安全点，
// 也不能被抢占式调度，因此这个函数提供了一个仍然高效的块复制操作，
// 同时也能在合理的粒度上被抢占。
//
// 使用时要小心；如果被清零的数据被标记为包含指针，这可能会在完全清零之前允许 GC 运行。
func memclrNoHeapPointersChunked(size uintptr, x unsafe.Pointer) {
	v := uintptr(x)
	// got this from benchmarking. 128k is too small, 512k is too large.
	// 这个值是通过基准测试得到的。128k 太小，512k 太大。
	const chunkBytes = 256 * 1024
	vsize := v + size
	for voff := v; voff < vsize; voff = voff + chunkBytes {
		if getg().preempt {
			// may hold locks, e.g., profiling
			// 可能持有锁，例如性能分析
			goschedguarded()
		}
		// clear min(avail, lump) bytes
		// 清除 min(可用, 块大小) 字节
		n := vsize - voff
		if n > chunkBytes {
			n = chunkBytes
		}
		memclrNoHeapPointers(unsafe.Pointer(voff), n)
	}
}

// implementation of new builtin
// compiler (both frontend and SSA backend) knows the signature
// of this function.
// newobject 是 Go 语言中 new 内置函数的实现
// 编译器（包括前端和 SSA 后端）知道这个函数的签名
// 它用于分配并返回一个指向新分配对象的指针
// 分配的内存会被清零（needzero=true）
func newobject(typ *_type) unsafe.Pointer {
	return mallocgc(typ.Size_, typ, true)
}

// reflect_unsafe_New is meant for package reflect,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - gitee.com/quant1x/gox
//   - github.com/goccy/json
//   - github.com/modern-go/reflect2
//   - github.com/v2pro/plz
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname reflect_unsafe_New reflect.unsafe_New
func reflect_unsafe_New(typ *_type) unsafe.Pointer {
	return mallocgc(typ.Size_, typ, true)
}

//go:linkname reflectlite_unsafe_New internal/reflectlite.unsafe_New
func reflectlite_unsafe_New(typ *_type) unsafe.Pointer {
	return mallocgc(typ.Size_, typ, true)
}

// newarray allocates an array of n elements of type typ.
//
// newarray should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/RomiChan/protobuf
//   - github.com/segmentio/encoding
//   - github.com/ugorji/go/codec
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname newarray
func newarray(typ *_type, n int) unsafe.Pointer {
	if n == 1 {
		return mallocgc(typ.Size_, typ, true)
	}
	mem, overflow := math.MulUintptr(typ.Size_, uintptr(n))
	if overflow || mem > maxAlloc || n < 0 {
		panic(plainError("runtime: allocation size out of range"))
	}
	return mallocgc(mem, typ, true)
}

// reflect_unsafe_NewArray is meant for package reflect,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - gitee.com/quant1x/gox
//   - github.com/bytedance/sonic
//   - github.com/goccy/json
//   - github.com/modern-go/reflect2
//   - github.com/segmentio/encoding
//   - github.com/segmentio/kafka-go
//   - github.com/v2pro/plz
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname reflect_unsafe_NewArray reflect.unsafe_NewArray
func reflect_unsafe_NewArray(typ *_type, n int) unsafe.Pointer {
	return newarray(typ, n)
}

// profilealloc records an allocation in the memory profile.
// 记录内存分配到内存配置文件中
func profilealloc(mp *m, x unsafe.Pointer, size uintptr) {
	// Get the mcache for the current m.
	// 获取当前 m 的 mcache
	c := getMCache(mp)
	if c == nil {
		// The mcache should always be available during normal operation.
		// 在正常操作期间，mcache 应该始终可用
		throw("profilealloc called without a P or outside bootstrapping")
	}
	// Set the next sampling point for heap profiling.
	// 设置堆分析的下一个采样点
	c.nextSample = nextSample()
	// Record the allocation in the memory profile.
	// 在内存配置文件中记录此次分配
	mProf_Malloc(mp, x, size)
}

// nextSample returns the next sampling point for heap profiling. The goal is
// to sample allocations on average every MemProfileRate bytes, but with a
// completely random distribution over the allocation timeline; this
// corresponds to a Poisson process with parameter MemProfileRate. In Poisson
// processes, the distance between two samples follows the exponential
// distribution (exp(MemProfileRate)), so the best return value is a random
// number taken from an exponential distribution whose mean is MemProfileRate.
//
// nextSample 返回堆分析的下一个采样点。目标是平均每 MemProfileRate 字节采样一次分配，
// 但在分配时间线上完全随机分布；这对应于参数为 MemProfileRate 的泊松过程。
// 在泊松过程中，两个采样点之间的距离遵循指数分布(exp(MemProfileRate))，
// 因此最佳返回值是从均值为 MemProfileRate 的指数分布中取出的随机数。
func nextSample() uintptr {
	if MemProfileRate == 1 {
		// Callers assign our return value to
		// mcache.next_sample, but next_sample is not used
		// when the rate is 1. So avoid the math below and
		// just return something.
		//
		// 调用者将我们的返回值赋给 mcache.next_sample，
		// 但当速率为 1 时不使用 next_sample。
		// 因此避免下面的数学计算，直接返回某个值。
		return 0
	}
	if GOOS == "plan9" {
		// Plan 9 doesn't support floating point in note handler.
		// Plan 9 在 note handler 中不支持浮点数。
		if gp := getg(); gp == gp.m.gsignal {
			return nextSampleNoFP()
		}
	}

	return uintptr(fastexprand(MemProfileRate))
}

// fastexprand returns a random number from an exponential distribution with
// the specified mean.
// fastexprand 返回一个来自指数分布的随机数，其均值为指定的 mean。
func fastexprand(mean int) int32 {
	// Avoid overflow. Maximum possible step is
	// -ln(1/(1<<randomBitCount)) * mean, approximately 20 * mean.
	// 避免溢出。最大可能的步长是 -ln(1/(1<<randomBitCount)) * mean，大约为 20 * mean。
	switch {
	case mean > 0x7000000:
		mean = 0x7000000
	case mean == 0:
		return 0
	}

	// Take a random sample of the exponential distribution exp(-mean*x).
	// The probability distribution function is mean*exp(-mean*x), so the CDF is
	// p = 1 - exp(-mean*x), so
	// q = 1 - p == exp(-mean*x)
	// log_e(q) = -mean*x
	// -log_e(q)/mean = x
	// x = -log_e(q) * mean
	// x = log_2(q) * (-log_e(2)) * mean    ; Using log_2 for efficiency
	// 从指数分布 exp(-mean*x) 中取一个随机样本。
	// 概率分布函数是 mean*exp(-mean*x)，所以累积分布函数是
	// p = 1 - exp(-mean*x)，所以
	// q = 1 - p == exp(-mean*x)
	// log_e(q) = -mean*x
	// -log_e(q)/mean = x
	// x = -log_e(q) * mean
	// x = log_2(q) * (-log_e(2)) * mean    ; 使用 log_2 以提高效率
	const randomBitCount = 26
	q := cheaprandn(1<<randomBitCount) + 1
	qlog := fastlog2(float64(q)) - randomBitCount
	if qlog > 0 {
		qlog = 0
	}
	const minusLog2 = -0.6931471805599453 // -ln(2)
	return int32(qlog*(minusLog2*float64(mean))) + 1
}

// nextSampleNoFP is similar to nextSample, but uses older,
// simpler code to avoid floating point.
// nextSampleNoFP 与 nextSample 类似，但使用更老、更简单的代码来避免浮点数运算。
func nextSampleNoFP() uintptr {
	// Set first allocation sample size.
	// 设置首次分配采样大小。
	rate := MemProfileRate
	if rate > 0x3fffffff { // make 2*rate not overflow
		// 确保 2*rate 不会溢出
		rate = 0x3fffffff
	}
	if rate != 0 {
		// 如果采样率不为0，返回一个0到2*rate之间的随机数
		return uintptr(cheaprandn(uint32(2 * rate)))
	}
	return 0
}

// persistentAlloc represents a persistent memory allocator.
// It maintains a base pointer and offset for memory allocation.
// persistentAlloc 表示一个持久内存分配器。
// 它维护一个基础指针和偏移量用于内存分配。
type persistentAlloc struct {
	base *notInHeap // 基础内存块的指针
	off  uintptr    // 当前分配位置的偏移量
}

// globalAlloc is a global instance of persistentAlloc with a mutex for synchronization.
// globalAlloc 是一个带有互斥锁的全局持久分配器实例，用于同步访问。
var globalAlloc struct {
	mutex           // 用于同步的互斥锁
	persistentAlloc // 持久分配器
}

// persistentChunkSize is the number of bytes we allocate when we grow
// a persistentAlloc.
// persistentChunkSize 是当我们扩展 persistentAlloc 时分配的字节数。
const persistentChunkSize = 256 << 10

// persistentChunks is a list of all the persistent chunks we have
// allocated. The list is maintained through the first word in the
// persistent chunk. This is updated atomically.
// persistentChunks 是我们已分配的所有持久内存块的列表。
// 这个列表通过持久内存块的第一个字来维护，并且是原子更新的。
var persistentChunks *notInHeap

// Wrapper around sysAlloc that can allocate small chunks.
// There is no associated free operation.
// Intended for things like function/type/debug-related persistent data.
// If align is 0, uses default align (currently 8).
// The returned memory will be zeroed.
// sysStat must be non-nil.
//
// Consider marking persistentalloc'd types not in heap by embedding
// runtime/internal/sys.NotInHeap.
// persistentalloc 是 sysAlloc 的包装器，可以分配小块内存。
// 没有相关的释放操作。
// 用于函数/类型/调试相关的持久数据。
// 如果 align 为 0，使用默认对齐（当前为 8）。
// 返回的内存将被清零。
// sysStat 必须非空。
//
// 考虑通过嵌入 runtime/internal/sys.NotInHeap 来标记 persistentalloc 分配的类型不在堆中。
func persistentalloc(size, align uintptr, sysStat *sysMemStat) unsafe.Pointer {
	var p *notInHeap
	systemstack(func() {
		p = persistentalloc1(size, align, sysStat)
	})
	return unsafe.Pointer(p)
}

// Must run on system stack because stack growth can (re)invoke it.
// See issue 9174.
// 必须在系统栈上运行，因为栈增长可能会（重新）调用它。
// 参见 issue 9174。
//
//go:systemstack
func persistentalloc1(size, align uintptr, sysStat *sysMemStat) *notInHeap {
	const (
		maxBlock = 64 << 10 // VM reservation granularity is 64K on windows
		// Windows 上的 VM 预留粒度是 64K
	)

	if size == 0 {
		throw("persistentalloc: size == 0")
	}
	if align != 0 {
		if align&(align-1) != 0 {
			throw("persistentalloc: align is not a power of 2")
		}
		if align > _PageSize {
			throw("persistentalloc: align is too large")
		}
	} else {
		align = 8
	}

	// 如果请求的大小超过最大块大小，直接使用 sysAlloc 分配
	if size >= maxBlock {
		return (*notInHeap)(sysAlloc(size, sysStat))
	}

	// 获取当前 M 并确定使用哪个持久分配器
	mp := acquirem()
	var persistent *persistentAlloc
	if mp != nil && mp.p != 0 {
		// 如果当前 P 存在，使用 P 的持久分配器
		persistent = &mp.p.ptr().palloc
	} else {
		// 否则使用全局持久分配器，需要加锁
		lock(&globalAlloc.mutex)
		persistent = &globalAlloc.persistentAlloc
	}

	// 对齐当前偏移量
	persistent.off = alignUp(persistent.off, align)
	// 如果当前块空间不足或没有分配块，分配新的块
	if persistent.off+size > persistentChunkSize || persistent.base == nil {
		persistent.base = (*notInHeap)(sysAlloc(persistentChunkSize, &memstats.other_sys))
		if persistent.base == nil {
			if persistent == &globalAlloc.persistentAlloc {
				unlock(&globalAlloc.mutex)
			}
			throw("runtime: cannot allocate memory")
		}

		// Add the new chunk to the persistentChunks list.
		// 将新块添加到 persistentChunks 列表中
		for {
			chunks := uintptr(unsafe.Pointer(persistentChunks))
			*(*uintptr)(unsafe.Pointer(persistent.base)) = chunks
			if atomic.Casuintptr((*uintptr)(unsafe.Pointer(&persistentChunks)), chunks, uintptr(unsafe.Pointer(persistent.base))) {
				break
			}
		}
		// 设置新块的起始偏移量，考虑对齐要求
		persistent.off = alignUp(goarch.PtrSize, align)
	}

	// 计算并返回分配的内存地址
	p := persistent.base.add(persistent.off)
	persistent.off += size
	releasem(mp)
	if persistent == &globalAlloc.persistentAlloc {
		unlock(&globalAlloc.mutex)
	}

	// 更新内存统计信息
	if sysStat != &memstats.other_sys {
		sysStat.add(int64(size))
		memstats.other_sys.add(-int64(size))
	}
	return p
}

// inPersistentAlloc reports whether p points to memory allocated by
// persistentalloc. This must be nosplit because it is called by the
// cgo checker code, which is called by the write barrier code.
//
// inPersistentAlloc 报告 p 是否指向由 persistentalloc 分配的内存。
// 这个函数必须是 nosplit 的，因为它被 cgo 检查器代码调用，
// 而 cgo 检查器代码又被写屏障代码调用。
//
//go:nosplit
func inPersistentAlloc(p uintptr) bool {
	// 原子加载 persistentChunks 链表的头节点
	chunk := atomic.Loaduintptr((*uintptr)(unsafe.Pointer(&persistentChunks)))
	// 遍历链表中的所有块
	for chunk != 0 {
		// 检查指针 p 是否在当前块的地址范围内
		if p >= chunk && p < chunk+persistentChunkSize {
			return true
		}
		// 移动到链表中的下一个块
		chunk = *(*uintptr)(unsafe.Pointer(chunk))
	}
	// 如果遍历完所有块都没有找到匹配的，返回 false
	return false
}

// linearAlloc is a simple linear allocator that pre-reserves a region
// of memory and then optionally maps that region into the Ready state
// as needed.
//
// The caller is responsible for locking.
// linearAlloc 是一个简单的线性分配器，它预先保留一块内存区域，
// 然后根据需要选择性地将该区域映射到 Ready 状态。
//
// 调用者负责加锁。
type linearAlloc struct {
	next   uintptr // next free byte
	mapped uintptr // one byte past end of mapped space
	end    uintptr // end of reserved space

	mapMemory bool // transition memory from Reserved to Ready if true
	// next: 下一个可用字节的地址
	// mapped: 已映射空间结束后的第一个字节地址
	// end: 保留空间的结束地址
	// mapMemory: 如果为 true，则将内存从 Reserved 状态转换到 Ready 状态
}

func (l *linearAlloc) init(base, size uintptr, mapMemory bool) {
	if base+size < base {
		// Chop off the last byte. The runtime isn't prepared
		// to deal with situations where the bounds could overflow.
		// Leave that memory reserved, though, so we don't map it
		// later.
		// 如果 base+size 小于 base，说明发生了溢出。
		// 运行时没有准备好处理边界溢出的情况。
		// 不过我们仍然保留这块内存，只是不映射它。
		size -= 1
	}
	l.next, l.mapped = base, base
	l.end = base + size
	l.mapMemory = mapMemory
	// 初始化分配器的状态：
	// - next 和 mapped 都设置为起始地址 base
	// - end 设置为 base + size，表示保留空间的结束地址
	// - mapMemory 设置为传入的 mapMemory 值，决定是否将内存映射到 Ready 状态
}

func (l *linearAlloc) alloc(size, align uintptr, sysStat *sysMemStat) unsafe.Pointer {
	// 将下一个可用地址按照对齐要求对齐
	p := alignUp(l.next, align)
	// 检查对齐后的地址加上请求的大小是否超出预留空间
	if p+size > l.end {
		return nil
	}
	// 更新下一个可用地址
	l.next = p + size
	// 计算需要映射的物理页边界
	if pEnd := alignUp(l.next-1, physPageSize); pEnd > l.mapped {
		if l.mapMemory {
			// Transition from Reserved to Prepared to Ready.
			// 从 Reserved 状态转换到 Prepared 再到 Ready 状态。
			// 计算需要映射的新内存大小
			n := pEnd - l.mapped
			// 将内存从 Reserved 状态转换到 Prepared 状态
			sysMap(unsafe.Pointer(l.mapped), n, sysStat)
			// 将内存从 Prepared 状态转换到 Ready 状态
			sysUsed(unsafe.Pointer(l.mapped), n, n)
		}
		// 更新已映射空间的结束地址
		l.mapped = pEnd
	}
	// 返回分配的内存地址
	return unsafe.Pointer(p)
}

// notInHeap is off-heap memory allocated by a lower-level allocator
// like sysAlloc or persistentAlloc.
//
// In general, it's better to use real types which embed
// runtime/internal/sys.NotInHeap, but this serves as a generic type
// for situations where that isn't possible (like in the allocators).
//
// TODO: Use this as the return type of sysAlloc, persistentAlloc, etc?
// notInHeap 是由低级分配器（如 sysAlloc 或 persistentAlloc）分配的堆外内存。
//
// 通常，最好使用嵌入了 runtime/internal/sys.NotInHeap 的真实类型，
// 但在不可能这样做的情况下（比如在分配器中），这作为一个通用类型使用。
//
// TODO: 是否应该将其用作 sysAlloc、persistentAlloc 等的返回类型？
type notInHeap struct{ _ sys.NotInHeap }

// add 方法将指针向前移动指定的字节数，并返回新的指针
// 用于在堆外内存中进行指针算术运算
func (p *notInHeap) add(bytes uintptr) *notInHeap {
	return (*notInHeap)(unsafe.Pointer(uintptr(unsafe.Pointer(p)) + bytes))
}

// computeRZlog computes the size of the redzone.
// Refer to the implementation of the compiler-rt.
// computeRZlog 计算红区的大小。
// 参考 compiler-rt 的实现。
// 红区是用于检测内存访问越界的保护区域，大小根据用户请求的内存大小动态调整
func computeRZlog(userSize uintptr) uintptr {
	switch {
	case userSize <= (64 - 16):
		return 16 << 0 // 16 字节红区
	case userSize <= (128 - 32):
		return 16 << 1 // 32 字节红区
	case userSize <= (512 - 64):
		return 16 << 2 // 64 字节红区
	case userSize <= (4096 - 128):
		return 16 << 3 // 128 字节红区
	case userSize <= (1<<14)-256:
		return 16 << 4 // 256 字节红区
	case userSize <= (1<<15)-512:
		return 16 << 5 // 512 字节红区
	case userSize <= (1<<16)-1024:
		return 16 << 6 // 1024 字节红区
	default:
		return 16 << 7 // 2048 字节红区
	}
}
