package server

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 6004 带「将在 … 重置」时，除了写模型级冷却，还要**登记额度窗口**：窗口按首次请求起算，
// 到点后若没有请求进来，新一轮窗口就不会开始（额度空转到下一次真实请求为止）。
func TestModelRateLimitSchedulesWindow(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	// 重置时刻必须是**未来**：ScheduleModelWindow 拒绝已过去的重置点（那说明窗口已经开始，
	// 或解析有偏差，登记它只会在下一轮巡检里立刻触发一次无意义探针）。以前这里写死成
	// 「2026-11-14 20:30:00」，CI 跑到 20:32 就红——定时炸弹式夹具。
	resetAt := time.Now().In(rateResetLoc()).Add(45 * time.Minute).Truncate(time.Second)
	body := `{"code":6004,"msg":"使用量已超出频率限制，将在 ` + resetAt.Format("2006-01-02 15:04:05") + ` 重置"}`
	want, ok := upstream.ParseRateReset(body)
	if !ok {
		t.Fatalf("夹具的重置文案应能解析出时刻: %s", body)
	}

	h.applyErrorPolicy("u1", upstream.ErrSoftRate, body, "deepseek-v4.1-flash", &upstream.Error{Kind: upstream.ErrSoftRate, Status: 429, Msg: body})

	ws := p.ModelWindows()
	if len(ws) != 1 {
		t.Fatalf("窗口登记数=%d want 1（6004 带重置时刻应登记）", len(ws))
	}
	w := ws[0]
	if w.UID != "u1" || w.Model != "deepseek-v4.1-flash" || !w.Quota {
		t.Fatalf("窗口内容不对: %+v", w)
	}
	if !w.ResetAt.Equal(want) {
		t.Fatalf("ResetAt=%v want %v（应与上游给的重置时刻完全一致）", w.ResetAt, want)
	}
	// 普通 429（没有模型级限额）不该登记窗口：临时限流没有「新一轮配额」这回事。
	before := len(p.ModelWindows())
	otherReset := time.Now().In(rateResetLoc()).Add(55 * time.Minute).Truncate(time.Second)
	otherBody := `{"code":11102,"msg":"请求过于频繁，将在 ` + otherReset.Format("2006-01-02 15:04:05") + ` 重置"}`
	h.applyErrorPolicy("u1", upstream.ErrSoftRate, otherBody, "deepseek-v4.1-flash",
		&upstream.Error{Kind: upstream.ErrSoftRate, Status: 429, Msg: otherBody})
	if got := len(p.ModelWindows()); got != before {
		t.Fatalf("非 6004 限流不该新增窗口：%d → %d", before, got)
	}
}

// rateResetLoc 上游重置时刻按 UTC+8 解释（ParseRateReset 的口径），夹具按同一时区拼文案。
func rateResetLoc() *time.Location { return time.FixedZone("CST", 8*3600) }
