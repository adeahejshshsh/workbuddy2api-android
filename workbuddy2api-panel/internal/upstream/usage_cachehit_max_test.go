package upstream

// 缓存命中数在 usage 里有多个同义字段，刷新时机不同。此前取**第一个正值**，
// 于是「先到的那个是上一轮残留的小值」时命中数偏小 —— 命中率算低、成本算高，
// 而命中率正是 budget.go 缓存率闸（cache_low_rate）的判据，会传导成「误判低命中 → 冷却」。

import "testing"

func TestBestUsageCacheHitTakesMax(t *testing.T) {
	usage := map[string]any{
		// 第一个路径（prompt_tokens_details.cached_tokens）是残留小值
		"prompt_tokens_details": map[string]any{"cached_tokens": float64(100)},
		// 真正的值在这里，更大
		"prompt_cache_hit_tokens": float64(900),
	}
	got, ok := bestUsageCacheHitTokens(usage)
	if !ok || got != 900 {
		t.Fatalf("best=%v ok=%v want 900（应取最大正值，不是第一个正值）", got, ok)
	}
}

func TestBestUsageCacheHitNoPositive(t *testing.T) {
	if _, ok := bestUsageCacheHitTokens(map[string]any{"prompt_cache_hit_tokens": float64(0)}); ok {
		t.Fatal("全为 0 时应报未观测（ok=false）")
	}
}
