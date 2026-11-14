package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// requireOneTerminal 断言客户端可见的 chat SSE 流「恰好一个终态」：
// 恰好一条 data: [DONE] 且在末尾，非空 finish_reason 帧与 error 帧合计恰好一个。
//
// 只用 strings.Contains 查「有没有 length / [DONE]」的断言抓不到重复终态——
// 「error 帧后又补一帧 length」「stop 之后再补 length」都能让 Contains 照样通过，
// 而客户端按最后一个终态判回合结果，后一个会把前一个洗掉。
func requireOneTerminal(t *testing.T, body string) {
	t.Helper()
	if n := strings.Count(body, "data: [DONE]"); n != 1 || !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("[DONE] 应恰好一条且在末尾，got %d:\n%s", n, body)
	}
	terminals := 0
	for _, line := range strings.Split(body, "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok || payload == "[DONE]" {
			continue
		}
		var obj struct {
			Error   any `json:"error"`
			Choices []struct {
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &obj) != nil {
			continue
		}
		if obj.Error != nil {
			terminals++
		}
		for _, c := range obj.Choices {
			if c.FinishReason != "" {
				terminals++
			}
		}
	}
	if terminals != 1 {
		t.Fatalf("终态（finish_reason 帧 + error 帧）应恰好一个，got %d:\n%s", terminals, body)
	}
}

// serveChatStream 用给定的上游 SSE 走一遍完整的流式 chat 管线（continueReader → StreamHint）。
func serveChatStream(t *testing.T, upstreamSSE string) string {
	t.Helper()
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, upstreamSSE, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	return rec.Body.String()
}

// TestStreamDoneWithoutFinishNotTruncated 上游正常发了 data: [DONE]、只是没给 finish_reason：
// 这是正常收尾，不是断流，不得补 finish_reason=length。
//
// 此前 continueReader 吞掉 [DONE]（由 StreamHint 统一补），StreamHint 于是永远看不到上游的
// [DONE]，把每条「没带 finish 的正常流」都当断流补一帧 length——responses 侧变 incomplete、
// messages 侧变 max_tokens，客户端据此去续写一条本已完整的回答。
func TestStreamDoneWithoutFinishNotTruncated(t *testing.T) {
	sse := `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"完整回答"}}]}` + "\n\n" +
		"data: [DONE]\n\n"
	body := serveChatStream(t, sse)
	if strings.Contains(body, `"finish_reason":"length"`) {
		t.Fatalf("正常 [DONE] 收尾被标成截断:\n%s", body)
	}
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Fatalf("[DONE] 应恰好一条，got %d:\n%s", n, body)
	}
}

// TestStreamErrorFrameNotFollowedByLength 正文之后上游给了 error 帧再断开：error 帧就是终态，
// 不得再补一帧 finish_reason=length（客户端按最后一个终态判结果，「截断」会盖掉「失败」）。
func TestStreamErrorFrameNotFollowedByLength(t *testing.T) {
	sse := `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"半句"}}]}` + "\n\n" +
		`data: {"error":{"message":"upstream exploded","type":"upstream_error","code":"x"}}` + "\n\n"
	body := serveChatStream(t, sse)
	if strings.Contains(body, `"finish_reason":"length"`) {
		t.Fatalf("error 帧之后又补了截断终态:\n%s", body)
	}
	requireOneTerminal(t, body)
}

// TestStreamTerminalsExactlyOnce 三种收尾形态都必须「恰好一个终态 + 恰好一条 [DONE]」。
func TestStreamTerminalsExactlyOnce(t *testing.T) {
	head := `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"正文"}}]}` + "\n\n"
	cases := map[string]string{
		"stop+DONE":  head + `data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" + "data: [DONE]\n\n",
		"stop 无DONE": head + `data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
		"断流":         head,
	}
	for name, sse := range cases {
		t.Run(name, func(t *testing.T) { requireOneTerminal(t, serveChatStream(t, sse)) })
	}
}
