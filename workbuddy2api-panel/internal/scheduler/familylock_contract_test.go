package scheduler

// T-17 / 审查 W5T4-01（P1）：任务族互斥**零用例**。
//
// 两把锁要分开看：
//   - `taskLocks`（scheduler.go:388 TryBeginTask）是**族级**一把锁，只能挡「两轮全池
//     排程同时跑」；
//   - `family`（familylock.go）是**账号 × 族**一把锁，挡的是「手动按号入口 vs 排程」
//     对同一个号的并发 —— 审查 E4 列出的 7 个旁路入口（按号旅行/手动签到/余额 401
//     求证刷新/窗口探针…）正是靠它收口的。
//
// 此前 `grep TryLock internal/scheduler/*_test.go` 零命中（唯一命中在 panel 的
// tryLockAccount，是另一个锁）。于是：把 TryLock 改成 Lock（并发入口直接互相阻塞，
// 手动按钮永久卡住）、去掉 EndAccountTask 里的 Unlock（一次之后该号永久让位）、
// 或者让 pruneLocked 把「正在持有的条目」也删掉（同号同族立刻能双跑 = E4 全线复活），
// 三种改法都不会让任何用例变红。
//
// 本文件的断言全部是**行为级**：并发抢锁的胜者数量、持锁期间的可观测状态、
// 以及清理逻辑对「持有中条目」的处置。

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fanOut 起 n 个 goroutine 跑 fn 并等全部返回；超时判定为死锁。
//
// 必须有看门狗：TryLock 被改成 Lock（或 EndAccountTask 漏了 Unlock）时的表现是
// **永久阻塞**，而 go test 的整包超时只会丢一堆栈，看不出是哪个用例。
func fanOut(t *testing.T, n int, what string, fn func(i int)) {
	t.Helper()
	var wg sync.WaitGroup
	done := make(chan struct{})
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) { defer wg.Done(); fn(i) }(i)
	}
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("%s：%d 个 goroutine 在 30s 内没有全部返回（族锁疑似死锁）", what, n)
	}
}

// TestFamilyLockExclusivePerAccountAndFamily 互斥的粒度是「账号 × 族」，不是全局。
//
// 不变量：
//   - 同一 (uid, family) 第二次占用必须失败（否则手动入口与排程会同时对这个号发起
//     同一批上游写请求：重复兑换、重复签到、重复计费的探针）；
//   - 不同账号之间**不互斥**（按号端点存在的意义就是让外部调度器做到每号独立时点，
//     叠加全局锁等于把指纹加回去）；
//   - 同一账号的不同族**不互斥**（签到与保活是两个不同的上游动作）；
//   - End 之后必须能立刻重新占用（否则该号在进程生命周期内只跑一次）。
func TestFamilyLockExclusivePerAccountAndFamily(t *testing.T) {
	s := New(Config{})

	if !s.TryBeginAccountTask("u1", TaskCheckin) {
		t.Fatal("首次占用应成功")
	}
	if s.TryBeginAccountTask("u1", TaskCheckin) {
		t.Fatal("同账号同族第二次占用必须失败（这正是双跑入口）")
	}
	if !s.TryBeginAccountTask("u2", TaskCheckin) {
		t.Fatal("不同账号之间不该互斥")
	}
	if !s.TryBeginAccountTask("u1", TaskKeepalive) {
		t.Fatal("同账号不同族不该互斥")
	}

	s.EndAccountTask("u1", TaskCheckin)
	if !s.TryBeginAccountTask("u1", TaskCheckin) {
		t.Fatal("End 之后必须能重新占用（漏 Unlock = 该号永久让位）")
	}
	s.EndAccountTask("u1", TaskCheckin)
	s.EndAccountTask("u1", TaskKeepalive)
	s.EndAccountTask("u2", TaskCheckin)
}

