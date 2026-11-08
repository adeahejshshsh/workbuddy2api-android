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

// fakeContinueUpstream 按调用次数返回脚本化 SSE 并记录每次实际发出的请求体。
// 注意计数口径：单测里「段1」是手工喂给 reader 的（不经 fake），所以 fake 的
// call=1 就是第一次续写请求——script 按「第几次续写」编写。
func fakeContinueUpstream(t *testing.T, script func(call int) (int, string)) (*upstream.Client, *[]string) {
	t.Helper()
	reqs := &[]string{}
	call := 0
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			call++
			raw, _ := io.ReadAll(r.Body)
			*reqs = append(*reqs, string(raw))
			status, body := script(call)
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	return up, reqs
}

// 段1：正文截断（finish_reason=length），带 usage。
const testSeg1Text = `data: {"id":"cmpl-1","choices":[{"index":0,"delta":{"role":"assistant","content":"你好"}}]}` + "\n\n" +
	`data: {"id":"cmpl-1","choices":[{"index":0,"delta":{"content":"世界"}}]}` + "\n\n" +
	`data: {"id":"cmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}` + "\n\n" +
	"data: [DONE]\n\n"

// 续写段：自然收尾（finish_reason=stop），带 usage。
const testSeg2Text = `data: {"id":"cmpl-2","choices":[{"index":0,"delta":{"content":"继续"}}]}` + "\n\n" +
	`data: {"id":"cmpl-2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":7,"total_tokens":27}}` + "\n\n" +
	"data: [DONE]\n\n"

func newTestContinueReader(t *testing.T, up *upstream.Client, body string, rc io.ReadCloser) *continueReader {
	t.Helper()
	acct := &auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999}
	return newContinueReader(context.Background(), up, acct, []byte(body), "1.2.3.4", upstream.ChatMeta{}, rc)
}

// TestContinueReaderTextCutContinues 正文截断 → 同模型续写；客户端可见流是一条
// 连续输出：两段正文都在、终态只留 stop、length 不外泄、usage 为跨段合计。
func TestContinueReaderTextCutContinues(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, testSeg2Text })
	r := newTestContinueReader(t, up, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`,
		io.NopCloser(strings.NewReader(testSeg1Text)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(out)
	if len(*reqs) != 1 {
		t.Fatalf("续写请求数=%d want 1", len(*reqs))
	}
	for _, want := range []string{"你好", "世界", "继续", `"finish_reason":"stop"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("输出缺少 %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, `"finish_reason":"length"`) {
		t.Fatalf("length 终态不应外泄:\n%s", s)
	}
	// usage：恰好一条，prompt 取末段（20），completion 跨段累计（5+7=12），
	// total 按 prompt+completion 重算（32）——跨段 prompt 求和会把续写段
	// 携带的整个上下文重复计入，客户端按 usage 判上下文压力会提前压缩。
	var usageFrames int
	var usage map[string]any
	for _, f := range strings.Split(s, "\n\n") {
		line, ok := strings.CutPrefix(f, "data: ")
		if !ok || line == "" || line == "[DONE]" {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(line), &obj) != nil {
			continue
		}
		if u, ok := obj["usage"].(map[string]any); ok {
			usageFrames++
			usage = u
		}
	}
	if usageFrames != 1 {
		t.Fatalf("usage 帧数=%d want 1:\n%s", usageFrames, s)
	}
	if usage["prompt_tokens"] != float64(20) || usage["completion_tokens"] != float64(12) || usage["total_tokens"] != float64(32) {
		t.Fatalf("usage 合并=%v want prompt=20 completion=12 total=32", usage)
	}
	// 续写请求：同模型 + assistant 携带已输出内容 + 续写指令 + 原始消息仍在。
	req := (*reqs)[0]
	for _, want := range []string{`"model":"glm-5.2"`, "你好世界", "从中断处继续输出剩余内容", `"content":"hi"`} {
		if !strings.Contains(req, want) {
			t.Fatalf("续写请求缺少 %q:\n%s", want, req)
		}
	}
	if !strings.Contains(req, `"role":"assistant"`) {
		t.Fatalf("续写请求缺 assistant 消息:\n%s", req)
	}
}

