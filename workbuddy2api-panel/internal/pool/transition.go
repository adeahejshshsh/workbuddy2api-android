// 账号状态机迁移的唯一权威实现。
//
// entry 的「可选择性」由四个正交维度决定：禁用(disabled)、账号级冷却(until/coolKind)、
// 模型级冷却(modelCooldowns)、熔断(breakerUntil)。维度之间以「迁移原语」收拢，
// 禁止在其他文件散写这些字段——所有入口（applyErrorPolicy / refresh / keepalive /
// 签到 / 选号）对状态的改动都必须经本文件的原语或经 Cooldown/NoteError/NoteSuccess 等
// 封装（它们在持锁下调用本文件原语）。
//
// 迁移矩阵（事件 → 动作 → 字段）：
//
//	disabled           ← disableLocked（Disable / NoteSessionDead 达阈）
//	until/coolKind     ← Cooldown(CoolSoft/Hard) / CooldownSoftForModel 无解析分支
//	modelCooldowns     ← CooldownSoftForModel 有解析分支；被 disableLocked/Cooldown/clearCoolingLocked
//	                     清**路由部分**（6004 类；11102/11158 负缓存与 AuditOnly 台账保留，审查 C5-88）
//	                     （reviveCoolingLocked 不清——余额恢复不构成限流解除证据）
//	breakerUntil       ← recordBreakerFailureLocked（Cooldown/NoteError 喂入）；NoteSuccess 清
//	softStreak         ← Cooldown(CoolSoft)/CooldownSoftForModel；NoteSuccess 清（revive 保留：与余额无关）
//	sessionDeadFails   ← NoteSessionDead；ClearSessionDead/NoteSuccess/ReviveDisabled 清
//
// 关键正交性（疑点 4 修正）：
//   - 冷却域（until/coolKind/softStreak/modelCooldowns）与熔断器（fails/retryCount/
//     breakerUntil）正交：冷却管「近期被限流/余额耗尽」，熔断管「反复 5xx 失败」。
//     disableLocked 只清冷却域、不动熔断——禁用是授权/session 终态，不应覆盖熔断观测。
//   - clearCoolingLocked 是「冷却域归零」的单一来源，被 disableLocked 共用
//     （禁用是终态，冷却随之作废）。reviveCoolingLocked（签到/余额刷新解冻）**不再**
//     走全清：余额恢复只解冻 CoolHard，软限流与模型级台账各有自身恢复时刻
//     （详见 reviveCoolingLocked 注释）。
//
// 锁序契约（审查 C5-87）：本文件（以及 pool 包全部日志点）在**持 p.mu 写锁**时调用
// e.a.NicknameValue()，即固定锁序 **pool.mu → auth.mu**（单向）。因此：
//   - auth 侧**禁止**在持 a.mu 时回调任何 pool 方法（否则 ABBA 死锁，而禁用/熔断
//     是错误路径，正是最需要可用性的时刻）；
//   - 新增日志点时优先只用 e.a.UID（无锁字段），需要昵称时确认调用方仍在同一锁序内。
//
// 现状已核实安全：upstream 的令牌刷新是「快照 → 解锁 → 网络 IO → 再锁写回」，
// auth 不会在持锁时进入 pool。
package pool

