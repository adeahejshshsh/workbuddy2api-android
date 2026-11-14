package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

// 归档事件必须带上实际服务本次请求的账号所在域：面板的模型可用性靠它区分 cn 与 global
// 的同名裸名模型，缺了它 cn 的成功流量会被算成 global 同名模型的「可用」证据。
func TestRequestEventCarriesAccountRealm(t *testing.T) {
	reqLog := reqlog.New(reqlog.Config{})
	h := NewHandler(Config{
		Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) {
			return 200, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n", true
		}),
		RequestLog: reqLog,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	recent := reqLog.Snapshot().Recent
	if len(recent) != 1 {
		t.Fatalf("recent = %+v", recent)
	}
	if recent[0].Realm != "cn" {
		t.Fatalf("event realm = %q, want cn", recent[0].Realm)
	}
}
