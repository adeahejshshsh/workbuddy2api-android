package server

// 入站请求体上限（bodylimit.go）：五条读体入口都必须在上限内读完，超了回 413。
//
// 这组用例钉的是「上限在**读体之前**」这件事本身：四条转协议入口（messages /
// count_tokens / responses / images）此前各自 `io.ReadAll` 完才进 chatCompletions，
// 那层 MaxBytesReader 到那时已经晚了 —— 内存早就被吃掉了。只断言「400/413 有响应」
// 是测不出来的，必须让 body 真的超过上限并确认状态码是 413。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// oversizeJSON 造一个超过 limitMB 的合法 JSON 字符串值。
func oversizeJSON(limitMB int) string {
	pad := strings.Repeat("x", (limitMB<<20)+4096)
	return `{"model":"glm-5.2","messages":[{"role":"user","content":"` + pad + `"}]}`
}

func TestBodyLimitRejectsOversizeOnAllEntrypoints(t *testing.T) {
	const mb = 1
	h := NewHandler(Config{
		Pool:      testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		MaxBodyMB: mb,
	})
	body := oversizeJSON(mb)
	cases := []struct {
		path string
		body string
	}{
		{"/v1/chat/completions", body},
		{"/v1/messages", body},
		{"/messages/count_tokens", body},
		{"/v1/responses", body},
		{"/v1/images/generations", body},
		{"/v1/images/edits", body},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", c.path, strings.NewReader(c.body)))
			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("code=%d want 413（上限未在读体前生效）body=%.120s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "max_body_mb") {
				t.Fatalf("413 文案应指向配置项：%s", rec.Body.String())
			}
		})
	}
}

// 上限之内的请求不该被误伤（否则「修 OOM」变成「所有请求都 413」）。
func TestBodyLimitAllowsNormalRequests(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{
		Pool:      testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:  up,
		MaxBodyMB: 1,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"glm-5.2","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("正常请求被误判超限：%s", rec.Body.String())
	}
}

// max_body_mb<=0 时不再真的「不限制」：回落到 32MB 硬上限（只挡内存炸弹）。
func TestLimitBodyFallbackWhenUnlimited(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(), MaxBodyMB: 0})
	rec := httptest.NewRecorder()
	got := h.limitBody(rec, httptest.NewRequest("POST", "/x", strings.NewReader("")))
	if want := int64(bodyLimitFallbackMB) << 20; got != want {
		t.Fatalf("fallback limit=%d want %d", got, want)
	}

	h2 := NewHandler(Config{Pool: testPoolWith(), MaxBodyMB: 7})
	got2 := h2.limitBody(httptest.NewRecorder(), httptest.NewRequest("POST", "/x", strings.NewReader("")))
	if want := int64(7) << 20; got2 != want {
		t.Fatalf("configured limit=%d want %d", got2, want)
	}
}
