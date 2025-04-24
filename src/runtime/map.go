// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// This file contains the implementation of Go's map type.
// 本文件包含了 Go 语言 map 类型的实现。
//
// A map is just a hash table. The data is arranged
// into an array of buckets. Each bucket contains up to
// 8 key/elem pairs. The low-order bits of the hash are
// used to select a bucket. Each bucket contains a few
// high-order bits of each hash to distinguish the entries
// within a single bucket.
// map 就是一个哈希表。数据被组织成一个桶数组。
// 每个桶最多包含 8 个键值对。哈希值的低位用于选择桶。
// 每个桶还包含了一些哈希值的高位，用于区分同一个桶中的不同条目。
//
// If more than 8 keys hash to a bucket, we chain on
// extra buckets.
// 如果超过 8 个键哈希到同一个桶，我们会链接上额外的溢出桶。
//
// When the hashtable grows, we allocate a new array
// of buckets twice as big. Buckets are incrementally
// copied from the old bucket array to the new bucket array.
// 当哈希表增长时，我们会分配一个两倍大小的新桶数组。
// 桶会被逐步从旧桶数组复制到新桶数组。
//
// Map iterators walk through the array of buckets and
// return the keys in walk order (bucket #, then overflow
// chain order, then bucket index).  To maintain iteration
// semantics, we never move keys within their bucket (if
// we did, keys might be returned 0 or 2 times).  When
// growing the table, iterators remain iterating through the
// old table and must check the new table if the bucket
// they are iterating through has been moved ("evacuated")
// to the new table.
// map 迭代器按照遍历顺序遍历桶数组并返回键
// (先是桶编号，然后是溢出链顺序，最后是桶内索引)。
// 为了维持迭代语义，我们从不在桶内移动键
// (如果这样做，键可能会被返回 0 次或 2 次)。
// 当表在增长时，迭代器会继续遍历旧表，
// 如果它们正在遍历的桶已被移动("疏散")到新表，
// 则必须检查新表。
// Picking loadFactor: too large and we have lots of overflow
// buckets, too small and we waste a lot of space. I wrote
// a simple program to check some stats for different loads:
// (64-bit, 8 byte keys and elems)
//  loadFactor    %overflow  bytes/entry     hitprobe    missprobe
//        4.00         2.13        20.77         3.00         4.00
//        4.50         4.05        17.30         3.25         4.50
//        5.00         6.85        14.77         3.50         5.00
//        5.50        10.55        12.94         3.75         5.50
//        6.00        15.27        11.67         4.00         6.00
//        6.50        20.90        10.79         4.25         6.50
//        7.00        27.14        10.15         4.50         7.00
//        7.50        34.03         9.73         4.75         7.50
//        8.00        41.10         9.40         5.00         8.00
//
// %overflow   = percentage of buckets which have an overflow bucket
// bytes/entry = overhead bytes used per key/elem pair
// hitprobe    = # of entries to check when looking up a present key
// missprobe   = # of entries to check when looking up an absent key
//
// Keep in mind this data is for maximally loaded tables, i.e. just
// before the table grows. Typical tables will be somewhat less loaded.
//
// 选择装载因子(loadFactor)：太大会导致大量溢出桶，太小则会浪费大量空间。
// 我写了一个简单的程序来检查不同负载下的一些统计数据：
// (64位系统，8字节的键和值)
//  装载因子    溢出率%   每项字节数     命中探测    未命中探测
//     4.00      2.13      20.77        3.00        4.00
//     4.50      4.05      17.30        3.25        4.50
//     5.00      6.85      14.77        3.50        5.00
//     5.50     10.55      12.94        3.75        5.50
//     6.00     15.27      11.67        4.00        6.00
//     6.50     20.90      10.79        4.25        6.50
//     7.00     27.14      10.15        4.50        7.00
//     7.50     34.03       9.73        4.75        7.50
//     8.00     41.10       9.40        5.00        8.00
//
// 溢出率%    = 拥有溢出桶的桶的百分比
// 每项字节数 = 每个键值对使用的开销字节数
// 命中探测   = 查找存在的键时需要检查的条目数
// 未命中探测 = 查找不存在的键时需要检查的条目数
//
// 请注意，这些数据是针对最大负载的表的，即在表增长之前的状态。
// 典型的表的负载会稍微低一些。

import (
	"internal/abi"
	"internal/goarch"
	"internal/runtime/atomic"
	"runtime/internal/math"
	"unsafe"
)

const (
	// Maximum number of key/elem pairs a bucket can hold.
	// 一个桶可以容纳的键值对的最大数量。
	bucketCntBits = abi.MapBucketCountBits

	// Maximum average load of a bucket that triggers growth is bucketCnt*13/16 (about 80% full)
	// Because of minimum alignment rules, bucketCnt is known to be at least 8.
	// Represent as loadFactorNum/loadFactorDen, to allow integer math.
	// 触发增长的桶的最大平均负载是 bucketCnt*13/16（约80%满）
	// 由于最小对齐规则，bucketCnt 已知至少为 8。
	// 表示为 loadFactorNum/loadFactorDen，以允许整数运算。
	loadFactorDen = 2
	loadFactorNum = loadFactorDen * abi.MapBucketCount * 13 / 16

	// data offset should be the size of the bmap struct, but needs to be
	// aligned correctly. For amd64p32 this means 64-bit alignment
	// even though pointers are 32 bit.
	// 数据偏移量应该是 bmap 结构体的大小，但需要正确对齐。
	// 对于 amd64p32，这意味着需要 64 位对齐，即使指针是 32 位的。
	dataOffset = unsafe.Offsetof(struct {
		b bmap
		v int64
	}{}.v)

	// Possible tophash values. We reserve a few possibilities for special marks.
	// Each bucket (including its overflow buckets, if any) will have either all or none of its
	// entries in the evacuated* states (except during the evacuate() method, which only happens
	// during map writes and thus no one else can observe the map during that time).
	// 可能的 tophash 值。我们保留一些特殊标记。
	// 每个桶（包括它的溢出桶，如果有的话）要么所有条目都处于 evacuated* 状态，要么都不处于该状态
	// （除了在 evacuate() 方法期间，该方法只在 map 写入时发生，因此在此期间没有其他人可以观察到 map）。
	emptyRest = 0 // this cell is empty, and there are no more non-empty cells at higher indexes or overflows.
	// 此单元为空，且在更高索引或溢出桶中没有更多非空单元。
	emptyOne = 1 // this cell is empty
	// 此单元为空
	evacuatedX = 2 // key/elem is valid.  Entry has been evacuated to first half of larger table.
	// 键/值有效。条目已被疏散到更大表的前半部分。
	evacuatedY = 3 // same as above, but evacuated to second half of larger table.
	// 同上，但被疏散到更大表的后半部分。
	evacuatedEmpty = 4 // cell is empty, bucket is evacuated.
	// 单元为空，桶已被疏散。
	minTopHash = 5 // minimum tophash for a normal filled cell.
	// 正常填充单元的最小 tophash 值。

	// flags
	// 标志位
	iterator = 1 // there may be an iterator using buckets
	// 可能有迭代器正在使用桶
	oldIterator = 2 // there may be an iterator using oldbuckets
	// 可能有迭代器正在使用旧桶
	hashWriting = 4 // a goroutine is writing to the map
	// 有一个 goroutine 正在写入 map
	sameSizeGrow = 8 // the current map growth is to a new map of the same size
	// 当前 map 增长是到相同大小的新 map

	// sentinel bucket ID for iterator checks
	// 用于迭代器检查的哨兵桶 ID
	noCheck = 1<<(8*goarch.PtrSize) - 1
)

// isEmpty reports whether the given tophash array entry represents an empty bucket entry.
func isEmpty(x uint8) bool {
	return x <= emptyOne
}

// A header for a Go map.
// Go map 的头部结构。
type hmap struct {
	// Note: the format of the hmap is also encoded in cmd/compile/internal/reflectdata/reflect.go.
	// Make sure this stays in sync with the compiler's definition.
	// 注意：hmap 的格式也被编码在 cmd/compile/internal/reflectdata/reflect.go 中。
	// 确保这里的定义与编译器的定义保持同步。
	count int // # live cells == size of map.  Must be first (used by len() builtin)
	// 活跃单元格数量 == map 的大小。必须放在第一位（被 len() 内建函数使用）
	flags uint8 // 标志位
	B     uint8 // log_2 of # of buckets (can hold up to loadFactor * 2^B items)
	// 桶数量的对数（可以容纳最多 loadFactor * 2^B 个项）
	noverflow uint16 // approximate number of overflow buckets; see incrnoverflow for details
	// 溢出桶的大致数量；详见 incrnoverflow 的说明
	hash0 uint32 // hash seed
	// 哈希种子

	buckets unsafe.Pointer // array of 2^B Buckets. may be nil if count==0.
	// 2^B 个桶的数组。如果 count==0 可能为 nil
	oldbuckets unsafe.Pointer // previous bucket array of half the size, non-nil only when growing
	// 前一个桶数组，大小是现在的一半，仅在扩容时非 nil
	nevacuate uintptr // progress counter for evacuation (buckets less than this have been evacuated)
	// 搬迁进度计数器（小于此数的桶已经被搬迁）

	extra *mapextra // optional fields
	// 可选字段
}

// mapextra holds fields that are not present on all maps.
// mapextra 保存不是所有 map 都具有的字段。
type mapextra struct {
	// If both key and elem do not contain pointers and are inline, then we mark bucket
	// type as containing no pointers. This avoids scanning such maps.
	// However, bmap.overflow is a pointer. In order to keep overflow buckets
	// alive, we store pointers to all overflow buckets in hmap.extra.overflow and hmap.extra.oldoverflow.
	// overflow and oldoverflow are only used if key and elem do not contain pointers.
	// overflow contains overflow buckets for hmap.buckets.
	// oldoverflow contains overflow buckets for hmap.oldbuckets.
	// The indirection allows to store a pointer to the slice in hiter.
	// 如果 key 和 elem 都不包含指针且是内联的，那么我们将桶类型标记为不包含指针。这样可以避免扫描这些 map。
	// 但是，bmap.overflow 是一个指针。为了保持溢出桶存活，我们将所有溢出桶的指针存储在
	// hmap.extra.overflow 和 hmap.extra.oldoverflow 中。
	// overflow 和 oldoverflow 仅在 key 和 elem 不包含指针时使用。
	// overflow 包含 hmap.buckets 的溢出桶。
	// oldoverflow 包含 hmap.oldbuckets 的溢出桶。
	// 这种间接方式允许在 hiter 中存储指向切片的指针。
	overflow    *[]*bmap
	oldoverflow *[]*bmap

	// nextOverflow holds a pointer to a free overflow bucket.
	// nextOverflow 保存指向空闲溢出桶的指针。
	nextOverflow *bmap
}

// A bucket for a Go map.
// Go map 的一个桶。
type bmap struct {
	// tophash generally contains the top byte of the hash value
	// for each key in this bucket. If tophash[0] < minTopHash,
	// tophash[0] is a bucket evacuation state instead.
	// tophash 通常包含此桶中每个 key 的哈希值的高 8 位。
	// 如果 tophash[0] < minTopHash，则 tophash[0] 表示桶的搬迁状态。
	tophash [abi.MapBucketCount]uint8
	// Followed by bucketCnt keys and then bucketCnt elems.
	// NOTE: packing all the keys together and then all the elems together makes the
	// code a bit more complicated than alternating key/elem/key/elem/... but it allows
	// us to eliminate padding which would be needed for, e.g., map[int64]int8.
	// Followed by an overflow pointer.
	// 后面跟着 bucketCnt 个 key，然后是 bucketCnt 个 elem。
	// 注意：将所有 key 打包在一起，然后将所有 elem 打包在一起，
	// 使得代码比交替排列 key/elem/key/elem/... 更复杂，
	// 但这样可以消除需要填充的情况，例如 map[int64]int8。
	// 最后跟着一个溢出指针。
}

