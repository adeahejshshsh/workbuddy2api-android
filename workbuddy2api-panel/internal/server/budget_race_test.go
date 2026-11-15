package server

// T-6 / 审查 F-08：dailyBudget 的 sync.Mutex 零并发用例。
//
// 既有 4 个用例（budget_test.go: TestDailyBudgetStopsAtLimit / BurstGuard /
// CacheGuard / GuardsOffByDefault）全部串行，budget_persist_test.go 与
// budget_floor_test.go 里也没有一个 goroutine —— 于是 mu 保护的五个方法
// （admit / add / observeCache / snapshot / rollLocked）的并发语义**一条断言都没有**：
// 累计值丢更新、拒绝计数漏加、跨日翻转把刚加的用量清零、saveLocked 与 add 互相
// 死锁，四种形态在串行用例下全是绿的（-race 也报不出来，因为没人并发）。
//
// 本文件的断言对象是**记账精确性**，不是「锁存在」：
//   - used 必须精确等于所有 add 之和（丢更新 → 少；跨日翻转时序错 → 少/多）；
//   - rejected 必须精确等于被拒次数（计数挪出锁区间 → 少）；
//   - 落盘值必须等于内存值，且不得自死锁（saveLocked 一旦自己加锁就是永久阻塞，
//     而 go test 的超时只会丢一堆 goroutine 栈）。
//
// 说明：admit() 是**准入检查**不是预扣（handler 先 admit 再执行、最后 add），
// 所以「并发 admit 的成功数 ≤ 预算允许数」这条不变量本就不成立 —— 这里不去断言
// 一个假的不变量，只钉住真正该成立的三条。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// budgetRaceTimeout 并发用例的看门狗上限。
//
// 为什么必须有：budget 的锁区间里会做落盘 I/O（saveLocked），锁序写错就是永久
// 阻塞；没有看门狗时表现为整包 180s 超时 + 一堆栈，看不出是哪个用例。
// 正常路径（几百次内存累加）远小于 1s，30s 只是死锁判据，不是性能断言。
const budgetRaceTimeout = 30 * time.Second

// runConcurrently 起 n 个 goroutine 跑 fn 并等全部返回；超时判定为死锁。
func runConcurrently(t *testing.T, n int, what string, fn func(i int)) {
	t.Helper()
	var wg sync.WaitGroup
	done := make(chan struct{})
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			fn(i)
		}(i)
	}
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(budgetRaceTimeout):
		t.Fatalf("%s：%d 个 goroutine 在 %v 内没有全部返回（疑似 dailyBudget 锁区间死锁）",
			what, n, budgetRaceTimeout)
	}
}

// TestBudgetConcurrentAddNoLostUpdate 并发 add 的累计值必须精确。
//
// 不变量：used == 所有已观测 credit 之和（一个不能少、一个不能多），突发窗口的
// 样本和与 used 一致。把 add 里的 mu.Lock/Unlock 去掉（或改成读旧值写新值）就会
// 丢更新 —— 1000 次 0.25 的累加在丢更新下几乎必然小于 250。
//
// 0.25 是刻意的：二进制可精确表示，累加 1000 次不会产生浮点漂移，断言可以用
// 精确相等而不是容差（容差会让「丢一两条」也过）。
func TestBudgetConcurrentAddNoLostUpdate(t *testing.T) {
	const (
		workers = 8
		perGuy  = 125
		step    = 0.25
	)
	b := newDailyBudget(0, "") // 日上限关闭：只测记账
	b.setGuards(1e9, 0)        // 突发闸开着但不触发，顺带覆盖 burst 记账
	runConcurrently(t, workers, "并发 add", func(int) {
		for i := 0; i < perGuy; i++ {
			b.add(step, true)
		}
	})

	want := float64(workers) * perGuy * step
	used, _, rejected, last := b.snapshot()
	if used != want {
		t.Fatalf("used=%v want %v（丢了 %v 的用量 = 并发丢更新，止损线会低估）",
			used, want, want-used)
	}
	if rejected != 0 || last != "" {
		t.Fatalf("rejected=%d lastReject=%q，无闸触发时不该有拒绝", rejected, last)
	}

	b.mu.Lock()
	samples, sum := len(b.burst), 0.0
	for _, s := range b.burst {
		sum += s.credit
	}
	b.mu.Unlock()
	if samples != workers*perGuy {
		t.Fatalf("突发窗口样本数=%d want %d（样本丢失 = 突发闸漏判）", samples, workers*perGuy)
	}
	if sum != want {
		t.Fatalf("突发窗口 credit 和=%v want %v", sum, want)
	}
}

