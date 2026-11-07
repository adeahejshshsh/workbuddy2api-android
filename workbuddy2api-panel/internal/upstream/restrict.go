// restrict.go 上游 11140「账号存在异常行为」的识别（风控连坐保护的判据）。
//
// 背景：11140 有三种形态，处置方向各不相同，必须分开：
//   - 英文 "request illegal"（auth_forbidden，授权/渠道校验）→ 既有 accountFaultMarkers
//     与 IsAccountRestricted 处置（见 client.go）；
//   - 账号风控限制文书（中文「账号存在异常行为，已被限制使用」/ 英文 "unusual activity"）
//     → 本文件。这一类判据在上游是**设备/出口级**，换账号只会连坐（hub #422 实测：
//     一次任务 7 个号全封）；
//   - 同一 code 也承载模型级限流（"The model provider is rate-limiting requests."）→
//     必须保持软限流语义。
//
// 为什么必须 code + 文案同时要求：只按 code 判会把限流转成账号终态（白扔一天）；
// 只按文案判会误伤正文里恰好讨论这些词的数据（用户的提示词、检索回来的网页）。
package upstream

import "strings"

// restrictionMarkers 风控限制文书的实测措辞（社区封禁复盘 + hub #422 的 403 样例）。
var restrictionMarkers = []string{
	"账号存在异常行为",
	"已被限制",
	"unusual activity",
}

// IsRestricted 报告响应信封是否为「账号被上游风控限制」（结构化 code 11140 + 限制文案）。
//
// 与 IsAccountRestricted 的分工：那个认的是 "request illegal"（授权形态），本函数认的是
// 限制文书（设备/出口形态）。两个都要，任缺其一都会让一类 11140 落进「只换号不罚号」。
func IsRestricted(body string) bool {
	if !hasBusinessCode(body, "11140") && !HasCodeMarker(body, "11140") {
		return false
	}
	lower := strings.ToLower(body)
	// 显式排除限流语义：11140 同时承载「模型级限流」，那种场景账号是健康的。
	// 本仓对「误禁用」的取舍一直是「宁可短冷却」（见 IsAccountRestricted 的同款排除）：
	// 一次误判的代价是禁用账号 + 就地停手 + 可能全池暂停 30 分钟，远高于一次软限流。
	for _, m := range softRateMarkers {
		if strings.Contains(lower, m) {
			return false
		}
	}
	for _, m := range restrictionMarkers {
		if strings.Contains(body, m) || strings.Contains(lower, m) {
			return true
		}
	}
	return false
}
