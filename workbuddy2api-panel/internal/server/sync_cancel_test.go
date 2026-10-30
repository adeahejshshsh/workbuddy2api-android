package server

// 非流式「调用方断开」不该记成 502 upstream_parse（上游 2026-10-30 `7d43b96`）。
//
// 现场：上游正文还在流、客户端先掐断（CDN 回源超时、AsyncOpenAI 类客户端 timeout、
// 用户点停止）。上游当时在正常输出、网关也没出错，此前一律记 502 upstream_parse，
// 把「别人掐断」报成上游故障 —— 上游实测一天 11 次，全部恰好 total=15.0s、tok=-、
// do=1~3s，来源全是 EdgeOne 回源节点（默认 15s 超时）。
// 改成 499（nginx 的 client closed request）+ OutcomeInterrupted，与真 5xx 一眼可分。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// ctxBlockingReader 阻塞到 ctx 取消再返回 ctx.Err()：模拟「上游正文还在流、客户端先断开」。
type ctxBlockingReader struct{ ctx context.Context }

func (r ctxBlockingReader) Read([]byte) (int, error) {
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

func TestChatSyncClientCancelIsNotUpstreamParse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(ctxBlockingReader{ctx}),
				Request:    r,
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	rec := httptest.NewRecorder()

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler 卡死：客户端断开后没有及时返回")
	}

	body := rec.Body.String()
	if strings.Contains(body, "upstream_parse") {
		t.Fatalf("客户端断开被记成上游解析失败（502 upstream_parse）: %s", body)
	}
	if rec.Code == http.StatusBadGateway {
		t.Fatalf("客户端断开不该回 502，got %d body=%s", rec.Code, body)
	}
}
