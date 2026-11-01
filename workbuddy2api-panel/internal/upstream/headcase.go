// headcase.go 出站请求头**大小写拼写**对齐官方真机。
//
// 背景：Go 的 net/http 对 header key 做 CanonicalMIMEHeaderKey 归一化
// （`acp-connection-id` → `Acp-Connection-Id`、`X-B3-ParentSpanId` →
// `X-B3-Parentspanid`），而官方客户端是 Node，wire 上写的是代码里的字面量。
// 头名集合可以完全一致、值可以完全一致，但逐字节看仍然是两族包。
//
// 2026-11-01 双版本真机抓包（testdata/official_*_head.txt 逐名提取）确认
// 官方的拼写是**大小写混合**的，不是全大写也不是全小写：
//
//	小写：  x-stainless-*（8 个）、acp-connection-id、x-codebuddy-request、
//	        baggage、traceparent、b3
//	大写：  X-Conversation-ID / X-Request-ID / X-IDE-{Type,Name,Version} /
//	        X-Trace-ID / X-Root-Request-ID / X-B3-{TraceId,ParentSpanId,SpanId}
//
// 所以正确的做法是**逐名映射**，不是无差别小写化——后者会把 User-Agent 打回
// `Go-http-client/1.1`（见 normalizeHeaderSpelling 的注释）。
package upstream

import (
	"net/http"
	"strings"
)

// officialHeaderSpelling 官方出站头的精确拼写（key 一律小写，value 是 wire 上
// 真实出现的形式）。表内含 45 个真机确认过的头名，是本项目的单一事实来源。
//
// 只放真机样本里**实际出现过**的名字：X-Enterprise-Id / X-Tenant-Id 这类企业号
// 才有、个人号抓包里没有的头不在表里，落到 default 分支保持原样（不乱猜）。
var officialHeaderSpelling = map[string]string{
	// —— 本来就与 Go canonical 一致，列出来是为了钉死（防止被将来谁改成小写） ——
	"accept":            "Accept",
	"accept-encoding":   "Accept-Encoding",
	"accept-language":   "Accept-Language",
	"authorization":     "Authorization",
	"content-encoding":  "Content-Encoding",
	"content-length":    "Content-Length",
	"content-type":      "Content-Type",
	"cookie":            "Cookie",
	"origin":            "Origin",
	"referer":           "Referer",
	"user-agent":        "User-Agent",
	"x-agent-intent":    "X-Agent-Intent",
	"x-agent-purpose":   "X-Agent-Purpose",
	"x-agent-runtime":   "X-Agent-Runtime",
	"x-agent-type":      "X-Agent-Type",
	"x-b3-sampled":      "X-B3-Sampled",
	"x-domain":          "X-Domain",
	"x-private-data":    "X-Private-Data",
	"x-product":         "X-Product",
	"x-requested-with":  "X-Requested-With",
	"x-user-id":         "X-User-Id",
	"x-client-platform": "X-Client-Platform",

	// —— 官方全小写（Node 侧字面量就是小写；三个 B3/链路头尤其） ——
	"x-stainless-arch":            "x-stainless-arch",
	"x-stainless-lang":            "x-stainless-lang",
	"x-stainless-os":              "x-stainless-os",
	"x-stainless-package-version": "x-stainless-package-version",
	"x-stainless-retry-count":     "x-stainless-retry-count",
	"x-stainless-runtime":         "x-stainless-runtime",
	"x-stainless-runtime-version": "x-stainless-runtime-version",
	"acp-connection-id":           "acp-connection-id",
	"x-codebuddy-request":         "x-codebuddy-request",
	"baggage":                     "baggage",
	"traceparent":                 "traceparent",
	"b3":                          "b3",

	// —— 官方保留大写缩写（Go 会把这些缩写拍成首字母大写，是最明显的 Go 痕迹） ——
	"x-conversation-id":         "X-Conversation-ID",
	"x-conversation-message-id": "X-Conversation-Message-ID",
	"x-conversation-request-id": "X-Conversation-Request-ID",
	"x-request-id":              "X-Request-ID",
	"x-root-request-id":         "X-Root-Request-ID",
	"x-trace-id":                "X-Trace-ID",
	"x-ide-type":                "X-IDE-Type",
	"x-ide-name":                "X-IDE-Name",
	"x-ide-version":             "X-IDE-Version",
	"x-b3-traceid":              "X-B3-TraceId",
	"x-b3-parentspanid":         "X-B3-ParentSpanId",
	"x-b3-spanid":               "X-B3-SpanId",
}

// normalizeHeaderSpelling 返回一个 header 拼写已对齐官方的请求副本。
//
// 为什么返回副本而不是原地改：调用方与测试常在发请求后回读 req.Header，
// 原地改写会让 `Header.Get("X-Request-ID")` 之类取不到值（Get 会先把查询键
// canonicalize 成 `X-Request-Id`，而 map 里存的是 `X-Request-ID`）。
//
// 为什么不用"全部小写化"这一招：Go 的 Request.write 用 **canonical key** 查
// 六个特判头——User-Agent / Host / Content-Length / Transfer-Encoding /
// Trailer / Accept-Encoding。把它们存成小写会导致：
//   - `user-agent` 查不到 → 回落成 `Go-http-client/1.1`，精心对齐的 UA 全废
//   - `content-length` 不被跳过 → 与真实长度重复发送
//   - `accept-encoding` 被判为未设置 → Transport 自己再补一个 gzip
//
// 逐名映射恰好避开全部三个坑：官方那六个的拼写本来就是 canonical 形式，
// 表里写的就是原样值。这也是本方案与"无差别小写"最本质的区别。
func normalizeHeaderSpelling(req *http.Request) *http.Request {
	if req == nil || len(req.Header) == 0 {
		return req
	}
	r2 := req.Clone(req.Context())
	h := make(http.Header, len(req.Header))
	for k, v := range req.Header {
		if want, ok := officialHeaderSpelling[strings.ToLower(k)]; ok {
			h[want] = v
			continue
		}
		h[k] = v // 表外头（企业号专属等）保持原样，不猜
	}
	r2.Header = h
	return r2
}

// headerFold 大小写不敏感地取头值。
//
// normalizeHeaderSpelling 之后，wire 上的 key 是官方拼写（如
// "X-Conversation-Request-ID"），而 http.Header.Get 会把**查询键**canonicalize 成
// "X-Conversation-Request-Id" → 取不到。测试与探针在自定义 RoundTripper 层读头时
// 必须用它，不要再直接 Get。
func headerFold(h http.Header, name string) string {
	for k, v := range h {
		if strings.EqualFold(k, name) && len(v) > 0 {
			return v[0]
		}
	}
	return ""
}
