// 选号：Pick 簇（healthy 成本分层 + 快过期虚拟实例权重 + 全冷却兜底 + 在途占满过滤）。
package pool

import (
	"log"
	"math"
	"math/rand/v2"
	"sort"
	"strconv"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/logfmt"
)

// Pick 单一选号入口（无请求级轮换、无 realm 过滤，模型感知缺省账号级）。
// 需要请求级轮换（tried）或分池（realm）时用 PickExcludingForRealm。
func (p *Pool) Pick() *auth.Auth {
	return p.pick(nil, "", "")
}

// PickExcluding 同上，但跳过 tried 中的 uid（请求级轮换）。
// 挑选策略：healthy 账号中按权重取前 5 名，再在 Top5 内按同一权重加权随机抽签，
// 意图是打散热点，避免永远打同一个账号。
func (p *Pool) PickExcluding(tried map[string]bool) *auth.Auth {
	return p.pick(tried, "", "")
}

// PickExcludingForModel 模型感知选号：等同 PickExcluding，但对「6004 模型级冷却中的
// 账号」进行模型豁免——请求模型与其 trigger 模型不同时视为可用（issue #31）。
// reqModel 为空时即普通 PickExcluding（不影响既有调用语义）。
func (p *Pool) PickExcludingForModel(tried map[string]bool, reqModel string) *auth.Auth {
	return p.pick(tried, reqModel, "")
}

// PickExcludingForRealm 模型感知 + 分池选号：候选集先按 Realm()==realm 过滤
// （realm 空 = 不过滤，退化为 PickExcludingForModel），再按模型健康口径判定。
// 供 handler 在 global/cn 双域下分流（global 模型请求只路由 global 账号）。
func (p *Pool) PickExcludingForRealm(tried map[string]bool, reqModel, realm string) *auth.Auth {
	return p.pick(tried, reqModel, realm)
}