// A hash iteration structure.
// If you modify hiter, also change cmd/compile/internal/reflectdata/reflect.go
// and reflect/value.go to match the layout of this structure.
// 哈希迭代结构。
// 如果修改 hiter，还需要修改 cmd/compile/internal/reflectdata/reflect.go
// 和 reflect/value.go 以匹配此结构的布局。
type hiter struct {
	key unsafe.Pointer // Must be in first position.  Write nil to indicate iteration end (see cmd/compile/internal/walk/range.go).
	// 必须在第一个位置。写入 nil 表示迭代结束（见 cmd/compile/internal/walk/range.go）。
	elem unsafe.Pointer // Must be in second position (see cmd/compile/internal/walk/range.go).
	// 必须在第二个位置（见 cmd/compile/internal/walk/range.go）。
	t       *maptype       // map 类型
	h       *hmap          // map 头部指针
	buckets unsafe.Pointer // bucket ptr at hash_iter initialization time
	// 哈希迭代初始化时的桶指针
	bptr *bmap // current bucket
	// 当前桶
	overflow *[]*bmap // keeps overflow buckets of hmap.buckets alive
	// 保持 hmap.buckets 的溢出桶存活
	oldoverflow *[]*bmap // keeps overflow buckets of hmap.oldbuckets alive
	// 保持 hmap.oldbuckets 的溢出桶存活
	startBucket uintptr // bucket iteration started at
	// 桶迭代的起始位置
	offset uint8 // intra-bucket offset to start from during iteration (should be big enough to hold bucketCnt-1)
	// 迭代期间桶内的起始偏移量（应该足够大以容纳 bucketCnt-1）
	wrapped bool // already wrapped around from end of bucket array to beginning
	// 是否已经从桶数组末尾环绕到开始
	B           uint8   // 桶数量的对数
	i           uint8   // 当前桶内的索引
	bucket      uintptr // 当前正在迭代的桶的编号
	checkBucket uintptr // 用于检查的桶编号
}

// bucketShift returns 1<<b, optimized for code generation.
// bucketShift 返回 1<<b，针对代码生成进行了优化。
func bucketShift(b uint8) uintptr {
	// Masking the shift amount allows overflow checks to be elided.
	// 通过掩码位移量可以省略溢出检查。
	return uintptr(1) << (b & (goarch.PtrSize*8 - 1))
}

// bucketMask returns 1<<b - 1, optimized for code generation.
// bucketMask 返回 1<<b - 1，针对代码生成进行了优化。
func bucketMask(b uint8) uintptr {
	return bucketShift(b) - 1
}

// tophash calculates the tophash value for hash.
// tophash 计算哈希值的 tophash 值。
func tophash(hash uintptr) uint8 {
	// 取哈希值的高8位作为 tophash 值
	top := uint8(hash >> (goarch.PtrSize*8 - 8))
	// 如果 tophash 值小于最小值，则加上最小值以避免与特殊标记冲突
	if top < minTopHash {
		top += minTopHash
	}
	return top
}

// evacuated 判断一个桶是否已经被搬迁
func evacuated(b *bmap) bool {
	h := b.tophash[0]
	// 如果桶的第一个 tophash 值在 emptyOne 和 minTopHash 之间，
	// 说明这个桶已经被搬迁
	return h > emptyOne && h < minTopHash
}

// overflow 返回当前桶的溢出桶指针
func (b *bmap) overflow(t *maptype) *bmap {
	// 溢出桶指针存储在桶的最后一个指针位置
	return *(**bmap)(add(unsafe.Pointer(b), uintptr(t.BucketSize)-goarch.PtrSize))
}

// setoverflow 设置当前桶的溢出桶指针
func (b *bmap) setoverflow(t *maptype, ovf *bmap) {
	// 将溢出桶指针存储在桶的最后一个指针位置
	*(**bmap)(add(unsafe.Pointer(b), uintptr(t.BucketSize)-goarch.PtrSize)) = ovf
}

// keys 返回指向桶中键起始位置的指针
func (b *bmap) keys() unsafe.Pointer {
	// dataOffset 是键值对数据区域相对于桶起始位置的偏移量
	return add(unsafe.Pointer(b), dataOffset)
}

// incrnoverflow increments h.noverflow.
// noverflow counts the number of overflow buckets.
// This is used to trigger same-size map growth.
// See also tooManyOverflowBuckets.
// To keep hmap small, noverflow is a uint16.
// When there are few buckets, noverflow is an exact count.
// When there are many buckets, noverflow is an approximate count.
// incrnoverflow 增加 h.noverflow。
// noverflow 计数溢出桶的数量。
// 这用于触发相同大小的 map 增长。
// 另请参阅 tooManyOverflowBuckets。
// 为了保持 hmap 结构体小，noverflow 是一个 uint16。
// 当桶数量较少时，noverflow 是精确计数。
// 当桶数量较多时，noverflow 是近似计数。
func (h *hmap) incrnoverflow() {
	// We trigger same-size map growth if there are
	// as many overflow buckets as buckets.
	// We need to be able to count to 1<<h.B.
	// 如果溢出桶的数量与常规桶的数量一样多，
	// 我们就会触发相同大小的 map 增长。
	// 我们需要能够计数到 1<<h.B。
	if h.B < 16 {
		h.noverflow++
		return
	}
	// Increment with probability 1/(1<<(h.B-15)).
	// When we reach 1<<15 - 1, we will have approximately
	// as many overflow buckets as buckets.
	// 以概率 1/(1<<(h.B-15)) 增加计数。
	// 当我们达到 1<<15 - 1 时，
	// 我们将有大约与常规桶数量相同的溢出桶数量。
	mask := uint32(1)<<(h.B-15) - 1
	// Example: if h.B == 18, then mask == 7,
	// and rand() & 7 == 0 with probability 1/8.
	// 例如：如果 h.B == 18，那么 mask == 7，
	// 且 rand() & 7 == 0 的概率为 1/8。
	if uint32(rand())&mask == 0 {
		h.noverflow++
	}
}
func (h *hmap) newoverflow(t *maptype, b *bmap) *bmap {
	var ovf *bmap
	if h.extra != nil && h.extra.nextOverflow != nil {
		// We have preallocated overflow buckets available.
		// See makeBucketArray for more details.
		// 我们有预分配的溢出桶可用。
		// 详见 makeBucketArray。
		ovf = h.extra.nextOverflow
		if ovf.overflow(t) == nil {
			// We're not at the end of the preallocated overflow buckets. Bump the pointer.
			// 我们还没有到达预分配溢出桶的末尾。移动指针。
			h.extra.nextOverflow = (*bmap)(add(unsafe.Pointer(ovf), uintptr(t.BucketSize)))
		} else {
			// This is the last preallocated overflow bucket.
			// Reset the overflow pointer on this bucket,
			// which was set to a non-nil sentinel value.
			// 这是最后一个预分配的溢出桶。
			// 重置这个桶的溢出指针，
			// 该指针之前被设置为非空的哨兵值。
			ovf.setoverflow(t, nil)
			h.extra.nextOverflow = nil
		}
	} else {
		// 如果没有预分配的溢出桶，则分配一个新的桶
		ovf = (*bmap)(newobject(t.Bucket))
	}
	// 增加溢出桶计数
	h.incrnoverflow()
	if !t.Bucket.Pointers() {
		// 如果桶的类型不包含指针，需要记录溢出桶
		h.createOverflow()
		*h.extra.overflow = append(*h.extra.overflow, ovf)
	}
	// 设置当前桶的溢出桶指针
	b.setoverflow(t, ovf)
	return ovf
}

func (h *hmap) createOverflow() {
	if h.extra == nil {
		h.extra = new(mapextra)
	}
	if h.extra.overflow == nil {
		h.extra.overflow = new([]*bmap)
	}
}

func makemap64(t *maptype, hint int64, h *hmap) *hmap {
	if int64(int(hint)) != hint {
		hint = 0
	}
	return makemap(t, int(hint), h)
}

// makemap_small implements Go map creation for make(map[k]v) and
// make(map[k]v, hint) when hint is known to be at most bucketCnt
// at compile time and the map needs to be allocated on the heap.
//
// makemap_small should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/bytedance/sonic
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
// makemap_small 实现了 Go 的 map 创建功能，用于处理 make(map[k]v) 和
// make(map[k]v, hint) 的情况，其中 hint 在编译时已知不超过 bucketCnt，
// 且 map 需要在堆上分配。
//
// makemap_small 应该是一个内部实现细节，
// 但是很多广泛使用的包通过 linkname 访问它。
// 值得注意的"耻辱堂"成员包括：
//   - github.com/bytedance/sonic
//
// 不要删除或更改函数签名。
// 参见 go.dev/issue/67401。
//
//go:linkname makemap_small
func makemap_small() *hmap {
	// 创建一个新的 hmap 结构体
	h := new(hmap)
	// 为 map 生成一个随机的 hash 种子
	h.hash0 = uint32(rand())
	return h
}

// makemap implements Go map creation for make(map[k]v, hint).
// If the compiler has determined that the map or the first bucket
// can be created on the stack, h and/or bucket may be non-nil.
// If h != nil, the map can be created directly in h.
// If h.buckets != nil, bucket pointed to can be used as the first bucket.
//
// makemap should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/cloudwego/frugal
//   - github.com/ugorji/go/codec
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
// makemap 实现了 Go 的 map 创建功能，用于处理 make(map[k]v, hint)。
// 如果编译器确定 map 或第一个桶可以在栈上创建，则 h 和/或 bucket 可能非空。
// 如果 h != nil，则可以直接在 h 中创建 map。
// 如果 h.buckets != nil，则指向的 bucket 可以用作第一个桶。
//
// makemap 应该是一个内部实现细节，
// 但是很多广泛使用的包通过 linkname 访问它。
// 值得注意的"耻辱堂"成员包括：
//   - github.com/cloudwego/frugal
//   - github.com/ugorji/go/codec
//
// 不要删除或更改函数签名。
// 参见 go.dev/issue/67401。
//
//go:linkname makemap
func makemap(t *maptype, hint int, h *hmap) *hmap {
	// 计算所需的内存大小，检查是否溢出
	mem, overflow := math.MulUintptr(uintptr(hint), t.Bucket.Size_)
	if overflow || mem > maxAlloc {
		hint = 0
	}

	// 初始化 hmap
	if h == nil {
		h = new(hmap)
	}
	// 为 map 生成一个随机的 hash 种子
	h.hash0 = uint32(rand())

	// 找到能容纳请求元素数量的大小参数 B
	// 对于 hint < 0，由于 hint < bucketCnt，overLoadFactor 返回 false
	B := uint8(0)
	for overLoadFactor(hint, B) {
		B++
	}
	h.B = B

	// 分配初始哈希表
	// 如果 B == 0，buckets 字段稍后会在 mapassign 中懒加载
	// 如果 hint 较大，对这块内存进行清零可能需要一段时间
	if h.B != 0 {
		var nextOverflow *bmap
		h.buckets, nextOverflow = makeBucketArray(t, h.B, nil)
		if nextOverflow != nil {
			h.extra = new(mapextra)
			h.extra.nextOverflow = nextOverflow
		}
	}

	return h
}

