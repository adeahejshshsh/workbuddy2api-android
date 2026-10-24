package server

// 静默回合救援（issue #44）的服务端行为：
//   1) forceToolChoiceRequired 的边界（只在客户端没钉死工具选择时才改）；
//   2) 端到端：模型零内容 stop → 换成 tool_choice=required 重发 → 客户端拿到工具调用，
//      且整个过程它一个字节都没提前收到（扣帧）。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func TestForceToolChoiceRequired(t *testing.T) {
	const tools = `"tools":[{"type":"function","function":{"name":"do_it","parameters":{"type":"object","properties":{}}}}]`
	cases := []struct {
		name string
		body string
		want bool // 是否应改写
	}{
		{"缺省 tool_choice", `{"model":"m","messages":[],` + tools + `}`, true},
		{"auto 字符串", `{"model":"m","tool_choice":"auto","messages":[],` + tools + `}`, true},
		{"auto 对象", `{"model":"m","tool_choice":{"type":"auto"},"messages":[],` + tools + `}`, true},
		{"none 字符串（客户端明确关工具）", `{"model":"m","tool_choice":"none","messages":[],` + tools + `}`, false},
		{"none 对象", `{"model":"m","tool_choice":{"type":"none"},"messages":[],` + tools + `}`, false},
		{"钉死具体函数", `{"model":"m","tool_choice":{"type":"function","function":{"name":"do_it"}},"messages":[],` + tools + `}`, false},
		{"没有工具声明", `{"model":"m","messages":[]}`, false},
		{"非法 JSON", `not json`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, ok := forceToolChoiceRequired([]byte(c.body))
			if ok != c.want {
				t.Fatalf("ok=%v want %v (out=%s)", ok, c.want, out)
			}
			if !c.want {
				if out != nil {
					t.Fatalf("不该返回改写结果: %s", out)
				}
				return
			}
			var obj map[string]any
			if err := json.Unmarshal(out, &obj); err != nil {
				t.Fatalf("改写结果不是 JSON: %v", err)
			}
			if obj["tool_choice"] != "required" {
				t.Fatalf("tool_choice=%v want required", obj["tool_choice"])
			}
			// 其余字段必须原样保留（工具声明不能被顺手丢掉）
			if _, ok := obj["tools"]; !ok {
				t.Fatalf("tools 被丢了: %s", out)
			}
		})
	}
}

// 大整数不能被改写掉精度（UseNumber 的理由）：seed 类字段超过 2^53 时默认解码会
// 变成近似值，重发就悄悄改了客户端的请求。
func TestForceToolChoiceRequiredKeepsBigNumbers(t *testing.T) {
	body := `{"model":"m","seed":9007199254740993,"tool_choice":"auto","messages":[],` +
		`"tools":[{"type":"function","function":{"name":"do_it","parameters":{"type":"object","properties":{}}}}]}`
	out, ok := forceToolChoiceRequired([]byte(body))
	if !ok {
		t.Fatal("应改写")
	}
	if !strings.Contains(string(out), "9007199254740993") {
		t.Fatalf("大整数精度丢了: %s", out)
	}
}

const silentStopSSE = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"x\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":0,\"total_tokens\":1}}\n\n" +
	"data: [DONE]\n\n"

const toolCallSSE = "data: {\"id\":\"c2\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"do_it\",\"arguments\":\"{}\"}}]},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"c2\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"x\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
	"data: [DONE]\n\n"

// silentThenToolUpstream 第 1 次回「零内容 stop」，第 2 次回工具调用，并记下每次
// 上游**实际收到**的 tool_choice —— 「重发时真的改成了 required」是核心验收点。
type silentThenToolUpstream struct {
	mu          sync.Mutex
	bodies      []string
	toolChoices []string
}

func (u *silentThenToolUpstream) client() *upstream.Client {
	return &upstream.Client{
		ChatBaseCN: "https://fake-upstream.test",
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			var obj map[string]any
			_ = json.Unmarshal(raw, &obj)
			tc, _ := obj["tool_choice"].(string)

			u.mu.Lock()
			u.bodies = append(u.bodies, string(raw))
			u.toolChoices = append(u.toolChoices, tc)
			n := len(u.bodies)
			u.mu.Unlock()

			body := silentStopSSE
			if n > 1 {
				body = toolCallSSE
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
	}
}

func (u *silentThenToolUpstream) seen() (n int, toolChoices []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.bodies), append([]string(nil), u.toolChoices...)
}