// pick 在 healthy 候选集中按权重加权随机选出账号，并记录 lastUsed（防并发撞号）。
// reqModel 非空时把健康口径换成 healthyForModel（6004 模型豁免生效）。
// realm 非空时候选过滤叠加 Realm()==realm 谓词（分池选号域）。
func (p *Pool) pick(tried map[string]bool, reqModel, realm string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	realmOK := func(e *entry) bool { return realm == "" || e.a.Realm() == realm }
	// 不健康 / 在途占满 / 保留积分停牌：都不接单（判据统一在 Pool.selectable；
	// reqModel 非空时用 healthyForModel 口径）。
	selectableOf := func(e *entry) bool { return realmOK(e) && p.selectable(e, now, reqModel) }
	// floorBlocked 积分保底拦截判定（实现在 floorBlockedForRealmModel，与粘性路径共用）：
	// 触底 + 收费（本地实测台账 或 上游目录倍率）即拦；免费/未知倍率不受限。
	floorBlocked := func(e *entry) bool { return p.floorBlockedForRealmModel(e, reqModel, realm, now) }
	var cands []*entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		// 惰性清理过期的模型级冷却与成本台账（两者的 map 都不无限膨胀；
		// status 只读遍历天然跳过过期项，但内存条目必须在此真正删除）。
		e.pruneExpiredModelCooldowns(now)
		e.pruneExpiredModelCosts(now)
		if !selectableOf(e) {
			continue
		}
		if floorBlocked(e) {
			continue // 积分保底：触底号不接实测收费模型（tier 0/1 不受限）
		}
		cands = append(cands, e)
	}
	if len(cands) == 0 {
		// 全冷却兜底：无 healthy 候选时，从冷却账号里选 until 最早到期的一个
		// （熔断/冷却共用 expiry 口径，取较早截止者）。禁用的账号永不参与兜底。
		return p.pickEarliestExpiryLocked(tried, now, realm, reqModel)
	}
	// 账号优先级（issue #62）：配了优先级的号构成「先烧层」，层内有号可用时其余号一律
	// 不接单——「优先消耗某个账号的余额」是一条硬语义，加权排序做不到（排序只改抽签
	// 次序，高优先级号仍会被抽到）。放在成本分层之前：人的显式配置压过自动启发式。
	// 零配置时 prioritizeLocked 原样返回，本行无副作用。冷却/停牌/在途满的号已在上面的
	// 候选循环里被逐出，所以层内号烧空/冻结后本层自动让位给下一层。
	cands = p.prioritizeLocked(cands)
	// top5 短名单按权重降序截断（而非 credits 单纯降序）：否则闲置补偿根本进不了
	// 短名单决策，低 credits 但久置的账号会永远排不进 top5。
	// maxCredits 统一用**全集口径**（tier 过滤前的全部 healthy 候选）：截断排序与
	// 抽签权重共享同一基准，两个阶段权重可比。
	maxCredits := secondCredits(cands)
	// 成本分层（reqModel 非空时）：按该模型的实测扣费把候选分层，只保留最优层。
	//   0 = 已实测免费（限免期/夜间免费的号，最强偏好）
	//   1 = 无观测（含观测过期）
	//   2 = 已实测收费
	// 为什么"无观测"排在"已实测收费"之前：新号的限免状态只能靠实测发现，
	// 若已知收费的号恒压过未知号，那台免费的号永远轮不到，也就永远学不到。
	// 为什么用硬过滤而非仅排序：pickWeighted 会在候选内加权随机，只排序的话
	// 收费号仍有机会抽中，达不到"优先免费"的语义。
	costTier := func(e *entry) (int, float64) {
		mc, ok := e.modelCostOf(reqModel, now)
		if !ok {
			return 1, 0
		}
		if mc.CostPer1k <= 0 {
			return 0, 0
		}
		return 2, mc.CostPer1k
	}
	bestTier := 2
	hasTier1 := false
	explored := false // 本次 pick 是否切了探索层（事件日志在选中号确定后打）
	for _, e := range cands {
		if ti, _ := costTier(e); ti < bestTier {
			bestTier = ti
		}
	}
	// 条件探索（issue #136 方案 a′）：tier 0 垄断层存在（bestTier==0 且 reqModel
	// 非空）且候选含 tier 1（冻结存在）且距上次探索 ≥ 窗口（零值 timer=从未探索
	// →首次满足即探）时，本次 pick 生效层切 tier 1-only——探索=搭车改道，把一个
	// 既有真实用户请求改道给未知号（零新增上游请求；IP 维度零增量，WAF 友好）。
	// 成功 → NoteModelCost 首观测 → 毕业（tier 0/2，下一轮 pick 立即生效）；
	// 失败 → 既有错误策略照常，无探测风暴。
	// hasTier1 复用本循环上方 costTier 的预计算口径（每候选一次的契约不变）。
	// timer 同锁写入：并发 pick 串行进入写锁，只有一个进入者能通过窗口判定
	//（天然防重复探索）。key = realm + "\x1f" + reqModel：同模型名可跨域，
	// 探索节奏按 (域, 模型) 独立；realm==""（Pick 老语义）单独成键。
	if p.costExploreInterval > 0 && bestTier == 0 && reqModel != "" {
		for _, e := range cands {
			if ti, _ := costTier(e); ti == 1 {
				hasTier1 = true
				break
			}
		}
		key := realm + "\x1f" + reqModel
		if hasTier1 && now.Sub(p.exploreLast[key]) >= p.costExploreInterval {
			p.exploreLast[key] = now
			p.costExploreEvents++
			bestTier = 1
			explored = true
		}
	}
	// 权重只算一次：顶 5 截断要排序，若在 sort 比较器里现算 weightOf 会翻成 O(n log n) 次
	// 冗余浮点计算（46 账号约 500 次）。先做 O(n) 预计算，再按 (权重, uid) 排序。
	// costTier/modelCostOf 同样每候选只算一次（存入 tier/cost1k），比较器只读缓存字段。
	type weighted struct {
		e      *entry
		w      float64
		tier   int
		cost1k float64
	}
	ws := make([]weighted, 0, len(cands))
	for _, e := range cands {
		ti, ci := costTier(e)
		if ti == bestTier {
			ws = append(ws, weighted{e: e, tier: ti, cost1k: ci})
		}
	}
	// 窗口用量调度（issue #112）：按 (账号, 模型) 在本窗口内已花的 token 反比降权，
	// 权重 /= 1 + used/mean。分母取**同层均值**而不是固定阈值：量纲自洽，换池子规模、
	// 换模型倍率都不用调参；全员 0 用量时除数恒 1，行为与旧版逐字节一致。
	// 两处都要套（top5 排序 + 实际抽签），否则 penalty 只影响谁进短名单、不影响抽签概率。
	// 只降权不排除——本功能是「打散热点」，不是限流；保底/优先级/占比的硬语义不受影响。
	// shortcut: 翻滚窗口（从窗口内首次记账起算），不是滑动窗口——跨窗口边界的最初
	// 一段会短暂少算用量；要精确滑窗得存每秒桶，收益不值得这份复杂度。
	var uctx usageCtx
	if p.balanceWindow > 0 {
		uctx = usageCtx{model: reqModel, n: len(ws)}
		for _, w := range ws {
			uctx.sum += w.e.windowTokens(reqModel, p.balanceWindow, now)
		}
	}
	for i := range ws {
		ws[i].w = p.routingWeightOf(ws[i].e, maxCredits, now) / p.usageDiv(ws[i].e, uctx, now)
	}
	// 等权重洗牌：仅当存在权重相等且候选数超过 top5 时，才对 ws 做 Fisher-Yates
	// 洗牌（且**不消耗 p.randInt64N 注入源**，避免改变 pickWeighted 的确定性语义，
	// 见 TestPickDeterministicViaSetRandomSource）。权重全等或存在并列时，按字典序
	// 截断会让 uid 靠后的账号永远进不了 top5（等权重账号被字典序饿死、LRU 兜底
	// 又只在 top5 内转——惊群集中单号的根因）。洗牌用独立的 time-seeded 源，
	// 只在截断边界制造等权重随机次序，不影响加权抽签本身的确定性。
	if len(ws) > 5 {
		eq := false
		for i := 1; i < len(ws); i++ {
			if ws[i].w == ws[0].w {
				eq = true
				break
			}
		}
		if eq {
			shuf := rand.New(rand.NewPCG(uint64(now.UnixNano()), uint64(len(ws))))
			shuf.Shuffle(len(ws), func(i, j int) { ws[i], ws[j] = ws[j], ws[i] })
		}
	}
	sort.SliceStable(ws, func(i, j int) bool {
		// costTier 硬过滤后 ws 全员同层，但仍按 cost1k 升序排（tier 2 层内单价低者
		// 在前；tier 0/1 层 cost1k 恒 0，本比较退化为权重比较）——读缓存字段不现算。
		if ws[i].cost1k != ws[j].cost1k {
			return ws[i].cost1k < ws[j].cost1k // 收费层：单价低的在前
		}
		if ws[i].w != ws[j].w {
			return ws[i].w > ws[j].w
		}
		return ws[i].e.a.UID < ws[j].e.a.UID // 稳定兜底（洗牌后此项几乎不触发）
	})
	cands = cands[:0]
	for _, c := range ws {
		cands = append(cands, c.e)
	}
	// candsAll 保留截断前的全候选（权重降序），供 LRU 兜底在全量范围选最旧者，
	// 避免 top5 字典序截断把等权重靠后账号饿死（惊群根因之一）。
	candsAll := cands
	if len(cands) > 5 {
		cands = cands[:5]
	}
	var e *entry
	// 账号占比（issue #62）：本层内配了占比的号独占抽签，按 share 归一化分流量
	// （70/30 与 7/3 同义）。两条次序要求：
	//   1. 先于「快过期虚拟实例加权」——后者是自动启发式，显式占比压在它之前；
	//   2. 不走 top5 截断——截断按权重取前 5，低占比号会永远进不了短名单，占比无从生效。
	// 配了占比的号全不可用（冷却/在途满/停牌已被逐出）时本分支自动让位，回到既有
	// 加权路由：占比是分配偏好，不是排他锁，不会把请求饿死。
	if sp := p.sharePoolLocked(candsAll); len(sp) > 0 {
		e = p.pickByShare(sp)
	}
	// 防并发撞号：在持锁内基于「上次选中时刻」过滤，但同一批并发 goroutine 会串行进入
	// 本函数（写锁），每个进入者都把 lastUsed 置为 now —— 于是同一瞬间的第 2..N 个
	// 进入者看到前一个账号 lastUsed==now（距今 0 < minPickGap），被自然挤向其他账号。
	// 关键：lastUsed 在锁内赋值，使时间窗口判定在并发下可重入。
	if e == nil {
		eligible := make([]*entry, 0, len(cands))
		for _, c := range cands {
			if now.Sub(c.lastUsed) >= minPickGap {
				eligible = append(eligible, c)
			}
		}
		if len(eligible) == 0 {
			// top5 全部刚被用过：LRU 兜底，在**全候选 candsAll**（非仅 top5）里选最旧者。
			// 用 usedSeq 单调序号而非 lastUsed 墙钟比较：Windows 等平台 time.Now() 精度
			// ~0.5ms，快速连续选号时所有 lastUsed 完全相等，Before 全 false 会恒选
			// candsAll[0] 导致集中。usedSeq 严格全序，与时间精度无关。
			e = candsAll[0]
			for _, c := range candsAll[1:] {
				if c.usedSeq < e.usedSeq {
					e = c
				}
			}
		} else {
			e = p.pickWeighted(eligible, uctx) // eligible 保序 = top5 降序子集
		}
	}
	if explored {
		// 探索事件日志（可观测性）：选中号此时才确定，故在选中点打出。
		// 毕业结果由相邻的既有日志闭环（免费号无日志、收费号走 NoteModelCost
		// 常规路径）。
		log.Printf("[pool] cost explore model=%s realm=%q acct=%s window=%s",
			reqModel, realm, logfmt.Label(e.a.UID, e.a.Nickname), p.costExploreInterval)
	}
	e.lastUsed = now // 锁内即时标记：下一个进入 pick 的 goroutine 立即看到本号已用
	p.pickSeq++
	e.usedSeq = p.pickSeq // 单调序号：保证 usedSeq 严格全序（防惊群/LRU 的权威依据）
	return e.a
}

