// backoff.go 轮转退避与抖动的单一事实来源（WAF 403 修复 P0-2/P0-1 共用）：
// 指数基数/封顶/抖动比例一处定义，chatCompletions 轮转退避与 WAF 软冷却基数
// （handler.wafCooldownBase）共用同一 jitterDur，不在 handler 与 upstream 各写一份。
package server

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// rotateBackoffBase 轮转退避基数（对齐官方 intl CLI computeRequestRetryDelayMs
// 的 500ms 形态，WAF 403 并发研究报告 §2.1/§6 P0-2）。测试可置 0 跳过等待
// （TestMain 已置 0 加速轮转测试；退避界断言测试临时恢复）。
var rotateBackoffBase = 500 * time.Millisecond

const (
	// rotateBackoffCap 轮转退避封顶（报告 P0-2 建议 8s：轮转上限默认 3 次，
	// 实际等待序列 500ms/1s，封顶只约束极端配置下的 MaxRotate）。
	rotateBackoffCap = 8 * time.Second
	// jitterFraction 抖动比例（±25%，对齐 intl CLI delay×(1±0.25) 形态）。
	jitterFraction = 0.25
)

// jitterDur 给时长施加 ±jitterFraction 的均匀抖动，返回 [d·(1-f), d·(1+f)] 区间值。
// d<=0 原样返回（零等待不抖动）。抖动目的是打散多请求同相位重试（WAF 频控按
// 密度判罚，齐步走的退避会以固定周期再次聚团）。
func jitterDur(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	f := 1 + (rand.Float64()*2-1)*jitterFraction
	out := time.Duration(float64(d) * f)
	if out < 0 {
		return 0
	}
	return out
}

// backoffAfter 返回第 n 次轮转（0 基：首次失败换号前 n=0）前应等待的退避时长：
// base·2^n 封顶 rotateBackoffCap，再施加 ±25% 抖动。base 置 0（测试）时恒 0。
// 用逐次翻倍而非位移：base 调整后无需同步维护移位上限，溢出由封顶比较兜底。
func backoffAfter(n int) time.Duration {
	d := rotateBackoffBase
	if d <= 0 {
		return 0
	}
	for k := 0; k < n && d < rotateBackoffCap; k++ {
		d *= 2
		if d <= 0 { // 翻倍溢出成非正数：直接按封顶处理
			return jitterDur(rotateBackoffCap)
		}
	}
	if d > rotateBackoffCap {
		d = rotateBackoffCap
	}
	return jitterDur(d)
}

// sleepCtx 可取消的等待：ctx 取消立即返回 false（客户端断连/优雅停机不必等退避
// 睡醒），等满返回 true。d<=0 立即放行。与 scheduler.sleepCtx 同模式（该函数未
// 导出且 scheduler 不宜被 server 反向依赖，按任务书「等价物」口径在消费侧实现）。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// egressBackoffBase 出口级瞬时失败的退避基数（1s 起、翻倍、封顶 15s、±25% 抖动）。
//
// 与轮转退避的区别是**记账对象**不同，不是同一件事换个数字：轮转退避是「换号前
// 歇一下，让上游频控窗口滑过」；本阶梯是「这条出口连续失败 N 次，整条链路歇一下」
// ——直连出口被本域全部账号共用，换号换不到别的链路。测试置 0 跳过真实等待。
var egressBackoffBase = 1 * time.Second

const (
	// egressBackoffCap 出口级退避封顶。
	egressBackoffCap = 15 * time.Second
	// egressGiveUpStreak 连续失败到这么多次就不再换号重试：链路已经证明是断的，
	// 换号只是把同样的等待与失败重复一遍（同一出口）。4 次刚好走完 1s/2s/4s
	// 三级阶梯——累计约 7 秒，对客户端可忍，对池子是「别把整池号一个个拖进惩罚」
	// 的分界线。
	egressGiveUpStreak = 4
)

// egressBackoff 出口连续失败 streak 次后应等待的时长：base·2^(streak-1) 封顶
// egressBackoffCap，再施加 ±25% 抖动（与轮转退避同一 jitterDur，单一来源）。
// base<=0（测试）恒 0。
func egressBackoff(streak int, base time.Duration) time.Duration {
	if base <= 0 || streak <= 0 {
		return 0
	}
	d := base
	for k := 1; k < streak && d < egressBackoffCap; k++ {
		d *= 2
		if d <= 0 {
			return jitterDur(egressBackoffCap)
		}
	}
	if d > egressBackoffCap {
		d = egressBackoffCap
	}
	return jitterDur(d)
}

// busyWaitCap 「等一个名额 / 等模型解封」这类等待的硬上限。
var busyWaitCap = 10 * time.Second

// slotPoll 等在途名额的轮询间隔：名额是按请求收尾释放的，几十毫秒内必有一次，不必更密。
const slotPoll = 50 * time.Millisecond

// waitForSlot 在「号都在途占满」时等一个空名额：轮询 slotPoll，上限 busyWaitCap。
// 返回 false = 客户端断连。等到名额、或池况变成别的原因（冷却/停牌）都提前收手 ——
// 由调用方重选一次定论。
func waitForSlot(ctx context.Context, p *pool.Pool, model, realm string) bool {
	deadline := time.Now().Add(busyWaitCap)
	for time.Now().Before(deadline) {
		if !sleepCtx(ctx, slotPoll) {
			return false
		}
		if !p.SlotWaitForRealm(model, realm) {
			return true
		}
	}
	return true
}
