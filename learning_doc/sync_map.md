# Go 源码学习笔记：sync.Map

`sync.Map` 是 Go 语言标准库 `sync` 包提供的一个并发安全的 `map` 实现。它旨在解决在特定场景下，使用 `map` 加 `sync.RWMutex` 可能存在的性能瓶颈。

## 1. 设计哲学与适用场景

`sync.Map` 的设计目标是针对**“读多写少”**或**“键不重叠的并发写入”**这两种常见场景进行优化，以减少锁竞争。

*   **读多写少**: 如果对某个键的访问以读取为主，并且写入频率较低，`sync.Map` 可以提供接近无锁读取的性能。
*   **键不重叠的并发写入**: 如果不同的 Goroutine 倾向于写入不同的键，`sync.Map` 也能表现良好。

在其他情况下，例如写入频繁或键重叠严重的并发写入，使用 `map` 配合 `sync.RWMutex` 可能会有更好的性能，并且类型安全方面也更具优势（`sync.Map` 使用 `any` 类型，需要类型断言）。

## 2. 核心数据结构

`sync.Map` 的内部结构是其高效并发性能的关键。它主要由一个读写锁 `mu` 和两个 `map` 组成：`read` 和 `dirty`。

```go
type Map struct {
	mu Mutex // 用于保护 dirty 字段以及 read 字段的更新

	// read 包含 map 中可以安全地并发访问的部分内容（无论是否持有 mu 锁）。
	// read 字段本身总是可以安全地加载，但只能在持有 mu 锁的情况下存储。
	// read 中存储的 entry 可以并发更新而不持有 mu 锁，但更新一个之前被 expunged 的 entry 需要
	// 在持有 mu 锁的情况下将其拷贝到 dirty map 并 unexpunge。
	read atomic.Pointer[readOnly]

	// dirty 包含 map 中需要持有 mu 锁才能访问的部分内容。
	// 为了确保 dirty map 可以快速提升为 read map，它也包含了 read map 中所有未被 expunged 的 entry。
	// expunged 的 entry 不会存储在 dirty map 中。
	// clean map 中被 expunged 的 entry 必须在存储新值之前被 unexpunge 并添加到 dirty map。
	// 如果 dirty map 为 nil，下次写入时将通过浅拷贝 clean map（并忽略过时 entry）来初始化它。
	dirty map[any]*entry

	// misses 统计自上次 read map 更新以来，因需要锁定 mu 才能确定键是否存在而导致的加载失败次数。
	// 一旦 misses 达到足够覆盖拷贝 dirty map 的成本，dirty map 将被提升为 read map，
	// 并且下次存储时会创建新的 dirty 拷贝。
	misses int
}

// readOnly 是一个不可变结构体，原子存储在 Map.read 字段中。
type readOnly struct {
	m       map[any]*entry
	amended bool // 如果 dirty map 包含 read map 中不存在的某些键，则为 true。
}

// entry 是 map 中与特定键对应的槽位。
type entry struct {
	// p 指向为该 entry 存储的 interface{} 值。
	//
	// 如果 p == nil，该 entry 已被删除，且 m.dirty == nil 或 m.dirty[key] 就是 e。
	//
	// 如果 p == expunged，该 entry 已被删除，m.dirty != nil，且该 entry 不存在于 m.dirty 中。
	//
	// 否则，该 entry 有效，并记录在 m.read.m[key] 和（如果 m.dirty != nil）m.dirty[key] 中。
	//
	// 通过原子替换为 nil 可以删除一个 entry：当下一次创建 m.dirty 时，它会原子地将 nil 替换为 expunged，
	// 并使 m.dirty[key] 未设置。
	//
	// 一个 entry 关联的值可以通过原子替换进行更新，前提是 p != expunged。如果 p == expunged，
	// 只有在首先设置 m.dirty[key] = e 之后，才能更新其关联的值，以便使用 dirty map 的查找能找到该 entry。
	p atomic.Pointer[any]
}

// expunged 是一个任意指针，用于标记已从 dirty map 中删除的 entry。
var expunged = new(any)
```

**字段解析**:

