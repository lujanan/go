# Go 源码学习笔记：map.go

Go 语言的 `map` 是一个使用哈希表实现的无序键值对集合。它的实现非常精巧，兼顾了高性能和内存效率。其核心源码位于 `src/runtime/map.go`。

## 1. 核心数据结构

`map` 的实现主要围绕两个核心结构体：`hmap` (哈希表头) 和 `bmap` (哈希桶)。

## `hmap` 结构体

`hmap` 是 `map` 在运行时的头部表示，它定义了 `map` 的所有核心属性。

```go
// A header for a Go map.
type hmap struct {
	count     int            // map 中元素的个数，len() 函数返回的就是这个值
	flags     uint8          // 标志位，例如是否处于写入状态
	B         uint8          // 桶数量的对数，即桶的数量为 2^B
	noverflow uint16         // 溢出桶的大致数量
	hash0     uint32         // 哈希种子

	buckets    unsafe.Pointer // 指向当前桶数组的指针，数组大小为 2^B
	oldbuckets unsafe.Pointer // 指向旧桶数组的指针，仅在扩容时非 nil，大小为 2^(B-1)
	nevacuate  uintptr        // 扩容进度计数器，表示已迁移的旧桶数量

	extra *mapextra // 可选字段，用于存储溢出桶等信息
}
```

**字段详解:**

*   `count`: `map` 中当前存储的键值对数量。
*   `B`: 一个整数，表示 `map` 中桶的数量为 `2^B`。
*   `hash0`: 哈希种子，在创建 `map` 时随机生成，用于计算 key 的哈希值，以减少哈希冲突的概率。
*   `buckets`: 指向一个包含 `2^B` 个桶（`bmap`）的数组。
*   `oldbuckets`: 在 `map` 扩容时，`oldbuckets` 指向旧的桶数组，`buckets` 指向新的、更大的桶数组。数据会从 `oldbuckets` 渐进地迁移到 `buckets`。
*   `nevacuate`: 记录了从 `oldbuckets` 到 `buckets` 的迁移进度。
*   `noverflow`: 一个近似值，记录了溢出桶的数量。当溢出桶过多时，会触发“等量扩容”。

## `bmap` 结构体 (桶)

`bmap` 是 `map` 的基本存储单元，通常被称为“桶”。

```go
// A bucket for a Go map.
type bmap struct {
	tophash [8]uint8
	// 后面跟着 8 个 key
	// 再后面跟着 8 个 value
	// 最后可能有一个指向溢出桶的指针
}
```

**结构特点:**

*   **`tophash`**: 一个长度为 8 的数组，存储了每个 key 哈希值的高 8 位 (top hash)。这是一种优化，用于在查找时快速过滤掉不匹配的 key，而无需比较完整的 key。
*   **固定容量**: 每个 `bmap` 最多可以存储 8 个键值对。
*   **数据布局**: `bmap` 的内存布局是连续的：`tophash` 数组 -> 8 个 key -> 8 个 value -> 1 个溢出桶指针。这种布局可以优化内存访问，并减少因对齐造成的内存浪费。
*   **溢出桶 (Overflow Bucket)**: 如果超过 8 个键值对哈希到同一个桶，`map` 会创建一个新的 `bmap`（称为溢出桶），并用链表的方式将它们连接起来。

## `bmap` 结构体在编译后的变化

`bmap` 结构体在 `src/runtime/map.go` 中的定义更像一个**模板或蓝图**。在编译期间，编译器会根据你声明的 `map` 的具体 `key` 和 `value` 类型，动态地创建一个**特化**的、具体的 `bmap` 结构体。

例如，当你定义一个 `map[int64]string` 时，编译器会生成一个类似下面这样的内部结构体（此为示意）：

```go
// 编译器内部为 map[int64]string 生成的特化 bmap
type bmap_int64_string struct {
    tophash  [8]uint8
    keys     [8]int64
    values   [8]string
    overflow *bmap_int64_string // 指向同类型的溢出桶
}
```

**这种编译时转换带来了几个关键好处：**

1.  **类型安全**: 源码中 `bmap` 后面跟着的 `key` 和 `value` 区域在编译后被替换成了**具体类型的数组**。这使得运行时代码可以直接通过具体类型进行操作，而不需要像 `mapaccess` 源码里那样通过 `unsafe.Pointer` 进行手动的、不安全的指针运算。

2.  **内存布局优化 (Padding Elimination)**: 编译器会将所有的 `key` 放在一起，所有的 `value` 放在一起，形成 `[tophash] - [keys] - [values] - [overflow]` 这样的内存布局。这可以避免因数据对齐（如 `map[int64]int8`）而产生的内存空洞，提高内存利用率。

3.  **GC 扫描优化**: 当 `key` 和 `value` 都不包含指针时（例如 `map[int]int`），编译器生成的特化 `bmap` 结构体中，除了最后的 `overflow` 指针外，就不再包含任何指针。运行时会将 `overflow` 指针单独处理（存储在 `hmap.extra` 字段中），使得整个 `buckets` 数组可以被标记为**不含指针**。这样，垃圾回收器（GC）在扫描时就可以**完全跳过**这个巨大的 `buckets` 数组，极大地提高了 GC 的效率。

总而言之，`bmap` 在编译后，会从一个泛化的、依赖 `unsafe.Pointer` 的运行时定义，转变为一个为特定 `map` 类型量身定做的、类型安全的、内存布局紧凑的具体结构体。

