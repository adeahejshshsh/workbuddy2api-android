// searchtool.go /v1/messages 的 web_search 服务端工具执行（上游 issue #195 的 B 形态）。
//
// Anthropic 的 web_search 是**服务端工具**：客户端（Claude Code 等）只声明
// {"type":"web_search_20250305","name":"web_search"}，检索由服务方执行、结果以
// server_tool_use / web_search_tool_result 块回报。本网关此前把它与 code_execution /
// bash_* 一起丢掉并在 system 里写「不可用」；这里补上执行器，分三步：
//
//  1. 探测：把 web_search 当普通 function 工具加进请求，非流式聚合
//     （upstream.Aggregate）——让**模型自己写检索词**，比拿用户原话去搜准得多；
//  2. 执行：命中 tool_call 就调官方 /agenttool/v1/search（见 upstream/search.go），
//     取标题/URL/摘要（**URL 是让模型能引用的关键**）；
//  3. 出答案：把检索结果作为一条 user 消息追加到原始 messages 尾部，再走既有
//     chatCompletions 流式管道——客户端拿到的仍是正常 Anthropic SSE 流，
//     流式机器一行未改，也不存在把服务端工具的 tool_use 泄漏给客户端的可能。
//
// 失败一律降级：探测/检索不可用时按普通对话继续（返回 nil 表示不注入），联网是加分项，
// 不该让整轮请求失败；同时打 WARN 让面板日志能看到。
//
// shortcut: 探测轮与最终答案各占一次上游调用（模型没打算搜索时白多一次）；
// 不向客户端回发 server_tool_use/web_search_tool_result 块（搜索过程对客户端不可见）。
// 升级路径是「流里嗅探 tool_call → 原地续跑」，代价要动流式机器。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const (
	// webSearchToolName 官方服务端工具的固定名（客户端声明里也用它）。
	webSearchToolName = "web_search"
	// webSearchMaxUses 客户端未给 max_uses 时的检索次数上限（延迟优先：每轮都是一次
	// 完整上游调用 + 一次检索，Claude Code 自己也会给 max_uses）。
	webSearchMaxUses = 3
	// webSearchPickTries 探测/检索的取号重试次数（尽力而为，不参与 chat 的失败处置链）。
	webSearchPickTries = 3
)

// serverSearchTool 客户端声明且本网关有执行器的服务端工具。
type serverSearchTool struct {
	Name    string
	MaxUses int
}

// webSearchFunction 服务端工具声明 → 上游认的 function 工具（模型据此写检索词）。
func webSearchFunction(name string) map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{
		"name": name,
		"description": "Search the public web for up-to-date information. " +
			"Returns web pages with title, url and snippet. Use it when the answer " +
			"depends on recent or external facts.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "Search query"},
			},
			"required": []string{"query"},
		},
	}}
}

// searchAugment 探测→检索→注入。返回 nil body 表示没有可注入的内容（探测失败、模型没用
// 搜索、检索全部失败），调用方按普通对话继续；prelude 是给 Anthropic 客户端回报的
// server_tool_use / web_search_tool_result 块（Responses 侧暂不合成等价 output item）。
func (h *Handler) searchAugment(ctx context.Context, chat []byte, tool serverSearchTool) ([]byte, []any) {
	var obj map[string]any
	if err := json.Unmarshal(chat, &obj); err != nil {
		return nil, nil
	}
	msgs, _ := obj["messages"].([]any)
	if len(msgs) == 0 {
		return nil, nil
	}
	model, _ := obj["model"].(string)
	// realm 前缀是网关侧路由协议：出站与选号一律用裸模型名（与 chat 路径同一口径，
	// 否则上游对带前缀的名字回 11102「model ... service info not found」）。
	realm, bare := resolveModel(model)
	if realm != "global" {
		realm = "cn"
	}
	limit := tool.MaxUses
	if limit <= 0 || limit > webSearchMaxUses {
		limit = webSearchMaxUses
	}
	tools, _ := obj["tools"].([]any)
	probeTools := append(append([]any{}, tools...), webSearchFunction(tool.Name))
	probeMsgs := append([]any{}, msgs...)

	var items []string
	var done []searchDone
	for uses := 0; uses < limit; {
		probe := map[string]any{"model": bare, "messages": probeMsgs, "tools": probeTools, "stream": false}
		for _, k := range []string{"temperature", "top_p", "max_tokens", "reasoning_effort", "tool_choice"} {
			if v, ok := obj[k]; ok {
				probe[k] = v
			}
		}
		pb, err := json.Marshal(probe)
		if err != nil {
			return nil, nil
		}
		msg, err := h.probeOnce(ctx, realm, bare, pb)
		if err != nil {
			log.Printf("WARN: [messages] web_search 探测失败，降级为普通对话: %v", err)
			return nil, nil
		}
		calls := webSearchCalls(msg, tool.Name)
		if len(calls) == 0 {
			break // 模型自己决定不搜了
		}
		probeMsgs = append(probeMsgs, msg) // 探测轮自己的助手消息（含 tool_calls）回填
		for _, c := range calls {
			if uses >= limit {
				break
			}
			uses++
			text, results, ok := h.searchText(ctx, realm, bare, c.Query)
			if !ok {
				// 检索失败：把失败如实告诉模型，别让它以为「网上没有」
				text = "检索「" + c.Query + "」失败（上游检索接口不可用）。"
			}
			items = append(items, text)
			if ok {
				done = append(done, searchDone{Query: c.Query, Results: results})
			}
			probeMsgs = append(probeMsgs, map[string]any{
				"role": "tool", "tool_call_id": c.ID, "content": text,
			})
		}
	}
	if len(items) == 0 {
		return nil, nil
	}
	out := map[string]any{}
	for k, v := range obj {
		out[k] = v
	}
	out["messages"] = append(append([]any{}, msgs...), map[string]any{
		"role":    "user",
		"content": webSearchContext(items),
	})
	b, err := json.Marshal(out)
	if err != nil {
		return nil, nil
	}
	return b, searchPrelude(tool.Name, done)
}

