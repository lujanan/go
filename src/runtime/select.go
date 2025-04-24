// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// This file contains the implementation of Go select statements.

import (
	"internal/abi"
	"unsafe"
)

const debugSelect = false

// Select case descriptor.
// Known to compiler.
// Changes here must also be made in src/cmd/compile/internal/walk/select.go's scasetype.
type scase struct {
	c    *hchan         // chan
	elem unsafe.Pointer // data element
}

var (
	// channel send operation program counter
	// channel发送操作的程序计数器
	chansendpc = abi.FuncPCABIInternal(chansend)
	// channel receive operation program counter
	// channel接收操作的程序计数器
	chanrecvpc = abi.FuncPCABIInternal(chanrecv)
)

// selectsetpc records the program counter at the point of a select call.
// selectsetpc记录select调用点的程序计数器
func selectsetpc(pc *uintptr) {
	*pc = getcallerpc()
}

// sellock locks all channels involved in the select in the proper order.
// sellock按照正确的顺序锁定select中涉及的所有channel
func sellock(scases []scase, lockorder []uint16) {
	var c *hchan
	for _, o := range lockorder {
		c0 := scases[o].c
		// 只在遇到新的channel时才加锁
		// 避免对同一个channel重复加锁
		if c0 != c {
			c = c0
			lock(&c.lock)
		}
	}
}
func selunlock(scases []scase, lockorder []uint16) {
	// We must be very careful here to not touch sel after we have unlocked
	// the last lock, because sel can be freed right after the last unlock.
	// Consider the following situation.
	// First M calls runtime·park() in runtime·selectgo() passing the sel.
	// Once runtime·park() has unlocked the last lock, another M makes
	// the G that calls select runnable again and schedules it for execution.
	// When the G runs on another M, it locks all the locks and frees sel.
	// Now if the first M touches sel, it will access freed memory.
	//
	// 在解锁最后一个锁之后我们必须非常小心不能访问sel,因为sel可能在最后一个锁解锁后立即被释放。
	// 考虑以下情况:
	// 第一个M在runtime·selectgo()中调用runtime·park()并传递sel。
	// 一旦runtime·park()解锁了最后一个锁,另一个M会使调用select的G再次可运行并调度执行。
	// 当G在另一个M上运行时,它会锁定所有锁并释放sel。
	// 此时如果第一个M访问sel,它将访问已释放的内存。
	for i := len(lockorder) - 1; i >= 0; i-- {
		c := scases[lockorder[i]].c
		if i > 0 && c == scases[lockorder[i-1]].c {
			continue // will unlock it on the next iteration
			// 将在下一次迭代中解锁它
		}
		unlock(&c.lock)
	}
}
func selparkcommit(gp *g, _ unsafe.Pointer) bool {
	// There are unlocked sudogs that point into gp's stack. Stack
	// copying must lock the channels of those sudogs.
	// Set activeStackChans here instead of before we try parking
	// because we could self-deadlock in stack growth on a
	// channel lock.
	// 有未锁定的sudogs指向gp的栈。栈复制必须锁定这些sudogs的channel。
	// 在这里设置activeStackChans而不是在尝试停放之前设置,
	// 因为在channel锁上的栈增长可能导致自我死锁。
	gp.activeStackChans = true

	// Mark that it's safe for stack shrinking to occur now,
	// because any thread acquiring this G's stack for shrinking
	// is guaranteed to observe activeStackChans after this store.
	// 标记现在可以安全地进行栈收缩,
	// 因为任何获取此G的栈进行收缩的线程都保证在此存储之后观察到activeStackChans。
	gp.parkingOnChan.Store(false)

	// Make sure we unlock after setting activeStackChans and
	// unsetting parkingOnChan. The moment we unlock any of the
	// channel locks we risk gp getting readied by a channel operation
	// and so gp could continue running before everything before the
	// unlock is visible (even to gp itself).
	// 确保在设置activeStackChans和取消设置parkingOnChan之后再解锁。
	// 一旦我们解锁任何channel锁,就有可能因channel操作而使gp准备就绪,
	// 因此gp可能在解锁之前的所有操作可见之前就继续运行(即使对gp本身也是如此)。

	// This must not access gp's stack (see gopark). In
	// particular, it must not access the *hselect. That's okay,
	// because by the time this is called, gp.waiting has all
	// channels in lock order.
	// 这里不能访问gp的栈(参见gopark)。
	// 特别是不能访问*hselect。这没问题,
	// 因为在调用此函数时,gp.waiting已经按锁定顺序包含了所有channel。
	var lastc *hchan
	for sg := gp.waiting; sg != nil; sg = sg.waitlink {
		if sg.c != lastc && lastc != nil {
			// As soon as we unlock the channel, fields in
			// any sudog with that channel may change,
			// including c and waitlink. Since multiple
			// sudogs may have the same channel, we unlock
			// only after we've passed the last instance
			// of a channel.
			// 一旦我们解锁channel,具有该channel的任何sudog中的字段都可能改变,
			// 包括c和waitlink。由于多个sudog可能具有相同的channel,
			// 我们只在遍历完某个channel的最后一个实例后才解锁它。
			unlock(&lastc.lock)
		}
		lastc = sg.c
	}
	if lastc != nil {
		unlock(&lastc.lock)
	}
	return true
}

