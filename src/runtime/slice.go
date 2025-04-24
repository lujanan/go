// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/abi"
	"internal/goarch"
	"runtime/internal/math"
	"runtime/internal/sys"
	"unsafe"
)

type slice struct {
	array unsafe.Pointer
	len   int
	cap   int
}

// A notInHeapSlice is a slice backed by runtime/internal/sys.NotInHeap memory.
type notInHeapSlice struct {
	array *notInHeap
	len   int
	cap   int
}

func panicmakeslicelen() {
	panic(errorString("makeslice: len out of range"))
}

func panicmakeslicecap() {
	panic(errorString("makeslice: cap out of range"))
}

// makeslicecopy allocates a slice of "tolen" elements of type "et",
// then copies "fromlen" elements of type "et" into that new allocation from "from".
func makeslicecopy(et *_type, tolen int, fromlen int, from unsafe.Pointer) unsafe.Pointer {
	var tomem, copymem uintptr
	if uintptr(tolen) > uintptr(fromlen) {
		var overflow bool
		tomem, overflow = math.MulUintptr(et.Size_, uintptr(tolen))
		if overflow || tomem > maxAlloc || tolen < 0 {
			panicmakeslicelen()
		}
		copymem = et.Size_ * uintptr(fromlen)
	} else {
		// fromlen is a known good length providing and equal or greater than tolen,
		// thereby making tolen a good slice length too as from and to slices have the
		// same element width.
		tomem = et.Size_ * uintptr(tolen)
		copymem = tomem
	}

	var to unsafe.Pointer
	if !et.Pointers() {
		to = mallocgc(tomem, nil, false)
		if copymem < tomem {
			memclrNoHeapPointers(add(to, copymem), tomem-copymem)
		}
	} else {
		// Note: can't use rawmem (which avoids zeroing of memory), because then GC can scan uninitialized memory.
		to = mallocgc(tomem, et, true)
		if copymem > 0 && writeBarrier.enabled {
			// Only shade the pointers in old.array since we know the destination slice to
			// only contains nil pointers because it has been cleared during alloc.
			//
			// It's safe to pass a type to this function as an optimization because
			// from and to only ever refer to memory representing whole values of
			// type et. See the comment on bulkBarrierPreWrite.
			bulkBarrierPreWriteSrcOnly(uintptr(to), uintptr(from), copymem, et)
		}
	}

	if raceenabled {
		callerpc := getcallerpc()
		pc := abi.FuncPCABIInternal(makeslicecopy)
		racereadrangepc(from, copymem, callerpc, pc)
	}
	if msanenabled {
		msanread(from, copymem)
	}
	if asanenabled {
		asanread(from, copymem)
	}

	memmove(to, from, copymem)

	return to
}

// makeslice should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/bytedance/sonic
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname makeslice
func makeslice(et *_type, len, cap int) unsafe.Pointer {
	mem, overflow := math.MulUintptr(et.Size_, uintptr(cap))
	if overflow || mem > maxAlloc || len < 0 || len > cap {
		// NOTE: Produce a 'len out of range' error instead of a
		// 'cap out of range' error when someone does make([]T, bignumber).
		// 'cap out of range' is true too, but since the cap is only being
		// supplied implicitly, saying len is clearer.
		// See golang.org/issue/4085.
		mem, overflow := math.MulUintptr(et.Size_, uintptr(len))
		if overflow || mem > maxAlloc || len < 0 {
			panicmakeslicelen()
		}
		panicmakeslicecap()
	}

	return mallocgc(mem, et, true)
}

func makeslice64(et *_type, len64, cap64 int64) unsafe.Pointer {
	len := int(len64)
	if int64(len) != len64 {
		panicmakeslicelen()
	}

	cap := int(cap64)
	if int64(cap) != cap64 {
		panicmakeslicecap()
	}

	return makeslice(et, len, cap)
}

