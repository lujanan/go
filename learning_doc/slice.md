# Go 源码学习笔记：slice.go

## 2025-11-02: 初识 Slice 源码

与 `context` 和 `chan` 不同，`slice` 的核心实现与 Go 的运行时和编译器结合得更紧密。其大部分运行时逻辑位于 `src/runtime/slice.go` 文件中。这个文件包含了 `slice` 创建、扩容等关键操作的底层实现。

### 1. `slice` 结构体：切片头

在 Go 的运行时中，一个切片由 `slice` 结构体表示，它也被称为“切片头” (slice header)。

```go
type slice struct {
	array unsafe.Pointer // 指向底层数组的指针
	len   int              // 切片的长度
	cap   int              // 切片的容量
}
```

这个结构清晰地揭示了切片的本质：它是一个对底层数组的引用，加上长度和容量两个属性。我们对切片的所有操作，实际上都是通过这个结构体来间接操作底层数组。

### 2. `makeslice`：创建切片

当我们使用 `make([]T, len, cap)` 语法创建一个切片时，编译器会将其转换为对 `makeslice` 函数的调用。

```go
//go:linkname makeslice
func makeslice(et *_type, len, cap int) unsafe.Pointer {
	// 根据元素大小和容量计算所需的内存大小
	mem, overflow := math.MulUintptr(et.Size_, uintptr(cap))

	// 进行一系列安全检查，确保 len 和 cap 的值是合法的
	// 1. cap * 元素大小是否导致溢出
	// 2. 分配的内存是否超过最大限制 (maxAlloc)
	// 3. len 是否为负数或大于 cap
	if overflow || mem > maxAlloc || len < 0 || len > cap {
		// 为了提供更明确的错误信息，这里会进行更细致的检查
		// 比如 make([]T, bignumber) 应该报 len out of range 而不是 cap out of range
		mem, overflow := math.MulUintptr(et.Size_, uintptr(len))
		if overflow || mem > maxAlloc || len < 0 {
			panicmakeslicelen() // 抛出 len 越界 panic
		}
		panicmakeslicecap() // 抛出 cap 越界 panic
	}

	// 调用内存分配函数 mallocgc 来分配底层数组的内存
	// 第三个参数 true 表示需要清零内存，这对于包含指针的类型是必须的
	return mallocgc(mem, et, true)
}
```

**核心逻辑:**

1.  **安全检查**: 检查传入的 `len` 和 `cap` 是否合法（例如 `len > cap` 或 `cap` 超出最大内存限制）。
2.  **内存计算**: 根据元素类型 `et` 的大小和容量 `cap`，计算出底层数组所需的总内存大小 (`mem = et.Size_ * uintptr(cap)`)。
3.  **内存分配**: 调用 `mallocgc` 函数来分配一块连续的内存作为底层数组。`mallocgc` 是 Go 的内存分配器，它会负责垃圾回收相关的处理。
4.  **返回指针**: 返回指向新分配内存的指针，这个指针将成为新切片 `slice` 结构体中的 `array` 字段。

### 3. `growslice`：切片扩容

`growslice` 是 `slice` 实现中最核心、最复杂的函数。当我们向一个切片 `append` 元素，并且其容量不足时，Go 运行时就会调用此函数来获取一个更大的新切片。

