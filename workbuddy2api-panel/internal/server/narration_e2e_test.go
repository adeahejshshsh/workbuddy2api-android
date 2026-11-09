package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// narrationHandler 起一个 handler，第一次上游调用返回「预告式收尾」，之后返回续写段。
func narrationHandler(t *testing.T) (*Handler, *[]string) {
	t.Helper()
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, segNarrateContinue })
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		*reqs = append(*reqs, string(raw))
		body := segNarrateContinue
		if len(*reqs) == 1 {
			body = segNarrateLeadIn
		}
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	return NewHandler(Config{Pool: p, Upstream: up}), reqs
}

const narrateReqMessages = `"messages":[{"role":"user","content":"查 user-store 的批量赋值"},` +
	`{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{}"}}]},` +
	`{"role":"tool","tool_call_id":"c1","content":"ok"}]`

const narrateTools = `"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}]`

// TestChatNarrationRescueE2E 端到端钉住 handler → continueReader 的接线：
// 真实客户端形态（工具类请求、正文停在「……：」）必须自动续写，客户端看到一条
// 连续的流，而不是「说到一半没了」。
func TestChatNarrationRescueE2E(t *testing.T) {
	h, reqs := narrationHandler(t)
	body := `{"model":"deepseek-v4.1-flash","stream":true,` + narrateTools + `,` + narrateReqMessages + `}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))

	if len(*reqs) != 2 {
		t.Fatalf("上游调用=%d want 2（首段 + 续写段）", len(*reqs))
	}
	s := rec.Body.String()
	if !strings.Contains(s, "重新连接并测试下行：") || !strings.Contains(s, "下行 400-600 Mbps 稳定。") {
		t.Fatalf("客户端应看到拼接后的完整正文:\n%s", s)
	}
	if !strings.Contains(s, `"finish_reason":"stop"`) {
		t.Fatalf("应以正常终态收尾:\n%s", s)
	}
}

// TestResponsesNarrationRescueE2E Responses 适配器走同一条 continueReader：
// 续写段必须落在同一个 response 流里（客户端不该看到第二条 message 或失败）。
//
// input 里必须带 function_call + function_call_output：真机 agent 会话的 /v1/responses
// 请求都是这个形态，转成 chat 后才有 tool 消息（判据之一）。只发一条 user 字符串
// 不构成工具轮，本就不该救。
func TestResponsesNarrationRescueE2E(t *testing.T) {
	h, reqs := narrationHandler(t)
	body := `{"model":"deepseek-v4.1-flash","stream":true,` + narrateTools + `,"input":[` +
		`{"role":"user","content":"查 user-store 的批量赋值"},` +
		`{"type":"function_call","call_id":"c1","name":"read_file","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"c1","output":"ok"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))

	if len(*reqs) != 2 {
		t.Fatalf("上游调用=%d want 2（首段 + 续写段）", len(*reqs))
	}
	s := rec.Body.String()
	if !strings.Contains(s, "下行 400-600 Mbps 稳定。") {
		t.Fatalf("续写段正文应接到客户端流里:\n%s", s)
	}
	if !strings.Contains(s, "response.completed") {
		t.Fatalf("应以 response.completed 收尾:\n%s", s)
	}
	if strings.Contains(s, "response.failed") {
		t.Fatalf("不该判失败:\n%s", s)
	}
}