// floorBlockedForModel 积分保底拦截判定（pick 普通轮换与 PickByUIDForModel 粘性
// 路径的单一事实来源）：floor>0 且账号触底（credits < floor）且该模型**收费**
// 时为真。
//
// 收费判据两级（任一成立即判收费 → 拦）：
//  1. 本地实测台账（e.modelCostOf）：该号在该模型上实测 cost>0（tier 2）。
//  2. 上游目录倍率（p.modelRateOf）：本地无观测/观测过期（tier 1）时的兜底。
//     只看实测会让「无观测」恒等于「放行」——而高价新模型恰恰全池无观测
//     （kimi-k3-1 实案：x1.62、223 分/百万 token，两笔打穿 100 分的号并触发
//     硬冷却到次日 04:00）。倍率由上游随模型目录下发，请求前即已知，不必付学费。
//
// 不拦的情形：
//   - 模型免费：本地实测 cost<=0（tier 0），或目录倍率为 0/"0.00"。保底的目的
//     正是「留余额给免费模型用」——但若账号已归零，上游仍会 402（余额门禁是
//     账号级的，与模型无关），此时由 ErrHardCredit 冷却承接，与本判定无关。
//   - 模型倍率未知（台账无观测且目录未下发该模型）：无法判收费，按放行处理。
//     这是有意的保守选择——目录未覆盖的模型多为内部/别名模型，拦了会让号
//     永久失联；风险由「未知」本身承担，但已知收费的一律拦。
//   - model 为空（无模型上下文）不拦：无成本维度，floor 无从判收费。
//
// 余额用本地插值口径（签到权威值 - 每笔 usage.credit 实扣，见 NoteModelCost）：
// 只会偏低不会偏高（官方对账延迟方向安全），正是保底需要的安全方向。
// 调用方必须已持有 p.mu（读 e.credits / e.modelCost / p.modelRateOf）。
func (p *Pool) floorBlockedForModel(e *entry, model string, now time.Time) bool {
	return p.floorBlockedForRealmModel(e, model, "", now)
}

