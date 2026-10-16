package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestReviveClearsDegrade 回归：连败降权后调 Revive（面板「解冻」按钮）必须立刻可再次选中。
//
// 背景（用户实测）：面板点「解冻」提示「已解冻」，但号依然不可选 —— Revive 清了
// disabled/paused/冷却/熔断/各计数，**唯独漏清连败降权的 consecutiveFails 与
// degradeUntil**；而 healthy() 在 degradeUntil 未到期时恒 false。更糟的是这两个字段
// 会落盘（persist.go），重启网关也甩不掉，只能等自然过期（10 分钟 ~ 2 小时）。
func TestReviveClearsDegrade(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100, 0)

	for i := 0; i < defaultDegradeThreshold; i++ {
		p.NoteFailures("u1")
	}
	e := p.byUID["u1"]
	if e == nil {
		t.Fatal("账号未加入池")
	}
	if e.degradeUntil.IsZero() || !time.Now().Before(e.degradeUntil) {
		t.Fatalf("前置条件不成立：连败 %d 次未触发降权", defaultDegradeThreshold)
	}
	// 注意：不能拿 Pick 判"不可选"——选号有 fallback_earliest_expiry 兜底，饿死场景下
	// 降权号仍会被兜底选中（日志会打 WARN）。用户看到的是 **healthy 计数为 0**（面板标签
	// 「连败降权 · 剩余 xx」、/healthz 的 healthy 也是它），所以这里对齐同一口径。
	if _, healthy, _, _, _ := p.CountsDetailed(); healthy != 0 {
		t.Fatalf("降权期内 healthy 应为 0，实际 %d（前置条件）", healthy)
	}

	if !p.Revive("u1") {
		t.Fatal("Revive 返回 false（账号不存在）")
	}
	if !e.degradeUntil.IsZero() || e.consecutiveFails != 0 {
		t.Fatalf("Revive 后降权残留：degradeUntil=%v consecutiveFails=%d",
			e.degradeUntil, e.consecutiveFails)
	}
	if _, healthy, _, _, _ := p.CountsDetailed(); healthy != 1 {
		t.Fatalf("Revive 后 healthy 应为 1，实际 %d —— 这正是「解冻点了没效果」的形态", healthy)
	}
}