// TestContinueReaderToolCutNoContinue 工具调用分片在场 → 不续写（零续写请求），
// finish=length 原样外泄（交回既有截断语义）。
func TestContinueReaderToolCutNoContinue(t *testing.T) {
	seg := `data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Bash","arguments":"{\"cmd\":"}}]}}]}` + "\n\n" +
		`data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, testSeg2Text })
	r := newTestContinueReader(t, up, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		io.NopCloser(strings.NewReader(seg)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 0 {
		t.Fatalf("续写请求数=%d want 0（工具截断不续）", len(*reqs))
	}
	if !strings.Contains(string(out), `"finish_reason":"length"`) {
		t.Fatalf("length 终态应原样回放:\n%s", out)
	}
}

// TestContinueReaderMidToolCallCutNoRetry 断流落在工具调用参数中间 → 不盲重试。
// 与 finish=length 那条同一条理由（本文件开头写明）：工具参数已被切，重试体只带
// 正文/推理当 assistant 前缀（continuationBody 不含残缺 tool_calls），模型会重发一遍
// 调用，客户端在同一块里拼接两次参数 → 半截 JSON 拼成垃圾，比"不续"更糟。
// 形态刻意带上正文（模型先说话再调工具是常态），否则零输出守卫会掩盖这条路径。
func TestContinueReaderMidToolCallCutNoRetry(t *testing.T) {
	cut := `data: {"id":"c","choices":[{"index":0,"delta":{"content":"好的，我来执行"}}]}` + "\n\n" +
		`data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Bash","arguments":"{\"cmd\":"}}]}}]}` + "\n\n"
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, testSeg2Text })
	r := newTestContinueReader(t, up, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		io.NopCloser(strings.NewReader(cut)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 0 {
		t.Fatalf("工具调用中断流不得盲重试，实际发了 %d 次续写请求", len(*reqs))
	}
	// 客户端仍要知道这一回合是残缺的：补截断终态（恰好一个）。
	if n := strings.Count(string(out), `"finish_reason":"length"`); n != 1 {
		t.Fatalf("截断终态帧应恰好一个，got=%d:\n%s", n, out)
	}
	if !strings.Contains(string(out), `"arguments":"{\"cmd\":"`) {
		t.Fatalf("已发出的工具分片应原样保留:\n%s", out)
	}
}

// TestContinueReaderExplicitLimitNoContinue 客户端显式输出限额 → 截断是它的
// 预期语义，不续写。
func TestContinueReaderExplicitLimitNoContinue(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, testSeg2Text })
	r := newTestContinueReader(t, up, `{"model":"m","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`,
		io.NopCloser(strings.NewReader(testSeg1Text)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 0 {
		t.Fatalf("续写请求数=%d want 0（显式限额不续）", len(*reqs))
	}
	if !strings.Contains(string(out), `"finish_reason":"length"`) {
		t.Fatalf("length 终态应原样回放:\n%s", out)
	}
}

// TestContinueReaderMainSegCutBlindRetry 主段断流（有正文、无 finish、无 [DONE]）也做
// 一次盲重试：断流不是「请求本身失败」——它已经产出过内容，重试请求体把已收内容作为
// assistant 前缀（幂等不重复），客户端因此看到的是接续而不是半截回合。线上实测这是
// Codex「莫名中断、要手动继续」的主因。
func TestContinueReaderMainSegCutBlindRetry(t *testing.T) {
	cut := `data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant"}}]}` + "\n\n" +
		`data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"半截正文"}}]}` + "\n\n"
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, testSeg2Text })
	r := newTestContinueReader(t, up, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		io.NopCloser(strings.NewReader(cut)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(out)
	if len(*reqs) != 1 {
		t.Fatalf("主段断流应发一次盲重试，got=%d", len(*reqs))
	}
	if !strings.Contains((*reqs)[0], "半截正文") {
		t.Fatalf("重试请求应携带已收内容（幂等不重复）:\n%s", (*reqs)[0])
	}
	for _, want := range []string{"半截正文", "继续", `"finish_reason":"stop"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("客户端可见流缺 %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, `"finish_reason":"length"`) {
		t.Fatalf("续上之后 length 终态不应外泄:\n%s", s)
	}
}

// TestContinueReaderMainSegCutRetryFailsEmitsTerminal 盲重试拿不到 200 → 降级补截断
// 终态（恰好一个），客户端仍能判断这个回合是残缺的。
func TestContinueReaderMainSegCutRetryFailsEmitsTerminal(t *testing.T) {
	cut := `data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"半截正文"}}]}` + "\n\n"
	up, _ := fakeContinueUpstream(t, func(call int) (int, string) { return 500, `{"code":11102,"msg":"boom"}` })
	r := newTestContinueReader(t, up, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		io.NopCloser(strings.NewReader(cut)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(out)
	if n := strings.Count(s, `"finish_reason":"length"`); n != 1 {
		t.Fatalf("截断终态帧应恰好一个，got=%d:\n%s", n, s)
	}
	if idx := strings.Index(s, `"finish_reason":"length"`); idx < strings.Index(s, "半截正文") {
		t.Fatalf("终态帧必须在正文之后:\n%s", s)
	}
}

// TestContinueReaderCutNoContentNoTerminal 断流但零模型输出（只有 role 帧）→ 不补
// 终态：补了会让 StreamHint 的 validEvents 从 0 变 1，把空流本该收敛的 502 观测
// 伪装成「有内容的 200」。空流判据必须留在上游侧。
func TestContinueReaderCutNoContentNoTerminal(t *testing.T) {
	onlyRole := `data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant"}}]}` + "\n\n"
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, testSeg2Text })
	r := newTestContinueReader(t, up, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		io.NopCloser(strings.NewReader(onlyRole)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(out), "finish_reason") {
		t.Fatalf("零输出断流不得补终态（会让空流 502 观测失真）:\n%s", out)
	}
	// 也没有可续的东西：盲重试的资格要求「已经产出过内容」。
	if len(*reqs) != 0 {
		t.Fatalf("零输出断流不该发盲重试，got=%d", len(*reqs))
	}
}

