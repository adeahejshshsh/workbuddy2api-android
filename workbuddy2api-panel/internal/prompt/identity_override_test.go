package prompt

import (
	"encoding/json"
	"strings"
	"testing"
)

func firstSystem(t *testing.T, body []byte) string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("body 不是合法 JSON: %v", err)
	}
	msgs, _ := obj["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("没有 messages")
	}
	m, _ := msgs[0].(map[string]any)
	s, _ := m["content"].(string)
	return s
}

// 只替换开头 system 里以 "You are " 开头的**首句**，其余逐字保留。
func TestOverrideHarnessIdentityReplacesFirstSentence(t *testing.T) {
	body := []byte(`{"model":"x","messages":[
		{"role":"system","content":"You are DeepSeekRuntime, an AI assistant. 后续内容必须保留。"},
		{"role":"user","content":"hi"}]}`)
	out := OverrideHarnessIdentity(body, "You are CodeBuddy Code.")
	got := firstSystem(t, out)
	if !strings.HasPrefix(got, "You are CodeBuddy Code.") {
		t.Fatalf("首句没被替换: %q", got)
	}
	if !strings.Contains(got, "后续内容必须保留。") {
		t.Fatalf("首句之后的内容被动了: %q", got)
	}
	if strings.Contains(got, "DeepSeekRuntime") {
		t.Fatalf("旧身份还在: %q", got)
	}
}

// 词内句点不是句尾：v4.1 不能被切成 v4.
func TestOverrideHarnessIdentityKeepsVersionDots(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"You are DeepSeek v4.1 flash. Rest."}]}`)
	out := OverrideHarnessIdentity(body, "You are CodeBuddy Code.")
	got := firstSystem(t, out)
	if got != "You are CodeBuddy Code. Rest." {
		t.Fatalf("句末判定把版本号切断了: %q", got)
	}
}

// 对话中段的 system 不动（只认开头连续块）。
func TestOverrideHarnessIdentityIgnoresMidConversation(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"user","content":"hi"},
		{"role":"system","content":"You are Something Else."}]}`)
	out := OverrideHarnessIdentity(body, "You are CodeBuddy Code.")
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	msgs, _ := obj["messages"].([]any)
	m, _ := msgs[1].(map[string]any)
	if m["content"] != "You are Something Else." {
		t.Fatalf("对话中段的 system 被改了: %v", m["content"])
	}
}

// 多模态 content 数组：只改首个文本块。
func TestOverrideHarnessIdentityArrayContent(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":[
		{"type":"text","text":"You are Old Harness. more"}]}]}`)
	out := OverrideHarnessIdentity(body, "You are CodeBuddy Code.")
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	msgs, _ := obj["messages"].([]any)
	m, _ := msgs[0].(map[string]any)
	parts, _ := m["content"].([]any)
	p0, _ := parts[0].(map[string]any)
	if p0["text"] != "You are CodeBuddy Code. more" {
		t.Fatalf("多模态首块没替换: %v", p0["text"])
	}
}

// 无命中 / 空替换值 / 非 JSON：原样返回（不重新序列化）。
func TestOverrideHarnessIdentityNoop(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"You are helpful."}]}`)
	if out := OverrideHarnessIdentity(body, "You are helpful."); string(out) != string(body) {
		t.Fatal("已是目标值时不该改写")
	}
	plain := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	if out := OverrideHarnessIdentity(plain, "You are CodeBuddy Code."); string(out) != string(plain) {
		t.Fatal("没有 system 时不该改写")
	}
	bad := []byte(`not json`)
	if out := OverrideHarnessIdentity(bad, "x"); string(out) != string(bad) {
		t.Fatal("非 JSON 应原样返回")
	}
	// 空替换值回落默认
	withEmpty := []byte(`{"messages":[{"role":"system","content":"You are Old. x"}]}`)
	if got := firstSystem(t, OverrideHarnessIdentity(withEmpty, "  ")); !strings.HasPrefix(got, DefaultHarnessIdentity) {
		t.Fatalf("空值应回落默认: %q", got)
	}
}
