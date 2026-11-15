// webactivity.go 网页通道每日活跃腿（仅国际版）。
//
// 为什么单开一条腿（issue #111 实测）：国际版官方每日活跃奖励只认**跑完的网页
// agent 会话**。桌面身分的 chat/completions 与 /v2/report 都只点亮 growth 连登，
// 30/50 每日积分不动。链路实现见 upstream/webagent.go。
//
// 三条纪律：
//
//  1. 只在 global 账号上跑。国内版没有这套「网页会话换积分」的机制，对 CN 号建
//     云会话纯属白花钱（CN 侧每日活跃走签到 + 任务中心）。
//  2. 每天每号最多一轮。activity_hours 配了多个时点时，runActivity 会跑多轮；
//     重复建会话既浪费上游名额（单号活跃会话上限 10 条）又没有额外收益。
//     记的是「本进程内今天已成功」，重启会重新允许一轮——比落盘简单，且代价只是
//     一天多一条会话（上游按天判定活跃，多跑不加分）。
//  3. 收尾必须发生。会话跑完 stop（保留在网页端历史里），失败 stop+delete
//     （失败轮次没有保留价值，免得历史里堆满「Hi」；不删会一直占活跃名额，
//     攒满 10 条就再也建不出会话）。
package scheduler

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/cst"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// webActivityBudget 单账号网页腿的总预算：建会话 + 等沙箱 + 跑一轮 + 收尾。
// 一轮「Hi」实测约 7 秒；upstream.RunWebAgentTurn 自己还有 120 秒的内部上限，
// 这里再兜一层，避免沙箱卡死时拖住整轮巡检（activity 是遍历型任务）。
const webActivityBudget = 180 * time.Second

// webActivityBatchBudget 一轮 activity 遍历里网页腿的**整批**预算（审查 C7-070）。
//
// 单号 180s + 收尾 30s，且这条腿在 activity 的遍历循环里串行执行：46 个国际号
// 最坏 46×210s ≈ 2.7 小时，期间 TaskActivity 族锁一直被占着 → 下一个 activity
// 时点整轮被「上一轮仍在执行」静默跳过（叠加 C7-004 的丢轮）。10 分钟 ≈ 3 个号
// 跑满预算，剩下的账号留给下一个时点/次日——每日活跃是「今天跑过就算数」的
// 语义，少跑几个号不影响已经跑完的那些。
var webActivityBatchBudget = 10 * time.Minute

// webLegDay 记录「本进程内该账号今天已在网页通道成功过」的日期（CST）。
// 键 = uid，值 = 2026-10-29 形态的自然日。
type webLegDay struct {
	mu   sync.Mutex
	done map[string]string
}

// webActivityDoneToday 今天是否已经成功过（跨零点自动失效）。
func (s *Scheduler) webActivityDoneToday(uid string) bool {
	s.webLeg.mu.Lock()
	defer s.webLeg.mu.Unlock()
	d, ok := s.webLeg.done[uid]
	return ok && d == cst.Now().Format("2006-01-02")
}

// markWebActivityDone 记下今天已成功。
func (s *Scheduler) markWebActivityDone(uid string) {
	s.webLeg.mu.Lock()
	defer s.webLeg.mu.Unlock()
	if s.webLeg.done == nil {
		s.webLeg.done = make(map[string]string, 8)
	}
	day := cst.Now().Format("2006-01-02")
	// 写入时顺手清掉非今日条目（审查 C7-071）：这张表只用来判「今天跑没跑」，
	// 旧日期的记录没有任何用处，而它此前只写不删 —— 长期运行 + 账号轮换会让
	// map 随「历史出现过的 uid 数」单调增长。
	for k, v := range s.webLeg.done {
		if v != day {
			delete(s.webLeg.done, k)
		}
	}
	s.webLeg.done[uid] = day
}

// webActivityBatchDeadline 本批网页腿的截止时刻；腿被关掉时返回零值（= 不跑）。
func (s *Scheduler) webActivityBatchDeadline(now time.Time) time.Time {
	if s.webActivityDisabled() {
		return time.Time{}
	}
	return now.Add(webActivityBatchBudget)
}