*   **`mu sync.Mutex`**: 一个普通的互斥锁，用于保护 `dirty` 字段的读写，以及 `read` 字段的原子指针更新。
*   **`read atomic.Pointer[readOnly]`**: 一个原子指针，指向一个 `readOnly` 结构体。`readOnly` 内部包含一个普通的 `map[any]*entry`。**这是实现无锁读的关键**。读取操作会优先从 `read` 字段指向的 `map` 中获取数据，这部分操作是无锁且高效的。
*   **`dirty map[any]*entry`**: 一个普通的 `map`，用于存储所有最近写入/修改的键值对。这个 `map` 的访问需要持有 `mu` 锁。当 `dirty` map 为 `nil` 时，表示它是“干净”的，所有有效数据都在 `read` map 中。
*   **`misses int`**: 计数器，记录了从 `read` map 中未能找到键，转而需要加锁访问 `dirty` map 的次数。当 `misses` 达到一定阈值时，`dirty` map 会被“提升”为新的 `read` map。
*   **`expunged`**: 一个特殊的哨兵值 (`new(any)`)，用于标记 `read` map 中已被逻辑删除但尚未从 `read` map 中移除的 `entry`。

## 3. 方法源码及详细注释

### 3.1. `Load(key any) (value any, ok bool)` - 读取操作

```go
// Load 返回 map 中指定键的值。如果键不存在，则返回 nil。
// ok 结果表示是否在 map 中找到了值。
func (m *Map) Load(key any) (value any, ok bool) {
	// 【快速路径】
	// 原子地加载 readOnly 结构体，此操作是无锁的。
	read := m.loadReadOnly()
	e, ok := read.m[key]

	// 如果在 read.m 中没有找到，并且 read.amended 为 true
	// (这暗示 dirty map 中可能存在新添加的键)，则进入慢速路径。
	if !ok && read.amended {
		m.mu.Lock() // 【慢速路径】加锁
		// 再次原子加载 readOnly，因为在等待锁的过程中，
		// dirty map 可能已经被提升为了新的 read map。
		read = m.loadReadOnly()
		e, ok = read.m[key]
		// 如果在新的 read map 中还是没找到，并且它仍然是 amended 状态，
		// 那么就去 dirty map 中查找。
		if !ok && read.amended {
			e, ok = m.dirty[key]
			// 无论是否在 dirty map 中找到，都记录一次“未命中”(miss)。
			// 这会增加 misses 计数，当 misses 足够多时会触发 dirty map 的提升。
			m.missLocked()
		}
		m.mu.Unlock() // 解锁
	}
	// 如果最终都没有找到对应的 entry，直接返回。
	if !ok {
		return nil, false
	}
	// 如果找到了 entry，调用其 load 方法原子地加载值。
	return e.load()
}

// load 从 entry 中原子地加载值。
func (e *entry) load() (value any, ok bool) {
	// 原子加载指针 p
	p := e.p.Load()
	// 如果指针为 nil 或等于 expunged 哨兵值，说明该 entry 已被删除。
	if p == nil || p == expunged {
		return nil, false
	}
	// 否则，解引用指针返回实际的值。
	return *p, true
}
```
**逻辑总结**: `Load` 方法完美体现了读写分离的思想。它首先无锁地尝试从 `read` map 读取，这是最高效的路径。如果失败且 `dirty` map 可能有新数据，则加锁进入慢速路径，查找 `dirty` map，并记录一次 `miss`，为后续的性能优化（提升 `dirty` map）提供数据。


### 3.2. `Store(key, value any)` 和 `Swap(key, value any) (previous any, loaded bool)` - 写入/交换操作

`Store` 只是 `Swap` 的一个简单封装，核心逻辑在 `Swap` 中，它原子地更新一个键的值并返回旧值。