// growslice allocates new backing store for a slice.
// growslice 为切片分配新的底层存储空间。
//
// arguments:
// 参数:
//
//	oldPtr = pointer to the slice's backing array
//	oldPtr = 指向切片底层数组的指针
//	newLen = new length (= oldLen + num)
//	newLen = 新的长度 (= oldLen + num)
//	oldCap = original slice's capacity.
//	oldCap = 原始切片的容量
//	   num = number of elements being added
//	   num = 要添加的元素数量
//	    et = element type
//	    et = 元素类型
//
// return values:
// 返回值:
//
//	newPtr = pointer to the new backing store
//	newPtr = 指向新的底层存储空间的指针
//	newLen = same value as the argument
//	newLen = 与参数相同的值
//	newCap = capacity of the new backing store
//	newCap = 新的底层存储空间的容量
//
// Requires that uint(newLen) > uint(oldCap).
// 要求 uint(newLen) > uint(oldCap)。
// Assumes the original slice length is newLen - num
// 假设原始切片的长度是 newLen - num
//
// A new backing store is allocated with space for at least newLen elements.
// 分配一个新的底层存储空间，至少能容纳 newLen 个元素。
// Existing entries [0, oldLen) are copied over to the new backing store.
// 将现有的元素 [0, oldLen) 复制到新的底层存储空间。
// Added entries [oldLen, newLen) are not initialized by growslice
// (although for pointer-containing element types, they are zeroed). They
// must be initialized by the caller.
// 新增的元素 [oldLen, newLen) 不会被 growslice 初始化
// (尽管对于包含指针的元素类型，它们会被置零)。
// 它们必须由调用者初始化。
// Trailing entries [newLen, newCap) are zeroed.
// 尾部的元素 [newLen, newCap) 会被置零。
//
// growslice's odd calling convention makes the generated code that calls
// this function simpler. In particular, it accepts and returns the
// new length so that the old length is not live (does not need to be
// spilled/restored) and the new length is returned (also does not need
// to be spilled/restored).
// growslice 的特殊调用约定使得调用此函数的生成代码更简单。
// 特别是，它接受并返回新的长度，这样旧的长度就不需要保持活跃
// (不需要溢出/恢复)，而新的长度被返回(也不需要溢出/恢复)。
//
// growslice should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/bytedance/sonic
//   - github.com/chenzhuoyu/iasm
//   - github.com/cloudwego/dynamicgo
//   - github.com/ugorji/go/codec
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname growslice
func growslice(oldPtr unsafe.Pointer, newLen, oldCap, num int, et *_type) slice {
	oldLen := newLen - num
	if raceenabled {
		callerpc := getcallerpc()
		racereadrangepc(oldPtr, uintptr(oldLen*int(et.Size_)), callerpc, abi.FuncPCABIInternal(growslice))
	}
	if msanenabled {
		msanread(oldPtr, uintptr(oldLen*int(et.Size_)))
	}
	if asanenabled {
		asanread(oldPtr, uintptr(oldLen*int(et.Size_)))
	}

	if newLen < 0 {
		panic(errorString("growslice: len out of range"))
	}

	if et.Size_ == 0 {
		// append should not create a slice with nil pointer but non-zero len.
		// We assume that append doesn't need to preserve oldPtr in this case.
		// append 不应该创建一个指针为 nil 但长度非零的切片。
		// 我们假设在这种情况下 append 不需要保留 oldPtr。
		return slice{unsafe.Pointer(&zerobase), newLen, newLen}
	}

	newcap := nextslicecap(newLen, oldCap)

	var overflow bool
	var lenmem, newlenmem, capmem uintptr
	// Specialize for common values of et.Size.
	// For 1 we don't need any division/multiplication.
	// For goarch.PtrSize, compiler will optimize division/multiplication into a shift by a constant.
	// For powers of 2, use a variable shift.
	// 针对 et.Size 的常见值进行特殊处理。
	// 对于 1，我们不需要任何除法/乘法。
	// 对于 goarch.PtrSize，编译器会将除法/乘法优化为常量位移。
	// 对于 2 的幂，使用变量位移。
	// Check if the slice elements contain pointers
	// 检查切片元素是否包含指针
	noscan := !et.Pointers()
	switch {
	case et.Size_ == 1:
		// For 1-byte elements, no scaling needed
		// 对于1字节的元素，不需要缩放
		lenmem = uintptr(oldLen)
		newlenmem = uintptr(newLen)
		capmem = roundupsize(uintptr(newcap), noscan)
		overflow = uintptr(newcap) > maxAlloc
		newcap = int(capmem)
	case et.Size_ == goarch.PtrSize:
		// For pointer-sized elements, scale by pointer size
		// 对于指针大小的元素，按指针大小缩放
		lenmem = uintptr(oldLen) * goarch.PtrSize
		newlenmem = uintptr(newLen) * goarch.PtrSize
		capmem = roundupsize(uintptr(newcap)*goarch.PtrSize, noscan)
		overflow = uintptr(newcap) > maxAlloc/goarch.PtrSize
		newcap = int(capmem / goarch.PtrSize)
	case isPowerOfTwo(et.Size_):
		// For power-of-two sizes, use shifts
		// 对于2的幂大小的元素，使用位移操作
		var shift uintptr
		if goarch.PtrSize == 8 {
			// Mask shift for better code generation.
			// 对位移进行掩码处理以获得更好的代码生成。
			shift = uintptr(sys.TrailingZeros64(uint64(et.Size_))) & 63
		} else {
			// For 32-bit systems
			// 对于32位系统
			shift = uintptr(sys.TrailingZeros32(uint32(et.Size_))) & 31
		}
		lenmem = uintptr(oldLen) << shift
		newlenmem = uintptr(newLen) << shift
		capmem = roundupsize(uintptr(newcap)<<shift, noscan)
		overflow = uintptr(newcap) > (maxAlloc >> shift)
		newcap = int(capmem >> shift)
		capmem = uintptr(newcap) << shift
	default:
		// For all other element sizes, use multiplication
		// 对于所有其他元素大小，使用乘法
		lenmem = uintptr(oldLen) * et.Size_
		newlenmem = uintptr(newLen) * et.Size_
		capmem, overflow = math.MulUintptr(et.Size_, uintptr(newcap))
		capmem = roundupsize(capmem, noscan)
		newcap = int(capmem / et.Size_)
		capmem = uintptr(newcap) * et.Size_
	}

	// The check of overflow in addition to capmem > maxAlloc is needed
	// to prevent an overflow which can be used to trigger a segfault
	// on 32bit architectures with this example program:
	// 除了检查 capmem > maxAlloc 之外，还需要检查溢出，
	// 以防止在 32 位架构上使用以下示例程序触发段错误：
	//
	// type T [1<<27 + 1]int64
	//
	// var d T
	// var s []T
	//
	// func main() {
	//   s = append(s, d, d, d, d)
	//   print(len(s), "\n")
	// }
	if overflow || capmem > maxAlloc {
		panic(errorString("growslice: len out of range"))
	}

	var p unsafe.Pointer
	if !et.Pointers() {
		p = mallocgc(capmem, nil, false)
		// The append() that calls growslice is going to overwrite from oldLen to newLen.
		// Only clear the part that will not be overwritten.
		// The reflect_growslice() that calls growslice will manually clear
		// the region not cleared here.
		// 调用 growslice 的 append() 将覆盖从 oldLen 到 newLen 的部分。
		// 只清除不会被覆盖的部分。
		// 调用 growslice 的 reflect_growslice() 将手动清除这里未清除的区域。
		memclrNoHeapPointers(add(p, newlenmem), capmem-newlenmem)
	} else {
		// Note: can't use rawmem (which avoids zeroing of memory), because then GC can scan uninitialized memory.
		// 注意：不能使用 rawmem（避免内存置零），因为这样 GC 可能会扫描未初始化的内存。
		p = mallocgc(capmem, et, true)
		if lenmem > 0 && writeBarrier.enabled {
			// Only shade the pointers in oldPtr since we know the destination slice p
			// only contains nil pointers because it has been cleared during alloc.
			// 只遮蔽 oldPtr 中的指针，因为我们知道目标切片 p
			// 只包含 nil 指针，因为它在分配时已被清除。
			//
			// It's safe to pass a type to this function as an optimization because
			// from and to only ever refer to memory representing whole values of
			// type et. See the comment on bulkBarrierPreWrite.
			// 将类型传递给此函数作为优化是安全的，因为
			// from 和 to 只引用表示 et 类型完整值的内存。
			// 参见 bulkBarrierPreWrite 的注释。
			bulkBarrierPreWriteSrcOnly(uintptr(p), uintptr(oldPtr), lenmem-et.Size_+et.PtrBytes, et)
		}
	}
	memmove(p, oldPtr, lenmem)

	return slice{p, newLen, newcap}
}

