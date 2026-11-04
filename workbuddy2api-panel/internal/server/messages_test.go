package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func TestMessagesToChat(t *testing.T) {
	src := []byte(`{
		"model":"glm-5.2",
		"max_tokens":128,
		"system":[{"type":"text","text":"sys-a","cache_control":{"type":"ephemeral"}},{"type":"text","text":"sys-b"}],
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"look"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aaa"}}
			]},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"secret","signature":"sig"},
				{"type":"text","text":"ok"},
				{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"cmd":"ls"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","content":"a.txt"},
				{"type":"text","text":"next"}
			]}
		],
		"tools":[{"name":"Bash","description":"run","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"tool","name":"Bash","disable_parallel_tool_use":true},
		"stop_sequences":["END"],
		"top_k":5,
		"metadata":{"user_id":"u"},
		"thinking":{"type":"adaptive","display":"summarized"},
		"output_config":{"effort":"high","format":{"type":"json_schema","schema":{"type":"object"}}},
		"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}
		}`)
	got, _, err := messagesToChat(src)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatal(err)
	}
	if _, ok := obj["top_k"]; ok {
		t.Fatal("top_k must be dropped")
	}
	if obj["max_tokens"] != float64(128) || obj["user"] != "u" {
		t.Fatalf("scalars=%v", obj)
	}
	stops, _ := obj["stop"].([]any)
	if len(stops) != 1 || stops[0] != "END" {
		t.Fatalf("stop=%v", obj["stop"])
	}
	if obj["parallel_tool_calls"] != false {
		t.Fatalf("parallel=%v", obj["parallel_tool_calls"])
	}
	choice := obj["tool_choice"].(map[string]any)
	fn := choice["function"].(map[string]any)
	if fn["name"] != "Bash" {
		t.Fatalf("tool_choice=%v", choice)
	}
	if tools := obj["tools"].([]any); len(tools) != 1 {
		t.Fatalf("tools=%v", tools)
	}
	if obj["reasoning_effort"] != "high" {
		t.Fatalf("effort=%v", obj["reasoning_effort"])
	}
	if _, ok := obj["thinking"]; ok {
		t.Fatal("thinking must not be forwarded")
	}
	if _, ok := obj["output_config"]; ok {
		t.Fatal("output_config must not be forwarded")
	}
	if _, ok := obj["context_management"]; ok {
		t.Fatal("context_management must not be forwarded")
	}
	msgs := obj["messages"].([]any)
	if len(msgs) != 5 {
		t.Fatalf("len=%d %#v", len(msgs), msgs)
	}
	if msgs[0].(map[string]any)["content"] != "sys-a\nsys-b" {
		t.Fatalf("system=%v", msgs[0])
	}
	user := msgs[1].(map[string]any)
	parts := user["content"].([]any)
	img := parts[1].(map[string]any)["image_url"].(map[string]any)["url"]
	if img != "data:image/png;base64,aaa" {
		t.Fatalf("image=%v", img)
	}
	asst := msgs[2].(map[string]any)
	if strings.Contains(mustJSON(asst), "secret") || strings.Contains(mustJSON(asst), "signature") {
		t.Fatalf("thinking leaked: %v", asst)
	}
	tc := asst["tool_calls"].([]any)[0].(map[string]any)
	if tc["id"] != "toolu_1" || tc["function"].(map[string]any)["arguments"] != `{"cmd":"ls"}` {
		t.Fatalf("tool_call=%v", tc)
	}
	tool := msgs[3].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "toolu_1" || tool["content"] != "a.txt" {
		t.Fatalf("tool=%v", tool)
	}
	if msgs[4].(map[string]any)["content"] != "next" {
		t.Fatalf("next=%v", msgs[4])
	}
}