// TestContinueReaderNormalEndNoDuplicateTerminal 正常收尾（finish=stop + [DONE]）：
// 不得多补一条终态（客户端会看到两个 finish_reason）。
func TestContinueReaderNormalEndNoDuplicateTerminal(t *testing.T) {
	up, _ := fakeContinueUpstream(t, func(call int) (int, string) { return 200, testSeg2Text })
	r := newTestContinueReader(t, up, `{"model":"m","max_tokens":9,"messages":[{"role":"user","content":"hi"}]}`,
		io.NopCloser(strings.NewReader(testSeg2Text)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if n := strings.Count(string(out), "finish_reason"); n != 1 {
		t.Fatalf("终态帧应恰好一个，got=%d:\n%s", n, out)
	}
}

// TestStreamCutTruncatedAcrossAllProtocols 同一份断流，三条适配器都必须如实标截断：
// 补在 continue 这一处接缝（唯一共用层），三条路径各自把 length 映射到自己的词表。
// 这是用户报的 bug 的端到端回归：此前 chat 裸 [DONE]、responses 标 completed、
// messages 标 end_turn——agent 客户端（Codex/CC）把半截回合当正常结束。
func TestStreamCutTruncatedAcrossAllProtocols(t *testing.T) {
	cut := `data: {"id":"c1","object":"chat.completion.chunk","model":"glm-5.2","choices":[{"index":0,"delta":{"role":"assistant"}}]}` + "\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","model":"glm-5.2","choices":[{"index":0,"delta":{"content":"半截"}}]}` + "\n\n"
	cases := []struct {
		name, path, body string
		want             []string
		reject           []string
	}{
		{
			"chat", "/v1/chat/completions",
			`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			[]string{`"finish_reason":"length"`, "data: [DONE]"}, nil,
		},
		{
			"responses", "/v1/responses",
			`{"model":"glm-5.2","stream":true,"input":"hi"}`,
			[]string{`"type":"response.incomplete"`, `"reason":"max_output_tokens"`},
			[]string{`"type":"response.completed"`},
		},
		{
			"messages", "/v1/messages",
			`{"model":"glm-5.2","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`,
			[]string{`"stop_reason":"max_tokens"`},
			[]string{`"stop_reason":"end_turn"`},
		},
	}
	for _, tc := range cases {
		up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, cut, true })
		p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
		h := NewHandler(Config{Pool: p, Upstream: up})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body)))
		body := rec.Body.String()
		for _, w := range tc.want {
			if !strings.Contains(body, w) {
				t.Errorf("%s: 期望收尾 %s，实际:\n%s", tc.name, w, body)
			}
		}
		for _, r := range tc.reject {
			if strings.Contains(body, r) {
				t.Errorf("%s: 断流不得伪装成正常完成（%s）:\n%s", tc.name, r, body)
			}
		}
	}
}

