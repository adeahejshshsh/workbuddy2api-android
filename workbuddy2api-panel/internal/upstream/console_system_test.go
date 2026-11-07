// console_system_test.go global 兜底 system 的两条钉：形态（首条必须是 system）与
// 内容（不声明身份）。
//
// 为什么要钉内容（上游 e4e6f5c / issue #187）：模型会把「被代理插入的身份声明」当成
// 提示注入而拒绝执行。兜底只需要满足上游「首条是 system」的渠道校验，正文写成身份句
// 是自找冲突 —— 与客户端 harness 自带的人设并列时尤其明显。
package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

func firstMessage(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var o struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(body, &o); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(o.Messages) == 0 {
		t.Fatal("messages 为空")
	}
	return o.Messages[0]
}

// TestEnsureConsoleSystemPrefersBehaviorOverIdentity 首条是 user → 前置一条只讲行为
// 约定的 system，且不出现任何身份声明。
func TestEnsureConsoleSystemPrefersBehaviorOverIdentity(t *testing.T) {
	in := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)
	first := firstMessage(t, ensureConsoleSystem(in))

	if role, _ := first["role"].(string); role != "system" {
		t.Fatalf("首条 role=%q want system（上游渠道校验要求）", role)
	}
	content, _ := first["content"].(string)
	if content != consoleSystemFallback {
		t.Errorf("content=%q want %q", content, consoleSystemFallback)
	}
	if strings.Contains(strings.ToLower(content), "you are") {
		t.Errorf("兜底 system 不该声明身份（会被模型当成注入）: %q", content)
	}
}

// TestEnsureConsoleSystemLeavesSystemAlone 首条已是 system → 一个字节都不动。
func TestEnsureConsoleSystemLeavesSystemAlone(t *testing.T) {
	in := []byte(`{"model":"m","messages":[{"role":"system","content":"You are Buddy."},{"role":"user","content":"hi"}]}`)
	if got := string(ensureConsoleSystem(in)); got != string(in) {
		t.Errorf("首条已是 system 却被改写:\n got %s\nwant %s", got, in)
	}
}
