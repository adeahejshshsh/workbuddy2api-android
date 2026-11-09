package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// segReasoningOnlyStop 首段：上游只吐了思考、正文 0B、finish_reason=stop、无工具调用。
// 这是用户截图里那个 502 的形态（upstream returned reasoning without output text）。
const segReasoningOnlyStop = `data: {"id":"c9","choices":[{"index":0,"delta":{"reasoning_content":"先想一下"}}]}` + "\n\n" +
	`data: {"id":"c9","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":50,"completion_thinking_tokens":50,"total_tokens":150}}` + "\n\n" +
	"data: [DONE]\n\n"

// TestChatRescuesReasoningOnlyStop 「思考完直接收尾」必须救援一次，而不是把空回复交给上层。
//
// 线上表现：responses 适配器发现「有思考、无正文、无工具调用」→ response.failed
// （code=empty_reasoning_only），用户看到一个 502「upstream returned reasoning
// without output text; retry」。救援动作与「预算被思考吃光」相同：同额度、关思考
// 重发一次——同一份额度全给正文，通常就有内容了。
func TestChatRescuesReasoningOnlyStop(t *testing.T) {
	// 首段由下面的 reader 直接提供，所以上游的**第一次**调用就是救援段。
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, testSeg2Text })
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		*reqs = append(*reqs, string(raw))
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(testSeg2Text)),
		}, nil
	})

	r := newContinueReaderWithFloor(context.Background(), up,
		&auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999},
		[]byte(`{"model":"cn:deepseek-v4.1-flash","max_tokens":4096,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`),
		"1.2.3.4", upstream.ChatMeta{}, io.NopCloser(strings.NewReader(segReasoningOnlyStop)), 8192)
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("救援请求数=%d want 1（思考完直接收尾应关思考重发一次）", len(*reqs))
	}
	var rescue map[string]any
	if err := json.Unmarshal([]byte((*reqs)[0]), &rescue); err != nil {
		t.Fatalf("救援段请求体非法: %v", err)
	}
	if th, _ := rescue["thinking"].(map[string]any); th == nil || th["type"] != "disabled" {
		t.Fatalf("救援段应关思考，实际 thinking=%v", rescue["thinking"])
	}
	if _, has := rescue["reasoning_effort"]; has {
		t.Fatalf("关思考后不该再带档位，实际 %v", rescue["reasoning_effort"])
	}
	if got, _ := rescue["max_tokens"].(float64); int64(got) < 8192 {
		t.Fatalf("救援段额度=%v want >= 8192（继承下限）", rescue["max_tokens"])
	}
	if !strings.Contains(string(out), "继续") {
		t.Fatalf("客户端应看到救援段正文:\n%s", out)
	}
}

// TestChatNoRescueWhenTextPresent 有正文的正常收尾不能被救援逻辑碰。
func TestChatNoRescueWhenTextPresent(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, testSeg2Text })
	r := newContinueReaderWithFloor(context.Background(), up,
		&auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999},
		[]byte(`{"model":"cn:deepseek-v4.1-flash","max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`),
		"1.2.3.4", upstream.ChatMeta{}, io.NopCloser(strings.NewReader(testSeg2Text)), 8192)
	out, _ := io.ReadAll(r)
	if len(*reqs) != 0 {
		t.Fatalf("正常收尾不应重发，实际 %d 次", len(*reqs))
	}
	if !strings.Contains(string(out), "继续") {
		t.Fatalf("正文应原样下发:\n%s", out)
	}
}

// TestChatNoRescueWhenNoReasoning 空回复但没有任何思考：可能是合法的空答案，
// 也可能是上游缺陷——不猜，按原样收尾（上层自有判据）。
func TestChatNoRescueWhenNoReasoning(t *testing.T) {
	empty := `data: {"id":"c9","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" + "data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, testSeg2Text })
	r := newContinueReaderWithFloor(context.Background(), up,
		&auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999},
		[]byte(`{"model":"cn:deepseek-v4.1-flash","max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`),
		"1.2.3.4", upstream.ChatMeta{}, io.NopCloser(strings.NewReader(empty)), 8192)
	_, _ = io.ReadAll(r)
	if len(*reqs) != 0 {
		t.Fatalf("无思考的空回复不应重发，实际 %d 次", len(*reqs))
	}
}

// TestChatNoRescueWhenToolCallsSeen 工具轮（有 tool_calls 分片）不能被当成空回复重发。
func TestChatNoRescueWhenToolCallsSeen(t *testing.T) {
	withTools := `data: {"id":"c9","choices":[{"index":0,"delta":{"reasoning_content":"想","tool_calls":[{"index":0,"id":"call_1","function":{"name":"read_file","arguments":"{\"p\":1}"}}]}}]}` + "\n\n" +
		`data: {"id":"c9","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" + "data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, testSeg2Text })
	r := newContinueReaderWithFloor(context.Background(), up,
		&auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999},
		[]byte(`{"model":"cn:deepseek-v4.1-flash","max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`),
		"1.2.3.4", upstream.ChatMeta{}, io.NopCloser(strings.NewReader(withTools)), 8192)
	_, _ = io.ReadAll(r)
	if len(*reqs) != 0 {
		t.Fatalf("工具轮不应重发，实际 %d 次", len(*reqs))
	}
}

// TestResponsesReasoningOnlyFailsOnlyAfterRescue 端到端：同一形态走 /v1/responses，
// 救援段有正文时客户端应拿到正常 completed，而不是 502。
func TestResponsesReasoningOnlyFailsOnlyAfterRescue(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, testSeg2Text })
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		*reqs = append(*reqs, string(raw))
		body := testSeg2Text
		if len(*reqs) == 1 {
			body = segReasoningOnlyStop
		}
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	body := `{"model":"cn:deepseek-v4.1-flash","stream":true,"max_output_tokens":4096,
		"reasoning":{"effort":"high"},"input":[{"role":"user","content":"你好"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))

	if len(*reqs) != 2 {
		t.Fatalf("上游调用=%d want 2（首段 + 救援段）", len(*reqs))
	}
	if rec.Code != 200 {
		t.Fatalf("状态码=%d want 200（救援后应有正文，不该 502）\n%s", rec.Code, rec.Body.String())
	}
	s := rec.Body.String()
	if strings.Contains(s, "empty_reasoning_only") {
		t.Fatalf("救援后不应再回 empty_reasoning_only:\n%s", s)
	}
	if !strings.Contains(s, "response.completed") {
		t.Fatalf("应是正常 completed:\n%s", s)
	}
	if !strings.Contains(s, "继续") {
		t.Fatalf("客户端应看到救援段正文:\n%s", s)
	}
}
