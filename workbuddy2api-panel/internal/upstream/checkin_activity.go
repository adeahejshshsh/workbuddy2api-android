// checkin_activity.go 签到活动状态查询（POST /billing/meter/checkin-activity-status）。
//
// 逆向来源：HanawaBanana/workbuddy2api（延续上游 Sliverkiss 的副本）的
// internal/upstream/checkin_activity.go；路径与 daily-checkin 同族，cn 域带 /v2 前缀
// （实测无前缀也通，global 口径按 checkinMeterPaths 的老规矩先行无前缀、404 再回落）。
//
// 线上实测（2026-10-16，CN）：
//
//	{"code":0,"msg":"OK","data":{"active":true,"today_checked_in":true,"streak_days":17,
//	   "daily_credit":100,"today_credit":100,"is_streak_day":false,"next_streak_day":0,
//	   "streak_bonus_days":0,"streak_bonus_credit":0,"checkin_dates":["2026-10-16",...],
//	   "week_checkin_days":5,"week_progress":[true,...],"total_credits":1700,
//	   "start_time":"2026-10-01 00:00:00","end_time":"2026-10-31 23:59:59"}}
//
// 为什么值得一查：本机 pool 的 checkin_done 只记录**我们自己**发起的签到，用户从手机 /
// 官方客户端签过就看不到；上游的 today_checked_in 才是权威事实。且活动带截止日，
// 停摆（关机 / 排程关）会静默过期——active=false 与 end_time 让这件事提前显形。
// 本方法只读、无副作用，调用方失败只记日志（不得据此跳过签到）。
package upstream

import (
	"encoding/json"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const (
	checkinActivityPath   = "/billing/meter/checkin-activity-status"
	checkinActivityPathV2 = "/v2/billing/meter/checkin-activity-status"
)

// CheckinActivity 签到活动状态（只解析调用方要用的字段；数组字段刻意不收）。
type CheckinActivity struct {
	Active          bool   `json:"active"`            // 活动是否生效（false = 已结束/未开始）
	TodayCheckedIn  bool   `json:"today_checked_in"`  // 今天是否已签到（权威口径）
	StreakDays      int64  `json:"streak_days"`       // 当前连续签到天数
	DailyCredit     int64  `json:"daily_credit"`      // 每日签到积分
	TodayCredit     int64  `json:"today_credit"`      // 今天已到账积分
	WeekCheckinDays int64  `json:"week_checkin_days"` // 本周已签天数
	TotalCredits    int64  `json:"total_credits"`     // 活动累计积分
	EndTime         string `json:"end_time"`          // 活动截止（"2006-01-02 15:04:05"，无时区）
}

// checkinActivityPaths 与 checkinMeterPaths 同口径：global 先无 /v2 前缀、404 再回落；
// cn 单路径（有 /v2）。
func (c *Client) checkinActivityPaths(a *auth.Auth) []string {
	if c.globalOn(a) {
		return []string{checkinActivityPath, checkinActivityPathV2}
	}
	return []string{checkinActivityPathV2}
}

// GetCheckinActivityStatus 查签到活动状态。走 billingMeterJSON（与 daily-checkin 同域同头
// 同 fallback 语义）；只读，不参与任何账号惩罚。
func (c *Client) GetCheckinActivityStatus(a *auth.Auth) (CheckinActivity, error) {
	data, err := c.billingMeterJSON(a, c.checkinActivityPaths(a), http.MethodPost, map[string]any{})
	if err != nil {
		return CheckinActivity{}, err
	}
	var st CheckinActivity
	if err := json.Unmarshal(data, &st); err != nil {
		return CheckinActivity{}, err
	}
	return st, nil
}
