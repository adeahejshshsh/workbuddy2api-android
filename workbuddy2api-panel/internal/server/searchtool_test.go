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

// searchToolFake 按路径分流：chat 端点第一轮回「模型要检索」，之后回正常答案；
// agenttool 端点回检索结果。记录每次 chat 的请求体与检索词。
type searchToolFake struct {
	chatCalls  int
	chatBodies []string
	searchQ    []string
	searchFail bool
}

func (f *searchToolFake) client() *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			if strings.HasPrefix(r.URL.Path, "/agenttool/v1/search") {
				var q struct {
					Query string `json:"query"`
				}
				_ = json.Unmarshal(raw, &q)
				f.searchQ = append(f.searchQ, q.Query)
				if f.searchFail {
					return &http.Response{StatusCode: 500, Header: jsonHdr(),
						Body: io.NopCloser(strings.NewReader(`{"code":500,"msg":"boom"}`))}, nil
				}
				return &http.Response{StatusCode: 200, Header: jsonHdr(),
					Body: io.NopCloser(strings.NewReader(`{"query":"玉林 电梯 维保","type":"text2text",` +
						`"provider":"0","results":[{"title":"广西电梯维保信用评价办法",` +
						`"url":"http://scjdglj.gxzf.example/1.shtml","snippet":"每年一次年度综合检查"}],` +
						`"total_results":5,"response_time_ms":631}`))}, nil
			}
			f.chatCalls++
			f.chatBodies = append(f.chatBodies, string(raw))
			if f.chatCalls == 1 {
				return &http.Response{StatusCode: 200, Header: sseHdr(),
					Body: io.NopCloser(strings.NewReader(webSearchCallStream()))}, nil
			}
			return &http.Response{StatusCode: 200, Header: sseHdr(),
				Body: io.NopCloser(strings.NewReader(sseOK))}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

func jsonHdr() http.Header {
	return http.Header{"Content-Type": []string{"application/json"}}
}

func sseHdr() http.Header {
	return http.Header{"Content-Type": []string{"text/event-stream"}}
}

