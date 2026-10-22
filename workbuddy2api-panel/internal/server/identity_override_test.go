package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// captureUpstream 记录每次发给上游的 body，返回一个正常回复。
func captureUpstream(t *testing.T, seen *[]string, mu *sync.Mutex) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			mu.Lock()
			*seen = append(*seen, string(raw))
			mu.Unlock()
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader(
					"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n" +
						"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n" +
						"data: [DONE]\n")),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

// 开关打开 + passthrough：发给上游的 system 身份首句必须已被替换。
func TestChatIdentityOverrideWires(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	up := captureUpstream(t, &seen, &mu)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{
		Pool: p, Upstream: up,
		PromptMode:                    "passthrough",
		PromptIdentityOverrideEnabled: true,
		PromptIdentityOverrideText:    "You are CodeBuddy Code.",
	})
	body := `{"model":"deepseek-v4.1-flash","stream":true,"messages":[
		{"role":"system","content":"You are DeepSeekRuntime, an AI assistant. Keep."},
		{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("没打到上游")
	}
	if !strings.Contains(seen[0], "You are CodeBuddy Code.") {
		t.Fatalf("身份首句没替换: %s", seen[0])
	}
	if strings.Contains(seen[0], "DeepSeekRuntime") {
		t.Fatalf("旧身份仍在上游请求里: %s", seen[0])
	}
	if !strings.Contains(seen[0], "Keep.") {
		t.Fatal("首句之后的内容被动了")
	}
}

// 开关关闭（缺省）：透传原样，绝不改客户端身份句。
func TestChatIdentityOverrideOffByDefault(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	up := captureUpstream(t, &seen, &mu)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})
	body := `{"model":"deepseek-v4.1-flash","stream":true,"messages":[
		{"role":"system","content":"You are DeepSeekRuntime. Keep."},
		{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 || !strings.Contains(seen[0], "DeepSeekRuntime") {
		t.Fatalf("开关关闭时不该替换: %v", seen)
	}
}