// TestBudgetConcurrentAddAcrossDayFlipCountsExactlyOnce 跨日翻转与并发 add 交错时，
// 翻转必须**恰好发生一次**，且翻转后新增的用量一条都不能被清掉。
//
// 场景：进程跨过 CST 零点时，多个请求同时进入 add → 每个都要过 rollLocked。
// 若 rollLocked 在锁外调用（或先 add 后 roll），后到的那个会把前一个刚累加的用量
// 清零 → 当日用量恒小于真实值，成本止损线形同虚设；反之重复累加则多算。
// 这里预置「昨天的 500 用量 + 9 次拒绝 + 5 连败缓存计数」，断言翻转把旧状态清零、
// 而这一轮并发的 8 条用量完整留下。
func TestBudgetConcurrentAddAcrossDayFlipCountsExactlyOnce(t *testing.T) {
	const workers = 8
	b := newDailyBudget(0, "")
	b.setGuards(1e9, 0) // 打开突发记账（样本只在闸开启时才记录），但不触发
	b.mu.Lock()
	b.day = "2000-01-01" // 构造「昨天」
	b.used = 500
	b.rejected = 9
	b.cacheStreak = 5
	b.lastReject = "burst"
	b.burst = []budgetSample{{at: time.Now(), credit: 3}}
	b.mu.Unlock()

	runConcurrently(t, workers, "跨日翻转 + 并发 add", func(int) {
		b.add(1, true)
	})

	used, _, rejected, last := b.snapshot()
	if used != workers {
		t.Fatalf("used=%v want %d：跨日翻转与并发 add 交错时用量被清掉/重复累加", used, workers)
	}
	if rejected != 0 {
		t.Fatalf("rejected=%d want 0：跨日必须清零拒绝计数（否则面板显示昨天的限流次数）", rejected)
	}
	if last != "" {
		t.Fatalf("lastReject=%q want 空：跨日必须清掉拒因", last)
	}
	b.mu.Lock()
	streak, burstLen := b.cacheStreak, len(b.burst)
	burstSum := 0.0
	for _, sample := range b.burst {
		burstSum += sample.credit
	}
	b.mu.Unlock()
	if streak != 0 {
		t.Fatalf("cacheStreak=%d want 0：跨日必须清零连续低命中计数", streak)
	}
	if burstLen != workers {
		t.Fatalf("突发样本=%d want %d：昨天的样本必须随翻转丢弃", burstLen, workers)
	}
	if burstSum != workers {
		t.Fatalf("突发样本和=%v want %d：昨天的 3 credit 样本残留会让突发闸提前触发", burstSum, workers)
	}
	if got := b.day; got != cstDay(time.Now()) {
		t.Fatalf("day=%q want %q", got, cstDay(time.Now()))
	}
}

// TestBudgetConcurrentRejectCountIsExact 拒绝计数必须精确等于被拒次数。
//
// 不变量：snapshot().rejected == 所有返回 false 的 admit 次数。这个计数是面板
// 「今天被限流多少次」的唯一数据源，也是判断「预算是否设得太紧」的依据。
// 把 rejected++ 挪到锁外（或改成读-改-写）在 64 并发下必然丢计数。
func TestBudgetConcurrentRejectCountIsExact(t *testing.T) {
	const workers = 64
	b := newDailyBudget(1, "")
	b.add(1, true) // used == limit，之后每次 admit 都必须拒

	runConcurrently(t, workers, "并发 admit", func(int) {
		if ok, why := b.admit(); ok {
			t.Errorf("used==limit 时 admit 仍放行（why=%q）", why)
		}
	})

	_, _, rejected, last := b.snapshot()
	if rejected != workers {
		t.Fatalf("rejected=%d want %d：拒绝计数在并发下丢更新", rejected, workers)
	}
	if last != "daily" {
		t.Fatalf("lastReject=%q want daily", last)
	}
}

// TestBudgetConcurrentAddAndForcedSave 落盘与累加并发：不得死锁，且落盘值 == 内存值。
//
// 两条不变量：
//  1. saveLocked 的调用契约是「调用方已持锁」。它若自己再加一次锁（很自然的
//     「顺手加个锁更安全」改法）就是自死锁 —— 看门狗会把它变成一条明确的失败，
//     而不是整包 180s 超时。
//  2. 落盘内容必须与内存一致：重启后 loadBudgetState 读回的就是这条成本止损线。
//     写成 b.limit 或漏写 used 都会让重启后预算归零/错值。
func TestBudgetConcurrentAddAndForcedSave(t *testing.T) {
	const (
		adders = 8
		savers = 4
		perGuy = 50
		step   = 0.5
	)
	path := filepath.Join(t.TempDir(), "budget.json")
	b := newDailyBudget(0, path)

	runConcurrently(t, adders+savers, "并发 add + 强制落盘", func(i int) {
		if i < adders {
			for j := 0; j < perGuy; j++ {
				b.add(step, true)
			}
			return
		}
		// 直接按契约调用（持锁 → saveLocked → 放锁），与 add 内的调用同形。
		b.mu.Lock()
		b.saveLocked(time.Now(), true)
		b.mu.Unlock()
	})

	want := float64(adders) * perGuy * step
	used, _, _, _ := b.snapshot()
	if used != want {
		t.Fatalf("used=%v want %v", used, want)
	}

	// 收尾再强制写一次，确保磁盘上是最终值（并发期间的最后一次写可能早于最后一条 add）。
	b.mu.Lock()
	b.saveLocked(time.Now(), true)
	b.mu.Unlock()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回落盘文件: %v", err)
	}
	var st budgetState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("落盘内容不是合法 JSON（半截写）：%v raw=%q", err, raw)
	}
	if st.Used != want {
		t.Fatalf("落盘 used=%v want %v：重启后成本止损线会读回错值", st.Used, want)
	}
	if st.Day != cstDay(time.Now()) {
		t.Fatalf("落盘 day=%q want %q", st.Day, cstDay(time.Now()))
	}

	// 跨重启：新实例读回同一个用量（这是落盘存在的唯一理由）。
	nb := newDailyBudget(0, path)
	if got, _, _, _ := nb.snapshot(); got != want {
		t.Fatalf("重启后 used=%v want %v：落盘/读回不闭环", got, want)
	}
}
