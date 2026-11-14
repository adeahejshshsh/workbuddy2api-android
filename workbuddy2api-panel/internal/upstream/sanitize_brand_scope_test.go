package upstream

// features.sanitize_brand_scope：身份类规则（品牌词 / 身份句式）的作用域。
//
// 缺省 "all"（所有角色）与旧行为逐字节一致；"identity" 只改 system/developer。
// 动机：实测 user 消息里的品牌词不触发上游 11‑-128，而把用户自己写的内容改掉是
// 不可见的数据损坏（用户在讲 DeepSeek Harness 时，回看历史发现词被改了）。

import (
	"encoding/json"
	"strings"
	"testing"
)

const brandWord = "DeepSeek Harness"

func userMsg(text string) []any {
	return []any{map[string]any{"role": "user", "content": text}}
}

func systemMsg(text string) []any {
	return []any{map[string]any{"role": "system", "content": text}}
}

func firstContent(t *testing.T, msgs []any) string {
	t.Helper()
	m := msgs[0].(map[string]any)
	s, _ := m["content"].(string)
	return s
}

// TestBrandScopeAllRewritesUserMessages：缺省口径（all）—— 用户消息也改写，旧行为不变。
func TestBrandScopeAllRewritesUserMessages(t *testing.T) {
	msgs := userMsg("I use " + brandWord + " daily")
	if !sanitizeMessages(msgs, true) {
		t.Fatal("all 口径下应发生改写")
	}
	if got := firstContent(t, msgs); strings.Contains(got, brandWord) {
		t.Fatalf("all 口径下用户消息里的品牌词应被改写：%q", got)
	}
}

// TestBrandScopeIdentityLeavesUserMessagesAlone：identity 口径 —— 用户消息原样透传。
func TestBrandScopeIdentityLeavesUserMessagesAlone(t *testing.T) {
	const text = "I use " + brandWord + " daily"
	msgs := userMsg(text)
	if sanitizeMessages(msgs, false) {
		t.Fatalf("identity 口径下用户消息不该被改写：%q", firstContent(t, msgs))
	}
	if got := firstContent(t, msgs); got != text {
		t.Fatalf("identity 口径下用户消息必须逐字节保留，got %q", got)
	}
}

// TestBrandScopeIdentityStillRewritesSystem：system/developer 始终按身份消息处理。
func TestBrandScopeIdentityStillRewritesSystem(t *testing.T) {
	msgs := systemMsg("harness:identity " + brandWord)
	if !sanitizeMessages(msgs, false) {
		t.Fatal("identity 口径下 system 消息仍应被改写")
	}
	if got := firstContent(t, msgs); strings.Contains(got, brandWord) {
		t.Fatalf("system 里的品牌词应被改写：%q", got)
	}

	// developer 角色同口径（Responses 的开发者消息）。
	dev := []any{map[string]any{"role": "developer", "content": brandWord}}
	if !sanitizeMessages(dev, false) {
		t.Fatal("identity 口径下 developer 消息也应被改写")
	}
}

// TestBrandScopeIdentityKeepsGlobalRulesOnUser：identity 只放行「身份类」规则，
// 全局规则（数字错误码 / header 键值）对用户消息照常生效。
func TestBrandScopeIdentityKeepsGlobalRulesOnUser(t *testing.T) {
	msgs := userMsg("error code 11128 and x-anthropic-billing-hdr: abc")
	sanitizeMessages(msgs, false)
	got := firstContent(t, msgs)
	if strings.Contains(got, "11128") {
		t.Fatalf("数字错误码应仍被改写（全局规则）：%q", got)
	}
	if strings.Contains(got, "x-anthropic-billing-hdr") {
		t.Fatalf("header 键值应仍被剥离（全局规则）：%q", got)
	}
}

// TestBrandScopeToolsAlwaysIdentity：工具描述与 system prompt 同源（客户端作者写的
// 固定模板句），identity 口径下仍按身份消息处理 —— 否则这里就是 11‑-128 的漏口。
func TestBrandScopeToolsAlwaysIdentity(t *testing.T) {
	tools := []any{map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "helper",
			"description": "Runs inside " + brandWord,
		},
	}}
	brandAll := false
	sanitizeTools(tools, brandAll)
	desc := tools[0].(map[string]any)["function"].(map[string]any)["description"].(string)
	if strings.Contains(desc, brandWord) {
		t.Fatalf("工具描述里的品牌词应被改写（身份口径），got %q", desc)
	}
}

// TestPrepareBodyBrandScope：整条出站管线按 brandAll 生效，且旧入口（无作用域参数）
// 仍是 all 口径 —— 这条保证「不配新键的部署行为逐字节不变」。
func TestPrepareBodyBrandScope(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"` + brandWord + `"}]}`)

	// 旧公开入口：all（改写）。
	out := PrepareBodyOpt(body, true)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	msgs := obj["messages"].([]any)
	if got := msgs[0].(map[string]any)["content"].(string); strings.Contains(got, brandWord) {
		t.Fatalf("PrepareBodyOpt 应保持 all 口径（改写用户消息），got %q", got)
	}

	// 新管线：identity（不动用户消息）。
	out2 := prepareBodyOptFull(body, true, nil, nil, "", nil, false, nil)
	if err := json.Unmarshal(out2, &obj); err != nil {
		t.Fatal(err)
	}
	msgs = obj["messages"].([]any)
	if got := msgs[0].(map[string]any)["content"].(string); got != brandWord {
		t.Fatalf("identity 口径下用户消息应原样透传，got %q", got)
	}
}
