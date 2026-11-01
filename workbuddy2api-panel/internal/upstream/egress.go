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
