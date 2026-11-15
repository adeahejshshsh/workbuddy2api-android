// streak.go 连登管家：签到排程后自动检查连登兑换档位 → 可兑换即兑换 → 按抽奖次数抽奖。
//
// 背景（2026-09-12）：成长中心连登档位（7d/14d/28d）按连续登录天数解锁，兑换发
// credit/energy/补签卡/抽奖次数；抽奖次数只能从兑换获得。兑换按钮在 UI 上恒可点，
// 但未解锁时服务端 403「连续登录天数不足」——所以放在每日签到后跑一遍（幂等），
// 到天数那天自动完成「兑换 → 抽奖」闭环，无需人工盯。
package scheduler

import (
	"context"
	"encoding/json"
	"github.com/linguo2625469/workbuddy2api-panel/internal/cst"
	"log"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// maxLotteryDrawPerRound 单账号单轮抽奖次数的硬上限（审查 C7-054）。
const maxLotteryDrawPerRound = 50

// RunStreakBonusNow 对所有可用账号执行连登兑换 + 抽奖（幂等：locked/无次数自动跳过）。
// 由签到排程（RunCheckinNow）末尾调用；也可面板手动触发。
func (s *Scheduler) RunStreakBonusNow() { s.RunStreakBonusNowCtx(context.Background()) }

// RunStreakBonusNowCtx 带 ctx 与账号间节流的连登兑换（bug #14）。
//
// 此前全池零节流：签到有 800ms/号、旅行/活跃/夜猫子都有，唯独这里没有 ——
// 46 个账号一轮就是数百个请求连发（每号：补签查询 + streak + gift + compensation
// + redeem + lottery summary + N 次 draw），正是仓库多处专门防范的 WAF 频控形态。
func (s *Scheduler) RunStreakBonusNowCtx(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	first := true
	// 禁用号是否参与：与签到/活跃/保活/余额同口径（审查 C7-056）。此前恒跳过，
	// 打开 schedule.include_disabled_in_tasks 后连登兑换/抽奖对禁用号仍不执行。
	includeDisabled := s.includeDisabledInTasks()
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled && !includeDisabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if s.skipBehaviorTask(a) {
			continue
		}
		if a == nil || a.AccessTokenValue() == "" {
			continue
		}
		if a.IsGlobal() {
			continue // D4 门控：global 无 CN 任务体系，不发起任何上游调用
		}
		if !a.SupportsPersonalGrowth() {
			continue // 企业版门控：无成长体系（/activity/growth/streak 上游一律 403）
		}
		if !first && activityAccountDelay > 0 {
			if !sleepCtx(ctx, s.accountDelay(activityAccountDelay)) {
				return // 停机/取消：不再把剩余账号的节流全睡完
			}
		}
		first = false
		// 账号级族锁（审查 E4）：手动入口与排程签到末尾的这次调用会撞车，
		// 撞上就是同一档位重复兑换、同一批 chances 重复抽。
		if !s.TryBeginAccountTask(st.UID, TaskActivity) {
			continue
		}
		s.streakBonusAccount(a)
		s.EndAccountTask(st.UID, TaskActivity)
	}
}

