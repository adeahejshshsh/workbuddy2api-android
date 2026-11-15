package upstream

import "encoding/json"

// normalizeUsageCacheAliases keeps cache-hit aliases consistent before the
// response leaves the gateway. Some WorkBuddy responses carry the real hit in
// prompt_tokens_details.cached_tokens while also emitting
// cache_read_input_tokens: 0 and cached_tokens: 0 compatibility aliases.
// Strict downstream parsers may prefer those zero aliases and lose the hit.
func normalizeUsageCacheAliases(usage map[string]any) map[string]any {
	best, ok := bestUsageCacheHitTokens(usage)
	if !ok || best <= 0 {
		return usage
	}

	out := cloneUsageMap(usage)
	out["cache_read_input_tokens"] = best
	out["cached_tokens"] = best
	out["prompt_cache_hit_tokens"] = best

	// prompt_tokens_details 只在**上游已给**时改写（审查 W4-U2-087）：此前无条件
	// clone + 写回，等于给上游根本没下发该字段的响应凭空造一个 `{cached_tokens:N}`
	// —— 与同函数内 input_tokens_details「不凭空造」的口径自相矛盾，严格解析的
	// 下游可能因为多出来的字段改变缓存判定（把「未观测」当成「观测到命中」）。
	if _, exists := out["prompt_tokens_details"]; exists {
		promptDetails := cloneUsageDetails(out, "prompt_tokens_details")
		promptDetails["cached_tokens"] = best
		out["prompt_tokens_details"] = promptDetails
	}

	// Responses API consumers use this nested form. Preserve it when the
	// upstream already supplies it, but do not invent it for Chat-only clients.
	if _, exists := out["input_tokens_details"]; exists {
		inputDetails := cloneUsageDetails(out, "input_tokens_details")
		inputDetails["cached_tokens"] = best
		out["input_tokens_details"] = inputDetails
	}

	return out
}

func bestUsageCacheHitTokens(usage map[string]any) (float64, bool) {
	paths := []struct {
		section string
		key     string
	}{
		{"prompt_tokens_details", "cached_tokens"},
		{"", "prompt_cache_hit_tokens"},
		{"", "cache_read_input_tokens"},
		{"", "cached_tokens"},
		{"input_tokens_details", "cached_tokens"},
	}

	// 取**最大**正值，不是第一个正值：同一个语义（缓存命中）上游会在多个字段里重复
	// 上报，而它们的刷新时机不同 —— 先命中的那个可能是上一轮残留的小值。取首个正值
	// 会让缓存命中数偏小（进而把「命中率」算低、把成本算高），而命中率又是缓存率闸
	// （budget.go 的 cache_low_rate）的判据，偏差会传导到「误判低命中 → 冷却 60s」。
	best := 0.0
	found := false
	for _, path := range paths {
		var value any
		if path.section == "" {
			value = usage[path.key]
		} else if details, ok := usage[path.section].(map[string]any); ok {
			value = details[path.key]
		}
		if tokens, ok := positiveUsageNumber(value); ok && tokens > best {
			best, found = tokens, true
		}
	}
	return best, found
}

func positiveUsageNumber(value any) (float64, bool) {
	switch n := value.(type) {
	case float64:
		return n, n > 0
	case float32:
		value := float64(n)
		return value, value > 0
	case int:
		return float64(n), n > 0
	case int64:
		return float64(n), n > 0
	case int32:
		return float64(n), n > 0
	case uint:
		return float64(n), n > 0
	case uint64:
		return float64(n), n > 0
	case uint32:
		return float64(n), n > 0
	case json.Number:
		// json.Number 只在解码侧用了 UseNumber 时出现（payload.go 主管线就是这么解
		// 请求体的）。补上它，两个函数才真正同一套类型（审查 W4-U2-088）。
		f, err := n.Float64()
		return f, err == nil && f > 0
	default:
		return 0, false
	}
}

func cloneUsageMap(usage map[string]any) map[string]any {
	out := make(map[string]any, len(usage))
	for key, value := range usage {
		out[key] = value
	}
	return out
}

func cloneUsageDetails(usage map[string]any, key string) map[string]any {
	out := make(map[string]any)
	details, _ := usage[key].(map[string]any)
	for detailKey, value := range details {
		out[detailKey] = value
	}
	return out
}

// UsagePromptTokens 返回 usage 顶层的 prompt_tokens；缺失或非数值时 ok=false。
func UsagePromptTokens(usage map[string]any) (float64, bool) {
	if usage == nil {
		return 0, false
	}
	return usageNumber(usage, "prompt_tokens")
}

// UsageCacheHitTokens 返回 usage 里的缓存命中 token 数（多别名取最优，口径与
// 回写给客户端的 normalizeUsageCacheAliases 一致）。供网关统计层（usage 桶 /
// reqlog）观测命中率使用；usage 缺失该维度时 ok=false。
func UsageCacheHitTokens(usage map[string]any) (float64, bool) {
	if usage == nil {
		return 0, false
	}
	return bestUsageCacheHitTokens(usage)
}

// UsageCacheMissTokens 返回 usage 里的缓存未命中 token 数：优先读上游显式的
// prompt_cache_miss_tokens，缺失时按 prompt_tokens - 命中 推导（推导值为负时
// 视为不可信，返回 ok=false）。
func UsageCacheMissTokens(usage map[string]any) (float64, bool) {
	if usage == nil {
		return 0, false
	}
	// 0 值别名不可信（BUG-05）：上游会同时下发真实命中数与 0 值的兼容别名
	// （本文件头已记录同形态的 cache_read_input_tokens:0 / cached_tokens:0）。
	// 直接采信 0 会让命中率显示 100%，并让 server 侧「大前缀整段重算」的告警永不触发。
	if miss, ok := usageNumber(usage, "prompt_cache_miss_tokens"); ok && miss > 0 {
		return miss, true
	}
	prompt, okP := usageNumber(usage, "prompt_tokens")
	hit, okH := UsageCacheHitTokens(usage)
	if okP && okH && prompt-hit >= 0 {
		return prompt - hit, true
	}
	return 0, false
}

// usageNumber 从 usage 顶层取数值字段（JSON 数字可能是 float64 / json.Number 形态）。
//
// 类型集合与 positiveUsageNumber 对齐（审查 W4-U2-088）：两个函数原先支持的
// 类型不同（前者只有 float64/int/int64/json.Number，后者多了 float32/int32/uint*），
// 于是**同一份 usage** 在「命中率统计」与「prompt_tokens 读取」两条路径上会一个
// ok 一个不 ok —— 表现为「有命中数但 miss 推导失败」，命中率闸拿到半截输入。
// 这里补齐成同一套。
func usageNumber(usage map[string]any, key string) (float64, bool) {
	v, ok := usage[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint64:
		return float64(n), true
	case uint32:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}
