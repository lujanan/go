// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Malloc small size classes.
//
// See malloc.go for overview.
// See also mksizeclasses.go for how we decide what size classes to use.
//
// 小对象内存分配的大小类别。
//
// 有关概述，请参见 malloc.go。
// 有关我们如何决定使用哪些大小类别的详细信息，请参见 mksizeclasses.go。

package runtime

// Returns size of the memory block that mallocgc will allocate if you ask for the size,
// minus any inline space for metadata.
// 返回当你请求分配 size 大小的内存时，mallocgc 实际会分配的内存块大小，
// 减去任何内联的元数据空间。
func roundupsize(size uintptr, noscan bool) (reqSize uintptr) {
	reqSize = size
	if reqSize <= maxSmallSize-mallocHeaderSize {
		// Small object.
		// 小对象
		if !noscan && reqSize > minSizeForMallocHeader { // !noscan && !heapBitsInSpan(reqSize)
			// 如果对象需要扫描（包含指针）且大小超过最小元数据头大小，
			// 则添加元数据头大小
			reqSize += mallocHeaderSize
		}
		// (reqSize - size) is either mallocHeaderSize or 0. We need to subtract mallocHeaderSize
		// from the result if we have one, since mallocgc will add it back in.
		// (reqSize - size) 要么是 mallocHeaderSize 要么是 0。
		// 如果添加了元数据头，我们需要从结果中减去它，
		// 因为 mallocgc 会重新添加它。
		if reqSize <= smallSizeMax-8 {
			// 对于较小的对象，使用 size_to_class8 查找对应的 size class
			return uintptr(class_to_size[size_to_class8[divRoundUp(reqSize, smallSizeDiv)]]) - (reqSize - size)
		}
		// 对于较大的小对象，使用 size_to_class128 查找对应的 size class
		return uintptr(class_to_size[size_to_class128[divRoundUp(reqSize-smallSizeMax, largeSizeDiv)]]) - (reqSize - size)
	}
	// Large object. Align reqSize up to the next page. Check for overflow.
	// 大对象。将 reqSize 向上对齐到下一个页面大小。检查溢出。
	reqSize += pageSize - 1
	if reqSize < size {
		// 如果发生溢出，返回原始大小
		return size
	}
	// 将大小向下对齐到页面边界
	return reqSize &^ (pageSize - 1)
}