// streakBonusAccount 单账号：补签保连登 → 礼包/补偿 → 兑换所有已解锁档位 → 抽完所有 chances。
func (s *Scheduler) streakBonusAccount(a *auth.Auth) {
	// 连登总览先取一次，后面补签与档位共用（审查 C7-057）：此前 makeupYesterday
	// 内部又调一次 GrowthStreakFull，46 个号每天白多 46 次公网往返 —— 正是本文件
	// 自述要防的「数百请求连发」形态。
	//
	// 副作用：这次查询失败时补签也一并跳过（此前补签有自己的一次查询）。两者走
	// 同一个上游端点，端点挂了补签本来也成功不了，少打一次反而更对。
	full, err := s.cfg.Upstream.GrowthStreakFull(a)
	if err != nil {
		log.Printf("streak-bonus %s: %v", logfmt.Label(a.UID, a.NicknameValue()), err)
		return
	}
	// 0. 补签保连登：昨日漏签且有补签卡则补上（连续天数一断就要重攒 7 天）。
	s.makeupYesterday(a, full)
	// 0.5 礼包（先查后领）：ClaimGift 对「已领过」也返回 (0, nil)——上游 200 且 body
	// 无 credit 字段，所以盲领写法会给每个已领号**每天**打一行假的「🎊 新手礼包 +0c」
	// （日志有、余额流水里 delta 一次都没有），还白打一次上游。改成先查状态：
	// active=false 是「当前无活动」的正常 200，不报错也不刷日志。
	if st, err := s.cfg.Upstream.GetGiftStatus(a); err != nil {
		log.Printf("streak-bonus %s: 礼包状态查询失败: %v", logfmt.Label(a.UID, a.NicknameValue()), err)
	} else if st.Pending() {
		if credit, cerr := s.cfg.Upstream.ClaimGift(a); cerr != nil {
			log.Printf("streak-bonus %s: 礼包领取失败（截止 %s，可领 %dc）: %v",
				logfmt.Label(a.UID, a.NicknameValue()), st.EndTime, st.CreditNum, cerr)
		} else {
			log.Printf("streak-bonus %s: 🎊 新手礼包 +%dc（截止 %s）",
				logfmt.Label(a.UID, a.NicknameValue()), credit, st.EndTime)
		}
	}
	// 先查后领（上游 PR #196）：直接 claim 会把「没有活动 / 已领 / 领取失败」
	// 压成同一个静默错误，带截止日的补偿会静默过期。
	if info, cerr := s.cfg.Upstream.CompensationStatus(a); cerr != nil {
		log.Printf("streak-bonus %s: 补偿状态查询失败: %v", logfmt.Label(a.UID, a.NicknameValue()), cerr)
	} else if info.Active && !info.Claimed {
		if credit, err := s.cfg.Upstream.ClaimCompensation(a); err != nil {
			log.Printf("streak-bonus %s: 补偿领取失败（截止 %s，可领 %dc）: %v",
				logfmt.Label(a.UID, a.NicknameValue()), info.EndTime, info.Credit, err)
		} else {
			log.Printf("streak-bonus %s: 🎊 补偿领取 +%dc（截止 %s）",
				logfmt.Label(a.UID, a.NicknameValue()), credit, info.EndTime)
		}
	}

	statuses := map[string]string{
		"7d":  full.RedemptionStatus.Tier7dStatus,
		"14d": full.RedemptionStatus.Tier14dStatus,
		"28d": full.RedemptionStatus.Tier28dStatus,
	}
	for _, tier := range full.RedemptionStatus.Tiers {
		status := statuses[tier.Tier]
		if status == "locked" || status == "claimed" {
			continue
		}
		if err := s.cfg.Upstream.GrowthRedeemTier(a, tier.Tier); err != nil {
			// 未解锁（403）属预期，静默；其余记日志。
			log.Printf("streak-bonus %s: redeem %s: %v", logfmt.Label(a.UID, a.NicknameValue()), tier.Tier, err)
			continue
		}
		// 用 logfmt.Label 保持全仓 uid8 口径（bug #10）：这一行原本直接打完整 UID，
		// 面板日志/归档里的账号标识格式不一致，grep 与对账都要多一步。
		log.Printf("streak-bonus %s: ★ 兑换 %s 档（+%dc +%de 卡×%d 抽奖×%d）",
			logfmt.Label(a.UID, a.NicknameValue()), tier.Tier, tier.Credit, tier.Energy, tier.Cards, tier.Chances)
	}
	// 抽奖：按当前 chances 全抽完（兑换刚发的次数已在服务端累加）。
	chances, err := s.cfg.Upstream.LotteryChances(a)
	if err != nil {
		log.Printf("streak-bonus %s: lottery summary: %v", logfmt.Label(a.UID, a.NicknameValue()), err)
		return
	}
	// 抽奖次数夹上限（审查 C7-054）：此前直接拿上游返回的 chances 当循环上界，
	// 字段异常/解析错位（例如回 10000）会让网关对同一个号连发上万次抽奖 —— 一次
	// 就把号打进上游风控。50 次/轮远超正常值（档位合计最多十几次），超出记 WARN
	// 并只抽前 50 次，剩下的留给下一轮（次数在服务端，不会丢）。
	if chances > maxLotteryDrawPerRound {
		log.Printf("WARN: streak-bonus %s: 上游返回抽奖次数 %d 超过单轮上限，只抽 %d 次",
			logfmt.Label(a.UID, a.NicknameValue()), chances, maxLotteryDrawPerRound)
		chances = maxLotteryDrawPerRound
	}
	if chances < 0 {
		chances = 0
	}
	for i := 0; i < chances; i++ {
		raw, err := s.cfg.Upstream.LotteryDraw(a)
		if err != nil {
			log.Printf("streak-bonus %s: draw: %v", logfmt.Label(a.UID, a.NicknameValue()), err)
			return
		}
		log.Printf("streak-bonus %s: 🎲 第%d抽 %s", logfmt.Label(a.UID, a.NicknameValue()), i+1, compactJSON(raw))
	}
	if chances > 0 {
		log.Printf("streak-bonus %s: 抽奖完成 %d 次", logfmt.Label(a.UID, a.NicknameValue()), chances)
	}
}

// compactJSON 裁剪奖品载荷（日志单行可读）。
func compactJSON(raw json.RawMessage) string {
	s := string(raw)
	if len(s) > 220 {
		// rune 边界（bug P2-2）：奖品载荷多是中文，按字节切日志输出乱码
		return logfmt.Truncate(s, 220) + "…"
	}
	return s
}

// makeupYesterday 昨日漏签且有补签卡时自动补签（保住连登连续天数）。
// 无卡 / 无漏签 / 查询失败均静默（不影响主流程）。
func (s *Scheduler) makeupYesterday(a *auth.Auth, full *upstream.StreakFull) {
	missed, err := s.cfg.Upstream.HeatmapYesterdayMissed(a)
	if err != nil || !missed {
		return
	}
	if full == nil || full.MakeupCards.Balance <= 0 {
		return
	}
	// 「昨天」按北京时间算（报告 #8）：heatmap 的日期单元是 CST 自然日，
	// 用宿主本地日界会在跨日窗口里查错单元（漏签误判、补签卡补到错误日期）。
	yesterday := cst.Now().AddDate(0, 0, -1).Format("2006-01-02")
	if err := s.cfg.Upstream.UseMakeupCard(a, yesterday); err != nil {
		log.Printf("streak-bonus %s: 补签 %s 失败: %v", logfmt.Label(a.UID, a.NicknameValue()), yesterday, err)
		return
	}
	log.Printf("streak-bonus %s: ★ 已用补签卡补签 %s（保连登）", logfmt.Label(a.UID, a.NicknameValue()), yesterday)
}
