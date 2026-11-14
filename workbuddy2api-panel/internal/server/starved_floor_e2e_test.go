package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// segStarvedAllThinking 首段：整份额度被思考吃光，正文 0B，finish_reason=length。
// 这是线上实测的形态（effort=high + 客户端 max_tokens=1024，被抬到 8192 后仍然全烧在思考上）。
const segStarvedAllThinking = `data: {"id":"c1","choices":[{"index":0,"delta":{"reasoning_content":"想了很久"}}]}` + "\n\n" +
	`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":8192,"completion_thinking_tokens":8192,"total_tokens":8202}}` + "\n\n" +
	"data: [DONE]\n\n"

// TestChatStarvedRescueCarriesBudgetFloor 端到端钉住 handler 的接线：
// 首段被思考吃光 → 救援段的请求体必须带「本轮档位对应的预算下限」，而不是客户端
// 原来那个装不下正文的小额度。
//
// 这是线上实测出来的真实缺陷：首段被抬到 8192（全被思考吃掉，正文 0B）→ 救援段
// 从客户端原始 body 重建、又关掉思考 → prepareBody 的下限逻辑跳过 → 正文只有 1024
// 的空间 → 用户看到 1946 个字就断。
func TestChatStarvedRescueCarriesBudgetFloor(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, testSeg2Text })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	// 下限只对目录里有输出上限的模型生效（见 upstream.OutputBudgetFloor / outputFloor）。
	up.StoreModelMaxOutForTest("cn", map[string]int64{"deepseek-v4.1-flash": 131072, "m": 131072})
	// 首段由 fakeUpstream 风格的 transport 提供：这里直接让 ChatStreamContext 的第一次
	// 调用返回「纯思考」段，第二次（救援段）返回正常收尾段。
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		*reqs = append(*reqs, string(raw))
		body := testSeg2Text
		if len(*reqs) == 1 {
			body = segStarvedAllThinking
		}
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})

	body := `{"model":"deepseek-v4.1-flash","max_tokens":1024,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))

	if len(*reqs) != 2 {
		t.Fatalf("上游调用=%d want 2（首段 + 救援段）", len(*reqs))
	}
	var rescue map[string]any
	if err := json.Unmarshal([]byte((*reqs)[1]), &rescue); err != nil {
		t.Fatalf("救援段请求体非法: %v", err)
	}
	got, _ := rescue["max_tokens"].(float64)
	if int64(got) != 8192 {
		t.Fatalf("救援段 max_tokens=%v want 8192（high 档预算下限，不能沿用客户端的 1024）", rescue["max_tokens"])
	}
	// 首段同样被抬到下限（这是 prepareBody 那一层的事，一并钉住）。
	var first map[string]any
	json.Unmarshal([]byte((*reqs)[0]), &first)
	if f, _ := first["max_tokens"].(float64); int64(f) != 8192 {
		t.Fatalf("首段 max_tokens=%v want 8192", first["max_tokens"])
	}
}

// TestChatStarvedRescueStreamCarriesBudgetFloor 同上，但走**流式**路径：
// 流式的救援由 continueReader.tryStarvedRetry 发起，用的是 r.budgetFloor，
// 与非流式的 handler 分支是两处接线，必须各自钉住。
func TestChatStarvedRescueStreamCarriesBudgetFloor(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, testSeg2Text })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	// 下限只对目录里有输出上限的模型生效（见 upstream.OutputBudgetFloor / outputFloor）。
	up.StoreModelMaxOutForTest("cn", map[string]int64{"deepseek-v4.1-flash": 131072, "m": 131072})
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		*reqs = append(*reqs, string(raw))
		body := testSeg2Text
		if len(*reqs) == 1 {
			body = segStarvedAllThinking
		}
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})

	body := `{"model":"deepseek-v4.1-flash","stream":true,"max_tokens":1024,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))

	if len(*reqs) != 2 {
		t.Fatalf("上游调用=%d want 2（首段 + 流式救援段）", len(*reqs))
	}
	var rescue map[string]any
	json.Unmarshal([]byte((*reqs)[1]), &rescue)
	if got, _ := rescue["max_tokens"].(float64); int64(got) != 8192 {
		t.Fatalf("流式救援段 max_tokens=%v want 8192", rescue["max_tokens"])
	}
	// 客户端应看到救援段的正文（不再停在半句）。
	if !strings.Contains(rec.Body.String(), "继续") {
		t.Fatalf("流式救援段正文未接上:\n%s", rec.Body.String())
	}
}

// roundTripFunc 已在其它测试里声明时复用（见 handler_test.go）；这里仅在未声明时兜底。
var _ = upstream.MinOutputBudget