func block() {
	gopark(nil, nil, waitReasonSelectNoCases, traceBlockForever, 1) // forever
}

// selectgo 实现了 select 语句的功能
//
// cas0 指向一个类型为 [ncases]scase 的数组,order0 指向一个类型为 [2*ncases]uint16 的数组,
// 其中 ncases 必须小于等于 65536。这两个数组都存储在 goroutine 的栈上
// (无论 selectgo 中是否发生逃逸)。
//
// 在启用竞态检测时,pc0 指向一个类型为 [ncases]uintptr 的数组(也在栈上);
// 在其他构建中,pc0 被设置为 nil。
//
// selectgo 返回被选中的 scase 的索引,该索引与其对应的 select{recv,send,default} 调用的
// 序号位置相匹配。此外,如果选中的 scase 是一个接收操作,它还会报告是否接收到了值。
func selectgo(cas0 *scase, order0 *uint16, pc0 *uintptr, nsends, nrecvs int, block bool) (int, bool) {
	if debugSelect {
		print("select: cas0=", cas0, "\n")
	}

	// NOTE: In order to maintain a lean stack size, the number of scases
	// is capped at 65536.
	// 注意：为了保持精简的栈大小，scases 的数量上限为 65536
	cas1 := (*[1 << 16]scase)(unsafe.Pointer(cas0))
	order1 := (*[1 << 17]uint16)(unsafe.Pointer(order0))

	// NOTE: pollorder/lockorder's underlying array was not zero-initialized by compiler.
	// 注意：pollorder/lockorder 的底层数组未被编译器初始化为零
	ncases := nsends + nrecvs                    // 计算总的 case 数量
	scases := cas1[:ncases:ncases]               // 从 cas1 中切片出所有的 case
	pollorder := order1[:ncases:ncases]          // 从 order1 中切片出轮询顺序数组
	lockorder := order1[ncases:][:ncases:ncases] // 从 order1 中切片出加锁顺序数组
	// NOTE: pollorder/lockorder's underlying array was not zero-initialized by compiler.
	// 注意：pollorder/lockorder 的底层数组未被编译器初始化为零

	// Even when raceenabled is true, there might be select
	// statements in packages compiled without -race (e.g.,
	// ensureSigM in runtime/signal_unix.go).
	// 即使启用了竞态检测，在一些没有 -race 编译选项的包中仍可能存在 select 语句
	// (例如 runtime/signal_unix.go 中的 ensureSigM)
	var pcs []uintptr
	if raceenabled && pc0 != nil {
		pc1 := (*[1 << 16]uintptr)(unsafe.Pointer(pc0))
		pcs = pc1[:ncases:ncases]
	}
	casePC := func(casi int) uintptr {
		if pcs == nil {
			return 0
		}
		return pcs[casi]
	}

	var t0 int64
	if blockprofilerate > 0 {
		t0 = cputicks()
	}

	// The compiler rewrites selects that statically have
	// only 0 or 1 cases plus default into simpler constructs.
	// The only way we can end up with such small sel.ncase
	// values here is for a larger select in which most channels
	// have been nilled out. The general code handles those
	// cases correctly, and they are rare enough not to bother
	// optimizing (and needing to test).
	// 编译器会将静态只有0或1个case加上default的select重写为更简单的结构。
	// 在这里出现如此小的sel.ncase值的唯一可能是在一个较大的select中,
	// 大多数channel都被置为nil。通用代码可以正确处理这些情况,
	// 而且这种情况很少见,不值得优化(也不需要测试)。

	// generate permuted order
	// 生成随机排列顺序
	norder := 0
	for i := range scases {
		cas := &scases[i]

		// Omit cases without channels from the poll and lock orders.
		// 从轮询和锁定顺序中忽略没有channel的case
		if cas.c == nil {
			cas.elem = nil // allow GC // 允许垃圾回收
			continue
		}

		// 如果channel有定时器,可能需要运行定时器
		if cas.c.timer != nil {
			cas.c.timer.maybeRunChan()
		}

		// 使用随机数生成轮询顺序
		j := cheaprandn(uint32(norder + 1))
		pollorder[norder] = pollorder[j]
		pollorder[j] = uint16(i)
		norder++
	}
	// 截取实际有效的轮询和锁定顺序数组
	pollorder = pollorder[:norder]
	lockorder = lockorder[:norder]
	// sort the cases by Hchan address to get the locking order.
	// simple heap sort, to guarantee n log n time and constant stack footprint.
	// 根据Hchan地址对case进行排序以获得锁定顺序
	// 使用简单的堆排序,保证n log n时间复杂度和常量栈空间

	// 第一阶段:构建最大堆
	for i := range lockorder {
		j := i
		// Start with the pollorder to permute cases on the same channel.
		// 从pollorder开始,对同一个channel的case进行排列
		c := scases[pollorder[i]].c
		// 向上调整,将当前元素放到合适的位置
		for j > 0 && scases[lockorder[(j-1)/2]].c.sortkey() < c.sortkey() {
			k := (j - 1) / 2
			lockorder[j] = lockorder[k]
			j = k
		}
		lockorder[j] = pollorder[i]
	}

	// 第二阶段:堆排序
	for i := len(lockorder) - 1; i >= 0; i-- {
		o := lockorder[i]
		c := scases[o].c
		// 将堆顶元素(最大值)与末尾元素交换
		lockorder[i] = lockorder[0]
		j := 0
		// 向下调整堆,维护最大堆性质
		for {
			k := j*2 + 1 // 左子节点
			if k >= i {
				break
			}
			// 如果右子节点存在且大于左子节点,选择右子节点
			if k+1 < i && scases[lockorder[k]].c.sortkey() < scases[lockorder[k+1]].c.sortkey() {
				k++
			}
			// 如果当前节点小于子节点,继续向下调整
			if c.sortkey() < scases[lockorder[k]].c.sortkey() {
				lockorder[j] = lockorder[k]
				j = k
				continue
			}
			break
		}
		lockorder[j] = o
	}

	// 调试模式:验证排序结果是否正确
	if debugSelect {
		for i := 0; i+1 < len(lockorder); i++ {
			if scases[lockorder[i]].c.sortkey() > scases[lockorder[i+1]].c.sortkey() {
				print("i=", i, " x=", lockorder[i], " y=", lockorder[i+1], "\n")
				throw("select: broken sort")
			}
		}
	}
	// lock all the channels involved in the select
	// 锁定select中涉及的所有channel
	sellock(scases, lockorder)

	var (
		gp     *g             // 当前goroutine指针
		sg     *sudog         // sudog结构体指针,用于channel等待队列
		c      *hchan         // channel指针
		k      *scase         // select case指针
		sglist *sudog         // sudog链表头指针
		sgnext *sudog         // 下一个sudog指针
		qp     unsafe.Pointer // 通用指针
		nextp  **sudog        // sudog二级指针
	)

	// pass 1 - look for something already waiting
	// 第一轮 - 查找是否有已经就绪的case
	var casi int                   // case索引
	var cas *scase                 // 当前case指针
	var caseSuccess bool           // case是否成功
	var caseReleaseTime int64 = -1 // case释放时间
	var recvOK bool                // 接收是否成功
	for _, casei := range pollorder {
		casi = int(casei)
		cas = &scases[casi]
		c = cas.c

		if casi >= nsends {
			// 接收case的处理
			sg = c.sendq.dequeue()
			if sg != nil {
				// 有发送方正在等待,直接接收
				goto recv
			}
			if c.qcount > 0 {
				// channel缓冲区有数据,从缓冲区接收
				goto bufrecv
			}
			if c.closed != 0 {
				// channel已关闭
				goto rclose
			}
		} else {
			// 发送case的处理
			if raceenabled {
				racereadpc(c.raceaddr(), casePC(casi), chansendpc)
			}
			if c.closed != 0 {
				// channel已关闭,不能发送
				goto sclose
			}
			sg = c.recvq.dequeue()
			if sg != nil {
				// 有接收方正在等待,直接发送
				goto send
			}
			if c.qcount < c.dataqsiz {
				// channel缓冲区有空间,发送到缓冲区
				goto bufsend
			}
		}
	}

	if !block {
		// 非阻塞模式且没有可用case,解锁并返回
		selunlock(scases, lockorder)
		casi = -1
		goto retc
	}
	// pass 2 - enqueue on all chans
	// 第二轮 - 将当前goroutine入队到所有相关channel
	gp = getg()
	if gp.waiting != nil {
		throw("gp.waiting != nil")
	}
	nextp = &gp.waiting
	for _, casei := range lockorder {
		casi = int(casei)
		cas = &scases[casi]
		c = cas.c
		sg := acquireSudog() // 获取一个sudog结构体,用于channel通信
		sg.g = gp            // 设置sudog对应的goroutine
		sg.isSelect = true   // 标记这是一个select操作
		// No stack splits between assigning elem and enqueuing
		// sg on gp.waiting where copystack can find it.
		// 在分配elem和将sg加入gp.waiting之间不能发生栈分裂
		// 因为copystack需要能找到它
		sg.elem = cas.elem // 设置数据元素
		sg.releasetime = 0 // 初始化释放时间
		if t0 != 0 {
			sg.releasetime = -1
		}
		sg.c = c // 设置对应的channel
		// Construct waiting list in lock order.
		// 按锁的顺序构造等待链表
		*nextp = sg // 将sudog加入等待链表
		nextp = &sg.waitlink

		if casi < nsends {
			c.sendq.enqueue(sg) // 发送case则加入sendq队列
		} else {
			c.recvq.enqueue(sg) // 接收case则加入recvq队列
		}

		if c.timer != nil {
			blockTimerChan(c) // 如果channel有定时器,则阻塞定时器
		}
	}
	// wait for someone to wake us up
	// 等待其他goroutine唤醒当前goroutine
	gp.param = nil
	// Signal to anyone trying to shrink our stack that we're about
	// to park on a channel. The window between when this G's status
	// changes and when we set gp.activeStackChans is not safe for
	// stack shrinking.
	// 通知任何尝试收缩我们栈的goroutine,我们即将在channel上阻塞
	// 在G的状态改变和设置gp.activeStackChans之间的窗口期是不安全的,不能进行栈收缩
	gp.parkingOnChan.Store(true)
	// 将当前goroutine挂起,等待被唤醒
	gopark(selparkcommit, nil, waitReasonSelect, traceBlockSelect, 1)
	gp.activeStackChans = false

	// 重新获取所有case的锁
	sellock(scases, lockorder)

	// 重置selectDone标志
	gp.selectDone.Store(0)
	// 获取唤醒我们的sudog
	sg = (*sudog)(gp.param)
	gp.param = nil

	// pass 3 - dequeue from unsuccessful chans
	// otherwise they stack up on quiet channels
	// record the successful case, if any.
	// We singly-linked up the SudoGs in lock order.
	// 第三轮 - 将未成功的channel从队列中移除
	// 否则它们会在静默的channel上堆积
	// 记录成功的case(如果有的话)
	// 我们按照加锁顺序将SudoG单向链接
	casi = -1
	cas = nil
	caseSuccess = false
	sglist = gp.waiting
	// Clear all elem before unlinking from gp.waiting.
	// 在从gp.waiting解除链接之前清除所有elem
	for sg1 := gp.waiting; sg1 != nil; sg1 = sg1.waitlink {
		sg1.isSelect = false // 清除select标记
		sg1.elem = nil       // 清除数据元素
		sg1.c = nil          // 清除channel引用
	}
	gp.waiting = nil // 清空等待队列
	// pass 3 - dequeue from unsuccessful chans
	// otherwise they stack up on quiet channels
	// record the successful case, if any.
	// We singly-linked up the SudoGs in lock order.
	// 第三轮 - 将未成功的channel从队列中移除
	// 否则它们会在静默的channel上堆积
	// 记录成功的case(如果有的话)
	// 我们按照加锁顺序将SudoG单向链接
	for _, casei := range lockorder {
		k = &scases[casei]
		// 如果channel有定时器,则解除阻塞
		if k.c.timer != nil {
			unblockTimerChan(k.c)
		}
		if sg == sglist {
			// sg has already been dequeued by the G that woke us up.
			// sg已经被唤醒我们的G从队列中移除
			casi = int(casei)
			cas = k
			caseSuccess = sglist.success
			if sglist.releasetime > 0 {
				caseReleaseTime = sglist.releasetime
			}
		} else {
			// 获取当前case对应的channel
			c = k.c
			// 根据case类型(发送/接收)从相应队列中移除sudog
			if int(casei) < nsends {
				c.sendq.dequeueSudoG(sglist)
			} else {
				c.recvq.dequeueSudoG(sglist)
			}
		}
		// 解除sudog的链接关系并释放
		sgnext = sglist.waitlink
		sglist.waitlink = nil
		releaseSudog(sglist)
		sglist = sgnext
	}

	// 如果没有找到成功的case,则抛出异常
	if cas == nil {
		throw("selectgo: bad wakeup")
	}

	// 获取成功case对应的channel
	c = cas.c

	if debugSelect {
		print("wait-return: cas0=", cas0, " c=", c, " cas=", cas, " send=", casi < nsends, "\n")
	}

	// 处理发送/接收操作的结果
	if casi < nsends {
		if !caseSuccess {
			goto sclose
		}
	} else {
		recvOK = caseSuccess
	}

	// 竞态检测相关代码
	if raceenabled {
		if casi < nsends {
			raceReadObjectPC(c.elemtype, cas.elem, casePC(casi), chansendpc)
		} else if cas.elem != nil {
			raceWriteObjectPC(c.elemtype, cas.elem, casePC(casi), chanrecvpc)
		}
	}
	// 内存检查相关代码
	if msanenabled {
		if casi < nsends {
			msanread(cas.elem, c.elemtype.Size_)
		} else if cas.elem != nil {
			msanwrite(cas.elem, c.elemtype.Size_)
		}
	}
	if asanenabled {
		if casi < nsends {
			asanread(cas.elem, c.elemtype.Size_)
		} else if cas.elem != nil {
			asanwrite(cas.elem, c.elemtype.Size_)
		}
	}

	// 解锁所有case并返回
	selunlock(scases, lockorder)
	goto retc

bufrecv:
	// can receive from buffer
	if raceenabled {
		if cas.elem != nil {
			raceWriteObjectPC(c.elemtype, cas.elem, casePC(casi), chanrecvpc)
		}
		racenotify(c, c.recvx, nil)
	}
	if msanenabled && cas.elem != nil {
		msanwrite(cas.elem, c.elemtype.Size_)
	}
	if asanenabled && cas.elem != nil {
		asanwrite(cas.elem, c.elemtype.Size_)
	}
	recvOK = true
	qp = chanbuf(c, c.recvx)
	if cas.elem != nil {
		typedmemmove(c.elemtype, cas.elem, qp)
	}
	typedmemclr(c.elemtype, qp)
	c.recvx++
	if c.recvx == c.dataqsiz {
		c.recvx = 0
	}
	c.qcount--
	selunlock(scases, lockorder)
	goto retc

bufsend:
	// can send to buffer
	if raceenabled {
		racenotify(c, c.sendx, nil)
		raceReadObjectPC(c.elemtype, cas.elem, casePC(casi), chansendpc)
	}
	if msanenabled {
		msanread(cas.elem, c.elemtype.Size_)
	}
	if asanenabled {
		asanread(cas.elem, c.elemtype.Size_)
	}
	typedmemmove(c.elemtype, chanbuf(c, c.sendx), cas.elem)
	c.sendx++
	if c.sendx == c.dataqsiz {
		c.sendx = 0
	}
	c.qcount++
	selunlock(scases, lockorder)
	goto retc

recv:
	// can receive from sleeping sender (sg)
	recv(c, sg, cas.elem, func() { selunlock(scases, lockorder) }, 2)
	if debugSelect {
		print("syncrecv: cas0=", cas0, " c=", c, "\n")
	}
	recvOK = true
	goto retc

rclose:
	// read at end of closed channel
	selunlock(scases, lockorder)
	recvOK = false
	if cas.elem != nil {
		typedmemclr(c.elemtype, cas.elem)
	}
	if raceenabled {
		raceacquire(c.raceaddr())
	}
	goto retc

send:
	// can send to a sleeping receiver (sg)
	if raceenabled {
		raceReadObjectPC(c.elemtype, cas.elem, casePC(casi), chansendpc)
	}
	if msanenabled {
		msanread(cas.elem, c.elemtype.Size_)
	}
	if asanenabled {
		asanread(cas.elem, c.elemtype.Size_)
	}
	send(c, sg, cas.elem, func() { selunlock(scases, lockorder) }, 2)
	if debugSelect {
		print("syncsend: cas0=", cas0, " c=", c, "\n")
	}
	goto retc

retc:
	if caseReleaseTime > 0 {
		blockevent(caseReleaseTime-t0, 1)
	}
	return casi, recvOK

sclose:
	// send on closed channel
	selunlock(scases, lockorder)
	panic(plainError("send on closed channel"))
}