## 2. `makemap`：创建 Map (详细源码分析)

当我们使用 `make(map[K]V, hint)` 创建 `map` 时，编译器会根据 `hint` 的大小和 `map` 是否会发生逃逸，可能将其转换为 `makemap_small` (针对 `hint` <= 8 的情况) 或 `makemap` (更通用的情况)。这里我们重点分析更通用的 `makemap` 函数。

```go
// src/runtime/map.go

func makemap(t *maptype, hint int, h *hmap) *hmap {
	// 1. 检查 hint 是否溢出或过大
	mem, overflow := math.MulUintptr(uintptr(hint), t.Bucket.Size_)
	if overflow || mem > maxAlloc {
		hint = 0
	}

	// 2. 初始化 hmap 头部
	if h == nil {
		h = new(hmap)
	}
	h.hash0 = uint32(rand()) // 生成随机哈希种子

	// 3. 计算并设置 B 值 (桶数量的对数)
	B := uint8(0)
	for overLoadFactor(hint, B) {
		B++
	}
	h.B = B

	// 4. 分配桶数组内存
	if h.B != 0 {
		var nextOverflow *bmap
		// 调用 makeBucketArray 分配 2^B 个桶，并可能预分配一些溢出桶
		h.buckets, nextOverflow = makeBucketArray(t, h.B, nil)
		if nextOverflow != nil {
			h.extra = new(mapextra)
			h.extra.nextOverflow = nextOverflow
		}
	}

	return h
}
```

**源码分析:**

1.  **安全检查**: 首先，函数会估算 `hint` 个元素大约需要多少内存 (`mem`)。如果 `hint` 值过大导致计算溢出，或者所需内存超过了最大分配限制 (`maxAlloc`)，`hint` 会被重置为 0。
2.  **初始化 `hmap`**: 如果传入的 `h` 为 `nil`，则在堆上分配一个新的 `hmap` 结构体。然后，为 `h.hash0` 赋予一个随机的哈希种子，这是 `map` 遍历无序和降低哈希冲突攻击风险的关键。
3.  **计算 `B` 值**: 这是决定 `map` 初始大小的核心。函数会进入一个 `for` 循环，不断增加 `B` 的值，直到 `2^B` 个桶足以在负载因子（默认为 6.5）下容纳 `hint` 个元素。`overLoadFactor` 函数负责这个判断。
4.  **分配桶内存**:
    *   如果计算出的 `B` 不为 0，则调用 `makeBucketArray` 来实际分配桶数组的内存。
    *   `makeBucketArray` 不仅会分配 `2^B` 个主桶，还会根据 `B` 的大小，策略性地**预分配一些溢出桶**。这些预分配的溢出桶可以减少后续插入操作时因创建溢出桶带来的性能开销。
    *   预分配的溢出桶信息会存放在 `h.extra.nextOverflow` 中。
    *   如果 `B` 为 0 (例如 `make(map[int]int)`), `makemap` 不会立即分配桶内存，而是等到第一次向 `map` 中插入数据时再懒加载。

## 3. `mapassign`：写入/更新键值对 (详细源码分析)

`mapassign` 是 `map` 实现中逻辑最复杂的函数之一，因为它不仅处理键值对的插入和更新，还承担了触发扩容和协助扩容数据迁移的职责。

```go
// src/runtime/map.go

func mapassign(t *maptype, h *hmap, key unsafe.Pointer) unsafe.Pointer {
	// 1. 前置检查
	if h == nil {
		panic(plainError("assignment to entry in nil map"))
	}
	if h.flags&hashWriting != 0 {
		fatal("concurrent map writes")
	}

	// 2. 计算哈希
	hash := t.Hasher(key, uintptr(h.hash0))

	// 3. 设置写标志位
	h.flags ^= hashWriting

	// 4. 懒加载桶
	if h.buckets == nil {
		h.buckets = newobject(t.Bucket)
	}

again:
	// 5. 定位桶
	bucket := hash & bucketMask(h.B)
	if h.growing() {
		// 协助扩容
		growWork(t, h, bucket)
	}
	b := (*bmap)(add(h.buckets, bucket*uintptr(t.BucketSize)))
	top := tophash(hash)

	var inserti *uint8
	var insertk unsafe.Pointer
	var elem unsafe.Pointer

bucketloop:
	// 6. 遍历桶链查找 key 或空位
	for {
		for i := uintptr(0); i < abi.MapBucketCount; i++ {
			if b.tophash[i] != top {
				// 记录第一个空位
				if isEmpty(b.tophash[i]) && inserti == nil {
					inserti = &b.tophash[i]
					insertk = add(unsafe.Pointer(b), dataOffset+i*uintptr(t.KeySize))
					elem = add(unsafe.Pointer(b), dataOffset+abi.MapBucketCount*uintptr(t.KeySize)+i*uintptr(t.ValueSize))
				}
				if b.tophash[i] == emptyRest {
					break bucketloop
				}
				continue
			}
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.KeySize))
			if t.IndirectKey() {
				k = *((*unsafe.Pointer)(k))
			}
			if !t.Key.Equal(key, k) {
				continue
			}
			// 找到了 key，直接更新
			if t.NeedKeyUpdate() {
				typedmemmove(t.Key, k, key)
			}
			elem = add(unsafe.Pointer(b), dataOffset+abi.MapBucketCount*uintptr(t.KeySize)+i*uintptr(t.ValueSize))
			goto done
		}
		ovf := b.overflow(t)
		if ovf == nil {
			break
		}
		b = ovf
	}

	// 7. 检查是否需要扩容
	if !h.growing() && (overLoadFactor(h.count+1, h.B) || tooManyOverflowBuckets(h.noverflow, h.B)) {
		hashGrow(t, h)
		goto again // 扩容后需要重试整个过程
	}

	// 8. 插入新元素
	if inserti == nil {
		// 所有桶都满了，创建新溢出桶
		newb := h.newoverflow(t, b)
		inserti = &newb.tophash[0]
		insertk = add(unsafe.Pointer(newb), dataOffset)
		elem = add(insertk, abi.MapBucketCount*uintptr(t.KeySize))
	}
	// ... (省略了为 key 和 elem 分配内存的细节)
	typedmemmove(t.Key, insertk, key) // 拷贝 key
	*inserti = top                     // 设置 tophash
	h.count++                          // 计数加一

done:
	// 9. 清除写标志位并返回
	h.flags &^= hashWriting
	if t.IndirectElem() {
		elem = *((*unsafe.Pointer)(elem))
	}
	return elem
}
```