```go
//go:linkname growslice
func growslice(oldPtr unsafe.Pointer, newLen, oldCap, num int, et *_type) slice {
	// 旧的长度是新长度减去要添加的元素数量
	oldLen := newLen - num

	// 如果新长度为负数，说明发生了溢出，抛出 panic
	if newLen < 0 {
		panic(errorString("growslice: len out of range"))
	}

	// 如果元素大小为 0，直接返回一个指向 zerobase 的切片
	// zerobase 是一个虚拟的零大小内存地址
	if et.Size_ == 0 {
		return slice{unsafe.Pointer(&zerobase), newLen, newLen}
	}

	// 调用 nextslicecap 计算新的容量
	newcap := nextslicecap(newLen, oldCap)

	// ... (省略了针对不同元素大小进行内存计算和优化的代码)
	// 这部分代码会根据元素大小是否为1、指针大小、2的幂等情况，
	// 使用不同的方式计算内存大小，以提高效率。

	// 计算新旧切片所需的内存大小
	var capmem uintptr
	capmem, overflow = math.MulUintptr(et.Size_, uintptr(newcap))

	// 检查新容量是否导致溢出或超过最大内存限制
	if overflow || capmem > maxAlloc {
		panic(errorString("growslice: len out of range"))
	}

	// 分配新的底层数组内存
	var p unsafe.Pointer
	if !et.Pointers() {
		// 如果元素不包含指针，分配的内存不需要清零
		p = mallocgc(capmem, nil, false)
	} else {
		// 如果元素包含指针，分配的内存必须清零，以避免 GC 扫描到野指针
		p = mallocgc(capmem, et, true)
	}

	// 将旧底层数组的数据拷贝到新的底层数组
	// lenmem 是旧切片实际占用的内存大小
	lenmem := uintptr(oldLen) * et.Size_
	memmove(p, oldPtr, lenmem)

	// 返回一个新的 slice 结构体，包含了新的指针、长度和容量
	return slice{p, newLen, newcap}
}
```

**核心逻辑:**

1.  **计算新容量**: 调用 `nextslicecap` 函数来确定新的、更合适的容量。
2.  **内存分配**: 调用 `mallocgc` 分配一块具有新容量的内存块，作为新的底层数组。
3.  **数据迁移**: 使用 `memmove` 函数将旧底层数组中的所有元素高效地复制到新的底层数组中。
4.  **返回新切片**: 函数最后返回一个新的 `slice` 结构体，其 `array` 指向新分配的内存，`len` 更新为 `append` 后的长度，`cap` 更新为新的容量。

### 4. `nextslicecap`：扩容策略

`growslice` 内部调用的 `nextslicecap` 函数定义了 Go 中著名的切片扩容策略，目的是在性能和内存使用之间取得平衡。

```go
func nextslicecap(newLen, oldCap int) int {
	newcap := oldCap
	doublecap := newcap + newcap

	// 如果 append 后的新长度比旧容量的两倍还大，
	// 那么新容量就直接使用这个新长度。
	if newLen > doublecap {
		return newLen
	}

	// 定义一个阈值，用于区分小切片和大切片
	const threshold = 256

	// 如果旧容量小于 256，新容量直接翻倍
	if oldCap < threshold {
		return doublecap
	}

	// 当容量大于等于 256 时，进入循环，以一个较小的增长因子进行扩容
	for {
		// 增长因子大约是 1.25，即 newcap += newcap / 4
		// (newcap + 3*threshold) >> 2 是一个更高效的计算方式
		newcap += (newcap + 3*threshold) >> 2

		// 如果计算出的新容量已经足够，或者发生了溢出（变为负数），则退出循环
		if uint(newcap) >= uint(newLen) {
			break
		}
	}

	// 如果因为溢出导致 newcap 变为负数，那么直接使用 newLen 作为新容量
	if newcap <= 0 {
		return newLen
	}
	return newcap
}
```

**策略细节:**

*   **小切片 (容量 < 256)**: 如果旧的容量小于 256，新的容量会直接翻倍。例如，一个容量为 10 的切片扩容后，容量会变为 20。
*   **大切片 (容量 >= 256)**: 当切片变大后，每次都翻倍会造成巨大的内存浪费。因此，此时扩容策略会变得更加保守。它会进入一个循环，每次增加大约 **25%** (即 `newcap += (newcap + 3*threshold) >> 2`，近似于 `newcap *= 1.25`)，直到新容量足够容纳 `append` 后的所有元素。
*   **容量保证**: 无论采用哪种策略，`nextslicecap` 保证返回的新容量一定足以容纳 `append` 后的新长度 (`newLen`)。

