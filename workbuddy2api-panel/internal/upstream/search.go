// search.go 官方 agenttool 联网能力（国内版与国际版同构）：
//
//	POST {chatBase}/agenttool/v1/search   联网检索（snippet 级）
//	POST {agenttool}/v1/webfetch          抓网页正文（markdown）
//
// 鉴权与 chat 同源：Bearer + 会话三件套，官方无 HMAC/sign（实测只需 Authorization
// 即 200，三件套按官方客户端补齐是零风险的防御）。回程**原文透传**——该族响应没有
// {code,msg,data} 信封，故不走 doJSON。
//
// 请求字段的实测边界（2026-10-16，CN 两家 base 均验证）：
//   - type 只有小写 text2text 合法，省略时服务端自动回填；
//   - provider 是**响应字段**，请求里带上任何值都 400 {code:15003}（unsupported provider）;
//   - max_results 是上限不是保证（服务端硬上限约 10），官方固定用 5；
//   - allowed_domains / freshness / blocked_domains 传了不生效 —— 故不作为入参暴露，
//     免得给出「已过滤」的错误预期。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const (
	webSearchPath = "/agenttool/v1/search"
	webFetchPath  = "/agenttool/v1/webfetch"

	// WebSearchDefaultMaxResults 官方自己的取值（上限不是保证）。
	WebSearchDefaultMaxResults = 5
)

// SearchResult 单条检索结果（site/favicon 上游可选，缺失时零值）。
type SearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
	Site    string `json:"site,omitempty"`
}

// SearchResponse 检索响应（原文结构，供调用方按字段取用）。
type SearchResponse struct {
	Query          string         `json:"query"`
	Type           string         `json:"type"`
	Provider       string         `json:"provider"`
	Results        []SearchResult `json:"results"`
	TotalResults   int            `json:"total_results"`
	ResponseTimeMS int            `json:"response_time_ms"`
}

// Search 联网检索，返回上游原文 JSON（含 results/total_results/provider）。
// query 为空、maxResults <= 0 时由服务端/本函数填默认值。
func (c *Client) Search(ctx context.Context, a *auth.Auth, query string, maxResults int) (json.RawMessage, error) {
	if maxResults <= 0 {
		maxResults = WebSearchDefaultMaxResults
	}
	body, err := json.Marshal(map[string]any{
		"query":       query,
		"type":        "text2text",
		"max_results": maxResults,
	})
	if err != nil {
		return nil, err
	}
	return c.agentTool(ctx, a, webSearchPath, body)
}

// WebFetch 抓取网页正文，返回上游原文 JSON（{url,title,content}，content 为 markdown）。
func (c *Client) WebFetch(ctx context.Context, a *auth.Auth, url string) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]any{"url": url})
	if err != nil {
		return nil, err
	}
	return c.agentTool(ctx, a, webFetchPath, body)
}

// agentTool POST 到 agenttool 族端点。与 chat 同域（chatBase，realm 决定 CN/global）
// 同头（ChatHeaders，零值 ChatMeta 由三件套兜底恒发），≥400 交 Classify 定 Kind
// 供调用方走同一套失败处置。
func (c *Client) agentTool(ctx context.Context, a *auth.Auth, path string, body []byte) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatBase(a)+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.ChatHeaders(req, a, "", ChatMeta{})
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	// 200 也可以是业务错误（信封外确有 code 字段：15003/15013 实测走 400，
	// 但服务端若改用 200 返 code，这里必须拦住，否则把错误当结果透传）。
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && env.Code != 0 {
		kind := Classify(resp.StatusCode, env.Msg)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode,
			Msg: fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 160))}
	}
	return json.RawMessage(raw), nil
}