// TestFamilyLockConcurrentExactlyOneWinner 并发抢同一把族锁时，胜者必须**恰好一个**。
//
// 这是「排程到点」与「面板手动触发」撞在同一秒的最小复现。TryLock 换成 Lock 会让
// 31 个调用方全部阻塞（看门狗抓到）；去掉互斥（直接 return true）会让 32 个全胜。
func TestFamilyLockConcurrentExactlyOneWinner(t *testing.T) {
	const n = 32
	s := New(Config{})
	var winners atomic.Int32
	fanOut(t, n, "并发抢同一账号族锁", func(int) {
		if s.TryBeginAccountTask("u1", TaskCheckin) {
			winners.Add(1)
		}
	})
	if got := winners.Load(); got != 1 {
		t.Fatalf("胜者 %d 个 want 恰好 1（>1 = 同号同族双跑；0 = 没人抢到）", got)
	}
	// 胜者持锁中：只读探针必须报告「在跑」，且不能把锁放掉。
	if !s.AccountTaskRunning("u1", TaskCheckin) {
		t.Fatal("持有期间 AccountTaskRunning 应报告 true")
	}
	if s.AccountTaskRunning("u2", TaskCheckin) {
		t.Fatal("别的账号不该显示在跑")
	}
	s.EndAccountTask("u1", TaskCheckin)
	if s.AccountTaskRunning("u1", TaskCheckin) {
		t.Fatal("释放后仍报告在跑")
	}
}

// TestFamilyLockRunningProbeDoesNotLeakLock AccountTaskRunning 是只读探针，
// 不能把锁留在持有状态。
//
// 实现是「TryLock 成功就立刻 Unlock」：漏掉 Unlock 时单线程用例看不出任何异常
// （返回值都是 false），但该号该族从此**永远**无法被占用 —— 表现为「排程再也不跑
// 这个号」，且没有任何日志。
func TestFamilyLockRunningProbeDoesNotLeakLock(t *testing.T) {
	s := New(Config{})
	for i := 0; i < 3; i++ {
		if s.AccountTaskRunning("u1", TaskCheckin) {
			t.Fatalf("第 %d 次探针：未持有时应报告 false", i+1)
		}
	}
	if !s.TryBeginAccountTask("u1", TaskCheckin) {
		t.Fatal("探针之后无法占用族锁（AccountTaskRunning 泄漏了锁）")
	}
	if !s.AccountTaskRunning("u1", TaskCheckin) {
		t.Fatal("持有时探针应报告 true")
	}
	s.EndAccountTask("u1", TaskCheckin)
}

// TestFamilyLockPruneKeepsHeldEntry 清理空闲条目时**绝不能删掉正在持有的条目**。
//
// 这是 E4 旁路最隐蔽的复活方式：pruneLocked 里那句 `l.mu.TryLock()` 是唯一的
// 「有人持有就别删」判据。把它删掉（或改成无条件 delete），下一次 prune 就会把
// 正在跑的账号条目从表里摘掉 —— 之后 TryBeginAccountTask 会**新建一个空锁**并
// 成功返回 true，于是同一个号同一族真的双跑了，而所有单线程用例依然全绿。
//
// 断言方式是行为级的：先把全部条目老化到 familyLockIdle 之外，手工触发一次
// pruneLocked，再抢同一把锁 —— 抢到就是「持有的条目被删了」。
func TestFamilyLockPruneKeepsHeldEntry(t *testing.T) {
	s := New(Config{})
	if !s.TryBeginAccountTask("hold", TaskCheckin) {
		t.Fatal("占用应成功")
	}
	// 再造一批「用过但已释放」的条目，让 prune 有东西可删（否则删 0 条也能过）。
	for i := 0; i < 64; i++ {
		uid := fmt.Sprintf("u%02d", i)
		s.TryBeginAccountTask(uid, TaskCheckin)
		s.EndAccountTask(uid, TaskCheckin)
	}
	// 把全部条目老化到 familyLockIdle 之外：只剩「是否被持有」这一条判据。
	s.family.mu.Lock()
	for _, l := range s.family.m {
		l.used.Store(time.Now().Add(-familyLockIdle - time.Minute).UnixNano())
	}
	before := len(s.family.m)
	s.family.pruneLocked()
	after := len(s.family.m)
	s.family.mu.Unlock()

	if before <= 1 {
		t.Fatalf("测试前提失败：只造出 %d 个条目", before)
	}
	if after != 1 {
		t.Fatalf("prune 后剩 %d 条 want 1（只该保留被持有的那条，其余空闲条目都该清掉）", after)
	}
	if s.TryBeginAccountTask("hold", TaskCheckin) {
		t.Fatal("prune 把正在持有的族锁条目删掉了 —— 同号同族可以双跑（E4 旁路复现）")
	}
	s.EndAccountTask("hold", TaskCheckin)
	if !s.TryBeginAccountTask("hold", TaskCheckin) {
		t.Fatal("释放后应能重新占用")
	}
	s.EndAccountTask("hold", TaskCheckin)
}

