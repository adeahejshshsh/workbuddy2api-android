package upstream

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// do 是**全部出站请求的唯一出口**（13 个调用点收敛到这里）。
//
// 为什么需要这一层：涉及「出站字节形态」的规则（头名拼写、头发送顺序）只应该写一次。
// 此前它们散在 client.go / desktop.go / global_models.go / global_register.go /
// webagent.go / search.go 六处，每加一条规则就要改六遍，漏一处就是一个可被比对的
// 指纹差异（上游 2026-11-01 的 `ee5467c`/`dd2ec95` 就是栽在这个形状上 —— 他们把
// 规则挂在自己的单一出口 proxy.do()，我们这边当时没有这一层）。
//
// 本文件的职责边界：**只做出站前的统一改写**，不改变请求内容、不做重试、不改超时。
// 谁需要按路径区分行为，在调用方决定，不在这里加分支。
// prepareOutbound 出站前的统一头改写。**do() 与 chat 流式路径都必须走它。**
//
// 头名大小写对齐官方真机（Go 的 CanonicalMIMEHeaderKey 会把 acp-connection-id 归一成
// Acp-Connection-Id、X-B3-ParentSpanId 拍成 X-B3-Parentspanid —— 头集合与取值全对，
// 逐字节看仍是另一族包）。逐名映射而非无差别小写化：后者会把 User-Agent 打回
// Go-http-client/1.1。外加官方必发、Go 默认不发的 Connection 行（见 applyConnectionHeader）。
//
// 为什么单独抽出来：chat 流式走 `c.chatHTTP().Do(req)`（无总时长上限的专用 client），
// 此前**绕过了 do()**，于是最敏感的那条链上每请求少一行 Connection、错两个大小写 ——
// 而唯一的端到端验证 headorder_e2e_test 走的却是 do()，测试永远绿。
// 注意这里**不做** decodeBodyIfNeeded：chat 是 SSE，Accept-Encoding 已被删掉让
// net/http 自己透明解压，再套一层解码会打乱 monitorBody/idle/首包计时的时序。
func prepareOutbound(req *http.Request) *http.Request {
	sanitizeOutboundHeaderValues(req)
	applyConnectionHeader(req)
	return normalizeHeaderSpelling(req)
}

// sanitizeOutboundHeaderValues 出站前把所有头值过一遍 HeaderSafe（审查 W1-06）。
//
// 为什么放在这里而不是逐点包 HeaderSafe：X-User-Id / X-Enterprise-Id / X-Domain
// 在十余处构造点各 Set 一次，逐点加必然漏 —— 本轮就漏了这三条。它们的取值来自
// 账号落盘数据（uid / 企业号 / domain 都可由导入的 auths/*.json 提供），一旦带
// CRLF：
//   - net/http 在写请求时直接拒绝整个请求（invalid header field value）；
//   - 这次失败被上层记成「上游故障」，同一账号连续 5 次降权出池 ——
//     一个畸形字段就能把好号耗死，且日志里只看得到「上游 5 次失败」。
//
// prepareOutbound 是全部出站请求的唯一出口（do() 与 chat 流式路径都过它），
// 在这里做「默认清洗」，以后新增任何头都不可能绕过。只删控制字符，可打印内容
// 逐字保留，与 HeaderSafe 既有语义一致。
func sanitizeOutboundHeaderValues(req *http.Request) {
	if req == nil {
		return
	}
	for k, vs := range req.Header {
		for i, v := range vs {
			if s := HeaderSafe(v); s != v {
				vs[i] = s
			}
		}
		req.Header[k] = vs
	}
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	req = prepareOutbound(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return resp, err
	}
	// 我们在 CommonHeaders 里显式声明了 Accept-Encoding（关掉了 net/http 的透明解压），
	// 所以解码责任也在这里 —— 九个出站点一次覆盖，不会有谁忘了解。
	return decodeBodyIfNeeded(resp), nil
}

// randHex32 生成 32 hex 纯随机请求 ID。
//
// 真机 `/v2/report` 的 `X-Request-ID` 是这份形态（**无时间位**）——与 chat 的
// UUIDv7 不是同一套规则，不要混用。熵源故障时回落时间派生，仍恒 32 hex、不 panic、不空串。
func randHex32() string {
	var b [16]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		ms := time.Now().UnixMilli()
		return fmt.Sprintf("%012x%020x", ms, uint64(ms))
	}
	return hex.EncodeToString(b[:])
}

// randHex16 16 位 hex（span id 形态）。
func randHex16() string { return randHex32()[:16] }