// searchDone 一次成功的检索（回报给客户端用）。
type searchDone struct {
	Query   string
	Results []upstream.SearchResult
}

// searchPrelude 组装 Anthropic 的服务端工具块：每次检索一对
// server_tool_use（模型发起）+ web_search_tool_result（网关执行结果）。
// 只在真检索成功时回报——失败的检索没有结果块可发（客户端可见的失败仍走文本）。
//
// web_search_result 的字段必须补齐（见 webSearchResult）：官方响应里
// encrypted_content 是**必填字符串**（官方 SDK 的 WebSearchResultBlock 把它写成
// encrypted_content: string，无 ?），page_age 是**可空**（string | null）。
// 少给 encrypted_content，整条结果块在客户端解析阶段就被判非法，表现为
// 「联网搜索这次调用直接失败」——比缺内容严重得多。
func searchPrelude(name string, done []searchDone) []any {
	if len(done) == 0 {
		return nil
	}
	out := make([]any, 0, 2*len(done))
	for _, d := range done {
		id := "srvtoolu_" + session.NewMessageID()
		out = append(out, map[string]any{
			"type": "server_tool_use", "id": id, "name": name,
			"input": map[string]any{"query": d.Query},
		})
		content := make([]any, 0, len(d.Results))
		for _, r := range d.Results {
			content = append(content, webSearchResult(r))
		}
		out = append(out, map[string]any{
			"type": "web_search_tool_result", "tool_use_id": id, "content": content,
		})
	}
	return out
}

// searchCall 模型要的一次检索。
type searchCall struct{ ID, Query string }

// webSearchResult 单条检索结果 → Anthropic 的 web_search_result 块。
//
// 字段纪律（照官方 /v1/messages + web_search 的响应实况，也与官方 SDK 的
// WebSearchResultBlock 一一对应）：
//   - type/url/title：原样带；
//   - encrypted_content：**必填 string**（SDK 类型无 ?），官方拿它做多轮引用回传
//     校验（官方文档：缺了或被改过，把结果回传的后续请求会 400）。网关的检索结果
//     来自官方 agenttool 明文，没有可回传的密文，故给空串——「不加密的空内容」
//     在协议上是合法值。**不能省略**：客户端 schema 是 required，缺字段会让
//     整条 web_search_tool_result 块判非法、本次联网调用直接失败；
//   - page_age：可空（SDK 类型是 string | null，客户端 schema 为 nullish）。上游
//     agenttool 不提供该信息，故显式给 null —— 与官方对「未知更新时间」的回法一致，
//     也让出站块形状固定、便于排查。
func webSearchResult(r upstream.SearchResult) map[string]any {
	return map[string]any{
		"type":              "web_search_result",
		"url":               r.URL,
		"title":             r.Title,
		"encrypted_content": "",
		"page_age":          nil,
	}
}