// makeBucketArray initializes a backing array for map buckets.
// 1<<b is the minimum number of buckets to allocate.
// dirtyalloc should either be nil or a bucket array previously
// allocated by makeBucketArray with the same t and b parameters.
// If dirtyalloc is nil a new backing array will be alloced and
// otherwise dirtyalloc will be cleared and reused as backing array.
//
// makeBucketArray 初始化一个用于 map 桶的底层数组。
// 1<<b 是要分配的最小桶数。
// dirtyalloc 要么是 nil，要么是之前由 makeBucketArray 使用相同的 t 和 b 参数分配的桶数组。
// 如果 dirtyalloc 是 nil，则会分配一个新的底层数组，
// 否则会清空 dirtyalloc 并重用它作为底层数组。
func makeBucketArray(t *maptype, b uint8, dirtyalloc unsafe.Pointer) (buckets unsafe.Pointer, nextOverflow *bmap) {
	base := bucketShift(b)
	nbuckets := base
	// For small b, overflow buckets are unlikely.
	// Avoid the overhead of the calculation.
	// 对于较小的 b，溢出桶不太可能出现。
	// 避免计算开销。
	if b >= 4 {
		// Add on the estimated number of overflow buckets
		// required to insert the median number of elements
		// used with this value of b.
		// 添加预估的溢出桶数量，
		// 这是在使用此 b 值时插入中位数个元素所需的数量。
		nbuckets += bucketShift(b - 4)
		sz := t.Bucket.Size_ * nbuckets
		up := roundupsize(sz, !t.Bucket.Pointers())
		if up != sz {
			nbuckets = up / t.Bucket.Size_
		}
	}

	if dirtyalloc == nil {
		buckets = newarray(t.Bucket, int(nbuckets))
	} else {
		// dirtyalloc was previously generated by
		// the above newarray(t.Bucket, int(nbuckets))
		// but may not be empty.
		// dirtyalloc 之前是由上面的 newarray(t.Bucket, int(nbuckets)) 生成的，
		// 但可能不是空的。
		buckets = dirtyalloc
		size := t.Bucket.Size_ * nbuckets
		if t.Bucket.Pointers() {
			memclrHasPointers(buckets, size)
		} else {
			memclrNoHeapPointers(buckets, size)
		}
	}

	if base != nbuckets {
		// We preallocated some overflow buckets.
		// To keep the overhead of tracking these overflow buckets to a minimum,
		// we use the convention that if a preallocated overflow bucket's overflow
		// pointer is nil, then there are more available by bumping the pointer.
		// We need a safe non-nil pointer for the last overflow bucket; just use buckets.
		// 我们预分配了一些溢出桶。
		// 为了将跟踪这些溢出桶的开销降到最低，
		// 我们使用这样的约定：如果预分配的溢出桶的溢出指针为 nil，
		// 则可以通过递增指针获得更多可用的桶。
		// 我们需要一个安全的非 nil 指针作为最后一个溢出桶；直接使用 buckets。
		nextOverflow = (*bmap)(add(buckets, base*uintptr(t.BucketSize)))
		last := (*bmap)(add(buckets, (nbuckets-1)*uintptr(t.BucketSize)))
		last.setoverflow(t, (*bmap)(buckets))
	}
	return buckets, nextOverflow
}

// mapaccess1 returns a pointer to h[key].  Never returns nil, instead
// it will return a reference to the zero object for the elem type if
// the key is not in the map.
// NOTE: The returned pointer may keep the whole map live, so don't
// hold onto it for very long.
// mapaccess1 返回指向 h[key] 的指针。永远不会返回 nil，如果 key 不在 map 中，
// 会返回一个指向该元素类型零值对象的引用。
// 注意：返回的指针可能会导致整个 map 保持存活，所以不要长时间持有它。
func mapaccess1(t *maptype, h *hmap, key unsafe.Pointer) unsafe.Pointer {
	if raceenabled && h != nil {
		callerpc := getcallerpc()
		pc := abi.FuncPCABIInternal(mapaccess1)
		racereadpc(unsafe.Pointer(h), callerpc, pc)
		raceReadObjectPC(t.Key, key, callerpc, pc)
	}
	if msanenabled && h != nil {
		msanread(key, t.Key.Size_)
	}
	if asanenabled && h != nil {
		asanread(key, t.Key.Size_)
	}
	// 如果 map 为 nil 或者为空
	if h == nil || h.count == 0 {
		if err := mapKeyError(t, key); err != nil {
			panic(err) // see issue 23734
		}
		return unsafe.Pointer(&zeroVal[0])
	}
	// 检查是否存在并发写入
	if h.flags&hashWriting != 0 {
		fatal("concurrent map read and map write")
	}
	// 计算 key 的哈希值
	hash := t.Hasher(key, uintptr(h.hash0))
	// 计算桶掩码
	m := bucketMask(h.B)
	// 找到 key 对应的桶
	b := (*bmap)(add(h.buckets, (hash&m)*uintptr(t.BucketSize)))
	// 如果正在扩容
	if c := h.oldbuckets; c != nil {
		if !h.sameSizeGrow() {
			// 如果是增量扩容（容量翻倍），需要将掩码右移一位
			m >>= 1
		}
		// 找到旧桶
		oldb := (*bmap)(add(c, (hash&m)*uintptr(t.BucketSize)))
		if !evacuated(oldb) {
			// 如果旧桶未迁移完成，则在旧桶中查找
			b = oldb
		}
	}
	// 获取 hash 的高 8 位
	top := tophash(hash)
bucketloop:
	// 遍历桶及其溢出桶
	for ; b != nil; b = b.overflow(t) {
		// 遍历桶中的每个单元
		for i := uintptr(0); i < abi.MapBucketCount; i++ {
			// 如果 tophash 不匹配
			if b.tophash[i] != top {
				if b.tophash[i] == emptyRest {
					// 如果遇到空单元，说明后面都是空的，可以结束查找
					break bucketloop
				}
				continue
			}
			// 获取 key 的地址
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.KeySize))
			if t.IndirectKey() {
				k = *((*unsafe.Pointer)(k))
			}
			// 比较 key 是否相等
			if t.Key.Equal(key, k) {
				// 找到匹配的 key，获取对应的 value
				e := add(unsafe.Pointer(b), dataOffset+abi.MapBucketCount*uintptr(t.KeySize)+i*uintptr(t.ValueSize))
				if t.IndirectElem() {
					e = *((*unsafe.Pointer)(e))
				}
				return e
			}
		}
	}
	// 未找到 key，返回零值
	return unsafe.Pointer(&zeroVal[0])
}

// mapaccess2 should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/ugorji/go/codec
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname mapaccess2
func mapaccess2(t *maptype, h *hmap, key unsafe.Pointer) (unsafe.Pointer, bool) {
	// 如果启用了竞态检测
	if raceenabled && h != nil {
		callerpc := getcallerpc()
		pc := abi.FuncPCABIInternal(mapaccess2)
		racereadpc(unsafe.Pointer(h), callerpc, pc)
		raceReadObjectPC(t.Key, key, callerpc, pc)
	}
	// 如果启用了内存消毒器
	if msanenabled && h != nil {
		msanread(key, t.Key.Size_)
	}
	// 如果启用了地址消毒器
	if asanenabled && h != nil {
		asanread(key, t.Key.Size_)
	}
	// 如果map为空或元素个数为0，返回零值和false
	if h == nil || h.count == 0 {
		if err := mapKeyError(t, key); err != nil {
			panic(err) // see issue 23734
		}
		return unsafe.Pointer(&zeroVal[0]), false
	}
	// 检查是否有并发写入
	if h.flags&hashWriting != 0 {
		fatal("concurrent map read and map write")
	}
	// 计算key的哈希值
	hash := t.Hasher(key, uintptr(h.hash0))
	// 计算桶掩码
	m := bucketMask(h.B)
	// 找到key对应的桶
	b := (*bmap)(add(h.buckets, (hash&m)*uintptr(t.BucketSize)))
	// 如果正在扩容
	if c := h.oldbuckets; c != nil {
		if !h.sameSizeGrow() {
			// 如果是增量扩容（容量翻倍），需要将掩码右移一位
			m >>= 1
		}
		// 找到旧桶
		oldb := (*bmap)(add(c, (hash&m)*uintptr(t.BucketSize)))
		if !evacuated(oldb) {
			// 如果旧桶未迁移完成，则在旧桶中查找
			b = oldb
		}
	}
	// 获取hash的高8位
	top := tophash(hash)
bucketloop:
	// 遍历桶及其溢出桶
	for ; b != nil; b = b.overflow(t) {
		// 遍历桶中的每个单元
		for i := uintptr(0); i < abi.MapBucketCount; i++ {
			// 如果tophash不匹配
			if b.tophash[i] != top {
				if b.tophash[i] == emptyRest {
					// 如果遇到空单元，说明后面都是空的，可以结束查找
					break bucketloop
				}
				continue
			}
			// 获取key的地址
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.KeySize))
			if t.IndirectKey() {
				k = *((*unsafe.Pointer)(k))
			}
			// 比较key是否相等
			if t.Key.Equal(key, k) {
				// 找到匹配的key，获取对应的value
				e := add(unsafe.Pointer(b), dataOffset+abi.MapBucketCount*uintptr(t.KeySize)+i*uintptr(t.ValueSize))
				if t.IndirectElem() {
					e = *((*unsafe.Pointer)(e))
				}
				return e, true
			}
		}
	}
	// 未找到key，返回零值和false
	return unsafe.Pointer(&zeroVal[0]), false
}

// returns both key and elem. Used by map iterator.
// 返回键和值。被map迭代器使用。
func mapaccessK(t *maptype, h *hmap, key unsafe.Pointer) (unsafe.Pointer, unsafe.Pointer) {
	// 如果map为空或者元素个数为0，直接返回nil
	if h == nil || h.count == 0 {
		return nil, nil
	}
	// 计算key的哈希值
	hash := t.Hasher(key, uintptr(h.hash0))
	// 计算桶掩码
	m := bucketMask(h.B)
	// 找到key对应的桶
	b := (*bmap)(add(h.buckets, (hash&m)*uintptr(t.BucketSize)))
	// 如果正在扩容
	if c := h.oldbuckets; c != nil {
		if !h.sameSizeGrow() {
			// There used to be half as many buckets; mask down one more power of two.
			// 之前的桶数量是现在的一半，将掩码右移一位
			m >>= 1
		}
		// 找到旧桶
		oldb := (*bmap)(add(c, (hash&m)*uintptr(t.BucketSize)))
		// 如果旧桶未迁移完成，则在旧桶中查找
		if !evacuated(oldb) {
			b = oldb
		}
	}
	// 获取hash的高8位
	top := tophash(hash)
bucketloop:
	// 遍历桶及其溢出桶
	for ; b != nil; b = b.overflow(t) {
		// 遍历桶中的每个单元
		for i := uintptr(0); i < abi.MapBucketCount; i++ {
			// 如果tophash不匹配
			if b.tophash[i] != top {
				// 如果遇到空单元，说明后面都是空的，可以结束查找
				if b.tophash[i] == emptyRest {
					break bucketloop
				}
				continue
			}
			// 获取key的地址
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.KeySize))
			if t.IndirectKey() {
				k = *((*unsafe.Pointer)(k))
			}
			// 比较key是否相等
			if t.Key.Equal(key, k) {
				// 找到匹配的key，获取对应的value
				e := add(unsafe.Pointer(b), dataOffset+abi.MapBucketCount*uintptr(t.KeySize)+i*uintptr(t.ValueSize))
				if t.IndirectElem() {
					e = *((*unsafe.Pointer)(e))
				}
				// 返回key和value的指针
				return k, e
			}
		}
	}
	// 未找到key，返回nil
	return nil, nil
}

