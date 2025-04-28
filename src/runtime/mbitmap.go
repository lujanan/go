// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Garbage collector: type and heap bitmaps.
// 垃圾回收器：类型和堆位图。
//
// Stack, data, and bss bitmaps
// 栈、数据段和bss段的位图
//
// Stack frames and global variables in the data and bss sections are
// described by bitmaps with 1 bit per pointer-sized word. A "1" bit
// means the word is a live pointer to be visited by the GC (referred to
// as "pointer"). A "0" bit means the word should be ignored by GC
// (referred to as "scalar", though it could be a dead pointer value).
// 栈帧以及数据段和bss段中的全局变量使用位图来描述，每个指针大小的字对应1位。
// "1"位表示该字是一个需要被GC访问的存活指针（称为"pointer"）。
// "0"位表示该字应该被GC忽略（称为"scalar"，尽管它可能是一个死指针值）。
//
// Heap bitmaps
// 堆位图
//
// The heap bitmap comprises 1 bit for each pointer-sized word in the heap,
// recording whether a pointer is stored in that word or not. This bitmap
// is stored at the end of a span for small objects and is unrolled at
// runtime from type metadata for all larger objects. Objects without
// pointers have neither a bitmap nor associated type metadata.
// 堆位图对堆中每个指针大小的字使用1位来记录该字是否存储了指针。
// 对于小对象，这个位图存储在span的末尾；对于所有较大的对象，位图在运行时从类型元数据展开。
// 不包含指针的对象既没有位图也没有相关的类型元数据。
//
// Bits in all cases correspond to words in little-endian order.
// 在所有情况下，位都按照小端序对应到字。
//
// For small objects, if s is the mspan for the span starting at "start",
// then s.heapBits() returns a slice containing the bitmap for the whole span.
// That is, s.heapBits()[0] holds the goarch.PtrSize*8 bits for the first
// goarch.PtrSize*8 words from "start" through "start+63*ptrSize" in the span.
// On a related note, small objects are always small enough that their bitmap
// fits in goarch.PtrSize*8 bits, so writing out bitmap data takes two bitmap
// writes at most (because object boundaries don't generally lie on
// s.heapBits()[i] boundaries).
// 对于小对象，如果s是起始于"start"的span的mspan，那么s.heapBits()返回包含整个span位图的切片。
// 也就是说，s.heapBits()[0]包含了从"start"到"start+63*ptrSize"的前goarch.PtrSize*8个字的goarch.PtrSize*8位。
// 另外，小对象总是足够小，它们的位图可以放入goarch.PtrSize*8位中，所以写出位图数据最多需要两次位图写入
//（因为对象边界通常不在s.heapBits()[i]边界上）。
//
// For larger objects, if t is the type for the object starting at "start",
// within some span whose mspan is s, then the bitmap at t.GCData is "tiled"
// from "start" through "start+s.elemsize".
// Specifically, the first bit of t.GCData corresponds to the word at "start",
// the second to the word after "start", and so on up to t.PtrBytes. At t.PtrBytes,
// we skip to "start+t.Size_" and begin again from there. This process is
// repeated until we hit "start+s.elemsize".
// This tiling algorithm supports array data, since the type always refers to
// the element type of the array. Single objects are considered the same as
// single-element arrays.
// The tiling algorithm may scan data past the end of the compiler-recognized
// object, but any unused data within the allocation slot (i.e. within s.elemsize)
// is zeroed, so the GC just observes nil pointers.
// Note that this "tiled" bitmap isn't stored anywhere; it is generated on-the-fly.
// 对于较大的对象，如果t是起始于"start"的对象的类型，在某个mspan为s的span内，那么t.GCData处的位图会从"start"到"start+s.elemsize"进行"平铺"。
// 具体来说，t.GCData的第一位对应"start"处的字，第二位对应"start"后的字，以此类推直到t.PtrBytes。
// 在t.PtrBytes处，我们跳到"start+t.Size_"并从那里重新开始。这个过程会重复直到达到"start+s.elemsize"。
// 这个平铺算法支持数组数据，因为类型总是引用数组的元素类型。单个对象被视为单元素数组。
// 平铺算法可能会扫描超出编译器识别的对象末尾的数据，但分配槽内任何未使用的数据（即在s.elemsize内）都被置零，
// 所以GC只会观察到nil指针。注意这个"平铺"的位图并不存储在任何地方；它是动态生成的。
//
// For objects without their own span, the type metadata is stored in the first
// word before the object at the beginning of the allocation slot. For objects
// with their own span, the type metadata is stored in the mspan.
// 对于没有自己span的对象，类型元数据存储在分配槽开始处对象之前的第一个字中。
// 对于有自己span的对象，类型元数据存储在mspan中。
//
// The bitmap for small unallocated objects in scannable spans is not maintained
// (can be junk).
// 可扫描span中未分配的小对象的位图不会被维护（可能是垃圾数据）。

package runtime

import (
	"internal/abi"
	"internal/goarch"
	"internal/runtime/atomic"
	"runtime/internal/sys"
	"unsafe"
)

const (
	// A malloc header is functionally a single type pointer, but
	// we need to use 8 here to ensure 8-byte alignment of allocations
	// on 32-bit platforms. It's wasteful, but a lot of code relies on
	// 8-byte alignment for 8-byte atomics.
	// malloc header 在功能上是一个单一的类型指针，但我们需要使用8来确保在32位平台上分配的内存是8字节对齐的。
	// 这虽然有些浪费，但很多代码都依赖于8字节对齐来实现8字节原子操作。
	mallocHeaderSize = 8

	// The minimum object size that has a malloc header, exclusive.
	//
	// The size of this value controls overheads from the malloc header.
	// The minimum size is bound by writeHeapBitsSmall, which assumes that the
	// pointer bitmap for objects of a size smaller than this doesn't cross
	// more than one pointer-word boundary. This sets an upper-bound on this
	// value at the number of bits in a uintptr, multiplied by the pointer
	// size in bytes.
	//
	// We choose a value here that has a natural cutover point in terms of memory
	// overheads. This value just happens to be the maximum possible value this
	// can be.
	//
	// A span with heap bits in it will have 128 bytes of heap bits on 64-bit
	// platforms, and 256 bytes of heap bits on 32-bit platforms. The first size
	// class where malloc headers match this overhead for 64-bit platforms is
	// 512 bytes (8 KiB / 512 bytes * 8 bytes-per-header = 128 bytes of overhead).
	// On 32-bit platforms, this same point is the 256 byte size class
	// (8 KiB / 256 bytes * 8 bytes-per-header = 256 bytes of overhead).
	//
	// Guaranteed to be exactly at a size class boundary. The reason this value is
	// an exclusive minimum is subtle. Suppose we're allocating a 504-byte object
	// and its rounded up to 512 bytes for the size class. If minSizeForMallocHeader
	// is 512 and an inclusive minimum, then a comparison against minSizeForMallocHeader
	// by the two values would produce different results. In other words, the comparison
	// would not be invariant to size-class rounding. Eschewing this property means a
	// more complex check or possibly storing additional state to determine whether a
	// span has malloc headers.
	// 具有 malloc header 的最小对象大小（不包含该大小）。
	//
	// 这个值的大小控制着 malloc header 带来的开销。
	// 最小大小受 writeHeapBitsSmall 的限制，它假设小于这个大小的对象的指针位图不会跨越
	// 超过一个指针字边界。这为该值设置了一个上限，即 uintptr 的位数乘以指针大小（字节）。
	//
	// 我们在这里选择一个在内存开销方面有自然分界点的值。这个值恰好是可能的最大值。
	//
	// 包含堆位图的 span 在64位平台上有128字节的堆位图，在32位平台上有256字节的堆位图。
	// 在64位平台上，第一个 malloc header 开销匹配的大小类是512字节
	// (8 KiB / 512 bytes * 8 bytes-per-header = 128 bytes 开销)。
	// 在32位平台上，这个点是256字节大小类
	// (8 KiB / 256 bytes * 8 bytes-per-header = 256 bytes 开销)。
	//
	// 保证正好在大小类边界上。这个值作为不包含的最小值的原因很微妙。
	// 假设我们分配一个504字节的对象，它被向上取整到512字节作为大小类。
	// 如果 minSizeForMallocHeader 是512且是包含的最小值，那么对这两个值的比较会产生不同的结果。
	// 换句话说，比较不会对大小类取整保持不变。避免这个特性意味着需要更复杂的检查，
	// 或者可能需要存储额外的状态来确定 span 是否有 malloc header。
	// 512
	minSizeForMallocHeader = goarch.PtrSize * ptrBits
)

// heapBitsInSpan returns true if the size of an object implies its ptr/scalar
// data is stored at the end of the span, and is accessible via span.heapBits.
//
// Note: this works for both rounded-up sizes (span.elemsize) and unrounded
// type sizes because minSizeForMallocHeader is guaranteed to be at a size
// class boundary.
//
// heapBitsInSpan 返回 true 如果对象的大小表明其指针/标量数据存储在 span 的末尾，
// 并且可以通过 span.heapBits 访问。
//
// 注意：这适用于向上取整的大小（span.elemsize）和未取整的类型大小，
// 因为 minSizeForMallocHeader 保证在大小类边界上。
//
//go:nosplit
func heapBitsInSpan(userSize uintptr) bool {
	// N.B. minSizeForMallocHeader is an exclusive minimum so that this function is
	// invariant under size-class rounding on its input.
	// 注意：minSizeForMallocHeader 是一个不包含的最小值，这样这个函数在输入的大小类取整下保持不变。
	return userSize <= minSizeForMallocHeader
}

// typePointers is an iterator over the pointers in a heap object.
//
// Iteration through this type implements the tiling algorithm described at the
// top of this file.
// typePointers 是堆对象中指针的迭代器。
//
// 通过这个类型的迭代实现了文件顶部描述的平铺算法。
type typePointers struct {
	// elem is the address of the current array element of type typ being iterated over.
	// Objects that are not arrays are treated as single-element arrays, in which case
	// this value does not change.
	// elem 是当前正在迭代的类型为 typ 的数组元素的地址。
	// 非数组对象被视为单元素数组，在这种情况下该值不会改变。
	elem uintptr

	// addr is the address the iterator is currently working from and describes
	// the address of the first word referenced by mask.
	// addr 是迭代器当前工作的地址，描述了 mask 引用的第一个字的地址。
	addr uintptr

	// mask is a bitmask where each bit corresponds to pointer-words after addr.
	// Bit 0 is the pointer-word at addr, Bit 1 is the next word, and so on.
	// If a bit is 1, then there is a pointer at that word.
	// nextFast and next mask out bits in this mask as their pointers are processed.
	// mask 是一个位掩码，其中每一位对应 addr 之后的指针字。
	// 位 0 是 addr 处的指针字，位 1 是下一个字，以此类推。
	// 如果某位为 1，则表示该字处有一个指针。
	// nextFast 和 next 在处理指针时会屏蔽掉这个掩码中的位。
	mask uintptr

	// typ is a pointer to the type information for the heap object's type.
	// This may be nil if the object is in a span where heapBitsInSpan(span.elemsize) is true.
	// typ 是指向堆对象类型的类型信息的指针。
	// 如果对象在 heapBitsInSpan(span.elemsize) 为 true 的 span 中，则可能为 nil。
	typ *_type
}

// typePointersOf returns an iterator over all heap pointers in the range [addr, addr+size).
//
// addr and addr+size must be in the range [span.base(), span.limit).
//
// Note: addr+size must be passed as the limit argument to the iterator's next method on
// each iteration. This slightly awkward API is to allow typePointers to be destructured
// by the compiler.
//
// nosplit because it is used during write barriers and must not be preempted.
//
// typePointersOf 返回一个迭代器，用于遍历范围 [addr, addr+size) 内的所有堆指针。
//
// addr 和 addr+size 必须在范围 [span.base(), span.limit) 内。
//
// 注意：addr+size 必须在每次迭代时作为迭代器 next 方法的 limit 参数传入。
// 这个稍微有点别扭的 API 是为了允许编译器对 typePointers 进行解构。
//
// 使用 nosplit 是因为它在写屏障期间使用，不能被抢占。
//
//go:nosplit
func (span *mspan) typePointersOf(addr, size uintptr) typePointers {
	// 获取对象的基础地址
	base := span.objBase(addr)
	// 获取未检查的类型指针迭代器
	tp := span.typePointersOfUnchecked(base)
	// 如果基础地址等于输入地址且大小等于 span 的元素大小，直接返回迭代器
	if base == addr && size == span.elemsize {
		return tp
	}
	// 否则，快速前进到指定范围并返回新的迭代器
	return tp.fastForward(addr-tp.addr, addr+size)
}

// typePointersOfUnchecked is like typePointersOf, but assumes addr is the base
// of an allocation slot in a span (the start of the object if no header, the
// header otherwise). It returns an iterator that generates all pointers
// in the range [addr, addr+span.elemsize).
//
// nosplit because it is used during write barriers and must not be preempted.
//
// typePointersOfUnchecked 类似于 typePointersOf，但假设 addr 是 span 中分配槽的基础地址
// （如果没有头部，则是对象的开始；否则是头部）。它返回一个迭代器，用于生成
// 范围 [addr, addr+span.elemsize) 内的所有指针。
//
// 使用 nosplit 是因为它在写屏障期间使用，不能被抢占。
//
//go:nosplit
func (span *mspan) typePointersOfUnchecked(addr uintptr) typePointers {
	// 用于调试的常量，控制是否进行额外检查
	const doubleCheck = false
	// 如果启用了额外检查且地址不是对象的基础地址，则抛出错误
	if doubleCheck && span.objBase(addr) != addr {
		print("runtime: addr=", addr, " base=", span.objBase(addr), "\n")
		throw("typePointersOfUnchecked consisting of non-base-address for object")
	}

	// 获取 span 的 spanclass
	spc := span.spanclass
	// 如果 span 不需要扫描（noscan），直接返回空的 typePointers
	if spc.noscan() {
		return typePointers{}
	}
	// 如果对象大小适合在 span 中存储位图，处理无头部的对象
	if heapBitsInSpan(span.elemsize) {
		// 处理无头部的对象
		return typePointers{elem: addr, addr: addr, mask: span.heapBitsSmallForAddr(addr)}
	}

	// 所有剩余的对象都有头部
	var typ *_type
	if spc.sizeclass() != 0 {
		// 从小对象中提取分配头部（从对象的第一个字中）
		typ = *(**_type)(unsafe.Pointer(addr))
		addr += mallocHeaderSize
	} else {
		// 对于大对象，从 span 中获取类型信息
		typ = span.largeType
		if typ == nil {
			// 允许类型为 nil，用于延迟清零。参见 mallocgc
			return typePointers{}
		}
	}
	// 获取类型的 GC 数据
	gcdata := typ.GCData
	// 返回包含类型信息的 typePointers
	return typePointers{elem: addr, addr: addr, mask: readUintptr(gcdata), typ: typ}
}

