// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Fixed-size object allocator. Returned memory is not zeroed.
//
// See malloc.go for overview.

package runtime

import (
	"runtime/internal/sys"
	"unsafe"
)

// fixalloc is a simple free-list allocator for fixed size objects.
// Malloc uses a FixAlloc wrapped around sysAlloc to manage its
// mcache and mspan objects.
//
// Memory returned by fixalloc.alloc is zeroed by default, but the
// caller may take responsibility for zeroing allocations by setting
// the zero flag to false. This is only safe if the memory never
// contains heap pointers.
//
// The caller is responsible for locking around FixAlloc calls.
// Callers can keep state in the object but the first word is
// smashed by freeing and reallocating.
//
// Consider marking fixalloc'd types not in heap by embedding
// runtime/internal/sys.NotInHeap.

// fixalloc 是一个用于固定大小对象的简单空闲列表分配器。
// Malloc 使用包装了 sysAlloc 的 FixAlloc 来管理其 mcache 和 mspan 对象。
//
// 默认情况下，fixalloc.alloc 返回的内存会被清零，但调用者可以通过将 zero 标志设置为 false
// 来负责清零分配。这只有在内存从不包含堆指针的情况下才是安全的。
//
// 调用者负责在 FixAlloc 调用周围加锁。
// 调用者可以在对象中保持状态，但第一个字会在释放和重新分配时被覆盖。
//
// 考虑通过嵌入 runtime/internal/sys.NotInHeap 来标记不在堆中的 fixalloc 类型。
type fixalloc struct {
	size   uintptr                     // 分配对象的大小
	first  func(arg, p unsafe.Pointer) // 当 p 第一次被返回时调用的函数
	arg    unsafe.Pointer              // 传递给 first 函数的参数
	list   *mlink                      // 空闲对象链表
	chunk  uintptr                     // 当前内存块的地址，使用 uintptr 而不是 unsafe.Pointer 以避免写屏障
	nchunk uint32                      // 当前内存块中剩余的字节数
	nalloc uint32                      // 新内存块的大小（以字节为单位）
	inuse  uintptr                     // 当前已使用的字节数
	stat   *sysMemStat                 // 内存统计信息
	zero   bool                        // 是否清零分配的内存
}

// A generic linked list of blocks.  (Typically the block is bigger than sizeof(MLink).)
// Since assignments to mlink.next will result in a write barrier being performed
// this cannot be used by some of the internal GC structures. For example when
// the sweeper is placing an unmarked object on the free list it does not want the
// write barrier to be called since that could result in the object being reachable.
// 一个通用的块链表。（通常块的大小大于 sizeof(MLink)）
// 由于对 mlink.next 的赋值会导致写屏障被执行，
// 因此某些内部 GC 结构不能使用它。例如，当清扫器将未标记的对象
// 放入空闲列表时，它不希望调用写屏障，因为这可能导致对象变得可达。
type mlink struct {
	_    sys.NotInHeap // 标记该类型不在堆上分配
	next *mlink        // 指向链表中的下一个节点
}

// Initialize f to allocate objects of the given size,
// using the allocator to obtain chunks of memory.
// 初始化 f 以分配给定大小的对象，
// 使用分配器获取内存块。
func (f *fixalloc) init(size uintptr, first func(arg, p unsafe.Pointer), arg unsafe.Pointer, stat *sysMemStat) {
	// 检查分配大小是否超过最大块大小
	if size > _FixAllocChunk {
		throw("runtime: fixalloc size too large")
	}
	// 确保分配大小至少能容纳一个 mlink 结构
	size = max(size, unsafe.Sizeof(mlink{}))

	// 初始化 fixalloc 结构体的各个字段
	f.size = size                                   // 设置对象大小
	f.first = first                                 // 设置首次分配时的回调函数
	f.arg = arg                                     // 设置回调函数的参数
	f.list = nil                                    // 初始化空闲链表为空
	f.chunk = 0                                     // 初始化当前内存块地址为0
	f.nchunk = 0                                    // 初始化当前内存块剩余字节数为0
	f.nalloc = uint32(_FixAllocChunk / size * size) // 将 _FixAllocChunk 向下取整为 size 的整数倍，以消除尾部浪费
	f.inuse = 0                                     // 初始化已使用字节数为0
	f.stat = stat                                   // 设置内存统计信息
	f.zero = true                                   // 默认启用内存清零
}

func (f *fixalloc) alloc() unsafe.Pointer {
	// Check if the allocator has been initialized
	// 检查分配器是否已初始化
	if f.size == 0 {
		print("runtime: use of FixAlloc_Alloc before FixAlloc_Init\n")
		throw("runtime: internal error")
	}

	// If there are free objects in the list, reuse one
	// 如果空闲链表中有可用对象，则重用其中一个
	if f.list != nil {
		v := unsafe.Pointer(f.list)
		f.list = f.list.next
		f.inuse += f.size
		// Clear the memory if zeroing is enabled
		// 如果启用了清零，则清除内存
		if f.zero {
			memclrNoHeapPointers(v, f.size)
		}
		return v
	}

	// If current chunk is too small, allocate a new one
	// 如果当前内存块太小，则分配一个新的内存块
	if uintptr(f.nchunk) < f.size {
		f.chunk = uintptr(persistentalloc(uintptr(f.nalloc), 0, f.stat))
		f.nchunk = f.nalloc
	}

	// Allocate from current chunk
	// 从当前内存块中分配
	v := unsafe.Pointer(f.chunk)
	// Call first-time initialization function if set
	// 如果设置了首次初始化函数，则调用它
	if f.first != nil {
		f.first(f.arg, v)
	}
	// Update chunk pointer and remaining size
	// 更新内存块指针和剩余大小
	f.chunk = f.chunk + f.size
	f.nchunk -= uint32(f.size)
	f.inuse += f.size
	return v
}

func (f *fixalloc) free(p unsafe.Pointer) {
	// Decrease the in-use count by the size of one object
	// 减少已使用计数，减少量为一个对象的大小
	f.inuse -= f.size

	// Convert the pointer to an mlink type
	// 将指针转换为 mlink 类型
	v := (*mlink)(p)

	// Set the next pointer of the freed object to point to the current free list
	// 设置被释放对象的下一个指针指向当前的空闲链表
	v.next = f.list

	// Update the free list to point to the newly freed object
	// 更新空闲链表，使其指向新释放的对象
	f.list = v
}
