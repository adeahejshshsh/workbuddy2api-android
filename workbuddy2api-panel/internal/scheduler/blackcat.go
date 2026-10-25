// blackcat.go 夜猫子任务执行器：23:00–08:00 窗口内对池内账号补足 glm-5.2 对话
// 并上报事件链（black_cat 判据）。窗口外触发则直接跳过（只观测，不报错）。
package scheduler

import (
	"context"
	"github.com/linguo2625469/workbuddy2api-panel/internal/cst"
	"log"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// nowForNightWindow 夜猫子窗口判定所用时钟（生产恒为 time.Now）。
//
// 之所以留一个可替换点：窗口守卫**先于**账号循环，若不钉住时钟，「企业号门控是否生效」
// 这条断言的判别力就完全取决于运行环境的墙钟——本机白天跑等于该用例什么都没测，
// 只有 CI 恰好落在 UTC 夜里才真正执行。属典型的「随环境静默失效」的测试。
// 按北京时间取时钟（报告 #7）：排程时点已按 CST 算，若窗口判定仍用宿主时区，
// 非 CST 宿主上每天到点都会在窗口判定处被跳过。面板手动入口也用 cst.Now()，
// 两条路径必须同源。
var nowForNightWindow = cst.Now

// RunBlackcatNow 对所有可用账号执行夜猫子对话补足（窗口外跳过）。
// 由 blackcat_hours 排程（默认 [23]）触发；执行前二次校验 InNightWindow。
// RunBlackcatNowCtx 带 ctx 的夜猫子补足（bug #13）。
func (s *Scheduler) RunBlackcatNow() { s.RunBlackcatNowCtx(context.Background(), false) }

func (s *Scheduler) RunBlackcatNowCtx(ctx context.Context, spread bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !upstream.InNightWindow(nowForNightWindow()) {
		log.Printf("blackcat: 当前不在 23:00–08:00 计数窗口，跳过")
		return
	}
	plan := s.spreadPlan(taskBlackcat, spread, cst.Now())
	includeDisabled := s.includeDisabledInTasks()
	// 夜间参与上限（schedule.night_account_cap）：夜猫子是全任务体系里唯一「整任务
	// 都是真实模型对话」的，指纹最重 —— 深夜全池同 IP 齐做真实对话是最好认的机器人
	// 形状。>0 时候选收窄为积分最高的前 N 个（其余号夜里静默）；0 = 不限（旧行为）。
	// 先剔不参与的号再按积分取前 N（见 nightCandidates：反过来会让被禁用的高积分
	// 老号占满名额，夜猫子整晚静默）。在 spread 计划之前收窄：不参与的不占窗口。
	for _, st := range plan.order(nightCandidates(s.cfg.Pool.List(), includeDisabled, s.NightAccountCap())) {
		// 禁用账号门控（review #2）：签到/旅行/活跃/保活/成长队列都有这道门，
		// 只有夜猫子漏了 —— 被人工禁用或 12153 判死的号每天 23:00 仍会被发
		// 真实模型对话，烧额度还继续打上游。
		if st.Disabled && !includeDisabled {
			continue
		}
		// 账号级摊开（review #3）：只排序不等待等于没摊开，实际执行时刻仍是固定节拍。
		if !plan.wait(ctx, st.UID) {
			return
		}
		// 暂停选号（paused）账号跳过：夜猫子是全任务体系里**唯一「整任务都是真实模型
		// 对话」**的（RunNightChats 逐条 ChatStream 发 glm-5.2 短对话），与「让位防风控」
		// 正面冲突。旅行 / 成长任务 / 连登管家都走上报与领奖 RPC，照常参与
		// （吸收上游 b1a2284 的口径修正）。
		if st.Paused {
			continue // 暂停选号：夜猫子是唯一「整任务都是真实模型对话」的，让位防关联
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
			continue // 企业版门控：无成长体系（growth 403 / claim-gift 400「企业账号不支持该操作」）
		}
		need, err := s.cfg.Upstream.BlackcatNeed(a)
		if err != nil {
			log.Printf("blackcat %s: %v", logfmt.Label(a.UID, a.NicknameValue()), err)
			continue
		}
		if need <= 0 {
			continue
		}
		ok, err := s.cfg.Upstream.RunNightChatsCtx(ctx, a, int(need))
		if err != nil {
			log.Printf("blackcat %s: %d/%d 完成，中断: %v", logfmt.Label(a.UID, a.NicknameValue()), ok, need, err)
			continue
		}
		log.Printf("blackcat %s: 完成 %d 次夜间对话", logfmt.Label(a.UID, a.NicknameValue()), ok)
	}
}