// typePointersOfType is like typePointersOf, but assumes addr points to one or more
// contiguous instances of the provided type. The provided type must not be nil and
// it must not have its type metadata encoded as a gcprog.
//
// It returns an iterator that tiles typ.GCData starting from addr. It's the caller's
// responsibility to limit iteration.
//
// nosplit because its callers are nosplit and require all their callees to be nosplit.
//
// typePointersOfType 类似于 typePointersOf，但假设 addr 指向一个或多个连续的指定类型实例。
// 提供的类型不能为 nil，且其类型元数据不能以 gcprog 形式编码。
//
// 它返回一个迭代器，从 addr 开始平铺 typ.GCData。调用者有责任限制迭代范围。
//
// 使用 nosplit 是因为其调用者都是 nosplit 的，并要求所有被调用者也是 nosplit 的。
//
//go:nosplit
func (span *mspan) typePointersOfType(typ *abi.Type, addr uintptr) typePointers {
	// 用于调试的常量，控制是否进行额外检查
	const doubleCheck = false
	// 如果启用了额外检查，验证类型参数的有效性
	if doubleCheck && (typ == nil || typ.Kind_&abi.KindGCProg != 0) {
		throw("bad type passed to typePointersOfType")
	}
	// 如果 span 不需要扫描（noscan），直接返回空的 typePointers
	if span.spanclass.noscan() {
		return typePointers{}
	}
	// 由于我们已经有了类型信息，假装我们有一个头部
	// 获取类型的 GC 数据
	gcdata := typ.GCData
	// 返回包含类型信息的 typePointers
	return typePointers{elem: addr, addr: addr, mask: readUintptr(gcdata), typ: typ}
}

// nextFast is the fast path of next. nextFast is written to be inlineable and,
// as the name implies, fast.
//
// Callers that are performance-critical should iterate using the following
// pattern:
//
//	for {
//		var addr uintptr
//		if tp, addr = tp.nextFast(); addr == 0 {
//			if tp, addr = tp.next(limit); addr == 0 {
//				break
//			}
//		}
//		// Use addr.
//		...
//	}
//
// nosplit because it is used during write barriers and must not be preempted.
//
// nextFast 是 next 的快速路径。nextFast 被设计为可内联的，顾名思义，它很快。
//
// 对性能要求较高的调用者应该使用以下模式进行迭代：
//
//	for {
//		var addr uintptr
//		if tp, addr = tp.nextFast(); addr == 0 {
//			if tp, addr = tp.next(limit); addr == 0 {
//				break
//			}
//		}
//		// 使用 addr
//		...
//	}
//
// 使用 nosplit 是因为它在写屏障期间使用，不能被抢占。
//
//go:nosplit
func (tp typePointers) nextFast() (typePointers, uintptr) {
	// TESTQ/JEQ - 测试掩码是否为空
	if tp.mask == 0 {
		return tp, 0
	}
	// BSFQ - 找到最低位的1
	var i int
	if goarch.PtrSize == 8 {
		// 64位系统：使用64位尾随零计数
		i = sys.TrailingZeros64(uint64(tp.mask))
	} else {
		// 32位系统：使用32位尾随零计数
		i = sys.TrailingZeros32(uint32(tp.mask))
	}
	// BTCQ - 清除找到的位
	tp.mask ^= uintptr(1) << (i & (ptrBits - 1))
	// LEAQ - 计算并返回下一个指针的地址
	return tp, tp.addr + uintptr(i)*goarch.PtrSize
}

// next advances the pointers iterator, returning the updated iterator and
// the address of the next pointer.
//
// limit must be the same each time it is passed to next.
//
// nosplit because it is used during write barriers and must not be preempted.
//
// next 推进指针迭代器，返回更新后的迭代器和下一个指针的地址。
//
// 每次传递给 next 的 limit 必须相同。
//
// 使用 nosplit 是因为它在写屏障期间使用，不能被抢占。
//
//go:nosplit
func (tp typePointers) next(limit uintptr) (typePointers, uintptr) {
	for {
		// 如果当前掩码不为0，使用快速路径
		if tp.mask != 0 {
			return tp.nextFast()
		}

		// Stop if we don't actually have type information.
		// 如果没有类型信息，则停止迭代
		if tp.typ == nil {
			return typePointers{}, 0
		}

		// Advance to the next element if necessary.
		// 如果需要，前进到下一个元素
		if tp.addr+goarch.PtrSize*ptrBits >= tp.elem+tp.typ.PtrBytes {
			// 如果当前地址加上一个完整的位图块超出了元素的指针区域，
			// 则移动到下一个元素
			tp.elem += tp.typ.Size_
			tp.addr = tp.elem
		} else {
			// 否则，在当前元素内前进一个位图块
			tp.addr += ptrBits * goarch.PtrSize
		}

		// Check if we've exceeded the limit with the last update.
		// 检查是否超出了限制
		if tp.addr >= limit {
			return typePointers{}, 0
		}

		// Grab more bits and try again.
		// 获取更多位并重试
		// 计算新的位图掩码：从GCData中读取对应位置的位图数据
		tp.mask = readUintptr(addb(tp.typ.GCData, (tp.addr-tp.elem)/goarch.PtrSize/8))
		// 如果当前地址加上一个完整的位图块超出了限制，
		// 则需要调整掩码以排除超出限制的部分
		if tp.addr+goarch.PtrSize*ptrBits > limit {
			bits := (tp.addr + goarch.PtrSize*ptrBits - limit) / goarch.PtrSize
			tp.mask &^= ((1 << (bits)) - 1) << (ptrBits - bits)
		}
	}
}

// fastForward moves the iterator forward by n bytes. n must be a multiple
// of goarch.PtrSize. limit must be the same limit passed to next for this
// iterator.
//
// nosplit because it is used during write barriers and must not be preempted.
//
// fastForward 将迭代器向前移动 n 字节。n 必须是 goarch.PtrSize 的倍数。
// limit 必须与此迭代器传递给 next 的限制相同。
//
// 使用 nosplit 是因为它在写屏障期间使用，不能被抢占。
//
//go:nosplit
func (tp typePointers) fastForward(n, limit uintptr) typePointers {
	// Basic bounds check.
	// 基本边界检查
	target := tp.addr + n
	if target >= limit {
		return typePointers{}
	}
	if tp.typ == nil {
		// Handle small objects.
		// Clear any bits before the target address.
		// 处理小对象
		// 清除目标地址之前的任何位
		tp.mask &^= (1 << ((target - tp.addr) / goarch.PtrSize)) - 1
		// Clear any bits past the limit.
		// 清除超出限制的任何位
		if tp.addr+goarch.PtrSize*ptrBits > limit {
			bits := (tp.addr + goarch.PtrSize*ptrBits - limit) / goarch.PtrSize
			tp.mask &^= ((1 << (bits)) - 1) << (ptrBits - bits)
		}
		return tp
	}

	// Move up elem and addr.
	// Offsets within an element are always at a ptrBits*goarch.PtrSize boundary.
	// 向上移动 elem 和 addr
	// 元素内的偏移量总是在 ptrBits*goarch.PtrSize 边界处
	if n >= tp.typ.Size_ {
		// elem needs to be moved to the element containing
		// tp.addr + n.
		// elem 需要移动到包含 tp.addr + n 的元素
		oldelem := tp.elem
		tp.elem += (tp.addr - tp.elem + n) / tp.typ.Size_ * tp.typ.Size_
		tp.addr = tp.elem + alignDown(n-(tp.elem-oldelem), ptrBits*goarch.PtrSize)
	} else {
		tp.addr += alignDown(n, ptrBits*goarch.PtrSize)
	}

	if tp.addr-tp.elem >= tp.typ.PtrBytes {
		// We're starting in the non-pointer area of an array.
		// Move up to the next element.
		// 我们从数组的非指针区域开始
		// 移动到下一个元素
		tp.elem += tp.typ.Size_
		tp.addr = tp.elem
		tp.mask = readUintptr(tp.typ.GCData)

		// We may have exceeded the limit after this. Bail just like next does.
		// 在此之后我们可能已经超出了限制。像 next 一样退出
		if tp.addr >= limit {
			return typePointers{}
		}
	} else {
		// Grab the mask, but then clear any bits before the target address and any
		// bits over the limit.
		// 获取掩码，但随后清除目标地址之前的任何位和超出限制的任何位
		tp.mask = readUintptr(addb(tp.typ.GCData, (tp.addr-tp.elem)/goarch.PtrSize/8))
		tp.mask &^= (1 << ((target - tp.addr) / goarch.PtrSize)) - 1
	}
	if tp.addr+goarch.PtrSize*ptrBits > limit {
		bits := (tp.addr + goarch.PtrSize*ptrBits - limit) / goarch.PtrSize
		tp.mask &^= ((1 << (bits)) - 1) << (ptrBits - bits)
	}
	return tp
}

// objBase returns the base pointer for the object containing addr in span.
//
// Assumes that addr points into a valid part of span (span.base() <= addr < span.limit).
//
// objBase 返回包含 span 中 addr 的对象的基指针。
//
// 假设 addr 指向 span 的有效部分（span.base() <= addr < span.limit）。
//
//go:nosplit
func (span *mspan) objBase(addr uintptr) uintptr {
	return span.base() + span.objIndex(addr)*span.elemsize
}

