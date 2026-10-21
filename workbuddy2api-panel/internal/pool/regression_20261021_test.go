package pool

import (
	"testing"
	"time"
)

// bug #3：上游给的重置时刻已经过期（时钟偏移/文案过期）时，冷却时长必须回落到
// 调用方给的退避基数，而不是"1 毫秒"—— 后者等于把被限流的号立刻放回池里，
// 同一个号马上又被选中，限流保护形同虚设。
func TestCappedSoftUntilExpiredResetFallsBackToBase(t *testing.T) {
	p := New("")
	now := time.Now()
	base := 30 * time.Second

	got := p.cappedSoftUntilLocked(now, now.Add(-time.Hour), base)
	d := got.Sub(now)
	if d < base-2*time.Second || d > base+2*time.Second {
		t.Fatalf("resetAt 过期时冷却 %v，期望约 %v（不是 1ms）", d, base)
	}

	// 未来时间仍按上游墙钟
	future := now.Add(5 * time.Minute)
	if got := p.cappedSoftUntilLocked(now, future, base); !got.Equal(future) {
		t.Fatalf("resetAt 在未来时冷却到 %v，期望 %v", got, future)
	}

	// base<=0 时回落到 1 分钟兜底
	if got := p.cappedSoftUntilLocked(now, now.Add(-time.Hour), 0); got.Sub(now) < 50*time.Second {
		t.Fatalf("base<=0 时冷却 %v，期望回落到 1 分钟", got.Sub(now))
	}

	// 超过 softRateMax 的 resetAt 必须被截断
	p.softRateMax = time.Minute
	if got := p.cappedSoftUntilLocked(now, now.Add(time.Hour), base); got.Sub(now) > time.Minute+time.Second {
		t.Fatalf("冷却 %v 没被 softRateMax 截断", got.Sub(now))
	}
}