func mapaccess1_fat(t *maptype, h *hmap, key, zero unsafe.Pointer) unsafe.Pointer {
	e := mapaccess1(t, h, key)
	if e == unsafe.Pointer(&zeroVal[0]) {
		return zero
	}
	return e
}

func mapaccess2_fat(t *maptype, h *hmap, key, zero unsafe.Pointer) (unsafe.Pointer, bool) {
	e := mapaccess1(t, h, key)
	if e == unsafe.Pointer(&zeroVal[0]) {
		return zero, false
	}
	return e, true
}

// Like mapaccess, but allocates a slot for the key if it is not present in the map.
// 类似于 mapaccess，但如果 key 不在 map 中，会为其分配一个槽位。
//
// mapassign should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/bytedance/sonic
//   - github.com/cloudwego/frugal
//   - github.com/RomiChan/protobuf
//   - github.com/segmentio/encoding
//   - github.com/ugorji/go/codec
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname mapassign
func mapassign(t *maptype, h *hmap, key unsafe.Pointer) unsafe.Pointer {
	// 检查 map 是否为 nil
	if h == nil {
		panic(plainError("assignment to entry in nil map"))
	}
	// 竞态检测相关代码
	if raceenabled {
		callerpc := getcallerpc()
		pc := abi.FuncPCABIInternal(mapassign)
		racewritepc(unsafe.Pointer(h), callerpc, pc)
		raceReadObjectPC(t.Key, key, callerpc, pc)
	}
	// 内存检查相关代码
	if msanenabled {
		msanread(key, t.Key.Size_)
	}
	if asanenabled {
		asanread(key, t.Key.Size_)
	}
	// 检查是否存在并发写入
	if h.flags&hashWriting != 0 {
		fatal("concurrent map writes")
	}
	// 计算 key 的哈希值
	hash := t.Hasher(key, uintptr(h.hash0))

	// 在调用 t.hasher 之后设置 hashWriting 标志
	// 因为 t.hasher 可能会 panic，这种情况下实际上并没有进行写操作
	h.flags ^= hashWriting

	// 如果 map 还没有分配 bucket，则分配第一个 bucket
	if h.buckets == nil {
		h.buckets = newobject(t.Bucket) // newarray(t.Bucket, 1)
	}

again:
	// 计算 key 应该在哪个 bucket
	bucket := hash & bucketMask(h.B)
	// 如果 map 正在扩容，协助完成扩容工作
	if h.growing() {
		growWork(t, h, bucket)
	}
	// 获取 bucket 地址
	b := (*bmap)(add(h.buckets, bucket*uintptr(t.BucketSize)))
	top := tophash(hash)

	// 用于记录可以插入的位置
	var inserti *uint8         // 指向 tophash 的指针
	var insertk unsafe.Pointer // key 的插入位置
	var elem unsafe.Pointer    // value 的插入位置
bucketloop:
	for {
		// 遍历 bucket 中的每个 cell
		for i := uintptr(0); i < abi.MapBucketCount; i++ {
			if b.tophash[i] != top {
				// 如果当前位置为空且还没找到插入位置，记录这个位置
				if isEmpty(b.tophash[i]) && inserti == nil {
					inserti = &b.tophash[i]
					insertk = add(unsafe.Pointer(b), dataOffset+i*uintptr(t.KeySize))
					elem = add(unsafe.Pointer(b), dataOffset+abi.MapBucketCount*uintptr(t.KeySize)+i*uintptr(t.ValueSize))
				}
				// 如果遇到 emptyRest，说明后面都是空的，结束查找
				if b.tophash[i] == emptyRest {
					break bucketloop
				}
				continue
			}
			// tophash 匹配，获取 key 的地址进行进一步比较
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.KeySize))
			if t.IndirectKey() {
				k = *((*unsafe.Pointer)(k))
			}
			// 如果 key 不相等，继续查找
			if !t.Key.Equal(key, k) {
				continue
			}
			// 找到已存在的 key，更新它
			if t.NeedKeyUpdate() {
				typedmemmove(t.Key, k, key)
			}
			// 获取对应 value 的地址
			elem = add(unsafe.Pointer(b), dataOffset+abi.MapBucketCount*uintptr(t.KeySize)+i*uintptr(t.ValueSize))
			goto done
		}
		// 获取溢出桶
		ovf := b.overflow(t)
		if ovf == nil {
			break
		}
		b = ovf
	}

	// 没有找到 key 的映射，需要分配新的 cell 并添加条目

	// 如果达到了最大负载因子或者有太多的溢出桶
	// 且目前没有在扩容过程中，开始扩容
	if !h.growing() && (overLoadFactor(h.count+1, h.B) || tooManyOverflowBuckets(h.noverflow, h.B)) {
		hashGrow(t, h)
		goto again // 扩容会使之前的操作无效，需要重试
	}

	// 如果没有找到可插入的位置，说明当前 bucket 和它的所有溢出桶都满了
	// 需要分配一个新的溢出桶
	if inserti == nil {
		newb := h.newoverflow(t, b)
		inserti = &newb.tophash[0]
		insertk = add(unsafe.Pointer(newb), dataOffset)
		elem = add(insertk, abi.MapBucketCount*uintptr(t.KeySize))
	}

	// 在插入位置存储新的 key/value
	if t.IndirectKey() {
		kmem := newobject(t.Key)
		*(*unsafe.Pointer)(insertk) = kmem
		insertk = kmem
	}
	if t.IndirectElem() {
		vmem := newobject(t.Elem)
		*(*unsafe.Pointer)(elem) = vmem
	}
	// 复制 key 到插入位置
	typedmemmove(t.Key, insertk, key)
	*inserti = top
	// 增加计数
	h.count++

done:
	// 检查并清除写标志
	if h.flags&hashWriting == 0 {
		fatal("concurrent map writes")
	}
	h.flags &^= hashWriting
	// 如果 value 是间接存储的，获取真实地址
	if t.IndirectElem() {
		elem = *((*unsafe.Pointer)(elem))
	}
	return elem
}

// mapdelete should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/ugorji/go/codec
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname mapdelete
func mapdelete(t *maptype, h *hmap, key unsafe.Pointer) {
	if raceenabled && h != nil {
		callerpc := getcallerpc()
		pc := abi.FuncPCABIInternal(mapdelete)
		racewritepc(unsafe.Pointer(h), callerpc, pc)
		raceReadObjectPC(t.Key, key, callerpc, pc)
	}
	if msanenabled && h != nil {
		msanread(key, t.Key.Size_)
	}
	if asanenabled && h != nil {
		asanread(key, t.Key.Size_)
	}
	// Return if h is nil or empty
	// 如果 map 为 nil 或为空则返回
	if h == nil || h.count == 0 {
		if err := mapKeyError(t, key); err != nil {
			panic(err) // see issue 23734
		}
		return
	}
	// Check for concurrent modifications
	// 检查是否有并发修改
	if h.flags&hashWriting != 0 {
		fatal("concurrent map writes")
	}

	// Calculate hash for key
	// 计算 key 的哈希值
	hash := t.Hasher(key, uintptr(h.hash0))

	// Set hashWriting after calling t.hasher, since t.hasher may panic,
	// in which case we have not actually done a write (delete).
	// 在调用 t.hasher 之后设置 hashWriting 标志，因为 t.hasher 可能会 panic，
	// 这种情况下我们实际上并没有进行写操作(删除)
	h.flags ^= hashWriting

	// Find the bucket for this key
	// 找到该 key 对应的桶
	bucket := hash & bucketMask(h.B)
	if h.growing() {
		growWork(t, h, bucket)
	}
	b := (*bmap)(add(h.buckets, bucket*uintptr(t.BucketSize)))
	bOrig := b
	top := tophash(hash)
search:
	// Search for the key in the bucket chain
	// 在桶链中搜索 key
	for ; b != nil; b = b.overflow(t) {
		for i := uintptr(0); i < abi.MapBucketCount; i++ {
			if b.tophash[i] != top {
				if b.tophash[i] == emptyRest {
					break search
				}
				continue
			}
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.KeySize))
			k2 := k
			if t.IndirectKey() {
				k2 = *((*unsafe.Pointer)(k2))
			}
			if !t.Key.Equal(key, k2) {
				continue
			}
			// Only clear key if there are pointers in it.
			// 只有当 key 中包含指针时才清除
			if t.IndirectKey() {
				*(*unsafe.Pointer)(k) = nil
			} else if t.Key.Pointers() {
				memclrHasPointers(k, t.Key.Size_)
			}
			// Clear the value
			// 清除 value
			e := add(unsafe.Pointer(b), dataOffset+abi.MapBucketCount*uintptr(t.KeySize)+i*uintptr(t.ValueSize))
			if t.IndirectElem() {
				*(*unsafe.Pointer)(e) = nil
			} else if t.Elem.Pointers() {
				memclrHasPointers(e, t.Elem.Size_)
			} else {
				memclrNoHeapPointers(e, t.Elem.Size_)
			}
			// Mark slot as empty
			// 标记槽位为空
			b.tophash[i] = emptyOne
			// If the bucket now ends in a bunch of emptyOne states,
			// change those to emptyRest states.
			// It would be nice to make this a separate function, but
			// for loops are not currently inlineable.
			// 如果桶现在以一串 emptyOne 状态结束，
			// 将这些状态改为 emptyRest 状态。
			// 最好将此功能做成单独的函数，但目前 for 循环无法内联。
			if i == abi.MapBucketCount-1 {
				if b.overflow(t) != nil && b.overflow(t).tophash[0] != emptyRest {
					goto notLast
				}
			} else {
				if b.tophash[i+1] != emptyRest {
					goto notLast
				}
			}
			for {
				b.tophash[i] = emptyRest
				if i == 0 {
					if b == bOrig {
						break // beginning of initial bucket, we're done.
					}
					// Find previous bucket, continue at its last entry.
					// 找到前一个桶，从它的最后一个条目继续
					c := b
					for b = bOrig; b.overflow(t) != c; b = b.overflow(t) {
					}
					i = abi.MapBucketCount - 1
				} else {
					i--
				}
				if b.tophash[i] != emptyOne {
					break
				}
			}
		notLast:
			h.count--
			// Reset the hash seed to make it more difficult for attackers to
			// repeatedly trigger hash collisions. See issue 25237.
			// 重置哈希种子，使攻击者更难重复触发哈希冲突。参见 issue 25237。
			if h.count == 0 {
				h.hash0 = uint32(rand())
			}
			break search
		}
	}

	// Check for concurrent modifications again
	// 再次检查是否有并发修改
	if h.flags&hashWriting == 0 {
		fatal("concurrent map writes")
	}
	h.flags &^= hashWriting
}

// mapiterinit initializes the hiter struct used for ranging over maps.
// The hiter struct pointed to by 'it' is allocated on the stack
// by the compilers order pass or on the heap by reflect_mapiterinit.
// Both need to have zeroed hiter since the struct contains pointers.
// mapiterinit 初始化用于遍历 map 的 hiter 结构体。
// 由 'it' 指向的 hiter 结构体由编译器的 order pass 分配在栈上，
// 或由 reflect_mapiterinit 分配在堆上。
// 由于结构体包含指针，两种情况都需要将 hiter 清零。