// nextslicecap computes the next appropriate slice length.
func nextslicecap(newLen, oldCap int) int {
	newcap := oldCap
	doublecap := newcap + newcap
	if newLen > doublecap {
		return newLen
	}

	const threshold = 256
	if oldCap < threshold {
		return doublecap
	}
	for {
		// Transition from growing 2x for small slices
		// to growing 1.25x for large slices. This formula
		// gives a smooth-ish transition between the two.
		newcap += (newcap + 3*threshold) >> 2

		// We need to check `newcap >= newLen` and whether `newcap` overflowed.
		// newLen is guaranteed to be larger than zero, hence
		// when newcap overflows then `uint(newcap) > uint(newLen)`.
		// This allows to check for both with the same comparison.
		if uint(newcap) >= uint(newLen) {
			break
		}
	}

	// Set newcap to the requested cap when
	// the newcap calculation overflowed.
	if newcap <= 0 {
		return newLen
	}
	return newcap
}

// reflect_growslice should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/cloudwego/dynamicgo
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname reflect_growslice reflect.growslice
func reflect_growslice(et *_type, old slice, num int) slice {
	// Semantically equivalent to slices.Grow, except that the caller
	// is responsible for ensuring that old.len+num > old.cap.
	num -= old.cap - old.len // preserve memory of old[old.len:old.cap]
	new := growslice(old.array, old.cap+num, old.cap, num, et)
	// growslice does not zero out new[old.cap:new.len] since it assumes that
	// the memory will be overwritten by an append() that called growslice.
	// Since the caller of reflect_growslice is not append(),
	// zero out this region before returning the slice to the reflect package.
	if !et.Pointers() {
		oldcapmem := uintptr(old.cap) * et.Size_
		newlenmem := uintptr(new.len) * et.Size_
		memclrNoHeapPointers(add(new.array, oldcapmem), newlenmem-oldcapmem)
	}
	new.len = old.len // preserve the old length
	return new
}