// TestStreamCutMidToolCallMarksTruncated 断流落在工具调用参数中间（agent 客户端最
// 常见的形态）：必须仍然补截断终态（guard 里 toolSeen 就是为此），且不与既有工具帧
// 重复/错序——客户端据此能判断这个调用是残缺的，而不是拿半截 JSON 去执行。
func TestStreamCutMidToolCallMarksTruncated(t *testing.T) {
	cut := `data: {"id":"c1","object":"chat.completion.chunk","model":"glm-5.2","choices":[{"index":0,"delta":{"role":"assistant"}}]}` + "\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","model":"glm-5.2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":"}}]}}]}` + "\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, cut, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	body := rec.Body.String()
	if n := strings.Count(body, `"finish_reason":"length"`); n != 1 {
		t.Fatalf("工具调用中断流应恰好一个截断终态，got=%d:\n%s", n, body)
	}
	if !strings.Contains(body, `"tool_calls"`) {
		t.Fatalf("工具调用帧应保留:\n%s", body)
	}
	if strings.Index(body, `"tool_calls"`) > strings.Index(body, `"finish_reason":"length"`) {
		t.Fatalf("终态必须落在工具调用帧之后:\n%s", body)
	}
}

// TestResponsesStreamCutAutoContinued 端到端（用户报的那个症状）：上游流半路断掉
// （有正文、无 finish_reason、无 [DONE]）→ 网关自动带已收内容续写一次 → 客户端看到的
// 是一条连续输出、以 response.completed 正常收尾，而不是「半截 + 要手动继续」。
func TestResponsesStreamCutAutoContinued(t *testing.T) {
	cut := `data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"半截"}}]}` + "\n\n"
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) {
		if call == 1 {
			return 200, cut
		}
		return 200, testSeg2Text
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"m","stream":true,"input":"hi"}`)))
	body := rec.Body.String()
	if len(*reqs) != 2 {
		t.Fatalf("应恰好 1 次自动续写（共 2 次上游调用），got=%d", len(*reqs))
	}
	if !strings.Contains((*reqs)[1], "半截") {
		t.Fatalf("续写请求应带已收内容（幂等不重复）:\n%s", (*reqs)[1])
	}
	for _, want := range []string{"半截", "继续", "response.completed"} {
		if !strings.Contains(body, want) {
			t.Fatalf("客户端可见流缺 %q:\n%s", want, body)
		}
	}
	for _, bad := range []string{"response.incomplete", "max_output_tokens"} {
		if strings.Contains(body, bad) {
			t.Fatalf("续上之后不得报截断（%s）:\n%s", bad, body)
		}
	}
}

// TestNonStreamCutTruncatedAcrossAllProtocols 同一根因在非流式一侧：客户端不带
// stream 时，网关向**自己**聚合上游流（上游禁非流式），Aggregate 的 finish_reason
// 就是客户端看到的那个。responses / messages 的非流式按同一字段推导 status /
// stop_reason，所以 Aggregate 一处修正三边同时生效——这条测试盯住那条传导链。
func TestNonStreamCutTruncatedAcrossAllProtocols(t *testing.T) {
	cut := `data: {"id":"c1","object":"chat.completion.chunk","model":"glm-5.2","choices":[{"index":0,"delta":{"role":"assistant"}}]}` + "\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","model":"glm-5.2","choices":[{"index":0,"delta":{"content":"半截"}}]}` + "\n\n"
	cases := []struct {
		name, path, body string
		want             []string
		reject           []string
	}{
		{
			"chat", "/v1/chat/completions",
			`{"model":"glm-5.2","stream":false,"messages":[{"role":"user","content":"hi"}]}`,
			[]string{`"finish_reason":"length"`},
			[]string{`"finish_reason":"stop"`},
		},
		{
			"responses", "/v1/responses",
			`{"model":"glm-5.2","stream":false,"input":"hi"}`,
			[]string{`"status":"incomplete"`, `"reason":"max_output_tokens"`},
			[]string{`"status":"completed"`},
		},
		{
			"messages", "/v1/messages",
			`{"model":"glm-5.2","stream":false,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`,
			[]string{`"stop_reason":"max_tokens"`},
			[]string{`"stop_reason":"end_turn"`},
		},
	}
	for _, tc := range cases {
		up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, cut, true })
		p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
		h := NewHandler(Config{Pool: p, Upstream: up})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body)))
		body := rec.Body.String()
		for _, w := range tc.want {
			if !strings.Contains(body, w) {
				t.Errorf("%s: 期望 %s，实际:\n%s", tc.name, w, body)
			}
		}
		for _, r := range tc.reject {
			if strings.Contains(body, r) {
				t.Errorf("%s: 断流不得伪装成正常完成（%s）:\n%s", tc.name, r, body)
			}
		}
	}
}