**源码分析:**

1.  **前置检查**: 检查 `map` 是否为 `nil`，以及是否正在被其他 goroutine 写入。
2.  **计算哈希**: 调用 `key` 类型的哈希函数，结合 `hmap` 的随机种子 `hash0`，生成哈希值。
3.  **设置写标志**: 使用异或操作 `^=` 将 `hashWriting` 标志位置为 1，表示开始写入。`defer` 或 `goto done` 后的 `&^=` 操作会将其清零。
4.  **懒加载**: 如果 `map` 是首次插入数据 (`h.buckets == nil`)，会在此处分配第一个桶。
5.  **定位桶与协助扩容**:
    *   通过 `hash & bucketMask(h.B)` 计算出 `key` 应该落在哪个桶。
    *   **关键点**: 如果 `map` 正在扩容 (`h.growing()` 为 `true`)，它不会等待扩容结束，而是会调用 `growWork` 主动帮助迁移一两个桶的数据，这就是渐进式扩容的体现。
6.  **遍历查找**:
    *   `bucketloop` 是一个精巧的循环，它会遍历主桶和所有溢出桶。
    *   在内层循环中，它会检查 8 个槽位：
        *   如果 `tophash` 不匹配，但槽位是空的 (`isEmpty`)，则记录下这个位置作为备选的插入点。
        *   如果 `tophash` 匹配，则进行完整的 `key` 比较。
        *   如果 `key` 也匹配，说明是**更新操作**，直接 `goto done` 跳转到返回步骤。
7.  **检查扩容**: 如果遍历完所有桶链都没找到 `key`（说明是插入操作），则会检查是否满足扩容条件：
    *   `overLoadFactor`: 负载因子是否超过 6.5。
    *   `tooManyOverflowBuckets`: 溢出桶是否过多。
    *   如果满足任一条件，则调用 `hashGrow` 开始扩容，并使用 `goto again` **重新开始整个 `mapassign` 过程**。这是因为扩容会改变 `h.B` 和 `buckets` 指针，之前计算的桶位置可能失效。
8.  **插入新元素**:
    *   如果之前找到了可用的空槽位 (`inserti != nil`)，就直接使用。
    *   如果没找到（说明桶链全满了），则调用 `h.newoverflow` 创建一个新的溢出桶，并获取新桶的第一个槽位作为插入点。
    *   将 `key` 和 `value` 的数据拷贝到插入位置，设置 `tophash`，并将 `h.count` 加一。
9.  **返回**: 清除 `hashWriting` 标志，并返回新插入/更新的 `value` 的内存地址。

## 4. `mapaccess`：读取键值对 (详细源码分析)

`map` 的读取操作主要由 `mapaccess1` (用于 `v = m[k]`) 和 `mapaccess2` (用于 `v, ok = m[k]`) 实现。我们重点分析 `mapaccess2`，因为它包含了完整的逻辑。

```go
// src/runtime/map.go

func mapaccess2(t *maptype, h *hmap, key unsafe.Pointer) (unsafe.Pointer, bool) {
	// 1. 前置检查
	if h == nil || h.count == 0 {
		return unsafe.Pointer(&zeroVal[0]), false
	}
	if h.flags&hashWriting != 0 {
		fatal("concurrent map read and map write")
	}

	// 2. 计算哈希
	hash := t.Hasher(key, uintptr(h.hash0))

	// 3. 定位桶
	m := bucketMask(h.B)
	b := (*bmap)(add(h.buckets, (hash&m)*uintptr(t.BucketSize)))

	// 4. 检查旧桶 (扩容时)
	if c := h.oldbuckets; c != nil {
		if !h.sameSizeGrow() {
			m >>= 1
		}
		oldb := (*bmap)(add(c, (hash&m)*uintptr(t.BucketSize)))
		if !evacuated(oldb) {
			b = oldb // 如果旧桶未迁移，则从旧桶开始查找
		}
	}

	// 5. 遍历桶链查找
	top := tophash(hash)

bucketloop:
	for ; b != nil; b = b.overflow(t) {
		for i := uintptr(0); i < abi.MapBucketCount; i++ {
			if b.tophash[i] != top {
				if b.tophash[i] == emptyRest {
					break bucketloop // 优化：后续槽位都是空的
				}
				continue
			}
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.KeySize))
			if t.IndirectKey() {
				k = *((*unsafe.Pointer)(k))
			}
			if t.Key.Equal(key, k) {
				// 找到了 key
				e := add(unsafe.Pointer(b), dataOffset+abi.MapBucketCount*uintptr(t.KeySize)+i*uintptr(t.ValueSize))
				if t.IndirectElem() {
					e = *((*unsafe.Pointer)(e))
				}
				return e, true
			}
		}
	}

	// 6. 未找到
	return unsafe.Pointer(&zeroVal[0]), false
}
```

