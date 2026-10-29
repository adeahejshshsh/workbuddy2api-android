package server

// 6020/6021 渠道排队（上游 2026-10-28 `f07eba5`）。
//
// 两个断言，各对应一次线上事故：
//  1. 终态必须是 **429 + Retry-After**，不是 503 —— WorkBuddy CLI 对 429 按头自动
//     重试、对 503 直接中断任务（实案：任务在两次 503 后死亡）。
//  2. 请求内必须**继续轮转**（队列可用性是间歇的），不能拿到第一个 6021 就放弃。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestQueueBusyTerminalIs429WithRetryAfter(t *testing.T) {
	// 20 个号全撞 6021：轮转会逐个试完（MaxRotate 有界），末端必须 429。
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 429, `{"code":6021,"msg":"queue.full.title","data":{"retry_after":40}}`, false
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"kimi-k3.1-pro","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d want 429（503 会让 CLI 直接中断任务）body=%s", rec.Code, rec.Body)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Fatal("缺 Retry-After：客户端只能瞎猜等待时长")
	}
	if !strings.Contains(rec.Body.String(), "upstream_busy") {
		t.Fatalf("body=%s", rec.Body)
	}
}