func TestMessagesClientShapes(t *testing.T) {
	// Claude Code 实测顶层字段 + SDK 内置工具 + 文档块。模型名原样。
	src := []byte(`{
		"model":"claude-opus-4-6",
		"max_tokens":32000,
		"stream":true,
		"thinking":{"type":"adaptive"},
		"output_config":{"effort":"medium"},
		"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]},
		"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude.","cache_control":{"type":"ephemeral"}}],
		"tools":[
			{"name":"Bash","description":"run","input_schema":{"type":"object"},"eager_input_streaming":true},
			{"type":"bash_20250124","name":"bash"},
			{"type":"web_search_20250305","name":"web_search"}
		],
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"read "},
				{"type":"document","title":"note","source":{"type":"text","media_type":"text/plain","data":"hello"}},
				{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBE"}}
			]},
			{"role":"assistant","content":[
				{"type":"server_tool_use","id":"srv_1","name":"web_search","input":{"q":"x"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"srv_1","is_error":true,"content":[{"type":"text","text":"nope"}]}
			]}
		]
	}`)
	got, _, err := messagesToChat(src)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["model"] != "claude-opus-4-6" || obj["reasoning_effort"] != "medium" || obj["stream"] != true {
		t.Fatalf("scalars=%v", obj)
	}
	if _, ok := obj["context_management"]; ok {
		t.Fatal("context_management forwarded")
	}
	tools := obj["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["function"].(map[string]any)["name"] != "Bash" {
		t.Fatalf("tools=%v", tools)
	}
	msgs := obj["messages"].([]any)
	if len(msgs) != 5 {
		t.Fatalf("len=%d %#v", len(msgs), msgs)
	}
	if !strings.Contains(msgs[1].(map[string]any)["content"].(string), "bash") {
		t.Fatalf("note=%v", msgs[1])
	}
	// web_search 有网关侧执行器（searchtool.go）：不再出现在「不可用」说明里
	if strings.Contains(msgs[1].(map[string]any)["content"].(string), "web_search") {
		t.Fatalf("note=%v", msgs[1])
	}
	user := msgs[2].(map[string]any)["content"].([]any)
	if user[1].(map[string]any)["text"] != "note\nhello" {
		t.Fatalf("doc=%v", user[1])
	}
	if user[2].(map[string]any)["text"] != "[document application/pdf omitted]" {
		t.Fatalf("pdf=%v", user[2])
	}
	asst := msgs[3].(map[string]any)
	if asst["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["name"] != "web_search" {
		t.Fatalf("server tool=%v", asst)
	}
	if msgs[4].(map[string]any)["content"] != "error: nope" {
		t.Fatalf("result=%v", msgs[4])
	}
}

