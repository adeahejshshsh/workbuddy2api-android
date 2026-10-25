package server

// refresh token 失效 ≠ 账号不可用（上游 2026-10-26 `f6bd053`，issue #83）。
//
// 生产实测：29 个账号里 4 个 refresh 恒回 12153，AT 却还剩 26~333 天，
// /v2/chat/completions 全 200。旧行为在 chat 预刷新这一层就 fail + 换号，
// 三次之后把健康号禁用 —— 面板一片橙色，对话却秒回。

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 预刷新失败（refresh token 死）但 access token 仍有效时，chat 必须照常用现有
// AT 打上游，不能禁用账号、也不能换号。
func TestChatRefreshDeadButAccessTokenAliveStillServes(t *testing.T) {
	var chatHits int
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "/v2/plugin/auth/token/refresh") {
				return &http.Response{
					StatusCode: 401,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader(
						`{"code":12153,"msg":"12153:refresh token failed:400 Bad Request: invalid_grant: Session doesn't have required client"}`)),
				}, nil
			}
			chatHits++
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}

	// 距过期 5 分钟：落在 RefreshSkew(10m) 内 → 触发预刷新；但仍 > now，AT 还能用。
	a := &auth.Auth{UID: "u1", AccessToken: "at-u1", RefreshToken: "rt-1",
		ExpiresAt: time.Now().Add(5 * time.Minute).Unix()}
	h := NewHandler(Config{Pool: testPoolWith(a), Upstream: up, MaxRotate: 3})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != 200 {
		t.Fatalf("status=%d want 200（AT 有效就该正常服务）: %s", rec.Code, rec.Body.String())
	}
	if chatHits != 1 {
		t.Fatalf("chat 上游调用=%d want 1（不该因 refresh 失败换号重试）", chatHits)
	}
	st, _ := h.cfg.Pool.Status("u1")
	if st.Disabled {
		t.Fatalf("AT 仍有效不该禁用: %+v", st)
	}
	if st.SessionDeadFails != 0 {
		t.Errorf("session dead 计数=%d want 0", st.SessionDeadFails)
	}
}

// 反向保护：AT **已过期**时 refresh 判死仍要按老语义罚号换号 —— 这时账号真的不能用了。
func TestChatRefreshDeadAndAccessTokenExpiredStillFails(t *testing.T) {
	var chatHits int
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "/v2/plugin/auth/token/refresh") {
				return &http.Response{
					StatusCode: 401,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader(
						`{"code":12153,"msg":"12153:refresh token failed: invalid_grant"}`)),
				}, nil
			}
			chatHits++
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	a := &auth.Auth{UID: "u1", AccessToken: "at-u1", RefreshToken: "rt-1", ExpiresAt: 1}
	h := NewHandler(Config{Pool: testPoolWith(a), Upstream: up, MaxRotate: 3})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code == 200 {
		t.Fatalf("AT 已过期 + refresh 死 → 不该成功: %s", rec.Body.String())
	}
	if chatHits != 0 {
		t.Fatalf("AT 已过期时不该拿它去发请求，chat 调用=%d", chatHits)
	}
	st, _ := h.cfg.Pool.Status("u1")
	if st.SessionDeadFails == 0 && !st.Disabled {
		t.Errorf("AT 已过期时 refresh 判死应计 session dead: %+v", st)
	}
}