// mapiterinit should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/bytedance/sonic
//   - github.com/cloudwego/frugal
//   - github.com/goccy/go-json
//   - github.com/RomiChan/protobuf
//   - github.com/segmentio/encoding
//   - github.com/ugorji/go/codec
//   - github.com/wI2L/jettison
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname mapiterinit
func mapiterinit(t *maptype, h *hmap, it *hiter) {
	if raceenabled && h != nil {
		callerpc := getcallerpc()
		racereadpc(unsafe.Pointer(h), callerpc, abi.FuncPCABIInternal(mapiterinit))
	}

	it.t = t
	if h == nil || h.count == 0 {
		return
	}

	if unsafe.Sizeof(hiter{})/goarch.PtrSize != 12 {
		throw("hash_iter size incorrect") // see cmd/compile/internal/reflectdata/reflect.go
	}
	it.h = h

	// grab snapshot of bucket state
	// 获取桶状态的快照
	it.B = h.B
	it.buckets = h.buckets
	if !t.Bucket.Pointers() {
		// Allocate the current slice and remember pointers to both current and old.
		// This preserves all relevant overflow buckets alive even if
		// the table grows and/or overflow buckets are added to the table
		// while we are iterating.
		// 分配当前切片并记住指向当前和旧桶的指针。
		// 即使在迭代过程中表格增长和/或添加了溢出桶，
		// 这也能保持所有相关的溢出桶存活。
		h.createOverflow()
		it.overflow = h.extra.overflow
		it.oldoverflow = h.extra.oldoverflow
	}

	// decide where to start
	// 决定从哪里开始迭代
	r := uintptr(rand())
	it.startBucket = r & bucketMask(h.B)
	it.offset = uint8(r >> h.B & (abi.MapBucketCount - 1))

	// iterator state
	// 迭代器状态
	it.bucket = it.startBucket

	// Remember we have an iterator.
	// Can run concurrently with another mapiterinit().
	// 记住我们有一个迭代器。
	// 可以与另一个 mapiterinit() 并发运行。
	if old := h.flags; old&(iterator|oldIterator) != iterator|oldIterator {
		atomic.Or8(&h.flags, iterator|oldIterator)
	}

	mapiternext(it)
}

// mapiternext should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/bytedance/sonic
//   - github.com/cloudwego/frugal
//   - github.com/RomiChan/protobuf
//   - github.com/segmentio/encoding
//   - github.com/ugorji/go/codec
//   - gonum.org/v1/gonum
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname mapiternext
func mapiternext(it *hiter) {
	h := it.h
	if raceenabled {
		callerpc := getcallerpc()
		racereadpc(unsafe.Pointer(h), callerpc, abi.FuncPCABIInternal(mapiternext))
	}
	if h.flags&hashWriting != 0 {
		fatal("concurrent map iteration and map write")
	}
	t := it.t
	bucket := it.bucket
	b := it.bptr
	i := it.i
	checkBucket := it.checkBucket

next:
	if b == nil {
		if bucket == it.startBucket && it.wrapped {
			// end of iteration
			// 迭代结束
			it.key = nil
			it.elem = nil
			return
		}
		if h.growing() && it.B == h.B {
			// Iterator was started in the middle of a grow, and the grow isn't done yet.
			// If the bucket we're looking at hasn't been filled in yet (i.e. the old
			// bucket hasn't been evacuated) then we need to iterate through the old
			// bucket and only return the ones that will be migrated to this bucket.
			// 迭代器在扩容过程中启动，且扩容还未完成。
			// 如果我们正在查看的桶还未填充（即旧桶还未迁移），
			// 那么我们需要遍历旧桶，并且只返回那些将要迁移到这个桶的元素。
			oldbucket := bucket & it.h.oldbucketmask()
			b = (*bmap)(add(h.oldbuckets, oldbucket*uintptr(t.BucketSize)))
			if !evacuated(b) {
				checkBucket = bucket
			} else {
				b = (*bmap)(add(it.buckets, bucket*uintptr(t.BucketSize)))
				checkBucket = noCheck
			}
		} else {
			b = (*bmap)(add(it.buckets, bucket*uintptr(t.BucketSize)))
			checkBucket = noCheck
		}
		bucket++
		if bucket == bucketShift(it.B) {
			bucket = 0
			it.wrapped = true
		}
		i = 0
	}
	for ; i < abi.MapBucketCount; i++ {
		offi := (i + it.offset) & (abi.MapBucketCount - 1)
		if isEmpty(b.tophash[offi]) || b.tophash[offi] == evacuatedEmpty {
			// TODO: emptyRest is hard to use here, as we start iterating
			// in the middle of a bucket. It's feasible, just tricky.
			// TODO: 这里很难使用 emptyRest，因为我们从桶的中间开始迭代。
			// 这是可行的，但比较棘手。
			continue
		}
		k := add(unsafe.Pointer(b), dataOffset+uintptr(offi)*uintptr(t.KeySize))
		if t.IndirectKey() {
			k = *((*unsafe.Pointer)(k))
		}
		e := add(unsafe.Pointer(b), dataOffset+abi.MapBucketCount*uintptr(t.KeySize)+uintptr(offi)*uintptr(t.ValueSize))
		if checkBucket != noCheck && !h.sameSizeGrow() {
			// Special case: iterator was started during a grow to a larger size
			// and the grow is not done yet. We're working on a bucket whose
			// oldbucket has not been evacuated yet. Or at least, it wasn't
			// evacuated when we started the bucket. So we're iterating
			// through the oldbucket, skipping any keys that will go
			// to the other new bucket (each oldbucket expands to two
			// buckets during a grow).
			// 特殊情况：迭代器在扩容到更大尺寸的过程中启动，且扩容还未完成。
			// 我们正在处理的桶的旧桶还未迁移。或者至少在我们开始处理这个桶时还未迁移。
			// 所以我们正在遍历旧桶，跳过任何将要去往其他新桶的键
			// （在扩容过程中，每个旧桶会扩展为两个桶）。
			if t.ReflexiveKey() || t.Key.Equal(k, k) {
				// If the item in the oldbucket is not destined for
				// the current new bucket in the iteration, skip it.
				// 如果旧桶中的项不是要迁移到当前迭代中的新桶，则跳过它。
				hash := t.Hasher(k, uintptr(h.hash0))
				if hash&bucketMask(it.B) != checkBucket {
					continue
				}
			} else {
				// Hash isn't repeatable if k != k (NaNs).  We need a
				// repeatable and randomish choice of which direction
				// to send NaNs during evacuation. We'll use the low
				// bit of tophash to decide which way NaNs go.
				// NOTE: this case is why we need two evacuate tophash
				// values, evacuatedX and evacuatedY, that differ in
				// their low bit.
				// 如果 k != k（NaN值），哈希值是不可重复的。
				// 我们需要一个可重复且随机的选择来决定在迁移期间将 NaN 发送到哪个方向。
				// 我们将使用 tophash 的低位来决定 NaN 的去向。
				// 注意：这种情况就是为什么我们需要两个迁移 tophash 值（evacuatedX 和 evacuatedY），
				// 它们在低位上不同。
				if checkBucket>>(it.B-1) != uintptr(b.tophash[offi]&1) {
					continue
				}
			}
		}
		if (b.tophash[offi] != evacuatedX && b.tophash[offi] != evacuatedY) ||
			!(t.ReflexiveKey() || t.Key.Equal(k, k)) {
			// This is the golden data, we can return it.
			// OR
			// key!=key, so the entry can't be deleted or updated, so we can just return it.
			// That's lucky for us because when key!=key we can't look it up successfully.
			// 这是黄金数据，我们可以返回它。
			// 或者
			// key!=key，所以这个条目不能被删除或更新，所以我们可以直接返回它。
			// 这对我们来说是幸运的，因为当 key!=key 时我们无法成功查找它。
			it.key = k
			if t.IndirectElem() {
				e = *((*unsafe.Pointer)(e))
			}
			it.elem = e
		} else {
			// The hash table has grown since the iterator was started.
			// The golden data for this key is now somewhere else.
			// Check the current hash table for the data.
			// This code handles the case where the key
			// has been deleted, updated, or deleted and reinserted.
			// NOTE: we need to regrab the key as it has potentially been
			// updated to an equal() but not identical key (e.g. +0.0 vs -0.0).
			// 自迭代器启动以来，哈希表已经增长。
			// 这个键的黄金数据现在在其他地方。
			// 在当前哈希表中检查数据。
			// 这段代码处理键被删除、更新或删除后重新插入的情况。
			// 注意：我们需要重新获取键，因为它可能已经被更新为相等但不完全相同的键
			// （例如 +0.0 vs -0.0）。
			rk, re := mapaccessK(t, h, k)
			if rk == nil {
				continue // key has been deleted
				// 键已被删除
			}
			it.key = rk
			it.elem = re
		}
		it.bucket = bucket
		if it.bptr != b { // avoid unnecessary write barrier; see issue 14921
			// 避免不必要的写屏障；参见 issue 14921
			it.bptr = b
		}
		it.i = i + 1
		it.checkBucket = checkBucket
		return
	}
	b = b.overflow(t)
	i = 0
	goto next
}

// mapclear deletes all keys from a map.
// It is called by the compiler.
//
// mapclear should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/cloudwego/frugal
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
// mapclear 删除 map 中的所有键。
// 它由编译器调用。
//
//go:linkname mapclear
func mapclear(t *maptype, h *hmap) {
	if raceenabled && h != nil {
		callerpc := getcallerpc()
		pc := abi.FuncPCABIInternal(mapclear)
		racewritepc(unsafe.Pointer(h), callerpc, pc)
	}

	// 如果 map 为 nil 或为空，直接返回
	if h == nil || h.count == 0 {
		return
	}

	// 检查是否有并发写入
	if h.flags&hashWriting != 0 {
		fatal("concurrent map writes")
	}

	// 设置写入标志
	h.flags ^= hashWriting

	// Mark buckets empty, so existing iterators can be terminated, see issue #59411.
	// 将桶标记为空，以便终止现有的迭代器，参见 issue #59411
	markBucketsEmpty := func(bucket unsafe.Pointer, mask uintptr) {
		for i := uintptr(0); i <= mask; i++ {
			b := (*bmap)(add(bucket, i*uintptr(t.BucketSize)))
			for ; b != nil; b = b.overflow(t) {
				for i := uintptr(0); i < abi.MapBucketCount; i++ {
					b.tophash[i] = emptyRest
				}
			}
		}
	}
	// 标记当前桶和旧桶为空
	markBucketsEmpty(h.buckets, bucketMask(h.B))
	if oldBuckets := h.oldbuckets; oldBuckets != nil {
		markBucketsEmpty(oldBuckets, h.oldbucketmask())
	}

	// 重置 map 的各种状态
	h.flags &^= sameSizeGrow
	h.oldbuckets = nil
	h.nevacuate = 0
	h.noverflow = 0
	h.count = 0

	// Reset the hash seed to make it more difficult for attackers to
	// repeatedly trigger hash collisions. See issue 25237.
	// 重置哈希种子，使攻击者更难重复触发哈希冲突。参见 issue 25237
	h.hash0 = uint32(rand())

	// Keep the mapextra allocation but clear any extra information.
	// 保留 mapextra 的分配但清除所有额外信息
	if h.extra != nil {
		*h.extra = mapextra{}
	}

	// makeBucketArray clears the memory pointed to by h.buckets
	// and recovers any overflow buckets by generating them
	// as if h.buckets was newly alloced.
	// makeBucketArray 清除 h.buckets 指向的内存，
	// 并通过生成溢出桶来恢复它们，就像 h.buckets 是新分配的一样
	_, nextOverflow := makeBucketArray(t, h.B, h.buckets)
	if nextOverflow != nil {
		// If overflow buckets are created then h.extra
		// will have been allocated during initial bucket creation.
		// 如果创建了溢出桶，那么 h.extra 会在初始桶创建期间被分配
		h.extra.nextOverflow = nextOverflow
	}

	// 最后的并发检查
	if h.flags&hashWriting == 0 {
		fatal("concurrent map writes")
	}
	h.flags &^= hashWriting
}

