# Go 源码学习笔记：并发模型 (Concurrency)

## 2025-11-18: 进程、线程、协程与 Goroutine

理解并发（Concurrency）和并行（Parallelism）是现代软件开发的基础。Go 语言以其简洁而强大的并发模型著称，其核心就是 Goroutine。为了深入理解 Goroutine，我们首先需要厘清几个基本概念：进程、线程、协程。

### 1. 概念定义

#### a. 进程 (Process)

**进程是操作系统进行资源分配和调度的基本单位。**

可以将其想象成一个正在运行的程序实例。当你双击打开一个应用程序（如 Chrome 浏览器）时，操作系统就创建了一个或多个进程。

*   **资源独立**: 每个进程都拥有自己独立的内存空间（地址空间）、文件句柄、设备等。一个进程无法直接访问另一个进程的内存。
*   **进程间通信 (IPC)**: 由于内存隔离，进程间的通信必须通过操作系统提供的机制，如管道（Pipe）、信号（Signal）、套接字（Socket）等。这种通信方式相对复杂且开销较大。
*   **开销**: 创建和销毁进程的开销很大，因为操作系统需要为其分配和回收独立的资源。进程间的上下文切换（Context Switch）开销也很大，需要保存和恢复大量的状态。

#### b. 线程 (Thread)

**线程是操作系统能够进行运算调度的最小单位。它被包含在进程之中，是进程中的实际运作单位。**

一个进程可以包含一个或多个线程，这些线程共享该进程的资源。

*   **资源共享**: 同一进程内的所有线程共享该进程的内存地址空间、文件句柄等资源。这使得线程间的通信变得非常简单和高效（例如，通过读写全局变量）。
*   **独立执行流**: 每个线程拥有自己独立的程序计数器（PC）、寄存器和栈（Stack）。这保证了每个线程可以独立执行不同的代码路径。
*   **开销**: 线程的创建、销毁和上下文切换的开销比进程小得多，但仍然需要操作系统的介入，属于内核级别的操作，因此仍有不可忽视的成本。

#### c. 协程 (Coroutine)

**协程是一种用户态的、轻量级的线程。它的调度完全由用户（即应用程序）控制，操作系统对此一无所知。**

协程也被称为“用户态线程”或“纤程”（Fiber）。

*   **协作式调度 (Cooperative Scheduling)**: 与线程的抢占式调度（由操作系统决定谁运行、何时切换）不同，协程需要**主动让出**（yield）CPU 的控制权，其他协程才能获得执行机会。如果一个协程陷入长时间计算而不主动让出，会导致其他协程“饿死”。
*   **极低开销**: 协程的创建、销毁和切换完全在用户空间完成，无需进入内核态。因此，其开销极低，可以轻松创建成千上万个。
*   **栈空间**: 协程的栈空间通常很小，并且可以根据需要进行调整。

#### d. Goroutine

**Goroutine 是 Go 语言实现的、内置的、强大的协程。**

可以认为 Goroutine 是 Go 语言对协程概念的工程化、标准化实现，并在此基础上做了很多优化和封装。

*   **Go 运行时调度**: Goroutine 由 Go 的运行时调度器（Runtime Scheduler）进行管理，而不是操作系统。调度器采用了一种称为 **M:N 调度**的模型，即将 M 个 Goroutine 调度到 N 个操作系统线程上执行（M 通常远大于 N）。
*   **抢占式调度**: 与传统的协作式协程不同，Go 1.14 版本之后，调度器引入了**基于信号的异步抢占**机制。这意味着如果一个 Goroutine 长时间占用 CPU（例如进行密集的计算），Go 运行时会自动中断它，让其他 Goroutine 有机会运行。这避免了“饿死”问题。
*   **极小栈空间**: Goroutine 的初始栈空间非常小（约 2KB），并且可以根据需要动态增长和收缩。这使得创建大量 Goroutine 成为可能。
*   **内置通信**: Go 提供了 `channel` 作为 Goroutine 之间通信的主要方式，鼓励“通过通信来共享内存”，而不是“通过共享内存来通信”，这使得编写并发程序更安全、更简单。