// webSearchCallStream 首轮：模型发起 web_search(query=玉林 电梯 维保)。
func webSearchCallStream() string {
	return `data: {"id":"cm1","object":"chat.completion.chunk","created":1,"model":"glm-5.2",` +
		`"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1",` +
		`"type":"function","function":{"name":"web_search","arguments":"{\"query\":\"玉林 电梯 维保\"}"}}]}}]}` + "\n\n" +
		`data: {"id":"cm1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"
}

func messagesBodyWithTools(tools string) string {
	return `{"model":"glm-5.2","max_tokens":1024,"stream":true,"tools":[` + tools + `],` +
		`"messages":[{"role":"user","content":"玉林的电梯维保新规是什么？"}]}`
}

func newSearchToolHandler(f *searchToolFake) *Handler {
	p := testPoolWith(&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	return NewHandler(Config{Pool: p, Upstream: f.client()})
}

// TestMessagesWebSearchToolRuns 声明 web_search 的 /v1/messages：
// 一次探测（模型写检索词）→ 一次检索 → 结果注入最终调用 → 正常 Anthropic 流。
func TestMessagesWebSearchToolRuns(t *testing.T) {
	f := &searchToolFake{}
	h := newSearchToolHandler(f)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		messagesBodyWithTools(`{"type":"web_search_20250305","name":"web_search"}`))))

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	// ① 三轮：探测（模型要检索）→ 探测（看到结果后作答，不再要检索）→ 最终流式答案
	if f.chatCalls != 3 {
		t.Fatalf("chat 调用 %d 次，want 3（探测×2 + 最终答案）", f.chatCalls)
	}
	// ② 探测轮带上了 web_search 的 function 声明（模型才可能发起检索）
	var probe map[string]any
	_ = decodeJSON(f.chatBodies[0], &probe)
	if !bodyHasTool(probe, "web_search") {
		t.Fatalf("探测轮缺 web_search 工具声明: %v", probe["tools"])
	}
	// ③ 检索真的发生了，且用的是模型写的检索词（不是用户原话）
	if len(f.searchQ) != 1 || f.searchQ[0] != "玉林 电梯 维保" {
		t.Fatalf("检索词 %v", f.searchQ)
	}
	// ④ 第二轮探测带上了检索结果（模型据此决定还要不要再搜 = 多跳）
	var probe2 map[string]any
	_ = decodeJSON(f.chatBodies[1], &probe2)
	if !strings.Contains(lastMessage(probe2), "http://scjdglj.gxzf.example/1.shtml") {
		t.Fatalf("第二轮探测未回填检索结果: %v", probe2["messages"])
	}
	// ⑤ 最终调用注入检索结果（URL 必须在：模型靠它引用来源）
	var final map[string]any
	_ = decodeJSON(f.chatBodies[len(f.chatBodies)-1], &final)
	last := lastMessage(final)
	if !strings.Contains(last, "http://scjdglj.gxzf.example/1.shtml") || !strings.Contains(last, "联网检索结果") {
		t.Fatalf("最终调用未注入检索结果: %v", last)
	}
	// ⑥ 最终调用不得再带 web_search 声明——否则模型可能回一个服务端 tool_use，
	//    而客户端（Claude Code）会把它当自己的工具去执行。
	if bodyHasTool(final, "web_search") {
		t.Fatalf("最终调用仍带 web_search 声明: %v", final["tools"])
	}
	// ⑦ 客户端看到正常 Anthropic 流；检索过程以 server_tool_use + web_search_tool_result 回报
	blocks, body := sseBlocks(t, rec.Body.String())
	if !strings.Contains(body, "message_stop") {
		t.Fatalf("非正常 Anthropic 流: %s", body)
	}
	var use, res map[string]any
	for _, b := range blocks {
		switch asString(b["type"]) {
		case "server_tool_use":
			use = b
		case "web_search_tool_result":
			res = b
		}
	}
	if use == nil || res == nil {
		t.Fatalf("缺服务端工具块（%v）: %s", blocks, body)
	}
	if asString(use["name"]) != "web_search" || asString(use["id"]) != asString(res["tool_use_id"]) {
		t.Fatalf("server_tool_use 与结果块未配对: %v / %v", use, res)
	}
	if !strings.Contains(body, "http://scjdglj.gxzf.example/1.shtml") {
		t.Fatal("结果块里没有来源 URL")
	}
	// 服务端工具**不得**降级成客户端可执行的 tool_use（Claude Code 会去找执行器执行它）
	if strings.Contains(body, `"type":"tool_use"`) {
		t.Fatalf("服务端工具被当成客户端工具下发: %s", body)
	}
	// 每个结果项都得是**协议完整**的 web_search_result（缺字段=客户端解析整块失败）
	assertWebSearchResultFields(t, res)
}

// assertWebSearchResultFields 校验结果块里每一项都带齐官方要求的字段。
// 回归位：encrypted_content 曾漏发，官方 SDK 的 WebSearchResultBlock 把它定为
// 必填 string，客户端解析阶段直接判非法 → WebSearch 整次调用报
// "Model request failed."。page_age 在客户端 schema 里是 nullish（可选可空），
// 所以只断言「若在，必须是 string 或 null」，不要求必须出现。
func assertWebSearchResultFields(t *testing.T, res map[string]any) {
	t.Helper()
	items, ok := res["content"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("结果块没有 content 数组: %v", res)
	}
	for i, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			t.Fatalf("第 %d 项不是对象: %v", i, it)
		}
		if asString(m["type"]) != "web_search_result" {
			t.Fatalf("第 %d 项 type=%v", i, m["type"])
		}
		if asString(m["url"]) == "" || asString(m["title"]) == "" {
			t.Fatalf("第 %d 项缺 url/title: %v", i, m)
		}
		// encrypted_content 是必填 string（官方 SDK 类型无 ?），空串合法但键不能省。
		if v, ok := m["encrypted_content"]; !ok {
			t.Fatalf("第 %d 项缺 encrypted_content（官方 schema 必填，缺了客户端解析整块失败）: %v", i, m)
		} else if _, ok := v.(string); !ok {
			t.Fatalf("第 %d 项 encrypted_content 不是 string: %#v", i, v)
		}
		// page_age 可空：键可省；若在，值只能是 string 或 null。
		if v, ok := m["page_age"]; ok && v != nil {
			if _, ok := v.(string); !ok {
				t.Fatalf("第 %d 项 page_age 既不是 string 也不是 null: %#v", i, v)
			}
		}
	}
}

// sseBlocks 解析 Anthropic SSE 里的 content_block_start 块与全文（顺序即下标顺序）。
func sseBlocks(t *testing.T, body string) ([]map[string]any, string) {
	t.Helper()
	var blocks []map[string]any
	for _, ev := range strings.Split(body, "\n\n") {
		payload, ok := strings.CutPrefix(strings.TrimSpace(ev), "event: content_block_start")
		if !ok {
			continue
		}
		i := strings.Index(payload, "data: ")
		if i < 0 {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(payload[i+6:]), &obj); err != nil {
			t.Fatalf("解析 content_block_start 失败: %v (%s)", err, payload)
		}
		if b, ok := obj["content_block"].(map[string]any); ok {
			blocks = append(blocks, b)
		}
	}
	return blocks, body
}

// TestMessagesWebSearchBlocksNonStream 非流式：服务端工具块排在正文之前，且都是完整块。
func TestMessagesWebSearchBlocksNonStream(t *testing.T) {
	f := &searchToolFake{}
	h := newSearchToolHandler(f)
	body := `{"model":"glm-5.2","max_tokens":1024,"stream":false,` +
		`"tools":[{"type":"web_search_20250305","name":"web_search"}],` +
		`"messages":[{"role":"user","content":"玉林的电梯维保新规是什么？"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var msg struct {
		Content []map[string]any `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("非流式响应不是 JSON: %v (%s)", err, rec.Body.String())
	}
	if len(msg.Content) < 3 {
		t.Fatalf("块数不足（want 服务端工具对 + 正文）: %s", rec.Body.String())
	}
	if asString(msg.Content[0]["type"]) != "server_tool_use" || asString(msg.Content[1]["type"]) != "web_search_tool_result" {
		t.Fatalf("服务端工具块未排在正文之前: %v", msg.Content)
	}
	if asString(msg.Content[2]["type"]) != "text" {
		t.Fatalf("正文块位置不符: %v", msg.Content[2])
	}
	assertWebSearchResultFields(t, msg.Content[1])
}

// TestMessagesWithoutWebSearchToolSkipsProbe 没声明服务端工具时零额外开销：
// 只打一次上游、不碰检索端点、不动 messages。
func TestMessagesWithoutWebSearchToolSkipsProbe(t *testing.T) {
	f := &searchToolFake{}
	h := newSearchToolHandler(f)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		messagesBodyWithTools(`{"name":"Bash","input_schema":{"type":"object"}}`))))

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if f.chatCalls != 1 {
		t.Fatalf("chat 调用 %d 次，want 1（不得探测）", f.chatCalls)
	}
	if len(f.searchQ) != 0 {
		t.Fatalf("不该发起检索: %v", f.searchQ)
	}
	if strings.Contains(f.chatBodies[0], "联网检索结果") {
		t.Fatal("未声明服务端工具却注入了检索结果")
	}
}

// TestMessagesWebSearchToolFailsLoudly 检索失败：请求照常成功（联网是加分项），
// 但要把「检索失败」如实告诉模型——不能静默当成「网上没有」，那是编造的温床。
func TestMessagesWebSearchToolFailsLoudly(t *testing.T) {
	f := &searchToolFake{searchFail: true}
	h := newSearchToolHandler(f)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		messagesBodyWithTools(`{"type":"web_search_20250305","name":"web_search"}`))))

	if rec.Code != 200 {
		t.Fatalf("检索失败不应让请求失败: code=%d body=%s", rec.Code, rec.Body.String())
	}
	last := f.chatBodies[len(f.chatBodies)-1]
	if !strings.Contains(last, "玉林的电梯维保新规") {
		t.Fatal("降级后仍应带原始用户消息")
	}
	if !strings.Contains(last, "失败") {
		t.Fatalf("检索失败必须显形给模型: %s", last)
	}
	if strings.Contains(last, "http://scjdglj") {
		t.Fatal("检索失败却带上了并没有取到的结果")
	}
}

// TestAnthropicToolsServerToolRouting web_search 进 srv（可执行）、其它服务端工具仍进 skipped。
func TestAnthropicToolsServerToolRouting(t *testing.T) {
	out, skipped, srv := anthropicTools([]any{
		map[string]any{"type": "web_search_20250305", "name": "web_search", "max_uses": float64(4)},
		map[string]any{"type": "bash_20250124", "name": "bash"},
		map[string]any{"name": "Bash", "input_schema": map[string]any{"type": "object"}},
		map[string]any{"type": "web_search_20250305"}, // 无名：无法构造声明，当不可执行
	})
	if len(srv) != 1 || srv[0].Name != "web_search" || srv[0].MaxUses != 4 {
		t.Fatalf("srv=%v", srv)
	}
	if strings.Join(skipped, ",") != "bash,web_search_20250305" {
		t.Fatalf("skipped=%v", skipped)
	}
	if len(out) != 1 || !bodyHasTool(map[string]any{"tools": out}, "Bash") {
		t.Fatalf("out=%v", out)
	}
}

func decodeJSON(s string, v any) error {
	return json.Unmarshal([]byte(s), v)
}

func bodyHasTool(obj map[string]any, name string) bool {
	tools, _ := obj["tools"].([]any)
	for _, t := range tools {
		m, _ := t.(map[string]any)
		if fn, ok := m["function"].(map[string]any); ok && asString(fn["name"]) == name {
			return true
		}
		if asString(m["name"]) == name {
			return true
		}
	}
	return false
}

func lastMessage(obj map[string]any) string {
	msgs, _ := obj["messages"].([]any)
	if len(msgs) == 0 {
		return ""
	}
	m, _ := msgs[len(msgs)-1].(map[string]any)
	return asString(m["content"])
}

// TestResponsesHostedWebSearchExecutes Codex 的托管联网：声明 {"type":"web_search"}
// → 网关侧执行检索（用模型自己写的检索词）→ 结果注入 → 正常 Responses 流。
func TestResponsesHostedWebSearchExecutes(t *testing.T) {
	f := &searchToolFake{}
	h := newSearchToolHandler(f)
	body := `{"model":"glm-5.2","stream":true,"input":"玉林的电梯维保新规是什么？",` +
		`"tools":[{"type":"web_search"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if f.chatCalls != 3 {
		t.Fatalf("chat 调用 %d 次，want 3（探测×2 + 最终答案）", f.chatCalls)
	}
	if len(f.searchQ) != 1 || f.searchQ[0] != "玉林 电梯 维保" {
		t.Fatalf("检索词 %v（应为模型写的词）", f.searchQ)
	}
	// 托管工具不得原样留在上游 tools 里：上游收下不报错但也不会执行（空转）。
	var final map[string]any
	_ = decodeJSON(f.chatBodies[len(f.chatBodies)-1], &final)
	if bodyHasTool(final, "web_search") {
		t.Fatalf("托管 web_search 仍被透传给上游: %v", final["tools"])
	}
	if !strings.Contains(lastMessage(final), "http://scjdglj.gxzf.example/1.shtml") {
		t.Fatalf("未注入检索结果: %v", lastMessage(final))
	}
	if !strings.Contains(rec.Body.String(), "response.completed") {
		t.Fatalf("非正常 Responses 流: %s", rec.Body.String())
	}
}

// TestResponsesWithoutHostedSearchSkipsProbe 没声明托管搜索时零额外开销（含非托管工具）。
func TestResponsesWithoutHostedSearchSkipsProbe(t *testing.T) {
	f := &searchToolFake{}
	h := newSearchToolHandler(f)
	body := `{"model":"glm-5.2","stream":true,"input":"hi","tools":[{"type":"function",` +
		`"name":"f","parameters":{"type":"object"}}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if f.chatCalls != 1 || len(f.searchQ) != 0 {
		t.Fatalf("不该探测/检索: chatCalls=%d searchQ=%v", f.chatCalls, f.searchQ)
	}
}

// TestSplitHostedSearch 只摘托管检索（无 name 的 web_search*），其余原样留下。
func TestSplitHostedSearch(t *testing.T) {
	kept, srv := splitHostedSearch([]any{
		map[string]any{"type": "web_search"},
		map[string]any{"type": "web_search_preview", "max_uses": float64(2)},
		map[string]any{"type": "web_search", "name": "web_search"}, // 带 name：客户端自己的函数工具
		map[string]any{"type": "code_interpreter"},
		map[string]any{"type": "function", "name": "f"},
	})
	if len(srv) != 2 || srv[0].Name != webSearchToolName || srv[0].MaxUses != 0 || srv[1].MaxUses != 2 {
		t.Fatalf("srv=%v", srv)
	}
	if len(kept) != 3 {
		t.Fatalf("kept=%v", kept)
	}
	if _, srv2 := splitHostedSearch([]any{map[string]any{"type": "function", "name": "f"}}); len(srv2) != 0 {
		t.Fatal("普通 function 工具被误摘")
	}
}