// bulkBarrierPreWrite executes a write barrier
// for every pointer slot in the memory range [src, src+size),
// using pointer/scalar information from [dst, dst+size).
// This executes the write barriers necessary before a memmove.
// src, dst, and size must be pointer-aligned.
// The range [dst, dst+size) must lie within a single object.
// It does not perform the actual writes.
//
// As a special case, src == 0 indicates that this is being used for a
// memclr. bulkBarrierPreWrite will pass 0 for the src of each write
// barrier.
//
// Callers should call bulkBarrierPreWrite immediately before
// calling memmove(dst, src, size). This function is marked nosplit
// to avoid being preempted; the GC must not stop the goroutine
// between the memmove and the execution of the barriers.
// The caller is also responsible for cgo pointer checks if this
// may be writing Go pointers into non-Go memory.
//
// Pointer data is not maintained for allocations containing
// no pointers at all; any caller of bulkBarrierPreWrite must first
// make sure the underlying allocation contains pointers, usually
// by checking typ.PtrBytes.
//
// The typ argument is the type of the space at src and dst (and the
// element type if src and dst refer to arrays) and it is optional.
// If typ is nil, the barrier will still behave as expected and typ
// is used purely as an optimization. However, it must be used with
// care.
//
// If typ is not nil, then src and dst must point to one or more values
// of type typ. The caller must ensure that the ranges [src, src+size)
// and [dst, dst+size) refer to one or more whole values of type src and
// dst (leaving off the pointerless tail of the space is OK). If this
// precondition is not followed, this function will fail to scan the
// right pointers.
//
// When in doubt, pass nil for typ. That is safe and will always work.
//
// Callers must perform cgo checks if goexperiment.CgoCheck2.
//
// bulkBarrierPreWrite 为内存范围 [src, src+size) 中的每个指针槽执行写屏障，
// 使用来自 [dst, dst+size) 的指针/标量信息。
// 这是在 memmove 之前执行必要的写屏障。
// src、dst 和 size 必须是指针对齐的。
// 范围 [dst, dst+size) 必须位于单个对象内。
// 它不执行实际的写入操作。
//
// 特殊情况：当 src == 0 时，表示这是用于 memclr 操作。
// bulkBarrierPreWrite 将为每个写屏障传递 0 作为 src。
//
// 调用者应该在调用 memmove(dst, src, size) 之前立即调用 bulkBarrierPreWrite。
// 此函数被标记为 nosplit 以避免被抢占；GC 不能在 memmove 和屏障执行之间停止 goroutine。
// 如果可能将 Go 指针写入非 Go 内存，调用者还需要负责 cgo 指针检查。
//
// 对于完全不包含指针的分配，不维护指针数据；
// bulkBarrierPreWrite 的任何调用者必须首先确保底层分配包含指针，
// 通常通过检查 typ.PtrBytes 来实现。
//
// typ 参数是 src 和 dst 处空间的类型（如果 src 和 dst 引用数组，则为元素类型），它是可选的。
// 如果 typ 为 nil，屏障仍将按预期工作，typ 仅用作优化。
// 但是，必须谨慎使用它。
//
// 如果 typ 不为 nil，则 src 和 dst 必须指向一个或多个类型为 typ 的值。
// 调用者必须确保范围 [src, src+size) 和 [dst, dst+size) 引用一个或多个完整的 src 和 dst 类型的值
// （忽略空间的无指针尾部是可以的）。
// 如果不遵循这个前提条件，此函数将无法扫描正确的指针。
//
// 如有疑问，传递 nil 作为 typ。这是安全的，并且总是有效。
//
// 如果启用了 goexperiment.CgoCheck2，调用者必须执行 cgo 检查。
//
//go:nosplit
func bulkBarrierPreWrite(dst, src, size uintptr, typ *abi.Type) {
	// Check if any of the arguments are unaligned
	// 检查参数是否对齐
	if (dst|src|size)&(goarch.PtrSize-1) != 0 {
		throw("bulkBarrierPreWrite: unaligned arguments")
	}
	// If write barriers are not enabled, no need to proceed
	// 如果写屏障未启用，则无需继续
	if !writeBarrier.enabled {
		return
	}
	// Get the span containing dst
	// 获取包含 dst 的 span
	s := spanOf(dst)
	if s == nil {
		// If dst is a global, use the data or BSS bitmaps to
		// execute write barriers.
		// 如果 dst 是全局变量，使用数据段或 BSS 段的位图来执行写屏障
		for _, datap := range activeModules() {
			if datap.data <= dst && dst < datap.edata {
				bulkBarrierBitmap(dst, src, size, dst-datap.data, datap.gcdatamask.bytedata)
				return
			}
		}
		for _, datap := range activeModules() {
			if datap.bss <= dst && dst < datap.ebss {
				bulkBarrierBitmap(dst, src, size, dst-datap.bss, datap.gcbssmask.bytedata)
				return
			}
		}
		return
	} else if s.state.get() != mSpanInUse || dst < s.base() || s.limit <= dst {
		// dst was heap memory at some point, but isn't now.
		// It can't be a global. It must be either our stack,
		// or in the case of direct channel sends, it could be
		// another stack. Either way, no need for barriers.
		// This will also catch if dst is in a freed span,
		// though that should never have.
		// dst 曾经是堆内存，但现在不是了。
		// 它不可能是全局变量。它要么是我们的栈，
		// 要么在直接通道发送的情况下，可能是另一个栈。
		// 无论哪种情况，都不需要屏障。
		// 这也会捕获 dst 在已释放的 span 中的情况，
		// 虽然这种情况不应该发生。
		return
	}
	// Get the write barrier buffer for the current P
	// 获取当前 P 的写屏障缓冲区
	buf := &getg().m.p.ptr().wbBuf

	// Double-check that the bitmaps generated in the two possible paths match.
	// 双重检查两种可能路径生成的位图是否匹配
	const doubleCheck = false
	if doubleCheck {
		doubleCheckTypePointersOfType(s, typ, dst, size)
	}

	// Get type pointers based on whether we have type information
	// 根据是否有类型信息获取类型指针
	var tp typePointers
	if typ != nil && typ.Kind_&abi.KindGCProg == 0 {
		// If we have type info and it's not a GC program, use type-based lookup
		// 如果有类型信息且不是 GC 程序，使用基于类型的查找
		tp = s.typePointersOfType(typ, dst)
	} else {
		// Otherwise use size-based lookup
		// 否则使用基于大小的查找
		tp = s.typePointersOf(dst, size)
	}

	// Handle the case where src is 0 (zeroing memory)
	// 处理 src 为 0 的情况（内存清零）
	if src == 0 {
		for {
			var addr uintptr
			// Get next pointer address, break if none left
			// 获取下一个指针地址，如果没有则退出
			if tp, addr = tp.next(dst + size); addr == 0 {
				break
			}
			// Convert address to pointer and get buffer slot
			// 将地址转换为指针并获取缓冲区槽
			dstx := (*uintptr)(unsafe.Pointer(addr))
			p := buf.get1()
			// Store old pointer value in buffer
			// 在缓冲区中存储旧指针值
			p[0] = *dstx
		}
	} else {
		// Handle the case where we're copying from src to dst
		// 处理从 src 复制到 dst 的情况
		for {
			var addr uintptr
			// Get next pointer address, break if none left
			// 获取下一个指针地址，如果没有则退出
			if tp, addr = tp.next(dst + size); addr == 0 {
				break
			}
			// Convert addresses to pointers and get buffer slots
			// 将地址转换为指针并获取缓冲区槽
			dstx := (*uintptr)(unsafe.Pointer(addr))
			srcx := (*uintptr)(unsafe.Pointer(src + (addr - dst)))
			p := buf.get2()
			// Store both old and new pointer values in buffer
			// 在缓冲区中存储新旧指针值
			p[0] = *dstx
			p[1] = *srcx
		}
	}
}

// bulkBarrierPreWriteSrcOnly is like bulkBarrierPreWrite but
// does not execute write barriers for [dst, dst+size).
//
// In addition to the requirements of bulkBarrierPreWrite
// callers need to ensure [dst, dst+size) is zeroed.
//
// This is used for special cases where e.g. dst was just
// created and zeroed with malloc.
//
// The type of the space can be provided purely as an optimization.
// See bulkBarrierPreWrite's comment for more details -- use this
// optimization with great care.
//
// bulkBarrierPreWriteSrcOnly 类似于 bulkBarrierPreWrite，但
// 不会对 [dst, dst+size) 执行写屏障。
//
// 除了 bulkBarrierPreWrite 的要求外，
// 调用者还需要确保 [dst, dst+size) 已被清零。
//
// 这用于特殊情况，例如 dst 刚刚被创建并通过 malloc 清零。
//
// 可以提供空间的类型纯粹作为优化。
// 有关更多详细信息，请参阅 bulkBarrierPreWrite 的注释 -- 请谨慎使用此优化。
//
//go:nosplit
func bulkBarrierPreWriteSrcOnly(dst, src, size uintptr, typ *abi.Type) {
	// 检查参数是否按指针大小对齐
	if (dst|src|size)&(goarch.PtrSize-1) != 0 {
		throw("bulkBarrierPreWrite: unaligned arguments")
	}
	// 如果写屏障未启用，直接返回
	if !writeBarrier.enabled {
		return
	}
	// 获取当前 goroutine 的写屏障缓冲区
	buf := &getg().m.p.ptr().wbBuf
	// 获取目标地址所在的 span
	s := spanOf(dst)

	// 双重检查两种可能路径生成的位图是否匹配
	const doubleCheck = false
	if doubleCheck {
		doubleCheckTypePointersOfType(s, typ, dst, size)
	}

	var tp typePointers
	// 如果有类型信息且不是 GC 程序，使用基于类型的查找
	if typ != nil && typ.Kind_&abi.KindGCProg == 0 {
		tp = s.typePointersOfType(typ, dst)
	} else {
		// 否则使用基于大小的查找
		tp = s.typePointersOf(dst, size)
	}
	// 遍历所有指针位置
	for {
		var addr uintptr
		// 获取下一个指针地址，如果没有则退出
		if tp, addr = tp.next(dst + size); addr == 0 {
			break
		}
		// 计算源地址对应的指针位置
		srcx := (*uintptr)(unsafe.Pointer(addr - dst + src))
		// 获取写屏障缓冲区的一个槽位
		p := buf.get1()
		// 存储源指针的值
		p[0] = *srcx
	}
}

// initHeapBits initializes the heap bitmap for a span.
//
// TODO(mknyszek): This should set the heap bits for single pointer
// allocations eagerly to avoid calling heapSetType at allocation time,
// just to write one bit.
//
// initHeapBits 初始化一个 span 的堆位图。
//
// TODO(mknyszek): 这应该为单个指针分配急切地设置堆位，
// 以避免在分配时调用 heapSetType，仅仅是为了写入一个位。
func (s *mspan) initHeapBits(forceClear bool) {
	// 如果 span 是可扫描的且元素大小适合在 span 中存储位图，
	// 或者是用户 arena 块，则需要初始化堆位图
	if (!s.spanclass.noscan() && heapBitsInSpan(s.elemsize)) || s.isUserArenaChunk {
		// 获取 span 的堆位图
		b := s.heapBits()
		// 清空位图
		clear(b)
	}
}

// heapBits returns the heap ptr/scalar bits stored at the end of the span for
// small object spans and heap arena spans.
//
// Note that the uintptr of each element means something different for small object
// spans and for heap arena spans. Small object spans are easy: they're never interpreted
// as anything but uintptr, so they're immune to differences in endianness. However, the
// heapBits for user arena spans is exposed through a dummy type descriptor, so the byte
// ordering needs to match the same byte ordering the compiler would emit. The compiler always
// emits the bitmap data in little endian byte ordering, so on big endian platforms these
// uintptrs will have their byte orders swapped from what they normally would be.
//
// heapBitsInSpan(span.elemsize) or span.isUserArenaChunk must be true.
//
// heapBits 返回存储在 span 末尾的堆指针/标量位，用于小对象 spans 和堆 arena spans。
//
// 注意，每个元素的 uintptr 对于小对象 spans 和堆 arena spans 有不同的含义。
// 小对象 spans 很简单：它们只被解释为 uintptr，所以不受字节序差异的影响。
// 但是，用户 arena spans 的 heapBits 通过一个虚拟类型描述符暴露，
// 所以字节序需要与编译器生成的字节序匹配。
// 编译器总是以小端字节序生成位图数据，
// 所以在小端平台上这些 uintptrs 的字节序会与正常情况相反。
//
// heapBitsInSpan(span.elemsize) 或 span.isUserArenaChunk 必须为 true。
//
//go:nosplit
func (span *mspan) heapBits() []uintptr {
	const doubleCheck = false

	if doubleCheck && !span.isUserArenaChunk {
		if span.spanclass.noscan() {
			throw("heapBits called for noscan")
		}
		if span.elemsize > minSizeForMallocHeader {
			throw("heapBits called for span class that should have a malloc header")
		}
	}
	// Find the bitmap at the end of the span.
	//
	// Nearly every span with heap bits is exactly one page in size. Arenas are the only exception.
	// 在 span 的末尾找到位图。
	//
	// 几乎所有带有堆位的 span 大小正好是一页。Arenas 是唯一的例外。
	if span.npages == 1 {
		// This will be inlined and constant-folded down.
		// 这将被内联并常量折叠。
		return heapBitsSlice(span.base(), pageSize)
	}
	return heapBitsSlice(span.base(), span.npages*pageSize)
}

// Helper for constructing a slice for the span's heap bits.
// 用于构造 span 堆位的切片的辅助函数。
//
//go:nosplit
func heapBitsSlice(spanBase, spanSize uintptr) []uintptr {
	// Calculate the size of the bitmap in bytes.
	// Each pointer-sized word needs 1/8 of a byte in the bitmap.
	// 计算位图的大小（以字节为单位）。
	// 每个指针大小的字在位图中需要 1/8 字节。
	bitmapSize := spanSize / goarch.PtrSize / 8

	// Calculate the number of uintptr elements needed to store the bitmap.
	// 计算存储位图所需的 uintptr 元素数量。
	elems := int(bitmapSize / goarch.PtrSize)

	// Create a notInHeapSlice to hold the bitmap data.
	// 创建一个 notInHeapSlice 来保存位图数据。
	var sl notInHeapSlice

	// Initialize the slice with:
	// - Pointer to the bitmap data (located at span end - bitmap size)
	// - Length and capacity both set to elems
	// 初始化切片：
	// - 指向位图数据的指针（位于 span 末尾 - 位图大小）
	// - 长度和容量都设置为 elems
	sl = notInHeapSlice{(*notInHeap)(unsafe.Pointer(spanBase + spanSize - bitmapSize)), elems, elems}

	// Convert the notInHeapSlice to a []uintptr and return it.
	// 将 notInHeapSlice 转换为 []uintptr 并返回。
	return *(*[]uintptr)(unsafe.Pointer(&sl))
}

// heapBitsSmallForAddr loads the heap bits for the object stored at addr from span.heapBits.
//
// addr must be the base pointer of an object in the span. heapBitsInSpan(span.elemsize)
// must be true.
//
// heapBitsSmallForAddr 从 span.heapBits 加载存储在 addr 处的对象的堆位。
//
// addr 必须是 span 中对象的基指针。heapBitsInSpan(span.elemsize) 必须为 true。
//
//go:nosplit
func (span *mspan) heapBitsSmallForAddr(addr uintptr) uintptr {
	// 计算 span 的总大小（页数 * 页大小）
	spanSize := span.npages * pageSize
	// 计算位图大小（总大小 / 指针大小 / 8，因为每个指针需要 1/8 字节的位图）
	bitmapSize := spanSize / goarch.PtrSize / 8
	// 获取位图的起始位置（span 基地址 + span 大小 - 位图大小）
	hbits := (*byte)(unsafe.Pointer(span.base() + spanSize - bitmapSize))

	// These objects are always small enough that their bitmaps
	// fit in a single word, so just load the word or two we need.
	//
	// Mirrors mspan.writeHeapBitsSmall.
	//
	// We should be using heapBits(), but unfortunately it introduces
	// both bounds checks panics and throw which causes us to exceed
	// the nosplit limit in quite a few cases.
	//
	// 这些对象总是足够小，它们的位图可以放在一个字中，所以只需要加载我们需要的字。
	//
	// 与 mspan.writeHeapBitsSmall 相对应。
	//
	// 我们应该使用 heapBits()，但不幸的是它会引入边界检查 panic 和 throw，
	// 这会导致我们在很多情况下超出 nosplit 限制。

	// 计算位图索引 i（对象在 span 中的偏移量 / 指针大小 / 每个字的位数）
	i := (addr - span.base()) / goarch.PtrSize / ptrBits
	// 计算位图偏移量 j（对象在 span 中的偏移量 / 指针大小 % 每个字的位数）
	j := (addr - span.base()) / goarch.PtrSize % ptrBits
	// 计算对象包含的指针数量
	bits := span.elemsize / goarch.PtrSize
	// 获取第一个字的指针
	word0 := (*uintptr)(unsafe.Pointer(addb(hbits, goarch.PtrSize*(i+0))))
	// 获取第二个字的指针（如果需要）
	word1 := (*uintptr)(unsafe.Pointer(addb(hbits, goarch.PtrSize*(i+1))))

	var read uintptr
	if j+bits > ptrBits {
		// Two reads.
		// 需要两次读取
		// 计算第一个字中需要读取的位数
		bits0 := ptrBits - j
		// 计算第二个字中需要读取的位数
		bits1 := bits - bits0
		// 从第一个字中读取位
		read = *word0 >> j
		// 从第二个字中读取位并组合
		read |= (*word1 & ((1 << bits1) - 1)) << bits0
	} else {
		// One read.
		// 只需要一次读取
		// 从第一个字中读取位
		read = (*word0 >> j) & ((1 << bits) - 1)
	}
	return read
}