### 5. `slicecopy`：复制切片

内置的 `copy()` 函数的底层实现是 `slicecopy` 函数。

**核心逻辑:**

1.  **确定复制数量**: 取源切片和目标切片的长度的最小值，作为实际要复制的元素数量 `n`。
2.  **内存移动**: 使用 `memmove` 函数将源切片底层数组的数据复制到目标切片的底层数组。`memmove` 能够正确处理源和目标内存区域可能发生重叠的情况。
3.  **返回数量**: 返回实际复制的元素数量 `n`。

### 总结

`slice.go` 的源码揭示了切片高效且灵活的本质。它通过一个简单的 `slice` 结构体，配合 `makeslice`、`growslice` 和 `slicecopy` 等运行时函数，为 Go 开发者提供了强大而易用的动态数组功能。理解其扩容策略对于编写高性能的 Go 代码至关重要。

---

## 其他关键知识点

### 1. `append` 必须赋值回原变量

`append` 函数可能会也可能不会导致底层数组的重新分配。
- **容量足够**: `append` 直接在原底层数组上修改数据，返回的切片头虽然 `len` 变了，但 `array` 指针不变。
- **容量不足**: `append` 会调用 `growslice` 分配新数组，并将数据拷贝过去。返回的切片头会有一个全新的 `array` 指针。

由于无法确定是否会发生扩容，**必须总是将 `append` 的结果赋值回给原切片变量**，否则可能导致 bug 或数据丢失。

```go
// 正确做法
slice = append(slice, elem)

// 错误做法 (如果发生扩容，slice 变量将指向旧的、未更新的数组)
append(slice, elem)
```

### 2. 共享底层数组

通过切片表达式 `s2 := s1[start:end]` 创建的新切片 `s2`，与原切片 `s1` **共享同一个底层数组**。

- 对 `s2` 中元素的修改，会影响到 `s1`。
- `s2` 的容量 `cap` 是从 `s2` 的起始位置到 `s1` 底层数组的末尾。

```go
s1 := []int{10, 20, 30, 40, 50}
s2 := s1[1:3] // s2 = {20, 30}, len=2, cap=4 (指向 s1 数组的 20, 30, 40, 50)

s2[0] = 99 // 修改 s2 的元素
fmt.Println(s1) // 输出: [10 99 30 40 50]，s1 也被改变了
```

### 3. 切片导致的“内存泄漏”

这是一个非常经典的场景。如果你从一个非常大的切片中，截取一小段作为新的切片，那么只要这一小段切片还在使用，**整个大的底层数组就无法被垃圾回收器（GC）回收**。

```go
func getFirstTwoElements() []int {
    bigSlice := make([]int, 10000) // 分配一个很大的数组
    // ... 对 bigSlice 进行一些操作 ...
    return bigSlice[0:2] // 返回一个只包含前两个元素的小切片
}

// 在 main 函数中
smallSlice := getFirstTwoElements()
// 此时 smallSlice 虽然 len=2, cap=2，但它背后引用着一个包含 10000 个 int 的巨大数组。
// 只要 smallSlice 存在，这个大数组就无法被回收。
```

**解决方法**：使用 `copy` 函数创建一个新的、独立的切片，断开与旧的大数组的联系。

```go
func getFirstTwoElementsFixed() []int {
    bigSlice := make([]int, 10000)
    // ...
    
    result := make([]int, 2) // 创建一个大小刚好的新切片
    copy(result, bigSlice[0:2]) // 只拷贝需要的数据
    return result // 返回这个新的、独立的切片
}
```

### 4. Nil 切片 vs 空切片

- **Nil 切片**: `var s []int`。它的值为 `nil`，切片头三个字段都是零值（指针为 `nil`, len=0, cap=0）。
- **空切片**: `s := make([]int, 0)` 或 `s := []int{}`。它是一个非 `nil` 值，切片头的指针指向一个有效的、但大小为零的内存地址，len=0, cap=0。

