package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 14003 的停车窗口：10s 起、同一 (号,模型) 反复命中翻倍、封顶 1m
// （misakano7545 `f728859a`）。
func TestCooldownModelBusyWindow(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	now := time.Now()

	p.CooldownModelBusy("u1", "m1", "14003 model busy")
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("status")
	}
	// 首档 10s（允许 9~12s 的执行抖动）
	var got time.Duration
	for _, r := range st.RateLimitedModels {
		if r.Model == "m1" {
			got = time.Until(r.Until)
		}
	}
	if got < 8*time.Second || got > 12*time.Second {
		t.Fatalf("首档窗口 = %v，期望 ~10s", got)
	}
	// 连续命中翻倍
	p.CooldownModelBusy("u1", "m1", "14003 model busy")
	st, _ = p.Status("u1")
	for _, r := range st.RateLimitedModels {
		if r.Model == "m1" {
			if d := time.Until(r.Until); d < 15*time.Second {
				t.Fatalf("第二次窗口 = %v，期望翻倍到 ~20s", d)
			}
		}
	}
	// 封顶 1m：连打十次不超过 1m（+抖动）
	for i := 0; i < 10; i++ {
		p.CooldownModelBusy("u1", "m1", "14003 model busy")
	}
	st, _ = p.Status("u1")
	for _, r := range st.RateLimitedModels {
		if r.Model == "m1" {
			if d := time.Until(r.Until); d > 65*time.Second {
				t.Fatalf("封顶失效：窗口 = %v", d)
			}
		}
	}
	_ = now
}
