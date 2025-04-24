// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/cpu"
	"internal/goarch"
	"internal/runtime/atomic"
	"unsafe"
)

// A spanSet is a set of *mspans.
//
// spanSet is safe for concurrent push and pop operations.
type spanSet struct {
	// A spanSet is a two-level data structure consisting of a
	// growable spine that points to fixed-sized blocks. The spine
	// can be accessed without locks, but adding a block or
	// growing it requires taking the spine lock.
	//
	// Because each mspan covers at least 8K of heap and takes at
	// most 8 bytes in the spanSet, the growth of the spine is
	// quite limited.
	//
	// The spine and all blocks are allocated off-heap, which
	// allows this to be used in the memory manager and avoids the
	// need for write barriers on all of these. spanSetBlocks are
	// managed in a pool, though never freed back to the operating
	// system. We never release spine memory because there could be
	// concurrent lock-free access and we're likely to reuse it
	// anyway. (In principle, we could do this during STW.)

	// spanSet 是一个两级数据结构，由一个可增长的 spine（脊柱）组成，
	// spine 指向固定大小的块。spine 可以在没有锁的情况下访问，
	// 但添加块或增长 spine 需要获取 spine 锁。
	//
	// 因为每个 mspan 至少覆盖 8K 的堆空间，在 spanSet 中最多占用 8 字节，
	// 所以 spine 的增长是相当有限的。
	//
	// spine 和所有块都是在堆外分配的，这允许它在内存管理器中使用，
	// 并避免了对所有这些进行写屏障的需要。spanSetBlocks 在池中管理，
	// 但永远不会释放回操作系统。我们从不释放 spine 内存，因为可能存在
	// 并发的无锁访问，而且我们很可能会重用它。
	// （原则上，我们可以在 STW 期间这样做。）

	spineLock mutex
	spine     atomicSpanSetSpinePointer // *[N]atomic.Pointer[spanSetBlock]
	spineLen  atomic.Uintptr            // Spine array length
	spineCap  uintptr                   // Spine array cap, accessed under spineLock

	// spineLock 是一个互斥锁，用于保护 spine 数组的并发访问
	// spine 是一个原子指针，指向 spanSetBlock 指针数组，用于存储 spanSet 的块
	// spineLen 是一个原子无符号整数，表示 spine 数组的当前长度
	// spineCap 是一个无符号整数，表示 spine 数组的容量，只能在持有 spineLock 时访问

	// index is the head and tail of the spanSet in a single field.
	// The head and the tail both represent an index into the logical
	// concatenation of all blocks, with the head always behind or
	// equal to the tail (indicating an empty set). This field is
	// always accessed atomically.
	//
	// The head and the tail are only 32 bits wide, which means we
	// can only support up to 2^32 pushes before a reset. If every
	// span in the heap were stored in this set, and each span were
	// the minimum size (1 runtime page, 8 KiB), then roughly the
	// smallest heap which would be unrepresentable is 32 TiB in size.

	// index 是 spanSet 的头和尾在单个字段中的表示。
	// 头和尾都表示所有块的逻辑连接中的索引，头总是在尾的后面或等于尾
	// （表示一个空集合）。这个字段总是以原子方式访问。
	//
	// 头和尾只有 32 位宽，这意味着在重置之前我们最多只能支持 2^32 次推送。
	// 如果堆中的每个 span 都存储在这个集合中，并且每个 span 都是最小大小
	// （1 个运行时页，8 KiB），那么粗略地说，最小的无法表示的堆大小是 32 TiB。
	index atomicHeadTailIndex
}

const (
	spanSetBlockEntries = 512 // 4KB on 64-bit
	spanSetInitSpineCap = 256 // Enough for 1GB heap on 64-bit
)

type spanSetBlock struct {
	// Free spanSetBlocks are managed via a lock-free stack.
	// 空闲的 spanSetBlocks 通过无锁栈进行管理。
	lfnode

	// popped is the number of pop operations that have occurred on
	// this block. This number is used to help determine when a block
	// may be safely recycled.
	// popped 表示在这个块上发生的 pop 操作次数。
	// 这个数字用于帮助确定一个块何时可以安全地回收。
	popped atomic.Uint32

	// spans is the set of spans in this block.
	// spans 是这个块中的 span 集合。
	spans [spanSetBlockEntries]atomicMSpanPointer
}

