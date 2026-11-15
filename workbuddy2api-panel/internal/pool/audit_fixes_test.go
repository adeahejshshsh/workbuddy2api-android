package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 一次 429 不能把已有的更长模型冷却砍短（审查 C4）。
func TestModelCooldownNeverShortens(t *testing.T) {
	p := New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"}
	p.Add(a)
	now := time.Now()
	// 先落一条长冷却（上游给的窗口 6h 后）。
	p.CooldownSoftForModel("u1", 30*time.Minute, now.Add(6*time.Hour), "m1", "long window")
	before := p.modelCooldownUntil("u1", "m1")
	if before.Before(now.Add(5 * time.Hour)) {
		t.Fatalf("长冷却没落上：until=%v", before)
	}
	// 再来一次 6004 的短窗（30 分钟）：不能把 Until 拉短。
	p.CooldownSoftForModel("u1", 30*time.Minute, now.Add(30*time.Minute), "m1", "model rate limit")
	if got := p.modelCooldownUntil("u1", "m1"); got.Before(before.Add(-time.Minute)) {
		t.Fatalf("短冷却把长冷却砍短了：%v → %v", before, got)
	}
}

// 换凭证（占位期 realm 为空 → 真实 realm）后必须重算规则（审查 C5）。
func TestUpsertRecomputesRulesAfterRealmKnown(t *testing.T) {
	p := New("")
	// 占位条目：realm 为空。
	p.byUID["u1"] = &entry{a: &auth.Auth{UID: "u1"}}
	// 一条只作用于 global 的拒绝规则。
	rule, err := p.UpsertRule(ModelRule{Name: "global-only", Mode: "deny", Models: []string{"global:m1"}})
	if err != nil {
		t.Fatalf("UpsertRule: %v", err)
	}
	if _, err := p.AssignRules("u1", []string{rule.ID}); err != nil {
		t.Fatalf("AssignRules: %v", err)
	}
	// 真实凭证到了：cn 域。规则不该命中 cn 模型。
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.cn"}) // cn 域由 domain 后缀决定
	if p.RuleBlocked("u1", "m1") {
		t.Fatal("cn 号被 global 规则拦住了：upsertLocked 没重算规则")
	}
}

// 窗口恢复时按 uid/model 重算 key，不一致的丢弃（审查 C6）。
func TestLoadWindowsDropsInconsistentKey(t *testing.T) {
	p := New("")
	now := time.Now()
	p.loadWindowsLocked(map[string]stateModelWindow{
		"bogus-key":           {UID: "u1", Model: "m1", ResetAt: now.Add(time.Hour)},
		windowKey("u2", "m2"): {UID: "u2", Model: "m2", ResetAt: now.Add(2 * time.Hour)},
	}, now)
	ws := p.ModelWindows()
	if len(ws) != 1 || ws[0].UID != "u2" {
		t.Fatalf("应只保留 key 一致的那条，实际 %+v", ws)
	}
}

// modelCooldownUntil 测试辅助：读 (uid, model) 的模型级冷却截止。
func (p *Pool) modelCooldownUntil(uid, model string) time.Time {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return time.Time{}
	}
	return e.modelCooldowns[model].Until
}
