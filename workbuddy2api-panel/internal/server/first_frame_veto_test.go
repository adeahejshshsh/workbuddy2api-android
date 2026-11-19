package server

// W2-08 回归：流式请求在「一个字节都没写」时遇到请求级错误帧，必须回真实 HTTP
// 状态码，而不是 200 + SSE error。
//
// 旧行为让 Claude Code / Codex 的退避重试全部失效：429/400/503 都是 200，
// 客户端按状态码判 retryable 的分支永远走不到。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func firstFrameVetoHandler(t *testing.T, frame string, maxRotate int) (*Handler, *atomic.Int32) {
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
		MaxRotate: maxRotate,
	})
	return h, &calls
}

func TestFirstFrameVetoRequestErrorReturns400(t *testing.T) {
	h, calls := firstFrameVetoHandler(t,
		`data: {"error":{"message":"工具定义不合规","code":11129}}`, 3)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":{}}}]}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("首帧请求级错误应回 400，实际 code=%d body=%s", rec.Code, rec.Body)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("请求级错误不该轮转：上游调用 %d 次", got)
	}
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Hint    string `json:"gateway_hint"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是 JSON error（首帧否决后仍在写 SSE？）: %v body=%s", err, rec.Body)
	}
	if resp.Error.Code != "bad_params" {
		t.Fatalf("error.code=%q want bad_params", resp.Error.Code)
	}
	if !strings.Contains(resp.Error.Message, "工具定义不合规") {
		t.Fatalf("应透传上游原文：%q", resp.Error.Message)
	}
	if !strings.Contains(resp.Error.Hint, "tool definition") {
		t.Fatalf("应带点名 tools 的 gateway_hint：%q", resp.Error.Hint)
	}
}

func TestFirstFrameVetoRateLimitReturns429(t *testing.T) {
	h, _ := firstFrameVetoHandler(t,
		`data: {"error":{"message":"使用量已超出频率限制，将在 2026-11-19 00:00:00 重置","code":6004}}`, 1)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("限流首帧应回 429，实际 code=%d body=%s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "data:") {
		t.Fatalf("真实 HTTP 错误体里不该混入 SSE 帧：%s", rec.Body)
	}
}

// 对照：正文之后才到的 error 帧仍必须是 200 + 流内 error 帧（状态码已提交）。
func TestMidStreamErrorFrameStays200SSE(t *testing.T) {
	h, _ := firstFrameVetoHandler(t,
		`data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","content":"半句"}}]}`+"\n\n"+
			`data: {"error":{"message":"upstream exploded","type":"upstream_error","code":"x"}}`, 1)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("已开流后状态码不可改，实际 code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "upstream exploded") {
		t.Fatalf("中途 error 帧应透传：%s", rec.Body)
	}
}