func (c *hchan) sortkey() uintptr {
	return uintptr(unsafe.Pointer(c))
}

// A runtimeSelect is a single case passed to rselect.
// This must match ../reflect/value.go:/runtimeSelect
type runtimeSelect struct {
	dir selectDir
	typ unsafe.Pointer // channel type (not used here)
	ch  *hchan         // channel
	val unsafe.Pointer // ptr to data (SendDir) or ptr to receive buffer (RecvDir)
}

// These values must match ../reflect/value.go:/SelectDir.
type selectDir int

const (
	_             selectDir = iota
	selectSend              // case Chan <- Send
	selectRecv              // case <-Chan:
	selectDefault           // default
)

//go:linkname reflect_rselect reflect.rselect
func reflect_rselect(cases []runtimeSelect) (int, bool) {
	// 如果没有case,则永久阻塞
	if len(cases) == 0 {
		block()
	}
	// 创建内部select使用的case数组和原始索引数组
	sel := make([]scase, len(cases))
	orig := make([]int, len(cases))
	nsends, nrecvs := 0, 0 // 发送和接收case的计数器
	dflt := -1             // default case的索引,初始为-1

	// 遍历所有case,将其转换为内部格式
	for i, rc := range cases {
		var j int
		switch rc.dir {
		case selectDefault:
			dflt = i // 记录default case的位置
			continue
		case selectSend:
			j = nsends // 发送case放在数组前面
			nsends++
		case selectRecv:
			nrecvs++ // 接收case放在数组后面
			j = len(cases) - nrecvs
		}

		// 转换为内部scase格式
		sel[j] = scase{c: rc.ch, elem: rc.val}
		orig[j] = i // 记录原始位置
	}

	// Only a default case.
	// 如果只有default case,直接返回
	if nsends+nrecvs == 0 {
		return dflt, false
	}

	// Compact sel and orig if necessary.
	// 如果有必要,压缩sel和orig数组
	// 将接收case移到发送case后面
	if nsends+nrecvs < len(cases) {
		copy(sel[nsends:], sel[len(cases)-nrecvs:])
		copy(orig[nsends:], orig[len(cases)-nrecvs:])
	}

	// 创建轮询顺序数组
	order := make([]uint16, 2*(nsends+nrecvs))
	var pc0 *uintptr
	// 如果启用了竞态检测,创建PC值数组
	if raceenabled {
		pcs := make([]uintptr, nsends+nrecvs)
		for i := range pcs {
			selectsetpc(&pcs[i])
		}
		pc0 = &pcs[0]
	}

	// 调用selectgo执行实际的select操作
	chosen, recvOK := selectgo(&sel[0], &order[0], pc0, nsends, nrecvs, dflt == -1)

	// Translate chosen back to caller's ordering.
	// 将选中的case索引转换回调用者的顺序
	if chosen < 0 {
		chosen = dflt // 如果返回负数,表示选中了default case
	} else {
		chosen = orig[chosen] // 否则转换为原始索引
	}
	return chosen, recvOK
}

func (q *waitq) dequeueSudoG(sgp *sudog) {
	x := sgp.prev
	y := sgp.next
	if x != nil {
		if y != nil {
			// middle of queue
			x.next = y
			y.prev = x
			sgp.next = nil
			sgp.prev = nil
			return
		}
		// end of queue
		x.next = nil
		q.last = x
		sgp.prev = nil
		return
	}
	if y != nil {
		// start of queue
		y.prev = nil
		q.first = y
		sgp.next = nil
		return
	}

	// x==y==nil. Either sgp is the only element in the queue,
	// or it has already been removed. Use q.first to disambiguate.
	if q.first == sgp {
		q.first = nil
		q.last = nil
	}
}
