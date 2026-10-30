// streak_extras.go 新手礼包 / 补偿 / 签到热力图 / 补签卡 等「非任务」接口。
//
// 原 blackcat.go 里的夜猫子部分（InNightWindow / BlackcatNeed / RunNightChats /
// DesktopDesignCanvasSequence）已随上游下架一并删除：官方 /v2/activity/growth/tasks
// 实测（29 账号 × 两种口径共 1018 行）里 black_cat 在任何账号的列表里都不存在 ——
// 不是过期（没有 valid_start/end），是下架。留着只会每晚白烧真实对话 + 事件链。
package upstream

import (
	"encoding/json"
	"github.com/linguo2625469/workbuddy2api-panel/internal/cst"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// ClaimGift 领取新手礼包（每号一次，已领返回业务错误）。
func (c *Client) ClaimGift(a *auth.Auth) (int64, error) {
	data, err := c.billingJSON(a, http.MethodPost, "/billing/meter/claim-gift", map[string]any{})
	if err != nil {
		return 0, err
	}
	var resp struct {
		Credit int64 `json:"credit"`
	}
	_ = json.Unmarshal(data, &resp)
	return resp.Credit, nil
}

// ClaimCompensation 领取活动补偿（有则领，无则业务错误）。
func (c *Client) ClaimCompensation(a *auth.Auth) (int64, error) {
	data, err := c.billingJSON(a, http.MethodPost, "/billing/meter/claim-compensation", map[string]any{})
	if err != nil {
		return 0, err
	}
	var resp struct {
		Credit int64 `json:"credit"`
	}
	_ = json.Unmarshal(data, &resp)
	return resp.Credit, nil
}

// HeatmapYesterdayMissed 检查昨日是否漏签（heatmap cell score==0）。
func (c *Client) HeatmapYesterdayMissed(a *auth.Auth) (bool, error) {
	// 同上（报告 #8）：与 heatmap 的 CST 日期单元同源。
	yesterday := cst.Now().AddDate(0, 0, -1).Format("2006-01-02")
	data, err := c.growthJSON(a, http.MethodGet, "/activity/growth/heatmap", nil)
	if err != nil {
		return false, err
	}
	var resp struct {
		Cells []struct {
			Date  string `json:"date"`
			Score int    `json:"score"`
		} `json:"cells"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return false, err
	}
	for _, cell := range resp.Cells {
		if len(cell.Date) >= 10 && cell.Date[:10] == yesterday {
			return cell.Score == 0, nil
		}
	}
	return false, nil
}

// UseMakeupCard 对指定日期使用补签卡（保住连登连续天数；无卡返回业务错误）。
func (c *Client) UseMakeupCard(a *auth.Auth, date string) error {
	_, err := c.growthJSON(a, http.MethodPost, "/activity/growth/makeup-cards/use",
		map[string]any{"target_date": date})
	return err
}

// CompensationInfo 官方补偿活动的状态（上游 PR #196）。
type CompensationInfo struct {
	Active  bool   `json:"active"`   // false = 当前无补偿活动（正常 200，不算错误）
	Claimed bool   `json:"claimed"`  // 本账号已领
	Credit  int64  `json:"credit"`   // 可领积分数
	EndTime string `json:"end_time"` // 领取截止（RFC3339 或 "2006-01-02 15:04:05"）
}

// CompensationStatus 查询官方补偿活动状态（上游 PR #196「先查后领」）。
//
// 为什么必须先查：旧实现直接 claim-compensation，把「没有活动 / 已领过 / 领取失败」
// 三态压成同一个业务错误静默跳过 —— 带截止日的补偿会**静默过期**，
// 而「查询失败」与「没有补偿」也分不开，面板上完全看不出来。
func (c *Client) CompensationStatus(a *auth.Auth) (CompensationInfo, error) {
	var info CompensationInfo
	data, err := c.billingJSON(a, http.MethodPost, "/billing/meter/compensation-status", map[string]any{})
	if err != nil {
		return info, err
	}
	var resp struct {
		Active    bool   `json:"active"`
		Claimed   bool   `json:"claimed"`
		CreditNum int64  `json:"credit_num"`
		EndTime   string `json:"end_time"`
		Validity  string `json:"validity_period"`
	}
	_ = json.Unmarshal(data, &resp)
	info.Active = resp.Active
	info.Claimed = resp.Claimed
	info.Credit = resp.CreditNum
	info.EndTime = resp.EndTime
	if info.EndTime == "" {
		info.EndTime = resp.Validity
	}
	return info, nil
}