func TestEffortFromBudget(t *testing.T) {
	got, _, err := messagesToChat([]byte(`{"model":"m","thinking":{"type":"enabled","budget_tokens":16000},"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["reasoning_effort"] != "high" {
		t.Fatalf("effort=%v", obj["reasoning_effort"])
	}
	got, _, err = messagesToChat([]byte(`{"model":"m","thinking":{"type":"disabled"},"output_config":{"effort":"low"},"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["reasoning_effort"] != "low" {
		t.Fatalf("effort wins=%v", obj["reasoning_effort"])
	}
}

func TestChatCompletionToMessage(t *testing.T) {
	got := chatCompletionToMessage(map[string]any{
		"id":    "chatcmpl-1",
		"model": "glm-5.2",
		"choices": []any{map[string]any{
			"message": map[string]any{
				"role":              "assistant",
				"content":           "hi",
				"reasoning_content": "think",
				"tool_calls": []any{map[string]any{
					"id":       "call_a",
					"type":     "function",
					"function": map[string]any{"name": "Bash", "arguments": `{"cmd":"ls"}`},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 2},
	})
	if got["id"] != "msg_chatcmpl-1" || got["type"] != "message" || got["stop_reason"] != "tool_use" {
		t.Fatalf("envelope=%v", got)
	}
	content := got["content"].([]any)
	// reasoning_content 无 signature，作为 thinking 块下发展示（客户端回传时丢弃）。
	th := content[0].(map[string]any)
	if th["type"] != "thinking" || th["thinking"] != "think" {
		t.Fatalf("thinking=%v", th)
	}
	if content[1].(map[string]any)["text"] != "hi" {
		t.Fatalf("text=%v", content[1])
	}
	tu := content[2].(map[string]any)
	in := tu["input"].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "call_a" || in["cmd"] != "ls" {
		t.Fatalf("tool_use=%v", tu)
	}
	u := got["usage"].(map[string]any)
	if u["input_tokens"] != 1 || u["output_tokens"] != 2 {
		t.Fatalf("usage=%v", u)
	}
}

// TestAnthropicStopMapping 截断/内容过滤不得静默映射成 end_turn（Claude 侧语义）。
func TestAnthropicStopMapping(t *testing.T) {
	for fr, want := range map[string]string{
		"stop":           "end_turn",
		"length":         "max_tokens",
		"max_tokens":     "max_tokens",
		"tool_calls":     "tool_use",
		"function_call":  "tool_use",
		"content_filter": "refusal",
	} {
		if got := anthropicStop(fr); got != want {
			t.Fatalf("anthropicStop(%s)=%s want %s", fr, got, want)
		}
	}
}

func TestMessagesStreamEventOrder(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &messagesWriter{ResponseWriter: rec, stream: true}
	rw.Header().Set("Content-Type", "text/event-stream")
	frames := []string{
		`data: {"id":"chatcmpl-1","model":"glm-5.2","choices":[{"index":0,"delta":{"reasoning_content":"think"}}]}` + "\n\n",
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n",
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"Bash","arguments":"{\"x\":"}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}` + "\n\n",
		"data: [DONE]\n\n",
	}
	for _, f := range frames {
		if _, err := rw.Write([]byte(f)); err != nil {
			t.Fatal(err)
		}
	}
	rw.finish()
	body := rec.Body.String()
	types := sseTypes(body)
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	for _, n := range want {
		if !containsStr(types, n) {
			t.Fatalf("missing %s in %v\n%s", n, types, body)
		}
	}
	if types[0] != "message_start" || types[len(types)-1] != "message_stop" {
		t.Fatalf("order=%v", types)
	}
	if strings.Contains(body, "data: [DONE]") {
		t.Fatalf("chat [DONE] 泄漏: %s", body)
	}
	// reasoning_content 以 thinking 块下发（Claude Code 展示），且先于文本块。
	if !strings.Contains(body, `"type":"thinking_delta"`) || !strings.Contains(body, `"thinking":"think"`) {
		t.Fatalf("thinking 块 missing: %s", body)
	}
	if ti, xi := strings.Index(body, "thinking_delta"), strings.Index(body, `"type":"text_delta"`); ti < 0 || xi < 0 || ti > xi {
		t.Fatalf("thinking 应先于文本: ti=%d xi=%d", ti, xi)
	}
	if n := strings.Count(body, `"type":"content_block_stop"`); n < 2 {
		t.Fatalf("thinking/文本块都要关闭: stops=%d body=%s", n, body)
	}
	if !strings.Contains(body, `"text":"hi"`) || !strings.Contains(body, `"partial_json":"{\"x\":"`) || !strings.Contains(body, `"partial_json":"1}"`) {
		t.Fatalf("deltas missing: %s", body)
	}
	if !strings.Contains(body, `"stop_reason":"tool_use"`) {
		t.Fatalf("stop missing: %s", body)
	}
}

// TestMessagesInterleavedReasoningKeepsBlocksWellFormed 上游把 reasoning 与正文交错
// 下发（思考→正文→再思考→再正文）时，messages 侧的块必须始终是「一个开着一个」：
// 每次 content_block_start 配一次 stop、块号严格递增、thinking/text delta 各归各类。
// 这条路径与 responses 侧不同——那边整条响应只能有一个 reasoning item，这边切回思考
// 前会先把正文块停掉（ensureThinking），所以交替是可表达的；怕的是将来改成"不关块"
// 或复用块号：Claude Code 按 index 收块，两个块同时开着就会串。
func TestMessagesInterleavedReasoningKeepsBlocksWellFormed(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &messagesWriter{ResponseWriter: rec, stream: true}
	rw.Header().Set("Content-Type", "text/event-stream")
	for _, rc := range []string{"A", "B"} {
		frames := []string{
			`data: {"id":"c","choices":[{"index":0,"delta":{"reasoning_content":"` + rc + `"}}]}` + "\n\n",
			`data: {"id":"c","choices":[{"index":0,"delta":{"content":"x` + rc + `"}}]}` + "\n\n",
		}
		for _, f := range frames {
			if _, err := rw.Write([]byte(f)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := rw.Write([]byte("data: [DONE]\n\n")); err != nil {
		t.Fatal(err)
	}
	rw.finish()
	body := rec.Body.String()

	var open = map[int]string{} // index → block type
	var starts, stops int
	var lastIdx = -1
	maxOpen := 0
	for _, blk := range strings.Split(body, "\n\n") {
		ev := ""
		var obj map[string]any
		for _, l := range strings.Split(blk, "\n") {
			if strings.HasPrefix(l, "event: ") {
				ev = strings.TrimPrefix(l, "event: ")
			}
			if strings.HasPrefix(l, "data: ") {
				_ = json.Unmarshal([]byte(strings.TrimPrefix(l, "data: ")), &obj)
			}
		}
		if obj == nil {
			continue
		}
		idx := -1
		if v, ok := obj["index"].(float64); ok {
			idx = int(v)
		}
		switch ev {
		case "content_block_start":
			starts++
			if idx <= lastIdx {
				t.Fatalf("块号不递增：%d 接在 %d 之后\n%s", idx, lastIdx, body)
			}
			lastIdx = idx
			cb, _ := obj["content_block"].(map[string]any)
			open[idx] = asString(cb["type"])
		case "content_block_stop":
			stops++
			if _, ok := open[idx]; !ok {
				t.Fatalf("stop 了没开的块 index=%d\n%s", idx, body)
			}
			delete(open, idx)
		case "content_block_delta":
			d, _ := obj["delta"].(map[string]any)
			dt := asString(d["type"])
			bt := open[idx]
			if (dt == "thinking_delta") != (bt == "thinking") || (dt == "text_delta") != (bt == "text") {
				t.Fatalf("delta 落错块：delta=%s block=%s index=%d\n%s", dt, bt, idx, body)
			}
		}
		if len(open) > maxOpen {
			maxOpen = len(open)
		}
	}
	if starts != stops || len(open) != 0 {
		t.Fatalf("块未配平：start=%d stop=%d 未关=%v\n%s", starts, stops, open, body)
	}
	if maxOpen != 1 {
		t.Fatalf("同时开着的块数 = %d，应为 1\n%s", maxOpen, body)
	}
	if starts != 4 {
		t.Fatalf("两段思考+两段正文应开 4 个块，实际 %d\n%s", starts, body)
	}
	if strings.Count(body, `"type":"message_stop"`) != 1 {
		t.Fatalf("message_stop 应恰好一个\n%s", body)
	}
}

func TestMessagesEndpoint(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/messages"} {
		up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
		p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
		h := NewHandler(Config{Pool: p, Upstream: up})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", path, strings.NewReader(`{"model":"glm-5.2","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stream":false}`))
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s code=%d body=%s", path, rec.Code, rec.Body.Bytes())
		}
		var obj map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
			t.Fatal(err)
		}
		if obj["type"] != "message" || obj["stop_reason"] != "end_turn" {
			t.Fatalf("%s got=%v", path, obj)
		}
		text := obj["content"].([]any)[0].(map[string]any)["text"]
		if text != "你好" {
			t.Fatalf("%s text=%v", path, text)
		}
	}
}

func TestMessagesEndpointStream(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"glm-5.2","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stream":true}`))
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.Bytes())
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "event-stream") {
		t.Fatalf("ct=%s", rec.Header().Get("Content-Type"))
	}
	types := sseTypes(rec.Body.String())
	if len(types) == 0 || types[0] != "message_start" || types[len(types)-1] != "message_stop" {
		t.Fatalf("types=%v body=%s", types, rec.Body.String())
	}
	if !containsStr(types, "content_block_delta") {
		t.Fatalf("no text delta in %v", types)
	}
}

func TestMessagesEndpointStreamErrorBeforeSSE(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: &upstream.Client{}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"glm-5.2","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stream":true}`))
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d want 200 SSE", rec.Code)
	}
	types := sseTypes(rec.Body.String())
	if !containsStr(types, "error") {
		t.Fatalf("types=%v body=%s", types, rec.Body.String())
	}
	if containsStr(types, "message_stop") {
		t.Fatalf("failed stream must not stop cleanly: %v", types)
	}
}

func TestMessagesAuthXAPIKey(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "secret"})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing key code=%d", rec.Code)
	}
	var obj map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
		t.Fatal(err)
	}
	if obj["type"] != "error" || obj["error"].(map[string]any)["type"] != "authentication_error" {
		t.Fatalf("auth body=%v", obj)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", "secret")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("x-api-key code=%d body=%s", rec.Code, rec.Body.Bytes())
	}
}

func TestMessagesInvalidJSON(t *testing.T) {
	_, _, err := messagesToChat([]byte(`{`))
	if err == nil {
		t.Fatal("want error")
	}
	h := NewHandler(Config{Pool: pool.New(""), Upstream: &upstream.Client{}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", bytes.NewReader([]byte(`{`))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
	var obj map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
		t.Fatal(err)
	}
	if obj["type"] != "error" {
		t.Fatalf("body=%v", obj)
	}
}

func TestMessagesEndpointNonStreamError(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: &upstream.Client{}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/messages", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.Bytes())
	}
	var obj map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
		t.Fatalf("body=%s err=%v", rec.Body.Bytes(), err)
	}
	errObj, _ := obj["error"].(map[string]any)
	if obj["type"] != "error" || errObj["type"] == "" || errObj["message"] == "" {
		t.Fatalf("code=%d body=%v", rec.Code, obj)
	}
}

// Anthropic 入站：tool_result 里的图片不能留在 tool 消息里，也不能继续拼进正文 ——
// 拼进去等于把 base64 当正文分词（Claude Code 读截图、MCP 的截图类工具都把图放在
// tool_result.content）：模型其实看不到图，token 却按字符数涨。图片另起一条 user
// 消息，且要等这批工具结果全部落地（插在 assistant.tool_calls 与 tool 之间判 11148）。
func TestAnthropicToolResultImagesMoveToUserMessage(t *testing.T) {
	msgsFor := func(messages string) []any {
		t.Helper()
		got, _, err := messagesToChat([]byte(`{"model":"m","max_tokens":16,"messages":` + messages + `}`))
		if err != nil {
			t.Fatal(err)
		}
		var obj map[string]any
		if err := json.Unmarshal(got, &obj); err != nil {
			t.Fatal(err)
		}
		msgs, _ := obj["messages"].([]any)
		return msgs
	}
	img := `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}`

	m := msgsFor(`[
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"shot","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[
			{"type":"text","text":"done"},` + img + `]}]},
		{"role":"assistant","content":[{"type":"text","text":"next"}]}]`)
	var roles []string
	for _, x := range m {
		roles = append(roles, asString(x.(map[string]any)["role"]))
	}
	if want := "assistant,tool,user,assistant"; strings.Join(roles, ",") != want {
		t.Fatalf("roles=%v want=%s", roles, want)
	}
	if c := asString(m[1].(map[string]any)["content"]); c != "done" || strings.Contains(c, "base64") {
		t.Fatalf("tool content 不该带图/不该焊 base64: %q", c)
	}
	parts, _ := m[2].(map[string]any)["content"].([]any)
	if len(parts) != 2 || asString(parts[0].(map[string]any)["text"]) != toolImagePlaceholder {
		t.Fatalf("user msg=%v", m[2])
	}
	iu, _ := parts[1].(map[string]any)["image_url"].(map[string]any)
	if asString(iu["url"]) != "data:image/png;base64,QUJD" {
		t.Fatalf("image part=%v", parts[1])
	}

	// 只有图、且 is_error：不能焊成 "error: data:image…"
	m = msgsFor(`[
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"shot","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":[` + img + `]}]}]`)
	if c := asString(m[1].(map[string]any)["content"]); c != "error: " {
		t.Fatalf("tool content=%q", c)
	}
	if len(m) != 3 || asString(m[2].(map[string]any)["role"]) != "user" {
		t.Fatalf("msgs=%#v", m)
	}
}

// TestMessagesEndpointRecordedInRequestLog /v1/messages 与 /v1/chat/completions 同口径
// 进请求归档，且事件里的 Path 是入站协议路径而不是写死的 chat 端点 —— 否则面板的
// 请求记录里整类 Anthropic 请求要么不出现，要么全被标成 chat。
func TestMessagesEndpointRecordedInRequestLog(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/messages"} {
		t.Run(path, func(t *testing.T) {
			up := newFakeUpstream(t, func(string) (int, string, bool) {
				return 200, sseOK, true
			})
			reqLog := reqlog.New(reqlog.Config{})
			h := NewHandler(Config{
				Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
				Upstream:   up,
				RequestLog: reqLog,
			})
			body := `{"model":"glm-5.2","max_tokens":16,"stream":true,` +
				`"messages":[{"role":"user","content":"hi"}]}`
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader(body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
			}
			s := reqLog.Snapshot()
			if len(s.Recent) != 1 {
				t.Fatalf("请求未进归档: %+v", s.Recent)
			}
			e := s.Recent[0]
			if e.Path != path {
				t.Errorf("Path=%q want %q", e.Path, path)
			}
			if e.Model != "glm-5.2" || !e.OK {
				t.Errorf("event = %+v", e)
			}
		})
	}
}

// TestCountTokensEndpoint count_tokens 是本地估算端点（上游没有计数接口）：
// 只要求形状对、量级合理，且**图片不能被按字符数算** —— 否则一张截图就能把估算
// 顶到几十万，Claude Code 会误判上下文已满、反复压缩。
func TestCountTokensEndpoint(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith()})
	bigB64 := strings.Repeat("A", 200000) // 20 万字符的 base64 ≈ 一张截图
	body := `{"model":"glm-5.2","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"你好世界"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + bigB64 + `"}}]}]}`
	for _, path := range []string{"/v1/messages/count_tokens", "/messages/count_tokens"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s code=%d body=%s", path, rec.Code, rec.Body.String())
		}
		var got struct {
			InputTokens int `json:"input_tokens"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("%s 响应不是 JSON: %s", path, rec.Body.String())
		}
		// 4 个汉字 ≈ 4 token + 图片固定 1600 + model 名等零头，远小于 base64 长度。
		if got.InputTokens <= 0 {
			t.Fatalf("%s input_tokens=%d 应为正", path, got.InputTokens)
		}
		if got.InputTokens > 2000 {
			t.Fatalf("%s input_tokens=%d 把 base64 按字符算了（应固定 1600/张）", path, got.InputTokens)
		}
	}
}