// writeHeapBitsSmall writes the heap bits for small objects whose ptr/scalar data is
// stored as a bitmap at the end of the span.
//
// Assumes dataSize is <= ptrBits*goarch.PtrSize. x must be a pointer into the span.
// heapBitsInSpan(dataSize) must be true. dataSize must be >= typ.Size_.
//
// writeHeapBitsSmall 为小对象写入堆位图，这些对象的指针/标量数据存储在 span 末尾的位图中。
//
// 假设 dataSize <= ptrBits*goarch.PtrSize。x 必须是指向 span 的指针。
// heapBitsInSpan(dataSize) 必须为 true。dataSize 必须 >= typ.Size_。
//
//go:nosplit
func (span *mspan) writeHeapBitsSmall(x, dataSize uintptr, typ *_type) (scanSize uintptr) {
	// The objects here are always really small, so a single load is sufficient.
	// 这里的对象总是很小，所以一次加载就足够了。
	src0 := readUintptr(typ.GCData)

	// Create repetitions of the bitmap if we have a small array.
	// 如果我们有一个小数组，创建位图的重复。
	bits := span.elemsize / goarch.PtrSize
	scanSize = typ.PtrBytes
	src := src0
	switch typ.Size_ {
	case goarch.PtrSize:
		// 如果类型大小等于指针大小，创建一个完整的位掩码
		src = (1 << (dataSize / goarch.PtrSize)) - 1
	default:
		// 对于其他情况，需要重复位图模式
		for i := typ.Size_; i < dataSize; i += typ.Size_ {
			src |= src0 << (i / goarch.PtrSize)
			scanSize += typ.Size_
		}
	}

	// Since we're never writing more than one uintptr's worth of bits, we're either going
	// to do one or two writes.
	// 由于我们永远不会写入超过一个 uintptr 的位数，所以我们要么进行一次写入，要么进行两次写入。
	dst := span.heapBits()
	o := (x - span.base()) / goarch.PtrSize
	i := o / ptrBits
	j := o % ptrBits
	if j+bits > ptrBits {
		// Two writes.
		// 需要两次写入
		bits0 := ptrBits - j
		bits1 := bits - bits0
		// 写入第一个字
		dst[i+0] = dst[i+0]&(^uintptr(0)>>bits0) | (src << j)
		// 写入第二个字
		dst[i+1] = dst[i+1]&^((1<<bits1)-1) | (src >> bits0)
	} else {
		// One write.
		// 只需要一次写入
		dst[i] = (dst[i] &^ (((1 << bits) - 1) << j)) | (src << j)
	}

	const doubleCheck = false
	if doubleCheck {
		// 双重检查写入的位是否正确
		srcRead := span.heapBitsSmallForAddr(x)
		if srcRead != src {
			print("runtime: x=", hex(x), " i=", i, " j=", j, " bits=", bits, "\n")
			print("runtime: dataSize=", dataSize, " typ.Size_=", typ.Size_, " typ.PtrBytes=", typ.PtrBytes, "\n")
			print("runtime: src0=", hex(src0), " src=", hex(src), " srcRead=", hex(srcRead), "\n")
			throw("bad pointer bits written for small object")
		}
	}
	return
}

// heapSetType records that the new allocation [x, x+size)
// holds in [x, x+dataSize) one or more values of type typ.
// (The number of values is given by dataSize / typ.Size.)
// If dataSize < size, the fragment [x+dataSize, x+size) is
// recorded as non-pointer data.
// It is known that the type has pointers somewhere;
// malloc does not call heapSetType when there are no pointers.
//
// There can be read-write races between heapSetType and things
// that read the heap metadata like scanobject. However, since
// heapSetType is only used for objects that have not yet been
// made reachable, readers will ignore bits being modified by this
// function. This does mean this function cannot transiently modify
// shared memory that belongs to neighboring objects. Also, on weakly-ordered
// machines, callers must execute a store/store (publication) barrier
// between calling this function and making the object reachable.

// heapSetType 记录新分配的内存区域 [x, x+size) 中
// 在 [x, x+dataSize) 范围内包含一个或多个类型为 typ 的值
// (值的数量由 dataSize / typ.Size 给出)
// 如果 dataSize < size，则 [x+dataSize, x+size) 这段内存
// 被记录为非指针数据
// 已知该类型在某处包含指针；
// 当没有指针时，malloc 不会调用 heapSetType

// heapSetType 与读取堆元数据的操作（如 scanobject）之间可能存在读写竞争
// 但是，由于 heapSetType 仅用于尚未可达的对象，
// 读取器会忽略此函数正在修改的位
// 这意味着此函数不能临时修改属于相邻对象的共享内存
// 此外，在弱序机器上，调用者必须在此函数调用和使对象可达之间
// 执行 store/store（发布）屏障
func heapSetType(x, dataSize uintptr, typ *_type, header **_type, span *mspan) (scanSize uintptr) {
	const doubleCheck = false

	gctyp := typ
	if header == nil {
		if doubleCheck && (!heapBitsInSpan(dataSize) || !heapBitsInSpan(span.elemsize)) {
			throw("tried to write heap bits, but no heap bits in span")
		}
		// Handle the case where we have no malloc header.
		// 处理没有 malloc header 的情况
		scanSize = span.writeHeapBitsSmall(x, dataSize, typ)
	} else {
		if typ.Kind_&abi.KindGCProg != 0 {
			// Allocate space to unroll the gcprog. This space will consist of
			// a dummy _type value and the unrolled gcprog. The dummy _type will
			// refer to the bitmap, and the mspan will refer to the dummy _type.
			// 分配空间来展开 gcprog。这个空间将包含一个虚拟的 _type 值和展开后的 gcprog。
			// 虚拟的 _type 将引用位图，而 mspan 将引用这个虚拟的 _type。
			if span.spanclass.sizeclass() != 0 {
				throw("GCProg for type that isn't large")
			}
			spaceNeeded := alignUp(unsafe.Sizeof(_type{}), goarch.PtrSize)
			heapBitsOff := spaceNeeded
			spaceNeeded += alignUp(typ.PtrBytes/goarch.PtrSize/8, goarch.PtrSize)
			npages := alignUp(spaceNeeded, pageSize) / pageSize
			var progSpan *mspan
			systemstack(func() {
				progSpan = mheap_.allocManual(npages, spanAllocPtrScalarBits)
				memclrNoHeapPointers(unsafe.Pointer(progSpan.base()), progSpan.npages*pageSize)
			})
			// Write a dummy _type in the new space.
			//
			// We only need to write size, PtrBytes, and GCData, since that's all
			// the GC cares about.
			// 在新的空间中写入一个虚拟的 _type。
			//
			// 我们只需要写入 size、PtrBytes 和 GCData，因为这就是 GC 关心的全部内容。
			gctyp = (*_type)(unsafe.Pointer(progSpan.base()))
			gctyp.Size_ = typ.Size_
			gctyp.PtrBytes = typ.PtrBytes
			gctyp.GCData = (*byte)(add(unsafe.Pointer(progSpan.base()), heapBitsOff))
			gctyp.TFlag = abi.TFlagUnrolledBitmap

			// Expand the GC program into space reserved at the end of the new span.
			// 将 GC 程序展开到新 span 末尾保留的空间中
			runGCProg(addb(typ.GCData, 4), gctyp.GCData)
		}

		// Write out the header.
		// 写出 header
		*header = gctyp
		scanSize = span.elemsize
	}

	if doubleCheck {
		doubleCheckHeapPointers(x, dataSize, gctyp, header, span)

		// To exercise the less common path more often, generate
		// a random interior pointer and make sure iterating from
		// that point works correctly too.
		// 为了更频繁地测试不太常见的路径，生成一个随机的内部指针
		// 并确保从该点开始的迭代也能正确工作
		maxIterBytes := span.elemsize
		if header == nil {
			maxIterBytes = dataSize
		}
		// 计算一个随机的偏移量，并确保它按指针大小对齐
		off := alignUp(uintptr(cheaprand())%dataSize, goarch.PtrSize)
		size := dataSize - off
		if size == 0 {
			off -= goarch.PtrSize
			size += goarch.PtrSize
		}
		// 计算内部指针的位置
		interior := x + off
		// 随机减少大小，并确保按指针大小对齐
		size -= alignDown(uintptr(cheaprand())%size, goarch.PtrSize)
		if size == 0 {
			size = goarch.PtrSize
		}
		// Round up the type to the size of the type.
		// 将大小向上取整到类型的整数倍
		size = (size + gctyp.Size_ - 1) / gctyp.Size_ * gctyp.Size_
		// 确保不会超出最大迭代字节数
		if interior+size > x+maxIterBytes {
			size = x + maxIterBytes - interior
		}
		// 检查内部指针的堆指针
		doubleCheckHeapPointersInterior(x, interior, size, dataSize, gctyp, header, span)
	}
	return
}

func doubleCheckHeapPointers(x, dataSize uintptr, typ *_type, header **_type, span *mspan) {
	// Check that scanning the full object works.
	// 检查完整对象的扫描是否正常工作
	tp := span.typePointersOfUnchecked(span.objBase(x))
	maxIterBytes := span.elemsize
	if header == nil {
		maxIterBytes = dataSize
	}
	bad := false
	for i := uintptr(0); i < maxIterBytes; i += goarch.PtrSize {
		// Compute the pointer bit we want at offset i.
		// 计算在偏移量 i 处我们期望的指针位
		want := false
		if i < span.elemsize {
			off := i % typ.Size_
			if off < typ.PtrBytes {
				j := off / goarch.PtrSize
				want = *addb(typ.GCData, j/8)>>(j%8)&1 != 0
			}
		}
		if want {
			var addr uintptr
			tp, addr = tp.next(x + span.elemsize)
			if addr == 0 {
				println("runtime: found bad iterator")
			}
			if addr != x+i {
				print("runtime: addr=", hex(addr), " x+i=", hex(x+i), "\n")
				bad = true
			}
		}
	}
	if !bad {
		var addr uintptr
		tp, addr = tp.next(x + span.elemsize)
		if addr == 0 {
			return
		}
		println("runtime: extra pointer:", hex(addr))
	}
	print("runtime: hasHeader=", header != nil, " typ.Size_=", typ.Size_, " hasGCProg=", typ.Kind_&abi.KindGCProg != 0, "\n")
	print("runtime: x=", hex(x), " dataSize=", dataSize, " elemsize=", span.elemsize, "\n")
	print("runtime: typ=", unsafe.Pointer(typ), " typ.PtrBytes=", typ.PtrBytes, "\n")
	print("runtime: limit=", hex(x+span.elemsize), "\n")
	tp = span.typePointersOfUnchecked(x)
	dumpTypePointers(tp)
	for {
		var addr uintptr
		if tp, addr = tp.next(x + span.elemsize); addr == 0 {
			println("runtime: would've stopped here")
			dumpTypePointers(tp)
			break
		}
		print("runtime: addr=", hex(addr), "\n")
		dumpTypePointers(tp)
	}
	throw("heapSetType: pointer entry not correct")
}

// doubleCheckHeapPointersInterior checks if the interior pointers in a heap object are correct
// 检查堆对象中的内部指针是否正确
func doubleCheckHeapPointersInterior(x, interior, size, dataSize uintptr, typ *_type, header **_type, span *mspan) {
	bad := false
	// Check if interior pointer is valid (must be >= x)
	// 检查内部指针是否有效（必须大于等于x）
	if interior < x {
		print("runtime: interior=", hex(interior), " x=", hex(x), "\n")
		throw("found bad interior pointer")
	}
	off := interior - x
	tp := span.typePointersOf(interior, size)
	for i := off; i < off+size; i += goarch.PtrSize {
		// Compute the pointer bit we want at offset i.
		// 计算在偏移量i处我们期望的指针位
		want := false
		if i < span.elemsize {
			off := i % typ.Size_
			if off < typ.PtrBytes {
				j := off / goarch.PtrSize
				want = *addb(typ.GCData, j/8)>>(j%8)&1 != 0
			}
		}
		if want {
			var addr uintptr
			tp, addr = tp.next(interior + size)
			if addr == 0 {
				println("runtime: found bad iterator")
				bad = true
			}
			if addr != x+i {
				print("runtime: addr=", hex(addr), " x+i=", hex(x+i), "\n")
				bad = true
			}
		}
	}
	// If no errors found so far, check for extra pointers
	// 如果到目前为止没有发现错误，检查是否有额外的指针
	if !bad {
		var addr uintptr
		tp, addr = tp.next(interior + size)
		if addr == 0 {
			return
		}
		println("runtime: extra pointer:", hex(addr))
	}
	// Print debug information
	// 打印调试信息
	print("runtime: hasHeader=", header != nil, " typ.Size_=", typ.Size_, "\n")
	print("runtime: x=", hex(x), " dataSize=", dataSize, " elemsize=", span.elemsize, " interior=", hex(interior), " size=", size, "\n")
	print("runtime: limit=", hex(interior+size), "\n")
	tp = span.typePointersOf(interior, size)
	dumpTypePointers(tp)
	// Iterate through all pointers and print them
	// 遍历所有指针并打印它们
	for {
		var addr uintptr
		if tp, addr = tp.next(interior + size); addr == 0 {
			println("runtime: would've stopped here")
			dumpTypePointers(tp)
			break
		}
		print("runtime: addr=", hex(addr), "\n")
		dumpTypePointers(tp)
	}

	// Print the expected pointer bitmap
	// 打印期望的指针位图
	print("runtime: want: ")
	for i := off; i < off+size; i += goarch.PtrSize {
		// Compute the pointer bit we want at offset i.
		// 计算在偏移量i处我们期望的指针位
		want := false
		if i < dataSize {
			off := i % typ.Size_
			if off < typ.PtrBytes {
				j := off / goarch.PtrSize
				want = *addb(typ.GCData, j/8)>>(j%8)&1 != 0
			}
		}
		if want {
			print("1")
		} else {
			print("0")
		}
	}
	println()

	throw("heapSetType: pointer entry not correct")
}

