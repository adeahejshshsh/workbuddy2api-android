// debugdump.go WB2A_DEBUG_CHAT 的请求头调试输出（审查 W1-01）。
//
// 为什么单独抽出来：排障探针只该看到「形状」，不该看到凭据。此前那段代码
// `for k := range req.Header` 把**全部**头打进 stdout —— Authorization: Bearer
// <accessToken> 与 X-Device-Token 明文落在 stdout，容器里就是 docker logs；
// 一次日志导出/截图等于交出整个账号池。而且 X-Device-Token 是设备级凭据，
// 泄漏面比单个账号更大。
//
// 现在两条硬规则：
//  1. 只有白名单里的头会打印 —— 新增头默认不打印。黑名单式（只挡已知敏感头）
//     必然会在「上游又加了一个凭据头」时漏，白名单漏了顶多排障少一行；
//  2. Authorization 只打「方案 + 前 8 位 + 长度」，足够判断「是不是同一个号 /
//     有没有带 / 换了没有」，不足以复原。
package upstream

import (
	"fmt"
	"io"
	"net/http"
	"strings"
)

// debugChatHeaderAllow 调试日志允许打印的请求头（白名单，全为非凭据的形态头）。
//
// 刻意**不含** Authorization / X-Device-Token / Cookie / X-User-Id / X-Enterprise-Id
// / X-Domain：前三个是凭据，后三个是账号可识别信息（uid 能直接关联到具体账号，
// 排障时用日志里的请求号就够了）。
var debugChatHeaderAllow = []string{
	"Content-Type",
	"Accept",
	"X-Request-ID",
	"X-Conversation-ID",
	"X-Agent-Intent",
	"X-Agent-Type",
	"X-IDE-Name",
	"X-IDE-Version",
	"X-Product",
	"X-Private-Data",
}

// debugMaskKeep 打码保留的前缀长度（够看清「同一把 token 的前缀」，不足以复原）。
const debugMaskKeep = 8

// maskDebugSecret 打码一个凭据值：前 8 位 + 长度。
//
// 短值（<= 2×保留位）连前 8 位也不给：那种长度下「前 8 位」已经是大半把密钥，
// 只剩长度这一个可用信息。
func maskDebugSecret(v string) string {
	if v == "" {
		return ""
	}
	if len(v) <= debugMaskKeep*2 {
		return fmt.Sprintf("*** (len=%d)", len(v))
	}
	return fmt.Sprintf("%s… (len=%d)", v[:debugMaskKeep], len(v))
}

// maskAuthorization 打码 Authorization：保留认证方案（Bearer/Basic…），只遮凭据本体。
//
// 直接对整串取前 8 位会打出 "Bearer x" —— 方案占满了保留位，等于什么也没遮。
func maskAuthorization(v string) string {
	scheme := ""
	token := v
	if i := strings.IndexByte(v, ' '); i > 0 {
		scheme = v[:i+1]
		token = strings.TrimSpace(v[i+1:])
	}
	return scheme + maskDebugSecret(token)
}

// dumpDebugChatRequest 输出一次调试用的请求摘要（URL + 白名单头 + 打码后的 Authorization）。
func dumpDebugChatRequest(w io.Writer, req *http.Request) {
	if w == nil || req == nil {
		return
	}
	fmt.Fprintf(w, "[dbg] URL=%s\n", req.URL)
	for _, k := range debugChatHeaderAllow {
		if v := req.Header.Get(k); v != "" {
			fmt.Fprintf(w, "[dbg] %s: %s\n", k, v)
		}
	}
	if v := req.Header.Get("Authorization"); v != "" {
		fmt.Fprintf(w, "[dbg] Authorization: %s\n", maskAuthorization(v))
	}
}
