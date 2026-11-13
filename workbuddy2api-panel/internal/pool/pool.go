// Pool 账号池核心：结构定义、构造（New/Set* 注入）、在途租约（Acquire/Release）
// 与账号增删（Add/SyncToDir/upsertLocked）。选号/冷却/状态/持久化见同包其他文件。
package pool

import (
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

type Pool struct {
	mu    sync.RWMutex
	byUID map[string]*entry
	// rules 模型规则表（id → 规则）。面板「模型规则」页维护，随 state.json 落盘。
	rules   map[string]ModelRule
	stateFp string
	dirty   atomic.Bool // 内存有变更待落盘
	// store 池状态快照镜像（redisstore.Store）；nil = 无需镜像（未配置 Redis / Noop 之外也可能 nil）。
	// SaveState/LoadState 经它接线，与本地 state.json 并存作启动恢复备份。
	store StoreSnapshotter
	// 熔断器调优（SetBreaker 注入；默认值见 defaultBreaker*）。
	breakerThreshold   int
	breakerCooldown    time.Duration
	breakerCooldownMax time.Duration
	// softRateMax 软冷却指数退避的封顶（SetSoftRateMax 注入；默认 defaultSoftRateMax）。
	softRateMax time.Duration
	// softResetMax 上游权威重置墙钟的合理性上限（SetSoftResetMax 注入；
	// 默认 defaultSoftResetMax）。只挡荒谬的解析结果，不参与指数退避。
	softResetMax time.Duration
	// costExploreInterval costTier 条件探索窗口（issue #136 方案 a′，SetCostExploreInterval
	// 注入；默认 defaultCostExploreInterval 30m）。tier 0 垄断 + tier 1 存在且距上次
	// 探索 ≥ 窗口时，本次 pick 生效层切 tier 1-only（探索=搭车改道，零新增上游请求）。
	// 0 = 关停（完全回到现状行为）。
	costExploreInterval time.Duration
	// exploreLast 各 (realm, 模型) 的上次探索时刻，键 = realm + "\x1f" + model。
	// 运行态（不持久化，同 lastUsed/usedSeq 口径）：重启归零 → 每个仍冻结的
	// (域, 模型) 多至 1 次即时重探；已毕业号经 ModelCosts 恢复 tier，学费不重付。
	// 只在探索事件时写入（tier 1 枯竭期间停走，陈旧无害）；不做对称清理。
	exploreLast map[string]time.Time
	// costExploreEvents 累计探索事件数（/status 透出；pick 写锁内 ++，无需 atomic）。
	costExploreEvents int64
	// egress 出口级瞬时失败计数（egress.go，键 = 出口身份）。运行态，不持久化：
	// 链路状态易失，重启后从零开始是正确的。
	egress map[string]egressStat

	// strategy 选号策略（weighted / round_robin / sequential，见 strategy.go）。
	// 空 = weighted（既有行为）。它只决定「同样合格的候选里选哪个」——健康、冷却、
	// 在途、积分保底、成本分层、账号优先级全部先于它生效，策略无法绕过它们。
	strategy string
	// scheduling 策略游标：(realm, 模型) → 上次用到的账号。运行态不持久化
	// （重启从环头重来，只影响下一次的起点，不影响正确性）。
	scheduling map[string]scheduleCursor
	// degradeThreshold / degradeCooldown / degradeCooldownMax 连败降权参数
	// （SetDegrade 注入；默认值见 defaultDegrade*，issue #114）。
	degradeThreshold   int
	degradeCooldown    time.Duration
	degradeCooldownMax time.Duration
	// tierFile / tier：账号等级闸门（modeltier.go）。tier 惰性加载，nil = 还没读过。
	tierFile string
	tier     *tierFacts
	// creditFloor 积分保底（SetCreditFloor 注入；0 = 关闭，缺省即现状）。
	// 账号 credits < floor 时不再参与选号——防止收费请求把余额打穿、连免费模型都
	// 402 冷却到次日签到。签到回血（SetCreditsDetailed）越过 floor 即自动恢复。
	// 全池触底且无免费模型可接时选号返回 nil（硬语义：宁 503 不打穿）。
	// 收费与否的判据见 floorBlockedForModel：本地实测台账优先，缺失时用上游目录
	// 倍率（modelRateOf 注入）兜底，避免「无观测的高价新模型」绕过保底。
	creditFloor int64
	// creditFloorByRealm 按域覆盖 creditFloor（issue #147；键 "cn"/"global"）。
	// nil/空表 = 全部回落全局阈值。
	creditFloorByRealm map[string]int64
	// creditFloorMaxRate 积分保底允许的**目录倍率上限**（issue #30 / PR #2）：
	// 触底号上只放行倍率 <= 该值的便宜模型（0.2 → x0.11 放行、x1.62 仍拦），
	// 0 = 严格模式（任何已知正倍率都拦）。只影响 tier 1（无本地实测）判定。
	creditFloorMaxRate float64
	// reserveCredits 保留积分阈值（SetReserveCredits 注入；0 = 关闭）：余额已知且
	// <= 该值的账号停止接单（仍在池中、照常签到/旅行，充值后自动恢复）。
	// 上游余额耗尽会发提醒短信，设阈值即避免被用到 0。免费模型不受限（见 modelFreeOf）。
	reserveCredits int64
	// reserveByRealm 按域覆盖 reserveCredits（issue #147；键 "cn"/"global"）。
	// nil/空表 = 全部回落全局阈值。
	reserveByRealm map[string]int64
	// accountPriority 账号优先级（uid → 正整数，小 = 先消耗；issue #62）。
	// 配了的号构成「先烧层」，层内有号可用时其余号不接单；层内烧空/冷却/停牌后自动
	// 让位给下一层。空表 = 未配置，选号行为与旧版完全一致。
	accountPriority map[string]int
	// accountShare 账号占比（uid → 正权重）：同层内配了占比的号独占抽签，按归一化
	// 比例分流量（70/30 与 7/3 同义）。空表 = 未配置。
	accountShare map[string]float64
	// balanceWindow 窗口用量调度的窗口长度（issue #112）：>0 时按 (账号, 模型) 本窗口
	// 已花的 token 反比降权；0 = 关停（行为与旧版逐字节一致）。
	balanceWindow time.Duration
	// modelRateOf 按 (realm, 模型) 查上游目录积分倍率（"0.79" / "" = 未知）。
	// 由 main 用 upstream.Client.ModelRate 注入——pool 不依赖 upstream 包（避免
	// 循环依赖与分层破坏），nil 时倍率兜底不生效（退化为仅本地台账判定）。
	// 仅在持 p.mu 时由 floorBlockedForModel 调用；回调不得反向调用 Pool 方法。
	modelRateOf func(realm, model string) string
	// exclusiveModelOf 报告 (域, 模型) 是否「免费号调不通」（modelTiers accessMode=exclusive）；
	// exclusivePaidOf 报告 uid 是否付费号（第二返回值 false = 不知道）。
	// 同为注入项、同为 nil = 关闭（缺省）。只在持锁时由 exclusiveBlockedForRealmModel 调用。
	exclusiveModelOf func(realm, model string) bool
	exclusivePaidOf  func(uid string) (bool, bool)
	// 加权路由的闲置补偿调优（SetWeights 注入；默认值见 defaultIdle*）。
	idleWeightPerHour float64
	idleWeightMax     float64
	// preferExpiring 最早到期优先路由开关（默认 true）。开启且快过期窗口内存在有效
	// 批次时，选号在成本层内先按最早到期排序；关闭后只使用普通加权路由。
	preferExpiring bool
	// maxInFlight 单账号最大在途请求数；0 = 不限（租约关闭）。
	maxInFlight int
	// maxInFlightGlobal global 域单账号在途上限分档（WAF 403 修复 P1-1：global 域
	// WAF 风控更紧，压低并发）；0 = 未设置，回落 maxInFlight（不分档，零回归）。
	maxInFlightGlobal int
	// randInt64N 仅供测试注入确定性随机源；nil 时用 math/rand/v2 全局源。
	// 生产代码不应设置此字段。
	randInt64N func(n int64) int64
	// persistFails 本地 state.json 连续落盘失败计数（仅 saveLocked 在持锁下读写，无需 atomic）。
	// 用于落盘失败的日志节流：首败/每 N 次提醒/恢复各打一条，避免磁盘满时刷屏。
	persistFails int
	// pickSeq 单调递增的选号序号：每次 pick 选中账号时自增并记到 entry.usedSeq，
	// 为 LRU 兜底/防惊群提供与 time.Now() 精度无关的严格全序（Windows ~0.5ms 精度下
	// lastUsed 墙钟会全等）。仅 pick 写锁路径读写，无需 atomic。
	pickSeq uint64
	// stopCh 关闭信号：Close 关闭它使 startFlusher 的后台 goroutine 退出。
	// nil = 未启动 flusher（stateFp 为空时 New 不起 flusher）。
	stopCh chan struct{}
	// closeOnce 保证 Close 幂等（多次调用不重复 close channel）。
	// lastFallbackLog 上一次打过兜底选号日志的账号（bug-10：每请求路径要节流）。
	lastFallbackLog string
	closeOnce       sync.Once
}

// New 构建账号池（stateFp 为空 = 不落盘，纯内存，测试用）。
func New(stateFp string) *Pool {
	// 账号等级闸门（modeltier.go）：与 state 文件同目录的独立小文件，不动 state schema。
	tierFile := tierFilePath(stateFp)
	p := &Pool{
		byUID:              map[string]*entry{},
		stateFp:            stateFp,
		breakerThreshold:   defaultBreakerThreshold,
		breakerCooldown:    defaultBreakerCooldown,
		breakerCooldownMax: defaultBreakerCooldownMax,
		idleWeightPerHour:  defaultIdleWeightPerHour,
		idleWeightMax:      defaultIdleWeightMax,
		preferExpiring:     true,
		degradeThreshold:   defaultDegradeThreshold,
		degradeCooldown:    defaultDegradeCooldown,
		degradeCooldownMax: defaultDegradeCooldownMax,
		// 探索缺省 30m：tier 0 垄断下的 tier 1 探索窗口（issue #136）。用户经
		// config 显式 "0" 关停（SetCostExploreInterval(0)）。
		costExploreInterval: defaultCostExploreInterval,
		exploreLast:         map[string]time.Time{},
		strategy:            StrategyWeighted,
		scheduling:          map[string]scheduleCursor{},
	}
	if stateFp != "" {
		p.load()
		p.startFlusher()
	}
	p.tierFile = tierFile
	// 构造时就把等级事实读进内存（文件很小、每进程一次）：ModelDeniedForAllAccounts
	// 在 /v1/models 与面板模型页的热路径上被调用，不该在那里做惰性 IO。
	//
	// 显式持锁调用（bug D3）：loadTierLocked 的注释要求「调用方必须持 p.mu」，
	// 构造期单线程所以现在安全，但契约已经失效 —— 以后 New() 里插入任何 goroutine
	// 或把 New 改成可并发调用，这里就是对 p.tier 的数据竞争。
	p.mu.Lock()
	p.loadTierLocked()
	p.mu.Unlock()
	return p
}

// Close 停止后台落盘 goroutine 并做最后一次落盘（幂等）。
// 进程退出前调用，消除 startFlusher 的 goroutine 泄漏；不调用也不影响正确性
// （进程退出即回收），仅是生命周期卫生。
func (p *Pool) Close() {
	if p.stopCh == nil {
		return
	}
	p.closeOnce.Do(func() {
		close(p.stopCh)
	})
	p.Flush()
}

// SetBreaker 注入熔断器参数（main 从 config 解析后调用）。非正值保留原值（用默认）。
func (p *Pool) SetBreaker(threshold int, cooldown, cooldownMax time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if threshold > 0 {
		p.breakerThreshold = threshold
	}
	if cooldown > 0 {
		p.breakerCooldown = cooldown
	}
	if cooldownMax > 0 {
		p.breakerCooldownMax = cooldownMax
	}
}

// SetSoftRateMax 注入软冷却指数退避的封顶时长（main 从 config 解析后调用）。
// 非正值保留原值（用默认 2h），风格同 SetBreaker。
func (p *Pool) SetSoftRateMax(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d > 0 {
		p.softRateMax = d
	}
}

// SetSoftResetMax 注入上游权威重置墙钟的冷却上限（cooldown.soft_reset_max）。
// 与 SetSoftRateMax 分工：前者管「上游明确说了何时恢复」，后者管「不知道、按退避」。
// 非正值忽略（保持默认），与 SetSoftRateMax 同口径。
func (p *Pool) SetSoftResetMax(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d > 0 {
		p.softResetMax = d
	}
}

// SetCostExploreInterval 注入 costTier 条件探索窗口（main 从 config 解析后调用，
// issue #136）。0 = 关停（完全回到现状行为）；正值覆盖默认 30m。
// 注意：与 SetSoftRateMax「非正值保留默认」不同，0 在这里是**合法值**（关停开关，
// 与 config 的 "0" 关停语义对齐）——不设 0 语义就无法关停探索。
func (p *Pool) SetCostExploreInterval(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d < 0 {
		return // 负值非法，保留现值
	}
	p.costExploreInterval = d
}

// CostExploreStatus 透出探索台账（/status 用）：累计探索事件数 + 各 (域, 模型)
// 的最近探索时刻（键内 \x1f 分隔符输出为 "|"，与 model_costs 行对照即可读出
// 「探索→毕业」全链路）。RLock 只读遍历；map 大小受「服务过的 (域, 模型)」集合
// 约束（与 modelCost 同界，天然有界）。
func (p *Pool) CostExploreStatus() (events int64, last map[string]time.Time) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	last = make(map[string]time.Time, len(p.exploreLast))
	for k, ts := range p.exploreLast {
		// 键 realm+"\x1f"+model → 输出 "|"（JSON 安全可读；\x1f 不可打印）。
		last[strings.ReplaceAll(k, "\x1f", "|")] = ts
	}
	return p.costExploreEvents, last
}