// floorBlockedForRealmModel 同上，但带 realm 上下文（倍率按 (realm, 模型) 分桶，
// 同名模型在 CN / global 两域倍率可不同）。realm 为空时按倍率表的空域键查。
func (p *Pool) floorBlockedForRealmModel(e *entry, model, realm string, now time.Time) bool {
	if p.creditFloor <= 0 || model == "" || e.exactCredits() >= float64(p.creditFloor) {
		return false
	}
	// 1) 本地实测台账：最权威（真实扣费证据）。
	if mc, ok := e.modelCostOf(model, now); ok {
		return mc.CostPer1k > 0
	}
	// 2) 上游目录倍率兜底：无实测观测时用牌价判收费，堵住「无观测 = 全放行」漏洞
	//    （kimi-k3-1 实案：x1.62 的模型两笔打穿 100 分的号）。
	//    但全额拦会把便宜的新模型也锁死（issue #30/PR #2：x0.11 的新模型在触底号上
	//    永远跑不出第一笔账本 → 账本永远学不回来）。故按倍率分档：≤
	//    creditFloorMaxRate 放行（允许入账），> 该值仍拦；上限 0 = 严格模式（旧行为）。
	if p.modelRateOf == nil {
		return false
	}
	rate := p.modelRateOf(realm, model)
	if rate == "" {
		return false // 目录未覆盖：未知，放行（见上方注释）
	}
	v, err := strconv.ParseFloat(rate, 64)
	if err != nil {
		return false // 倍率非数值（异常形态）：不据此惩罚账号
	}
	return v > p.creditFloorMaxRate
}