import (
	"log"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// clearCoolingLocked 清冷却域：until/coolKind/softStreak 归零，模型级冷却只清
// **参与路由**的那部分（6004 类），reason 一并清空。熔断器（fails/retryCount/
// breakerUntil）不属冷却域，不动。
//
// 为什么不再整体置 nil（审查 C5-88）：modelCooldowns 里混着两类语义完全不同的条目 ——
//   - 6004 模型级限流：随账号冷却一起失效是合理的；
//   - 11102/11158 负缓存（"这个号就是用不了这个模型"）：这是**上游事实**，与本地
//     冷却无关，还带 Hits 退避历史。
//
// 原实现 `e.modelCooldowns = nil` 把两类一起抹掉：禁用 → ReviveDisabled 复活后，
// 该号立刻被派回「官方确定用不了」的模型，重新吃 11102 并从 6h→12h→24h 从头退避，
// 与 clearRoutingModelCooldownsLocked 刻意保留 11102 的口径正好相反。
// 现在复用同一函数，两个入口口径统一。
// 调用方必须已持有 p.mu。
func (e *entry) clearCoolingLocked() {
	e.until = time.Time{}
	e.coolKind = 0
	e.reason = ""
	e.softStreak = 0
	clearRoutingModelCooldownsLocked(e) // 只清 6004，保留 11102/11158 与 AuditOnly 台账
}

// disableLocked 禁用迁移：置 disabled 并清冷却域（禁用是比冷却更强的不可用终态）。
//
// 旧 Disable 只置 disabled+reason，不碰 until/modelCooldowns/softStreak，会出现
// 「disabled=true 但 cooling=true / 残留 modelCooldowns」的一致性问题——一个先被
// 硬冷却（到次日 04:00）再被禁用的账号会同时呈现两种状态。禁用后冷却无意义
// （账号已退出选号，冷却截止不再被读取），故一并清空。
//
// 熔断器保留：熔断是「连续 5xx 失败」信号（与授权/会话无关），禁用后再复活时
// 熔断观测仍有效，不应被禁用覆盖。
func (p *Pool) disableLocked(e *entry, reason string) {
	e.clearCoolingLocked()
	e.disabled = true
	e.paused = false // 禁用是比暂停更强的终态，二者不叠加（禁用后须 revive 才能复用）
	e.reason = reason
	// 记录禁用时刻：运维需要区分「刚刚被禁」和「挂了一周没人管」，
	// 两者处置优先级差很远（后者往往意味着没人看面板）。
	e.disabledAt = time.Now()
	p.dirty.Store(true)
	// 禁用是需人工 revive 的终态，日志里必须留痕：否则「号没了」只能靠轮询面板发现，
	// 事后排障连时刻与原因都查不到。
	log.Printf("WARN: [pool] 账号被禁用 acct=%s reason=%s（revive 前不再参与选号）",
		logfmt.Label(e.a.UID, e.a.NicknameValue()), reason)
}

// pauseLocked 暂停选号迁移：置 paused 使账号退出选号候选（healthy 判否），
// 但**不动任何惩罚域**——paused 是「临时让位」，账号本身健康，恢复后冷却/熔断
// 观测仍有效。与 disableLocked 的对比：disabled 清冷却域且须人工 revive（终态），
// paused 只置一个标志、随时可 resume（瞬时态）。
// 保号任务（scheduler）遍历只按 Disabled 过滤，故 paused 号天然继续参与签到/
// 活跃上报/保活/余额刷新——这正是「暂停选号但不掉保号」的实现基础。
func (p *Pool) pauseLocked(e *entry) {
	e.paused = true
	p.dirty.Store(true)
}

// resumeLocked 解除暂停选号（幂等）：只清 paused，账号若不在其它惩罚期即恢复可选。
func (p *Pool) resumeLocked(e *entry) {
	e.paused = false
	p.dirty.Store(true)
}

// reviveCoolingLocked 余额恢复解冻：只清**余额耗尽冷却**（CoolHard 的
// until/coolKind/reason）并更新 credits/creditsTotal。
//
// 不动 CoolSoft 软限流退避、softStreak 与 modelCooldowns（6004 模型级台账）：
// 后两者的恢复证据是上游重置墙钟到期或探测成功，不是「余额有钱」。余额刷新
// 周期任务（每 5 分钟）经 ReenableIfCredits 到达这里——若在此清空整个冷却域，
// 任何限流冷却的实际寿命都被压到一个刷新周期内：6004 台账被抹后撞限号被误判
// 健康、重新选中再撞 429，全池冷却保护形同虚设（两号池实测复现）。softStreak
// 亦保留：退避计数与余额无关，由 NoteSuccess（成功是最强恢复证据）或自然到期
// 收敛。硬冷却（CoolHard）的权威恢复证据正是余额恢复（remain>0），照旧解冻。
// 不动熔断器（fails/retryCount/breakerUntil）——签到成功只证明余额恢复与
// billing 通道健康，不证明 chat 通道健康。调用方必须已持有 p.mu。
func (p *Pool) reviveCoolingLocked(e *entry, credits, total int64) {
	e.credits = credits
	e.creditsTotal = total
	if e.coolKind == CoolHard {
		e.until = time.Time{}
		e.coolKind = 0
		e.reason = ""
	}
}