在实践中，它们大部分行为一致。`len()` 和 `cap()` 都返回 0，并且都可以直接用于 `append`。通常推荐使用 `nil` 切片作为未初始化切片的零值。

```go
var nilSlice []int
emptySlice := make([]int, 0)

fmt.Println(nilSlice == nil)      // true
fmt.Println(emptySlice == nil)    // false
fmt.Println(len(nilSlice), len(emptySlice)) // 0 0

// 两者都可以正常 append
nilSlice = append(nilSlice, 1)
emptySlice = append(emptySlice, 1)
```

## 常见面试题

### 1. 传递切片给函数，函数内部的修改会影响外部吗？

**回答**: 会，也不会，取决于修改的类型。

- **修改元素**: 如果函数内部通过索引修改切片的元素 (`s[i] = ...`)，由于函数内外两个切片头指向同一个底层数组，所以外部切片是可见的。
- **Append 操作**: 如果函数内部 `append` **没有超出其容量**，那么修改的也是同一个底层数组，外部可见。但如果 `append` **导致了扩容**，函数内部的切片会指向一个新的底层数组，此后的修改将与外部切片无关。

**关键点**: Go 中所有参数都是值传递。对于切片，意味着函数接收的是“切片头”的一个副本。但副本里的指针和外部切片头的指针，指向的是同一个底层数组。

### 2. 下面这段代码的输出是什么？请解释。

```go
func main() {
    s1 := []int{1, 2, 3, 4, 5}
    s2 := s1[1:3] // len=2, cap=4, s2={2, 3}

    s2 = append(s2, 6) // 未扩容, s1 变为 {1, 2, 3, 6, 5}
    fmt.Println("s1:", s1)
    fmt.Println("s2:", s2)

    s2 = append(s2, 7) // 未扩容, s1 变为 {1, 2, 3, 6, 7}
    fmt.Println("s1:", s1)
    fmt.Println("s2:", s2)

    s2 = append(s2, 8) // 超出容量，s2 发生扩容，指向新数组
    fmt.Println("s1:", s1)
    fmt.Println("s2:", s2)

    s2[0] = 99 // 只修改 s2 的新数组
    fmt.Println("s1:", s1)
    fmt.Println("s2:", s2)
}
```

**回答**:
```
s1: [1 2 3 6 5]
s2: [2 3 6]
s1: [1 2 3 6 7]
s2: [2 3 6 7]
s1: [1 2 3 6 7]
s2: [2 3 6 7 8]
s1: [1 2 3 6 7]
s2: [99 3 6 7 8]
```
**解释**:
1.  `s2` 初始时与 `s1` 共享数组，`cap` 为 4。
2.  前两次 `append` 没有超过 `s2` 的容量，所以直接修改了 `s1` 的底层数组。
3.  第三次 `append` 使得 `s2` 的 `len` 变为 5，超过了 `cap` 4，于是 `s2` 发生了扩容，拥有了新的底层数组。
4.  此后对 `s2` 的修改，不再影响 `s1`。

### 3. 如何正确地从一个大切片中截取一小部分，并避免内存泄漏？

**回答**: 使用 `copy` 函数。创建一个目标大小的新切片，然后将大切片中需要的数据拷贝过来。这样新的切片就拥有自己独立的底层数组，大切片关联的数组在不再被引用后就可以被 GC 回收。

```go
// 错误方式
small := bigSlice[0:10]

// 正确方式
small := make([]int, 10)
copy(small, bigSlice[0:10])
```

### 4. Go 切片的扩容策略是怎样的？

**回答**:
1.  如果期望容量 `newCap` 大于当前容量 `cap` 的两倍，则新容量 `newCap` 就直接是期望容量。
2.  如果当前 `cap` 小于 256，则新容量是旧容量的 2 倍。
3.  如果当前 `cap` 大于等于 256，则新容量以一个较小的因子（大约 1.25 倍）持续增长，直到满足期望容量。
这个策略旨在平衡内存使用和分配次数。