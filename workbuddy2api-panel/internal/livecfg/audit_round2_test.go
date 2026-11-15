package livecfg

// 第二轮代码审计（CODE-AUDIT-FULL.md 10.9 范围内 livecfg 的「热快照字段
// 遗漏 / 口径不一致」方向）的回归用例。

import (
	"testing"
	"time"
)

// ------------------------------------------------- 隐藏模型表的大小写/域口径 ----

// 大小写不敏感的前缀 + 双拼写登记：面板用精确 id 查表，服务端用 EqualFold 判定。
func TestNormalizeHiddenModelsCaseAndRealm(t *testing.T) {
	got := NormalizeHiddenModels([]string{
		"GLM-5.2",       // 裸名：两域都藏，且大小写两种拼写都登记
		"CN:alpha-1",    // 大写前缀：只藏 cn
		"global:BETA-2", // 小写前缀 + 大写模型名：只藏 global，两种拼写都登记
		"",              // 空条目跳过
		"   ",           // 空白跳过
	})
	if got == nil {
		t.Fatal("非空输入不该返回 nil")
	}
	want := []string{
		"cn:GLM-5.2", "cn:glm-5.2", "global:GLM-5.2", "global:glm-5.2",
		"cn:alpha-1",
		"global:BETA-2", "global:beta-2",
	}
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("缺键 %q（得到 %v）", k, got)
		}
	}
	// 域作用域必须准确：带前缀的条目不能把另一域也藏了。
	for _, k := range []string{"global:alpha-1", "cn:BETA-2", "cn:beta-2"} {
		if _, ok := got[k]; ok {
			t.Errorf("%q 不该存在（带前缀条目只能影响自己那一域；此前 `CN:x` 被当成裸名，两域都藏且永远匹配不上）", k)
		}
	}
}

// 模型名本身可以含冒号（`qwen:7b`）：不能把任何含冒号的条目都当带前缀。
func TestNormalizeHiddenModelsKeepsColonInModelName(t *testing.T) {
	got := NormalizeHiddenModels([]string{"qwen:7b"})
	for _, k := range []string{"cn:qwen:7b", "global:qwen:7b"} {
		if _, ok := got[k]; !ok {
			t.Fatalf("含冒号的裸模型名应两域登记，缺 %q（得到 %v）", k, got)
		}
	}
	if _, ok := got["qwen:7b"]; ok {
		t.Fatal("不该产生无域前缀的键")
	}
}

func TestNormalizeHiddenModelsEmpty(t *testing.T) {
	if got := NormalizeHiddenModels(nil); got != nil {
		t.Fatalf("nil 输入应返回 nil, 得到 %v", got)
	}
	if got := NormalizeHiddenModels([]string{"", "  "}); got != nil {
		t.Fatalf("全空条目应返回 nil, 得到 %v", got)
	}
}

// 别名表：前缀大小写不敏感（既有行为，钉住别回退）。
func TestNormalizeModelAliasesCaseInsensitiveRealm(t *testing.T) {
	got := NormalizeModelAliases([]string{"CN:Claude-* = glm-5.2"})
	if got["cn:claude-*"] != "glm-5.2" {
		t.Fatalf("别名键应小写且只登记 cn: %v", got)
	}
	if _, ok := got["global:claude-*"]; ok {
		t.Fatalf("带前缀的别名不该登记到 global: %v", got)
	}
}

// ------------------------------------------------------- 快照不可变（热口径） ----

// Store 必须克隆 map：调用方（配置装配/保存路径）后续改写同一个 map 时，
// 已发布的快照不能被带着变（否则读方看到半更新的别名表/隐藏表）。
func TestStoreClonesMaps(t *testing.T) {
	aliases := map[string]string{"cn:a": "b"}
	fallback := map[string]string{"cn:x": "y"}
	defaults := map[string]string{"cn:m": "high"}
	hidden := map[string]struct{}{"cn:h": {}}

	h := New(Snapshot{
		ModelAliases:          aliases,
		ModelFallback:         fallback,
		ThinkingModelDefaults: defaults,
		HiddenModels:          hidden,
	})
	// 发布后改写调用方手里的 map。
	aliases["cn:a"] = "CHANGED"
	aliases["cn:new"] = "leaked"
	fallback["cn:x"] = "CHANGED"
	defaults["cn:m"] = "low"
	hidden["cn:other"] = struct{}{}

	got := h.Load()
	if got.ModelAliases["cn:a"] != "b" {
		t.Fatalf("快照被调用方改写污染: %v", got.ModelAliases)
	}
	if _, ok := got.ModelAliases["cn:new"]; ok {
		t.Fatalf("调用方新增键漏进了快照: %v", got.ModelAliases)
	}
	if got.ModelFallback["cn:x"] != "y" || got.ThinkingModelDefaults["cn:m"] != "high" {
		t.Fatalf("map 未被克隆: fallback=%v defaults=%v", got.ModelFallback, got.ThinkingModelDefaults)
	}
	if _, ok := got.HiddenModels["cn:other"]; ok {
		t.Fatalf("hidden 未被克隆: %v", got.HiddenModels)
	}
}

// nil map 必须保持 nil（调用方用 nil 判定「没配」）。
func TestStoreKeepsNilMaps(t *testing.T) {
	h := New(Snapshot{APIKey: "k"})
	got := h.Load()
	if got.ModelAliases != nil || got.ModelFallback != nil || got.HiddenModels != nil || got.ThinkingModelDefaults != nil {
		t.Fatalf("nil map 被改成了空 map: %+v", got)
	}
	if got.APIKey != "k" {
		t.Fatalf("标量字段丢失: %+v", got)
	}
}

// 快照整体替换：旧值不残留（防止「保存配置后部分字段还是旧值」）。
func TestStoreReplacesWholeSnapshot(t *testing.T) {
	h := New(Snapshot{APIKey: "old", SoftCooldown: time.Minute})
	h.Store(Snapshot{APIKey: "new"})
	got := h.Load()
	if got.APIKey != "new" {
		t.Fatalf("APIKey=%q", got.APIKey)
	}
	if got.SoftCooldown != 0 {
		t.Fatalf("整体替换后旧字段应清零, SoftCooldown=%v", got.SoftCooldown)
	}
}

// nil Holder / 未 Store 的 Holder 必须返回零值快照（调用方无需判空）。
func TestLoadZeroSnapshot(t *testing.T) {
	var h *Holder
	if got := h.Load(); got.APIKey != "" {
		t.Fatalf("nil Holder 应返回零值: %+v", got)
	}
	if got := (&Holder{}).Load(); got.APIKey != "" {
		t.Fatalf("未 Store 的 Holder 应返回零值: %+v", got)
	}
}
