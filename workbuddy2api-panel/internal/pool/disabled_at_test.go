package pool

// disabled_at：禁用时刻的记录与清空（上游 2026-10-24 `4b489a4`）。
//
// 只看 reason 分不清「刚被禁」和「禁了一周没人管」，而两者处置优先级差很远
// （后者往往意味着没人看面板）。所以禁用时记时刻、解冻时清掉、落盘要能带过去。

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestDisabledAtSetOnDisableAndClearedOnRevive(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})

	if st, _ := p.Status("u1"); st.DisabledAt != "" {
		t.Fatalf("未禁用时不该有 disabled_at，实际 %q", st.DisabledAt)
	}

	p.Disable("u1", "面板手动禁用（管理员操作）")
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("账号应在池中")
	}
	if !st.Disabled {
		t.Fatal("应已禁用")
	}
	if st.DisabledAt == "" {
		t.Fatal("禁用后必须记录 disabled_at")
	}
	// 必须是可解析的 RFC3339，且落在「刚刚」附近（时钟/时区口径写错会落到别处）。
	ts, err := time.Parse(time.RFC3339, st.DisabledAt)
	if err != nil {
		t.Fatalf("disabled_at 不是 RFC3339: %q (%v)", st.DisabledAt, err)
	}
	if d := time.Since(ts); d < -time.Minute || d > time.Minute {
		t.Fatalf("disabled_at 偏离当前时刻 %v", d)
	}
	if st.DisabledReason != "面板手动禁用（管理员操作）" {
		t.Fatalf("reason=%q，自定义原因必须原样落库", st.DisabledReason)
	}

	p.ReviveDisabled("u1")
	st, _ = p.Status("u1")
	if st.Disabled {
		t.Fatal("解冻后不该还是禁用")
	}
	if st.DisabledAt != "" {
		t.Fatalf("解冻后 disabled_at 必须清空，实际 %q", st.DisabledAt)
	}
}

// 落盘要带上 disabled_at（重启后仍能看出禁了多久）。
func TestDisabledAtPersists(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.Disable("u1", "上游 12153 session dead")

	sf := p.stateOverviewLocked()
	acc, ok := sf.Accounts["u1"]
	if !ok {
		t.Fatal("state 里应有该账号")
	}
	if acc.DisabledAt.IsZero() {
		t.Fatal("state 里必须落 disabled_at")
	}

	// 重新加载到新池：disabled_at 要能被读回来（否则重启后「禁了多久」丢失）。
	p2 := New("")
	p2.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p2.applyAccountsLocked(sf.Accounts)
	st, _ := p2.Status("u1")
	if st.DisabledAt == "" {
		t.Fatal("重启后 disabled_at 丢了")
	}
}
