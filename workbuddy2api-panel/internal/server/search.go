// search.go POST /v1/search：官方 agenttool 联网检索的网关入口（issue #195 的 A 形态）。
//
// 非对话端点，与 images 同一套取号与失败处置：单次取号、失败换号上限 3、
// 请求级错误（参数/内容/模型）fail-fast 不烧全池。回程默认把上游原文原样写出
// （{query,type,provider,results,total_results,response_time_ms}），不做二次包装；
// 仅当请求来自 DSH（UA 含 deepseek-harness）时，额外补 DeepSeek Search API
// 需要的 date:null / title/snippet 字符串，并接受 count 作为条数。
//
// 选号域：请求可带 "model":"cn:x" 只用其 realm 前缀，或直接 "realm":"global"；都没有 → cn。
//
//	POST /v1/search
//	{"query": "玉林 电梯 维保 新规", "max_results": 5}
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// searchAttempts 失败换号上限（与 images 同量级：人工/低频触发，3 次足够绕开坏号）。
const searchAttempts = 3

type searchRequest struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results"`
	// Count DeepSeek Search API 形状的条数字段（DSH 的 deepseek-search-api 发
	// count 而不是 max_results）；max_results 缺省时回落到它。
	Count int    `json:"count"`
	Model string `json:"model"` // 可选，只取 realm 前缀
	Realm string `json:"realm"` // 可选，优先于 model 前缀
}

// deepSeekSearchClient 判定请求是否来自 DSH（DeepSeek Harness）。
//
// 兼容补丁只对这类客户端启用：DSH 的 UA 实测为
// "deepseek-harness/0.2.1-rc.2 (+https://github.com/deepseek-ai/deepseek-harness)"，
// 用窄子串匹配（大小写不敏感），不猜其它客户端。
//
// 为什么按客户端而不是无条件补：其余客户端（自建脚本 / curl / Codex …）拿到的
// 响应保持上游原文逐字节透传，避免为「不需要该字段」的调用方改动字段集合。
func deepSeekSearchClient(ua string) bool {
	return strings.Contains(strings.ToLower(ua), "deepseek-harness")
}

// compatDeepSeekSearch 把上游原文补成 DeepSeek Search API 兼容形状（仅 DSH）。
//
// 只做一件事：results[] 里缺 date 时补 `null`。上游（WorkBuddy /agenttool）不返回
// 发布日期，而按 DeepSeek Search API 接的客户端校验很硬——DSH 的
// deepseek-search-api 用 `item.date !== null && typeof item.date !== 'string'`
// 判定，字段**缺失**（undefined）会直接抛 invalidResponse（"invalid response body"）。
// 其余字段（title/url/snippet）上游本来就有。调用方只在
// deepSeekSearchClient(UA) 为真时走到这里。
func compatDeepSeekSearch(raw []byte) []byte {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw
	}
	rawResults, ok := obj["results"]
	if !ok {
		return raw
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(rawResults, &items); err != nil || len(items) == 0 {
		return raw
	}
	for _, it := range items {
		if _, has := it["date"]; !has {
			it["date"] = json.RawMessage("null")
		}
		// DSH 对 title/snippet 也要求是 string（空串可以）：上游偶发省略或给 null
		// 时补空串，避免同一类 invalidResponse 再出现一次。
		ensureJSONString(it, "title")
		ensureJSONString(it, "snippet")
	}
	patchedResults, err := json.Marshal(items)
	if err != nil {
		return raw
	}
	obj["results"] = patchedResults
	out, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return out
}

// ensureJSONString 把缺失或 null 的字段补成空串；已是字符串则原样保留。
func ensureJSONString(item map[string]json.RawMessage, key string) {
	v, has := item[key]
	if !has || string(v) == "null" {
		item[key] = json.RawMessage(`""`)
	}
}