**源码分析:**

1.  **前置检查**:
    *   如果 `map` 是 `nil` 或者 `count` 为 0，直接返回零值和 `false`。
    *   检查 `hashWriting` 标志位，防止并发读写。
2.  **计算哈希**: 与 `mapassign` 相同，计算 `key` 的哈希值。
3.  **定位桶**: 使用哈希值的低 `B` 位 `hash & bucketMask(h.B)` 来快速定位到主桶。
4.  **检查旧桶 (扩容场景)**:
    *   如果 `h.oldbuckets` 不为 `nil`，说明 `map` 正在扩容。
    *   计算出 `key` 在旧桶数组中对应的桶 `oldb`。
    *   `evacuated(oldb)` 函数通过检查旧桶的 `tophash[0]` 是否为特殊的迁移状态标志，来判断这个旧桶的数据是否已经被迁移到了新桶。
    *   如果**未迁移**，则将查找的起始桶 `b` 指向 `oldb`，即先从旧桶开始找。这是保证在渐进式扩容过程中也能正确读到数据的关键。
5.  **遍历桶链查找**:
    *   这是查找的核心循环，它会遍历主桶以及通过 `overflow` 指针连接的所有溢出桶。
    *   **`tophash` 快速过滤**: 循环内部首先比较 `tophash`，由于 `tophash` 只有 1 字节，比较成本极低。这可以迅速排除掉桶内 8 个槽位中的大部分不匹配项。
    *   **`Key.Equal` 完整比较**: 只有当 `tophash` 匹配时，才会继续调用 `key` 类型的 `Equal` 方法进行完整的、成本更高的键值比较。
    *   如果 `key` 完全匹配，则计算出对应 `value` 的地址并返回，同时 `ok` 为 `true`。
6.  **未找到**: 如果遍历完所有相关的桶链都没有找到匹配的 `key`，则返回元素类型的零值和 `false`。

## 5. `mapdelete`：删除键值对 (详细源码分析)

`delete` 操作的逻辑相对简单，它主要是在查找到元素后，对其进行清理和标记。

```go
// src/runtime/map.go

func mapdelete(t *maptype, h *hmap, key unsafe.Pointer) {
	// 1. 前置检查
	if h == nil || h.count == 0 {
		return
	}
	if h.flags&hashWriting != 0 {
		fatal("concurrent map writes")
	}

	// 2. 计算哈希并设置写标志
	hash := t.Hasher(key, uintptr(h.hash0))
	h.flags ^= hashWriting

	// 3. 定位桶并协助扩容
	bucket := hash & bucketMask(h.B)
	if h.growing() {
		growWork(t, h, bucket)
	}
	b := (*bmap)(add(h.buckets, bucket*uintptr(t.BucketSize)))
	top := tophash(hash)

search:
	// 4. 遍历桶链查找要删除的 key
	for ; b != nil; b = b.overflow(t) {
		for i := uintptr(0); i < abi.MapBucketCount; i++ {
			if b.tophash[i] != top {
				if b.tophash[i] == emptyRest {
					break search
				}
				continue
			}
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.KeySize))
			// ... (省略了间接 key 的处理)
			if !t.Key.Equal(key, k2) {
				continue
			}

			// 5. 执行删除和清理
			// 清理 key
			if t.IndirectKey() {
				*(*unsafe.Pointer)(k) = nil
			} else if t.Key.Pointers() {
				memclrHasPointers(k, t.Key.Size_)
			}
			// 清理 value
			e := add(unsafe.Pointer(b), dataOffset+abi.MapBucketCount*uintptr(t.KeySize)+i*uintptr(t.ValueSize))
			if t.IndirectElem() {
				*(*unsafe.Pointer)(e) = nil
			} else if t.Elem.Pointers() {
				memclrHasPointers(e, t.Elem.Size_)
			}

			// 6. 标记为空
			b.tophash[i] = emptyOne

			h.count--
			break search
		}
	}

	// 7. 清除写标志
	h.flags &^= hashWriting
}
```

**源码分析:**

1.  **前置检查**: 检查 `map` 是否为 `nil`、空，或存在并发写入。
2.  **计算哈希与设置标志**: 与 `mapassign` 类似，计算哈希并设置 `hashWriting` 标志。
3.  **定位桶与协助扩容**: 查找过程与 `mapaccess` 和 `mapassign` 完全一致，包括在 `map` 扩容时调用 `growWork` 协助数据迁移。
4.  **遍历查找**: 同样是遍历桶链，通过 `tophash` 和 `Key.Equal` 来精确定位要删除的元素。
5.  **执行清理**:
    *   这是 `delete` 的核心。如果找到了 `key`，它并**不是**物理上地移除这块内存。
    *   而是分别对 `key` 和 `value` 所在的内存区域进行清零。
    *   `memclrHasPointers` 函数会检查类型是否包含指针，如果包含，它会仔细地将指针置为 `nil`，以便垃圾回收器（GC）能够回收这些指针指向的对象。
