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

// TestContentBlockedStripsToolDescriptionsAndRetries 11128 的最后一层抢救。
//
// 复现线上形态（v1.35.21，DSH 客户端）：system 提示词已经降级成中性提示词，
// 上游**仍然**回 400 `Illegal API invocation from an unapproved channel` ——
// 说明触发点不在 messages，而在客户端自带的 tools 描述里。
//
// 期望的抢救阶梯：
//   1. 第 1 次 → 11128 → 触发降级（中性提示词）并重试；
//   2. 第 2 次 → 仍然 11128（降级无效）→ 摘掉 tools[].function.description 再试；
//   3. 第 3 次 → 200。
//
// 并且降级锁存必须被解除 —— 否则这个客户端一整个白天都在用中性提示词陪跑。
func TestContentBlockedStripsToolDescriptionsAndRetries(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			bodies = append(bodies, string(b))
			attempt := len(bodies)
			mu.Unlock()
			if attempt <= 2 {
				return &http.Response{
					StatusCode: 400,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader(
						`{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`)),
				}, nil
			}
			// chat 路径出站恒为 stream=true，成功响应必须是 SSE（否则会被判
			// upstream_parse: stream contained no valid data events）。
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader(
					"data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":" +
						"[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"}," +
						"\"finish_reason\":\"stop\"}]}\n\n" +
						"data: [DONE]\n\n")),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	reqBody := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"function","function":{"name":"dshmarketplace_search",` +
		`"description":"Search the DeepSeek Harness plugin catalogue",` +
		`"parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(reqBody)))

	if len(bodies) < 3 {
		t.Fatalf("应当尝试 3 次（原样 → 降级 → 摘描述），实际 %d 次；bodies=%v", len(bodies), bodies)
	}
	if !strings.Contains(bodies[2], `"name":"dshmarketplace_search"`) {
		t.Errorf("摘描述不能动 name：%s", bodies[2])
	}
	if strings.Contains(bodies[2], `"description"`) {
		t.Errorf("第三次请求应当已经没有 tools[].function.description：%s", bodies[2])
	}
	if !strings.Contains(bodies[2], `"parameters"`) {
		t.Errorf("摘描述不能动 parameters（schema 要保留，否则模型无法调用）：%s", bodies[2])
	}
	if rec.Code != 200 {
		t.Errorf("第三次成功后应当回 200，got %d body=%s", rec.Code, rec.Body.String())
	}
	if degradeActiveAny(&h.degrade) {
		t.Error("降级重试失败后必须解除锁存（Clear），否则该客户端一整天都用中性提示词")
	}
}

// TestContentBlockedWithoutToolsStillFails 没有可摘的工具描述时不该无限重试，
// 也不能把错误吞掉 —— 必须仍然回 400 + content_blocked。
func TestContentBlockedWithoutToolsStillFails(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != 400 {
		t.Fatalf("没有工具可摘时应当保持 400，got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "content_blocked") {
		t.Errorf("错误码应为 content_blocked：%s", rec.Body.String())
	}
}
