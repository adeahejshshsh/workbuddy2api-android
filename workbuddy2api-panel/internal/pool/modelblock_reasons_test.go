package pool

// ModelBlockStatus.Reasons 与「Reason 绑定最早解封那条」（上游 2026-10-26 `436631c`）。
//
// Reason 只有一条时分不清「全池是不是同一个锁因」；而锁因与解锁时刻若来自不同账号，
// 就会出现「原因说是 14003 瞬时忙、时刻却来自 6004 的配额重置」这种自相矛盾的提示。

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestModelBlockedReasonsAndReasonBinding(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "a2", AccessToken: "at2", ExpiresAt: 9999999999})

	early := time.Now().Add(5 * time.Minute)
	late := time.Now().Add(30 * time.Minute)
	// a1 解得晚、a2 解得早：Reason 必须来自 a2（最早解封那条），不能停在 a1 的锁因。
	p.CooldownSoftForModel("a1", time.Minute, late, "m1", `{"code":6004,"msg":"用量超出"}`)
	p.CooldownSoftForModel("a2", time.Minute, early, "m1", "14003 model busy")

	st := p.ModelBlocked("m1")
	if !st.Blocked {
		t.Fatal("应判定全池被挡")
	}
	if st.Count != 2 {
		t.Fatalf("Count=%d want 2", st.Count)
	}
	if len(st.Reasons) != 2 {
		t.Fatalf("Reasons=%v want 两条（每个被挡账号一条）", st.Reasons)
	}
	if st.Reason != "14003 model busy" {
		t.Fatalf("Reason=%q want 最早解封那条（14003 model busy）", st.Reason)
	}
	if !st.Until.Equal(early) {
		t.Fatalf("Until=%v want %v", st.Until, early)
	}
}

// 全都没有解封时刻（如 14003 无墙钟的有界退避）时退回第一条，保持「Blocked 必有 Reason」。
func TestModelBlockedReasonFallbackWithoutUntil(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.CooldownModelBusy("a1", "m1", "14003 model busy")

	st := p.ModelBlocked("m1")
	if !st.Blocked {
		t.Fatal("应判定全池被挡")
	}
	if st.Reason == "" {
		t.Fatal("没有 Until 时 Reason 仍应有值（退回第一条）")
	}
	if len(st.Reasons) != 1 || st.Reasons[0] != st.Reason {
		t.Fatalf("Reasons=%v Reason=%q", st.Reasons, st.Reason)
	}
}