// pickEarliestExpiryLocked 全冷却兜底：在非禁用的软冷却/熔断账号中选截止最早的一个。
// 分级：disabled 永不参与；CoolHard（余额耗尽，等签到的号）同样排除——调了必 402，浪费轮换并产生噪音日志；
// CoolSoft 与熔断号允许参与（可能已恢复，失败成本仅一轮换）。
// 被 tried 排除、在途占满的账号同样跳过（维持请求级轮换 + 租约语义）。无任何可用返回 nil。
//
// 积分保底同样在此生效（model 非空时）：floor 把健康号全部拦掉后 cands 为空会走到
// 这里，若兜底不看保底，触底号会被「捞回来」继续接收费模型——表现为同一条
// floor WARN 反复刷同一个号（实测：credits=1 < floor=150 仍持续中选）。
// 兜底是**最后一道**选号路径，保底在它之前挡不住就等于没挡。
func (p *Pool) pickEarliestExpiryLocked(tried map[string]bool, now time.Time, realm, model string) *auth.Auth {
	var best *entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if realm != "" && e.a.Realm() != realm {
			continue // 域过滤：池内跨 realm 的冷却账号不参与本 realm 兜底
		}
		if e.disabled || e.paused {
			continue // 禁用/暂停选号的账号永不参与兜底
		}
		if e.modelCooled(now, model) {
			continue // 兜底也不能绕过模型冷却或手动模型停牌。
		}
		if e.coolKind == CoolHard && !e.until.IsZero() && now.Before(e.until) {
			continue // 余额耗尽号（处于有效 hard 冷却期）不参与兜底：等签到恢复，调了必 402
		}
		if p.floorBlockedForRealmModel(e, model, realm, now) {
			continue // 积分保底：触底号不接收费模型（兜底路径同判据）
		}
		// 保留积分停牌（reserve）：与正常路径 selectable 的第一判据同源。兜底是
		// **最后一道**选号路径——上面那段注释怎么论证 floor 必须在这里生效，reserve
		// 就是同一回事：健康候选被 reserve 全拦净后 cands 为空，兜底若不看 reserve，
		// 就会把刚停牌的号捞回来继续接付费模型，reserve 形同虚设（且 ServableForRealm
		// 是按 reserve 判「不可服务」的，会出现 healthz 报不可用、真请求却还能选到的
		// 口径裂缝）。免费档豁免复用 reserveBlocked 的 modelFree 口径（modelFreeOf 查
		// 上游目录倍率 == 0），保证余额见底的号最后一点额度仍能服务于免费模型。
		if p.reserveBlocked(e, p.modelFreeOf(e, model)) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		exp := e.expiry(now)
		if exp.IsZero() {
			continue
		}
		if best == nil || exp.Before(best.expiry(now)) {
			best = e
		}
	}
	if best == nil {
		return nil
	}
	log.Printf("WARN: [pool] fallback_earliest_expiry acct=%s until=%s kind=%s", logfmt.Label(best.a.UID, best.a.Nickname), best.expiry(now).Format(time.RFC3339), best.fallbackKind(now))
	best.lastUsed = time.Now()
	// 兜底同样是「选中」，必须与 pick() 正常路径、粘性命中路径（PickByUIDForModel）
	// 一样推进 usedSeq/pickSeq：否则被兜底反复选中的账号 usedSeq 恒为 0，在 pick 的
	// LRU 兜底（按 usedSeq 取最旧）眼里永远是「最旧」，刚被用过就被立刻再选——
	// 防集中/防惊群失效（entry.usedSeq 契约：每次被选中时取 p.pickSeq 自增值）。
	p.pickSeq++
	best.usedSeq = p.pickSeq
	return best.a
}