// push adds span s to buffer b. push is safe to call concurrently
// with other push and pop operations.
// push 将 span s 添加到缓冲区 b 中。push 可以安全地与其他的 push 和 pop 操作并发调用。
func (b *spanSet) push(s *mspan) {
	// Obtain our slot.
	// 获取我们的槽位
	cursor := uintptr(b.index.incTail().tail() - 1)
	top, bottom := cursor/spanSetBlockEntries, cursor%spanSetBlockEntries

	// Do we need to add a block?
	// 我们需要添加一个新的块吗？
	spineLen := b.spineLen.Load()
	var block *spanSetBlock
retry:
	if top < spineLen {
		block = b.spine.Load().lookup(top).Load()
	} else {
		// Add a new block to the spine, potentially growing
		// the spine.
		// 向 spine 添加一个新的块，可能会增长 spine
		lock(&b.spineLock)
		// spineLen cannot change until we release the lock,
		// but may have changed while we were waiting.
		// 在我们释放锁之前，spineLen 不会改变，
		// 但在我们等待期间可能已经改变
		spineLen = b.spineLen.Load()
		if top < spineLen {
			unlock(&b.spineLock)
			goto retry
		}

		spine := b.spine.Load()
		if spineLen == b.spineCap {
			// Grow the spine.
			// 增长 spine
			newCap := b.spineCap * 2
			if newCap == 0 {
				newCap = spanSetInitSpineCap
			}
			newSpine := persistentalloc(newCap*goarch.PtrSize, cpu.CacheLineSize, &memstats.gcMiscSys)
			if b.spineCap != 0 {
				// Blocks are allocated off-heap, so
				// no write barriers.
				// 块是在堆外分配的，所以不需要写屏障
				memmove(newSpine, spine.p, b.spineCap*goarch.PtrSize)
			}
			spine = spanSetSpinePointer{newSpine}

			// Spine is allocated off-heap, so no write barrier.
			// Spine 是在堆外分配的，所以不需要写屏障
			b.spine.StoreNoWB(spine)
			b.spineCap = newCap
			// We can't immediately free the old spine
			// since a concurrent push with a lower index
			// could still be reading from it. We let it
			// leak because even a 1TB heap would waste
			// less than 2MB of memory on old spines. If
			// this is a problem, we could free old spines
			// during STW.
			// 我们不能立即释放旧的 spine，因为具有较低索引的并发 push
			// 可能仍在从中读取。我们让它泄漏，因为即使是 1TB 的堆
			// 在旧的 spine 上也只会浪费不到 2MB 的内存。如果这是一个问题，
			// 我们可以在 STW 期间释放旧的 spine
		}

		// Allocate a new block from the pool.
		// 从池中分配一个新的块
		block = spanSetBlockPool.alloc()

		// Add it to the spine.
		// Blocks are allocated off-heap, so no write barrier.
		// 将其添加到 spine
		// 块是在堆外分配的，所以不需要写屏障
		spine.lookup(top).StoreNoWB(block)
		b.spineLen.Store(spineLen + 1)
		unlock(&b.spineLock)
	}

	// We have a block. Insert the span atomically, since there may be
	// concurrent readers via the block API.
	// 我们有了一个块。原子地插入 span，因为可能通过块 API 有并发读取者
	block.spans[bottom].StoreNoWB(s)
}

