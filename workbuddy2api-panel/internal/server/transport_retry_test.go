package server

// 传输层失败（connection reset by peer / EOF，一个上游 HTTP 响应都没拿到）跟**账号**
// 无关：同一个 realm 的所有号打的是同一个上游 host（cn 全走 copilot.tencent.com），
// 换号只是换 token。
//
// 线上（2026-10-24）：
//   ERR: [upstream] chat_stream acct=…: transport error: Post "…/v2/chat/completions":
//        read tcp …: read: connection reset by peer
//   对话 … | stream | 502 | TTFB=- | total=10.0s | out=http_error | try=1
//
// `try=1` 说明一次都没重试：失败号被留在 tried 里，单账号池（CN 单号部署很常见）
// 第二圈直接选不到号 → 502 upstream_unreachable，把一次瞬时重置放大成
// 「网关连不上上游」。

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// resetThenOKUpstream 前 fail 次回传输层错误（模拟连接被重置），之后正常。
type resetThenOKUpstream struct {
	mu    sync.Mutex
	calls int
	fail  int
}

func (u *resetThenOKUpstream) client() *upstream.Client {
	return &upstream.Client{
		ChatBaseCN: "https://fake-upstream.test",
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			u.mu.Lock()
			u.calls++
			n := u.calls
			limit := u.fail
			u.mu.Unlock()
			if n <= limit {
				return nil, errors.New(
					"read tcp 172.19.0.1:35940->117.185.125.188:443: read: connection reset by peer")
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(okChatSSE)),
			}, nil
		})},
	}
}

func (u *resetThenOKUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

// TestTransportErrorRetriesSameAccount ⭐ 核心：单账号池遇到一次连接重置时，
// 必须重试**同一个号**并成功，而不是直接 502。
func TestTransportErrorRetriesSameAccount(t *testing.T) {
	up := &resetThenOKUpstream{fail: 1}
	p := testPoolWith(&auth.Auth{UID: "only", AccessToken: "at-only", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up.client(), MaxRotate: 3})

	rec := doStreamChat(h, "deepseek-v4.1-flash")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200（应重试同一个号并成功）body=%s", rec.Code, rec.Body.String())
	}
	if n := up.count(); n != 2 {
		t.Fatalf("上游应被调用 2 次（1 重置 + 1 重试），实际 %d", n)
	}
	if !strings.Contains(rec.Body.String(), `"content"`) {
		t.Fatalf("客户端没拿到正文: %s", rec.Body.String())
	}
}

// 重试仍全失败时：状态仍是 502 upstream_unreachable，但**连败只计一次** ——
// 一次网络抖动不该把一个号推过降权阈值（默认 5）。
func TestTransportErrorCountsOncePerAccount(t *testing.T) {
	up := &resetThenOKUpstream{fail: 99}
	p := testPoolWith(&auth.Auth{UID: "only", AccessToken: "at-only", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up.client(), MaxRotate: 3})

	rec := doStreamChat(h, "deepseek-v4.1-flash")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d want 502 body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "upstream_unreachable") {
		t.Fatalf("code 应为 upstream_unreachable: %s", rec.Body.String())
	}
	if n := up.count(); n != 3 {
		t.Fatalf("MaxRotate=3 应打满 3 次，实际 %d", n)
	}
	st, ok := p.Status("only")
	if !ok {
		t.Fatal("账号应仍在池中")
	}
	if st.ConsecutiveFails != 1 {
		t.Fatalf("一次请求只该记一次连败，实际 %d（会把单号池推过降权阈值）", st.ConsecutiveFails)
	}
	if st.Disabled {
		t.Fatal("传输层失败不该禁用账号")
	}
}

// ctxAwareUpstream 只在 ctx 还活着时才应答；被取消就直接返回 ctx 错误，
// 并记下「真的发出去过几次」。
type ctxAwareUpstream struct {
	mu sync.Mutex
	n  int
}

func (u *ctxAwareUpstream) client() *upstream.Client {
	return &upstream.Client{
		ChatBaseCN: "https://fake-upstream.test",
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if err := r.Context().Err(); err != nil {
				return nil, err
			}
			u.mu.Lock()
			u.n++
			u.mu.Unlock()
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(okChatSSE)),
			}, nil
		})},
	}
}

func (u *ctxAwareUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.n
}

// 客户端先走（ctx 取消）时：不真打上游、不罚号、不再往死连接写响应。
// `context canceled` 是**客户端行为**的结果，不是上游故障 —— 记成失败会把健康号
// 打成连败（用户频繁点停止时整池号轮着进降权期）。
func TestClientCancelDoesNotPenalizeAccount(t *testing.T) {
	up := &ctxAwareUpstream{}
	p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at-a1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up.client(), MaxRotate: 3})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	body := `{"model":"deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if n := up.count(); n != 0 {
		t.Fatalf("ctx 已取消时不该真的打上游，实际 %d 次", n)
	}
	st, ok := p.Status("a1")
	if !ok {
		t.Fatal("账号应仍在池中")
	}
	if st.ConsecutiveFails != 0 {
		t.Fatalf("客户端取消不该记连败，实际 %d", st.ConsecutiveFails)
	}
	if st.Disabled {
		t.Fatal("客户端取消不该禁用账号")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("客户端已断连，不该再写响应: %q", rec.Body.String())
	}
}