// webSearchCalls 取出本轮里对 name 工具的调用（参数非法/无 query 的丢弃）。
func webSearchCalls(msg map[string]any, name string) []searchCall {
	var out []searchCall
	for _, t := range anySlice(msg["tool_calls"]) {
		m, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := m["function"].(map[string]any)
		if asString(fn["name"]) != name {
			continue
		}
		var args struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal([]byte(asString(fn["arguments"])), &args); err != nil {
			continue
		}
		if q := strings.TrimSpace(args.Query); q != "" {
			out = append(out, searchCall{ID: asString(m["id"]), Query: q})
		}
	}
	return out
}

// anySlice 归一上游给的数组形态：Aggregate 的 tool_calls 是 []map[string]any
// （直接构造），而经 JSON 往返的是 []any——两种都要能遍历。
func anySlice(v any) []any {
	switch s := v.(type) {
	case []any:
		return s
	case []map[string]any:
		out := make([]any, 0, len(s))
		for _, m := range s {
			out = append(out, m)
		}
		return out
	}
	return nil
}

// probeOnce 一次非流式探测：上游只认流式，故按 SSE 聚合成整条响应后取
// choices[0].message（与 /v1/chat/completions 的 stream:false 同径）。
func (h *Handler) probeOnce(ctx context.Context, realm, model string, body []byte) (map[string]any, error) {
	tried := map[string]bool{}
	var lastErr error
	for i := 0; i < webSearchPickTries; i++ {
		a := h.cfg.Pool.PickExcludingForRealm(tried, model, realm)
		if a == nil {
			break
		}
		tried[a.UID] = true
		// AgentType=auxiliary：web_search 工具的内部探测调用（非用户主对话）。
		rc, _, _, err := h.cfg.Upstream.ChatStreamContext(ctx, a, body, "", upstream.ChatMeta{AgentType: upstream.AgentTypeAuxiliary})
		if err != nil {
			lastErr = err
			var ue *upstream.Error
			if errors.As(err, &ue) {
				h.applyErrorPolicy(a.UID, ue.Kind, ue.Msg, model, ue)
			}
			continue
		}
		obj, aerr := upstream.Aggregate(rc)
		_ = rc.Close()
		if aerr != nil {
			lastErr = aerr
			continue
		}
		if m := firstChoiceMessage(obj); m != nil {
			return m, nil
		}
		lastErr = errors.New("上游响应无 choices[0].message")
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("没有可用的 %s 账号", realm)
	}
	return nil, lastErr
}

func firstChoiceMessage(obj map[string]any) map[string]any {
	ch, _ := obj["choices"].([]any)
	if len(ch) == 0 {
		return nil
	}
	c, _ := ch[0].(map[string]any)
	m, _ := c["message"].(map[string]any)
	return m
}

// searchText 执行一次检索，返回注入用的文本与结构化结果；失败返回 false（调用方降级）。
func (h *Handler) searchText(ctx context.Context, realm, model, query string) (string, []upstream.SearchResult, bool) {
	tried := map[string]bool{}
	for i := 0; i < webSearchPickTries; i++ {
		a := h.cfg.Pool.PickExcludingForRealm(tried, model, realm)
		if a == nil {
			break
		}
		tried[a.UID] = true
		raw, err := h.cfg.Upstream.Search(ctx, a, query, upstream.WebSearchDefaultMaxResults)
		if err != nil {
			var ue *upstream.Error
			if errors.As(err, &ue) {
				h.applyErrorPolicy(a.UID, ue.Kind, ue.Msg, model, ue)
			}
			log.Printf("WARN: [messages] web_search %s: %v", logfmt.Label(a.UID, a.Nickname), err)
			continue
		}
		var rs upstream.SearchResponse
		if err := json.Unmarshal(raw, &rs); err != nil || len(rs.Results) == 0 {
			continue
		}
		return formatSearchResults(rs), rs.Results, true
	}
	return "", nil, false
}

// formatSearchResults 检索结果 → 注入文本（URL 必带：模型要靠它引用来源）。
func formatSearchResults(rs upstream.SearchResponse) string {
	var b strings.Builder
	fmt.Fprintf(&b, "检索「%s」的结果：", rs.Query)
	for i, r := range rs.Results {
		fmt.Fprintf(&b, "\n%d. %s\n   %s\n   %s", i+1, r.Title, r.URL, r.Snippet)
	}
	return b.String()
}

// webSearchContext 注入用上下文：说清这是网关代执行的联网结果、引用要给 URL。
func webSearchContext(items []string) string {
	return "[联网检索结果｜由网关代为执行，共 " + strconv.Itoa(len(items)) + " 次]\n" +
		"以下是外部网页检索结果，引用时请给出 URL；" +
		"检索不到或互相矛盾的细节不要当成事实。\n\n" +
		strings.Join(items, "\n\n")
}