// SetWeights 注入加权路由的闲置补偿参数。非正值保留原值（用默认）。
func (p *Pool) SetWeights(idlePerHour, idleMax float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if idlePerHour > 0 {
		p.idleWeightPerHour = idlePerHour
	}
	if idleMax > 0 {
		p.idleWeightMax = idleMax
	}
}

// SetPreferExpiring 注入最早到期优先路由开关（main 从 config 解析后调用）。
func (p *Pool) SetPreferExpiring(enabled bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.preferExpiring = enabled
}

// SetDegrade 注入连败降权参数（main 从 config 解析后调用，issue #114）。
// 非正值保留原值（用默认，见 defaultDegrade*），风格同 SetBreaker/SetSoftRateMax。
func (p *Pool) SetDegrade(threshold int, cooldown, cooldownMax time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if threshold > 0 {
		p.degradeThreshold = threshold
	}
	if cooldown > 0 {
		p.degradeCooldown = cooldown
	}
	if cooldownMax > 0 {
		p.degradeCooldownMax = cooldownMax
	}
}

// CreditFloor 透出生效的积分保底值（/status 用）。0 = 关闭。
func (p *Pool) CreditFloor() int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.creditFloor
}

// SetCreditFloorByRealm 注入按域覆盖表（issue #147；键 "cn"/"global"）。
// nil/空表 = 全部回落全局阈值。config 校验已剔非法键与负值，这里不再防御。
func (p *Pool) SetCreditFloorByRealm(m map[string]int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.creditFloorByRealm = m
}

