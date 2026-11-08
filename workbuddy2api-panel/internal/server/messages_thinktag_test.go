package server

// 上游 reasoning parser 偶发把整段 CoT 当正文下发（上游 issue #194）：正文首尾带
// <think:SUF>…</think:SUF>，usage 的 reasoning_tokens 是 0。Anthropic 客户端只认
// content_block.type=thinking，内联标签会被当正文显示出来 —— 网关必须拆回 thinking 块。

import "testing"

func TestChatCompletionToMessageSplitsInlineThinkTags(t *testing.T) {
	got := chatCompletionToMessage(map[string]any{
		"id":    "chatcmpl-9",
		"model": "glm-5.2",
		"choices": []any{map[string]any{
			"message": map[string]any{
				"role":    "assistant",
				"content": "<think:6124c78e>先想一下</think:6124c78e>正文在这里",
			},
			"finish_reason": "stop",
		}},
	})
	content, _ := got["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("应拆成 thinking + text 两块，实际 %d：%v", len(content), content)
	}
	th, _ := content[0].(map[string]any)
	if th["type"] != "thinking" || th["thinking"] != "先想一下" {
		t.Fatalf("thinking 块不对：%v", th)
	}
	tx, _ := content[1].(map[string]any)
	if tx["type"] != "text" || tx["text"] != "正文在这里" {
		t.Fatalf("text 块里还留着控制 token：%v", tx)
	}
}

// 正文中间聊到 "<think:" 不能重写（只在开头才启用）。
func TestChatCompletionToMessageKeepsMidTextThinkMention(t *testing.T) {
	got := chatCompletionToMessage(map[string]any{
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": "解释一下 <think:abc> 这个标记"},
			"finish_reason": "stop",
		}},
	})
	content, _ := got["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("不该拆块：%v", content)
	}
	tx, _ := content[0].(map[string]any)
	if tx["type"] != "text" || tx["text"] != "解释一下 <think:abc> 这个标记" {
		t.Fatalf("正文被改写了：%v", tx)
	}
}