// pop removes and returns a span from buffer b, or nil if b is empty.
// pop is safe to call concurrently with other pop and push operations.
// pop 从缓冲区 b 中移除并返回一个 span，如果 b 为空则返回 nil。
// pop 可以安全地与其他的 pop 和 push 操作并发调用。
func (b *spanSet) pop() *mspan {
	var head, tail uint32
claimLoop:
	for {
		headtail := b.index.load()
		head, tail = headtail.split()
		if head >= tail {
			// The buf is empty, as far as we can tell.
			// 就我们所能看到的，缓冲区是空的。
			return nil
		}
		// Check if the head position we want to claim is actually
		// backed by a block.
		// 检查我们想要声明的头部位置是否确实有块支持。
		spineLen := b.spineLen.Load()
		if spineLen <= uintptr(head)/spanSetBlockEntries {
			// We're racing with a spine growth and the allocation of
			// a new block (and maybe a new spine!), and trying to grab
			// the span at the index which is currently being pushed.
			// Instead of spinning, let's just notify the caller that
			// there's nothing currently here. Spinning on this is
			// almost definitely not worth it.
			// 我们正在与 spine 增长和新块（可能还有新的 spine！）的分配竞争，
			// 并试图获取当前正在被推送的索引处的 span。
			// 与其自旋，不如通知调用者当前这里什么都没有。
			// 在这种情况下自旋几乎肯定不值得。
			return nil
		}
		// Try to claim the current head by CASing in an updated head.
		// This may fail transiently due to a push which modifies the
		// tail, so keep trying while the head isn't changing.
		// 尝试通过 CAS 更新头部来声明当前头部。
		// 由于推送操作可能会修改尾部，这可能会暂时失败，
		// 所以在头部没有改变的情况下继续尝试。
		want := head
		for want == head {
			if b.index.cas(headtail, makeHeadTailIndex(want+1, tail)) {
				break claimLoop
			}
			headtail = b.index.load()
			head, tail = headtail.split()
		}
		// We failed to claim the spot we were after and the head changed,
		// meaning a popper got ahead of us. Try again from the top because
		// the buf may not be empty.
		// 我们未能声明我们想要的位点，并且头部发生了变化，
		// 这意味着一个 popper 领先于我们。从头开始重试，
		// 因为缓冲区可能不是空的。
	}
	top, bottom := head/spanSetBlockEntries, head%spanSetBlockEntries

	// We may be reading a stale spine pointer, but because the length
	// grows monotonically and we've already verified it, we'll definitely
	// be reading from a valid block.
	// 我们可能在读取一个过时的 spine 指针，但由于长度是单调增长的，
	// 并且我们已经验证了它，我们一定会从一个有效的块中读取。
	blockp := b.spine.Load().lookup(uintptr(top))

	// Given that the spine length is correct, we know we will never
	// see a nil block here, since the length is always updated after
	// the block is set.
	// 由于 spine 长度是正确的，我们知道这里永远不会看到 nil 块，
	// 因为长度总是在块设置后更新。
	block := blockp.Load()
	s := block.spans[bottom].Load()
	for s == nil {
		// We raced with the span actually being set, but given that we
		// know a block for this span exists, the race window here is
		// extremely small. Try again.
		// 我们与 span 的实际设置发生了竞争，但考虑到我们知道这个 span 的块存在，
		// 这里的竞争窗口非常小。重试。
		s = block.spans[bottom].Load()
	}
	// Clear the pointer. This isn't strictly necessary, but defensively
	// avoids accidentally re-using blocks which could lead to memory
	// corruption. This way, we'll get a nil pointer access instead.
	// 清除指针。这不是严格必要的，但防御性地避免意外重用块，
	// 这可能导致内存损坏。这样，我们会得到一个 nil 指针访问。
	block.spans[bottom].StoreNoWB(nil)

	// Increase the popped count. If we are the last possible popper
	// in the block (note that bottom need not equal spanSetBlockEntries-1
	// due to races) then it's our responsibility to free the block.
	//
	// If we increment popped to spanSetBlockEntries, we can be sure that
	// we're the last popper for this block, and it's thus safe to free it.
	// Every other popper must have crossed this barrier (and thus finished
	// popping its corresponding mspan) by the time we get here. Because
	// we're the last popper, we also don't have to worry about concurrent
	// pushers (there can't be any). Note that we may not be the popper
	// which claimed the last slot in the block, we're just the last one
	// to finish popping.
	// 增加已弹出计数。如果我们是块中最后一个可能的 popper
	//（注意，由于竞争，bottom 不必等于 spanSetBlockEntries-1），
	// 那么释放块就是我们的责任。
	//
	// 如果我们将 popped 增加到 spanSetBlockEntries，我们可以确定
	// 我们是这个块的最后一个 popper，因此可以安全地释放它。
	// 到我们到达这里时，其他所有 popper 都必须已经跨过这个障碍
	//（因此已经完成了其对应 mspan 的弹出）。
	// 因为我们是最后一个 popper，我们也不必担心并发的 pusher
	//（不可能有任何 pusher）。
	// 注意，我们可能不是声明块中最后一个槽位的 popper，
	// 我们只是最后一个完成弹出的。
	if block.popped.Add(1) == spanSetBlockEntries {
		// Clear the block's pointer.
		// 清除块的指针。
		blockp.StoreNoWB(nil)

		// Return the block to the block pool.
		// 将块返回到块池。
		spanSetBlockPool.free(block)
	}
	return s
}

