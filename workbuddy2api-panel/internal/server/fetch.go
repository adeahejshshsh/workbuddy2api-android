// fetch.go POST /v1/fetch：DeepSeek Fetch API 兼容入口（DSH 的 web-deepseek-api）。
//
// DSH 的 Search / Fetch 共用一份 baseURL（默认 https://api.deepseek.com/v1/tools，
// 追加 /search 与 /fetch）。把 baseURL 指向网关时：
//
//	POST /v1/search  {"query":"...", "count":3}      → 见 search.go
//	POST /v1/fetch   {"url":"https://example.com"}   → 本文件
//
// 期望响应是 JSON {"content":"<markdown>"}（DSH 只校验 content 是 string）；
// 上游 agenttool/v1/webfetch 返回 {url,title,content}，这里原字段透传，缺
// content 时按 502 报错（DSH 会当成 provider error 展示给模型）。
//
// 取号/失败处置与 /v1/search 同源：单次取号、失败换号上限 3、请求级 400
// fail-fast 不烧全池。realm 解析也一致（"model":"cn:x" 前缀或 "realm" 字段，
// 缺省 cn）；DSH 不会传，所以默认走 CN。
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

type fetchRequest struct {
	URL   string `json:"url"`
	Model string `json:"model"` // 可选，只取 realm 前缀（与 search 同口径）
	Realm string `json:"realm"` // 可选，优先于 model 前缀
}

func (h *Handler) fetch(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var req fetchRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid json: "+err.Error())
		return
	}
	target := strings.TrimSpace(req.URL)
	if target == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			`url is required（例：{"url":"https://example.com"})`)
		return
	}
	// 只接受 http/https：上游 webfetch 对其它 scheme 只会回 400，本地先拦住，
	// 少一次白烧的上游请求。
	if u, perr := url.Parse(target); perr != nil || (u.Scheme != "http" && u.Scheme != "https") {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			"url must be an absolute http(s) URL")
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
		data, ferr := h.cfg.Upstream.WebFetch(r.Context(), a, target)
		if ferr == nil {
			out, perr := normalizeFetchResponse(data, target)
			if perr != nil {
				// 上游 200 但形状不对：改写成 502，别把半成品当成功透传。
				log.Printf("fetch %s realm=%s: 上游响应不可用: %v", logfmt.Label(a.UID, a.Nickname), realm, perr)
				writeOpenAIError(w, http.StatusBadGateway, "upstream_error", perr.Error())
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(out)
			return
		}
		lastErr = ferr
		var ue *upstream.Error
		if errors.As(ferr, &ue) {
			// 与 search/images 同一套失败处置（冷却/熔断/模型负缓存/12153 计数）。
			h.applyErrorPolicy(a.UID, ue.Kind, ue.Msg, "", ue)
			// 请求级错误（参数/内容/模型，及 400+ErrClient 的 url 校验失败）换号
			// 必然同样被拒，直接透传上游原文。
			if requestLevelFailFastKind(ue.Kind) ||
				(ue.Status == http.StatusBadRequest && ue.Kind == upstream.ErrClient) {
				status := ue.Status
				if status < 400 {
					status = http.StatusBadRequest
				}
				log.Printf("fetch realm=%s: 请求级错误 kind=%v，不再换号", realm, ue.Kind)
				writeOpenAIError(w, status, "upstream_error", ue.Msg)
				return
			}
		}
		log.Printf("fetch %s realm=%s attempt=%d: %v", logfmt.Label(a.UID, a.Nickname), realm, attempt+1, ferr)
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
	log.Printf("fetch realm=%s: 全部尝试失败（%d 个账号）：%v", realm, len(tried), lastErr)
	writeOpenAIError(w, status, code, lastErr.Error())
}

// normalizeFetchResponse 确保响应是 DSH 需要的 {"content": "<markdown>"} 形状。
//
// 上游 agenttool/v1/webfetch 回 {url,title,content}：content 必须是 string，
// 缺 content / content 为 null / 非对象时判失败（502），不伪造空正文——空正文
// 会被 DSH 当成"页面没有可用内容"的成功抓取，掩盖真实故障。
// url 缺失时补请求 URL（DSH 不消费该字段，但上游语义上应回显）。
func normalizeFetchResponse(raw []byte, reqURL string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("upstream fetch response is not a JSON object")
	}
	content, ok := obj["content"]
	if !ok || string(content) == "null" {
		return nil, fmt.Errorf("upstream fetch response has no content field")
	}
	var s string
	if err := json.Unmarshal(content, &s); err != nil {
		// json.Unmarshal 对 null 不报错（保持零值），故上面先单独拦 null。
		return nil, fmt.Errorf("upstream fetch content is not a string")
	}
	if _, ok := obj["url"]; !ok {
		obj["url"] = json.RawMessage(strconv.Quote(reqURL))
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("encode fetch response: %w", err)
	}
	return out, nil
}
