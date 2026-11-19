package server

// W2-08 回归（适配层）：/v1/messages 与 /v1/responses 的流式请求在零字节时
// 遇到请求级错误，同样必须回真实 HTTP 状态码。
//
// chatCompletions 的否决只把真实状态写进 adapter 的 status 字段；如果 adapter
// 的 finish() 仍按「stream=true + 未开流 → 200 + SSE error」收尾，Claude Code /
// Codex 依旧看不到 4xx/429，退避重试继续失效。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func adapterVetoHandler(t *testing.T, frame string) (*Handler, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls.Add(1)
		return 200, frame + "\n\ndata: [DONE]\n\n", true
	})
	h := NewHandler(Config{
		Pool: testPoolWith(
			&auth.Auth{UID: "a", AccessToken: "at-a", ExpiresAt: 9999999999},
			&auth.Auth{UID: "b", AccessToken: "at-b", ExpiresAt: 9999999999},
		),
		Upstream:  up,
		MaxRotate: 1,
	})
	return h, &calls
}

func TestMessagesFirstFrameVetoReturns400(t *testing.T) {
	h, _ := adapterVetoHandler(t, `data: {"error":{"message":"工具定义不合规","code":11129}}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("Anthropic 流式首帧请求级错误应回 400，实际 code=%d body=%s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "event:") {
		t.Fatalf("真实 HTTP 错误体不该是 SSE：%s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "工具定义不合规") {
		t.Fatalf("应透传上游原文：%s", rec.Body)
	}
}

func TestResponsesFirstFrameVetoReturns429(t *testing.T) {
	h, _ := adapterVetoHandler(t, `data: {"error":{"message":"使用量已超出频率限制，将在 2026-11-19 00:00:00 重置","code":6004}}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"input":"hi"}`)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("Responses 流式首帧限流应回 429，实际 code=%d body=%s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "event:") {
		t.Fatalf("真实 HTTP 错误体不该是 SSE：%s", rec.Body)
	}
}