// SetCreditFloorMaxRate 注入积分保底的目录倍率上限（0 = 严格模式）。
func (p *Pool) SetCreditFloorMaxRate(v float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v < 0 {
		v = 0
	}
	p.creditFloorMaxRate = v
}

// SetReserveCredits 注入保留积分阈值（0 = 关闭）。
func (p *Pool) SetReserveCredits(v int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v < 0 {
		v = 0
	}
	p.reserveCredits = v
}

// SetReserveCreditsByRealm 注入按域覆盖表（键 "cn"/"global"）。
func (p *Pool) SetReserveCreditsByRealm(m map[string]int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reserveByRealm = m
}

// SetAccountRouting 注入账号级路由偏好（issue #62）：优先级（小 = 先烧）与占比。
// 空/nil = 未配置，选号行为与旧版完全一致。
func (p *Pool) SetAccountRouting(priority map[string]int, share map[string]float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.accountPriority = priority
	p.accountShare = share
}

// reserveThreshold 该域的保留积分阈值：按域覆盖优先，未写回落全局值。
// 调用方须持锁；realm 为空当 "cn"（与 creditFloorFor 同口径）。
func (p *Pool) reserveThreshold(realm string) int64 {
	if realm == "" {
		realm = "cn"
	}
	if v, ok := p.reserveByRealm[realm]; ok {
		return v
	}
	return p.reserveCredits
}

