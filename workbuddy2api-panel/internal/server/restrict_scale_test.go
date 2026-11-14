// restrict_scale_test.go 连坐暂停的阈值缩放与退避（2026-11-13 事故回归）。
//
// 事故：固定「2 个号被封 → 全站停派 30 分钟」，在 340 个号的池子上等于 0.6% 的账号出事
// 就让 100% 的请求 503；封号潮持续时更会「刚解除就再次触发」，11:11→14:44 与 21:41→23:41
// 两段连续停派，4,036 条请求快速失败。这里钉住三条：阈值随池子规模走、暂停时长指数退避、
// 平静期后连续计数清零。
package server

import (
	"testing"
	"time"
)

func TestRestrictionThresholdScalesWithPool(t *testing.T) {
	cases := []struct{ healthy, want int }{
		{0, 2},   // 拿不到池子规模：退回下限
		{3, 2},   // 小池子：下限 2
		{10, 2},  // 10/10=1 < 下限 → 仍是 2
		{19, 2},  // 同上
		{20, 2},  // 20/10=2 等于下限
		{50, 5},
		{340, 34}, // 11-13 那天的池子：要 15 分钟内封 34 个才判扫荡
	}
	for _, c := range cases {
		if got := restrictionThreshold(c.healthy); got != c.want {
			t.Fatalf("restrictionThreshold(%d)=%d want %d", c.healthy, got, c.want)
		}
	}

	old := restrictionWaveDiv
	restrictionWaveDiv = 0
	t.Cleanup(func() { restrictionWaveDiv = old })
	if got := restrictionThreshold(340); got != restrictionWaveMin {
		t.Fatalf("关闭缩放后退回固定阈值, got %d", got)
	}
}

// 关键回归：大池子上零星封号不再让整站停摆。
func TestRestrictionWaveIgnoresSmallBanRateOnBigPool(t *testing.T) {
	var g restrictionGate
	// 340 个号的池子，15 分钟内封 2 个（11-13 的现实数据）：不许暂停。
	for _, uid := range []string{"a", "b"} {
		if g.note("cn", uid, 30*time.Minute, 340) {
			t.Fatalf("340 healthy 时封 2 个不该触发全站停派（uid=%s）", uid)
		}
	}
	if d := g.remaining("cn"); d != 0 {
		t.Fatalf("不该暂停, remaining=%v", d)
	}
	// 封到阈值（34 个不同账号）才停。
	for i := 2; i < 34; i++ {
		uid := string(rune('a' + i%20))
		uid = uid + string(rune('0'+i/20))
		g.note("cn", uid, 30*time.Minute, 340)
	}
	if d := g.remaining("cn"); d <= 0 {
		t.Fatal("封到 34 个不同账号应触发该域暂停")
	}
}

// 连续触发按 2 倍退避：30s → 60s → … → 上限；配置更小时以配置为准。
func TestRestrictionHoldEscalates(t *testing.T) {
	var g restrictionGate
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute}
	for i, w := range want {
		if g.note("cn", "u1", 30*time.Minute, 0) {
			t.Fatalf("第 %d 轮第 1 个号不该触发", i)
		}
		if !g.note("cn", "u2", 30*time.Minute, 0) {
			t.Fatalf("第 %d 轮第 2 个号应触发", i)
		}
		d := g.remaining("cn")
		if d > w || d <= w-time.Second {
			t.Fatalf("第 %d 次暂停 = %v want ~%v（应指数退避）", i+1, d, w)
		}
		// 手工清掉暂停，模拟「到期后再次触发」。
		g.mu.Lock()
		g.m["cn"].until = time.Time{}
		g.mu.Unlock()
	}

	// 退避不会超过 restrictionHoldMax。
	var capped restrictionGate
	for i := 0; i < 12; i++ {
		capped.note("cn", "a", time.Hour, 0)
		capped.note("cn", "b", time.Hour, 0)
		capped.mu.Lock()
		capped.m["cn"].until = time.Time{}
		capped.mu.Unlock()
	}
	capped.note("cn", "a", time.Hour, 0)
	capped.note("cn", "b", time.Hour, 0)
	if d := capped.remaining("cn"); d > restrictionHoldMax {
		t.Fatalf("暂停 %v 超过上限 %v", d, restrictionHoldMax)
	}

	// 配置（upstream.restriction_hold_seconds）是上限。
	var small restrictionGate
	small.note("cn", "a", 5*time.Second, 0)
	small.note("cn", "b", 5*time.Second, 0)
	if d := small.remaining("cn"); d > 5*time.Second {
		t.Fatalf("配置 5s 时暂停 %v 超过配置上限", d)
	}
}

// 平静期（restrictionCalmReset）过后，连续计数清零，重新从 30 秒起。
func TestRestrictionStreakResetsAfterCalm(t *testing.T) {
	var g restrictionGate
	g.note("cn", "a", time.Hour, 0)
	g.note("cn", "b", time.Hour, 0) // streak=1, 30s
	if d := g.remaining("cn"); d > 30*time.Second || d <= 0 {
		t.Fatalf("首次暂停 = %v want ~30s", d)
	}
	// 把「上次触发」推到平静期之前，模拟隔了很久又来一波。
	g.mu.Lock()
	g.m["cn"].until = time.Time{}
	g.m["cn"].last = time.Now().Add(-restrictionCalmReset - time.Minute)
	g.mu.Unlock()
	g.note("cn", "c", time.Hour, 0)
	g.note("cn", "d", time.Hour, 0)
	if d := g.remaining("cn"); d > 30*time.Second || d <= 0 {
		t.Fatalf("平静期后应重新从 30s 起, got %v", d)
	}
}
