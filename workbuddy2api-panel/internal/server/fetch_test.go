package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestFetchEndpoint POST /v1/fetch：DSH 的 DeepSeek Fetch API 形状——
// 请求 {url}，回 {content}（上游原文的 title/url 一并保留）。
func TestFetchEndpoint(t *testing.T) {
	const upstreamBody = `{"url":"https://example.com/a","title":"示例","content":"# 标题\n\n正文"}`
	f := &searchFake{status: 200, body: upstreamBody}
	p := testPoolWith(&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	h := NewHandler(Config{Pool: p, Upstream: f.client()})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/fetch",
		strings.NewReader(`{"url":"https://example.com/a"}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, rec.Body.String())
	}
	if got["content"] != "# 标题\n\n正文" {
		t.Fatalf("content 透传不符: %v", got["content"])
	}
	if got["title"] != "示例" || got["url"] != "https://example.com/a" {
		t.Fatalf("其余字段应原样保留: %v", got)
	}
	if f.paths[0] != "/agenttool/v1/webfetch" {
		t.Fatalf("落点应为 webfetch: %v", f.paths)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(f.bodies[0]), &sent)
	if sent["url"] != "https://example.com/a" {
		t.Fatalf("出站 url 不符: %v", sent)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type=%q", ct)
	}
}

// TestFetchValidation 本地先拦：缺 url / 非 http(s) 一律 400 且不打上游。
func TestFetchValidation(t *testing.T) {
	f := &searchFake{status: 200, body: `{"content":"x"}`}
	p := testPoolWith(&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	h := NewHandler(Config{Pool: p, Upstream: f.client()})
	for _, body := range []string{
		`{}`,
		`{"url":"  "}`,
		`{"url":"file:///etc/passwd"}`,
		`{"url":"not-a-url"}`,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/fetch", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body=%s code=%d, want 400 (%s)", body, rec.Code, rec.Body)
		}
	}
	if len(f.paths) != 0 {
		t.Fatalf("校验失败不应打上游，实际 %d 次", len(f.paths))
	}
}

// TestFetchUpstreamShape 上游 200 但没有 string content → 502，不伪造空正文。
func TestFetchUpstreamShape(t *testing.T) {
	for _, body := range []string{`{"url":"https://x"}`, `{"content":null}`, `{"content":123}`, `not json`} {
		f := &searchFake{status: 200, body: body}
		p := testPoolWith(&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
		h := NewHandler(Config{Pool: p, Upstream: f.client()})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/fetch",
			strings.NewReader(`{"url":"https://x"}`)))
		if rec.Code != http.StatusBadGateway {
			t.Errorf("upstream=%s code=%d, want 502 (%s)", body, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), `"content":""`) {
			t.Errorf("不应把坏响应伪造成空正文: %s", rec.Body)
		}
	}
}

// TestFetchRequestLevelFailFast 上游 400（url 校验）→ 原文透传、只打一次、不轮转。
func TestFetchRequestLevelFailFast(t *testing.T) {
	f := &searchFake{status: 400, body: `{"code":15013,"msg":"url is required"}`}
	p := testPoolWith(
		&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
		&auth.Auth{UID: "c2", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
	)
	h := NewHandler(Config{Pool: p, Upstream: f.client()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/fetch",
		strings.NewReader(`{"url":"https://x"}`)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "15013") {
		t.Fatalf("请求级 400 应原文透传，得到 %d %s", rec.Code, rec.Body.String())
	}
	if len(f.paths) != 1 {
		t.Fatalf("请求级错误应 fail-fast 不轮转，实际打了 %d 次", len(f.paths))
	}
}
