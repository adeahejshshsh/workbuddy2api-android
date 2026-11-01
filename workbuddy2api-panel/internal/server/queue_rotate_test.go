package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestChatQueueWaitingDoesNotRotate 6020 带 data.retry_after 时**不换号**（issue #125）：
// 上游已给出「40 秒后重试」的通道级口径（队列按 client_user_type 全局共享，实测
// position 2502→2648 只随时间涨），这几毫秒内换号只会把同一份排队请求多发几次。
// 6021（队列满、无 retry_after）仍轮转，见 TestChatQueueBusyRotatesThen429WithoutPenalty。
func TestChatQueueWaitingDoesNotRotate(t *testing.T) {
	const upstreamBody = `{"code":6020,"msg":"queue.waiting.title","requestId":"01a181ca3b67934d01c3f64599c8490c","data":{"queue_position":2502,"queue_size":"999+","retry_after":40,"estimated_wait":12510}}`
	var calls int
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 429, upstreamBody, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "q1", AccessToken: "at-q1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "q2", AccessToken: "at-q2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if calls != 1 {
		t.Errorf("上游调用 %d 次，want 1（给了 retry_after 就不该再换号重打排队请求）", calls)
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d want 429 (body=%s)", rec.Code, rec.Body)
	}
	if ra := rec.Header().Get("Retry-After"); ra != "40" {
		t.Errorf("Retry-After=%q want 40（客户端按这个睡，别用本地兜底 5s）", ra)
	}
}