// reset resets a spanSet which is empty. It will also clean up
// any left over blocks.
//
// Throws if the buf is not empty.
//
// reset may not be called concurrently with any other operations
// on the span set.
// reset 重置一个空的 spanSet。它还会清理任何剩余的块。
//
// 如果缓冲区不为空则抛出异常。
//
// reset 不能与 span set 上的任何其他操作并发调用。
func (b *spanSet) reset() {
	head, tail := b.index.load().split()
	if head < tail {
		print("head = ", head, ", tail = ", tail, "\n")
		throw("attempt to clear non-empty span set")
	}
	top := head / spanSetBlockEntries
	if uintptr(top) < b.spineLen.Load() {
		// If the head catches up to the tail and the set is empty,
		// we may not clean up the block containing the head and tail
		// since it may be pushed into again. In order to avoid leaking
		// memory since we're going to reset the head and tail, clean
		// up such a block now, if it exists.
		// 如果 head 追上了 tail 且集合为空，
		// 我们可能不会清理包含 head 和 tail 的块，
		// 因为它可能会再次被推入。为了避免内存泄漏，
		// 既然我们要重置 head 和 tail，现在就清理这样的块（如果存在的话）。
		blockp := b.spine.Load().lookup(uintptr(top))
		block := blockp.Load()
		if block != nil {
			// Check the popped value.
			// 检查已弹出的值。
			if block.popped.Load() == 0 {
				// popped should never be zero because that means we have
				// pushed at least one value but not yet popped if this
				// block pointer is not nil.
				// popped 不应该为零，因为这意味着如果这个块指针不为 nil，
				// 我们已经至少推入了一个值但还没有弹出。
				throw("span set block with unpopped elements found in reset")
			}
			if block.popped.Load() == spanSetBlockEntries {
				// popped should also never be equal to spanSetBlockEntries
				// because the last popper should have made the block pointer
				// in this slot nil.
				// popped 也不应该等于 spanSetBlockEntries，
				// 因为最后一个 popper 应该已经将这个槽位中的块指针设为 nil。
				throw("fully empty unfreed span set block found in reset")
			}

			// Clear the pointer to the block.
			// 清除指向块的指针。
			blockp.StoreNoWB(nil)

			// Return the block to the block pool.
			// 将块返回到块池。
			spanSetBlockPool.free(block)
		}
	}
	b.index.reset()
	b.spineLen.Store(0)
}

// atomicSpanSetSpinePointer is an atomically-accessed spanSetSpinePointer.
// atomicSpanSetSpinePointer 是一个原子访问的 spanSetSpinePointer。
//
// It has the same semantics as atomic.UnsafePointer.
// 它具有与 atomic.UnsafePointer 相同的语义。
type atomicSpanSetSpinePointer struct {
	a atomic.UnsafePointer
}

// Loads the spanSetSpinePointer and returns it.
// 加载 spanSetSpinePointer 并返回它。
//
// It has the same semantics as atomic.UnsafePointer.
// 它具有与 atomic.UnsafePointer 相同的语义。
func (s *atomicSpanSetSpinePointer) Load() spanSetSpinePointer {
	return spanSetSpinePointer{s.a.Load()}
}

// Stores the spanSetSpinePointer.
// 存储 spanSetSpinePointer。
//
// It has the same semantics as [atomic.UnsafePointer].
// 它具有与 atomic.UnsafePointer 相同的语义。
func (s *atomicSpanSetSpinePointer) StoreNoWB(p spanSetSpinePointer) {
	s.a.StoreNoWB(p.p)
}

// spanSetSpinePointer represents a pointer to a contiguous block of atomic.Pointer[spanSetBlock].
// spanSetSpinePointer 表示指向一个连续的 atomic.Pointer[spanSetBlock] 块的指针。
type spanSetSpinePointer struct {
	p unsafe.Pointer
}

// lookup returns &s[idx].
// lookup 返回 &s[idx]，即返回指向索引位置元素的指针。
// 通过将基础指针加上偏移量(索引*指针大小)来计算目标元素的地址。
func (s spanSetSpinePointer) lookup(idx uintptr) *atomic.Pointer[spanSetBlock] {
	return (*atomic.Pointer[spanSetBlock])(add(s.p, goarch.PtrSize*idx))
}

// spanSetBlockPool is a global pool of spanSetBlocks.
// spanSetBlockPool 是一个全局的 spanSetBlocks 池。
var spanSetBlockPool spanSetBlockAlloc

// spanSetBlockAlloc represents a concurrent pool of spanSetBlocks.
// spanSetBlockAlloc 表示一个并发的 spanSetBlocks 池。
// 它使用无锁栈(lfstack)来管理 spanSetBlock 的分配和回收。
type spanSetBlockAlloc struct {
	stack lfstack
}

// alloc tries to grab a spanSetBlock out of the pool, and if it fails
// persistentallocs a new one and returns it.
// alloc 尝试从池中获取一个 spanSetBlock，如果失败则通过 persistentalloc 分配一个新的并返回。
// 首先尝试从无锁栈中弹出(pop)一个块，如果成功则直接返回。
// 如果栈为空，则通过 persistentalloc 分配一个新的块，并确保内存对齐到 CPU 缓存行大小。
func (p *spanSetBlockAlloc) alloc() *spanSetBlock {
	if s := (*spanSetBlock)(p.stack.pop()); s != nil {
		return s
	}
	return (*spanSetBlock)(persistentalloc(unsafe.Sizeof(spanSetBlock{}), cpu.CacheLineSize, &memstats.gcMiscSys))
}

