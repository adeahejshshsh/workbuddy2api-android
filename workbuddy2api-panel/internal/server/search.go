// search.go POST /v1/search：官方 agenttool 联网检索的网关入口（issue #195 的 A 形态）。
//
// 非对话端点，与 images 同一套取号与失败处置：单次取号、失败换号上限 3、
// 请求级错误（参数/内容/模型）fail-fast 不烧全池。回程把上游原文原样写出
// （{query,type,provider,results,total_results,response_time_ms}），不做二次包装。
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
	Model      string `json:"model"` // 可选，只取 realm 前缀
	Realm      string `json:"realm"` // 可选，优先于 model 前缀
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

	tried := map[string]bool{}
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
		data, err := h.cfg.Upstream.Search(r.Context(), a, query, req.MaxResults)
		h.cfg.Pool.Release(a.UID)
		if err == nil {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(data)
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