// inFlightFull 报告账号是否已占满在途名额（上限按 realm 分档，见 inFlightLimit；
// limit=0 不限 → 恒 false）。调用方需已持 p.mu（读锁或写锁均可，本方法只读上限）。
func (p *Pool) inFlightFull(e *entry) bool {
	limit := p.inFlightLimit(e)
	if limit <= 0 {
		return false
	}
	return e.inFlight.Load() >= int64(limit)
}

// usageCtx 一次 pick 的窗口用量归一上下文（同一成本层内的均值口径）。
// sum = 层内 token 用量总和，n = 层内账号数；sum 为 0 表示本层本窗口无用量账。
type usageCtx struct {
	model string
	sum   int64
	n     int
}

// usageDiv 窗口用量降权除数 = 1 + used/mean（mean = sum/n）。调用方用**除**：权重
// 除以它即「用得越多权重越低」。未开启调度或本层零用量时恒 1（恒等元，权重与旧版
// 逐字节一致）。
// shortcut: 只降权不排除——极端热点（用量远超同层）权重会趋近 0 但永不为 0，
// 需要硬隔离请用账号优先级/占比。调用方必须已持有 p.mu。
func (p *Pool) usageDiv(e *entry, u usageCtx, now time.Time) float64 {
	if u.sum <= 0 || u.n <= 0 {
		return 1
	}
	used := float64(e.windowTokens(u.model, p.balanceWindow, now))
	return 1 + used*float64(u.n)/float64(u.sum)
}

// minPickGap 防并发撞号窗口：同一账号在该窗口内不重复被选中（除非 top5 全部刚被用过）。
// 生产默认 100ms；纯加权分布测试可临时置 0 关闭防撞号。
var minPickGap = 100 * time.Millisecond

// pickWeighted 加权随机（claude-api selectWeightedRandom 参考口径）：
//
//		weight = credits 比例 × 10 + idleWeight
//
//	  - credits 比例 = 该号 credits / 候选集内最大 credits（避免量纲爆炸）
//	  - idleWeight = min(距 lastUsed 小时数 × idleWeightPerHour, idleWeightMax)；从未使用给满分
//
// credits 全 0 时仍按 idleWeight 加权（不退化均匀随机）。
// 权重为浮点，用 int64 定点（×1e6）抽签可保持确定性随机源注入（randInt64N 语义不变）。
// 随机源优先用 p.randInt64N（仅供测试注入确定性），nil 时回退 math/rand/v2 全局源。
func (p *Pool) pickWeighted(cands []*entry, uctx usageCtx) *entry {
	now := time.Now()
	maxCredits := secondCredits(cands)
	const scale = 1_000_000 // 定点放大：int64 累加权重大整数抽签
	weights := make([]int64, len(cands))
	var total int64
	for i, e := range cands {
		w := p.routingWeightOf(e, maxCredits, now) / p.usageDiv(e, uctx, now)
		weights[i] = int64(w * scale)
		total += weights[i]
	}
	rnd := rand.Int64N
	if p.randInt64N != nil {
		rnd = p.randInt64N
	}
	if total <= 0 {
		return cands[int(rnd(int64(len(cands))))]
	}
	r := rnd(total)
	var acc int64
	for i, e := range cands {
		acc += weights[i]
		if r < acc {
			return e
		}
	}
	return cands[len(cands)-1]
}