//go:nosplit
func doubleCheckTypePointersOfType(s *mspan, typ *_type, addr, size uintptr) {
	// 如果类型为空或者是GC程序类型，直接返回
	if typ == nil || typ.Kind_&abi.KindGCProg != 0 {
		return
	}
	if typ.Kind_&abi.KindMask == abi.Interface {
		// Interfaces are unfortunately inconsistently handled
		// when it comes to the type pointer, so it's easy to
		// produce a lot of false positives here.
		// 接口类型在类型指针处理上不一致，容易产生误报，所以直接返回
		return
	}
	// 使用两种不同的方法获取类型指针
	tp0 := s.typePointersOfType(typ, addr) // 基于类型信息获取指针
	tp1 := s.typePointersOf(addr, size)    // 基于地址和大小获取指针
	failed := false
	// 比较两种方法获取的指针是否一致
	for {
		var addr0, addr1 uintptr
		tp0, addr0 = tp0.next(addr + size)
		tp1, addr1 = tp1.next(addr + size)
		if addr0 != addr1 {
			failed = true
			break
		}
		if addr0 == 0 {
			break
		}
	}
	// 如果发现不一致，打印详细信息并抛出异常
	if failed {
		tp0 := s.typePointersOfType(typ, addr)
		tp1 := s.typePointersOf(addr, size)
		print("runtime: addr=", hex(addr), " size=", size, "\n")
		print("runtime: type=", toRType(typ).string(), "\n")
		dumpTypePointers(tp0)
		dumpTypePointers(tp1)
		// 打印所有不匹配的指针地址
		for {
			var addr0, addr1 uintptr
			tp0, addr0 = tp0.next(addr + size)
			tp1, addr1 = tp1.next(addr + size)
			print("runtime: ", hex(addr0), " ", hex(addr1), "\n")
			if addr0 == 0 && addr1 == 0 {
				break
			}
		}
		throw("mismatch between typePointersOfType and typePointersOf")
	}
}

// dumpTypePointers prints the contents of a typePointers struct for debugging purposes
// 打印typePointers结构体的内容，用于调试目的
func dumpTypePointers(tp typePointers) {
	// Print the element pointer and type pointer
	// 打印元素指针和类型指针
	print("runtime: tp.elem=", hex(tp.elem), " tp.typ=", unsafe.Pointer(tp.typ), "\n")
	// Print the address and start printing the mask
	// 打印地址并开始打印掩码
	print("runtime: tp.addr=", hex(tp.addr), " tp.mask=")
	// Iterate through each bit in the mask
	// 遍历掩码中的每一位
	for i := uintptr(0); i < ptrBits; i++ {
		// Check if the current bit is set in the mask
		// 检查掩码中当前位是否被设置
		if tp.mask&(uintptr(1)<<i) != 0 {
			print("1")
		} else {
			print("0")
		}
	}
	println()
}

// addb returns the byte pointer p+n.
// addb 返回指针p向后偏移n个字节后的位置
//
//go:nowritebarrier
//go:nosplit
func addb(p *byte, n uintptr) *byte {
	// Note: wrote out full expression instead of calling add(p, n)
	// to reduce the number of temporaries generated by the
	// compiler for this trivial expression during inlining.
	// 注意：这里直接写出完整表达式而不是调用add(p, n)
	// 是为了减少编译器在行内展开这个简单表达式时生成的临时变量数量
	return (*byte)(unsafe.Pointer(uintptr(unsafe.Pointer(p)) + n))
}

// subtractb returns the byte pointer p-n.
// subtractb 返回指针p向前偏移n个字节后的位置
//
//go:nowritebarrier
//go:nosplit
func subtractb(p *byte, n uintptr) *byte {
	// Note: wrote out full expression instead of calling add(p, -n)
	// to reduce the number of temporaries generated by the
	// compiler for this trivial expression during inlining.
	// 注意：这里直接写出完整表达式而不是调用add(p, -n)
	// 是为了减少编译器在行内展开这个简单表达式时生成的临时变量数量
	return (*byte)(unsafe.Pointer(uintptr(unsafe.Pointer(p)) - n))
}

// add1 returns the byte pointer p+1.
// add1 返回指针p向后偏移1个字节后的位置
//
//go:nowritebarrier
//go:nosplit
func add1(p *byte) *byte {
	// Note: wrote out full expression instead of calling addb(p, 1)
	// to reduce the number of temporaries generated by the
	// compiler for this trivial expression during inlining.
	// 注意：这里直接写出完整表达式而不是调用addb(p, 1)
	// 是为了减少编译器在行内展开这个简单表达式时生成的临时变量数量
	return (*byte)(unsafe.Pointer(uintptr(unsafe.Pointer(p)) + 1))
}

// subtract1 returns the byte pointer p-1.
// subtract1 返回指针p向前偏移1个字节后的位置
//
// nosplit because it is used during write barriers and must not be preempted.
// nosplit 标记表示该函数在写屏障期间使用，不能被抢占
//
//go:nowritebarrier
//go:nosplit
func subtract1(p *byte) *byte {
	// Note: wrote out full expression instead of calling subtractb(p, 1)
	// to reduce the number of temporaries generated by the
	// compiler for this trivial expression during inlining.
	// 注意：这里直接写出完整表达式而不是调用subtractb(p, 1)
	// 是为了减少编译器在行内展开这个简单表达式时生成的临时变量数量
	return (*byte)(unsafe.Pointer(uintptr(unsafe.Pointer(p)) - 1))
}

// markBits provides access to the mark bit for an object in the heap.
// bytep points to the byte holding the mark bit.
// mask is a byte with a single bit set that can be &ed with *bytep
// to see if the bit has been set.
// *m.byte&m.mask != 0 indicates the mark bit is set.
// index can be used along with span information to generate
// the address of the object in the heap.
// We maintain one set of mark bits for allocation and one for
// marking purposes.

// markBits 结构体用于访问堆中对象的标记位
// bytep 指向包含标记位的字节
// mask 是一个设置了单个位的字节，可以与 *bytep 进行按位与操作
// 来检查该位是否被设置
// *m.byte&m.mask != 0 表示标记位已被设置
// index 可以与 span 信息一起使用来生成
// 堆中对象的地址
// 我们维护一组用于分配的标记位和一组用于
// 标记目的的标记位
type markBits struct {
	bytep *uint8  // 指向标记位所在字节的指针
	mask  uint8   // 用于检查标记位的掩码
	index uintptr // 对象在span中的索引
}

// allocBitsForIndex returns the markBits for the given allocBitIndex.
// allocBitsForIndex 返回给定 allocBitIndex 对应的 markBits
// The function is marked as nosplit to prevent stack growth checks during execution.
// 该函数被标记为 nosplit，以防止执行期间的栈增长检查
//
//go:nosplit
func (s *mspan) allocBitsForIndex(allocBitIndex uintptr) markBits {
	// Get the byte pointer and mask for the given bit index
	// 获取给定位索引对应的字节指针和掩码
	bytep, mask := s.allocBits.bitp(allocBitIndex)
	// Create and return a new markBits struct with the obtained values
	// 使用获取的值创建并返回一个新的 markBits 结构体
	return markBits{bytep, mask, allocBitIndex}
}

// refillAllocCache takes 8 bytes s.allocBits starting at whichByte
// and negates them so that ctz (count trailing zeros) instructions
// can be used. It then places these 8 bytes into the cached 64 bit
// s.allocCache.
// refillAllocCache 从 whichByte 开始获取 s.allocBits 的 8 个字节，
// 并对它们取反，以便可以使用 ctz（计算尾随零）指令。
// 然后将这 8 个字节放入缓存的 64 位 s.allocCache 中。
func (s *mspan) refillAllocCache(whichByte uint16) {
	// 获取从 whichByte 开始的 8 个字节的指针
	bytes := (*[8]uint8)(unsafe.Pointer(s.allocBits.bytep(uintptr(whichByte))))
	// 初始化 64 位缓存
	aCache := uint64(0)
	// 将 8 个字节按位组合成一个 64 位整数
	// 每个字节左移相应的位数后与 aCache 进行按位或操作
	aCache |= uint64(bytes[0])
	aCache |= uint64(bytes[1]) << (1 * 8)
	aCache |= uint64(bytes[2]) << (2 * 8)
	aCache |= uint64(bytes[3]) << (3 * 8)
	aCache |= uint64(bytes[4]) << (4 * 8)
	aCache |= uint64(bytes[5]) << (5 * 8)
	aCache |= uint64(bytes[6]) << (6 * 8)
	aCache |= uint64(bytes[7]) << (7 * 8)
	// 对组合后的 64 位整数取反，存入 allocCache
	s.allocCache = ^aCache
}

// nextFreeIndex returns the index of the next free object in s at
// or after s.freeindex.
// There are hardware instructions that can be used to make this
// faster if profiling warrants it.
// nextFreeIndex 返回 s 中从 s.freeindex 开始或之后的第一个空闲对象的索引。
// 如果性能分析表明有必要，可以使用硬件指令来加速这个过程。
func (s *mspan) nextFreeIndex() uint16 {
	sfreeindex := s.freeindex
	snelems := s.nelems
	if sfreeindex == snelems {
		return sfreeindex
	}
	if sfreeindex > snelems {
		throw("s.freeindex > s.nelems")
	}

	aCache := s.allocCache

	bitIndex := sys.TrailingZeros64(aCache)
	for bitIndex == 64 {
		// Move index to start of next cached bits.
		// 将索引移动到下一个缓存位的开始位置。
		sfreeindex = (sfreeindex + 64) &^ (64 - 1)
		if sfreeindex >= snelems {
			s.freeindex = snelems
			return snelems
		}
		whichByte := sfreeindex / 8
		// Refill s.allocCache with the next 64 alloc bits.
		// 用接下来的64个分配位重新填充 s.allocCache。
		s.refillAllocCache(whichByte)
		aCache = s.allocCache
		bitIndex = sys.TrailingZeros64(aCache)
		// nothing available in cached bits
		// grab the next 8 bytes and try again.
		// 缓存位中没有可用的位
		// 获取下一个8字节并重试。
	}
	result := sfreeindex + uint16(bitIndex)
	if result >= snelems {
		s.freeindex = snelems
		return snelems
	}

	s.allocCache >>= uint(bitIndex + 1)
	sfreeindex = result + 1

	if sfreeindex%64 == 0 && sfreeindex != snelems {
		// We just incremented s.freeindex so it isn't 0.
		// As each 1 in s.allocCache was encountered and used for allocation
		// it was shifted away. At this point s.allocCache contains all 0s.
		// Refill s.allocCache so that it corresponds
		// to the bits at s.allocBits starting at s.freeindex.
		// 我们刚刚增加了 s.freeindex，所以它不是0。
		// 当 s.allocCache 中的每个1被遇到并用于分配时，
		// 它就被移除了。此时 s.allocCache 包含全0。
		// 重新填充 s.allocCache，使其对应从 s.freeindex 开始的 s.allocBits 中的位。
		whichByte := sfreeindex / 8
		s.refillAllocCache(whichByte)
	}
	s.freeindex = sfreeindex
	return result
}

// isFree reports whether the index'th object in s is unallocated.
// isFree 报告 s 中索引为 index 的对象是否未被分配。
//
// The caller must ensure s.state is mSpanInUse, and there must have
// been no preemption points since ensuring this (which could allow a
// GC transition, which would allow the state to change).
// 调用者必须确保 s.state 是 mSpanInUse，并且从确保这一点开始
// 不能有任何抢占点（这可能导致 GC 转换，从而允许状态改变）。
func (s *mspan) isFree(index uintptr) bool {
	if index < uintptr(s.freeIndexForScan) {
		return false
	}
	bytep, mask := s.allocBits.bitp(index)
	return *bytep&mask == 0
}

// divideByElemSize returns n/s.elemsize.
// n must be within [0, s.npages*_PageSize),
// or may be exactly s.npages*_PageSize
// if s.elemsize is from sizeclasses.go.
//
// nosplit, because it is called by objIndex, which is nosplit
//
// divideByElemSize 返回 n/s.elemsize 的结果
// n 必须在 [0, s.npages*_PageSize) 范围内，
// 或者如果 s.elemsize 来自 sizeclasses.go，
// 则 n 可以正好等于 s.npages*_PageSize
//
// nosplit 标记，因为该函数被 nosplit 函数 objIndex 调用
//
//go:nosplit
func (s *mspan) divideByElemSize(n uintptr) uintptr {
	const doubleCheck = false

	// See explanation in mksizeclasses.go's computeDivMagic.
	// 使用魔法数进行快速除法运算
	// 具体解释见 mksizeclasses.go 中的 computeDivMagic
	q := uintptr((uint64(n) * uint64(s.divMul)) >> 32)

	if doubleCheck && q != n/s.elemsize {
		// 如果开启了双重检查且结果不匹配，则打印错误信息并抛出异常
		println(n, "/", s.elemsize, "should be", n/s.elemsize, "but got", q)
		throw("bad magic division")
	}
	return q
}

