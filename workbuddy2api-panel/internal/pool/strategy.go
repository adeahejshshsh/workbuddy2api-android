// strategy.go 选号策略：加权随机（默认）/ 轮询 / 顺序耗尽。
//
// 为什么需要可配（而不是只有加权随机）：加权随机的目标是「打散热点、让免费号和
// 久置号都有机会」，这在多账号分摊流量时是对的；但有两类场景它不合适——
//
//	轮询      客户端明确希望「这一轮用 A、下一轮用 B」，便于按账号对账或做灰度；
//	顺序耗尽  想先把一个号烧完再用下一个（清库存、准备停用某批号），加权随机
//	          永远会雨露均沾，做不到「用完一个再用下一个」。
//
// 三条策略都只在**既有候选集之内**重新排列选择，不改变任何可用性判定：
// 健康/冷却/在途/积分保底/成本分层/账号优先级全部先于策略生效——策略只决定
// 「在这些同样合格的号里,这次选哪个」。这条边界很重要：如果策略能绕过
// 积分保底，那它就是一个能烧穿余额的后门。
package pool

import (
	"sort"
	"strings"
	"time"
)

// 选号策略常量（config pool.selection_strategy 的取值）。
const (
	// StrategyWeighted 加权随机（缺省，既有行为）。
	StrategyWeighted = "weighted"
	// StrategyRoundRobin 轮询：每次请求换下一个号（按 UID 排序后的环）。
	StrategyRoundRobin = "round_robin"
	// StrategySequentialExhaust 顺序耗尽：当前号一直用到不可用（冷却/在途满/被
	// tried 排除）才换下一个。
	StrategySequentialExhaust = "sequential"
)

// NormalizeStrategy 归一化策略名（空/未知 → weighted，保证未知值不会让选号失效）。
//
// 接受几个常见别名：运维手写 sequential_exhaust / seq / rr 时不必先查文档。
// 下划线必须保留——round_robin 的规范写法本身就带下划线（此前手写的 lower 顺手
// 去掉了 "_"，导致 "round_robin" 归一成 "roundrobin" 落到 default 分支，
// 策略静默失效为加权随机：切了配置却完全没生效，而且没有任何报错）。
func NormalizeStrategy(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case StrategyRoundRobin, "rr", "round-robin", "roundrobin":
		return StrategyRoundRobin
	case StrategySequentialExhaust, "sequential_exhaust", "seq", "exhaust", "sequentialexhaust":
		return StrategySequentialExhaust
	default:
		return StrategyWeighted
	}
}

// SetStrategy 设置选号策略（热生效）。未知值回落 weighted。
func (p *Pool) SetStrategy(raw string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.strategy = NormalizeStrategy(raw)
}

// Strategy 当前生效策略（面板展示用）。
func (p *Pool) Strategy() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.strategy == "" {
		return StrategyWeighted
	}
	return p.strategy
}

// scheduleKey 游标分桶键：**按 realm + 模型**，不按全局。
//
// 为什么分桶：顺序耗尽语义是「这个模型先用 A 号」，不同模型/不同域的可用集合
// 本来就不一样（global 模型只有 global 号能接）；共用一个游标会让每次域切换都
// 从环头重来，「用完一个再用下一个」就不成立了。
func scheduleKey(realm, model string) string {
	return realm + "\x1f" + model
}

// strategyOrder 给策略用的确定性顺序：按 UID 升序。
//
// 不沿用传入切片的顺序：candsAll 是**权重降序**，权重每笔请求都在变（credit、
// 闲置、连败都会影响），用它当环会让「轮询」在每次权重抖动后换一条环，实际效果
// 是随机。UID 序是池里唯一稳定的顺序。
func strategyOrder(cands []*entry) []*entry {
	out := make([]*entry, len(cands))
	copy(out, cands)
	sort.SliceStable(out, func(i, j int) bool { return out[i].a.UID < out[j].a.UID })
	return out
}