// weightOf 计算单个账号的普通加权分值。
func (p *Pool) weightOf(e *entry, maxCredits int64, now time.Time) float64 {
	return p.weightOfScaled(e, maxCredits, now, 1)
}

// sentinelCredits 企业版「不限量」在池内的哨兵剩余量，与
// upstream.enterpriseUnlimitedRemain 同值（pool 不 import upstream，故重复一次常量）。
const sentinelCredits int64 = 1 << 40

// secondCredits 返回权重分母：正常情况下就是最大余额；最大余额是哨兵时取第二大。
//
// 哨兵当分母会把其余账号的 credits 项压到 0 → 不限量号独吞流量（issue #222）。
// shortcut: 只按哨兵值本身判，不按倍数——100000 对 10 也是 1e4 倍，但那是真实余额差。
func secondCredits(cands []*entry) int64 {
	var first, second int64
	for _, e := range cands {
		switch {
		case e.credits > first:
			second, first = first, e.credits
		case e.credits > second:
			second = e.credits
		}
	}
	if first >= sentinelCredits {
		return second
	}
	return first
}

// weightOfScaled 同 weightOf，但 credits 项乘 creditScale（1 = 原口径）。
// 窗口内账号传 expiringCreditScale，把余额从主轴降为次轴（见该常量）。
func (p *Pool) weightOfScaled(e *entry, maxCredits int64, now time.Time, creditScale float64) float64 {
	w := 1.0
	// 1. credits 比例 ×10（会计入 mid-credit 锚点，避免全员 0 时 credits 项为 0）。
	// 超分母的余额钳到满权重。分母已排除哨兵，所以这里只可能命中企业「不限量」
	// （1<<40）；正常余额差（哪怕 1e4 倍）不会被钳，「余额最多优先」照旧（issue #222）。
	if maxCredits > 0 {
		cred := e.credits
		if cred > maxCredits {
			cred = maxCredits
		}
		w += float64(cred) / float64(maxCredits) * 10 * creditScale
	}
	// 2. 闲置补偿。
	if e.lastUsed.IsZero() {
		w += p.idleWeightMax // 从未使用 → 满分
	} else {
		hours := now.Sub(e.lastUsed).Hours()
		idleW := hours * p.idleWeightPerHour
		if idleW > p.idleWeightMax {
			idleW = p.idleWeightMax
		}
		if idleW < 0 {
			idleW = 0 // lastUsed 在未来（时钟回拨）时钳 0
		}
		w += idleW
	}
	// 3.（原「成功率 ×3」因子已删，对齐上游 success-ema-review：errTotal 是终身
	// 累计、只增不减，成功率 = successCount/(successCount+errTotal) 会让早期出过错
	// 的号被永久压权且永不恢复；瞬时健康信号已由冷却/熔断/连败降权承接。）
	return w
}

// expiringNow 报告账号是否存在当前仍有效的快过期积分批次。
func expiringNow(e *entry, now time.Time) bool {
	return e.creditsExpiring > 0 &&
		e.creditsEarliestRemaining > 0 &&
		!e.creditsEarliestExpiry.IsZero() &&
		e.creditsEarliestExpiry.After(now)
}

// expiringUrgency 快过期账号的紧迫度倍率（1 = 不加权）。
//
// 与固定倍率（expiringVirtualSlots）的差别：刚进窗口只有 expiringUrgencyMin，
// 越逼近到期越接近 expiringUrgencyMax —— 「今天到期」因此真正优先于「下周到期」，而
// 不是两者等价（灵感：上游 issue #140 第 1、2 点）。
//
// 形状用 sqrt(T/W) 而非 T/W（线性）：窗口默认 168h，线性下「还剩 6 天」已走到 86% 的
// 紧迫度，「还剩 1 天」是 99% —— 两者只差 1.16 倍，永远翻不过 credits 基数（实测：线性
// 下把 Max 调到 100 也压不过）。sqrt 让最后几小时才陡升：6 天 → 7% 行程、1 天 → 92%，差距
// 拉到 ~12 倍，才够压过「余量 10 倍差」。
//
// 软偏好语义不变：始终只是权重上的一个有限倍率（≤ Max），不会把流量全压到单一账号。
// 未注入窗口（expiringWindow <= 0，如单测/嵌入方未给第二参）时退回旧的固定倍率，
// 结果与旧实现逐字节一致。
func (p *Pool) expiringUrgency(e *entry, now time.Time) float64 {
	if e == nil || !expiringNow(e, now) {
		return 1
	}
	if p.expiringWindow <= 0 {
		return expiringVirtualSlots // ponytail: 无窗口信息 → 保持旧口径
	}
	remain := e.creditsEarliestExpiry.Sub(now)
	if remain >= p.expiringWindow {
		return expiringUrgencyMin
	}
	frac := math.Sqrt(float64(remain) / float64(p.expiringWindow)) // T/W ∈ (0,1)
	return expiringUrgencyMin + (expiringUrgencyMax-expiringUrgencyMin)*(1-frac)
}