// priorityOf / shareOf 读账号级路由偏好，未配置 → 0。调用方须持锁。
func (p *Pool) priorityOf(uid string) int  { return p.accountPriority[uid] }
func (p *Pool) shareOf(uid string) float64 { return p.accountShare[uid] }

// creditFloorFor 该域的积分保底阈值：按域覆盖优先，未写回落全局值。
// 调用方须持锁；realm 为空当 "cn"（与 Auth.Realm 的归一化口径一致）。
func (p *Pool) creditFloorFor(realm string) int64 {
	if realm == "" {
		realm = "cn"
	}
	if v, ok := p.creditFloorByRealm[realm]; ok {
		return v
	}
	return p.creditFloor
}

// SetCreditFloor 注入积分保底线（main 从 config 解析后调用）。
// 0 = 关闭（缺省即现状，零回归）；负值非法保留原值（0）。
// 语义见 Pool.creditFloor 字段注释。
func (p *Pool) SetCreditFloor(n int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n >= 0 {
		p.creditFloor = n
	}
}

// SetModelRateOf 注入上游目录积分倍率查表（main 用 upstream.Client.ModelRate 装配）。
// 供积分保底兜底判定「未实测过的模型是否收费」——本地台账无观测时，不能因为
// 「没学过」就放行，否则高价新模型会把触底号一次性打穿（kimi-k3-1 实案：
// 全池无观测 → 保底全部放行 → 两笔扣 111 分打穿到 0 并硬冷却到次日 04:00）。
// fn 可为 nil（清注入）；回调只在持 p.mu 时被调用，不得反向调用 Pool 方法。
func (p *Pool) SetModelRateOf(fn func(realm, model string) string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.modelRateOf = fn
}

