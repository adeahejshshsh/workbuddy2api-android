package server

import (
	"log"
	"sync"
	"time"
)

// 11140 风控连坐保护（吸收 workbuddy2api-hub 的 restriction hold）。
//
// 上游把「账号存在异常行为，已被限制使用」（403 / code 11140，判据见 upstream/restrict.go）
// 判在设备与出口上，不是判在单个账号上。原来的处置是「禁用这个号、立刻换下一个号继续同一
// 请求」，于是一个客户端请求能顺着轮转把池子挨个撞一遍（hub #422 实测：一次任务 7 个号全封）。
//
// 两道闸：
//  1. 命中即停手：禁用该号，本请求不再换号（chatCompletions 的 HTTP 错误与首帧否决两条路径）；
//  2. 同一域、同一窗口内 ≥ restrictionWaveMin 个不同账号被限制 → 判为连坐扫荡，该域暂停派发
//     upstream.restriction_hold_seconds，期间该域的 chat 请求快速失败（503 + Retry-After）。
//
// 为什么按域（cn / global）而不是全池：两个域是两套独立的上游服务（不同域名、不同账号体系、
// 各自的风控），CN 的扫荡说明不了 global 的出口有没有被盯上；全池暂停会把一个域的风控事件
// 放大成整个网关停摆。更细的按出口（代理路由）分组需要 Handler 知道每个号的路由，
// 目前拿不到，按域是能确定成立的最小范围。
//
// 作用域：只挂 chat 主链路（11140 由 chat 返回，其余端点的流量与风险都小得多）。状态在进程内、
// 重启清零——「暂停」的意思是「现在先别打」，重启后重新判定即可。

// 阈值必须随池子规模缩放（2026-11-13 事故）：曾经固定「2 个号被封 → 全站停派 30 分钟」，
// 在 340 个号的池子上等于 0.6% 的账号出事就让 100% 的请求失败；当天封号潮持续，于是
// 11:11→14:44、21:41→23:41 连续停派，4,036 条请求以 503 快速失败（客户端表现为
// 「没想完也没作答就断了」），而同期被封的账号并没有因此少掉。现在阈值取「本域可用号数的
// 1/10」（下限 2）：340 个号的池子要 15 分钟内封掉 34 个才判扫荡。
//
// 暂停时长也要退避：固定 30 分钟在持续扫荡下会变成「刚解除就再次触发」的连续黑障。
// 现在首次 30 秒，连续触发按 2 倍升到上限 10 分钟（且不超过 upstream.restriction_hold_seconds），
// 平静 30 分钟没有新限制就清零重来——扫荡早期用短暂停探路，真持续再拉长。
//
// restrictionWaveWindow 连坐判定窗口；restrictionWaveMin 阈值下限；restrictionWaveDiv 池子
// 每多少个可用号把阈值抬 1（0/负 = 不缩放，退回固定阈值）。单号反复 11140 归账号自己的禁用，
// 不触发暂停。var 只为测试能调短。
var (
	restrictionWaveWindow = 15 * time.Minute
	restrictionWaveMin    = 2
	restrictionWaveDiv    = 10
	// restrictionHoldBase 首次暂停时长；restrictionHoldMax 退避上限（与配置取小）。
	restrictionHoldBase = 30 * time.Second
	restrictionHoldMax  = 10 * time.Minute
	// restrictionCalmReset 这么久没有新的限制就把连续计数清零（扫荡过去后重新从 30 秒起）。
	restrictionCalmReset = 30 * time.Minute
)

// restrictionThreshold 本域可用号数 → 触发扫荡所需的不同账号数。
// healthy<=0（拿不到池子规模）时退回固定下限：宁可早停，也不要在未知规模的池子上放行。
func restrictionThreshold(healthy int) int {
	if restrictionWaveDiv <= 0 {
		return restrictionWaveMin
	}
	if n := healthy / restrictionWaveDiv; n > restrictionWaveMin {
		return n
	}
	return restrictionWaveMin
}

type restrictionState struct {
	hits   map[string]time.Time // uid → 最近一次被限制的时刻（窗内，惰性剪枝）
	until  time.Time            // 该域暂停截止；零值 = 未暂停
	streak int                  // 连续触发次数（决定本次暂停时长）
	last   time.Time            // 最近一次触发时刻（判平静清零）
}

// restrictionGate 按域的连坐状态机。零值可用。
type restrictionGate struct {
	mu sync.Mutex
	m  map[string]*restrictionState
}

// note 记一次 uid 在 realm 上的账号风控限制，返回本次是否触发该域暂停。hold <= 0 表示配置
// 关掉了暂停（只保留「命中即停手」）。healthy 是本域当前可用号数，用于缩放阈值。
//
// 同一 uid 重复命中只更新时间、不增计数（口径是「不同账号数」），一个号被反复重试不会把
// 暂停骗出来；判定成立后清空窗口，暂停解除后要全新的一轮才再触发。
func (g *restrictionGate) note(realm, uid string, hold time.Duration, healthy int) bool {
	if hold <= 0 {
		return false
	}
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.m == nil {
		g.m = map[string]*restrictionState{}
	}
	st := g.m[realm]
	if st == nil {
		st = &restrictionState{hits: map[string]time.Time{}}
		g.m[realm] = st
	}

	for u, t := range st.hits {
		if now.Sub(t) > restrictionWaveWindow {
			delete(st.hits, u)
		}
	}
	st.hits[uid] = now
	need := restrictionThreshold(healthy)
	if len(st.hits) < need {
		return false
	}
	// 连续触发按 2 倍退避，上限 min(配置, restrictionHoldMax)。平静期后重新从基数起：
	// 扫荡过去 30 分钟没有新限制，说明那一波结束了，不该继续拿上一波的 streak 加码。
	if !st.last.IsZero() && now.Sub(st.last) > restrictionCalmReset {
		st.streak = 0
	}
	st.streak++
	st.last = now
	pause := restrictionHoldBase
	for i := 1; i < st.streak; i++ {
		pause *= 2
		if pause >= restrictionHoldMax {
			break
		}
	}
	if pause > restrictionHoldMax {
		pause = restrictionHoldMax
	}
	if pause > hold {
		pause = hold
	}
	st.until = now.Add(pause)
	st.hits = map[string]time.Time{}
	log.Printf("WARN: [server] upstream restriction wave realm=%q: %d accounts restricted within %s (threshold %d, %d healthy), dispatch paused %s (until %s, streak=%d)",
		realm, need, restrictionWaveWindow, need, healthy, pause, st.until.Format(time.RFC3339), st.streak)
	return true
}

// remaining realm 的暂停剩余时长（0 = 未暂停，可正常派发）。
func (g *restrictionGate) remaining(realm string) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	if st := g.m[realm]; st != nil {
		if d := time.Until(st.until); d > 0 {
			return d
		}
	}
	return 0
}

// reset 立即解除全部域的暂停（测试与将来面板「一键解冻」用）。
func (g *restrictionGate) reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.m = nil
}

// realmHealthy 本域可用号数，供连坐阈值缩放；拿不到（未配池子）返回 0 = 用固定下限。
func (h *Handler) realmHealthy(realm string) int {
	if h.cfg.Pool == nil {
		return 0
	}
	_, healthy, _, _, _ := h.cfg.Pool.CountsDetailedForRealm(realm)
	return healthy
}