// free returns a spanSetBlock back to the pool.
// free 将 spanSetBlock 返回到池中。
// 首先将块的 popped 标志重置为 0，表示该块可以被重新使用。
// 然后将块的 lfnode 推入(push)到无锁栈中，使其可以被其他分配请求重用。
func (p *spanSetBlockAlloc) free(block *spanSetBlock) {
	block.popped.Store(0)
	p.stack.push(&block.lfnode)
}

// headTailIndex represents a combined 32-bit head and 32-bit tail
// of a queue into a single 64-bit value.
// headTailIndex 表示将队列的32位头部和32位尾部组合成一个64位值。
type headTailIndex uint64

// makeHeadTailIndex creates a headTailIndex value from a separate
// head and tail.
// makeHeadTailIndex 从独立的头部和尾部创建一个 headTailIndex 值。
// 通过将头部左移32位并与尾部进行按位或操作来组合这两个值。
func makeHeadTailIndex(head, tail uint32) headTailIndex {
	return headTailIndex(uint64(head)<<32 | uint64(tail))
}

// head returns the head of a headTailIndex value.
// head 返回 headTailIndex 值中的头部。
// 通过将64位值右移32位并转换为32位无符号整数来获取头部值。
func (h headTailIndex) head() uint32 {
	return uint32(h >> 32)
}

// tail returns the tail of a headTailIndex value.
// tail 返回 headTailIndex 值中的尾部。
// 通过将64位值直接转换为32位无符号整数来获取尾部值。
func (h headTailIndex) tail() uint32 {
	return uint32(h)
}

// split splits the headTailIndex value into its parts.
// split 将 headTailIndex 值分解为其组成部分。
// 返回头部和尾部两个32位无符号整数。
func (h headTailIndex) split() (head uint32, tail uint32) {
	return h.head(), h.tail()
}

// atomicHeadTailIndex is an atomically-accessed headTailIndex.
// atomicHeadTailIndex 是一个原子访问的 headTailIndex。
type atomicHeadTailIndex struct {
	u atomic.Uint64
}

// load atomically reads a headTailIndex value.
// load 原子地读取 headTailIndex 值。
func (h *atomicHeadTailIndex) load() headTailIndex {
	return headTailIndex(h.u.Load())
}

// cas atomically compares-and-swaps a headTailIndex value.
// cas 原子地比较并交换 headTailIndex 值。
func (h *atomicHeadTailIndex) cas(old, new headTailIndex) bool {
	return h.u.CompareAndSwap(uint64(old), uint64(new))
}

// incHead atomically increments the head of a headTailIndex.
// incHead 原子地增加 headTailIndex 的头部值。
func (h *atomicHeadTailIndex) incHead() headTailIndex {
	return headTailIndex(h.u.Add(1 << 32))
}

// decHead atomically decrements the head of a headTailIndex.
// decHead 原子地减少 headTailIndex 的头部值。
func (h *atomicHeadTailIndex) decHead() headTailIndex {
	return headTailIndex(h.u.Add(-(1 << 32)))
}

// incTail atomically increments the tail of a headTailIndex.
// incTail 原子地增加 headTailIndex 的尾部值。
func (h *atomicHeadTailIndex) incTail() headTailIndex {
	ht := headTailIndex(h.u.Add(1))
	// Check for overflow.
	// 检查是否溢出。
	if ht.tail() == 0 {
		print("runtime: head = ", ht.head(), ", tail = ", ht.tail(), "\n")
		throw("headTailIndex overflow")
	}
	return ht
}

// reset clears the headTailIndex to (0, 0).
// reset 将 headTailIndex 重置为 (0, 0)。
func (h *atomicHeadTailIndex) reset() {
	h.u.Store(0)
}

// atomicMSpanPointer is an atomic.Pointer[mspan]. Can't use generics because it's NotInHeap.
// atomicMSpanPointer 是一个原子指针类型，指向 mspan。由于 mspan 是 NotInHeap 类型，所以不能使用泛型。
type atomicMSpanPointer struct {
	p atomic.UnsafePointer
}

// Load returns the *mspan.
// Load 返回指向 mspan 的指针。
func (p *atomicMSpanPointer) Load() *mspan {
	return (*mspan)(p.p.Load())
}

// Store stores an *mspan.
// Store 存储一个指向 mspan 的指针。
func (p *atomicMSpanPointer) StoreNoWB(s *mspan) {
	p.p.StoreNoWB(unsafe.Pointer(s))
}
