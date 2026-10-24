package server

// 上游「首帧之前就断流」的处置：换号重试（对客户端透明），而不是伪 200 + error 帧。
//
// 背景（2026-10-24 线上）：DSH 报「对话失败」，日志是
//   upstream read error: upstream stream read failed: unexpected EOF
// 其中有一类断在**第一帧之前** —— 那时响应头还没发出，换一个账号重发本可以完全
// 透明地成功，旧实现却照样写 200 + error 帧，客户端只能把整个回合判死。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// errBeforeFirstFrame 首次 Read 就报 unexpected EOF：模拟上游 200 之后立刻断流
// （一个字节都没吐）。io.ErrUnexpectedEOF 与线上日志里的 "unexpected EOF" 同源。
type errBeforeFirstFrame struct{}

func (errBeforeFirstFrame) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

// truncatingUpstream 第 1 次请求回「200 + 首帧前断流」，之后正常吐 okChatSSE。
// 断言「客户端最终拿到完整流」+「上游确实被调用了不止一次」= 透明重试成立。
type truncatingUpstream struct {
	mu    sync.Mutex
	calls int
	auths []string
}

func (u *truncatingUpstream) client() *upstream.Client {
	return &upstream.Client{
		// ChatBaseCN 必须设：出站 URL 由它拼。
		ChatBaseCN: "https://fake-upstream.test",
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			u.mu.Lock()
			u.calls++
			n := u.calls
			u.auths = append(u.auths, r.Header.Get("Authorization"))
			u.mu.Unlock()

			var body io.Reader = strings.NewReader(okChatSSE)
			if n == 1 {
				body = errBeforeFirstFrame{}
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(body),
			}, nil
		})},
	}
}

func (u *truncatingUpstream) stats() (calls int, auths []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls, append([]string(nil), u.auths...)
}

func doStreamChat(h *Handler, model string) *httptest.ResponseRecorder {
	body := `{"model":"` + model + `","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	return rec
}

// TestStreamRetriesWhenUpstreamBrokeBeforeFirstFrame ⭐ 核心：首帧前断流 →
// 换号重试，客户端拿到的是**完整正常流**（没有 error 帧、以 [DONE] 收尾）。
func TestStreamRetriesWhenUpstreamBrokeBeforeFirstFrame(t *testing.T) {
	up := &truncatingUpstream{}
	p := testPoolWith(
		&auth.Auth{UID: "a1", AccessToken: "at-a1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a2", AccessToken: "at-a2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up.client(), MaxRotate: 3})

	rec := doStreamChat(h, "deepseek-v4.1-flash")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 (重试后应成功)", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type=%q want text/event-stream", ct)
	}
	body := rec.Body.String()
	if strings.Contains(body, "upstream_read_error") {
		t.Fatalf("客户端不该看到失败帧（那次尝试对它是不可见的）: %s", body)
	}
	if !strings.HasSuffix(strings.TrimRight(body, "\n"), "data: [DONE]") {
		t.Fatalf("流必须以恰好一个 [DONE] 收尾: %q", body)
	}
	if !strings.Contains(body, `"content"`) {
		t.Fatalf("缺正文帧: %s", body)
	}
	calls, auths := up.stats()
	if calls < 2 {
		t.Fatalf("上游只被调用 %d 次 —— 首帧前断流没有换号重试", calls)
	}
	if auths[0] == auths[1] {
		t.Fatalf("重试打到了同一个账号（%s）—— 断流的号该被排除", auths[0])
	}
}

// TestStreamEarlyBreakReturns502JSONWhenRetriesExhausted 轮转名额用尽时，头还没发出，
// 必须回**正经的 502 JSON**（客户端可正常重试），而不是空 200 或半截 SSE。
func TestStreamEarlyBreakReturns502JSONWhenRetriesExhausted(t *testing.T) {
	up := &truncatingUpstream{}
	p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at-a1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up.client(), MaxRotate: 1})

	rec := doStreamChat(h, "deepseek-v4.1-flash")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d want 502", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type=%q want application/json（SSE 头必须撤掉）", ct)
	}
	body := rec.Body.String()
	if strings.Contains(body, "data: ") {
		t.Fatalf("不该往客户端写 SSE 帧: %q", body)
	}
	var got struct {
		Error struct {
			Message     string `json:"message"`
			Code        string `json:"code"`
			GatewayHint string `json:"gateway_hint"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("响应不是 JSON: %v body=%s", err, body)
	}
	if got.Error.Code != "upstream_error" {
		t.Fatalf("code=%q want upstream_error (body=%s)", got.Error.Code, body)
	}
	if got.Error.GatewayHint == "" {
		t.Fatalf("缺 gateway_hint（运维/客户端需要知道这是可重试的上游断流）: %s", body)
	}
	if rec.Header().Get("X-Accel-Buffering") != "" {
		t.Fatal("SSE 专用头必须撤掉，否则反代仍按不缓冲处理一个 JSON 响应")
	}
}
