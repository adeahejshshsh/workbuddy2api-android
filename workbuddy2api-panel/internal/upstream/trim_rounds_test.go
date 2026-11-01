package upstream

import (
	"encoding/json"
	"testing"
)

// prompt.keep_recent_rounds：按**整轮**裁剪，system 全留，tool 配对不被切开。
func TestTrimRounds(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","messages":[
		{"role":"system","content":"人格"},
		{"role":"user","content":"u1"},
		{"role":"assistant","content":"a1"},
		{"role":"user","content":"u2"},
		{"role":"assistant","content":"","tool_calls":[{"id":"c1"}]},
		{"role":"tool","tool_call_id":"c1","content":"r2"},
		{"role":"user","content":"u3"},
		{"role":"assistant","content":"a3"}
	],"stream":true}`)

	// 关（0）：逐字节不变
	if got := TrimRounds(body, 0); string(got) != string(body) {
		t.Fatal("keep=0 时应原样返回")
	}
	// 保留最近 1 轮：system + u3 之后
	out := TrimRounds(body, 1)
	var root struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	var roles []string
	for _, m := range root.Messages {
		roles = append(roles, m.Role)
	}
	want := []string{"system", "user", "assistant"}
	if len(roles) != len(want) {
		t.Fatalf("roles=%v want %v", roles, want)
	}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("roles=%v want %v", roles, want)
		}
	}
	// 其余字段不动
	if !root.Stream {
		t.Error("stream 字段被吃掉了")
	}
	// 保留最近 2 轮：从 u2 起 → 不会切出孤立的 tool（u2 那轮的 tool_calls+tool 成对保留）
	out2 := TrimRounds(body, 2)
	var r2 struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(out2, &r2)
	hasTool := false
	for _, m := range r2.Messages {
		if m.Role == "tool" {
			hasTool = true
		}
	}
	if !hasTool {
		t.Error("保留 2 轮时应带上 u2 那条 tool 结果（整轮边界）")
	}
	// 轮数不足：不裁
	if got := TrimRounds(body, 10); string(got) != string(body) {
		t.Error("轮数不足时应原样返回")
	}
	// 坏 JSON：原样返回（不 panic）
	if got := TrimRounds([]byte("{bad"), 2); string(got) != "{bad" {
		t.Error("非法 JSON 应原样返回")
	}
}
