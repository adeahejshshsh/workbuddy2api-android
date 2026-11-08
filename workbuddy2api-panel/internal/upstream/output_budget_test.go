package upstream

import (
	"encoding/json"
	"testing"
)

// 高档位思考 + 小额度 = 正文断在半句（上游 2b9b773 的生产实测）。
// 网关在出站前把 max_tokens 抬到该档下限；抬的是上限不是目标长度，不改变答案。

func TestEnsureMinOutputBudgetRaises(t *testing.T) {
	cases := []struct {
		effort string
		in     int64
		want   int64
	}{
		{"high", 2048, 8192},
		{"max", 2048, 16384},
		{"low", 1024, 4096},
		{"medium", 4096, 6144},
		{"xhigh", 8000, 12288},
	}
	for _, tc := range cases {
		obj := map[string]any{"reasoning_effort": tc.effort, "max_tokens": json.Number("0")}
		obj["max_tokens"] = json.Number(itoaTest(tc.in))
		ensureMinOutputBudget(obj)
		got, _ := maxTokensValue(obj)
		if got != tc.want {
			t.Errorf("effort=%s max_tokens=%d → %d，期望 %d", tc.effort, tc.in, got, tc.want)
		}
	}
	// 大写档位与字符串额度也要认
	obj := map[string]any{"reasoningEffort": "HIGH", "max_tokens": "2048"}
	ensureMinOutputBudget(obj)
	if got, _ := maxTokensValue(obj); got != 8192 {
		t.Errorf("大写档位/字符串额度：%d want 8192", got)
	}
}

func TestEnsureMinOutputBudgetLeavesAlone(t *testing.T) {
	cases := []struct {
		name string
		obj  map[string]any
	}{
		{"没有显式额度", map[string]any{"reasoning_effort": "high"}},
		{"额度已够", map[string]any{"reasoning_effort": "high", "max_tokens": json.Number("16384")}},
		{"探活级额度", map[string]any{"reasoning_effort": "high", "max_tokens": json.Number("1")}},
		{"关闭思考(type)", map[string]any{"reasoning_effort": "high", "max_tokens": json.Number("1024"),
			"thinking": map[string]any{"type": "disabled"}}},
		{"关闭思考(off)", map[string]any{"reasoning_effort": "off", "max_tokens": json.Number("1024")}},
		{"档位未知", map[string]any{"reasoning_effort": "weird", "max_tokens": json.Number("1024")}},
		{"没有档位", map[string]any{"max_tokens": json.Number("1024")}},
	}
	for _, tc := range cases {
		before, _ := maxTokensValue(tc.obj)
		ensureMinOutputBudget(tc.obj)
		after, _ := maxTokensValue(tc.obj)
		if before != after {
			t.Errorf("%s：不该改（%d → %d）", tc.name, before, after)
		}
	}
}

// max_completion_tokens 路径也要抬，且不擅自换成 max_tokens。
func TestEnsureMinOutputBudgetKeepsFieldName(t *testing.T) {
	obj := map[string]any{"reasoning_effort": "high", "max_completion_tokens": json.Number("2048")}
	ensureMinOutputBudget(obj)
	if _, has := obj["max_tokens"]; has {
		t.Fatal("不该自造 max_tokens 键")
	}
	if got, _ := maxTokensValue(obj); got != 8192 {
		t.Fatalf("max_completion_tokens 没被抬：%d", got)
	}
}

func itoaTest(v int64) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}