func hashGrow(t *maptype, h *hmap) {
	// If we've hit the load factor, get bigger.
	// Otherwise, there are too many overflow buckets,
	// so keep the same number of buckets and "grow" laterally.
	// 如果达到了负载因子，就扩大容量。
	// 否则，说明溢出桶太多了，
	// 保持相同数量的桶，进行"横向扩容"。
	bigger := uint8(1)
	if !overLoadFactor(h.count+1, h.B) {
		bigger = 0
		h.flags |= sameSizeGrow
	}
	oldbuckets := h.buckets
	newbuckets, nextOverflow := makeBucketArray(t, h.B+bigger, nil)

	// 处理迭代器标志
	flags := h.flags &^ (iterator | oldIterator)
	if h.flags&iterator != 0 {
		flags |= oldIterator
	}
	// commit the grow (atomic wrt gc)
	// 提交扩容操作（对 GC 是原子的）
	h.B += bigger
	h.flags = flags
	h.oldbuckets = oldbuckets
	h.buckets = newbuckets
	h.nevacuate = 0
	h.noverflow = 0

	if h.extra != nil && h.extra.overflow != nil {
		// Promote current overflow buckets to the old generation.
		// 将当前的溢出桶提升到旧一代
		if h.extra.oldoverflow != nil {
			throw("oldoverflow is not nil")
		}
		h.extra.oldoverflow = h.extra.overflow
		h.extra.overflow = nil
	}
	if nextOverflow != nil {
		if h.extra == nil {
			h.extra = new(mapextra)
		}
		h.extra.nextOverflow = nextOverflow
	}

	// the actual copying of the hash table data is done incrementally
	// by growWork() and evacuate().
	// 哈希表数据的实际复制是由 growWork() 和 evacuate() 增量完成的
}

// overLoadFactor reports whether count items placed in 1<<B buckets is over loadFactor.
// overLoadFactor 报告将 count 个元素放入 1<<B 个桶中是否超过了负载因子。
func overLoadFactor(count int, B uint8) bool {
	return count > abi.MapBucketCount && uintptr(count) > loadFactorNum*(bucketShift(B)/loadFactorDen)
}

// tooManyOverflowBuckets reports whether noverflow buckets is too many for a map with 1<<B buckets.
// Note that most of these overflow buckets must be in sparse use;
// if use was dense, then we'd have already triggered regular map growth.
// tooManyOverflowBuckets 报告对于有 1<<B 个桶的 map 来说，noverflow 个溢出桶是否太多。
// 注意这些溢出桶大多数必须是稀疏使用的；
// 如果使用是密集的，那么我们早就触发常规的 map 增长了。
func tooManyOverflowBuckets(noverflow uint16, B uint8) bool {
	// If the threshold is too low, we do extraneous work.
	// If the threshold is too high, maps that grow and shrink can hold on to lots of unused memory.
	// "too many" means (approximately) as many overflow buckets as regular buckets.
	// See incrnoverflow for more details.
	// 如果阈值太低，我们会做多余的工作。
	// 如果阈值太高，增长和收缩的 map 会保留大量未使用的内存。
	// "太多"意味着（大约）溢出桶的数量和常规桶的数量一样多。
	// 更多细节请参见 incrnoverflow。
	if B > 15 {
		B = 15
	}
	// The compiler doesn't see here that B < 16; mask B to generate shorter shift code.
	// 编译器在这里看不到 B < 16；对 B 进行掩码以生成更短的移位代码。
	return noverflow >= uint16(1)<<(B&15)
}

// growing reports whether h is growing. The growth may be to the same size or bigger.
// growing 报告 h 是否正在增长。增长可能是到相同的大小或更大。
func (h *hmap) growing() bool {
	return h.oldbuckets != nil
}

// sameSizeGrow reports whether the current growth is to a map of the same size.
// sameSizeGrow 报告当前的增长是否是到相同大小的 map。
func (h *hmap) sameSizeGrow() bool {
	return h.flags&sameSizeGrow != 0
}

//go:linkname sameSizeGrowForIssue69110Test
func sameSizeGrowForIssue69110Test(h *hmap) bool {
	return h.sameSizeGrow()
}

// noldbuckets calculates the number of buckets prior to the current map growth.
// noldbuckets 计算当前 map 增长之前的桶数量。
func (h *hmap) noldbuckets() uintptr {
	oldB := h.B
	if !h.sameSizeGrow() {
		oldB--
	}
	return bucketShift(oldB)
}

// oldbucketmask provides a mask that can be applied to calculate n % noldbuckets().
// oldbucketmask 提供一个掩码，可以用来计算 n % noldbuckets()。
func (h *hmap) oldbucketmask() uintptr {
	return h.noldbuckets() - 1
}

func growWork(t *maptype, h *hmap, bucket uintptr) {
	// make sure we evacuate the oldbucket corresponding
	// to the bucket we're about to use
	// 确保我们疏散与我们即将使用的桶对应的旧桶
	evacuate(t, h, bucket&h.oldbucketmask())

	// evacuate one more oldbucket to make progress on growing
	// 再疏散一个旧桶以推进增长进度
	if h.growing() {
		evacuate(t, h, h.nevacuate)
	}
}

// bucketEvacuated reports whether the bucket has been evacuated.
// bucketEvacuated 报告桶是否已经被疏散。
func bucketEvacuated(t *maptype, h *hmap, bucket uintptr) bool {
	// Get the bucket from oldbuckets at the given index
	// 从 oldbuckets 中获取指定索引位置的桶
	b := (*bmap)(add(h.oldbuckets, bucket*uintptr(t.BucketSize)))
	// Check if the bucket has been evacuated
	// 检查该桶是否已经被疏散
	return evacuated(b)
}

// evacDst is an evacuation destination.
// evacDst 是疏散目标，用于在 map 扩容时记录数据迁移的目标位置
type evacDst struct {
	b *bmap          // current destination bucket
	i int            // key/elem index into b
	k unsafe.Pointer // pointer to current key storage
	e unsafe.Pointer // pointer to current elem storage
}

func evacuate(t *maptype, h *hmap, oldbucket uintptr) {
	// 获取需要迁移的旧桶
	b := (*bmap)(add(h.oldbuckets, oldbucket*uintptr(t.BucketSize)))
	// 获取旧桶的数量
	newbit := h.noldbuckets()
	if !evacuated(b) {
		// TODO: reuse overflow buckets instead of using new ones, if there
		// is no iterator using the old buckets.  (If !oldIterator.)

		// xy contains the x and y (low and high) evacuation destinations.
		// xy 包含 x 和 y（低和高）疏散目标
		// 在扩容时，每个旧桶的数据会被分散到两个新桶中
		var xy [2]evacDst
		x := &xy[0]
		// 设置 x 目标桶的位置
		x.b = (*bmap)(add(h.buckets, oldbucket*uintptr(t.BucketSize)))
		x.k = add(unsafe.Pointer(x.b), dataOffset)
		x.e = add(x.k, abi.MapBucketCount*uintptr(t.KeySize))

		if !h.sameSizeGrow() {
			// Only calculate y pointers if we're growing bigger.
			// Otherwise GC can see bad pointers.
			// 只有在扩容时才计算 y 指针
			// 否则 GC 可能会看到错误的指针
			y := &xy[1]
			// 设置 y 目标桶的位置，位置是 x 桶位置加上旧桶数量
			y.b = (*bmap)(add(h.buckets, (oldbucket+newbit)*uintptr(t.BucketSize)))
			y.k = add(unsafe.Pointer(y.b), dataOffset)
			y.e = add(y.k, abi.MapBucketCount*uintptr(t.KeySize))
		}

		// 遍历旧桶及其溢出桶
		for ; b != nil; b = b.overflow(t) {
			// 获取键和值的起始位置
			k := add(unsafe.Pointer(b), dataOffset)
			e := add(k, abi.MapBucketCount*uintptr(t.KeySize))
			// 遍历桶中的每个槽位
			for i := 0; i < abi.MapBucketCount; i, k, e = i+1, add(k, uintptr(t.KeySize)), add(e, uintptr(t.ValueSize)) {
				top := b.tophash[i]
				// 如果槽位为空，标记为已疏散
				if isEmpty(top) {
					b.tophash[i] = evacuatedEmpty
					continue
				}
				if top < minTopHash {
					throw("bad map state")
				}
				// 获取实际的键
				k2 := k
				if t.IndirectKey() {
					k2 = *((*unsafe.Pointer)(k2))
				}
				var useY uint8
				if !h.sameSizeGrow() {
					// Compute hash to make our evacuation decision (whether we need
					// to send this key/elem to bucket x or bucket y).
					// 计算哈希值来决定疏散目标（是发送到 x 桶还是 y 桶）
					hash := t.Hasher(k2, uintptr(h.hash0))
					if h.flags&iterator != 0 && !t.ReflexiveKey() && !t.Key.Equal(k2, k2) {
						// If key != key (NaNs), then the hash could be (and probably
						// will be) entirely different from the old hash. Moreover,
						// it isn't reproducible. Reproducibility is required in the
						// presence of iterators, as our evacuation decision must
						// match whatever decision the iterator made.
						// Fortunately, we have the freedom to send these keys either
						// way. Also, tophash is meaningless for these kinds of keys.
						// We let the low bit of tophash drive the evacuation decision.
						// We recompute a new random tophash for the next level so
						// these keys will get evenly distributed across all buckets
						// after multiple grows.
						// 对于 NaN 这样的特殊键，它们的哈希值可能完全不同且不可重现
						// 我们使用 tophash 的最低位来决定疏散目标
						// 并重新计算一个新的随机 tophash，以确保这些键在多次扩容后能均匀分布
						useY = top & 1
						top = tophash(hash)
					} else {
						// 根据哈希值决定疏散目标
						if hash&newbit != 0 {
							useY = 1
						}
					}
				}

				// 检查疏散标记是否有效
				if evacuatedX+1 != evacuatedY || evacuatedX^1 != evacuatedY {
					throw("bad evacuatedN")
				}

				// 标记旧桶中的槽位为已疏散
				b.tophash[i] = evacuatedX + useY // evacuatedX + 1 == evacuatedY
				// 选择目标桶
				dst := &xy[useY] // evacuation destination

				// 如果目标桶已满，创建新的溢出桶
				if dst.i == abi.MapBucketCount {
					dst.b = h.newoverflow(t, dst.b)
					dst.i = 0
					dst.k = add(unsafe.Pointer(dst.b), dataOffset)
					dst.e = add(dst.k, abi.MapBucketCount*uintptr(t.KeySize))
				}
				// 复制 tophash
				dst.b.tophash[dst.i&(abi.MapBucketCount-1)] = top // mask dst.i as an optimization, to avoid a bounds check
				// 复制键
				if t.IndirectKey() {
					*(*unsafe.Pointer)(dst.k) = k2 // copy pointer
				} else {
					typedmemmove(t.Key, dst.k, k) // copy elem
				}
				// 复制值
				if t.IndirectElem() {
					*(*unsafe.Pointer)(dst.e) = *(*unsafe.Pointer)(e)
				} else {
					typedmemmove(t.Elem, dst.e, e)
				}
				dst.i++
				// These updates might push these pointers past the end of the
				// key or elem arrays.  That's ok, as we have the overflow pointer
				// at the end of the bucket to protect against pointing past the
				// end of the bucket.
				// 更新键和值的指针位置
				dst.k = add(dst.k, uintptr(t.KeySize))
				dst.e = add(dst.e, uintptr(t.ValueSize))
			}
		}
		// Unlink the overflow buckets & clear key/elem to help GC.
		// 如果没有迭代器在使用旧桶，则清理旧桶中的键值对以帮助 GC
		if h.flags&oldIterator == 0 && t.Bucket.Pointers() {
			b := add(h.oldbuckets, oldbucket*uintptr(t.BucketSize))
			// Preserve b.tophash because the evacuation
			// state is maintained there.
			// 保留 tophash 因为疏散状态保存在那里
			ptr := add(b, dataOffset)
			n := uintptr(t.BucketSize) - dataOffset
			memclrHasPointers(ptr, n)
		}
	}

	// 如果当前桶是下一个需要疏散的桶，则推进疏散标记
	if oldbucket == h.nevacuate {
		advanceEvacuationMark(h, t, newbit)
	}
}
func advanceEvacuationMark(h *hmap, t *maptype, newbit uintptr) {
	h.nevacuate++
	// Experiments suggest that 1024 is overkill by at least an order of magnitude.
	// Put it in there as a safeguard anyway, to ensure O(1) behavior.
	// 实验表明 1024 至少大了一个数量级。
	// 但为了确保 O(1) 行为，仍然将其作为安全保护措施。
	stop := h.nevacuate + 1024
	if stop > newbit {
		stop = newbit
	}
	// 检查连续的桶是否已经完成疏散
	for h.nevacuate != stop && bucketEvacuated(t, h, h.nevacuate) {
		h.nevacuate++
	}
	if h.nevacuate == newbit { // newbit == # of oldbuckets
		// Growing is all done. Free old main bucket array.
		// 增长已完成。释放旧的桶数组。
		h.oldbuckets = nil
		// Can discard old overflow buckets as well.
		// If they are still referenced by an iterator,
		// then the iterator holds a pointers to the slice.
		// 也可以丢弃旧的溢出桶。
		// 如果它们仍然被迭代器引用，
		// 那么迭代器会持有指向该切片的指针。
		if h.extra != nil {
			h.extra.oldoverflow = nil
		}
		h.flags &^= sameSizeGrow
	}
}

