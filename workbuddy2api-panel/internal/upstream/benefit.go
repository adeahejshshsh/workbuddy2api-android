// benefit.go 官方「福利」类活动的状态查询（新手礼包）。
//
// 背景（2026-10-16 cn 域实测）：ClaimGift 对「已领过」也返回 (0, nil)——上游 200 且
// body 无 credit 字段，于是盲领写法 `if credit, err := ...; err == nil { log }` 会给每个
// 已领号每天打一行假的「🎊 新手礼包 +0c」（线上 2026-09-16~10-16 实测：日志有、余额流水
// 里 delta 一次都没有）。本文件补上「先查状态再决定是否领」所缺的那一半。
//
// 曾经的「官方补偿」也走这套同构接口，活动已于 2026-10 结束，整条链路（状态查询 +
// 领取 + 排程 + 面板卡片）一并下线（见 commit 说明）；下面是保留下来的礼包形态。
//
// 上游口径（实测，CLI 域与 Web 域返回一致）：
//
//	{"code":0,"data":{"claimed":true,"claimed_at":"2026-09-24 18:15:15","active":true,
//	   "credit_num":1500,"validity_period":1,"start_time":"2026-04-01 00:00:00",
//	   "end_time":"2026-10-31 23:59:59"}}
//
// active=false 是「当前无活动」的**正常 200**，不是错误——故本接口只在传输失败 /
// 非 2xx / 业务 code 非 0 时报错，否则没活动的账号会把日志刷成一片错误。
package upstream

import (
	"encoding/json"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// giftStatusPath 新手礼包状态端点（**无 /v2 前缀**，带 /v2 是 404）。
const giftStatusPath = "/billing/meter/check-gift-claimed"

// BenefitStatus 单账号的礼包活动状态。
type BenefitStatus struct {
	Active    bool  `json:"active"`     // 当前是否有生效中的活动（false = 无活动，正常返回）
	Claimed   bool  `json:"claimed"`    // 本次活动是否已领取
	CreditNum int64 `json:"credit_num"` // 可领额度（积分）
	// ValidityPeriod 发放积分的有效期倍数（上游未明示单位，前端也不渲染）：
	// 只透传不换算，避免臆造语义。
	ValidityPeriod int64 `json:"validity_period"`
	// EndTime 领取截止（上游为 "2006-01-02 15:04:05" 无时区字符串）。
	EndTime string `json:"end_time"`
}

// Pending 是否存在「可领且未领」的额度——调 ClaimGift 的前置判据。
// 三与门：active=false 是没有活动；claimed=true 是已领完（再调只拿业务错误）；
// credit_num<=0 是活动在但无额度。三者都归「无需领取」。
// 排程与面板共用本判据（唯一权威实现，避免同一个与门散落成多份拷贝）。
func (s BenefitStatus) Pending() bool {
	return s.Active && !s.Claimed && s.CreditNum > 0
}

// GetGiftStatus 查新手礼包状态（每号一次、带有效期）。
func (c *Client) GetGiftStatus(a *auth.Auth) (BenefitStatus, error) {
	return c.benefitStatus(a, giftStatusPath)
}

// benefitStatus 福利状态查询：走 billingJSON（billingBase），与领取端点同域同头。
func (c *Client) benefitStatus(a *auth.Auth, path string) (BenefitStatus, error) {
	data, err := c.billingJSON(a, http.MethodPost, path, map[string]any{})
	if err != nil {
		return BenefitStatus{}, err
	}
	var st BenefitStatus
	if err := json.Unmarshal(data, &st); err != nil {
		return BenefitStatus{}, err
	}
	return st, nil
}
