package server

import (
	cstpkg "github.com/linguo2625469/workbuddy2api-panel/internal/cst"
	"log"
	"sync"
	"time"
)

// degradeGate 降级状态机：passthrough 模式下请求被上游内容策略拦截时，
// 切换到 Degraded 中性提示词直到次日 00:00 CST 重置。
//
// 定位：误报处理（与 internal/upstream/sanitize.go 同哲学）——内容拦截多为
// system 来源的指纹误报，换最小中性提示词即可绕开；不是对抗框架。
//
// 状态机：进程内存、重启清零（可接受：重启极少触发，且 custom 模式根本
// 不进降级路径）。降级期内的请求直达 Degraded，不再先撞 400。
type degradeGate struct {
	mu    sync.Mutex
	byKey map[string]time.Time
}

// degradeKeyMax 降级键数量上限（UA+模型 的组合可以很多，防内存无界）。
const degradeKeyMax = 256

// Active key 是否处于降级期（now < until）。
//
// 为什么按 key 隔离（bug 4）：状态原本是进程级单例，**任意一个**请求被上游内容策略
// 拦（最常见是 system 指纹误报）就把 until 设到次日 00:00，此后**所有客户端**的
// 请求都改走中性提示词 —— Codex/Claude Code 的项目规范、工具约定被整段替换，
// 一整个白天都退化。一个客户端的误报不该变成全站降智。
func (g *degradeGate) Active(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	until, ok := g.byKey[key]
	return ok && time.Now().Before(until)
}

// Trigger 触发 key 的降级，直到次日 00:00 CST（Asia/Shanghai）。
// 已在降级期内则不续期（保持最早触发点的 00:00 重置语义）。
func (g *degradeGate) Trigger(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	if until, ok := g.byKey[key]; ok && now.Before(until) {
		return // 未过期：保持原 until（不续期）
	}
	if g.byKey == nil {
		g.byKey = map[string]time.Time{}
	}
	g.byKey[key] = nextMidnightCST(now)
	// 键数兜底：先清过期，仍超限就整体重置（降级本就是"当天抢救"，重置代价可接受）
	if len(g.byKey) > degradeKeyMax {
		for k, u := range g.byKey {
			if !now.Before(u) {
				delete(g.byKey, k)
			}
		}
		if len(g.byKey) > degradeKeyMax {
			keep := g.byKey[key]
			g.byKey = map[string]time.Time{key: keep}
			log.Printf("WARN: [server] degrade gate 键数超上限，已重置（只保留当前客户端）")
		}
	}
}

// nextMidnightCST 返回 now 之后最近的 Asia/Shanghai 00:00 时刻（纯函数，方便单测）。
//
// 边界语义：
//   - 23:59 → 次日 00:00（几秒后）
//   - 00:00 → 次日 00:00（刚过 00:00，下个零点是次日）
//
// 用固定 +08:00 偏移计算，避免依赖系统时区配置（容器/宿主机时区不确定）。
func nextMidnightCST(now time.Time) time.Time {
	cst := cstpkg.Loc
	// 把 now 转到 CST 视角取当天 00:00，再加一天；若该 00:00 <= now 则再加一天。
	y, m, d := now.In(cst).Date()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, cst)
	for !midnight.After(now) {
		midnight = midnight.Add(24 * time.Hour)
	}
	return midnight
}