// nosplit, because it is called by other nosplit code like findObject
// nosplit 标记，因为该函数被其他 nosplit 代码如 findObject 调用
//
//go:nosplit
func (s *mspan) objIndex(p uintptr) uintptr {
	return s.divideByElemSize(p - s.base())
}

// markBitsForAddr returns the markBits for the object at address p.
// markBitsForAddr 返回地址 p 处对象的标记位
func markBitsForAddr(p uintptr) markBits {
	s := spanOf(p)
	objIndex := s.objIndex(p)
	return s.markBitsForIndex(objIndex)
}

// markBitsForIndex returns the markBits for the object at index objIndex.
// markBitsForIndex 返回索引 objIndex 处对象的标记位
func (s *mspan) markBitsForIndex(objIndex uintptr) markBits {
	bytep, mask := s.gcmarkBits.bitp(objIndex)
	return markBits{bytep, mask, objIndex}
}

// markBitsForBase returns the markBits for the first object in the span.
// markBitsForBase 返回 span 中第一个对象的标记位
func (s *mspan) markBitsForBase() markBits {
	return markBits{&s.gcmarkBits.x, uint8(1), 0}
}

// isMarked reports whether mark bit m is set.
// isMarked 报告标记位 m 是否被设置
func (m markBits) isMarked() bool {
	return *m.bytep&m.mask != 0
}

// setMarked sets the marked bit in the markbits, atomically.
// setMarked 原子地设置 markbits 中的标记位
func (m markBits) setMarked() {
	// Might be racing with other updates, so use atomic update always.
	// We used to be clever here and use a non-atomic update in certain
	// cases, but it's not worth the risk.
	// 可能存在与其他更新的竞争，所以始终使用原子更新
	// 我们之前在这里很聪明，在某些情况下使用非原子更新，
	// 但这样做不值得冒这个风险
	atomic.Or8(m.bytep, m.mask)
}

// setMarkedNonAtomic sets the marked bit in the markbits, non-atomically.
// setMarkedNonAtomic 非原子地设置 markbits 中的标记位
func (m markBits) setMarkedNonAtomic() {
	*m.bytep |= m.mask
}

// clearMarked clears the marked bit in the markbits, atomically.
// clearMarked 原子地清除 markbits 中的标记位
func (m markBits) clearMarked() {
	// Might be racing with other updates, so use atomic update always.
	// We used to be clever here and use a non-atomic update in certain
	// cases, but it's not worth the risk.
	// 可能存在与其他更新的竞争，所以始终使用原子更新
	// 我们之前在这里很聪明，在某些情况下使用非原子更新，
	// 但这样做不值得冒这个风险
	atomic.And8(m.bytep, ^m.mask)
}

// markBitsForSpan returns the markBits for the span base address base.
// markBitsForSpan 返回 span 基地址 base 对应的标记位
func markBitsForSpan(base uintptr) (mbits markBits) {
	mbits = markBitsForAddr(base)
	if mbits.mask != 1 {
		throw("markBitsForSpan: unaligned start")
	}
	return mbits
}

// advance advances the markBits to the next object in the span.
// advance 将标记位前进到 span 中的下一个对象
func (m *markBits) advance() {
	if m.mask == 1<<7 {
		m.bytep = (*uint8)(unsafe.Pointer(uintptr(unsafe.Pointer(m.bytep)) + 1))
		m.mask = 1
	} else {
		m.mask = m.mask << 1
	}
	m.index++
}

// clobberdeadPtr is a special value that is used by the compiler to
// clobber dead stack slots, when -clobberdead flag is set.
// clobberdeadPtr 是一个特殊值，当设置了 -clobberdead 标志时，
// 编译器用它来覆盖已死亡的栈槽位。这个值通过位运算构造，
// 在32位系统上为0xdeaddead，在64位系统上为0xdeaddeaddeaddead
const clobberdeadPtr = uintptr(0xdeaddead | 0xdeaddead<<((^uintptr(0)>>63)*32))

// badPointer throws bad pointer in heap panic.
// badPointer 在堆中发现错误指针时抛出 panic
func badPointer(s *mspan, p, refBase, refOff uintptr) {
	// Typically this indicates an incorrect use
	// of unsafe or cgo to store a bad pointer in
	// the Go heap. It may also indicate a runtime
	// bug.
	//
	// TODO(austin): We could be more aggressive
	// and detect pointers to unallocated objects
	// in allocated spans.
	// 通常这表明在 Go 堆中存储错误指针时
	// 不正确地使用了 unsafe 或 cgo。
	// 这也可能表明存在运行时错误。
	//
	// TODO(austin): 我们可以更激进地
	// 检测已分配 span 中指向未分配对象的指针。
	printlock()
	print("runtime: pointer ", hex(p))
	if s != nil {
		state := s.state.get()
		if state != mSpanInUse {
			print(" to unallocated span")
		} else {
			print(" to unused region of span")
		}
		print(" span.base()=", hex(s.base()), " span.limit=", hex(s.limit), " span.state=", state)
	}
	print("\n")
	if refBase != 0 {
		print("runtime: found in object at *(", hex(refBase), "+", hex(refOff), ")\n")
		gcDumpObject("object", refBase, refOff)
	}
	getg().m.traceback = 2
	throw("found bad pointer in Go heap (incorrect use of unsafe or cgo?)")
}

// findObject returns the base address for the heap object containing
// the address p, the object's span, and the index of the object in s.
// If p does not point into a heap object, it returns base == 0.
//
// If p points is an invalid heap pointer and debug.invalidptr != 0,
// findObject panics.
//
// refBase and refOff optionally give the base address of the object
// in which the pointer p was found and the byte offset at which it
// was found. These are used for error reporting.
//
// It is nosplit so it is safe for p to be a pointer to the current goroutine's stack.
// Since p is a uintptr, it would not be adjusted if the stack were to move.
//
// findObject 返回包含地址 p 的堆对象的基地址、对象的 span 以及对象在 s 中的索引。
// 如果 p 不指向堆对象，则返回 base == 0。
//
// 如果 p 指向无效的堆指针且 debug.invalidptr != 0，
// findObject 会触发 panic。
//
// refBase 和 refOff 可选地提供发现指针 p 的对象的基地址
// 以及发现它的字节偏移量。这些用于错误报告。
//
// 由于使用了 nosplit 标记，所以 p 可以安全地指向当前 goroutine 的栈。
// 因为 p 是 uintptr 类型，即使栈发生移动，它也不会被调整。
//
// findObject should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/bytedance/sonic
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname findObject
//go:nosplit
func findObject(p, refBase, refOff uintptr) (base uintptr, s *mspan, objIndex uintptr) {
	// 获取包含地址 p 的 span
	s = spanOf(p)
	// If s is nil, the virtual address has never been part of the heap.
	// This pointer may be to some mmap'd region, so we allow it.
	// 如果 s 为 nil，说明该虚拟地址从未属于堆内存
	// 这个指针可能指向某个 mmap 区域，所以我们允许这种情况
	if s == nil {
		if (GOARCH == "amd64" || GOARCH == "arm64") && p == clobberdeadPtr && debug.invalidptr != 0 {
			// Crash if clobberdeadPtr is seen. Only on AMD64 and ARM64 for now,
			// as they are the only platform where compiler's clobberdead mode is
			// implemented. On these platforms clobberdeadPtr cannot be a valid address.
			// 如果检测到 clobberdeadPtr 则崩溃。目前仅在 AMD64 和 ARM64 平台上实现，
			// 因为只有这些平台实现了编译器的 clobberdead 模式。
			// 在这些平台上 clobberdeadPtr 不能是一个有效地址。
			badPointer(s, p, refBase, refOff)
		}
		return
	}
	// If p is a bad pointer, it may not be in s's bounds.
	//
	// Check s.state to synchronize with span initialization
	// before checking other fields. See also spanOfHeap.
	// 如果 p 是一个坏指针，它可能不在 s 的边界内
	//
	// 在检查其他字段之前，先检查 s.state 以同步 span 的初始化
	// 另见 spanOfHeap
	if state := s.state.get(); state != mSpanInUse || p < s.base() || p >= s.limit {
		// Pointers into stacks are also ok, the runtime manages these explicitly.
		// 指向栈的指针也是可以的，运行时显式管理这些指针
		if state == mSpanManual {
			return
		}
		// The following ensures that we are rigorous about what data
		// structures hold valid pointers.
		// 以下确保我们对哪些数据结构持有有效指针是严格的
		if debug.invalidptr != 0 {
			badPointer(s, p, refBase, refOff)
		}
		return
	}

	// 计算对象在 span 中的索引
	objIndex = s.objIndex(p)
	// 计算对象的基地址 = span 的基地址 + 对象索引 * 元素大小
	base = s.base() + objIndex*s.elemsize
	return
}

// reflect_verifyNotInHeapPtr reports whether converting the not-in-heap pointer into a unsafe.Pointer is ok.
// reflect_verifyNotInHeapPtr 报告将非堆指针转换为 unsafe.Pointer 是否安全
//
//go:linkname reflect_verifyNotInHeapPtr reflect.verifyNotInHeapPtr
func reflect_verifyNotInHeapPtr(p uintptr) bool {
	// Conversion to a pointer is ok as long as findObject above does not call badPointer.
	// Since we're already promised that p doesn't point into the heap, just disallow heap
	// pointers and the special clobbered pointer.
	// 只要上面的 findObject 不调用 badPointer，指针转换就是安全的。
	// 既然我们已经保证 p 不会指向堆内存，那么只需要禁止堆指针和特殊的 clobbered 指针即可。
	return spanOf(p) == nil && p != clobberdeadPtr
}

const ptrBits = 8 * goarch.PtrSize

// bulkBarrierBitmap executes write barriers for copying from [src,
// src+size) to [dst, dst+size) using a 1-bit pointer bitmap. src is
// assumed to start maskOffset bytes into the data covered by the
// bitmap in bits (which may not be a multiple of 8).
//
// This is used by bulkBarrierPreWrite for writes to data and BSS.
//
// bulkBarrierBitmap 使用1位指针位图执行从[src, src+size)到[dst, dst+size)的写入屏障。
// src 被假定为从位图覆盖的数据的 maskOffset 字节处开始（这可能不是8的倍数）。
//
// 这被 bulkBarrierPreWrite 用于对数据和 BSS 段的写入操作。
//
//go:nosplit
func bulkBarrierBitmap(dst, src, size, maskOffset uintptr, bits *uint8) {
	// 计算位图中的字偏移量
	word := maskOffset / goarch.PtrSize
	// 调整位图指针到正确的字节位置
	bits = addb(bits, word/8)
	// 创建初始掩码，用于检查位图中的特定位
	mask := uint8(1) << (word % 8)

	// 获取当前 goroutine 的写入缓冲区
	buf := &getg().m.p.ptr().wbBuf
	// 遍历目标内存区域
	for i := uintptr(0); i < size; i += goarch.PtrSize {
		if mask == 0 {
			// 当前字节的所有位都已处理完，移动到下一个字节
			bits = addb(bits, 1)
			if *bits == 0 {
				// 如果下一个字节全为0，跳过8个字（优化）
				i += 7 * goarch.PtrSize
				continue
			}
			mask = 1
		}
		// 检查当前位是否标记为指针
		if *bits&mask != 0 {
			// 获取目标位置的指针
			dstx := (*uintptr)(unsafe.Pointer(dst + i))
			if src == 0 {
				// 如果源地址为0，只记录目标指针
				p := buf.get1()
				p[0] = *dstx
			} else {
				// 否则同时记录源指针和目标指针
				srcx := (*uintptr)(unsafe.Pointer(src + i))
				p := buf.get2()
				p[0] = *dstx
				p[1] = *srcx
			}
		}
		// 移动到下一个位
		mask <<= 1
	}
}

// typeBitsBulkBarrier executes a write barrier for every
// pointer that would be copied from [src, src+size) to [dst,
// dst+size) by a memmove using the type bitmap to locate those
// pointer slots.
//
// The type typ must correspond exactly to [src, src+size) and [dst, dst+size).
// dst, src, and size must be pointer-aligned.
// The type typ must have a plain bitmap, not a GC program.
// The only use of this function is in channel sends, and the
// 64 kB channel element limit takes care of this for us.
//
// Must not be preempted because it typically runs right before memmove,
// and the GC must observe them as an atomic action.
//
// Callers must perform cgo checks if goexperiment.CgoCheck2.
//
// 中文注释：
// typeBitsBulkBarrier 为每个指针执行写屏障操作，这些指针将通过 memmove 从 [src, src+size) 复制到 [dst, dst+size)，
// 使用类型位图来定位这些指针槽。
//
// 类型 typ 必须与 [src, src+size) 和 [dst, dst+size) 完全对应。
// dst、src 和 size 必须是指针对齐的。
// 类型 typ 必须具有普通位图，而不是 GC 程序。
// 此函数仅用于通道发送，64 kB 的通道元素限制为我们处理了这个问题。
//
// 不能被抢占，因为它通常在 memmove 之前运行，
// 并且 GC 必须将它们视为原子操作。
//
// 如果启用了 goexperiment.CgoCheck2，调用者必须执行 cgo 检查。
//
//go:nosplit
func typeBitsBulkBarrier(typ *_type, dst, src, size uintptr) {
	// 检查类型是否为空
	if typ == nil {
		throw("runtime: typeBitsBulkBarrier without type")
	}
	// 验证类型大小是否与内存大小匹配
	if typ.Size_ != size {
		println("runtime: typeBitsBulkBarrier with type ", toRType(typ).string(), " of size ", typ.Size_, " but memory size", size)
		throw("runtime: invalid typeBitsBulkBarrier")
	}
	// 检查类型是否使用 GC 程序而不是普通位图
	if typ.Kind_&abi.KindGCProg != 0 {
		println("runtime: typeBitsBulkBarrier with type ", toRType(typ).string(), " with GC prog")
		throw("runtime: invalid typeBitsBulkBarrier")
	}
	// 如果写屏障未启用，直接返回
	if !writeBarrier.enabled {
		return
	}
	// 获取类型的 GC 数据（指针位图）
	ptrmask := typ.GCData
	// 获取当前 goroutine 的写入缓冲区
	buf := &getg().m.p.ptr().wbBuf
	var bits uint32
	// 遍历所有指针字节
	for i := uintptr(0); i < typ.PtrBytes; i += goarch.PtrSize {
		// 每处理 8 个指针（或 4 个在 32 位系统上）后，读取新的位图字节
		if i&(goarch.PtrSize*8-1) == 0 {
			bits = uint32(*ptrmask)
			ptrmask = addb(ptrmask, 1)
		} else {
			bits = bits >> 1
		}
		// 如果当前位表示一个指针
		if bits&1 != 0 {
			// 获取目标位置的指针
			dstx := (*uintptr)(unsafe.Pointer(dst + i))
			// 获取源位置的指针
			srcx := (*uintptr)(unsafe.Pointer(src + i))
			// 获取写入缓冲区的两个槽位
			p := buf.get2()
			// 记录目标指针和源指针
			p[0] = *dstx
			p[1] = *srcx
		}
	}
}