// Reflect stubs. Called from ../reflect/asm_*.s

// reflect_makemap is for package reflect,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - gitee.com/quant1x/gox
//   - github.com/modern-go/reflect2
//   - github.com/goccy/go-json
//   - github.com/RomiChan/protobuf
//   - github.com/segmentio/encoding
//   - github.com/v2pro/plz
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname reflect_makemap reflect.makemap
func reflect_makemap(t *maptype, cap int) *hmap {
	// Check invariants and reflects math.
	if t.Key.Equal == nil {
		throw("runtime.reflect_makemap: unsupported map key type")
	}
	if t.Key.Size_ > abi.MapMaxKeyBytes && (!t.IndirectKey() || t.KeySize != uint8(goarch.PtrSize)) ||
		t.Key.Size_ <= abi.MapMaxKeyBytes && (t.IndirectKey() || t.KeySize != uint8(t.Key.Size_)) {
		throw("key size wrong")
	}
	if t.Elem.Size_ > abi.MapMaxElemBytes && (!t.IndirectElem() || t.ValueSize != uint8(goarch.PtrSize)) ||
		t.Elem.Size_ <= abi.MapMaxElemBytes && (t.IndirectElem() || t.ValueSize != uint8(t.Elem.Size_)) {
		throw("elem size wrong")
	}
	if t.Key.Align_ > abi.MapBucketCount {
		throw("key align too big")
	}
	if t.Elem.Align_ > abi.MapBucketCount {
		throw("elem align too big")
	}
	if t.Key.Size_%uintptr(t.Key.Align_) != 0 {
		throw("key size not a multiple of key align")
	}
	if t.Elem.Size_%uintptr(t.Elem.Align_) != 0 {
		throw("elem size not a multiple of elem align")
	}
	if abi.MapBucketCount < 8 {
		throw("bucketsize too small for proper alignment")
	}
	if dataOffset%uintptr(t.Key.Align_) != 0 {
		throw("need padding in bucket (key)")
	}
	if dataOffset%uintptr(t.Elem.Align_) != 0 {
		throw("need padding in bucket (elem)")
	}

	return makemap(t, cap, nil)
}

// reflect_mapaccess is for package reflect,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - gitee.com/quant1x/gox
//   - github.com/modern-go/reflect2
//   - github.com/v2pro/plz
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname reflect_mapaccess reflect.mapaccess
func reflect_mapaccess(t *maptype, h *hmap, key unsafe.Pointer) unsafe.Pointer {
	elem, ok := mapaccess2(t, h, key)
	if !ok {
		// reflect wants nil for a missing element
		elem = nil
	}
	return elem
}

//go:linkname reflect_mapaccess_faststr reflect.mapaccess_faststr
func reflect_mapaccess_faststr(t *maptype, h *hmap, key string) unsafe.Pointer {
	elem, ok := mapaccess2_faststr(t, h, key)
	if !ok {
		// reflect wants nil for a missing element
		elem = nil
	}
	return elem
}

// reflect_mapassign is for package reflect,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - gitee.com/quant1x/gox
//   - github.com/v2pro/plz
//
// Do not remove or change the type signature.
//
//go:linkname reflect_mapassign reflect.mapassign0
func reflect_mapassign(t *maptype, h *hmap, key unsafe.Pointer, elem unsafe.Pointer) {
	p := mapassign(t, h, key)
	typedmemmove(t.Elem, p, elem)
}

//go:linkname reflect_mapassign_faststr reflect.mapassign_faststr0
func reflect_mapassign_faststr(t *maptype, h *hmap, key string, elem unsafe.Pointer) {
	p := mapassign_faststr(t, h, key)
	typedmemmove(t.Elem, p, elem)
}

//go:linkname reflect_mapdelete reflect.mapdelete
func reflect_mapdelete(t *maptype, h *hmap, key unsafe.Pointer) {
	mapdelete(t, h, key)
}

//go:linkname reflect_mapdelete_faststr reflect.mapdelete_faststr
func reflect_mapdelete_faststr(t *maptype, h *hmap, key string) {
	mapdelete_faststr(t, h, key)
}

// reflect_mapiterinit is for package reflect,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/modern-go/reflect2
//   - gitee.com/quant1x/gox
//   - github.com/v2pro/plz
//   - github.com/wI2L/jettison
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname reflect_mapiterinit reflect.mapiterinit
func reflect_mapiterinit(t *maptype, h *hmap, it *hiter) {
	mapiterinit(t, h, it)
}

// reflect_mapiternext is for package reflect,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - gitee.com/quant1x/gox
//   - github.com/modern-go/reflect2
//   - github.com/goccy/go-json
//   - github.com/v2pro/plz
//   - github.com/wI2L/jettison
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname reflect_mapiternext reflect.mapiternext
func reflect_mapiternext(it *hiter) {
	mapiternext(it)
}

// reflect_mapiterkey is for package reflect,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/goccy/go-json
//   - gonum.org/v1/gonum
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname reflect_mapiterkey reflect.mapiterkey
func reflect_mapiterkey(it *hiter) unsafe.Pointer {
	return it.key
}

// reflect_mapiterelem is for package reflect,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/goccy/go-json
//   - gonum.org/v1/gonum
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname reflect_mapiterelem reflect.mapiterelem
func reflect_mapiterelem(it *hiter) unsafe.Pointer {
	return it.elem
}

// reflect_maplen is for package reflect,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/goccy/go-json
//   - github.com/wI2L/jettison
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname reflect_maplen reflect.maplen
func reflect_maplen(h *hmap) int {
	if h == nil {
		return 0
	}
	if raceenabled {
		callerpc := getcallerpc()
		racereadpc(unsafe.Pointer(h), callerpc, abi.FuncPCABIInternal(reflect_maplen))
	}
	return h.count
}

//go:linkname reflect_mapclear reflect.mapclear
func reflect_mapclear(t *maptype, h *hmap) {
	mapclear(t, h)
}

//go:linkname reflectlite_maplen internal/reflectlite.maplen
func reflectlite_maplen(h *hmap) int {
	if h == nil {
		return 0
	}
	if raceenabled {
		callerpc := getcallerpc()
		racereadpc(unsafe.Pointer(h), callerpc, abi.FuncPCABIInternal(reflect_maplen))
	}
	return h.count
}

// mapinitnoop is a no-op function known the Go linker; if a given global
// map (of the right size) is determined to be dead, the linker will
// rewrite the relocation (from the package init func) from the outlined
// map init function to this symbol. Defined in assembly so as to avoid
// complications with instrumentation (coverage, etc).
func mapinitnoop()

// mapclone for implementing maps.Clone
//
//go:linkname mapclone maps.clone
func mapclone(m any) any {
	e := efaceOf(&m)
	e.data = unsafe.Pointer(mapclone2((*maptype)(unsafe.Pointer(e._type)), (*hmap)(e.data)))
	return m
}