### 2. 关系与比较

#### 层次关系

一个形象的比喻：
*   **进程**：一个工厂，拥有独立的资源（电力、土地、原材料）。
*   **线程**：工厂里的工人，共享工厂的资源，但有自己的工具箱和任务清单（栈和寄存器）。
*   **Goroutine**：工人们（线程）需要完成的具体任务。一个工人可以在不同的任务之间快速切换。Go 的调度器就是工头，负责给工人们分派任务。

**结构图**:
```
一个操作系统
├── 进程 A
│   ├── 线程 1 -> 运行 Goroutine G1, G3
│   └── 线程 2 -> 运行 Goroutine G2, G4, G5
├── 进程 B
│   └── 线程 1 -> 运行 Goroutine G6
└── ...
```

#### 核心差异总结

| 特性 | 进程 (Process) | 线程 (Thread) | 协程 (Coroutine) | Goroutine |
| :--- | :--- | :--- | :--- | :--- |
| **定义** | 资源分配的基本单位 | OS 调度的最小单位 | 用户态的轻量级线程 | Go 实现的协程 |
| **调度者** | 操作系统 | 操作系统 | 用户/应用程序 | Go 运行时 |
| **调度方式** | 抢占式 | 抢占式 | 协作式 (通常) | 协作与抢占结合 |
| **资源** | 独立内存空间 | 共享进程内存 | 共享线程/进程内存 | 共享线程/进程内存 |
| **创建/切换开销** | 非常大 | 较大 (需内核态) | 极小 (用户态) | 极小 (用户态) |
| **栈大小** | 大 | 较大 (通常 1MB+) | 小，可自定义 | 很小 (约 2KB)，可动态增长 |
| **数量级** | 十/百 | 百/千 | 十万/百万 | 十万/百万 |
| **通信方式** | IPC (管道, Socket) | 共享内存, 锁 | 语言/库提供 | Channel, 共享内存 |

### 3. 为什么 Goroutine 如此高效？

1.  **用户态调度**: 避免了频繁的内核态和用户态之间的切换，这是最大的性能优势来源。
2.  **动态小栈**: 使得创建大量 Goroutine 成为可能，每个 Goroutine 只在需要时才占用更多内存。
3.  **M:N 调度模型**: Go 运行时将 Goroutine 动态地绑定到少数几个 OS 线程上。如果一个 Goroutine 因为系统调用（如文件 I/O）而阻塞，调度器会将该线程上的其他 Goroutine 迁移到另一个未阻塞的线程上继续执行，从而最大化 CPU 利用率。
4.  **work-stealing 算法**: 调度器内部实现了工作窃取算法。当某个线程上的 Goroutine 队列为空时，它会从其他线程的队列中“窃取”一些任务来执行，进一步保证了负载均衡。

通过 Goroutine 和 Channel，Go 语言将复杂的并发编程模型极大地简化，让开发者可以轻松编写出高性能、高并发的应用程序。

### 4. 使用 Goroutine 和 Channel 进行并发编程

Go 语言通过 Goroutine 和 Channel，提供了一种简洁、高效且安全的并发编程模型。这种模型被称为 **CSP (Communicating Sequential Processes)**，其核心思想是“不要通过共享内存来通信，而是通过通信来共享内存”。

#### a. 启动 Goroutine

使用 `go` 关键字即可轻松启动一个 Goroutine。这会在一个新的 Goroutine 中执行一个函数或方法。

```go
package main

import (
	"fmt"
	"time"
)

func sayHello() {
	fmt.Println("Hello from a Goroutine!")
}

func main() {
	go sayHello() // 启动一个新的 Goroutine 执行 sayHello 函数
	fmt.Println("Hello from main Goroutine!")
	// 主 Goroutine 退出时，所有子 Goroutine 也会被终止。
	// 为了确保 sayHello 有机会执行，需要等待一段时间。
	time.Sleep(100 * time.Millisecond)
}
```