// countAlloc returns the number of objects allocated in span s by
// scanning the mark bitmap.
// countAlloc 通过扫描标记位图返回 span s 中已分配的对象数量
func (s *mspan) countAlloc() int {
	count := 0
	bytes := divRoundUp(uintptr(s.nelems), 8)
	// Iterate over each 8-byte chunk and count allocations
	// with an intrinsic. Note that newMarkBits guarantees that
	// gcmarkBits will be 8-byte aligned, so we don't have to
	// worry about edge cases, irrelevant bits will simply be zero.
	// 遍历每个 8 字节块并使用内置函数计算分配数量
	// 注意 newMarkBits 保证 gcmarkBits 是 8 字节对齐的，所以我们不需要
	// 担心边界情况，无关的位将简单地为零
	for i := uintptr(0); i < bytes; i += 8 {
		// Extract 64 bits from the byte pointer and get a OnesCount.
		// Note that the unsafe cast here doesn't preserve endianness,
		// but that's OK. We only care about how many bits are 1, not
		// about the order we discover them in.
		// 从字节指针中提取 64 位并获取 OnesCount
		// 注意这里的不安全转换不保留字节序，但这没关系
		// 我们只关心有多少位是 1，而不是发现它们的顺序
		mrkBits := *(*uint64)(unsafe.Pointer(s.gcmarkBits.bytep(i)))
		count += sys.OnesCount64(mrkBits)
	}
	return count
}

// Read the bytes starting at the aligned pointer p into a uintptr.
// Read is little-endian.
// 从对齐的指针 p 开始读取字节到 uintptr
// 读取采用小端序
func readUintptr(p *byte) uintptr {
	// 将字节指针转换为 uintptr 指针并解引用
	// 使用 unsafe.Pointer 进行类型转换
	x := *(*uintptr)(unsafe.Pointer(p))
	// 如果是大端序系统
	if goarch.BigEndian {
		// 64位系统下需要交换64位
		if goarch.PtrSize == 8 {
			return uintptr(sys.Bswap64(uint64(x)))
		}
		// 32位系统下需要交换32位
		return uintptr(sys.Bswap32(uint32(x)))
	}
	// 小端序系统直接返回
	return x
}

// 用于调试的指针掩码结构体
// 包含互斥锁和数据指针
var debugPtrmask struct {
	lock mutex // 互斥锁，用于保护数据访问
	data *byte // 指向字节数据的指针
}

// progToPointerMask returns the 1-bit pointer mask output by the GC program prog.
// size the size of the region described by prog, in bytes.
// The resulting bitvector will have no more than size/goarch.PtrSize bits.
// progToPointerMask 返回由 GC 程序 prog 输出的 1 位指针掩码
// size 是 prog 描述的区域的字节大小
// 结果位向量将不超过 size/goarch.PtrSize 位
func progToPointerMask(prog *byte, size uintptr) bitvector {
	// 计算需要的字节数：(size/指针大小 + 7) / 8
	// 加7是为了向上取整，除以8是将位转换为字节
	n := (size/goarch.PtrSize + 7) / 8
	// 分配持久内存，大小为n+1字节，额外1字节用于溢出检查
	// 使用persistentalloc确保内存不会被GC回收
	x := (*[1 << 30]byte)(persistentalloc(n+1, 1, &memstats.buckhash_sys))[:n+1]
	// 在最后一个字节设置溢出检查标记
	x[len(x)-1] = 0xa1 // overflow check sentinel
	// 运行GC程序，将结果写入x[0]开始的内存
	n = runGCProg(prog, &x[0])
	// 检查是否发生溢出
	if x[len(x)-1] != 0xa1 {
		throw("progToPointerMask: overflow")
	}
	// 返回位向量，包含写入的位数和指向数据的指针
	return bitvector{int32(n), &x[0]}
}

// Packed GC pointer bitmaps, aka GC programs.
//
// For large types containing arrays, the type information has a
// natural repetition that can be encoded to save space in the
// binary and in the memory representation of the type information.
//
// The encoding is a simple Lempel-Ziv style bytecode machine
// with the following instructions:
//
//	00000000: stop
//	0nnnnnnn: emit n bits copied from the next (n+7)/8 bytes
//	10000000 n c: repeat the previous n bits c times; n, c are varints
//	1nnnnnnn c: repeat the previous n bits c times; c is a varint

// runGCProg returns the number of 1-bit entries written to memory.

// 压缩的GC指针位图，也称为GC程序
//
// 对于包含数组的大型类型，类型信息中存在自然的重复模式
// 可以通过编码来节省二进制文件和类型信息内存表示中的空间
//
// 编码使用简单的Lempel-Ziv风格的字节码机器
// 具有以下指令：
//
//	00000000: 停止
//	0nnnnnnn: 从接下来的(n+7)/8个字节中复制n位
//	10000000 n c: 重复前n位c次；n和c是变长整数
//	1nnnnnnn c: 重复前n位c次；c是变长整数

// runGCProg返回写入内存的1位条目的数量
func runGCProg(prog, dst *byte) uintptr {
	// Save the initial destination address for later use
	dstStart := dst

	// Bits waiting to be written to memory.
	// 等待写入内存的位
	// bits: 存储待写入的位数据
	// nbits: 记录当前bits中有效位的数量
	var bits uintptr
	var nbits uintptr

	// Initialize program pointer to the start of the GC program
	// 初始化程序指针，指向GC程序的开始位置
	p := prog
Run:
	for {
		// Flush accumulated full bytes.
		// The rest of the loop assumes that nbits <= 7.
		// 刷新已累积的完整字节
		// 循环的其余部分假设nbits <= 7
		for ; nbits >= 8; nbits -= 8 {
			*dst = uint8(bits)
			dst = add1(dst)
			bits >>= 8
		}

		// Process one instruction.
		// 处理一条指令
		inst := uintptr(*p)
		p = add1(p)
		n := inst & 0x7F
		if inst&0x80 == 0 {
			// Literal bits; n == 0 means end of program.
			// 字面量位；n == 0表示程序结束
			if n == 0 {
				// Program is over.
				// 程序结束
				break Run
			}
			nbyte := n / 8
			for i := uintptr(0); i < nbyte; i++ {
				bits |= uintptr(*p) << nbits
				p = add1(p)
				*dst = uint8(bits)
				dst = add1(dst)
				bits >>= 8
			}
			if n %= 8; n > 0 {
				bits |= uintptr(*p) << nbits
				p = add1(p)
				nbits += n
			}
			continue Run
		}

		// Repeat. If n == 0, it is encoded in a varint in the next bytes.
		// 重复。如果n == 0，它被编码为下一个字节中的varint
		if n == 0 {
			// 使用varint编码读取重复次数
			// 每次读取7位，最高位作为继续标志
			for off := uint(0); ; off += 7 {
				x := uintptr(*p)
				p = add1(p)
				// 将读取的7位数据左移off位后与n进行或运算
				// 这样可以正确组装多字节的varint值
				n |= (x & 0x7F) << off
				// 如果最高位为0，表示varint结束
				if x&0x80 == 0 {
					break
				}
			}
		}

		// Count is encoded in a varint in the next bytes.
		// 计数被编码为下一个字节中的varint
		c := uintptr(0)
		for off := uint(0); ; off += 7 {
			x := uintptr(*p)
			p = add1(p)
			// 将读取的7位数据左移off位后与c进行或运算
			// 这样可以正确组装多字节的varint值
			c |= (x & 0x7F) << off
			// 如果最高位为0，表示varint结束
			if x&0x80 == 0 {
				break
			}
		}
		c *= n // now total number of bits to copy
		// 计算需要复制的总位数

		// If the number of bits being repeated is small, load them
		// into a register and use that register for the entire loop
		// instead of repeatedly reading from memory.
		// Handling fewer than 8 bits here makes the general loop simpler.
		// The cutoff is goarch.PtrSize*8 - 7 to guarantee that when we add
		// the pattern to a bit buffer holding at most 7 bits (a partial byte)
		// it will not overflow.
		// 如果重复的位数较少，将它们加载到寄存器中并在整个循环中使用该寄存器，
		// 而不是重复从内存中读取。在这里处理少于8位的情况使通用循环更简单。
		// 截止值是goarch.PtrSize*8 - 7，以确保当我们将模式添加到最多持有7位
		// (一个不完整的字节)的位缓冲区时不会溢出。

		src := dst
		const maxBits = goarch.PtrSize*8 - 7
		if n <= maxBits {
			// Start with bits in output buffer.
			// 从输出缓冲区中的位开始
			pattern := bits
			npattern := nbits

			// If we need more bits, fetch them from memory.
			// 如果需要更多位，从内存中获取它们
			src = subtract1(src)
			for npattern < n {
				pattern <<= 8
				pattern |= uintptr(*src)
				src = subtract1(src)
				npattern += 8
			}

			// We started with the whole bit output buffer,
			// and then we loaded bits from whole bytes.
			// Either way, we might now have too many instead of too few.
			// Discard the extra.
			// 我们从完整的位输出缓冲区开始，
			// 然后从完整的字节中加载位。
			// 无论哪种方式，我们现在可能有太多而不是太少。
			// 丢弃多余的位。
			if npattern > n {
				pattern >>= npattern - n
				npattern = n
			}

			// Replicate pattern to at most maxBits.
			// 将模式复制到最多maxBits位
			if npattern == 1 {
				// One bit being repeated.
				// If the bit is 1, make the pattern all 1s.
				// If the bit is 0, the pattern is already all 0s,
				// but we can claim that the number of bits
				// in the word is equal to the number we need (c),
				// because right shift of bits will zero fill.
				// 重复单个位的情况
				// 如果该位是1，则将模式全部设为1
				// 如果该位是0，则模式已经全部是0
				// 但我们可以声称字中的位数等于我们需要的数量(c)
				// 因为位的右移会用0填充
				if pattern == 1 {
					pattern = 1<<maxBits - 1
					npattern = maxBits
				} else {
					npattern = c
				}
			} else {
				b := pattern
				nb := npattern
				if nb+nb <= maxBits {
					// Double pattern until the whole uintptr is filled.
					// 重复模式直到填满整个uintptr
					for nb <= goarch.PtrSize*8 {
						b |= b << nb
						nb += nb
					}
					// Trim away incomplete copy of original pattern in high bits.
					// TODO(rsc): Replace with table lookup or loop on systems without divide?
					// 在高位中修剪掉原始模式的不完整副本
					// TODO(rsc): 在没有除法的系统上，用表查找或循环替换？
					nb = maxBits / npattern * npattern
					b &= 1<<nb - 1
					pattern = b
					npattern = nb
				}
			}

			// Add pattern to bit buffer and flush bit buffer, c/npattern times.
			// Since pattern contains >8 bits, there will be full bytes to flush
			// on each iteration.
			// 将模式添加到位缓冲区并刷新位缓冲区，重复c/npattern次
			// 由于模式包含超过8位，每次迭代都会有完整的字节需要刷新
			for ; c >= npattern; c -= npattern {
				bits |= pattern << nbits
				nbits += npattern
				for nbits >= 8 {
					*dst = uint8(bits)
					dst = add1(dst)
					bits >>= 8
					nbits -= 8
				}
			}

			// Add final fragment to bit buffer.
			// 将最后的片段添加到位缓冲区
			if c > 0 {
				pattern &= 1<<c - 1
				bits |= pattern << nbits
				nbits += c
			}
			continue Run
		}

		// Repeat; n too large to fit in a register.
		// Since nbits <= 7, we know the first few bytes of repeated data
		// are already written to memory.
		// 重复; n太大无法放入寄存器
		// 由于nbits <= 7，我们知道重复数据的前几个字节已经写入内存
		off := n - nbits // n > nbits because n > maxBits and nbits <= 7
		// Leading src fragment.
		// 处理源数据的前导片段
		src = subtractb(src, (off+7)/8)
		if frag := off & 7; frag != 0 {
			bits |= uintptr(*src) >> (8 - frag) << nbits
			src = add1(src)
			nbits += frag
			c -= frag
		}
		// Main loop: load one byte, write another.
		// The bits are rotating through the bit buffer.
		// 主循环：读取一个字节，写入另一个字节
		// 位在缓冲区中循环移动
		for i := c / 8; i > 0; i-- {
			bits |= uintptr(*src) << nbits
			src = add1(src)
			*dst = uint8(bits)
			dst = add1(dst)
			bits >>= 8
		}
		// Final src fragment.
		// 处理源数据的最后片段
		if c %= 8; c > 0 {
			bits |= (uintptr(*src) & (1<<c - 1)) << nbits
			nbits += c
		}
	}

	// Write any final bits out, using full-byte writes, even for the final byte.
	// 将剩余的位写入内存，使用完整的字节写入，即使是最后一个字节
	// 计算总共写入的位数：已写入的字节数 * 8 + 剩余的位数
	totalBits := (uintptr(unsafe.Pointer(dst))-uintptr(unsafe.Pointer(dstStart)))*8 + nbits
	// 将nbits向上取整到8的倍数，确保按字节对齐
	nbits += -nbits & 7
	// 循环处理剩余的位，每次处理8位（一个字节）
	for ; nbits > 0; nbits -= 8 {
		*dst = uint8(bits) // 写入一个字节
		dst = add1(dst)    // 移动目标指针
		bits >>= 8         // 右移8位，准备处理下一个字节
	}
	return totalBits
}