// moveToBmap moves a bucket from src to dst. It returns the destination bucket or new destination bucket if it overflows
// and the pos that the next key/value will be written, if pos == bucketCnt means needs to written in overflow bucket.
// moveToBmap 将桶从 src 移动到 dst。如果发生溢出，它返回目标桶或新的目标桶，
// 以及下一个键/值将被写入的位置，如果 pos == bucketCnt 意味着需要写入溢出桶。
func moveToBmap(t *maptype, h *hmap, dst *bmap, pos int, src *bmap) (*bmap, int) {
	// 遍历源桶中的所有槽位
	for i := 0; i < abi.MapBucketCount; i++ {
		// 如果源桶中的槽位为空，跳过
		if isEmpty(src.tophash[i]) {
			continue
		}

		// 在目标桶中寻找下一个空槽位
		for ; pos < abi.MapBucketCount; pos++ {
			if isEmpty(dst.tophash[pos]) {
				break
			}
		}

		// 如果目标桶已满，创建新的溢出桶
		if pos == abi.MapBucketCount {
			dst = h.newoverflow(t, dst)
			pos = 0
		}

		// 计算源桶和目标桶中键和值的地址
		srcK := add(unsafe.Pointer(src), dataOffset+uintptr(i)*uintptr(t.KeySize))
		srcEle := add(unsafe.Pointer(src), dataOffset+abi.MapBucketCount*uintptr(t.KeySize)+uintptr(i)*uintptr(t.ValueSize))
		dstK := add(unsafe.Pointer(dst), dataOffset+uintptr(pos)*uintptr(t.KeySize))
		dstEle := add(unsafe.Pointer(dst), dataOffset+abi.MapBucketCount*uintptr(t.KeySize)+uintptr(pos)*uintptr(t.ValueSize))

		// 复制 tophash
		dst.tophash[pos] = src.tophash[i]

		// 处理键的复制
		if t.IndirectKey() {
			// 获取键的指针
			srcK = *(*unsafe.Pointer)(srcK)
			if t.NeedKeyUpdate() {
				// 如果需要更新键，创建新的存储空间并复制
				kStore := newobject(t.Key)
				typedmemmove(t.Key, kStore, srcK)
				srcK = kStore
			}
			// Note: if NeedKeyUpdate is false, then the memory
			// used to store the key is immutable, so we can share
			// it between the original map and its clone.
			// 注意：如果 NeedKeyUpdate 为 false，那么用于存储键的内存是不可变的，
			// 所以我们可以在原始 map 和其克隆之间共享它。
			*(*unsafe.Pointer)(dstK) = srcK
		} else {
			// 直接复制键
			typedmemmove(t.Key, dstK, srcK)
		}

		// 处理值的复制
		if t.IndirectElem() {
			// 获取值的指针并创建新的存储空间
			srcEle = *(*unsafe.Pointer)(srcEle)
			eStore := newobject(t.Elem)
			typedmemmove(t.Elem, eStore, srcEle)
			*(*unsafe.Pointer)(dstEle) = eStore
		} else {
			// 直接复制值
			typedmemmove(t.Elem, dstEle, srcEle)
		}
		pos++
		h.count++
	}
	return dst, pos
}
func mapclone2(t *maptype, src *hmap) *hmap {
	// 使用源 map 的元素数量作为初始容量
	hint := src.count
	if overLoadFactor(hint, src.B) {
		// Note: in rare cases (e.g. during a same-sized grow) the map
		// can be overloaded. Make sure we don't allocate a destination
		// bucket array larger than the source bucket array.
		// This will cause the cloned map to be overloaded also,
		// but that's better than crashing. See issue 69110.
		// 注意：在极少数情况下（例如在相同大小的增长期间），map 可能会过载。
		// 确保我们不会分配比源桶数组更大的目标桶数组。
		// 这会导致克隆的 map 也过载，但这比崩溃要好。参见 issue 69110。
		hint = int(loadFactorNum * (bucketShift(src.B) / loadFactorDen))
	}
	// 创建新的 map
	dst := makemap(t, hint, nil)
	// 复制哈希种子
	dst.hash0 = src.hash0
	dst.nevacuate = 0
	// flags do not need to be copied here, just like a new map has no flags.
	// 这里不需要复制 flags，就像新 map 没有 flags 一样

	// 如果源 map 为空，直接返回
	if src.count == 0 {
		return dst
	}

	// 检查并发写入
	if src.flags&hashWriting != 0 {
		fatal("concurrent map clone and map write")
	}

	// 对于小 map 的快速复制
	if src.B == 0 && !(t.IndirectKey() && t.NeedKeyUpdate()) && !t.IndirectElem() {
		// Quick copy for small maps.
		dst.buckets = newobject(t.Bucket)
		dst.count = src.count
		typedmemmove(t.Bucket, dst.buckets, src.buckets)
		return dst
	}

	// 确保目标 map 有桶数组
	if dst.B == 0 {
		dst.buckets = newobject(t.Bucket)
	}
	// 计算源和目标桶数组的大小
	dstArraySize := int(bucketShift(dst.B))
	srcArraySize := int(bucketShift(src.B))
	// 遍历目标桶数组
	for i := 0; i < dstArraySize; i++ {
		dstBmap := (*bmap)(add(dst.buckets, uintptr(i*int(t.BucketSize))))
		pos := 0
		// 遍历源桶数组，步长为目标桶数组大小
		for j := 0; j < srcArraySize; j += dstArraySize {
			srcBmap := (*bmap)(add(src.buckets, uintptr((i+j)*int(t.BucketSize))))
			// 处理源桶及其溢出桶
			for srcBmap != nil {
				dstBmap, pos = moveToBmap(t, dst, dstBmap, pos, srcBmap)
				srcBmap = srcBmap.overflow(t)
			}
		}
	}

	// 如果没有旧桶，直接返回
	if src.oldbuckets == nil {
		return dst
	}

	// 处理旧桶中的数据
	oldB := src.B
	srcOldbuckets := src.oldbuckets
	if !src.sameSizeGrow() {
		oldB--
	}
	oldSrcArraySize := int(bucketShift(oldB))

	// 遍历旧桶数组
	for i := 0; i < oldSrcArraySize; i++ {
		srcBmap := (*bmap)(add(srcOldbuckets, uintptr(i*int(t.BucketSize))))
		// 如果桶已被疏散，跳过
		if evacuated(srcBmap) {
			continue
		}

		if oldB >= dst.B { // main bucket bits in dst is less than oldB bits in src
			// 目标桶的位数小于源旧桶的位数
			dstBmap := (*bmap)(add(dst.buckets, (uintptr(i)&bucketMask(dst.B))*uintptr(t.BucketSize)))
			// 找到目标桶的最后一个溢出桶
			for dstBmap.overflow(t) != nil {
				dstBmap = dstBmap.overflow(t)
			}
			pos := 0
			// 处理源桶及其溢出桶
			for srcBmap != nil {
				dstBmap, pos = moveToBmap(t, dst, dstBmap, pos, srcBmap)
				srcBmap = srcBmap.overflow(t)
			}
			continue
		}

		// oldB < dst.B, so a single source bucket may go to multiple destination buckets.
		// Process entries one at a time.
		// 源旧桶的位数小于目标桶的位数，所以一个源桶可能分散到多个目标桶中
		// 一次处理一个条目
		for srcBmap != nil {
			// move from oldBlucket to new bucket
			// 从旧桶移动到新桶
			for i := uintptr(0); i < abi.MapBucketCount; i++ {
				if isEmpty(srcBmap.tophash[i]) {
					continue
				}

				if src.flags&hashWriting != 0 {
					fatal("concurrent map clone and map write")
				}

				// 获取源键和值的地址
				srcK := add(unsafe.Pointer(srcBmap), dataOffset+i*uintptr(t.KeySize))
				if t.IndirectKey() {
					srcK = *((*unsafe.Pointer)(srcK))
				}

				srcEle := add(unsafe.Pointer(srcBmap), dataOffset+abi.MapBucketCount*uintptr(t.KeySize)+i*uintptr(t.ValueSize))
				if t.IndirectElem() {
					srcEle = *((*unsafe.Pointer)(srcEle))
				}
				// 分配新位置并复制值
				dstEle := mapassign(t, dst, srcK)
				typedmemmove(t.Elem, dstEle, srcEle)
			}
			srcBmap = srcBmap.overflow(t)
		}
	}
	return dst
}

// keys for implementing maps.keys
// 实现 maps.keys 的底层函数
//
//go:linkname keys maps.keys
func keys(m any, p unsafe.Pointer) {
	// 获取 map 的类型信息和底层数据结构
	e := efaceOf(&m)
	t := (*maptype)(unsafe.Pointer(e._type))
	h := (*hmap)(e.data)

	// 如果 map 为空，直接返回
	if h == nil || h.count == 0 {
		return
	}
	// 获取目标切片
	s := (*slice)(p)
	// 生成随机数用于随机化遍历顺序
	r := int(rand())
	// 计算随机偏移量，用于随机化桶内元素的访问顺序
	offset := uint8(r >> h.B & (abi.MapBucketCount - 1))

	// 如果 map 只有一个桶，直接处理
	if h.B == 0 {
		copyKeys(t, h, (*bmap)(h.buckets), s, offset)
		return
	}

	// 计算桶数组大小并遍历所有桶
	arraySize := int(bucketShift(h.B))
	buckets := h.buckets
	for i := 0; i < arraySize; i++ {
		// 使用随机数打乱桶的访问顺序
		bucket := (i + r) & (arraySize - 1)
		b := (*bmap)(add(buckets, uintptr(bucket)*uintptr(t.BucketSize)))
		copyKeys(t, h, b, s, offset)
	}

	// 如果 map 正在扩容，还需要处理旧桶中的数据
	if h.growing() {
		oldArraySize := int(h.noldbuckets())
		for i := 0; i < oldArraySize; i++ {
			bucket := (i + r) & (oldArraySize - 1)
			b := (*bmap)(add(h.oldbuckets, uintptr(bucket)*uintptr(t.BucketSize)))
			// 跳过已经迁移的桶
			if evacuated(b) {
				continue
			}
			copyKeys(t, h, b, s, offset)
		}
	}
	return
}
func copyKeys(t *maptype, h *hmap, b *bmap, s *slice, offset uint8) {
	// 遍历桶及其溢出桶
	for b != nil {
		// 遍历桶中的每个槽位
		for i := uintptr(0); i < abi.MapBucketCount; i++ {
			// 使用随机偏移量计算实际要访问的槽位
			offi := (i + uintptr(offset)) & (abi.MapBucketCount - 1)
			// 如果槽位为空，跳过
			if isEmpty(b.tophash[offi]) {
				continue
			}
			// 检查是否有并发写入
			if h.flags&hashWriting != 0 {
				fatal("concurrent map read and map write")
			}
			// 计算键的地址
			k := add(unsafe.Pointer(b), dataOffset+offi*uintptr(t.KeySize))
			// 如果键是间接存储的，获取实际存储位置
			if t.IndirectKey() {
				k = *((*unsafe.Pointer)(k))
			}
			// 检查切片容量是否足够
			if s.len >= s.cap {
				fatal("concurrent map read and map write")
			}
			// 将键复制到切片中
			typedmemmove(t.Key, add(s.array, uintptr(s.len)*uintptr(t.Key.Size())), k)
			// 更新切片长度
			s.len++
		}
		// 处理下一个溢出桶
		b = b.overflow(t)
	}
}

// values for implementing maps.values
// values 函数用于实现 maps.values 方法，将 map 中的所有值复制到切片中
//
//go:linkname values maps.values
func values(m any, p unsafe.Pointer) {
	// 获取 map 的类型信息和底层数据结构
	e := efaceOf(&m)
	t := (*maptype)(unsafe.Pointer(e._type))
	h := (*hmap)(e.data)

	// 如果 map 为空，直接返回
	if h == nil || h.count == 0 {
		return
	}

	// 获取目标切片
	s := (*slice)(p)
	// 生成随机数用于随机访问桶
	r := int(rand())
	// 计算随机偏移量，用于随机访问桶中的槽位
	offset := uint8(r >> h.B & (abi.MapBucketCount - 1))

	// 处理小 map 的情况（B=0）
	if h.B == 0 {
		copyValues(t, h, (*bmap)(h.buckets), s, offset)
		return
	}

	// 计算桶数组大小
	arraySize := int(bucketShift(h.B))
	buckets := h.buckets
	// 遍历所有桶
	for i := 0; i < arraySize; i++ {
		// 使用随机数计算要访问的桶
		bucket := (i + r) & (arraySize - 1)
		b := (*bmap)(add(buckets, uintptr(bucket)*uintptr(t.BucketSize)))
		copyValues(t, h, b, s, offset)
	}

	// 如果 map 正在扩容，还需要处理旧桶中的数据
	if h.growing() {
		oldArraySize := int(h.noldbuckets())
		for i := 0; i < oldArraySize; i++ {
			bucket := (i + r) & (oldArraySize - 1)
			b := (*bmap)(add(h.oldbuckets, uintptr(bucket)*uintptr(t.BucketSize)))
			// 跳过已经迁移的桶
			if evacuated(b) {
				continue
			}
			copyValues(t, h, b, s, offset)
		}
	}
	return
}

// copyValues 将桶中的值复制到切片中
func copyValues(t *maptype, h *hmap, b *bmap, s *slice, offset uint8) {
	// 遍历桶及其溢出桶
	for b != nil {
		// 遍历桶中的每个槽位
		for i := uintptr(0); i < abi.MapBucketCount; i++ {
			// 使用随机偏移量计算实际要访问的槽位
			offi := (i + uintptr(offset)) & (abi.MapBucketCount - 1)
			// 如果槽位为空，跳过
			if isEmpty(b.tophash[offi]) {
				continue
			}

			// 检查是否有并发写入
			if h.flags&hashWriting != 0 {
				fatal("concurrent map read and map write")
			}

			// 计算值的地址
			ele := add(unsafe.Pointer(b), dataOffset+abi.MapBucketCount*uintptr(t.KeySize)+offi*uintptr(t.ValueSize))
			// 如果值是间接存储的，获取实际存储位置
			if t.IndirectElem() {
				ele = *((*unsafe.Pointer)(ele))
			}
			// 检查切片容量是否足够
			if s.len >= s.cap {
				fatal("concurrent map read and map write")
			}
			// 将值复制到切片中
			typedmemmove(t.Elem, add(s.array, uintptr(s.len)*uintptr(t.Elem.Size())), ele)
			// 更新切片长度
			s.len++
		}
		// 处理下一个溢出桶
		b = b.overflow(t)
	}
}
