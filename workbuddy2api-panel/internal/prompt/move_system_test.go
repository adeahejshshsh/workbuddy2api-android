package prompt

// 11‑-128 搬家抢救（上游 2026-10-29 `49f9db4`）：把 system 原文搬进首条 user，
// system 换中性提示词 —— 只换承载通道，客户端人设/项目约定一个字符都不丢。

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMoveSystemToUserKeepsText(t *testing.T) {
	sys := "你是某项目的助手。\n约定：只用 Go。"
	body, _ := json.Marshal(map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": sys},
			map[string]any{"role": "user", "content": "帮我写个函数"},
		},
	})
	out, ok := MoveSystemToUser(body, "You are a helpful assistant.")
	if !ok {
		t.Fatal("应搬运成功")
	}
	var obj struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj.Messages[0]["content"] != "You are a helpful assistant." {
		t.Errorf("system 应换成中性提示词，got %v", obj.Messages[0]["content"])
	}
	got, _ := obj.Messages[1]["content"].(string)
	if !strings.HasPrefix(got, sys+"\n\n") || !strings.Contains(got, "帮我写个函数") {
		t.Fatalf("原文未完整搬进首条 user: %q", got)
	}
}

func TestMoveSystemToUserBails(t *testing.T) {
	cases := map[string]string{
		"无 system":    `{"messages":[{"role":"user","content":"hi"}]}`,
		"system 非纯文本": `{"messages":[{"role":"system","content":[{"type":"text","text":"x"}]},{"role":"user","content":"hi"}]}`,
		"无 user":      `{"messages":[{"role":"system","content":"x"}]}`,
		"首条 user 多模态": `{"messages":[{"role":"system","content":"x"},{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
		"system 空白":   `{"messages":[{"role":"system","content":"   "},{"role":"user","content":"hi"}]}`,
	}
	for name, raw := range cases {
		if out, ok := MoveSystemToUser([]byte(raw), "neutral"); ok || string(out) != raw {
			t.Errorf("%s：应原样返回且 ok=false", name)
		}
	}
	// developer 角色同样可搬（OpenAI 新形态）。
	dev := `{"messages":[{"role":"developer","content":"rules"},{"role":"user","content":"hi"}]}`
	if _, ok := MoveSystemToUser([]byte(dev), "neutral"); !ok {
		t.Error("developer 角色应可搬")
	}
}
