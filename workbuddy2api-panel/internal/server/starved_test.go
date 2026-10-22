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

// 救援体：DeepSeek + 显式限额 → 关思考、删档位、其余原样。
func TestStarvedRetryBody(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4.1-flash","max_tokens":60,"reasoning_effort":"high",
		"messages":[{"role":"user","content":"hi"}]}`)
	out, ok := starvedRetryBody(body)
	if !ok {
		t.Fatal("应当产出救援体")
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("救援体不是合法 JSON: %v", err)
	}
	th, _ := obj["thinking"].(map[string]any)
	if th["type"] != "disabled" {
		t.Fatalf("thinking.type = %v want disabled", th["type"])
	}
	if _, has := obj["reasoning_effort"]; has {
		t.Error("关思考后不该再带 reasoning_effort")
	}
	// 额度与消息必须原样保留：超发是对客户端的语义违约。
	if obj["max_tokens"].(float64) != 60 {
		t.Errorf("max_tokens 被改了: %v", obj["max_tokens"])
	}
	msgs, _ := obj["messages"].([]any)
	if len(msgs) != 1 {
		t.Errorf("messages 被改了: %v", obj["messages"])
	}

	cases := []struct {
		name, body string
	}{
		{"非 DeepSeek 模型不猜", `{"model":"glm-5.2","max_tokens":60}`},
		{"已经关着思考", `{"model":"deepseek-v4.1-flash","max_tokens":60,"thinking":{"type":"disabled"}}`},
		{"额度小到不值得重发", `{"model":"deepseek-v4.1-flash","max_tokens":8}`},
		{"body 不可解析", `not json`},
	}
	for _, c := range cases {
		if _, ok := starvedRetryBody([]byte(c.body)); ok {
			t.Errorf("%s：不该救援", c.name)
		}
	}
	// realm 前缀也要认
	if _, ok := starvedRetryBody([]byte(`{"model":"cn:deepseek-v4.1-flash","max_tokens":60}`)); !ok {
		t.Error("带 realm 前缀的 DeepSeek 模型应当救援")
	}
}

// 只有「length + 正文空 + 无 tool_calls」才算被思考吃光。
func TestAggregateStarved(t *testing.T) {
	mk := func(fr string, content any, toolCalls bool) map[string]any {
		msg := map[string]any{"role": "assistant", "content": content}
		if toolCalls {
			msg["tool_calls"] = []any{map[string]any{"id": "c1"}}
		}
		return map[string]any{"choices": []any{map[string]any{"finish_reason": fr, "message": msg}}}
	}
	if !aggregateStarved(mk("length", "", false)) {
		t.Error("length + 空正文应当判为吃光")
	}
	if !aggregateStarved(mk("length", nil, false)) {
		t.Error("length + content=null 应当判为吃光")
	}
	if aggregateStarved(mk("length", "有正文", false)) {
		t.Error("有正文的截断不该救援")
	}
	if aggregateStarved(mk("stop", "", false)) {
		t.Error("finish=stop 空回复不是吃光形态")
	}
	if aggregateStarved(mk("length", "", true)) {
		t.Error("有 tool_calls 不该救援")
	}
	if aggregateStarved(map[string]any{}) {
		t.Error("空响应不该救援")
	}
}

func TestOutputBudgetAliases(t *testing.T) {
	for _, k := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		obj := map[string]any{k: float64(123)}
		if got := outputBudget(obj); got != 123 {
			t.Errorf("%s: got %d want 123", k, got)
		}
		if !bodySetsOutputLimit([]byte(`{"model":"x","` + k + `":123}`)) {
			t.Errorf("%s: bodySetsOutputLimit=false", k)
		}
	}
	if outputBudget(map[string]any{}) != 0 {
		t.Error("未携带限额应当返回 0")
	}
	if bodySetsOutputLimit([]byte(`{"model":"x"}`)) {
		t.Error("未携带限额时 bodySetsOutputLimit 应为 false")
	}
}

// starvedUpstreamBody 上游「正文零字节 + finish_reason=length」的 SSE 流。
const starvedUpstreamBody = `data: {"choices":[{"delta":{"reasoning_content":"想很久"}}]}
data: {"choices":[{"delta":{},"finish_reason":"length"}]}
data: [DONE]
`

// normalUpstreamBody 救援成功后的正常流（带正文）。
const normalUpstreamBody = `data: {"choices":[{"delta":{"content":"rescued"}}]}
data: {"choices":[{"delta":{},"finish_reason":"stop"}]}
data: [DONE]
`

// 接线级：第一发返回「思考吃光」的空截断，救援发（thinking=disabled）返回正常内容，
// 客户端最终必须拿到正文。防的是「函数写好了但没接进 handler」。
func TestChatStarvedRetryWires(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	var calls, rescued int
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			raw, _ := io.ReadAll(r.Body)
			body := starvedUpstreamBody
			// 救援发的判据：body 里出现了 thinking.type=disabled
			if strings.Contains(string(raw), `"type":"disabled"`) {
				rescued++
				body = normalUpstreamBody
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	h := NewHandler(Config{Pool: p, Upstream: up})
	body := `{"model":"deepseek-v4.1-flash","max_tokens":60,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if calls < 2 || rescued == 0 {
		t.Fatalf("只打了 %d 次上游、其中关思考 %d 次：救援没有接线", calls, rescued)
	}
	if !strings.Contains(rec.Body.String(), "rescued") {
		t.Fatalf("客户端没拿到救援后的正文: %s", rec.Body.String())
	}
}

// 救援体重写不得动数字原文（报告 #2）：整份 body 会被重发上游，
// 大整数被改成浮点近似就是给上游换了个请求。
func TestStarvedRetryBodyPreservesBigInts(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4.1-flash","max_tokens":60,"seed":9007199254740993,
		"messages":[{"role":"user","content":"hi"}]}`)
	out, ok := starvedRetryBody(body)
	if !ok {
		t.Fatal("应当产出救援体")
	}
	if !strings.Contains(string(out), "9007199254740993") {
		t.Fatalf("大整数被改写: %s", out)
	}
	if !strings.Contains(string(out), `"max_tokens":60`) {
		t.Fatalf("额度被改写: %s", out)
	}
}