// TestContinueReaderSeg2FailureDegrades 续写请求失败 → 降级为截断终态：
// tail 回放（length 在场）、不挂死、账目仍记（段1 usage 合成帧在场）。
func TestContinueReaderSeg2FailureDegrades(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) {
		return 500, `{"code":11102,"msg":"boom"}`
	})
	r := newTestContinueReader(t, up, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		io.NopCloser(strings.NewReader(testSeg1Text)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(out)
	if len(*reqs) != 1 {
		t.Fatalf("续写请求数=%d want 1", len(*reqs))
	}
	if !strings.Contains(s, `"finish_reason":"length"`) {
		t.Fatalf("降级后 length 终态应在场:\n%s", s)
	}
	if !strings.Contains(s, `"completion_tokens":5`) {
		t.Fatalf("段1 usage 应合成在场:\n%s", s)
	}
}

// TestContinueReaderSeamReasoningDropped 正文已开始后的续写段 reasoning 不外泄
// （仅累计，供下一段请求带回）：不能让 responses 侧在 message item 还开着时开
// 第二个 reasoning item（否则 codex 报 "OutputTextDelta without active item"）。
// 正文未开始时相反——照常外泄，见 TestContinueReaderSeg2ReasoningForwarded。
func TestContinueReaderSeamReasoningDropped(t *testing.T) {
	seg1 := `data: {"id":"cmpl-1","choices":[{"index":0,"delta":{"reasoning_content":"公开思考"}}]}` + "\n\n" + testSeg1Text
	seg2 := `data: {"id":"cmpl-2","choices":[{"index":0,"delta":{"reasoning_content":"内部思考"}}]}` + "\n\n" +
		`data: {"id":"cmpl-2","choices":[{"index":0,"delta":{"content":"继续"}}]}` + "\n\n" +
		`data: {"id":"cmpl-2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, seg2 })
	r := newTestContinueReader(t, up, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		io.NopCloser(strings.NewReader(seg1)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "公开思考") {
		t.Fatalf("段1 思考应外泄:\n%s", s)
	}
	if strings.Contains(s, "内部思考") {
		t.Fatalf("续写段思考不应外泄:\n%s", s)
	}
	if !strings.Contains(s, "继续") {
		t.Fatalf("续写段正文应外泄:\n%s", s)
	}
	// 续写请求应携带第一段累计的思考（上下文一致性；段2 的思考若产生段3 请求才携带）。
	if !strings.Contains((*reqs)[0], "公开思考") {
		t.Fatalf("续写请求应携带累计思考:\n%s", (*reqs)[0])
	}
}

// TestContinueReaderCapAtMaxSegments 连续截断封顶：每段都以 length 结束时，
// 最多 1 次原生 + (maxContinueSegments-1) 次续写，不无限续。
func TestContinueReaderCapAtMaxSegments(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, testSeg1Text })
	r := newTestContinueReader(t, up, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		io.NopCloser(strings.NewReader(testSeg1Text)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != maxContinueSegments-1 {
		t.Fatalf("续写请求数=%d want %d（封顶）", len(*reqs), maxContinueSegments-1)
	}
	if !strings.Contains(string(out), `"finish_reason":"length"`) {
		t.Fatalf("封顶后 length 终态应在场:\n%s", out)
	}
}

// TestContinueReaderSeg2ReasoningForwarded 正文尚未开始（text=0B）时的续写段
// 思考照常外泄：此时的截断全是「纯思考烧满」形态，放行思考既续着同一个
// reasoning item（零协议风险），又消掉客户端侧静默（codex stream_idle_timeout
// 会在长静默时判 responseStreamDisconnected 掐线，实测静默约 122s 断开）。
func TestContinueReaderSeg2ReasoningForwarded(t *testing.T) {
	// 段1：纯思考截断（正文 0B —— 线上观测到的全部续写场景）。
	seg1 := `data: {"id":"cmpl-1","choices":[{"index":0,"delta":{"reasoning_content":"公开思考"}}]}` + "\n\n" +
		`data: {"id":"cmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	seg2 := `data: {"id":"cmpl-2","choices":[{"index":0,"delta":{"reasoning_content":"内部思考"}}]}` + "\n\n" +
		`data: {"id":"cmpl-2","choices":[{"index":0,"delta":{"content":"继续"}}]}` + "\n\n" +
		`data: {"id":"cmpl-2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, seg2 })
	r := newTestContinueReader(t, up, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		io.NopCloser(strings.NewReader(seg1)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(out)
	if len(*reqs) != 1 {
		t.Fatalf("续写请求数=%d want 1", len(*reqs))
	}
	if !strings.Contains(s, "内部思考") {
		t.Fatalf("正文未开始时续写段思考应放行（消静默）:\n%s", s)
	}
	if strings.Index(s, "内部思考") > strings.Index(s, "继续") {
		t.Fatalf("思考应先于正文出现:\n%s", s)
	}
	// 放行不等于放弃累计：首次续写请求应携带段1 累计思考（上下文一致性；
	// 段2 的思考若再触发续写请求才会携带）。
	req := (*reqs)[0]
	if !strings.Contains(req, "公开思考") {
		t.Fatalf("续写请求应携带累计思考:\n%s", req)
	}
}

// TestContinueReaderKeepaliveOnEmptyFrames 残余静默源（续写段的全空 no-op 帧）
// 按 keepaliveEvery 补 SSE 注释帧——chat 路径直达客户端喂活连接。注释帧是
// SSE 规范 no-op（": keepalive"），不得影响正文与终态。
func TestContinueReaderKeepaliveOnEmptyFrames(t *testing.T) {
	old := keepaliveEvery
	keepaliveEvery = 0 // 确定性：每条被吞帧都补，不依赖墙钟
	t.Cleanup(func() { keepaliveEvery = old })

	// 段1：带正文截断（text>0，续写段思考会走剥离分支）。
	// 段2：全空 no-op 帧（残余静默源）→ 触发 keepalive。
	empty := `data: {"id":"cmpl-2","choices":[{"index":0,"delta":{}}]}` + "\n\n"
	seg2 := empty +
		`data: {"id":"cmpl-2","choices":[{"index":0,"delta":{"reasoning_content":"内部思考"}}]}` + "\n\n" +
		`data: {"id":"cmpl-2","choices":[{"index":0,"delta":{"content":"继续"}}]}` + "\n\n" +
		`data: {"id":"cmpl-2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, seg2 })
	r := newTestContinueReader(t, up, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		io.NopCloser(strings.NewReader(testSeg1Text)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(out)
	if len(*reqs) != 1 {
		t.Fatalf("续写请求数=%d want 1", len(*reqs))
	}
	if n := strings.Count(s, ": keepalive"); n < 1 {
		t.Fatalf("keepalive 注释帧=%d want ≥1:\n%s", n, s)
	}
	if strings.Contains(s, "内部思考") {
		t.Fatalf("正文已开始后续写段思考不应外泄:\n%s", s)
	}
	// 注释帧不能破坏正常收尾：正文与 stop 终态照常在。
	for _, want := range []string{"你好", "世界", "继续", `"finish_reason":"stop"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("输出缺少 %q:\n%s", want, s)
		}
	}
}

// TestContinueInStreamErrorFrameNoRetry 上游把失败塞在流里（HTTP 200 内嵌
// {code,msg,displayMsg}、无 choices 无 error）时：按显式失败终止，不续写、不盲重试
// ——撞的是已拦下这个账号的策略，重试只是白跑一次；此前这种帧不置 errSeen，日志会把它
// 记成「段无 finish 结束（疑似上游断流）」并让续写段白跑一次盲重试。
func TestContinueInStreamErrorFrameNoRetry(t *testing.T) {
	blocked := `data: {"code":11140,"msg":"request illegal","displayMsg":"请求被安全策略拦截"}` + "\n\n"

	// ① 续写段（seg=1）收到拦截帧：不盲重试（首次请求由测试直接喂入，故只应有 1 次
	// 取流请求 = 那次续写；未修时这里会变成 2 —— 多一次白撞同一账号的盲重试）。
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) {
		if call == 1 {
			return 200, blocked
		}
		return 200, testSeg2Text
	})
	r := newTestContinueReader(t, up, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		io.NopCloser(strings.NewReader(testSeg1Text)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("拦截帧不得触发盲重试：取流请求数=%d want 1（未修时为 2）", len(*reqs))
	}
	if s := string(out); !strings.Contains(s, "11140") || !strings.Contains(s, "request illegal") {
		t.Fatalf("拦截帧应如实透传给客户端：\n%s", s)
	}

	// ② 主段（seg=0）收到拦截帧：同样按显式失败终止，不续写。
	// 主段由初始流喂入（截到 length 终态之前 + 拦截帧 + EOF），故取流请求数应为 0。
	mainSeg := testSeg1Text[:strings.Index(testSeg1Text, `data: {"id":"cmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"length"}]`)] + blocked
	up2, reqs2 := fakeContinueUpstream(t, func(call int) (int, string) { return 200, testSeg2Text })
	r2 := newTestContinueReader(t, up2, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		io.NopCloser(strings.NewReader(mainSeg)))
	out2, err := io.ReadAll(r2)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs2) != 0 {
		t.Fatalf("主段拦截不得发起续写：取流请求数=%d want 0（未修时为 1）", len(*reqs2))
	}
	if s := string(out2); !strings.Contains(s, "11140") || !strings.Contains(s, "你好") {
		t.Fatalf("主段拦截：正文照常 + 拦截帧如实透传，实际：\n%s", s)
	}
}

// TestContinueReaderSeg2StreamCutBlindRetry 续写段被上游断流（无 finish_reason
// 直接 EOF）时盲重试一次：重试请求体与断流段逐字节相同（已收内容都进 assistant
// 前缀，幂等不重复），客户端最终拿到完整内容。线上 17:50 的「32k 处截断」正是
// 这条路径（15 次续写里 1 次上游断流）。
func TestContinueReaderSeg2StreamCutBlindRetry(t *testing.T) {
	// 断流段：有正文、无 finish_reason、无 [DONE]，直接 EOF。
	segCut := `data: {"id":"cmpl-2","choices":[{"index":0,"delta":{"content":"部分"}}]}` + "\n\n"
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) {
		if call == 1 {
			return 200, segCut
		}
		return 200, testSeg2Text
	})
	r := newTestContinueReader(t, up, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		io.NopCloser(strings.NewReader(testSeg1Text)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(out)
	if len(*reqs) != 2 {
		t.Fatalf("盲重试后应有 2 次续写请求，实际 %d", len(*reqs))
	}
	if !strings.Contains((*reqs)[1], "部分") {
		t.Fatalf("重试请求应携带断流段已收内容（幂等不重复）:\n%s", (*reqs)[1])
	}
	for _, want := range []string{"你好", "世界", "部分", "继续", `"finish_reason":"stop"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("客户端可见流缺 %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, `"finish_reason":"length"`) {
		t.Fatalf("续写成功后 length 终态不应外泄:\n%s", s)
	}
}

func TestBareModelOfStripsRealmPrefix(t *testing.T) {
	cases := []struct{ body, want string }{
		{`{"model":"cn:glm-5.2"}`, "glm-5.2"},
		{`{"model":"global:gpt-x"}`, "gpt-x"},
		{`{"model":"glm-5.2"}`, "glm-5.2"},
		{`{"model":"weird:model"}`, "weird:model"},
		{`not json`, "?"},
	}
	for _, c := range cases {
		if got := bareModelOf([]byte(c.body)); got != c.want {
			t.Errorf("bareModelOf(%s) = %q, want %q", c.body, got, c.want)
		}
	}
}

// 请求体带路由前缀时，断流补的截断终态帧仍须是裸模型名。
func TestContinueFrameUsesBareModel(t *testing.T) {
	up, _ := fakeContinueUpstream(t, func(call int) (int, string) { return 200, testSeg2Text })
	// 显式 max_tokens → limitSet，断流后不续写，直接补截断终态帧。
	cut := `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"半截"}}]}` + "\n\n"
	r := newTestContinueReader(t, up, `{"model":"cn:glm-5.2","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`,
		io.NopCloser(strings.NewReader(cut)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"model":"glm-5.2"`) {
		t.Fatalf("截断终态帧缺少裸模型名:\n%s", s)
	}
	if strings.Contains(s, `"model":"cn:glm-5.2"`) {
		t.Fatalf("截断终态帧泄露了路由前缀:\n%s", s)
	}
	if !strings.Contains(s, `"finish_reason":"length"`) {
		t.Fatalf("截断终态帧缺少 length:\n%s", s)
	}
}