#### b. Channel：Goroutine 间的通信

Channel 是 Goroutine 之间进行通信的管道。它们是带有类型的，只能传递指定类型的数据。

*   **创建 Channel**:
    `make(chan T, capacity)`
    *   `T`: Channel 中传输的数据类型。
    *   `capacity`: Channel 的容量。
        *   `capacity = 0` (默认): **无缓冲 Channel**。发送和接收操作会阻塞，直到另一端就绪。
        *   `capacity > 0`: **有缓冲 Channel**。发送操作在 Channel 未满时不会阻塞；接收操作在 Channel 非空时不会阻塞。

```go
// 创建一个无缓冲的 int 类型 Channel
ch := make(chan int)

// 创建一个容量为 5 的 string 类型 Channel
bufferedCh := make(chan string, 5)
```

*   **发送和接收数据**:
    *   发送: `ch <- value`
    *   接收: `value := <-ch` 或 `<-ch` (丢弃接收到的值)

```go
package main

import (
	"fmt"
	"time"
)

func producer(ch chan<- int) { // ch chan<- int 表示这是一个只能发送的 Channel
	for i := 0; i < 5; i++ {
		fmt.Printf("Producer: Sending %d\n", i)
		ch <- i // 发送数据到 Channel
		time.Sleep(50 * time.Millisecond)
	}
	close(ch) // 发送方关闭 Channel
}

func consumer(ch <-chan int) { // ch <-chan int 表示这是一个只能接收的 Channel
	for num := range ch { // 使用 for-range 循环从 Channel 接收数据，直到它被关闭
		fmt.Printf("Consumer: Received %d\n", num)
	}
	fmt.Println("Consumer: Channel closed.")
}

func main() {
	ch := make(chan int) // 无缓冲 Channel
	go producer(ch)
	go consumer(ch)
	time.Sleep(time.Second) // 等待 producer 和 consumer 完成
}
```

*   **关闭 Channel**:
    *   发送方可以关闭 Channel，表示不会再有数据发送。接收方会收到一个“零值”和 `ok=false`。
    *   关闭已关闭的 Channel 会引发 `panic`。向已关闭的 Channel 发送数据也会引发 `panic`。

```go
val, ok := <-ch // ok 为 true 表示成功接收到数据，false 表示 Channel 已关闭且没有更多数据
if !ok {
    fmt.Println("Channel is closed and drained.")
}
```

#### c. 常见的并发模式

##### i. `sync.WaitGroup`：等待多个 Goroutine 完成

当需要等待一组 Goroutine 全部执行完毕后再继续主 Goroutine 的执行时，`sync.WaitGroup` 是一个非常有用的工具。

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

func worker(id int, wg *sync.WaitGroup) {
	defer wg.Done() // Goroutine 完成时调用 Done()
	fmt.Printf("Worker %d starting\n", id)
	time.Sleep(time.Duration(id) * 100 * time.Millisecond)
	fmt.Printf("Worker %d finished\n", id)
}

func main() {
	var wg sync.WaitGroup
	for i := 1; i <= 3; i++ {
		wg.Add(1) // 每启动一个 Goroutine，计数器加 1
		go worker(i, &wg)
	}
	wg.Wait() // 等待所有 Goroutine 完成
	fmt.Println("All workers finished.")
}
```

##### ii. Select 语句：多路复用

`select` 语句允许 Goroutine 等待多个 Channel 操作。它会阻塞直到其中一个 case 可以执行。

```go
package main

import (
	"fmt"
	"time"
)

