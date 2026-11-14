package pool

import (
	"path/filepath"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// ParkMany 移出的号连同状态一起落盘；文件缺席期间重启，号回来时禁用/分组照样复活。
//
// 变异判据：stateOverviewLocked 不写 Parked，或 upsertLocked 不查 parked，本用例红。
func TestParkedAccountSurvivesRestart(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "人工禁用")
	p.SetGroup("u1", "vip")
	if got := p.ParkMany([]string{"u1", "nope"}); len(got) != 1 || got[0] != "u1" {
		t.Fatalf("ParkMany=%v want [u1]", got)
	}
	if _, ok := p.Status("u1"); ok {
		t.Fatal("暂存后不该还在池里")
	}

	p2 := New(fp) // 重启
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok || !st.Disabled || st.Group != "vip" {
		t.Fatalf("重启后文件回来，状态应复活：ok=%v %+v", ok, st)
	}
	p2.Flush()

	p3 := New(fp) // 复活后不应再留在 parked 里
	if len(p3.parked) != 0 {
		t.Fatalf("复活后 parked 应清空：%d", len(p3.parked))
	}
}

// Reconcile：已在池的号只在磁盘令牌更晚过期时原地更新，指针不变。
func TestReconcileAdoptsOnlyNewerToken(t *testing.T) {
	p := New("")
	cur := &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 1000}
	if p.Reconcile(cur) != ReconcileAdded {
		t.Fatal("首次应为 Added")
	}
	if p.Reconcile(&auth.Auth{UID: "u1", AccessToken: "older", ExpiresAt: 900}) != ReconcileUnchanged {
		t.Fatal("更旧的磁盘令牌不该采用")
	}
	if p.Reconcile(&auth.Auth{UID: "u1", AccessToken: "at2", ExpiresAt: 2000}) != ReconcileUpdated {
		t.Fatal("更晚过期的磁盘令牌应原地更新")
	}
	if p.AuthByUID("u1") != cur || cur.AccessTokenValue() != "at2" {
		t.Fatal("应原地更新，不换指针")
	}
}
