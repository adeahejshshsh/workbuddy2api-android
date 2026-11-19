package scheduler

// W4 分片修复的回归用例（W4-02 / W4-04）。
//
// 每条都按「把修复改坏 → 变红」设计：族锁的竞态无法在 Android/arm64 上用 -race
// 观测（不支持），所以用引用计数/持有者校验这些**可直接观测的不变量**做锚点，
// 另加一个高并发压力用例覆盖真实交错。

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestWindowProbeLockBusyPushesResetAt W4-02：族锁被占（面板「立即探针」在跑）时，
// 后台探针必须把窗口的 ResetAt 推后，而不是原样返回。
//
// 原实现是唯一既不 DropModelWindow 也不 RecordWindowProbe 的返回路径：窗口仍处于
// 「已到点」状态，Run 循环处理完立刻重查 → 同一条窗口反复命中，紧凑空转整个持锁
// 时长（最长 60s，每轮都是 RLock + 全表遍历 + sort）。
//
// 变异验证：删掉 probeWindow 抢锁失败分支里的 RecordWindowProbe → ResetAt 停在
// 原处（≈60s 后），本用例在「ResetAt 距现在 ≈15s」处必红。
func TestWindowProbeLockBusyPushesResetAt(t *testing.T) {
	p, s, f := windowEnv(t, func(int) (int, string) { return http.StatusOK, "" })
	reset := dueWindow(t, p, "glm-5.3")

	// 模拟面板「立即探针」持锁。
	if !s.TryBeginAccountTask("w1", TaskWindowProbe) {
		t.Fatal("前提失败：拿不到族锁")
	}
	before := time.Now()
	if got := s.runWindowProbes(t.Context(), p.DueModelWindows(reset.Add(time.Second))); got != 0 {
		t.Fatalf("族锁被占时不该发出探针，got %d", got)
	}
	s.EndAccountTask("w1", TaskWindowProbe)

	if f.calls != 0 {
		t.Fatalf("族锁被占时上游不该被调用，got %d", f.calls)
	}
	w := p.ModelWindows()
	if len(w) != 1 {
		t.Fatalf("窗口应保留（只是推后重试），got %+v", w)
	}
	if d := w[0].ResetAt.Sub(before); d < windowProbeRetryDelay/2 || d > windowProbeRetryDelay*2 {
		t.Fatalf("ResetAt 距现在 %v，want ≈%v：抢锁失败没有推后 → Run 循环会紧凑空转", d, windowProbeRetryDelay)
	}
	if w[0].Attempts != 0 {
		t.Fatalf("这次一个请求都没发出，不该计一次探针失败，attempts=%d", w[0].Attempts)
	}
	if !strings.Contains(w[0].LastResult, "族锁") {
		t.Fatalf("LastResult 应说明跳过原因（排障用），got %q", w[0].LastResult)
	}
	// 推后之后立刻重查不应再命中 —— 这正是「循环不会空转」的直接判据。
	if due := p.DueModelWindows(time.Now()); len(due) != 0 {
		t.Fatalf("推后之后仍立即到点（下一轮还会命中同一条）：%+v", due)
	}
	if got := s.probeLockSkips.Load(); got != 1 {
		t.Fatalf("跳过计数 = %d，want 1（失败计数 WARN 的判据）", got)
	}
}

