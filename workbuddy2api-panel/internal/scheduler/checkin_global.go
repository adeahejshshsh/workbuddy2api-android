// checkin_global.go 国际版（global realm）每日签到。
//
// 2026-10-29 复验（真实 global 账号直连 www.workbuddy.ai）：
//
//	POST /billing/meter/checkin-activity-status → 200，active=false、
//	     today_checked_in=false、daily_credit=100、start/end 全空
//	POST /v2/billing/meter/daily-checkinᅠ      → 400 code=10001「签到活动未开启或已过期」
//	国际客户端 main/account.js 里有 checkinStatus() / claimDailyCheckin() 两个方法
//
// 结论：**端点存在、客户端也有，活动本身当前未开放**。所以国际版不该像 CN 那样
// 盲发写请求（每天必得一次 10001，多一次风控面），而是先读状态、只在
// 「active 且当天未签」时才发 claim。
//
// 与 CN 分档的差别（CN 分支见 scheduler.go 的 runCheckin）：
//
//	CN   先查状态（只读，失败 fail-open 照常签到）→ 签到 → 余额分桶 → 连登兑换抽奖
//	国际 先查状态（失败**直接返回**，不猜）→ active 且未签才签到
//
// 国际版不跑连登礼包（RunStreakBonusNow 走 CN web base，国际号不发）。
package scheduler

import (
	"log"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// checkinGlobal 国际版签到：状态查询 → （仅 active 且未签）claim。
//
// 状态查询失败时不发写请求：国际版活动常态关闭，写请求的成功率完全取决于
// 服务端是否配了活动；网络/鉴权出错时盲发只会把一个可诊断的读失败变成两个错误。
// 这也与 CN 的 fail-open 形成对比——CN 的 fail-open 是因为「活动确实常开、盲签
// 幂等无害」，前提不同，故策略不同。
func (s *Scheduler) checkinGlobal(a *auth.Auth) {
	// 企业版无签到体系：上游对 daily-checkin 直接 400 10001「企业账号不支持该操作」。
	// 与 CN 分支同一个门控（SupportsPersonalEndpoints）。
	if !a.SupportsPersonalEndpoints() {
		return
	}
	label := logfmt.Label(a.UID, a.Nickname)
	act, err := s.cfg.Upstream.GetCheckinActivityStatus(a)
	if err != nil {
		log.Printf("WARN: checkin %s: 国际版活动状态查询失败（不发签到请求）: %v", label, err)
		return
	}
	if !act.Active {
		// 常态：国际版活动未配置。一行 INFO 级别的显形（只在到点时打一次），
		// 让人能确认「没签是因为活动没开」而不是「网关忘了签」。
		log.Printf("checkin %s: 国际版签到活动未开启（每日 %d 积分），跳过", label, act.DailyCredit)
		return
	}
	if act.TodayCheckedIn {
		// 上游 today_checked_in 是权威事实（本机 checkin_done 只记我们自己签的）。
		s.cfg.Pool.NoteCheckinDone(a.UID)
		log.Printf("checkin %s: 国际版今天已签到（连登 %d 天，每日 %d 积分）", label, act.StreakDays, act.DailyCredit)
		return
	}
	if err := s.cfg.Upstream.DailyCheckin(a); err != nil {
		if upstream.IsAlreadyCheckin(err) {
			// 幂等：并发/重复触发时上游回「今天已签到」，语义是成功。
			s.cfg.Pool.NoteCheckinDone(a.UID)
			log.Printf("checkin %s: 国际版今天已签到（幂等）", label)
			return
		}
		log.Printf("WARN: checkin %s: 国际版签到失败: %v", label, err)
		return
	}
	s.cfg.Pool.NoteCheckinDone(a.UID)
	log.Printf("checkin %s: 国际版签到成功（每日 %d 积分）", label, act.DailyCredit)
}
