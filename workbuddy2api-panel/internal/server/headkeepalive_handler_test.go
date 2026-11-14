package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// slowFirstRead 第一次 Read 前先睡 delay（模拟大上下文预填充：上游 200 头已回、首帧迟到）。
type slowFirstRead struct {
	r     io.Reader
	delay time.Duration
	slept bool
}

func (s *slowFirstRead) Read(p []byte) (int, error) {
	if !s.slept {
		s.slept = true
		time.Sleep(s.delay)
	}
	return s.r.Read(p)
}

// slowUpstream 上游先等 headDelay 才回响应头，再等 bodyDelay 才给第一帧。
func slowUpstream(headDelay, bodyDelay time.Duration, status int, body string) *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			time.Sleep(headDelay)
			ct := "text/event-stream"
			if status != 200 {
				ct = "application/json"
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(&slowFirstRead{r: strings.NewReader(body), delay: bodyDelay}),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

const headKeepaliveStreamBody = `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`

// TestHeadKeepaliveKeepsUpstreamErrorStatus 上游 HTTP 层回错（这里是 11148 → 400 fail-fast）
// 之前不得有任何心跳：错误必须照原状态码回给客户端。
//
// 此前心跳在轮转循环之前就起，上游回头之前已经写出注释帧把 200 提交了——之后所有
// 429/503/400 都只能变成「200 + error 帧」，Retry-After 这类头也再写不进去。
// 用 -race 跑时这条还会报心跳协程与 handler 并发写响应头 map（运行时会直接 fatal）。
func TestHeadKeepaliveKeepsUpstreamErrorStatus(t *testing.T) {
	up := slowUpstream(60*time.Millisecond, 0, 400,
		`{"code":11148,"msg":"tool calls and tool results do not match","requestId":"r"}`)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, HeadKeepalive: 5 * time.Millisecond})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(headKeepaliveStreamBody)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400（心跳不得抢先提交 200）body=%s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), ": keepalive") {
		t.Fatalf("上游回头之前就写了心跳:\n%s", rec.Body)
	}
}

// TestHeadKeepaliveAfterUpstream200 上游已回 200、首帧迟到：等待期间照常补注释帧，
// 提交出去的响应头是对的 SSE 头；首帧到达后不再心跳。
func TestHeadKeepaliveAfterUpstream200(t *testing.T) {
	sse := `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}` + "\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	// 回头也慢（> 心跳间隔）：此前心跳在回头之前就提交了响应头，X-Wb-Account 写不进去。
	up := slowUpstream(40*time.Millisecond, 80*time.Millisecond, 200, sse)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, HeadKeepalive: 10 * time.Millisecond})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(headKeepaliveStreamBody)))
	res := rec.Result()
	if res.StatusCode != 200 {
		t.Fatalf("code=%d body=%s", res.StatusCode, rec.Body)
	}
	// X-Wb-Account 那套「响应头回显本次选中账号」本仓还没有（来自更早的上游提交），
	// 所以这里只钉「SSE 头正确 + 心跳没有抢先提交 200」这两条本仓能保证的。
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type=%q", ct)
	}
	body := rec.Body.String()
	first := strings.Index(body, "data: ")
	if first < 0 || !strings.Contains(body[:first], ": keepalive\n\n") {
		t.Fatalf("首帧迟到期间没有心跳:\n%s", body)
	}
	if strings.Contains(body[first:], ": keepalive") {
		t.Fatalf("首帧之后还在心跳:\n%s", body)
	}
	requireOneTerminal(t, body)
}