```go
// Store 设置一个键的值。
func (m *Map) Store(key, value any) {
	// 直接调用 Swap，忽略返回值。
	_, _ = m.Swap(key, value)
}

// Swap 交换一个键的值并返回旧值。
// loaded 结果表示在此次调用之前键是否存在。
func (m *Map) Swap(key, value any) (previous any, loaded bool) {
	read := m.loadReadOnly()
	// 【快速路径】
	// 如果键存在于 read map 中...
	if e, ok := read.m[key]; ok {
		// ...尝试无锁地原子交换 entry 的值指针。
		if v, ok := e.trySwap(&value); ok {
			// 如果交换成功 (entry 未被 expunged)...
			if v == nil {
				return nil, false // 旧值是 nil，说明之前不存在或已被删除。
			}
			return *v, true // 返回旧值。
		}
	}

	// 【慢速路径】
	m.mu.Lock()
	read = m.loadReadOnly() // 再次检查 read map
	if e, ok := read.m[key]; ok {
		// 如果 entry 之前被标记为删除 (expunged)...
		if e.unexpungeLocked() {
			// ...在 dirty map 中“复活”它。
			m.dirty[key] = e
		}
		// 在持有锁的情况下，直接交换值。
		if v := e.swapLocked(&value); v != nil {
			loaded = true
			previous = *v
		}
	} else if e, ok := m.dirty[key]; ok { // 如果键在 dirty map 中...
		// ...直接在 dirty map 的 entry 上交换值。
		if v := e.swapLocked(&value); v != nil {
			loaded = true
			previous = *v
		}
	} else { // 如果是全新的键
		if !read.amended {
			// 这是第一次向 dirty map 添加新键。
			// 初始化 dirty map，并将 read map 标记为 "amended"。
			m.dirtyLocked()
			m.read.Store(&readOnly{m: read.m, amended: true})
		}
		// 在 dirty map 中创建新的 entry。
		m.dirty[key] = newEntry(value)
	}
	m.mu.Unlock()
	return previous, loaded
}
```
**逻辑总结**: 写入操作会优先尝试在无锁的情况下更新 `read` map 中已存在的 `entry`。如果失败（例如 `entry` 是新键或已被删除），则必须加锁，并在 `dirty` map 上完成操作。如果是全新的键，还会触发 `dirty` map 的创建和 `read` map 的 `amended` 状态更新。

### 3.3. `LoadOrStore(key, value any) (actual any, loaded bool)`

这是一个非常有用的原子操作：如果键存在，就加载并返回旧值；如果不存在，就存储新值并返回新值。

```go
// LoadOrStore 如果键存在，则返回现有值。否则，它存储并返回给定的值。
// loaded 结果在加载了值时为 true，在存储了值时为 false。
func (m *Map) LoadOrStore(key, value any) (actual any, loaded bool) {
	// 【快速路径】
	read := m.loadReadOnly()
	if e, ok := read.m[key]; ok {
		// 尝试在无锁情况下加载或存储。
		actual, loaded, ok := e.tryLoadOrStore(value)
		if ok { // 如果 entry 未被 expunged，操作成功。
			return actual, loaded
		}
	}

	// 【慢速路径】
	m.mu.Lock()
	read = m.loadReadOnly()
	if e, ok := read.m[key]; ok {
		if e.unexpungeLocked() { // 如果 entry 在 read map 但被删除了
			m.dirty[key] = e // 在 dirty map 中复活它
		}
		actual, loaded, _ = e.tryLoadOrStore(value)
	} else if e, ok := m.dirty[key]; ok { // 如果 entry 在 dirty map 中
		actual, loaded, _ = e.tryLoadOrStore(value)
		m.missLocked() // 记录一次 miss
	} else { // 全新的键
		if !read.amended {
			// 初始化 dirty map 并标记 read map
			m.dirtyLocked()
			m.read.Store(&readOnly{m: read.m, amended: true})
		}
		m.dirty[key] = newEntry(value)
		actual, loaded = value, false
	}
	m.mu.Unlock()

	return actual, loaded
}
```
**逻辑总结**: `LoadOrStore` 的逻辑与 `Store` 非常相似，都是优先走无锁快速路径。`e.tryLoadOrStore` 会原子地检查 `entry` 的值指针：如果非 `nil`，则加载；如果为 `nil`，则尝试用新值进行 `CompareAndSwap`。如果快速路径失败，则进入加锁的慢速路径，在 `dirty` map 上执行相同的逻辑。


### 3.4. `Delete(key any)` 和 `LoadAndDelete(key any)`

`Delete` 只是 `LoadAndDelete` 的简单封装。删除操作的核心是将 `entry` 的值指针 `p` 原子地设置为 `nil` (逻辑删除)，或者直接从 `dirty` map 中删除 (物理删除)。