// SetExclusiveModels 注入「免费号调不通」的模型查表与账号付费状态查表
// （main 用 upstream.Client.ModelExclusive / PaidUser 装配）。两者任一为 nil 即关闭闸门。
//
// 只在**知道**账号免费时才跳过：没观测过付费状态的号照旧参与（宁可按现状打一次上游，
// 也不因为缺数据把能用的号排除）。
func (p *Pool) SetExclusiveModels(modelOf func(realm, model string) bool, paidOf func(uid string) (bool, bool)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.exclusiveModelOf, p.exclusivePaidOf = modelOf, paidOf
}

// SetMaxInFlight 注入单账号最大在途请求数；0 = 不限。负值保留原值。
func (p *Pool) SetMaxInFlight(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n >= 0 {
		p.maxInFlight = n
	}
}

// SetMaxInFlightGlobal 注入 global 域单账号在途上限（WAF 403 修复 P1-1 分档）；
// 0 = 未设置，global 账号回落 maxInFlight（不分档）。负值保留原值。
func (p *Pool) SetMaxInFlightGlobal(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n >= 0 {
		p.maxInFlightGlobal = n
	}
}

// inFlightLimit 报告账号的生效在途上限（global 分档优先，回落 maxInFlight）；
// 0 = 不限。调用方需已持 p.mu（或快照过 limit，见 Acquire）。
func (p *Pool) inFlightLimit(e *entry) int {
	if p.maxInFlightGlobal > 0 && e.a.Realm() == "global" {
		return p.maxInFlightGlobal
	}
	return p.maxInFlight
}

// SetStore 注入池状态快照镜像（redisstore.Store）。nil 表示不镜像（纯本地恢复）。
// 必须在 SyncToDir 之前调用，使"择新恢复"发生在账号对齐之前。
func (p *Pool) SetStore(s StoreSnapshotter) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.store = s
}

