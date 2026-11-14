// search.go POST /v1/search：官方 agenttool 联网检索的网关入口（issue #195 的 A 形态）。
//
// 非对话端点，与 images 同一套取号与失败处置：单次取号、失败换号上限 3、
// 请求级错误（参数/内容/模型）fail-fast 不烧全池。回程默认把上游原文原样写出
// （{query,type,provider,results,total_results,response_time_ms}），不做二次包装；
// 仅当请求来自 DSH（UA 含 deepseek-harness）时，额外补成 DeepSeek Search API
// 需要的形状：date:null / title/snippet 字符串、空 url 的卡片改写为等价检索链接，
// 并接受 count 作为条数。
//
// 选号域：请求可带 "model":"cn:x" 只用其 realm 前缀，或直接 "realm":"global"；都没有 → cn。
//
//	POST /v1/search
//	{"query": "玉林 电梯 维保 新规", "max_results": 5}
//	{"query": "...", "deep": true}   ← 综述式（agentic_search），单次 40~66s
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
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
	// Deep 走官方综述式检索（agentic_search），回 done 帧原文（synthesis.content
	// 带引用的综述 + results 整页正文）。实测单次 40~66s、prompt 47K tokens，
	// 故默认 false 保持 snippet 快路径（~1s）。
	Deep bool `json:"deep"`
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

// cardSearchBase 卡片改写落点。
//
// 上游 /agenttool/v1/search 会把「网页」和「结构化卡片」混在同一个 results 里：卡片
// 是答案引擎直接生成的富文本（股价表、分数线表…），有 title/snippet/site/favicon，
// 唯独 **url 为空**——它不是网页。DSH 的 deepseek-search-api 要求 url 是非空字符串
// （`typeof item.url !== "string" || item.url.length === 0` 即抛 invalidResponse），
// 而且一条不合法就整包作废，表现为 "DeepSeek returned an invalid response body"。
//
// 这里改写而不是丢弃：卡片的 snippet 往往就是用户要的答案，改写后模型仍读得到、
// 用户仍点得开。用 var 便于测试替换落点。
var cardSearchBase = "https://cn.bing.com/search?q="

// compatDeepSeekSearch 把上游原文补成 DeepSeek Search API 兼容形状（仅 DSH）。
//
// 三件事：
//  1. 空 url 的条目按上游卡片处理，改写成一次同词检索的链接（见 cardSearchBase）；
//     query 与 title 皆空、无从取词的才丢弃，并同步修正 total_results。
//  2. results[] 里缺 date 时补 `null`。上游（WorkBuddy /agenttool）不返回发布日期，
//     而 DSH 用 `item.date !== null && typeof item.date !== 'string'` 判定，字段
//     **缺失**（undefined）会直接抛 invalidResponse（"invalid response body"）。
//  3. title/snippet 缺失或为 null 时补空串：DSH 要求是 string，空串合法。
//
// 调用方只在 deepSeekSearchClient(UA) 为真时走到这里；其余客户端逐字节透传。
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

	// 卡片改写的取词：优先本次检索词（上游响应自带 query），空则退回卡片标题。
	// 用检索词而不是标题：卡片的 title 常是表头（如「港股行情指标(货币-港元) |
	// A股行情指标(货币-人民币)」），拿去搜没有意义；检索词才是用户真正想找的东西。
	var query string
	if q, has := obj["query"]; has {
		_ = json.Unmarshal(q, &query)
	}
	query = strings.TrimSpace(query)

	kept := items[:0]
	for _, it := range items {
		if strings.TrimSpace(jsonString(it, "url")) == "" {
			term := query
			if term == "" {
				term = strings.TrimSpace(jsonString(it, "title"))
			}
			if term == "" {
				continue // 无取词可改写：发非法条目比丢掉更糟
			}
			patch, err := json.Marshal(cardSearchBase + url.QueryEscape(term))
			if err != nil {
				continue
			}
			it["url"] = patch
		}
		if _, has := it["date"]; !has {
			it["date"] = json.RawMessage("null")
		}
		// DSH 对 title/snippet 也要求是 string（空串可以）：上游偶发省略或给 null
		// 时补空串，避免同一类 invalidResponse 再出现一次。
		ensureJSONString(it, "title")
		ensureJSONString(it, "snippet")
		kept = append(kept, it)
	}
	if len(kept) != len(items) {
		// 有条目被丢弃：total_results 只扣掉丢弃数，而不是覆盖成 len(kept)——
		// 上游的它是命中总数（实测可大于本页条数，如 results 1 条而 total_results 5），
		// 覆盖会把「共 5 条」谎报成「共 1 条」。下限取 len(kept)，避免「说 7 条回 8 条」。
		var total int
		if raw, has := obj["total_results"]; has && json.Unmarshal(raw, &total) == nil {
			total = max(total-(len(items)-len(kept)), len(kept))
			if n, err := json.Marshal(total); err == nil {
				obj["total_results"] = n
			}
		}
	}
	patchedResults, err := json.Marshal(kept)
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

// jsonString 取字符串字段；缺失、null 或类型不符一律按空串处理。
func jsonString(item map[string]json.RawMessage, key string) string {
	v, has := item[key]
	if !has {
		return ""
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return ""
	}
	return s
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
		// 选号与在途占位在同一把锁内（check-then-act 收敛）：此前是 Pick + Acquire
		// 两步，并发下多个请求同时选中同一个还有空位的号、只有一个抢到，其余把这个
		// 号记进 tried 重选 → 空位明明还在却报「在途已满」。
		a := h.cfg.Pool.PickAcquireExcludingForRealm(tried, "", realm)
		if a == nil {
			break
		}
		tried[a.UID] = true
		// 用算好的 maxResults（含 count 回落）：直接用 req.MaxResults 会让
		// DeepSeek Search API 的 count 字段永远不生效。
		var data json.RawMessage
		var err error
		if req.Deep {
			data, err = h.cfg.Upstream.AgenticSearch(r.Context(), a, query)
		} else {
			data, err = h.cfg.Upstream.Search(r.Context(), a, query, maxResults)
		}
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
// search 与 images 共用：检索/出图的域级模型路由（14401 route config not found）、
// 审核与参数错误都是请求本身的属性，换号只会打满全池。chat 侧另有一套（各账号的
// 模型权限可能不同）。
func requestLevelFailFastKind(kind upstream.ErrKind) bool {
	switch kind {
	case upstream.ErrBadParams, upstream.ErrPromptTooLong, upstream.ErrImageInvalid,
		upstream.ErrModelParamInvalid, upstream.ErrEmptyContent,
		upstream.ErrContentBlocked, upstream.ErrModelBlocked:
		return true
	}
	return false
}
