package server

import (
	"strings"
	"testing"
	"time"
)

func TestDailyBudgetStopsAtLimit(t *testing.T) {
	b := newDailyBudget(1)
	b.add(0.4, true)
	if ok, _ := b.admit(); !ok {
		t.Fatal("under limit should pass")
	}
	b.add(0.6, true)
	if ok, _ := b.admit(); ok {
		t.Fatal("used == limit should reject")
	}
	used, limit, rejected, _ := b.snapshot()
	if used != 1 || limit != 1 || rejected != 1 {
		t.Fatalf("snapshot used=%v limit=%v rejected=%d", used, limit, rejected)
	}
	b.add(-3, true)
	b.add(5, false)
	used, _, _, _ = b.snapshot()
	if used != 1 {
		t.Fatalf("negative and unobserved must not move used, got %v", used)
	}
}

// TestDailyBudgetBurstGuard issue #229：5 分钟窗口内耗分达阈值就拒，窗口外的旧样本不再计入。
func TestDailyBudgetBurstGuard(t *testing.T) {
	b := newDailyBudget(0) // 日上限关闭，只有突发闸
	b.setGuards(30, 0)
	if ok, _ := b.admit(); !ok {
		t.Fatal("空窗口应放行")
	}
	b.add(20, true)
	if ok, _ := b.admit(); !ok {
		t.Fatal("20 < 30 应放行")
	}
	b.add(12, true)
	ok, why := b.admit()
	if ok {
		t.Fatal("32 >= 30 应被拒")
	}
	if !strings.Contains(why, "burst") {
		t.Fatalf("拒因未标明突发闸: %q", why)
	}
	_, _, _, last := b.snapshot()
	if last != "burst" {
		t.Fatalf("lastReject=%q want burst", last)
	}
	// 样本移出窗口后自愈（不必等自然日重置）。
	b.mu.Lock()
	for i := range b.burst {
		b.burst[i].at = time.Now().Add(-budgetBurstWindow - time.Second)
	}
	b.mu.Unlock()
	if ok, _ := b.admit(); !ok {
		t.Fatal("窗口外样本应失效并放行")
	}
}

// TestDailyBudgetCacheGuard issue #229：连续 5 轮低命中率触发 60s 冷却，恢复一轮即清零。
func TestDailyBudgetCacheGuard(t *testing.T) {
	b := newDailyBudget(0)
	b.setGuards(0, 10) // 命中率低于 10% 算低
	// 未观测样本（上游没给缓存字段）不参与判定。
	for i := 0; i < 10; i++ {
		b.observeCache(0, 100, false)
	}
	if ok, _ := b.admit(); !ok {
		t.Fatal("未观测样本不应触发缓存闸")
	}
	for i := 0; i < 4; i++ {
		b.observeCache(1, 99, true) // 1%
	}
	if ok, _ := b.admit(); !ok {
		t.Fatal("4 轮不应触发（阈值 5 轮）")
	}
	b.observeCache(1, 99, true)
	ok, why := b.admit()
	if ok {
		t.Fatal("连续 5 轮低命中率应被拒")
	}
	if !strings.Contains(why, "cache") {
		t.Fatalf("拒因未标明缓存闸: %q", why)
	}
	// 冷却过期后放行（半开探测，避免永久锁死）。
	b.mu.Lock()
	b.cacheCoolTill = time.Now().Add(-time.Second)
	b.mu.Unlock()
	if ok, _ := b.admit(); !ok {
		t.Fatal("冷却过期后应放行")
	}
	// 一轮好命中率把连败清零。
	b.observeCache(90, 10, true)
	b.mu.Lock()
	streak := b.cacheStreak
	b.mu.Unlock()
	if streak != 0 {
		t.Fatalf("好命中率应清零连败，streak=%d", streak)
	}
}

// TestDailyBudgetGuardsOffByDefault 两道闸缺省关闭，行为与旧版一致。
func TestDailyBudgetGuardsOffByDefault(t *testing.T) {
	b := newDailyBudget(0)
	b.add(1000, true)
	for i := 0; i < 10; i++ {
		b.observeCache(0, 1000, true)
	}
	if ok, _ := b.admit(); !ok {
		t.Fatal("闸默认关闭时不得拒绝")
	}
}