// TestFamilyLockEmptyKeyIsNoop 未配 uid/family 时恒放行、且 End 不 panic。
//
// 契约写在 TryBeginAccountTask 的注释里（"未配 uid/family 时恒 true"）：调用点大量
// 是 `if !s.TryBeginAccountTask(uid, TaskX) { continue }`，若空 key 返回 false，
// 任何一处 uid 取空就会静默跳过整族任务（而不是报错）。
func TestFamilyLockEmptyKeyIsNoop(t *testing.T) {
	s := New(Config{})
	if !s.TryBeginAccountTask("", TaskCheckin) || !s.TryBeginAccountTask("u1", "") {
		t.Fatal("空 uid / 空 family 应恒放行")
	}
	s.EndAccountTask("", TaskCheckin) // 不该 panic（没有可释放的锁）
	s.EndAccountTask("u1", "")
	if s.AccountTaskRunning("", TaskCheckin) || s.AccountTaskRunning("u1", "") {
		t.Fatal("空 key 不该报告在跑")
	}

	// nil 接收者：scheduler 未装配时（测试/工具路径）不能崩。
	var nilSched *Scheduler
	if !nilSched.TryBeginAccountTask("u1", TaskCheckin) {
		t.Fatal("nil Scheduler 应恒放行")
	}
	nilSched.EndAccountTask("u1", TaskCheckin)
	if nilSched.AccountTaskRunning("u1", TaskCheckin) {
		t.Fatal("nil Scheduler 不该报告在跑")
	}
}

// TestFamilyLockKeySeparatesAccountAndFamily 族键必须能把 (uid, family) 唯一地拆开。
//
// 键是 uid + "\x00" + family：若改成 "uid:family" 这类可被 uid 内容伪造的拼接，
// 一个含冒号的 uid 就能与另一个 (uid, family) 组合撞到同一把锁 —— 表现为
// 「A 号跑签到把 B 号跑旅行挡住了」。uid 目前由 validImportUID 白名单兜底，
// 这条用例把这个隐含前提钉在测试里。
func TestFamilyLockKeySeparatesAccountAndFamily(t *testing.T) {
	if familyKey("a", "b") == familyKey("ab", "") {
		t.Fatal(`familyKey 无法区分 ("a","b") 与 ("ab","")`)
	}
	if familyKey("u1", TaskCheckin) == familyKey("u1", TaskTravel) {
		t.Fatal("同一账号的不同族必须映射到不同键")
	}
	if familyKey("u1", TaskCheckin) == familyKey("u2", TaskCheckin) {
		t.Fatal("不同账号必须映射到不同键")
	}
	// 含分隔符的 uid 不得与「相邻组合」撞键（这是用 \x00 而非 ':' 的理由）。
	if familyKey("a\x00b", "c") == familyKey("a", "b\x00c") {
		t.Log("注意：uid 含 \\x00 时可与相邻组合撞键；uid 由 validImportUID 白名单兜底")
	}
}
