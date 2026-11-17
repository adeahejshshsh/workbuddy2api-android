package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestResponsesReordersEarlyToolResult 端到端钉桩「工具结果早于调用」的形态：
// Responses 的 input 里 function_call_output 排在对应 function_call 之前
// （Codex 经 cc-switch 类本地代理转发时会出现）。
//
// responses 适配器的 convertInputItems 按原始顺序拼接，所以修复前出站是
// tool(c0) 在 assistant(tool_calls c0) 之前，上游判 11148
// tool_call_sequence_broken 顶死整条会话。修复点在出站管线
// （payload.go 的 reorderEarlyToolResults），因此这里断言的是真正发到上游的形状，
// 而不是 handler 自己的中间产物。
//
// 变异验证：删掉 payload.go 里那行 reorderEarlyToolResults，本用例必须转红
// （出站顺序回退到 tool 在前）。
func TestResponsesReordersEarlyToolResult(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) {
		return 200, `data: {"id":"r1","choices":[{"index":0,"delta":{"content":"ok"}}]}` + "\n\n" +
			`data: {"id":"r1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
			"data: [DONE]\n\n"
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	body := `{"model":"deepseek-v4.1-flash","stream":false,"input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"run"}]},` +
		`{"type":"function_call_output","call_id":"c0","output":"ok"},` +
		`{"type":"function_call","name":"shell","arguments":"{}","call_id":"c0"},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}]}`

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))

	if len(*reqs) == 0 {
		t.Fatalf("没有发出上游请求，响应: %d %s", rec.Code, rec.Body.String())
	}
	var wire struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal([]byte((*reqs)[0]), &wire); err != nil {
		t.Fatalf("出站 body 解析失败: %v", err)
	}
	// 找出 assistant(tool_calls c0) 与 tool(c0) 的相对位置。
	callAt, toolAt := -1, -1
	for i, m := range wire.Messages {
		if raw, ok := m["tool_calls"].([]any); ok && len(raw) > 0 {
			if tc, ok := raw[0].(map[string]any); ok {
				if id, _ := tc["id"].(string); id == "c0" {
					callAt = i
				}
			}
		}
		if id, _ := m["tool_call_id"].(string); id == "c0" {
			toolAt = i
		}
	}
	if callAt < 0 {
		t.Fatalf("出站缺少 assistant(tool_calls c0)，原始 body: %s", (*reqs)[0])
	}
	if toolAt < 0 {
		t.Fatalf("出站缺少 tool(c0) 结果（被误删？），原始 body: %s", (*reqs)[0])
	}
	if toolAt < callAt {
		t.Fatalf("工具结果排在调用之前（上游会判 11148）：call@%d tool@%d\n%s",
			callAt, toolAt, (*reqs)[0])
	}
	// 重排不得改动内容与后续消息。
	if !strings.Contains((*reqs)[0], `"ok"`) {
		t.Fatalf("工具结果正文被改写：%s", (*reqs)[0])
	}
}
