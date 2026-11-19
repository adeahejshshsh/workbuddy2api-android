// 选不到号的 503 必须自带成因拆解（issue #276）。
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestNoHealthyAccountHintCarriesNoPick 池里两个号都在、都有余额，但余额 <= 保留积分阈值
// → 选号必失败。回给客户端的 gateway_hint 必须说清是 reserve 挡的：此前只有一个
// 「no healthy account available in pool; check /status or retry later」，而 /status 上
// 这两个号看起来完全正常（issue #276 的原话就是「还有 20 个账号没有限额」）。
func TestNoHealthyAccountHintCarriesNoPick(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, `{}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at-2", ExpiresAt: 9999999999},
	)
	p.SetCredits("u1", 5, 100) // 余额已知且 <= 阈值 → 停牌
	p.SetCredits("u2", 3, 100)
	p.SetReserveCredits(10)
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s (want 503)", rec.Code, rec.Body)
	}
	if calls != 0 {
		t.Fatalf("选号失败不该打上游，calls=%d", calls)
	}
	body := rec.Body.String()
	for _, want := range []string{"no_healthy_account", "no_pick{", "reserve=2", "total=2"} {
		if !strings.Contains(body, want) {
			t.Errorf("503 文案缺少 %q: %s", want, body)
		}
	}
}
