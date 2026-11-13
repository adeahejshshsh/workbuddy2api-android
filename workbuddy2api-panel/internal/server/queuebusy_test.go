// queuebusy_test.go 钉住 6020/6021 渠道排队的处置：上游明说「N 秒后再来」时
// 网关先在请求内等一轮再试，而不是把排队当成不可重试错误秒杀客户端；
// 等到还是排队才交回 429/可重试错误帧。
package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 6020 排队信封（字段与线上一致：data.retry_after 是秒）。
const queueBusyBody = `{"code":6020,"msg":"queue.waiting.title","requestId":"rid-q","data":{"queue_position":3133,"queue_size":"999+","retry_after":40,"estimated_wait":19215,"client_user_type":"free"}}`

// 第一次排队、第二次成功：客户端应拿到正常流，而不是中途 error 帧。
func TestChatQueueBusyWaitsThenRescues(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		if calls == 1 {
			return 429, queueBusyBody, false
		}
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:        testPoolWith(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "a", ExpiresAt: 9999999999}),
		Upstream:    up,
		MaxRotate:   1,
		BusyRetry:   true,
		BusyWaitMax: 50 * time.Millisecond, // 上游口径 40s，测试里压到 50ms
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "你好") || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("排队恢复后应是正常流，body=%q", body)
	}
	if strings.Contains(body, "rate_limit_error") || strings.Contains(body, "upstream_busy") {
		t.Fatalf("恢复的流里不该出现错误帧，body=%q", body)
	}
	if calls != 2 {
		t.Fatalf("upstream calls=%d want 2 (一次排队 + 一次重试)", calls)
	}
}

// 关闭 busy_retry（或等待上限 <=0）时保持旧行为：立刻 429 + Retry-After，
// 由客户端自己退避。
func TestChatQueueBusyWithoutWaitReturns429(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 429, queueBusyBody, false
	})
	h := NewHandler(Config{
		Pool:      testPoolWith(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "a", ExpiresAt: 9999999999}),
		Upstream:  up,
		MaxRotate: 1,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 429 || !strings.Contains(rec.Body.String(), "upstream_busy") {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Retry-After"); got != "40" {
		t.Fatalf("Retry-After=%q want 40", got)
	}
}

// 响应头已被 keepalive 注释帧提交（线上实际路径）：error 帧必须带
// rate_limit_error + statusCode=429 + retry_after，客户端才会判可重试；
// 上游原文仍在 message 里，排队位置留在 gateway_hint。
func TestWriteUpstreamBusyErrorCommittedFrameIsRetryable(t *testing.T) {
	rec := httptest.NewRecorder()
	hk := newHeadKeepaliveWriter(context.Background(), rec, time.Hour)
	defer hk.stop()
	hk.writeComment() // 提交 200 + SSE 头
	writeUpstreamBusyError(hk, 40, queueBusyBody, "upstream_busy: position 3133", nil)
	body := rec.Body.String()
	for _, want := range []string{
		`"type":"rate_limit_error"`,
		`"code":"rate_limit_error"`,
		`"statusCode":429`,
		`"retry_after":40`,
		`queue_position`,
		`"gateway_hint":"upstream_busy: position 3133"`,
		`data: [DONE]`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("error 帧缺 %s：%s", want, body)
		}
	}
}

// 未提交响应头时保持标准 429 JSON（code 仍是 upstream_busy，便于看板区分排队）。
func TestChatQueueBusyCancellationStopsWithoutSecondSubmission(t *testing.T) {
	var calls atomic.Int32
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls.Add(1)
		return 429, queueBusyBody, false
	})
	h := NewHandler(Config{
		Pool: testPoolWith(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "a", ExpiresAt: 9999999999}), Upstream: up, MaxRotate: 1,
		BusyRetry: true, BusyWaitMax: time.Hour,
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for calls.Load() == 0 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"global:deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx))
	if got := calls.Load(); got != 1 {
		t.Fatalf("cancelled queue wait submitted %d upstream calls, want 1", got)
	}
}

func TestChatQueueBusyTerminalStillReturnsRetryableResponse(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 429, queueBusyBody, false })
	h := NewHandler(Config{
		Pool: testPoolWith(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "a", ExpiresAt: 9999999999}), Upstream: up, MaxRotate: 1,
		BusyRetry: true, BusyWaitMax: time.Millisecond,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"global:deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "40" {
		t.Fatalf("code=%d Retry-After=%q body=%s", rec.Code, rec.Header().Get("Retry-After"), rec.Body)
	}
}

func TestWriteUpstreamBusyErrorUncommittedJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	writeUpstreamBusyError(rec, 40, queueBusyBody, "hint", nil)
	if rec.Code != 429 || !strings.Contains(rec.Body.String(), `"code":"upstream_busy"`) {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "rate_limit_error") {
		t.Fatalf("未提交时不应伪装成 rate_limit_error：%s", rec.Body)
	}
}