func main() {
	ch1 := make(chan string)
	ch2 := make(chan string)

	go func() {
		time.Sleep(1 * time.Second)
		ch1 <- "one"
	}()

	go func() {
		time.Sleep(2 * time.Second)
		ch2 <- "two"
	}()

	for i := 0; i < 2; i++ {
		select {
		case msg1 := <-ch1:
			fmt.Println("Received:", msg1)
		case msg2 := <-ch2:
			fmt.Println("Received:", msg2)
		case <-time.After(3 * time.Second): // 超时处理
			fmt.Println("Timeout!")
			return
		default: // 非阻塞模式，如果没有 Channel 就绪会立即执行
			// fmt.Println("No channel ready, doing other work...")
			// time.Sleep(50 * time.Millisecond) // 防止忙等待
		}
	}
	fmt.Println("Done processing messages.")
}
```

##### iii. Worker Pool：并发处理任务

Worker Pool 是一种通过预先创建固定数量的 Goroutine（worker）来处理一组任务的并发模式。这有助于控制并发量、复用资源。

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

func labor(id int, jobs <-chan int, results chan<- string) {
	for job := range jobs {
		fmt.Printf("Worker %d processing job %d\n", id, job)
		time.Sleep(time.Duration(job) * 100 * time.Millisecond) // 模拟工作
		results <- fmt.Sprintf("Worker %d finished job %d", id, job)
	}
}

func main() {
	const numJobs = 9
	const numWorkers = 3

	jobs := make(chan int, numJobs)
	results := make(chan string, numJobs)

	// 启动 worker Goroutines
	var wg sync.WaitGroup
	for w := 1; w <= numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			labor(workerID, jobs, results)
		}(w)
	}

	// 发送任务
	for j := 1; j <= numJobs; j++ {
		jobs <- j
	}
	close(jobs) // 所有任务发送完毕，关闭 jobs Channel

	// 等待所有 worker 完成任务
	go func() {
		wg.Wait()
		close(results) // 所有结果产出完毕，关闭 results Channel
	}()

	// 收集结果
	for res := range results {
		fmt.Println(res)
	}
	fmt.Println("All jobs processed and results collected.")
}
```

##### iv. `context` 包：管理 Goroutine 生命周期

`context` 包（`context.Context`）提供了一种在 Goroutine 之间传播请求范围的值、取消信号和截止日期的机制。它对于控制复杂 Goroutine 树的生命周期至关重要。

*   `context.WithCancel`: 创建一个可取消的 Context。
*   `context.WithTimeout`: 创建一个在指定时间后自动取消的 Context。
*   `context.WithDeadline`: 创建一个在指定时间点自动取消的 Context。
*   `context.WithValue`: 创建一个可以携带请求范围值的 Context。

```go
package main

import (
	"context"
	"fmt"
	"time"
)

func doWork(ctx context.Context, workerID int) {
	for {
		select {
		case <-ctx.Done(): // 监听取消信号
			fmt.Printf("Worker %d: received cancellation signal. Exiting.\n", workerID)
			return
		default:
			fmt.Printf("Worker %d: doing work...\n", workerID)
			time.Sleep(500 * time.Millisecond)
		}
	}
}

func main() {
	// 创建一个可取消的 Context
	ctx, cancel := context.WithCancel(context.Background())

	// 启动多个 worker Goroutine
	for i := 1; i <= 3; i++ {
		go doWork(ctx, i)
	}

	// 主 Goroutine 等待一段时间
	time.Sleep(2 * time.Second)

	// 发送取消信号，通知所有 worker 停止
	fmt.Println("Main: sending cancellation signal...")
	cancel()

	// 等待 Goroutine 优雅退出
	time.Sleep(1 * time.Second)
	fmt.Println("Main: program finished.")
}
```

#### d. 其他同步原语 (Sync Primitives)

尽管 Go 鼓励使用 Channel 进行通信，但 `sync` 包也提供了传统的并发原语，用于需要共享内存的场景。

*   **`sync.Mutex` / `sync.RWMutex`**: 互斥锁，用于保护共享资源，确保同一时间只有一个 Goroutine 访问临界区。
*   **`sync.Cond`**: 条件变量，用于Goroutine在特定条件满足时进行等待和唤醒。
*   **`sync.Once`**: 确保某个操作只执行一次。
*   **`sync.Pool`**: 对象池，用于复用对象，减少垃圾回收压力。

