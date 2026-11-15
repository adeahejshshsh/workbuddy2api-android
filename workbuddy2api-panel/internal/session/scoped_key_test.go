package session

import "testing"

// 显式会话键必须按调用方隔离（审查 A5）：否则任一客户端复用他人的 conversation_id
// 就能把受害者会话钉到自己选的账号（或抢占键让受害者被钉到攻击者账号上）。
func TestExtractKeyScopedByCaller(t *testing.T) {
	body := []byte(`{"metadata":{"conversation_id":"conv-1"},"messages":[]}`)
	a := ExtractKeyScoped(body, "callerA")
	b := ExtractKeyScoped(body, "callerB")
	if a == "" || b == "" {
		t.Fatal("显式会话键不该为空")
	}
	if a == b {
		t.Fatalf("不同调用方的同一 conversation_id 必须得到不同键，实际都是 %q", a)
	}
	// caller 为空 = 不隔离：保持旧行为（单租户/未配 key）。
	if got := ExtractKeyScoped(body, ""); got != ExtractKey(body) {
		t.Fatalf("caller 为空时应与 ExtractKey 一致：%q vs %q", got, ExtractKey(body))
	}
	// prompt_cache_key 同样隔离。
	pc := []byte(`{"prompt_cache_key":"pk-1"}`)
	if ExtractKeyScoped(pc, "callerA") == ExtractKeyScoped(pc, "callerB") {
		t.Fatal("prompt_cache_key 也必须按调用方隔离")
	}
}
