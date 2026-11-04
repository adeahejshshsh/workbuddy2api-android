// agentic.go 官方「综述式」联网检索（POST /agenttool/v1/agentic_search，SSE）。
//
// 与 /agenttool/v1/search（SnippetSearch）的分工：那是「给几条 snippet」，这条是
// **服务端自己跑多轮检索 + 抓页 + 综述**，done 帧一次给全：
//
//	{"synthesis":{"content":"<带引用的综述 13K 字>","inline_content":"<精简版>"},
//	 "results":[{"id":"yuanbao_xxx","content":"<整页正文>"}],   ← 实测 40KB
//	 "statuses":[{"source":"websearch|agent","status":"OK|ERROR","latency_ms":..,"result_count":..}],
//	 "token_usage":{...},"latency_ms":65108,"partial":false,"partial_reason":"","request_id":".."}
//
// 代价（实测 2026-10-16 CN）：单次 40~66s、prompt 47K tokens；故**默认不走**，只在调用方
// 显式要求时报（见 /v1/search 的 deep 字段）。
//
// 只做一件事：把 done 帧的 data **原文**取回来（含 partial/partial_reason——上游自己会标
// 降级，调用方看得见）。字段一个不改写、不裁剪：results 里的整页正文正是它的价值所在。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const (
	agenticSearchPath = "/agenttool/v1/agentic_search"
	// agenticSearchMode 官方面板/桌面端取的固定值（实测唯一取值）。
	agenticSearchMode = 2
	// agenticMaxBytes 收流上限：实测一次 ~95KB，留足余量又拦住失控流。
	agenticMaxBytes = 4 << 20
)

// AgenticSearch 综述式检索，返回 done 帧 data 原文（JSON 对象）。
// 长延迟由调用方承担（本函数只负责拿到底）；HTTP ≥400 按其状态码与原文分类。
func (c *Client) AgenticSearch(ctx context.Context, a *auth.Auth, query string) (json.RawMessage, error) {
	// biz_via 由账号类型决定：企业号走 internal（官方客户端的取法）。
	bizVia := "public"
	if a.EnterpriseID != "" {
		bizVia = "internal"
	}
	body, err := json.Marshal(map[string]any{
		"query": query, "search_mode": agenticSearchMode, "stream": true, "biz_via": bizVia,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatBase(a)+agenticSearchPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	c.ChatHeaders(req, a, "", ChatMeta{})
	// shortcut: 用 c.HTTP 统管整段流（Timeout 120s）——实测最慢 66s，够用；
	// 若哪天出现更长的综述，改用 chatHTTP（无总时长）+ 自己的读超时。
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, agenticMaxBytes))
	if err != nil {
		return nil, fmt.Errorf("read stream: %w", err)
	}
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	done, ok := agenticDone(raw)
	if !ok {
		// 200 但流里没有 done 帧（上游断流/换了协议）：不返回半截内容，如实报错。
		return nil, fmt.Errorf("上游未返回 done 帧（%d 字节）: %s", len(raw), truncate(string(raw), 160))
	}
	return done, nil
}

// agenticDone 从 SSE 流里取 event=done 帧的 data 原文。
// 帧分隔按 SSE 规范取空行，LF / CRLF / CR 三种换行都认（只找 "\n\n" 会让 CRLF 流永远切不出帧）。
func agenticDone(raw []byte) (json.RawMessage, bool) {
	var event string
	var data []string
	flush := func() (json.RawMessage, bool) {
		if event == "done" && len(data) > 0 {
			return json.RawMessage(strings.Join(data, "\n")), true
		}
		event, data = "", nil
		return nil, false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case line == "":
			if d, ok := flush(); ok {
				return d, true
			}
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	return flush() // 流末没有空行收尾的情况
}