// 带 1 个工具声明的流式请求（declaredTools > 0 才可能触发救援）。
const silentStopRequest = `{"model":"deepseek-v4.1-flash-sg","stream":true,` +
	`"messages":[{"role":"user","content":"hi"}],` +
	`"tools":[{"type":"function","function":{"name":"do_it","parameters":{"type":"object","properties":{}}}}]}`

// TestStreamSilentStopRetriesWithRequiredToolChoice ⭐ 核心：模型零内容 stop →
// 自动改成 tool_choice=required 重发一次 → 客户端拿到工具调用，且**从未**收到
// 那半截静默流（扣帧生效）。
func TestStreamSilentStopRetriesWithRequiredToolChoice(t *testing.T) {
	up := &silentThenToolUpstream{}
	p := testPoolWith(
		&auth.Auth{UID: "a1", AccessToken: "at-a1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a2", AccessToken: "at-a2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up.client(), MaxRotate: 3})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(silentStopRequest)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"tool_calls"`) {
		t.Fatalf("客户端没拿到工具调用（救援失败）: %s", out)
	}
	if !strings.HasSuffix(strings.TrimRight(out, "\n"), "data: [DONE]") {
		t.Fatalf("流应以 [DONE] 收尾: %q", out)
	}
	// 静默那一跳的 finish_reason=stop 不该出现在客户端流里（它被扣住后丢弃了）
	if strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("客户端收到了被丢弃那一跳的收尾帧: %s", out)
	}

	n, tcs := up.seen()
	if n != 2 {
		t.Fatalf("上游应被调用 2 次（1 静默 + 1 重发），实际 %d", n)
	}
	if tcs[0] != "" {
		t.Fatalf("第 1 次不该带 tool_choice，实际 %q", tcs[0])
	}
	if tcs[1] != "required" {
		t.Fatalf("重发时必须把 tool_choice 改成 required，实际 %q", tcs[1])
	}
}

// 客户端明确关掉工具（tool_choice=none）时不该救援：模型不调工具是**正确行为**，
// 重发成 required 会把它变成「必须调工具」。
func TestStreamSilentStopNoRetryWhenToolsOff(t *testing.T) {
	up := &silentThenToolUpstream{}
	p := testPoolWith(
		&auth.Auth{UID: "a1", AccessToken: "at-a1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a2", AccessToken: "at-a2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up.client(), MaxRotate: 3})

	body := strings.Replace(silentStopRequest, `"stream":true`, `"stream":true,"tool_choice":"none"`, 1)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	if n, _ := up.seen(); n != 1 {
		t.Fatalf("tool_choice=none 时不该重发，上游被调用 %d 次", n)
	}
	// 静默流照旧透传（含 stop 收尾）
	if !strings.Contains(rec.Body.String(), `"finish_reason":"stop"`) {
		t.Fatalf("静默流应原样透传: %s", rec.Body.String())
	}
}

// 单账号池 + MaxRotate=1 也要有救援机会：静默回合的那个号本身没问题（上游回了合法
// 流），不该因为「轮转名额用尽」或「同一个号被 tried 排除」而放弃救援 —— 那会让这条
// 修复在最保守的配置下等于不存在。
func TestStreamSilentStopRetryWorksWithSingleAccount(t *testing.T) {
	up := &silentThenToolUpstream{}
	p := testPoolWith(&auth.Auth{UID: "only", AccessToken: "at-only", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up.client(), MaxRotate: 1})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(silentStopRequest)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"tool_calls"`) {
		t.Fatalf("单账号池救援失败（客户端没拿到工具调用）: %s", rec.Body.String())
	}
	n, tcs := up.seen()
	if n != 2 {
		t.Fatalf("上游应被调用 2 次（1 静默 + 1 重发），实际 %d", n)
	}
	if tcs[1] != "required" {
		t.Fatalf("重发时必须带 tool_choice=required，实际 %q", tcs[1])
	}
}