6.  **标记为空**: 将该槽位的 `tophash` 值设置为 `emptyOne`。这个槽位在未来可以被新的键值对复用。
7.  **收尾**: `h.count--`，并清除 `hashWriting` 标志。

**关键点**: `delete` 操作并**不会**触发 `map` 的缩容。桶数组 `buckets` 的大小不会改变，已经分配的内存也不会被释放。这可能导致一个删除了大量元素的 `map` 仍然占用着高峰时期的内存。如果需要回收这部分内存，唯一的办法是创建一个新的 `map` 并将剩余元素复制过去。

## 6. `hashGrow` 与 `evacuate`：渐进式扩容 (详细源码分析)

`map` 的扩容机制旨在将巨大的数据迁移成本分摊到多次访问中，避免单次操作的性能抖动。

## `hashGrow`: 启动扩容

当 `mapassign` 检查到需要扩容时，会调用 `hashGrow`。

```go
// src/runtime/map.go

func hashGrow(t *maptype, h *hmap) {
	// 1. 判断扩容类型
	bigger := uint8(1)
	if !overLoadFactor(h.count+1, h.B) {
		bigger = 0
		h.flags |= sameSizeGrow
	}

	// 2. 记录旧桶，分配新桶
	oldbuckets := h.buckets
	newbuckets, nextOverflow := makeBucketArray(t, h.B+bigger, nil)

	// 3. 更新 hmap 状态，提交扩容
	h.B += bigger
	h.flags &^= iterator | oldIterator
	h.oldbuckets = oldbuckets
	h.buckets = newbuckets
	h.nevacuate = 0
	h.noverflow = 0
	// ... (处理 extra 字段中的溢出桶)
}
```

**源码分析:**

1.  **判断扩容类型**:
    *   `overLoadFactor` 再次被调用，检查是否是负载因子超标。
    *   如果是，`bigger` 为 1，表示进行 **2倍扩容** (`h.B+1`)。
    *   如果不是（说明是溢出桶过多），`bigger` 为 0，表示进行 **等量扩容** (`h.B+0`)，并设置 `sameSizeGrow` 标志。
2.  **分配新桶**: 调用 `makeBucketArray` 创建一个新的、更大的桶数组 (`newbuckets`)。
3.  **提交扩容**: 这是最关键的一步。函数更新 `hmap` 的核心指针和状态：
    *   `h.B` 更新为新的大小。
    *   `h.oldbuckets` 指向旧的桶数组。
    *   `h.buckets` 指向新分配的桶数组。
    *   `h.nevacuate` 计数器置为 0，表示迁移尚未开始。
    *   **此时，`map` 就进入了“扩容进行中”的状态**。

`hashGrow` 本身并不进行任何数据迁移，它只完成了“准备工作”。

## `evacuate`: 迁移数据

真正的数据迁移由 `evacuate` 函数完成，它在每次 `map` 写入或删除时被 `growWork` 调用。

```go
// src/runtime/map.go

func evacuate(t *maptype, h *hmap, oldbucket uintptr) {
	b := (*bmap)(add(h.oldbuckets, oldbucket*uintptr(t.BucketSize)))
	newbit := h.noldbuckets()
	if !evacuated(b) {
		// xy[0] 是低位桶，xy[1] 是高位桶
		var xy [2]evacDst
		x := &xy[0]
		x.b = (*bmap)(add(h.buckets, oldbucket*uintptr(t.BucketSize)))
		// ...
		if !h.sameSizeGrow() {
			y := &xy[1]
			y.b = (*bmap)(add(h.buckets, (oldbucket+newbit)*uintptr(t.BucketSize)))
			// ...
		}

		// 遍历旧桶和它的溢出桶
		for ; b != nil; b = b.overflow(t) {
			// ...
			for i := 0; i < abi.MapBucketCount; i++ {
				top := b.tophash[i]
				if isEmpty(top) {
					b.tophash[i] = evacuatedEmpty
					continue
				}
				// ...

				var useY uint8
				if !h.sameSizeGrow() {
					// 重新计算哈希，决定分流到 x 桶还是 y 桶
					hash := t.Hasher(k2, uintptr(h.hash0))
					if hash&newbit != 0 {
						useY = 1
					}
				}

				// 标记旧槽位为已迁移
				b.tophash[i] = evacuatedX + useY
				dst := &xy[useY] // 选择目标桶

				// ... (将 key 和 value 拷贝到目标桶 dst)
			}
		}
		// 标记旧桶为已迁移
		if h.flags&oldIterator == 0 {
			// ... (清理旧桶内存)
		}
	}
	// ...
}
```

**源码分析:**

