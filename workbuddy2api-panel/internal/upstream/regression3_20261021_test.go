package upstream

import "testing"

// BUG-05：prompt_cache_miss_tokens 的 0 值别名不可信 —— 采信它会让命中率显示 100%，
// 并让「大前缀整段重算」的告警永不触发。
func TestUsageCacheMissTokensIgnoresZeroAlias(t *testing.T) {
	usage := map[string]any{
		"prompt_tokens":            float64(1000),
		"prompt_cache_hit_tokens":  float64(200),
		"prompt_cache_miss_tokens": float64(0), // 0 值兼容别名
	}
	miss, ok := UsageCacheMissTokens(usage)
	if !ok {
		t.Fatal("应当能推导出 miss")
	}
	if miss != 800 {
		t.Fatalf("miss=%v，期望按 prompt-hit 推导出 800（0 值别名不可采信）", miss)
	}
	// 非 0 的显式值仍然优先
	usage["prompt_cache_miss_tokens"] = float64(123)
	if miss, ok := UsageCacheMissTokens(usage); !ok || miss != 123 {
		t.Fatalf("显式值应优先：miss=%v ok=%v", miss, ok)
	}
}