```go
// LoadAndDelete 删除一个键的值，并返回之前的值（如果存在）。
// loaded 结果报告该键是否存在。
func (m *Map) LoadAndDelete(key any) (value any, loaded bool) {
	// 【快速路径】 尝试在 read map 中查找
	read := m.loadReadOnly()
	e, ok := read.m[key]
	// 如果 read map 中没有，且 dirty map 可能有新数据，进入慢速路径
	if !ok && read.amended {
		m.mu.Lock() // 【慢速路径】
		read = m.loadReadOnly()
		e, ok = read.m[key]
		if !ok && read.amended {
			e, ok = m.dirty[key]
			// 直接从 dirty map 中物理删除
			delete(m.dirty, key)
			m.missLocked()
		}
		m.mu.Unlock()
	}
	// 如果找到了 entry，对其执行逻辑删除
	if ok {
		return e.delete()
	}
	return nil, false
}

// delete 原子地将 entry 的值指针设置为 nil，并返回旧值。
func (e *entry) delete() (value any, ok bool) {
	for {
		p := e.p.Load()
		// 如果已经被删除了，直接返回。
		if p == nil || p == expunged {
			return nil, false
		}
		// 通过 CAS 操作将指针交换为 nil。
		if e.p.CompareAndSwap(p, nil) {
			return *p, true
		}
	}
}
```
**逻辑总结**:
*   **逻辑删除**: `Delete` 并不直接从 `map` 中移除 `entry`。它只是通过 `e.delete()` 将 `entry` 的值指针 `p` 原子地设置为 `nil`。
*   **物理删除**:
    *   如果在 `dirty` map 中找到了 `key`，则会直接通过 `delete(m.dirty, key)` 进行**物理删除**。
    *   对于 `read` map 中被逻辑删除的 `entry` (即 `p` 为 `nil`)，它会在下一次 `dirty` map 初始化 (`dirtyLocked` 被调用) 时，被标记为 `expunged` 并且不会被拷贝到新的 `dirty` map 中，从而最终被垃圾回收，实现**延迟的物理删除**。

### 3.5. `Range(f func(key, value any) bool)` - 遍历操作

`Range` 旨在提供一个相对高效的遍历方式，但它不保证快照一致性。

```go
// Range 对 map 中存在的每个键和值顺序调用 f。
// 如果 f 返回 false，则停止迭代。
func (m *Map) Range(f func(key, value any) bool) {
	// 原子加载 read map。
	read := m.loadReadOnly()
	// 【关键行为】如果 dirty map 中有新数据...
	if read.amended {
		m.mu.Lock()
		read = m.loadReadOnly()
		if read.amended {
			// ...立即将 dirty map 提升为新的 read map。
			read = readOnly{m: m.dirty}
			copyRead := read
			m.read.Store(&copyRead)
			// 清空 dirty map 和 misses 计数。
			m.dirty = nil
			m.misses = 0
		}
		m.mu.Unlock()
	}

	// 遍历提升后的 read map。
	for k, e := range read.m {
		// 加载 entry 的值。
		v, ok := e.load()
		// 跳过已删除的 entry。
		if !ok {
			continue
		}
		// 调用用户传入的函数 f，如果 f 返回 false，则中断遍历。
		if !f(k, v) {
			break
		}
	}
}
```
**逻辑总结**:
1.  **提升 `dirty` map**: `Range` 的一个有趣行为是，如果它发现 `dirty` map 中有 `read` map 所没有的数据 (`read.amended == true`)，它会**立即触发一次提升**。这使得 `Range` 总是能遍历到调用时刻所有存在的键，但代价是可能需要一次加锁和 map 拷贝。
2.  **遍历**: `Range` 始终遍历 `read.m`。它会加载每个 `entry` 的值，并跳过那些已被删除的 (`ok == false`)。

这个设计保证了 `Range` 不会遗漏任何键，但也明确说明了它不提供一致性快照。在遍历期间，其他 Goroutine 对 `Map` 的修改可能会也可能不会被观察到。

---

## 4. 总结与使用建议

*   `sync.Map` 适用于**读多写少**或**键空间不重叠的并发写入**场景，旨在通过减少锁竞争来提升性能。
*   它的核心思想是**读写分离**：`read` map 提供快速的无锁读取路径，`dirty` map 负责处理所有写入，并在达到一定条件时提升为 `read` map。
*   通过 `misses` 计数器和 `dirty` map 的提升机制，`sync.Map` 能够适应工作负载的变化，将频繁访问的键保持在 `read` map 中。
*   它使用 `atomic.Pointer` 实现了无锁的 `read` map 切换和 `entry` 值更新。
*   `sync.Map` 不是类型安全的，需要配合类型断言。
*   `sync.Map` 对象在首次使用后**不能被复制**。
*   在非上述优化场景下，传统的 `map` 加 `sync.RWMutex` 组合通常是更好的选择，因为它更直观，并且提供了类型安全。

理解 `sync.Map` 的内部工作原理，可以帮助我们更好地判断何时选择它，以及如何避免潜在的问题。
