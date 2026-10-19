package server

import "testing"

// 上游没给 usage.credit 时不得记账：此前把「缺失」当 0 → NoteModelCost 记下
// per1k=0 → 该 (账号,模型) 被标成 tier 0「已实测免费」，而 pick.go 的 costTier
// 是硬过滤（只留最优层）→ 6 小时 TTL 内该模型全部流量压到一个号上。
func TestUsageCreditTotalRequiresCreditField(t *testing.T) {
	cases := []struct {
		name   string
		resp   map[string]any
		wantOK bool
	}{
		{"有 credit", map[string]any{"usage": map[string]any{"credit": 1.5, "total_tokens": 100.0}}, true},
		{"缺 credit", map[string]any{"usage": map[string]any{"total_tokens": 100.0}}, false},
		{"credit 为 null", map[string]any{"usage": map[string]any{"credit": nil, "total_tokens": 100.0}}, false},
		{"credit 是字符串", map[string]any{"usage": map[string]any{"credit": "1.5", "total_tokens": 100.0}}, false},
		{"缺 usage", map[string]any{}, false},
		{"缺 total_tokens", map[string]any{"usage": map[string]any{"credit": 1.0}}, false},
		{"total_tokens 为 0", map[string]any{"usage": map[string]any{"credit": 1.0, "total_tokens": 0.0}}, false},
		{"credit 真的是 0", map[string]any{"usage": map[string]any{"credit": 0.0, "total_tokens": 10.0}}, true},
	}
	for _, c := range cases {
		credit, total, ok := usageCreditTotal(c.resp)
		if ok != c.wantOK {
			t.Errorf("%s: ok=%v want %v", c.name, ok, c.wantOK)
			continue
		}
		if ok && total <= 0 {
			t.Errorf("%s: total=%d", c.name, total)
		}
		if ok && credit < 0 {
			t.Errorf("%s: credit=%v", c.name, credit)
		}
	}
}
