package upstream

// 客户端先走了（DSH 超时 / 点停止 / 关页面）时，出站请求的父 ctx 被取消 ——
// `Post "…": context canceled` 是**结果**，不是上游故障。
//
// 线上（2026-10-24）：`ERR: [upstream] chat_stream acct=…: transport error: Post
// "…/v2/chat/completions": context canceled`。这条被当成「上游坏了」报出来，排障
// 方向完全反了：真正要看的是客户端为什么先放弃（它的耐心比上游 TTFB 短）。

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestChatStreamContextClientCancelIsNotTransportError 父 ctx 已取消时：
// 不真的发请求、返回 context.Canceled（handler 据此记 Interrupted 而非罚号）。
func TestChatStreamContextClientCancelIsNotTransportError(t *testing.T) {
	sent := false
	c := &Client{
		ChatBaseCN: "https://fake-upstream.test",
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			if err := r.Context().Err(); err != nil {
				return nil, err
			}
			sent = true
			return nil, errors.New("boom")
		})},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 客户端在这一刻就走了

	a := &auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999}
	_, _, _, err := c.ChatStreamContext(ctx, a, []byte(`{"model":"m","messages":[]}`), "", ChatMeta{})
	if err == nil {
		t.Fatal("want error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if sent {
		t.Fatal("ctx 已取消时不该真的发出上游请求")
	}
}

// 对照组：ctx 正常、上游真出错时仍然是普通传输错误（别把两条路径合并掉）。
func TestChatStreamContextRealTransportErrorStillErrors(t *testing.T) {
	c := &Client{
		ChatBaseCN: "https://fake-upstream.test",
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("read tcp: connection reset by peer")
		})},
	}
	a := &auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999}
	_, _, _, err := c.ChatStreamContext(context.Background(), a, []byte(`{"model":"m","messages":[]}`), "", ChatMeta{})
	if err == nil {
		t.Fatal("want error")
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("真实传输错误不该被当成客户端取消: %v", err)
	}
}
