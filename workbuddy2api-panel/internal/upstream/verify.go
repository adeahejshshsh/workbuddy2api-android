package upstream

// verify.go 网关 401 的**唯一**求证口径（上游 2026-10-24 `21a6e45`）。
//
// 同一个坑踩了两次：网关 401 的「封禁 / 瞬时拦截 / 登录失效」三种成因响应体完全
// 一样（APISIX/openresty 的 HTML 页，无业务信封），只能靠 refresh 的结果区分。
// chat 路径（issue #66）实现过一遍，面板探活又实现了一遍，判据各写各的。
// 收敛到这里之后，任何新加的 401 处理都走同一套口径。

import (
	"errors"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// VerifyVerdict 网关 401 的求证结论。
type VerifyVerdict int

const (
	// VerifyAlive 会话仍有效：401 属瞬时拦截（风控 / 边缘抖动 / 并发限流），不该罚。
	VerifyAlive VerifyVerdict = iota
	// VerifyDead 登录态确实失效：refresh 被上游应用层明确拒绝，需重登。
	VerifyDead
	// VerifyInconclusive 无明确判词：网络 / 5xx / 429 / refresh 端点自身也被拦截 /
	// 无 RT 可求证。既不能判活也不能判死 —— 计入 session dead 就等于换个地方复发
	// 同一个误杀。
	VerifyInconclusive
)

func (v VerifyVerdict) String() string {
	switch v {
	case VerifyAlive:
		return "alive"
	case VerifyDead:
		return "dead"
	default:
		return "inconclusive"
	}
}

// VerifySessionByRefresh 用 refresh 求证会话是否仍有效（网关 401 的唯一可靠判据）。
//
// 判据：
//   - refresh 成功                        → VerifyAlive（会话活，401 只是那次被拦）
//   - refresh 被应用层明确拒绝（4xx 业务） → VerifyDead（token 真失效）
//   - 其它（网络错 / 5xx / 429 / refresh 端点自己也被 WAF 拦 / 无 RT）→ VerifyInconclusive
//
// ⚠️ 第三条是关键：refresh 端点同样挂在 /v2/* 上，边缘拦截时它也回 HTML 页。
// 把「refresh 也失败」一律当死号，就等于在同一层再误杀一次。
//
// 调用方职责（本函数不做）：成功时落盘（上游会轮换 RT，不落盘重启即真死）、
// 按结论决定罚不罚。
func (c *Client) VerifySessionByRefresh(a *auth.Auth) VerifyVerdict {
	if c == nil || a == nil || a.RefreshTokenValue() == "" {
		return VerifyInconclusive
	}
	err := c.RefreshToken(a)
	if err == nil {
		return VerifyAlive
	}
	var re *Error
	if !errors.As(err, &re) {
		return VerifyInconclusive // 传输层错误：没有判词
	}
	// refresh 端点自身被拦截 / 上游抖动：都不是「token 失效」的证据。
	if re.Status >= 500 || re.Status == http.StatusTooManyRequests ||
		IsGatewayUnauthorized(re.Status, re.Msg) || IsWafBlocked(re.Status, re.Msg) {
		return VerifyInconclusive
	}
	return VerifyDead
}