func isPowerOfTwo(x uintptr) bool {
	return x&(x-1) == 0
}

// slicecopy is used to copy from a string or slice of pointerless elements into a slice.
func slicecopy(toPtr unsafe.Pointer, toLen int, fromPtr unsafe.Pointer, fromLen int, width uintptr) int {
	if fromLen == 0 || toLen == 0 {
		return 0
	}

	n := fromLen
	if toLen < n {
		n = toLen
	}

	if width == 0 {
		return n
	}

	size := uintptr(n) * width
	if raceenabled {
		callerpc := getcallerpc()
		pc := abi.FuncPCABIInternal(slicecopy)
		racereadrangepc(fromPtr, size, callerpc, pc)
		racewriterangepc(toPtr, size, callerpc, pc)
	}
	if msanenabled {
		msanread(fromPtr, size)
		msanwrite(toPtr, size)
	}
	if asanenabled {
		asanread(fromPtr, size)
		asanwrite(toPtr, size)
	}

	if size == 1 { // common case worth about 2x to do here
		// TODO: is this still worth it with new memmove impl?
		*(*byte)(toPtr) = *(*byte)(fromPtr) // known to be a byte pointer
	} else {
		memmove(toPtr, fromPtr, size)
	}
	return n
}

//go:linkname bytealg_MakeNoZero internal/bytealg.MakeNoZero
func bytealg_MakeNoZero(len int) []byte {
	if uintptr(len) > maxAlloc {
		panicmakeslicelen()
	}
	cap := roundupsize(uintptr(len), true)
	return unsafe.Slice((*byte)(mallocgc(uintptr(cap), nil, false)), cap)[:len]
}
