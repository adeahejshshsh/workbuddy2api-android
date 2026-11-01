package upstream

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
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
func (c *Client) do(req *http.Request) (*http.Response, error) {
	// 头名大小写对齐官方真机（Go 的 CanonicalMIMEHeaderKey 会把
	// acp-connection-id 归一成 Acp-Connection-Id、X-B3-ParentSpanId 拍成
	// X-B3-Parentspanid —— 头集合与取值全对，逐字节看仍是另一族包）。
	// 逐名映射而非无差别小写化：后者会把 User-Agent 打回 Go-http-client/1.1。
	// 官方必发、Go 默认不发的那一行（见 applyConnectionHeader）
	applyConnectionHeader(req)
	req = normalizeHeaderSpelling(req)
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
	if req.Header.Get("traceparent") != "" || req.Header.Get("Traceparent") != "" {
		return // 调用方已显式设置（透传场景）不覆盖
	}
	trace := randHex32()
	span := randHex16()
	parent := randHex16()
	req.Header.Set("traceparent", "00-"+trace+"-"+span+"-01")
	req.Header.Set("b3", trace+"-"+span+"-1-"+parent)
	req.Header.Set("X-B3-ParentSpanId", parent)
	req.Header.Set("acp-connection-id", randUUID())
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
