// fetch.go POST /v1/fetch：DeepSeek Fetch API 兼容入口（DSH 的 web-deepseek-api）。
//
// DSH 的 Search / Fetch 共用一份 baseURL（默认 https://api.deepseek.com/v1/tools，
// 追加 /search 与 /fetch）。把 baseURL 指向网关时：
//
//	POST /v1/search        {"query":"...", "count":3}      → 见 search.go
//	POST /v1/fetch         {"url":"https://example.com"}   → 本文件
//	POST /v1/tools/search  {"query":"...", "count":3}      → 别名，同 search.go
//	POST /v1/tools/fetch   {"url":"https://example.com"}   → 别名，同本文件
//
// 四条路径同一份实现：官方形状（/v1/tools/*）与本网关的自有形状（/v1/*）并存，
// 差别只在 search 侧是否补 DeepSeek 字段（见 search.go 的 isDeepSeekToolsPath）；
// fetch 侧没有可补的字段，故两边行为天然一致。
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
	"log"
	"net"
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
	// 积分闸（审查 B1）：/v1/fetch 走上游抓取，同样烧额度。
	if !h.admitBudget(w, r) {
		return
	}
	// 超限 413，不静默截断（审查 W3C8-32 / W1A4-20）：与 search 同一处修复、同一文案口径。
	raw, ok := readBodyCapped(w, r, 1<<20, `（/v1/fetch 只收 url / realm 两个短字段）`)
	if !ok {
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
	// 私网/回环/链路本地/云元数据一律本地拦掉（审查 W1A4-10 / W3C8-29）：本端点把 url
	// 原样交给上游 webfetch 代取，等于给任何持 key 的调用方开了一个 SSRF 代理 ——
	// `http://169.254.169.254/latest/meta-data/iam/security-credentials/` 拿到的是云凭据，
	// `http://127.0.0.1:PORT/` / `10.0.0.0/8` 是内网探测面。可达性取决于上游的网络位置，
	// 但那不是本网关能赌的事：字面量地址先拒，域名交给上游（见 fetchURLGuard 的边界说明）。
	if err := fetchURLGuard(target); err != nil {
		writeOpenAIErrorHint(w, http.StatusBadRequest, "invalid_request", err.Error(),
			"本端点只抓公网 http(s) 页面；内网/回环/云元数据地址在网关侧就被拒绝")
		return
	}
	realm, _ := resolveModel(req.Model)
	if req.Realm != "" {
		realm = req.Realm
	}
	if realm != "global" {
		realm = "cn"
	}
	// realm=global 但部署没开 global 域时直接拒（审查 W3C8-32）：否则「只开 cn」的部署意图
	// 被一个请求字段绕过（global 池非空时会真的去 global 号上抓）。与 embeddings 同口径。
	if realm == "global" && !h.cfg.GlobalEnabled {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			"realm=global 未启用：本网关只开了 cn 域（config.features.global_enabled=false）")
		return
	}

	tried := map[string]bool{}
	lastErr, handled := h.fetchInRealm(w, r, target, realm, tried)
	if handled {
		return
	}
	// 域名被上游路由策略拒绝（code 15021）时**跨域兜底**：CN 域 edgebrowser 对 Google
	// 系域名一律 403 15021，这是**域名级**判定 —— 换遍全池 CN 号都是同一个 403，而
	// global 域同域名正常。DSh 这类客户端不传 realm、缺省走 CN，于是「抓 Google 文档」
	// 在默认配置下必失败。
	//
	// 只对边缘级失败（15021 域名被拒 / 15022 连不上）跨域：两域积分/账号互不相通，
	// 5xx / 限流跨域没有意义。DSH 不传 realm、缺省走 CN，于是「抓 Google 文档」在
	// 默认配置下必失败，这条兜底让它在用户无感的情况下用 global 账号拿到正文。
	if lastErr != nil && isFetchEdgeFault(lastErr) {
		alt := "global"
		if realm == "global" {
			alt = "cn"
		}
		if h.cfg.Pool.ServableForRealm(alt) {
			log.Printf("fetch realm=%s: 域名被上游路由策略拒绝（15021），改试 %s 域", realm, alt)
			lastErr, handled = h.fetchInRealm(w, r, target, alt, tried)
			if handled {
				return
			}
			realm = alt // 日志/错误口径按最终尝试的域
		}
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

// fetchInRealm 在**单域**内跑完选号/抓取/错误处置。
//
// 返回 (lastErr, handled)：handled=true 表示响应已经写出（成功 / 请求级错误透传），
// 调用方直接 return；否则把 lastErr 交给跨域兜底判断。
func (h *Handler) fetchInRealm(w http.ResponseWriter, r *http.Request, target, realm string, tried map[string]bool) (error, bool) {
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
		// 在途名额用 defer 释放（审查 W3C8-30）：此前是普通语句，WebFetch 一旦 panic
		// （JSON 解析 / TLS / 代理层都可能），该账号的在途计数永久 +1，累积到上限后
		// 这个号再也选不出来 —— 表现为「幽灵在途」一个个吃掉可用账号，最后 no_account。
		data, ferr := func() ([]byte, error) {
			defer h.cfg.Pool.Release(a.UID)
			return h.cfg.Upstream.WebFetch(r.Context(), a, target)
		}()
		if ferr == nil {
			out, perr := normalizeFetchResponse(data, target)
			if perr != nil {
				// 上游 200 但形状不对：改写成 502，别把半成品当成功透传。
				log.Printf("fetch %s realm=%s: 上游响应不可用: %v", logfmt.Label(a.UID, a.Nickname), realm, perr)
				writeOpenAIError(w, http.StatusBadGateway, "upstream_error", perr.Error())
				return lastErr, true
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(out)
			return nil, true
		}
		lastErr = ferr
		var ue *upstream.Error
		if errors.As(ferr, &ue) {
			// 与 search/images 同一套失败处置（冷却/熔断/模型负缓存/12153 计数）。
			h.applyErrorPolicy(a.UID, ue.Kind, ue.Msg, "", ue)
			// 域名/边缘级错误换号必然复现，立刻交回调用方（跨域兜底只在那一层做）。
			if isFetchEdgeFault(ferr) {
				log.Printf("fetch realm=%s: 域名/边缘级错误，不再换号: %v", realm, ferr)
				return ferr, false
			}
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
				return lastErr, true
			}
		}
		log.Printf("fetch %s realm=%s attempt=%d: %v", logfmt.Label(a.UID, a.Nickname), realm, attempt+1, ferr)
	}

	return lastErr, false
}

