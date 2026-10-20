package upstream

import (
	"encoding/json"
	"testing"
)

// asInt64 把 JSON 解出来的数值统一成 int64（float64/int64/int 都接受）。
func asInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return -1 << 62
}

// bug #1 回归：max_completion_tokens 只在**翻译成功**后才删别名。
// 此前第一行就 delete，小数/字符串形态既没翻译、也没保留 —— 上游回落默认上限（长任务被截），
// 而注释承诺的是"非数值别名原样透传由上游报 11101"。
func TestTranslateMaxCompletionTokensKeepsAliasWhenNotTranslatable(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantMax  int64 // 0 = 不该写 max_tokens
		wantKeep bool  // 别名是否应保留
	}{
		{"整数", `{"max_completion_tokens":1000}`, 1000, false},
		{"小数四舍五入", `{"max_completion_tokens":1000.5}`, 1001, false},
		{"字符串不可翻译", `{"max_completion_tokens":"1000"}`, 0, true},
		{"显式 max_tokens 优先", `{"max_completion_tokens":1000,"max_tokens":50}`, 50, false},
		{"非正数不可翻译", `{"max_completion_tokens":0}`, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var obj map[string]any
			if err := json.Unmarshal([]byte(c.in), &obj); err != nil {
				t.Fatal(err)
			}
			translateMaxCompletionTokens(obj)
			gotMax, hasMax := obj["max_tokens"]
			if c.wantMax == 0 {
				if c.name == "非正数不可翻译" {
					if hasMax {
						t.Errorf("不该写 max_tokens，got %v", gotMax)
					}
				}
			} else if n := asInt64(gotMax); n != c.wantMax {
				t.Errorf("max_tokens=%v(%T) want %d", gotMax, gotMax, c.wantMax)
			}
			_, hasAlias := obj["max_completion_tokens"]
			if hasAlias != c.wantKeep {
				t.Errorf("别名存在=%v want %v（obj=%v）", hasAlias, c.wantKeep, obj)
			}
		})
	}
}

// bug #19 回归：max_tokens=0/负数 不该被抬到 16（那是"用模型默认上限"的写法，
// 抬到 16 会把响应静默截断；让上游报 11133 更可诊断）。
func TestClampGPTMinMaxTokensLeavesZeroAndNegative(t *testing.T) {
	for _, v := range []float64{0, -1} {
		obj := map[string]any{"model": "gpt-6-sol", "max_tokens": v}
		clampGPTMinMaxTokens(obj)
		if got := obj["max_tokens"].(float64); got != v {
			t.Errorf("max_tokens=%v 不该被钳制，got %v", v, got)
		}
	}
	// 正但过小仍要钳（这是本函数的初衷：Claude Code 探针 1..15）
	obj := map[string]any{"model": "gpt-6-sol", "max_tokens": float64(8)}
	clampGPTMinMaxTokens(obj)
	if got := asInt64(obj["max_tokens"]); got != gptMinMaxTokens {
		t.Errorf("max_tokens=8 应钳到 %d，got %d", gptMinMaxTokens, got)
	}
}