// Acquire 为 uid 占一个在途名额（会话粘性命中后调用）；池上限内返回 true。
// 名额用 entry.inFlight 原子自增，满额返回 false。上限按账号 realm 分档
// （global 档 maxInFlightGlobal，P1-1；未设置回落 maxInFlight）。
func (p *Pool) Acquire(uid string) bool {
	p.mu.RLock()
	e, ok := p.byUID[uid]
	if !ok {
		p.mu.RUnlock()
		return false
	}
	limit := p.inFlightLimit(e)
	p.mu.RUnlock()
	if limit <= 0 {
		// 不限：计数仍累加（供状态观测），但永不拒绝。
		e.inFlight.Add(1)
		return true
	}
	for {
		cur := e.inFlight.Load()
		if cur >= int64(limit) {
			return false
		}
		if e.inFlight.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

// Release 释放一个在途名额。幂等减到 0 为止（防重复释放扣成负数）。
func (p *Pool) Release(uid string) {
	p.mu.RLock()
	e, ok := p.byUID[uid]
	p.mu.RUnlock()
	if !ok {
		return
	}
	for {
		cur := e.inFlight.Load()
		if cur <= 0 {
			return
		}
		if e.inFlight.CompareAndSwap(cur, cur-1) {
			return
		}
	}
}

// SetRandomSource 仅供测试注入确定性随机源；生产代码不应调用。
// 注入源取 n∈[0,n) 后，pickWeighted 的抽签结果完全可预测。
func (p *Pool) SetRandomSource(fn func(n int64) int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.randInt64N = fn
}

// Add 加入账号；已存在则保留原状态、更新凭证（upsert 单账号，不影响其他账号）。
func (p *Pool) Add(a *auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.upsertLocked(a)
}

// SyncToDir 用最新扫描结果对齐池：新账号加入、消失的账号剔除（状态保留）。
// 剔除结果持久化回 state.json，避免已删账号在下次启动时被 load() 复活。
func (p *Pool) SyncToDir(auths []*auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// 防「扫到 0 个账号 → 把整池剔光并立刻落盘」（review P0-4）：
	// filepath.Glob 对不存在的目录**不报错**，只返回空列表，于是 auth_dir 配错
	// 或 Docker 卷没挂上时，这里会把 credits/冷却/熔断计数/成本账本一次性清零，
	// 而且 saveLocked 当场写进 state.json —— 重启也找不回来。
	if len(auths) == 0 && len(p.byUID) > 0 {
		log.Printf("WARN: [pool] auth 目录扫描到 0 个账号，但池内有 %d 个 —— 疑似 auth_dir 配错/卷未挂载，跳过剔除以免清空状态", len(p.byUID))
		return
	}
	seen := make(map[string]bool, len(auths))
	for _, a := range auths {
		seen[a.UID] = true
		p.upsertLocked(a)
	}
	changed := false
	for uid := range p.byUID {
		if !seen[uid] {
			delete(p.byUID, uid)
			changed = true
		}
	}
	if changed {
		p.saveLocked()
	}
}

// Remove 从池中移除账号并立即落盘（管理面板用）。返回被移除账号的凭证
// （含 FilePath，供调用方删除 auth 文件）；uid 不存在返回 nil。
// 在途请求的 Release 对已删条目是 no-op，无需等待。
func (p *Pool) Remove(uid string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return nil
	}
	delete(p.byUID, uid)
	p.dirty.Store(true)
	p.saveLocked()
	return e.a
}

// RemoveMany 批量移除（一次落盘），返回实际移除的 uid。热重载用：逐个 Remove 会
// 触发 N 次全量 saveLocked（每轮扫描都要写一遍 state.json，账号多时明显）。
func (p *Pool) RemoveMany(uids []string) []string {
	if len(uids) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	removed := make([]string, 0, len(uids))
	for _, uid := range uids {
		if _, ok := p.byUID[uid]; !ok {
			continue
		}
		delete(p.byUID, uid)
		removed = append(removed, uid)
	}
	if len(removed) > 0 {
		p.dirty.Store(true)
		p.saveLocked()
	}
	return removed
}

// InFlight 该账号当前在途请求数（热重载删除前的安全检查：在途时先不删）。
func (p *Pool) InFlight(uid string) int {
	p.mu.RLock()
	e, ok := p.byUID[uid]
	p.mu.RUnlock()
	if !ok || e == nil {
		return 0
	}
	return int(e.inFlight.Load())
}

// upsertLocked 更新或插入单个账号；已存在则只换凭证、保留 credits/cooling 状态。
// 调用方必须已持有 p.mu；Add 与 SyncToDir 共用此 upsert 逻辑。
func (p *Pool) upsertLocked(a *auth.Auth) {
	if e, ok := p.byUID[a.UID]; ok {
		e.a = a            // 保留 credits/cooling 状态
		e.needAuth = false // 拿到真实凭证：解除占位标记（bug P1-3）
		return
	}
	p.byUID[a.UID] = &entry{a: a}
}

// Pick 返回 healthy 中积分最高的账号；无可用返回 nil。

// defaultBalanceWindow 窗口用量调度的缺省窗口（1h）。
const defaultBalanceWindow = time.Hour

// DefaultBalanceWindow 缺省窗口（1h）。main 侧给「配置没写」用。
func DefaultBalanceWindow() time.Duration { return defaultBalanceWindow }

// SetBalanceWindow 热改用量窗口；<=0 关停（缺省 1h，见 defaultBalanceWindow）。
func (p *Pool) SetBalanceWindow(d time.Duration) {
	p.mu.Lock()
	p.balanceWindow = d
	p.mu.Unlock()
}

// BalanceWindow 当前用量窗口（0 = 关停）。
func (p *Pool) BalanceWindow() time.Duration {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.balanceWindow
}