// materializeGCProg allocates space for the (1-bit) pointer bitmask
// for an object of size ptrdata.  Then it fills that space with the
// pointer bitmask specified by the program prog.
// The bitmask starts at s.startAddr.
// The result must be deallocated with dematerializeGCProg.
// materializeGCProg 为大小为ptrdata的对象分配(1位)指针位图的空间
// 然后用程序prog指定的指针位图填充该空间
// 位图从s.startAddr开始
// 结果必须使用dematerializeGCProg释放
func materializeGCProg(ptrdata uintptr, prog *byte) *mspan {
	// Each word of ptrdata needs one bit in the bitmap.
	// ptrdata的每个字在位图中需要一个位
	bitmapBytes := divRoundUp(ptrdata, 8*goarch.PtrSize)
	// Compute the number of pages needed for bitmapBytes.
	// 计算bitmapBytes需要的页数
	pages := divRoundUp(bitmapBytes, pageSize)
	s := mheap_.allocManual(pages, spanAllocPtrScalarBits)
	runGCProg(addb(prog, 4), (*byte)(unsafe.Pointer(s.startAddr)))
	return s
}

// dematerializeGCProg frees the space allocated by materializeGCProg
// dematerializeGCProg 释放由materializeGCProg分配的空间
func dematerializeGCProg(s *mspan) {
	mheap_.freeManual(s, spanAllocPtrScalarBits)
}

// dumpGCProg prints the GC program in a human-readable format.
// dumpGCProg 以人类可读的格式打印GC程序
func dumpGCProg(p *byte) {
	// nptr tracks the current position in the bitmap
	// nptr 跟踪位图中的当前位置
	nptr := 0
	for {
		// Read the next instruction byte
		// 读取下一个指令字节
		x := *p
		p = add1(p)
		// If we hit a 0 byte, we're done
		// 如果遇到0字节，表示程序结束
		if x == 0 {
			print("\t", nptr, " end\n")
			break
		}
		// Handle literal data (bits 0-6 contain the length)
		// 处理字面量数据（位0-6包含长度）
		if x&0x80 == 0 {
			print("\t", nptr, " lit ", x, ":")
			// Calculate number of bytes to read
			// 计算需要读取的字节数
			n := int(x+7) / 8
			// Read and print each byte in hex
			// 读取并以十六进制打印每个字节
			for i := 0; i < n; i++ {
				print(" ", hex(*p))
				p = add1(p)
			}
			print("\n")
			nptr += int(x)
		} else {
			// Handle repeat instruction
			// 处理重复指令
			// Extract the number of bits to repeat (bits 0-6)
			// 提取要重复的位数（位0-6）
			nbit := int(x &^ 0x80)
			// If nbit is 0, read a variable-length number
			// 如果nbit为0，读取一个可变长度的数字
			if nbit == 0 {
				for nb := uint(0); ; nb += 7 {
					x := *p
					p = add1(p)
					nbit |= int(x&0x7f) << nb
					if x&0x80 == 0 {
						break
					}
				}
			}
			// Read the repeat count as a variable-length number
			// 读取重复次数作为可变长度数字
			count := 0
			for nb := uint(0); ; nb += 7 {
				x := *p
				p = add1(p)
				count |= int(x&0x7f) << nb
				if x&0x80 == 0 {
					break
				}
			}
			print("\t", nptr, " repeat ", nbit, " × ", count, "\n")
			nptr += nbit * count
		}
	}
}

// Testing.

// reflect_gcbits returns the GC type info for x, for testing.
// The result is the bitmap entries (0 or 1), one entry per byte.
// reflect_gcbits 返回 x 的 GC 类型信息，用于测试。
// 返回值是位图条目（0 或 1），每个字节一个条目。
//
//go:linkname reflect_gcbits reflect.gcbits
func reflect_gcbits(x any) []byte {
	return getgcmask(x)
}

// Returns GC type info for the pointer stored in ep for testing.
// If ep points to the stack, only static live information will be returned
// (i.e. not for objects which are only dynamically live stack objects).
// 返回存储在 ep 中的指针的 GC 类型信息，用于测试。
// 如果 ep 指向栈，则只返回静态存活信息
// （即不会返回那些仅在动态存活栈对象中的对象信息）。
func getgcmask(ep any) (mask []byte) {
	// Get the underlying eface structure from the interface
	// 从接口中获取底层的 eface 结构
	e := *efaceOf(&ep)
	// Get the data pointer and type information
	// 获取数据指针和类型信息
	p := e.data
	t := e._type

	var et *_type
	// Check if the type is a pointer type
	// 检查类型是否为指针类型
	if t.Kind_&abi.KindMask != abi.Pointer {
		throw("bad argument to getgcmask: expected type to be a pointer to the value type whose mask is being queried")
	}
	// Get the element type of the pointer
	// 获取指针的元素类型
	et = (*ptrtype)(unsafe.Pointer(t)).Elem

	// data or bss
	// 遍历所有活跃的模块，检查指针是否在 data 或 bss 段中
	for _, datap := range activeModules() {
		// data
		// 检查指针是否在 data 段范围内
		if datap.data <= uintptr(p) && uintptr(p) < datap.edata {
			// 获取 data 段的 GC 位图数据
			bitmap := datap.gcdatamask.bytedata
			// 获取类型大小
			n := et.Size_
			// 创建掩码数组，大小为类型大小除以指针大小
			mask = make([]byte, n/goarch.PtrSize)
			// 遍历每个指针大小的位置
			for i := uintptr(0); i < n; i += goarch.PtrSize {
				// 计算在位图中的偏移量
				off := (uintptr(p) + i - datap.data) / goarch.PtrSize
				// 从位图中提取对应的位，并存储到掩码数组中
				mask[i/goarch.PtrSize] = (*addb(bitmap, off/8) >> (off % 8)) & 1
			}
			return
		}

		// bss
		// 检查指针是否在 bss 段范围内
		if datap.bss <= uintptr(p) && uintptr(p) < datap.ebss {
			// 获取 bss 段的 GC 位图数据
			bitmap := datap.gcbssmask.bytedata
			// 获取类型大小
			n := et.Size_
			// 创建掩码数组，大小为类型大小除以指针大小
			mask = make([]byte, n/goarch.PtrSize)
			// 遍历每个指针大小的位置
			for i := uintptr(0); i < n; i += goarch.PtrSize {
				// 计算在位图中的偏移量
				off := (uintptr(p) + i - datap.bss) / goarch.PtrSize
				// 从位图中提取对应的位，并存储到掩码数组中
				mask[i/goarch.PtrSize] = (*addb(bitmap, off/8) >> (off % 8)) & 1
			}
			return
		}
	}

	// heap
	if base, s, _ := findObject(uintptr(p), 0, 0); base != 0 {
		// 如果对象在堆上，检查其 span 是否不需要扫描
		if s.spanclass.noscan() {
			return nil
		}
		limit := base + s.elemsize

		// Move the base up to the iterator's start, because
		// we want to hide evidence of a malloc header from the
		// caller.
		// 将基地址移动到迭代器的起始位置，因为我们要向调用者隐藏 malloc 头部的信息
		tp := s.typePointersOfUnchecked(base)
		base = tp.addr

		// Unroll the full bitmap the GC would actually observe.
		// 展开 GC 实际观察到的完整位图
		maskFromHeap := make([]byte, (limit-base)/goarch.PtrSize)
		for {
			var addr uintptr
			if tp, addr = tp.next(limit); addr == 0 {
				break
			}
			maskFromHeap[(addr-base)/goarch.PtrSize] = 1
		}

		// Double-check that every part of the ptr/scalar we're not
		// showing the caller is zeroed. This keeps us honest that
		// that information is actually irrelevant.
		// 再次检查我们不向调用者展示的指针/标量部分的每个字节是否为零
		// 这确保我们诚实对待这些信息实际上是不相关的
		for i := limit; i < s.elemsize; i++ {
			if *(*byte)(unsafe.Pointer(i)) != 0 {
				throw("found non-zeroed tail of allocation")
			}
		}

		// Callers (and a check we're about to run) expects this mask
		// to end at the last pointer.
		// 调用者（以及我们即将运行的检查）期望这个掩码在最后一个指针处结束
		for len(maskFromHeap) > 0 && maskFromHeap[len(maskFromHeap)-1] == 0 {
			maskFromHeap = maskFromHeap[:len(maskFromHeap)-1]
		}

		if et.Kind_&abi.KindGCProg == 0 {
			// Unroll again, but this time from the type information.
			// 再次展开，但这次是从类型信息中获取
			maskFromType := make([]byte, (limit-base)/goarch.PtrSize)
			tp = s.typePointersOfType(et, base)
			for {
				var addr uintptr
				if tp, addr = tp.next(limit); addr == 0 {
					break
				}
				maskFromType[(addr-base)/goarch.PtrSize] = 1
			}

			// Validate that the prefix of maskFromType is equal to
			// maskFromHeap. maskFromType may contain more pointers than
			// maskFromHeap produces because maskFromHeap may be able to
			// get exact type information for certain classes of objects.
			// With maskFromType, we're always just tiling the type bitmap
			// through to the elemsize.
			//
			// It's OK if maskFromType has pointers in elemsize that extend
			// past the actual populated space; we checked above that all
			// that space is zeroed, so just the GC will just see nil pointers.
			// 验证 maskFromType 的前缀是否等于 maskFromHeap
			// maskFromType 可能包含比 maskFromHeap 更多的指针，因为 maskFromHeap 可能能够
			// 为某些类别的对象获取精确的类型信息
			// 对于 maskFromType，我们只是将类型位图平铺到 elemsize
			//
			// 如果 maskFromType 在 elemsize 中有超出实际填充空间的指针也没关系
			// 我们上面已经检查过所有这些空间都是零，所以 GC 只会看到 nil 指针
			differs := false
			// Compare the masks from heap and type to ensure they match
			// 比较来自堆和类型的掩码以确保它们匹配
			for i := range maskFromHeap {
				if maskFromHeap[i] != maskFromType[i] {
					differs = true
					break
				}
			}

			// If masks differ, print detailed debug information and throw an error
			// 如果掩码不同，打印详细的调试信息并抛出错误
			if differs {
				// Print the heap mask for debugging
				// 打印堆掩码用于调试
				print("runtime: heap mask=")
				for _, b := range maskFromHeap {
					print(b)
				}
				println()
				// Print the type mask for debugging
				// 打印类型掩码用于调试
				print("runtime: type mask=")
				for _, b := range maskFromType {
					print(b)
				}
				println()
				// Print the type information for debugging
				// 打印类型信息用于调试
				print("runtime: type=", toRType(et).string(), "\n")
				// Throw an error indicating mask mismatch
				// 抛出错误表明掩码不匹配
				throw("found two different masks from two different methods")
			}
		}

		// Select the heap mask to return. We may not have a type mask.
		// 选择要返回的堆掩码。我们可能没有类型掩码
		mask = maskFromHeap

		// Make sure we keep ep alive. We may have stopped referencing
		// ep's data pointer sometime before this point and it's possible
		// for that memory to get freed.
		// 确保 ep 保持活跃。我们可能在此之前已经停止引用 ep 的数据指针
		// 并且该内存可能已经被释放
		KeepAlive(ep)
		return
	}

	// stack
	// 检查指针是否在栈上
	if gp := getg(); gp.m.curg.stack.lo <= uintptr(p) && uintptr(p) < gp.m.curg.stack.hi {
		found := false
		var u unwinder
		// 遍历栈帧，查找包含该指针的栈帧
		// 从当前goroutine的PC和SP开始，初始化unwinder
		for u.initAt(gp.m.curg.sched.pc, gp.m.curg.sched.sp, 0, gp.m.curg, 0); u.valid(); u.next() {
			// 检查指针是否在当前栈帧的范围内
			if u.frame.sp <= uintptr(p) && uintptr(p) < u.frame.varp {
				found = true
				break
			}
		}
		if found {
			// 获取栈帧的局部变量位图
			locals, _, _ := u.frame.getStackMap(false)
			// 如果没有局部变量，直接返回
			if locals.n == 0 {
				return
			}
			// 计算局部变量区域的大小
			size := uintptr(locals.n) * goarch.PtrSize
			// 获取指针类型的大小
			n := (*ptrtype)(unsafe.Pointer(t)).Elem.Size_
			// 创建掩码数组，大小为指针类型大小除以指针大小
			mask = make([]byte, n/goarch.PtrSize)
			// 遍历每个指针大小的位置
			for i := uintptr(0); i < n; i += goarch.PtrSize {
				// 计算当前指针在栈帧中的偏移量
				off := (uintptr(p) + i - u.frame.varp + size) / goarch.PtrSize
				// 获取该位置的指针位信息并存入掩码
				mask[i/goarch.PtrSize] = locals.ptrbit(off)
			}
		}
		return
	}

	// otherwise, not something the GC knows about.
	// possibly read-only data, like malloc(0).
	// must not have pointers
	// 否则，这不是GC所知道的内容
	// 可能是只读数据，比如malloc(0)分配的内存
	// 一定不包含指针
	return
}
