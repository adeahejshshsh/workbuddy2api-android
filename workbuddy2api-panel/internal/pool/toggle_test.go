package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 手动关闭 (账号, 模型) 的语义：永久、稀疏、手动优先。
//
// 为什么需要这组测试：这是**人工意图**，与上游触发的临时冷却（6004/11102）正交。
// 最容易回归的点是「自动机制（冷却到期/成功清除）把手动关闭顺手解了」——
// 用户一旦发现"我关了它，过一会又开了"，这个开关就彻底失去信任。

// TestModelOffSetAndClear 基本往返：关 → 在列 → 解 → 不在列。
func TestModelOffSetAndClear(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	if got := p.ModelOffOf("u1"); got != nil {
		t.Fatalf("初始应为 nil，实际 %+v", got)
	}
	if !p.SetModelOff("u1", "gpt-4", "上游老 404") {
		t.Fatal("SetModelOff 应成功")
	}
	got := p.ModelOffOf("u1")
	if len(got) != 1 || got[0].Model != "gpt-4" || got[0].Note != "上游老 404" {
		t.Fatalf("ModelOffOf = %+v, want 1 条 gpt-4（带备注）", got)
	}
	if !p.SetModelOn("u1", "gpt-4") {
		t.Fatal("SetModelOn 应报告有变更")
	}
	if got := p.ModelOffOf("u1"); len(got) != 0 {
		t.Fatalf("解除后应为空，实际 %+v", got)
	}
}

// TestModelOffIdempotent 幂等性：重复解不算变更；未知账号一律 false。
func TestModelOffIdempotent(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	if p.SetModelOn("u1", "gpt-4") {
		t.Error("本来就没关，SetModelOn 不应报告变更")
	}
	if p.SetModelOff("nope", "gpt-4", "") {
		t.Error("未知账号 SetModelOff 应返回 false")
	}
	if p.SetModelOff("u1", "", "") {
		t.Error("空 model 应返回 false")
	}
	if p.SetModelOn("nope", "gpt-4") {
		t.Error("未知账号 SetModelOn 应返回 false")
	}
}

// TestModelOffBlocksHealthyForModel **核心语义**：手动关闭的组合在选路时不可用，
// 且该账号的其他模型不受影响（稀疏存储 = 精确到组合，不是整号禁用）。
func TestModelOffBlocksHealthyForModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetModelOff("u1", "gpt-4", "")

	p.mu.RLock()
	e := p.byUID["u1"]
	p.mu.RUnlock()

	now := time.Now()
	if e.healthyForModel(now, "gpt-4") {
		t.Error("手动关闭的模型不应被判为可用")
	}
	if !e.healthyForModel(now, "gpt-4-mini") {
		t.Error("同账号的其他模型应仍然可用（稀疏存储，不是整号禁用）")
	}
	// 空 model（未指定模型的路由）不应被模型级手动开关拦住。
	if !e.healthyForModel(now, "") {
		t.Error("空 reqModel 不应被模型级手动开关拦住")
	}
}

// TestModelOffBeatsAutoCooldown **手动优先**：关掉之后解除自动冷却也不能恢复，
// 反过来，自动冷却到期也不会把手动关闭顺手清掉。
//
// 判据是两条独立的路径都要成立：手动关 + 手动解 = 唯一开关方式。
func TestModelOffBeatsAutoCooldown(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetModelOff("u1", "gpt-4", "")

	p.mu.RLock()
	e := p.byUID["u1"]
	p.mu.RUnlock()

	// 模拟上游写入一条已过期的模型冷却（自动机制已恢复）。
	e.modelCooldowns = map[string]modelCooldown{
		"gpt-4": {Until: time.Now().Add(-time.Minute)},
	}
	if e.healthyForModel(time.Now(), "gpt-4") {
		t.Error("自动冷却到期不应让手动关闭的组合恢复可用（手动优先）")
	}

	// 手动解除后，该模型才真正可用。
	p.SetModelOn("u1", "gpt-4")
	p.mu.RLock()
	e = p.byUID["u1"]
	p.mu.RUnlock()
	if !e.healthyForModel(time.Now(), "gpt-4") {
		t.Error("手动解除后应恢复可用")
	}
}

// TestModelOffListSorted 输出稳定（按模型名升序）—— 面板列表不能因 map 遍历而抖动。
func TestModelOffListSorted(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	for _, m := range []string{"zeta", "alpha", "mid"} {
		p.SetModelOff("u1", m, "")
	}
	got := p.ModelOffOf("u1")
	want := []string{"alpha", "mid", "zeta"}
	if len(got) != len(want) {
		t.Fatalf("条数 %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Model != want[i] {
			t.Errorf("第 %d 条 = %q, want %q（应按名升序）", i, got[i].Model, want[i])
		}
	}
}
