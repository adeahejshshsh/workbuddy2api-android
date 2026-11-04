package upstream

// 非流式聚合里，上游有两种下发形态：delta 增量帧，或把整条消息放在 choices[].message。
// 后者曾被「正文已取过」的 latch 整段门控 —— 于是「先 delta.content、末帧用 message
// 带 tool_calls」这种形态会把工具调用连同 reasoning 一起丢掉，finish_reason 仍是
// stop，客户端判定「模型只说了话」，工具不执行且日志无痕。

import (
	"strings"
	"testing"
)

func TestAggregateKeepsToolCallsFromMessageAfterDeltaContent(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"我来读文件。"}}]}`,
		// 末帧把完整消息放在 message 里，带 tool_calls 与 reasoning。
		`data: {"id":"c1","model":"m","choices":[{"index":0,"message":{"role":"assistant",` +
			`"content":"我来读文件。","reasoning_content":"想想看",` +
			`"tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"p\":\"a.txt\"}"}}]},` +
			`"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	got, err := Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatal(err)
	}
	msg := got["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)

	// Aggregate 内部按 []map[string]any 存（JSON 序列化后与 []any 同形）。
	tcs, _ := msg["tool_calls"].([]map[string]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls 被丢弃：%+v（正文已由 delta 取过时仍必须合并工具调用）", msg)
	}
	fn := tcs[0]["function"].(map[string]any)
	if fn["name"] != "read_file" {
		t.Fatalf("tool_calls 内容不对：%+v", fn)
	}
	// reasoning 同样不该被 latch 吞掉。
	if rc, _ := msg["reasoning_content"].(string); rc != "想想看" {
		t.Fatalf("reasoning_content 被丢弃：%q", rc)
	}
	// 正文只取一次：不能 delta 一份、message 再拼一份。
	if c, _ := msg["content"].(string); c != "我来读文件。" {
		t.Fatalf("正文被重复拼接：%q", c)
	}
}

// 对照组：没有任何 delta 正文时，message 里的正文照常并入（latch 未生效）。
func TestAggregateMessageContentWhenNoDelta(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"c1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"整条下发"}}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	got, err := Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatal(err)
	}
	msg := got["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if c, _ := msg["content"].(string); c != "整条下发" {
		t.Fatalf("content=%q want 整条下发", c)
	}
}