// TestProbeZeroSentBackoffCounts W4-02：整轮零发送时的硬退避必须被调用。
//
// 变异验证：
//   - 删掉 waitProbeBackoff 里的 noteWindowProbeIdleRound → 计数断言红；
//   - 删掉 Run 循环里的 `if !s.waitProbeBackoff(ctx)` → 源码断言红。
func TestProbeZeroSentBackoffCounts(t *testing.T) {
	s := New(Config{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if s.waitProbeBackoff(ctx) {
		t.Fatal("ctx 已取消时应返回 false（调用方据此退出）")
	}
	if got := s.probeIdleRounds.Load(); got != 1 {
		t.Fatalf("零发送退避没有被记录，probeIdleRounds=%d want 1", got)
	}
	src := string(mustReadFile(t, "windowprobe.go"))
	if !strings.Contains(src, "if !s.waitProbeBackoff(ctx)") {
		t.Fatal("Run 循环没有调用 waitProbeBackoff：零发送路径会退回紧凑空转（W4-02 回归）")
	}
}

// TestFamilyLockPruneKeepsReferencedEntry W4-04：有引用的条目绝不能被 prune 删掉。
//
// 这里的 refs=1 模拟「已经认定这把锁、正在等 mu」的第三方 —— 旧实现用
// TryLock→Unlock→delete 判「没人持有」，Unlock 与 delete 之间被抢占时条目会被删，
// 第三人随后新建空锁并成功持有，同号同族双跑（两路并发 RefreshToken 同一 RT →
// 12153 → 健康号被计死禁用）。
//
// 变异验证：把 pruneLocked 里的 `refs.CompareAndSwap(0, familyLockRefDeleted)`
// 去掉（回到无条件 delete）→ 本用例红。
func TestFamilyLockPruneKeepsReferencedEntry(t *testing.T) {
	f := &familyLocks{}
	l := f.entry("u1", "x") // refs=1：模拟正在等锁的调用方
	l.used.Store(time.Now().Add(-2 * familyLockIdle).UnixNano())

	f.pruneLocked()
	if f.count() != 1 {
		t.Fatal("prune 删掉了有引用的条目：第三人会新建锁并成功持有（同号同族双跑）")
	}
	again := f.entry("u1", "x")
	if again != l {
		t.Fatal("prune 之后拿到了另一把锁（互斥已失效）")
	}

	// 引用全部释放且空闲 → 必须被清掉，否则表会无限增长。
	l.used.Store(time.Now().Add(-2 * familyLockIdle).UnixNano())
	again.release()
	l.release()
	f.pruneLocked()
	if f.count() != 0 {
		t.Fatalf("无引用的空闲条目没被清掉：仍有 %d 条", f.count())
	}
}

// TestEndAccountTaskWithoutBeginNoPanic W4-04：未 Begin 就 End 不得 panic。
//
// 历史实现对零值 mutex 无条件 Unlock（sync: unlock of unlocked mutex），而
// EndAccountTask 几乎总是 defer 调用 —— panic 发生在收尾阶段，goroutine 里没有
// recover 点，一条收尾路径写错就带走整个进程。
//
// 变异验证：去掉 held.CompareAndSwap 校验、恢复无条件 Unlock → 第一次 End 就 panic，
// 本用例红。
func TestEndAccountTaskWithoutBeginNoPanic(t *testing.T) {
	s := New(Config{})
	s.EndAccountTask("u1", TaskCheckin) // 从未 Begin
	s.EndAccountTask("u1", TaskCheckin) // 重复

	if !s.TryBeginAccountTask("u1", TaskCheckin) {
		t.Fatal("Begin 应成功")
	}
	s.EndAccountTask("u1", TaskCheckin)
	s.EndAccountTask("u1", TaskCheckin) // 重复 End：只该 WARN，不能把锁再放一次

	if !s.TryBeginAccountTask("u1", TaskCheckin) {
		t.Fatal("重复 End 破坏了锁状态：释放后应能立刻重新占用")
	}
	s.EndAccountTask("u1", TaskCheckin)
}

// TestFamilyLockPruneRacingAcquireStaysExclusive W4-04 压力用例：
// prune 与 entry/TryLock 高频交错时，同号同族仍然只能有一个持有者。
//
// 每个调用方在释放前把 used 老化，使条目对 prune 始终「看起来可删」——这正是
// 旧实现 Unlock 与 delete 之间的窗口。检测方式：临界区内计数必须恒为 1。
func TestFamilyLockPruneRacingAcquireStaysExclusive(t *testing.T) {
	f := &familyLocks{}
	var inside atomic.Int32
	var violations atomic.Int32

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			f.pruneLocked()
		}
	}()

	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for j := 0; j < 500; j++ {
				l := f.entry("u1", "x")
				if l.mu.TryLock() {
					if inside.Add(1) != 1 {
						violations.Add(1)
					}
					inside.Add(-1)
					l.mu.Unlock()
				}
				// 老化 + 释放引用：让 prune 有机会删掉这条，制造「删/重建」交错。
				l.used.Store(time.Now().Add(-2 * familyLockIdle).UnixNano())
				l.release()
			}
		}()
	}
	workers.Wait()
	close(stop)
	wg.Wait()

	if n := violations.Load(); n != 0 {
		t.Fatalf("检测到 %d 次同号同族并发持有（prune 删掉了在用的条目）", n)
	}
}
