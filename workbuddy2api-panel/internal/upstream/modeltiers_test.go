package upstream

import "testing"

// issue #235：/v3/config 的 data.modelTiers 要挂到目录条目上——海外「Pro Only」
// 与国内「订阅优先」都只是官方文案，模型是否可调由账号决定，这里只负责标注。
func TestApplyModelTiers(t *testing.T) {
	out := map[string]ModelInfo{
		"gpt-6.1-sol":    {ID: "gpt-6.1-sol"},
		"glm-5.3":        {ID: "glm-5.3"},
		"unlisted-model": {ID: "unlisted-model"},
	}
	tiers := []v3ModelTier{
		{Enabled: true, Priority: 200, ModelIDs: []string{"gpt-6.1-sol", "已下架"},
			Badge: &struct {
				Label string `json:"label"`
			}{Label: "Pro Only"}},
		{Enabled: false, Priority: 300, ModelIDs: []string{"glm-5.3"},
			Badge: &struct {
				Label string `json:"label"`
			}{Label: "停用的层级"}},
		{Enabled: true, Priority: 20, ModelIDs: []string{"glm-5.3"},
			Badge: &struct {
				Label string `json:"label"`
			}{Label: "订阅优先"}},
	}
	applyModelTiers(out, tiers)

	if got := out["gpt-6.1-sol"].Tier; got != "Pro Only" {
		t.Fatalf("gpt-6.1-sol tier = %q", got)
	}
	if got := out["glm-5.3"].Tier; got != "订阅优先" {
		t.Fatalf("glm-5.3 tier = %q（enabled=false 的层级不该生效）", got)
	}
	if got := out["unlisted-model"].Tier; got != "" {
		t.Fatalf("未被任何层级列出的模型不该有 tier，got %q", got)
	}
}

// primary-model 上游 modelTiers 漏标、但三个 global 号实测 11158 拒（2026-10-23）：
// 空 tiers 也要挂上；上游给了文案时以上游为准。
func TestApplyModelTiersProOnlyFallback(t *testing.T) {
	out := map[string]ModelInfo{"primary-model": {ID: "primary-model"}}
	applyModelTiers(out, nil)
	if got := out["primary-model"].Tier; got != "Pro Only" {
		t.Fatalf("空 tiers 时 primary-model tier = %q，want Pro Only", got)
	}

	// 上游若收录了它，用上游文案，不被兜底覆盖。
	out2 := map[string]ModelInfo{"primary-model": {ID: "primary-model"}}
	applyModelTiers(out2, []v3ModelTier{{Enabled: true, Priority: 1, ModelIDs: []string{"primary-model"},
		Badge: &struct {
			Label string `json:"label"`
		}{Label: "订阅优先"}}})
	if got := out2["primary-model"].Tier; got != "订阅优先" {
		t.Fatalf("上游文案被兜底覆盖：tier = %q", got)
	}

	// 目录里没有该模型时不凭空造条目。
	out3 := map[string]ModelInfo{"other": {ID: "other"}}
	applyModelTiers(out3, nil)
	if _, ok := out3["primary-model"]; ok {
		t.Fatal("目录外模型不该被兜底补进来")
	}
}

// 同模型命中多条 enabled 层级时取 priority 最高那条的文案。
func TestApplyModelTiersPriority(t *testing.T) {
	out := map[string]ModelInfo{"m": {ID: "m"}}
	badge := func(s string) *struct {
		Label string `json:"label"`
	} {
		return &struct {
			Label string `json:"label"`
		}{Label: s}
	}
	applyModelTiers(out, []v3ModelTier{
		{Enabled: true, Priority: 10, ModelIDs: []string{"m"}, Badge: badge("低优先")},
		{Enabled: true, Priority: 90, ModelIDs: []string{"m"}, Badge: badge("高优先")},
	})
	if out["m"].Tier != "高优先" {
		t.Fatalf("tier = %q", out["m"].Tier)
	}
}