// runWebActivityLegWithin 在**整批预算**内跑一条腿（审查 C7-070）。
//
// deadline 是批级截止时刻：单号自己的 180s 预算照旧，但不会越过整批截止 ——
// 否则「预算用完」要等当前这个号跑满 3 分钟才生效。
func (s *Scheduler) runWebActivityLegWithin(ctx context.Context, a *auth.Auth, deadline time.Time) {
	if !deadline.IsZero() {
		if !time.Now().Before(deadline) {
			return
		}
		lctx, cancel := context.WithDeadline(ctx, deadline)
		defer cancel()
		ctx = lctx
	}
	s.runWebActivityLeg(ctx, a)
}

// runWebActivityLeg 网页通道每日活跃：建会话 → 接沙箱跑完一轮 → 收尾。
//
// 全程失败都不上抛、不 NoteError：这条腿是每日活跃的旁路，与 chat 通道的账号健康
// 无关（同 /v1/models 拉取失败不罚号的理由）。失败只落一行 WARN 供排查。
func (s *Scheduler) runWebActivityLeg(ctx context.Context, a *auth.Auth) {
	if a == nil || s.cfg.Upstream == nil {
		return
	}
	// 国内版没有这条机制（见文件头第 1 条）。
	if !a.IsGlobal() {
		return
	}
	if a.AccessTokenValue() == "" || a.UID == "" {
		return
	}
	if s.webActivityDoneToday(a.UID) {
		log.Printf("web activity %s: 今天已跑完网页会话，跳过", logfmt.Label(a.UID, a.Nickname))
		return
	}

	label := logfmt.Label(a.UID, a.Nickname)
	up := s.cfg.Upstream
	lctx, cancel := context.WithTimeout(ctx, webActivityBudget)
	defer cancel()

	conv, err := up.CreateWebConversation(lctx, a, "", "")
	if err != nil {
		log.Printf("WARN: web activity %s: 建会话失败: %v", label, err)
		return
	}
	keep := false
	defer func() {
		// 收尾不带 lctx：主 ctx 已超时/取消时也要把会话停掉，否则活跃名额泄漏。
		cleanCtx, cleanCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cleanCancel()
		if err := up.StopWebConversation(cleanCtx, a, conv); err != nil {
			log.Printf("WARN: web activity %s: 结束会话 %s 失败: %v", label, conv, err)
		}
		if !keep {
			if err := up.DeleteWebConversation(cleanCtx, a, conv); err != nil {
				log.Printf("WARN: web activity %s: 删除会话 %s 失败: %v", label, conv, err)
			}
		}
	}()

	sess, err := up.WebConversationSession(lctx, a, conv)
	if err != nil {
		log.Printf("WARN: web activity %s: 取沙箱失败: %v", label, err)
		return
	}
	res, err := up.RunWebAgentTurn(lctx, a, conv, sess, "", "")
	if err != nil {
		log.Printf("WARN: web activity %s: 会话未跑完（status=%s, %d 段输出, %d ms）: %v",
			label, res.Status, res.Chunks, res.Elapsed.Milliseconds(), err)
		return
	}
	keep = true
	s.markWebActivityDone(a.UID)
	if res.SetModelFailed {
		// 会话跑完了，但没切到 deepseek-v4.1-flash（免费档）——本轮按账号默认模型
		// 计费。不判失败（活跃已算数），但必须能在日志里看见，否则「每天悄悄扣钱」。
		log.Printf("WARN: web activity %s: 会话已跑完（%d 段输出, %d ms），但 set_model 失败——本轮按默认模型计费",
			label, res.Chunks, res.Elapsed.Milliseconds())
		return
	}
	log.Printf("web activity %s: 网页会话跑完（%d 段输出, %d 事件, %d ms, 会话 %s）",
		label, res.Chunks, res.Events, res.Elapsed.Milliseconds(), conv)
}