func (h *Handler) search(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var req searchRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid json: "+err.Error())
		return
	}
	query := strings.TrimSpace(req.Query)
	if query == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			`query is required（例：{"query":"玉林 电梯 维保 新规","max_results":5}）`)
		return
	}
	realm, _ := resolveModel(req.Model)
	if req.Realm != "" {
		realm = req.Realm
	}
	if realm != "global" {
		realm = "cn"
	}

	// DeepSeek Search API 兼容层只服务 DSH：按 UA 判定。其余客户端保持上游原文
	// 逐字节透传、条数只认 max_results（行为与兼容层加入前完全一致）。
	dshClient := deepSeekSearchClient(r.UserAgent())

	tried := map[string]bool{}
	// 条数：优先 max_results；DSH 的 deepseek-search-api 只发 count，回落它
	//（0 由上游取默认 5）。
	maxResults := req.MaxResults
	if dshClient && maxResults <= 0 {
		maxResults = req.Count
	}
	var lastErr error
	for attempt := 0; attempt < searchAttempts; attempt++ {
		a := h.cfg.Pool.PickExcludingForRealm(tried, "", realm)
		if a == nil {
			break
		}
		tried[a.UID] = true
		// 占在途名额（review #7）：与 chat 路径同口径，否则 max_in_flight /
		// max_in_flight_global 对检索端点不生效，in_flight 观测也不含它。
		if !h.cfg.Pool.Acquire(a.UID) {
			// 必须留痕（review #1）：三个账号都抢不到在途名额时 lastErr 仍是 nil，
			// 末尾 lastErr.Error() 直接空指针 panic，客户端拿到空响应。
			lastErr = fmt.Errorf("账号 %s 在途已满", a.UID)
			continue
		}
		// 用算好的 maxResults（含 count 回落）：直接用 req.MaxResults 会让
		// DeepSeek Search API 的 count 字段永远不生效。
		data, err := h.cfg.Upstream.Search(r.Context(), a, query, maxResults)
		h.cfg.Pool.Release(a.UID)
		if err == nil {
			w.Header().Set("Content-Type", "application/json")
			if dshClient {
				_, _ = w.Write(compatDeepSeekSearch(data))
			} else {
				_, _ = w.Write(data)
			}
			return
		}
		lastErr = err
		var ue *upstream.Error
		if errors.As(err, &ue) {
			// 与 chat/images 同一套失败处置（冷却/熔断/模型负缓存/12153 计数）。
			h.applyErrorPolicy(a.UID, ue.Kind, ue.Msg, "", ue)
			// 请求级错误：共享清单（参数/内容/模型）之外，search 的未知 400 也不换号——
			// 检索没有「模型权限」维度，400 是请求字段的属性（实测 15003 unsupported
			// provider / 15013 url is required 都走 400），换号必然同样被拒；
			// 而 429/402/401（限流/见底/会话失效）换号可能有效，故只收 400+ErrClient。
			if requestLevelFailFastKind(ue.Kind) ||
				(ue.Status == http.StatusBadRequest && ue.Kind == upstream.ErrClient) {
				status := ue.Status
				if status < 400 {
					status = http.StatusBadRequest
				}
				log.Printf("search realm=%s: 请求级错误 kind=%v，不再换号", realm, ue.Kind)
				writeOpenAIError(w, status, "upstream_error", ue.Msg)
				return
			}
		}
		log.Printf("search %s realm=%s attempt=%d: %v", logfmt.Label(a.UID, a.Nickname), realm, attempt+1, err)
	}

	status, code := http.StatusBadGateway, "upstream_error"
	var ue *upstream.Error
	switch {
	case len(tried) == 0:
		status, code = http.StatusServiceUnavailable, "no_account"
		lastErr = fmt.Errorf("没有可用的 %s 账号", realm)
	case errors.As(lastErr, &ue) && ue.Status >= 400:
		status = ue.Status
	}
	log.Printf("search realm=%s: 全部尝试失败（%d 个账号）：%v", realm, len(tried), lastErr)
	writeOpenAIError(w, status, code, lastErr.Error())
}

// requestLevelFailFastKind 报告该类上游错误是否与账号无关（换号必然同样失败）。
// search 侧用它判「请求本身的问题」：参数/内容/模型级 400 换号只会打满全池。
func requestLevelFailFastKind(kind upstream.ErrKind) bool {
	switch kind {
	case upstream.ErrBadParams, upstream.ErrPromptTooLong, upstream.ErrImageInvalid,
		upstream.ErrContentBlocked, upstream.ErrModelBlocked:
		return true
	}
	return false
}