1.  **定位新旧桶**: `evacuate` 接收一个 `oldbucket` 索引，它会定位到 `oldbuckets` 中的旧桶 `b`，以及它将要迁移到的新桶 `x` 和 `y`。在 2 倍扩容时，一个旧桶的数据会分裂到两个新桶中。
2.  **遍历旧桶**: 循环遍历旧桶 `b` 及其所有溢出桶中的 8 个槽位。
3.  **重新哈希与分流**:
    *   对于每个有效的键值对，**重新计算其哈希值**。
    *   在 2 倍扩容时，通过 `hash & newbit != 0` 判断这个键值对应该迁移到低位的新桶（`x`）还是高位的新桶（`y`）。`newbit` 是旧桶数量，恰好是新哈希值中用于区分高低桶的那一位。
    *   等量扩容时，所有元素仍在原地迁移。
4.  **拷贝数据**: 将 `key` 和 `value` 拷贝到目标新桶 `dst` 的可用槽位中。
5.  **标记状态**:
    *   将旧槽位的 `tophash` 更新为 `evacuatedX` 或 `evacuatedY`，标记它“已迁移”以及迁移的方向。
    *   将旧桶中已处理的空槽位标记为 `evacuatedEmpty`。
6.  **推进进度**: 当一个旧桶（包括其所有溢出桶）的所有键值对都迁移完毕后，`growWork` 会更新 `h.nevacuate` 计数器。当 `h.nevacuate` 等于旧桶总数时，扩容过程彻底结束，`h.oldbuckets` 被置为 `nil`。

通过这种方式，`map` 将一次性的、可能耗时很长的全量数据迁移，巧妙地分摊到了多次的读写操作中，从而保证了绝大多数操作的低延迟和高效率。

## 7. `mapiterinit` 与 `mapiternext`: Map 遍历的随机之旅

`for range` 循环遍历 `map` 的背后，是 `mapiterinit` 和 `mapiternext` 这两个函数的支撑。它们共同实现了 `map` 遍历的无序性和完整性。

### `hiter` 结构体

`hiter` 是 `map` 的迭代器结构，它在 `for range` 循环开始时被创建和初始化。它记录了遍历过程中的所有状态，确保 `map` 中的每个元素在遍历中都只被访问一次。

```go
// A map iterator.
// If a map is not nil, the iterator is initialized by calling mapiterinit.
// A map can be iterated by calling mapiternext repeatedly.
// The iterator is finished when mapiternext returns.
type hiter struct {
	key         unsafe.Pointer // Must be inlined, so key ptr can be tracked.
	elem        unsafe.Pointer // Must be inlined, so elem ptr can be tracked.
	t           *maptype
	h           *hmap
	buckets     unsafe.Pointer // bucket ptr at hash_iter initialization.
	bptr        *bmap          // current bucket.
	overflow    *[]*bmap       // keeps overflow buckets of a bucket.
	startBucket uintptr        // bucket index where the iteration started.
	offset      uint8          // intra-bucket offset where the iteration started.
	wrapped     bool           // already wrapped around from end of bucket array to beginning.
	B           uint8
	i           uint8
	bucket      uintptr
	checkBucket uintptr
}
```

**字段详解:**

*   `key`: 指向当前迭代到的 `key` 的指针。
*   `elem`: 指向当前迭代到的 `value` 的指针。
*   `t`: `map` 的类型信息。
*   `h`: 指向 `map` 的 `hmap` 头部。
*   `buckets`: 遍历开始时 `map` 的 `buckets` 指针快照。
*   `bptr`: 指向当前正在遍历的桶 `bmap`。
*   `startBucket`: 随机选择的起始桶的索引。
*   `offset`: 随机选择的起始桶内的起始槽位偏移量。
*   `wrapped`: 标记遍历是否已经从桶数组的末尾“环绕”回了开头。
*   `B`: 遍历开始时 `map` 的 `B` 值快照。
*   `i`: 当前在桶内遍历到的槽位索引。
*   `bucket`: 当前正在遍历的桶的索引。

## `mapiterinit`: 随机选择遍历起点

`mapiterinit` 的核心任务是初始化迭代器结构 `hiter`，而其中最关键的一步，就是**随机选择一个遍历的起点**。这正是 `map` 遍历无序的根源。

```go
// src/runtime/map.go

func mapiterinit(t *maptype, h *hmap, it *hiter) {
	// ... 省略非核心代码 ...

	if h == nil || h.count == 0 {
		return // 空 map 或 nil map，直接返回
	}

	// ... 省略 hiter 结构体大小检查 ...
	it.h = h

	// 快照桶的状态
	it.B = h.B
	it.buckets = h.buckets

	// 决定从哪里开始遍历 (关键代码)
	r := uintptr(rand())
	it.startBucket = r & bucketMask(h.B) // 随机选择一个起始桶
	it.offset = uint8(r >> h.B & (abi.MapBucketCount - 1)) // 随机选择起始桶内的一个起始槽位

	// 设置迭代器状态
	it.bucket = it.startBucket

	// ...

	mapiternext(it) // 调用 mapiternext 找到第一个元素
}
```

**源码分析:**

1.  **快照**: 函数首先会把当前 `map` 的 `B` 值（桶数量的对数）和 `buckets` 指针保存到 `hiter` 结构体中。这相当于创建了一个遍历开始时刻的“快照”。
2.  **随机化**:
    *   `r := uintptr(rand())`: 获取一个随机数。
    *   `it.startBucket = r & bucketMask(h.B)`: 使用随机数的低位来确定一个**随机的起始桶 `startBucket`**。
    *   `it.offset = uint8(r >> h.B & (abi.MapBucketCount - 1))`: 使用随机数的高位来确定一个**随机的起始槽位 `offset`**。