**最佳实践**: 在 Go 中，优先考虑使用 Channel 来协调 Goroutine 和共享数据。只有当 Channel 不适用（例如，需要实现某种锁机制或对性能有极高要求）时，才考虑使用 `sync` 包中的原语。

### 5. Goroutine 泄露 (Goroutine Leaks)

Goroutine 非常轻量，但如果不加管理，它们仍然会消耗内存和 CPU 资源。当一个 Goroutine 在其生命周期结束后无法正常退出，被永久地阻塞在某个操作上，而垃圾回收器（GC）又无法回收它时，就发生了 **Goroutine 泄露**。

#### a. 什么是 Goroutine 泄露？

一个 Goroutine 启动后，必须有一个明确的退出路径。如果因为逻辑错误，导致 Goroutine 被永久地阻塞在某个 Channel 操作、`select` 语句或锁上，它占用的栈内存（初始为 2KB，但可能增长）将永远无法被释放。随着时间的推移，泄露的 Goroutine 数量不断累积，最终可能耗尽系统内存，导致程序崩溃。

#### b. 常见的泄露原因

##### i. Channel 阻塞 (最常见)

这是最常见的泄露原因。一个 Goroutine 尝试对 Channel 进行读或写操作，但另一端永远不会就绪。

**示例：只有发送方，没有接收方**

```go
package main

import (
	"fmt"
	"time"
)

func leakyOperation() {
	ch := make(chan int) // 无缓冲 Channel

	go func() {
		fmt.Println("Goroutine: Waiting to send data...")
		ch <- 10 // 阻塞在这里，因为没有接收方
		fmt.Println("Goroutine: Data sent!") // 这行代码永远不会执行
	}()

	// leakyOperation 函数返回了，但它启动的 Goroutine 却永远阻塞了
	fmt.Println("leakyOperation: Returning...")
}

func main() {
	leakyOperation()
	// 此时，一个 Goroutine 已经泄露
	time.Sleep(2 * time.Second)
	fmt.Println("Main: Program finished.")
}
```
在上面的例子中，子 Goroutine 尝试向一个无缓冲的 Channel 发送数据，但主 Goroutine (`leakyOperation`) 在启动它之后就直接返回了，没有任何代码来接收 `ch` 中的数据。因此，这个子 Goroutine 将被永久阻塞。

**其他 Channel 阻塞场景**:
*   **等待接收但无人发送**: 一个 Goroutine 尝试从 Channel 接收数据 (`<-ch`)，但所有其他 Goroutine 都已经退出，并且没有人关闭该 Channel。
*   **nil Channel**: 对一个 `nil` 的 Channel 进行读或写操作，会永久阻塞。

##### ii. `select` 语句死锁

`select` 语句中的所有 `case` 都无法执行，也没有 `default` 分支。

```go
func selectLeak() {
    ch1 := make(chan int)
    ch2 := make(chan int)
    
    select {
    case <-ch1:
        // ch1 永远不会有数据
    case ch2 <- 1:
        // ch2 永远不会有接收方
    }
}
```

##### iii. 未正确使用 `context`

当使用 `context` 控制 Goroutine 生命周期时，必须在所有可能阻塞的操作中检查 `ctx.Done()`。

```go
func contextLeak(ctx context.Context) {
    ch := make(chan int)
    
    // 这是一个耗时操作，但它只在 select 语句中检查了 context
    select {
    case <-time.After(10 * time.Second):
        // 模拟长时间工作
    case <-ctx.Done():
        return
    }
    
    // 如果在上面的 select 之后，context 被取消了，
    // 下面的操作仍然会阻塞，导致泄露。
    ch <- 1 // 如果没有接收方，这里会永久阻塞
}
```

#### c. 如何分析和定位泄露

Go 语言提供了强大的 `pprof` 工具来诊断运行时问题，包括 Goroutine 泄露。

##### i. 使用 `net/http/pprof` 进行实时分析

