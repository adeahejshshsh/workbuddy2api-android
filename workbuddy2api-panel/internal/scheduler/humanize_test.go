package scheduler

// 周期任务人味化抖动（上游 2026-10-26 `a24c10a`）。
//
// 背景：2026-10-24 池内 6 个号被 11140 集中封禁，复盘发现同出口 IP 上全部账号以
// 「每 5 分钟整点、全账号并发齐射」的完全一致节拍活动 —— 最好认的机器人形状。

import (
	"testing"
	"time"
)

func TestJitterFracStableAndBounded(t *testing.T) {
	setJitterSeed(12345)
	defer setJitterSeed(0)
	a := jitterFrac("bal|uid-1")
	b := jitterFrac("bal|uid-1")
	if a != b {
		t.Fatalf("同一 key 必须稳定: %v vs %v", a, b)
	}
	if a < 0 || a >= 1 {
		t.Fatalf("frac %v 越界 [0,1)", a)
	}
	distinct := 0
	prev := a
	for _, key := range []string{"bal|u1", "bal|u2", "bal|u3", "bal|u4", "bal|u5"} {
		f := jitterFrac(key)
		if f != prev {
			distinct++
		}
		prev = f
	}
	if distinct < 3 {
		t.Fatalf("不同 key 应散开，实际 5 个里只有 %d 个不同", distinct)
	}
	// 种子变化 → 同 key 结果变化（重启换新抖动相位）。
	setJitterSeed(999)
	if jitterFrac("bal|uid-1") == a {
		t.Fatal("换种子后偏移应变")
	}
}

func TestPeriodicJitter(t *testing.T) {
	interval := 5 * time.Minute
	if got := periodicJitter(interval, 0, "k"); got != 0 {
		t.Fatalf("frac 0 必须是 no-op，got %v", got)
	}
	if got := periodicJitter(0, 0.2, "k"); got != 0 {
		t.Fatalf("interval 0 必须是 no-op，got %v", got)
	}
	setJitterSeed(42)
	defer setJitterSeed(0)
	for i := 0; i < 50; i++ {
		d := periodicJitter(interval, 0.2, "k")
		if d < -time.Minute || d > time.Minute { // ±20% of 5m = ±1m
			t.Fatalf("抖动 %v 超出 ±1m", d)
		}
	}
}

func TestBalanceRefreshStagger(t *testing.T) {
	if got := spreadIntervalStagger(5*time.Minute, 0); got != 0 {
		t.Fatalf("frac 0 必须关闭错峰，got %v", got)
	}
	setJitterSeed(7)
	defer setJitterSeed(0)
	window := spreadIntervalStagger(5*time.Minute, 0.2) // 1m
	seen := map[time.Duration]bool{}
	for _, uid := range []string{"a", "b", "c", "d", "e"} {
		d := spreadStaggerDelay(window, uid)
		if d < 0 || d >= window {
			t.Fatalf("uid %s 的延迟 %v 越界 [0,%v)", uid, d, window)
		}
		seen[d] = true
	}
	if len(seen) < 4 {
		t.Fatalf("不同账号应散开，实际 5 个里只有 %d 个不同", len(seen))
	}
}

// 抖动关闭时窗口恒 0 → 余额刷新不等待（旧行为：全账号并发齐射）。
func TestStaggerZeroWhenDisabled(t *testing.T) {
	s := New(Config{})
	if got := spreadIntervalStagger(time.Minute, s.balanceJitterFrac()); got != 0 {
		t.Fatalf("缺省（未调 SetBalanceJitter）应为 0，got %v", got)
	}
	s.SetBalanceJitter(100)
	if got := s.balanceJitterFrac(); got != 0.1 {
		t.Fatalf("100‰ 应折成 0.1，got %v", got)
	}
	// 越界归一到边界（构造期由 config 校验，这里守 Set 的幂等性）。
	s.SetBalanceJitter(9999)
	if got := s.balanceJitterFrac(); got != 0.5 {
		t.Fatalf("超上限应夹到 0.5，got %v", got)
	}
	s.SetBalanceJitter(-5)
	if got := s.balanceJitterFrac(); got != 0 {
		t.Fatalf("负数应归 0，got %v", got)
	}
}
