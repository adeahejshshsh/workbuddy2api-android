package server

import "testing"

// 报告 #4：非流式缓存命中率不能把「miss 推不出来」当 miss=0
// （那会让 CacheHitRate 恒 100%，缓存告警永不触发）。
func TestCacheTokensFromUsageMissDerivation(t *testing.T) {
	// hit + miss 都有：原样返回
	if h, m, ok := cacheTokensFromUsage(map[string]any{
		"prompt_cache_hit_tokens": 900.0, "prompt_cache_miss_tokens": 100.0,
	}); !ok || h != 900 || m != 100 {
		t.Fatalf("hit+miss: %d/%d ok=%v", h, m, ok)
	}
	// 只有 hit + prompt：按 prompt-hit 推导 miss（与流式侧同口径）
	if h, m, ok := cacheTokensFromUsage(map[string]any{
		"prompt_tokens": 1000.0, "prompt_cache_hit_tokens": 900.0,
	}); !ok || h != 900 || m != 100 {
		t.Fatalf("hit+prompt 推导: %d/%d ok=%v, 期望 900/100/true", h, m, ok)
	}
	// 只有 hit、没有 prompt：推不出 miss → ok=false（不参与命中率，别报假 100%）
	if _, _, ok := cacheTokensFromUsage(map[string]any{
		"prompt_cache_hit_tokens": 900.0,
	}); ok {
		t.Fatal("推不出 miss 时必须 ok=false，否则命中率恒 100%")
	}
	// 只有 miss：按 prompt-miss 反推 hit（上游 PR #199 的对称口径）
	if h, m, ok := cacheTokensFromUsage(map[string]any{
		"prompt_tokens": 1000.0, "prompt_cache_miss_tokens": 100.0,
	}); !ok || h != 900 || m != 100 {
		t.Fatalf("miss 反推 hit: %d/%d ok=%v", h, m, ok)
	}
}