// routingWeightOf 在普通账号权重上叠加紧迫度倍率。prefer_expiring=false 或账号无
// 有效快过期批次时，结果与旧 weightOf 完全一致（不降权、不叠倍率）。
//
// 开启到期加权**永不低于关闭时的权重**——这是本函数的第一条不变量。梯度生效时
// 余额项被压到 expiringCreditScale（credits 项 10 → 2），基础权重最多降到约 1/3.7；
// 而窗口边缘的紧迫度只有 expiringUrgencyMin(1.5)，两者相乘 (1+2)×1.5=4.5 远低于
// 原权重 (1+10)=11。结果就是「只要落在窗口最后 22% 里，开了比不开还差」——勾了
// 「快过期积分优先」反而更少被选中。取 max 把它收口成「只加不减」的纯增益。
func (p *Pool) routingWeightOf(e *entry, maxCredits int64, now time.Time) float64 {
	normal := p.weightOf(e, maxCredits, now)
	if !p.preferExpiring {
		return normal
	}
	u := p.expiringUrgency(e, now)
	if u == 1 {
		return normal
	}
	// 未注入窗口（u 走固定倍率回退）= 兼容口径：只叠倍率、不降权，与旧实现逐字节一致。
	if p.expiringWindow <= 0 {
		return normal * u
	}
	// 梯度生效：紧迫度为主轴、余额为次轴（expiringCreditScale），但不低于基线。
	return math.Max(normal, p.weightOfScaled(e, maxCredits, now, expiringCreditScale)*u)
}

// weightScale 权重定点放大：int64 累加后抽签，保持随机源注入（randInt64N）语义不变。
const weightScale = 1_000_000

// pickByShare 在配了占比的账号里按 share 抽签（issue #62）：权重 = share，归一化后即比例。
// 不套 minPickGap：占比的语义就是「流量按这个比例分给这几号」，硬压间隔会让比例失真；
// 而在途上限/冷却/停牌已逐出不可用号，占满的号自动退出本次抽签，请求不会堆在单号上。
// share 极小（< 1/weightScale）时钳到最小权重，避免整轮权重归零退化成均匀随机——
// 那会让「1% 的号」和「99% 的号」一样多。
func (p *Pool) pickByShare(cands []*entry) *entry {
	weights := make([]int64, len(cands))
	for i, e := range cands {
		w := int64(p.shareOf(e.a.UID) * weightScale)
		if w <= 0 {
			w = 1
		}
		weights[i] = w
	}
	return p.drawWeighted(cands, weights)
}

// drawWeighted 定点权重抽签（weights 与 cands 等长、同序）。权重和 <= 0 时退化为均匀随机。
// 随机源优先用 p.randInt64N（仅供测试注入确定性），nil 时回退 math/rand/v2 全局源。
func (p *Pool) drawWeighted(cands []*entry, weights []int64) *entry {
	var total int64
	for _, w := range weights {
		total += w
	}
	rnd := rand.Int64N
	if p.randInt64N != nil {
		rnd = p.randInt64N
	}
	if total <= 0 {
		return cands[int(rnd(int64(len(cands))))]
	}
	r := rnd(total)
	var acc int64
	for i, e := range cands {
		acc += weights[i]
		if r < acc {
			return e
		}
	}
	return cands[len(cands)-1]
}

// SetCredits 更新账号余额。