3.  **调用 `mapiternext`**: 初始化完成后，它会立即调用 `mapiternext` 来定位到第一个有效的键值对，并将其地址填充到 `hiter` 的 `key` 和 `elem` 字段中。

这种在遍历开始时就引入双重随机（随机桶、随机槽位）的设计，是 Go 团队为了**强制让开发者不要依赖 `map` 的遍历顺序**而有意为之的。

## `mapiternext`: 步进与寻路

`mapiternext` 是一个状态机，`for range` 循环每迭代一次，它就会被调用一次，其职责是基于 `hiter` 的当前状态，找到并返回下一个元素。

```go
// src/runtime/map.go

func mapiternext(it *hiter) {
	// ... 省略并发检查 ...

next:
	if b == nil { // b 是当前桶的指针
		if bucket == it.startBucket && it.wrapped {
			// 1. 终止条件：如果已经绕了一圈回到起点，遍历结束
			it.key = nil
			it.elem = nil
			return
		}
		// ... 处理扩容中途开始遍历的复杂逻辑 ...
		
        // 2. 移动到下一个桶
		b = (*bmap)(add(it.buckets, bucket*uintptr(t.BucketSize)))
		bucket++
		if bucket == bucketShift(it.B) {
			bucket = 0 // 如果到了末尾，从 0 开始
			it.wrapped = true
		}
		i = 0 // 重置桶内槽位索引
	}

	// 3. 遍历当前桶内的 8 个槽位
	for ; i < abi.MapBucketCount; i++ {
		offi := (i + it.offset) & (abi.MapBucketCount - 1) // 应用随机偏移量
		if isEmpty(b.tophash[offi]) {
			continue // 跳过空槽位
		}
		
		// ... 获取 key 和 elem 的指针 ...
		// ... 处理遍历期间 map 发生扩容的复杂逻辑 ...

		// 4. 找到一个有效元素，更新 hiter 状态并返回
		it.key = k
		it.elem = e
		it.bucket = bucket
		it.bptr = b
		it.i = i + 1 // 下次从下一个槽位开始
		return
	}

	// 5. 当前桶遍历完毕，检查溢出桶
	b = b.overflow(t)
	i = 0
	goto next // 继续处理溢出桶
}
```

**遍历流程总结:**

1.  **起始**: 从 `mapiterinit` 确定的随机 `startBucket` 和随机 `offset` 开始。
2.  **桶内遍历**: 在一个桶内，从 `offset` 开始依次检查 8 个槽位。如果槽位非空，则找到一个元素，`mapiternext` 保存当前进度并返回。
3.  **遍历溢出桶**: 如果一个桶的 8 个槽位都检查完了，函数会通过 `overflow` 指针跳转到它的溢出桶，继续在溢出桶中进行第 2 步。
4.  **遍历下一个桶**: 当一个桶和它的所有溢出桶链都遍历完毕后，函数会递增桶的序号 `bucket`，移动到下一个主桶，重复第 2、3 步。
5.  **循环回到起点**: 如果 `bucket` 走到了桶数组的末尾，它会“环绕”到数组的开头（`bucket = 0`），继续遍历，直到再次回到 `startBucket`。
6.  **终止**: 当 `bucket` 再次等于 `startBucket` 并且已经环绕过一次（`it.wrapped == true`）时，说明所有桶都已被访问，遍历结束。

这个过程保证了 `map` 中的每个元素在遍历中都**只会被访问一次**。同时，由于复杂的扩容检查逻辑，即使在遍历期间 `map` 发生了增删和扩容，遍历行为也能得到正确的处理。

---

## 8. Go Map 面试常见问题及参考答案

### 1. 基础概念

n
n
*   **Go 的 `map` 是线程安全的吗？**

    *   **回答要点**: 不是。Go 的内建 `map` 类型并非线程安全。如果多个 Goroutine 同时对一个 `map` 进行读写操作，而不加任何同步控制，会导致未定义的行为，通常会直接 `panic` 并抛出 "concurrent map read and map write" 或 "concurrent map writes" 错误。这是因为 `map` 的内部操作（如扩容、读写）包含多个步骤，在并发环境下可能会导致其内部状态不一致。如果需要并发访问，应该使用 `sync.RWMutex` 进行显式加锁，或者使用 `sync.Map`。

n
n
*   **为什么 `map` 的遍历是无序的？它的遍历流程是怎样的？**

    *   **回答要点**: `map` 遍历的无序性是 Go 语言特意设计的，其根源在于遍历的**随机化初始化**和**固定的遍历寻路**。

        1.  **随机化起点**: 当 `for range` 开始时，运行时的 `mapiterinit` 函数会获取一个随机数，并用这个随机数来决定从哪个桶（bucket）以及桶内的哪个槽位（cell）开始遍历。

        2.  **固定的寻路**: 从这个随机起点开始，遍历过程 `mapiternext` 会遵循一个固定的路径：
            *   首先遍历当前桶内的所有 8 个槽位。
            *   然后顺着溢出桶链表，遍历所有溢出桶。
            *   当一个桶链都遍历完后，移动到**序号相邻的下一个主桶**，重复上述过程。
            *   当所有主桶都遍历完后，遍历结束。

        3.  **设计意图**: 正是由于起点的随机性，导致了每次 `for range` 的结果顺序都可能不同。Go 团队这样设计的目的就是为了**强制开发者不要依赖任何特定的遍历顺序**，从而避免写出在不同 Go 版本或不同运行条件下表现不一致的脆弱代码。