// pickRoundRobinLocked 轮询：取环上游标的下一个。
// 游标不存在或已不在候选里（冷却/禁用/被 tried 排除）时，从环头重新开始。
func (p *Pool) pickRoundRobinLocked(order []*entry, key string) *entry {
	if len(order) == 0 {
		return nil
	}
	cur := p.scheduling[key]
	if cur.Strategy != StrategyRoundRobin {
		cur = scheduleCursor{Strategy: StrategyRoundRobin}
	}
	next := 0
	if cur.UID != "" {
		for i, e := range order {
			if e.a.UID == cur.UID {
				next = (i + 1) % len(order)
				break
			}
		}
	}
	selected := order[next]
	p.scheduling[key] = scheduleCursor{Strategy: StrategyRoundRobin, UID: selected.a.UID}
	p.dirty.Store(true)
	return selected
}

// pickSequentialLocked 顺序耗尽：当前号还能用就一直用它，否则换排序上的下一个。
//
// 「还能用」由调用方保证：传进来的 order 已经是**本次可接单**的候选集
// （健康/在途/积分/分层都已过滤），所以当前号出现在 order 里就等于它还能接。
// 这也是它与轮询的唯一差别——轮询无条件推进，耗尽不推进。
func (p *Pool) pickSequentialLocked(order []*entry, key string) *entry {
	if len(order) == 0 {
		return nil
	}
	cur := p.scheduling[key]
	if cur.Strategy != StrategySequentialExhaust {
		cur = scheduleCursor{Strategy: StrategySequentialExhaust}
	}
	// 当前游标仍在候选里 → 继续用它（这是「耗尽」语义的核心）。
	for _, e := range order {
		if cur.UID != "" && e.a.UID == cur.UID {
			p.scheduling[key] = scheduleCursor{Strategy: StrategySequentialExhaust, UID: cur.UID}
			p.dirty.Store(true)
			return e
		}
	}
	// 游标不在了：拿排序上第一个 UID 大于游标的（环回环头）。用「有序后继」而不是
	// 环上的下一位，是为了让「被跳过/被禁用」的号不会把游标拽回去重新烧一遍。
	selected := order[0]
	for _, e := range order {
		if e.a.UID > cur.UID {
			selected = e
			break
		}
	}
	p.scheduling[key] = scheduleCursor{Strategy: StrategySequentialExhaust, UID: selected.a.UID}
	p.dirty.Store(true)
	return selected
}

// pickByStrategyLocked 按当前策略在候选里选一个；策略为 weighted 时返回 nil
// （调用方继续走既有的加权随机路径）。
//
// 候选必须是「已过全部可用性判定」的集合，且顺序已经由 strategyOrder 归一。
func (p *Pool) pickByStrategyLocked(cands []*entry, reqModel, realm string, now time.Time) *entry {
	if p.strategy == "" || p.strategy == StrategyWeighted {
		return nil
	}
	order := strategyOrder(cands)
	key := scheduleKey(realm, reqModel)
	var selected *entry
	switch p.strategy {
	case StrategyRoundRobin:
		selected = p.pickRoundRobinLocked(order, key)
	case StrategySequentialExhaust:
		selected = p.pickSequentialLocked(order, key)
	default:
		return nil
	}
	if selected == nil {
		return nil
	}
	// 防并发撞号：策略选中的号刚被别的 goroutine 用掉时（同一时钟刻度内），
	// 在剩余候选里按同一策略再取一次。轮询本身就会打散，这里只兜住「并发同一
	// 瞬间全部命中同一个号」的窄窗口。
	if selected.lastUsed.IsZero() || now.Sub(selected.lastUsed) >= minPickGap {
		return selected
	}
	for _, e := range order {
		if e == selected {
			continue
		}
		if e.lastUsed.IsZero() || now.Sub(e.lastUsed) >= minPickGap {
			// 只改本次选中的号，**不动游标**：游标已经推进到 selected，下一个请求
			// 自然从它之后继续。若这里也改游标，环会被临时借用的号拽偏，
			// 轮询序列就不再是「严格轮转」（实测会跳号、绕圈）。
			return e
		}
	}
	return selected // 全都刚用过：认了，比返回 nil（请求失败）好
}

// scheduleCursor 某 (realm, 模型) 上的策略游标。
//
// 带 Strategy 字段是为了「切策略后旧游标自动失效」：轮询的游标是「环上的位置」，
// 顺序耗尽的游标是「当前烧的那个号」，同一个 UID 在两种语义下含义不同；不校验
// 策略就会让切回去的瞬间从上次残留的位置继续，看起来像随机跳。
type scheduleCursor struct {
	Strategy string
	UID      string
}