// randUUID 生成 uuid 形态串（32 hex 分 8-4-4-4-12）。真机 acp-connection-id 是这个形状。
func randUUID() string {
	h := randHex32()
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// injectTraceHeaders 补 W3C / B3 链路头（官方 chat 四条恒发）：
//
//	traceparent:          00-<traceID 32>-<spanID 16>-01
//	b3:                   <traceID>-<spanID>-1-<parentSpanID>
//	X-B3-ParentSpanId:    <parentSpanID 16>
//	acp-connection-id:    uuid 形态
//
// 此前整族都缺 —— 对拍夹具里 4 条「漏发」就是它们。这些是链路追踪头，官方由
// undici/OTel 自动注入，每条请求都带。
func injectTraceHeaders(req *http.Request) {
	// 逐个头判空（审查 A1-18）：此前只要调用方设置了 traceparent 就整体 return，
	// b3 / X-B3-ParentSpanId / acp-connection-id 三条全部不再注入 —— 透传场景下
	// 头族只剩一条 traceparent，比「整族都缺」更像被中间件改写过，而头族一致性
	// 正是本项目反复强调的指纹面。
	//
	// 关键：调用方已给 traceparent 时，其余三条必须**沿用它的 trace id**，
	// 否则 traceparent 与 b3 指向两个不同的 trace，链路追踪直接断成两截
	// （比缺头更难查）。span id 仍是本跳自己的，符合 W3C/B3 语义。
	trace := randHex32()
	span := randHex16()
	if tp := req.Header.Get("traceparent"); tp != "" || req.Header.Get("Traceparent") != "" {
		if tp == "" {
			tp = req.Header.Get("Traceparent")
		}
		if parsed, ok := traceIDFromTraceparent(tp); ok {
			trace = parsed
		}
	}
	parent := randHex16()
	if req.Header.Get("traceparent") == "" && req.Header.Get("Traceparent") == "" {
		req.Header.Set("traceparent", "00-"+trace+"-"+span+"-01")
	}
	if req.Header.Get("b3") == "" {
		req.Header.Set("b3", trace+"-"+span+"-1-"+parent)
	}
	if req.Header.Get("X-B3-ParentSpanId") == "" {
		req.Header.Set("X-B3-ParentSpanId", parent)
	}
	if req.Header.Get("acp-connection-id") == "" {
		req.Header.Set("acp-connection-id", randUUID())
	}
}

// traceIDFromTraceparent 从 W3C traceparent 里取 32 hex 的 trace id。
// 形态：<version>-<trace-id>-<parent-id>-<flags>。非标准形态返回 ok=false，
// 调用方退回自己生成（宁可 id 不一致，也不要写一个非法 trace id 出去）。
func traceIDFromTraceparent(tp string) (string, bool) {
	parts := strings.Split(strings.TrimSpace(tp), "-")
	if len(parts) < 4 {
		return "", false
	}
	id := parts[1]
	if len(id) != 32 {
		return "", false
	}
	for _, r := range id {
		isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
		if !isHex {
			return "", false
		}
	}
	return strings.ToLower(id), true
}

// officialConnectionValue 给出该 path 上官方客户端实际发送的 Connection 值。
//
// 2026-10-30 双版本真机抓包（CN 5.9.3 / GL 5.9.4）：官方在每条 RPC 上都**显式**
// 写了 Connection —— 除 `/v3/config` 是 `close` 外，其余全是 `keep-alive`。
// HTTP/1.1 默认即复用，Go 因此从不发这一行；但头集合少了它就是少一个字段，
// 风控按白名单查头时，「缺一个官方必发的头」和「多一个官方不发的头」一样显眼。
//
// 没抓到过的 path 返回空串，保持 Go 默认行为，不臆造。
func officialConnectionValue(path string) string {
	switch path {
	case "/v3/config":
		return "close"
	case "/v2/chat/completions", "/v2/chat/busy", "/v2/report":
		return "keep-alive"
	}
	return ""
}

// applyConnectionHeader 把官方那个显式的 Connection 头补回去。
//
// `close` **不能**写进 Header：net/http 的 transferWriter 会按 req.Close 自己写
// 一行 `Connection: close`，手写会重复成两行。`keep-alive` 相反 —— Go 认为它是
// 默认值所以从不发出，必须手写才会出现在线上字节里。
func applyConnectionHeader(req *http.Request) {
	switch officialConnectionValue(req.URL.Path) {
	case "close":
		req.Close = true
	case "keep-alive":
		req.Header.Set("Connection", "keep-alive")
	}
}