### 2. 内部实现

n
n
*   **简述 `map` 的底层数据结构。**

    *   **回答要点**: `map` 的核心是一个 `hmap` 结构体，它指向一个由 `bmap`（桶）组成的数组。

        *   `hmap`: 包含 `map` 的元信息，如元素数量 `count`、桶数量的对数 `B`、哈希种子 `hash0`，以及指向当前桶数组 `buckets` 和旧桶数组 `oldbuckets`（扩容时使用）的指针。

        *   `bmap`: 每个桶最多能存 8 个键值对。它包含一个 `tophash` 数组（存储 key 哈希值的高 8 位，用于快速查找）、8 个 key、8 个 value，以及一个指向下一个溢出桶的指针。当一个桶存满后，会通过链表的方式连接一个溢出桶。

n
n
*   **`map` 是如何查找一个 key 的？**

    *   **回答要点**:

        1.  **计算哈希**: 使用哈希种子 `hash0` 计算 key 的哈希值。

        2.  **定位桶**: 使用哈希值的低 `B` 位来确定 key 所在的桶。

        3.  **检查旧桶**: 如果 `map` 正在扩容，会检查这个 key 是否还在旧桶 (`oldbuckets`) 中，如果是，则在旧桶中查找。

        4.  **比较 `tophash`**: 在桶内，首先遍历 `tophash` 数组，快速比较哈希值的高 8 位，以过滤掉大部分不匹配的 key。

        5.  **比较 `key`**: 如果 `tophash` 匹配，再完整地比较 `key` 的值是否相等。

        6.  **遍历溢出桶**: 如果在当前桶没找到，就顺着溢出桶链表继续查找，重复 4、5 步。

        7.  **返回结果**: 找到则返回 value 和 `true`，否则返回零值和 `false`。

n
n
*   **`map` 的扩容机制是怎样的？**

    *   **回答要点**: `map` 的扩容是**渐进式**的，避免了性能抖动。

        1.  **触发条件**:

            *   **负载因子超标**: 当 `元素数量 / 桶数量 > 6.5` 时，触发 **2倍扩容**，桶数量翻倍。

            *   **溢出桶过多**: 当溢出桶数量约等于桶数量时，触发 **等量扩容**，桶数量不变，目的是整理数据分布，减少溢出桶，优化访问速度。

        2.  **渐进式迁移**:

            *   扩容时，并不会立即迁移所有数据。而是分配新的桶数组 (`buckets`)，并将旧的桶数组地址存入 `oldbuckets`。

            *   真正的数据迁移发生在后续的每一次 `map` 写入或删除操作中。每次这类操作都会顺带迁移一到两个旧桶的数据到新桶中。

            *   当所有旧桶的数据都迁移完成后，`oldbuckets` 会被置为 `nil`，扩容结束。

### 3. 常见问题与陷阱

n
n
*   **向 `map` 中插入一个元素，然后 `delete` 掉，`map` 会释放内存吗？**

    *   **回答要点**: 不会。`delete` 操作只会将对应的键值对标记为“已删除”（将 `tophash` 设为 `emptyOne`），并将 `map` 的 `count` 减一。但 `map` 底层分配的桶数组内存**不会被释放**，也不会触发缩容。这可能导致 `map` 占用的内存看起来比实际存储的元素要多得多。如果需要释放内存，唯一的办法是重新创建一个新的 `map`，并将需要的元素拷贝过去。

n
n
*   **如何实现一个线程安全的 `map`？**

    *   **回答要点**:

        1.  **使用 `sync.RWMutex`**: 这是最直接的方法。为 `map` 配备一个读写锁。读取操作前加读锁 (`RLock`)，写入（增、删、改）操作前加写锁 (`Lock`)。

        2.  **使用 `sync.Map`**: Go 1.9 引入的 `sync.Map` 是专门为“读多写少”场景优化的并发 `map`。它通过 `read` 和 `dirty` 两个 `map` 以及原子操作，实现了在很多情况下无锁读取，性能远高于使用 `RWMutex` 的 `map`。但它的接口和内建 `map` 不同，且不适用于写操作频繁的场景。

n
n
*   **`map` 的 key 可以是哪些类型？为什么 `slice` 不能作为 `map` 的 key？**

    *   **回答要点**: `map` 的 `key` 必须是**可比较的**（comparable）类型。这意味着该类型的值可以用 `==` 和 `!=` 进行比较。常见的可比较类型包括布尔型、数字、字符串、指针、Channel、接口，以及只包含可比较字段的数组和结构体。

    *   `slice`、`map` 和 `function` 类型是不可比较的，因此不能作为 `map` 的 `key`。`slice` 不可比较的根本原因在于，它的值不是固定的。两个 `slice` 即使包含相同的元素，它们也可能指向不同的底层数组，或者有不同的长度/容量，`==` 操作的语义会变得模糊不清且难以定义。如果非要用 `slice` 作为 `key`，通常的做法是将 `slice` 转换为一个字符串（例如通过 `string(slice)` 或 `fmt.Sprintf`）作为 `map` 的 `key`。
