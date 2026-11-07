// restrict.go 11140 风控连坐保护（吸收 workbuddy2api-hub 的 restriction hold）。
//
// 背景：上游把「账号存在异常行为，已被限制使用」（403 / code 11140）判在设备与出口上，
// 不是判在单个账号上。本仓原来的处置是「禁用这个号、立刻换下一个号继续同一请求」，
// 于是一个客户端请求能顺着轮转把池子挨个撞一遍（hub #422 实测：一次任务 7 个号全封）。
//
// 两道闸：
//  1. 命中 11140 就地停手（不换号）——见 applyErrorPolicy 的返回值与 chat 的轮转分支；
//  2. 同一窗口内 ≥2 个不同账号被限制 → 判定为连坐扫荡，全池暂停派发 hold 时长，
//     期间请求快速失败（503 + Retry-After），不再拿账号去试。
//
// 与 wafip.go 的分工：那个按「短窗多号 WAF 403」判 IP 被拦（拦截页没有业务信封），
// 这个按「多号被判账号限制」判设备/出口被风控。两者信号源不同，各自独立终止轮转。
//
// 作用域：只挂 chat 主链路（11140 是 chat 返回的，其余端点的流量与风险都小得多）。
// 状态在进程内、重启清零——「暂停」是「现在先别打」的意思，重启后重新判定即可。
package server

import (
	"log"
	"sync"
	"time"
)

// restrictionWaveWindow 连坐判定窗口；restrictionWaveMin 窗口内不同账号数阈值。
// var 仅供测试注入短窗（生产恒 15min / 2——单号反复 11140 归账号自己的禁用，不触发全池）。
var (
	restrictionWaveWindow = 15 * time.Minute
	restrictionWaveMin    = 2
)

// restrictionGate 连坐状态机（Handler 内嵌，零值可用）。
type restrictionGate struct {
	mu    sync.Mutex
	hits  map[string]time.Time // uid → 最近一次被限制的时刻（窗内，惰性剪枝）
	until time.Time            // 全池暂停截止；零值 = 未暂停
}

// note 记一次账号级风控限制，返回本次是否把全池暂停。hold <= 0 表示配置关掉了连坐保护
// （只保留「命中即停手」那道闸）。
//
// 同一 uid 重复命中只更新时间、不增计数（判定口径是「不同账号数」），避免一个号被
// 反复重试把暂停骗长；判定成立后清空窗口，暂停解除后需要全新的一轮才再触发。
func (g *restrictionGate) note(uid string, hold time.Duration) bool {
	if hold <= 0 {
		return false
	}
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.hits == nil {
		g.hits = map[string]time.Time{}
	}
	for u, t := range g.hits {
		if now.Sub(t) > restrictionWaveWindow {
			delete(g.hits, u)
		}
	}
	g.hits[uid] = now
	if len(g.hits) < restrictionWaveMin {
		return false
	}
	g.until = now.Add(hold)
	g.hits = map[string]time.Time{}
	log.Printf("WARN: [server] upstream restriction wave: %d accounts restricted within %s, dispatch paused %s (to %s)",
		restrictionWaveMin, restrictionWaveWindow, hold, g.until.Format(time.RFC3339))
	return true
}

// remaining 全池暂停的剩余时长（0 = 未暂停，可正常派发）。
func (g *restrictionGate) remaining() time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	if d := time.Until(g.until); d > 0 {
		return d
	}
	return 0
}

// reset 立即解除（测试与将来面板「一键解冻」用）。
func (g *restrictionGate) reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.hits = nil
	g.until = time.Time{}
}