这是最简单、最直观的方法。只需在你的应用中匿名导入 `net/http/pprof` 包。

```go
import (
	_ "net/http/pprof"
	"net/http"
	"log"
)

func main() {
    // ... 你的应用逻辑 ...
    
    // 启动一个 HTTP 服务来暴露 pprof 端点
	go func() {
		log.Println(http.ListenAndServe("localhost:6060", nil))
	}()

    // ...
}
```

1.  **启动应用**后，访问 `http://localhost:6060/debug/pprof/`。
2.  点击 **goroutine** 链接，或者直接访问 `http://localhost:6060/debug/pprof/goroutine?debug=2`。
3.  这个页面会显示当前所有 Goroutine 的数量、状态以及它们的完整**调用栈**。

**分析技巧**:
*   **观察数量**: 如果 Goroutine 数量随着时间推移只增不减，那么很可能存在泄露。
*   **查找可疑调用栈**: 在 `debug=2` 的输出中，寻找大量处于 `chan receive`、`chan send` 或 `select` 状态，并且调用栈相似的 Goroutine。这些通常就是泄露的源头。

对于上面的泄露示例，你会在 `pprof` 输出中看到类似这样的信息：
```
1 @ 0x43ed0c 0x450931 0x4508d2 0x4c3c41
#	0x450930	runtime.chansend1+0xb0	/home/go/src/go/src/runtime/chan.go:137
#	0x4508d1	runtime.chansend+0x21	/home/go/src/go/src/runtime/chan.go:117
#	0x4c3c40	main.leakyOperation.func1+0x40	/path/to/your/main.go:13
```
这清晰地指出了有一个 Goroutine 在 `main.go` 的第 13 行（`ch <- 10`）被阻塞了。

##### ii. 使用 `go tool pprof`

`go tool pprof` 可以连接到正在运行的应用或分析离线采集的 profile 文件。

```bash
# 连接到正在运行的应用
go tool pprof http://localhost:6060/debug/pprof/goroutine
```

进入 `pprof` 交互式命令行后，可以使用以下命令：
*   `top`: 显示最耗费资源的函数（对于 Goroutine profile，显示 Goroutine 数量最多的函数）。
*   `list <function_name>`: 显示特定函数的源码，并标注出资源消耗点。
*   `traces`: 显示所有 Goroutine 的调用栈。

##### iii. 在测试中自动检测 (`goleak`)

`goleak` 是一个非常流行的第三方库，可以在单元测试结束时检查是否有未清理的 Goroutine，从而在开发阶段就发现泄露问题。

**使用示例**:
```go
import (
    "testing"
    "go.uber.org/goleak"
)

func TestMyLeakyFunction(t *testing.T) {
    // 在测试函数开始时调用 VerifyNone
    // 它会记录当前的 Goroutine 状态，并在测试结束时进行对比
    defer goleak.VerifyNone(t)

    // ... 调用你怀疑可能泄露的函数 ...
    leakyOperation()
}
```
如果测试结束后发现有新的 Goroutine 残留，`goleak` 会让测试失败并打印出泄露的 Goroutine 的调用栈。

#### d. 预防泄露的最佳实践

1.  **明确退出点**: 为每一个启动的 Goroutine 设计一个清晰、可达的退出路径。
2.  **使用 `context`**: 对于复杂的、可能长时间运行的 Goroutine，使用 `context.Context` 来传递取消信号。
3.  **`select` 中包含 `default` 或 `ctx.Done()`**: 在 `select` 语句中，如果 Channel 操作可能阻塞，应包含一个 `default` 分支（用于非阻塞轮询）或一个 `<-ctx.Done()` 分支（用于响应取消信号）。
4.  **正确关闭 Channel**: 遵循“发送方负责关闭 Channel”的原则。不要让接收方等待一个永远不会被关闭的 Channel。
5.  **使用 `goleak`**: 将 `goleak` 集成到你的测试套件中，实现自动化泄露检测。