// fetchURLGuard 拒绝「不该由网关代取」的目标（审查 W1A4-10 / W3C8-29）。
//
// 边界说明（刻意如此）：只校验**字面量**主机与端口，不做 DNS 解析。
//   - 做 DNS 再校验需要真的联网解析，会把一个纯本地校验变成可能阻塞/失败的调用，
//     而且解析结果与实际连接时的解析结果可以不同（DNS rebinding 照样绕过）；
//   - 真正的收口位置是上游 webfetch 的出口策略（本网关只是转发方，拿不到它的 socket）。
//
// 所以这里的目标是：把「一眼就是内网/元数据」的目标在本地拦掉，让误用与低级探测失败在
// 网关侧、而不是变成一次上游调用。域名形态的内网地址（如 intranet.corp）不在拦截范围内。
func fetchURLGuard(target string) error {
	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		return fmt.Errorf("url 缺少主机：%s", target)
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" {
		return fmt.Errorf("url 缺少主机：%s", target)
	}
	// 端口只放行 80/443（含缺省）：上游 webfetch 只会走这两个，其余端口是内网服务探测的典型形态。
	if port := u.Port(); port != "" && port != "80" && port != "443" {
		return fmt.Errorf("只允许 80/443 端口（收到 %s）", port)
	}
	switch host {
	case "localhost", "metadata", "metadata.google.internal", "instance-data":
		return fmt.Errorf("拒绝内网/元数据地址：%s", host)
	}
	if strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".internal") {
		return fmt.Errorf("拒绝内网/元数据地址：%s", host)
	}
	if ip := net.ParseIP(host); ip != nil {
		if fetchIPBlocked(ip) {
			return fmt.Errorf("拒绝私网/回环/链路本地地址：%s", host)
		}
	}
	return nil
}

// fetchIPBlocked 回环 / 私网 / 链路本地（含云元数据 169.254.169.254）/ 未指定 / 组播。
func fetchIPBlocked(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true
	}
	// IPv4 映射到 IPv6 的形态（::ffff:127.0.0.1）：IsLoopback 已覆盖，但 ::ffff:10.x 不是
	// IsPrivate 的判定对象（Go 只对 4in6 的 To4 结果判私网），这里显式再判一次。
	if v4 := ip.To4(); v4 != nil && net.IP(v4).IsPrivate() {
		return true
	}
	// IPv6 唯一本地地址 fc00::/7（云上常见的内部服务网段）。
	if len(ip) == net.IPv6len && (ip[0]&0xfe) == 0xfc {
		return true
	}
	return false
}

// isFetchEdgeFault 上游 edgebrowser 的**域名/边缘**级失败，与账号健康无关：
//   - 15021「domain rejected by via routing policy」：域名被上游路由策略拒绝；
//   - 15022「net::ERR_CONNECTION_REFUSED」：上游浏览器连不上目标站。
//
// 两个 code 都是「同一个 URL 换任何账号结果一致」的判定（issue #280 实测：一次请求
// 轮了 3 个 CN 号，三行的 code 与文案完全相同）。换号只是白烧名额、给健康号喂连败，
// 所以命中即退出本域，由 fetch() 决定是否换域再试（不同域的出口不同，15022 换域有救）。
func isFetchEdgeFault(err error) bool {
	var ue *upstream.Error
	if !errors.As(err, &ue) {
		return false
	}
	return upstream.HasCodeMarker(ue.Msg, "15021") || upstream.HasCodeMarker(ue.Msg, "15022")
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
