// 持久化：本地 state.json 落盘/加载、Redis 快照镜像（StoreSnapshotter）、
// 后台 flusher、择新恢复（RestoreFromSnapshot）。
package pool

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

var flushInterval = 5 * time.Second

// persistLogEvery 连续落盘失败每 N 次打一条提醒（flusher 5s 一把 ≈ 1 分钟一次），
// 避免磁盘持续满/权限丢失时日志刷屏。
const persistLogEvery = 12

// snapshot 池状态快照（Redis 镜像用）。与本地 state.json 同源（stateFile），
// 额外带 savedAt 时间戳供"择新恢复"（比较本地与 Redis 快照的新旧）。
type snapshot struct {
	stateFile
	SavedAt time.Time `json:"saved_at"`
}

// Pool 账号池。
type StoreSnapshotter interface {
	SaveState(data []byte)
	LoadState() ([]byte, bool)
}

// RestoreFromSnapshot 择新恢复：比较本地 state.json 与 Redis 快照，采用较新者。
//
// 本地**可用**时按新旧择一（快照不早于本地 → 采用快照，否则本地优先）；本地**不可用**
// （state.json 缺失或不可读，典型为首次在新卷/新节点启动）时**采用快照**——此时本地根本
// 没有可"优先"的状态，快照是本轮唯一的运行态来源，这正是快照作为「启动恢复备份」的核心
// 场景。分支情形：无快照 / 快照无 savedAt → 本地优先（无判据可比）。
//
// 每种情形都打一条对应的恢复来源日志，便于对账。必须在 SyncToDir 之前调用
// （SyncToDir 只增删不入值：值只能来自本地 load 或本函数采用快照）。
func (p *Pool) RestoreFromSnapshot() {
	// 读 store / stateFp：store 走 atomic.Pointer（W4-03），stateFp 构造后不再变、
	// 但仍在 RLock 内读，与 SetStore/其它注入保持同一套「不在锁外裸读字段」的纪律。
	p.mu.RLock()
	stateFp := p.stateFp
	p.mu.RUnlock()
	store := p.storeSnapshot()
	if store == nil || stateFp == "" {
		return
	}
	localInfo, localErr := os.Stat(stateFp)
	raw, ok := store.LoadState()
	if !ok {
		if localErr == nil {
			log.Printf("pool: 恢复来源=本地 state.json（无 Redis 快照）")
		}
		return
	}
	var snap snapshot
	if json.Unmarshal(raw, &snap) != nil || snap.SavedAt.IsZero() {
		// 快照无 savedAt：无法比较新旧，本地优先。
		log.Printf("pool: 恢复来源=本地 state.json（Redis 快照无 saved_at）")
		return
	}
	if localErr != nil {
		// 本地不可用 → 采用快照（本地没有可"优先"的状态）。
		//
		// 旧实现把该情形与「本地较新」合并成同一个 fall-through：既不改内存、不置 dirty
		//（有效快照被静默丢弃），又打出"本地 state.json（较新于 Redis 快照 …）"——一次
		// 从未发生过的比较，把排障引向根本不存在的本地文件；随后 SyncToDir 只增删不入值，
		// 全池运行态（credits/冷却/熔断计数/usedSeq/lastUsed）被清零。
		p.adoptSnapshot(snap)
		log.Printf("pool: 恢复来源=Redis 快照 (saved_at=%s)（本地 state.json 不可用: %v）",
			snap.SavedAt.Format(time.RFC3339), localErr)
		return
	}
	if !localInfo.ModTime().After(snap.SavedAt) {
		// 快照不早于本地 → 采用快照。
		p.adoptSnapshot(snap)
		log.Printf("pool: 恢复来源=Redis 快照 (saved_at=%s)", snap.SavedAt.Format(time.RFC3339))
		return
	}
	// 走到这里必然是「本地存在且严格新于快照」，日志结论属实。
	log.Printf("pool: 恢复来源=本地 state.json（较新于 Redis 快照 %s）", snap.SavedAt.Format(time.RFC3339))
}

// startFlusher 启动周期落盘协程（dirty 时 saveLocked，Close 时停止）。
func (p *Pool) startFlusher() {
	stopCh := make(chan struct{})
	// 在启动 goroutine 前同步写入（避免与测试对 flushInterval 的恢复写竞争）；
	// stopCh 同步登记，Close 才能可靠停止（New 与 startFlusher 之间无并发窗口）。
	// 写入持锁（审查 C5-41）：Close 在同一把锁内读，二者不再构成数据竞争。
	p.mu.Lock()
	p.stopCh = stopCh
	// 周期在 New 里一次性落到 Pool 字段（审查 C5-54）：flushInterval 是包级 var，
	// goroutine 每轮读它、测试并发改它就是数据竞争（-race 会报）。现在 goroutine
	// 只读自己的字段，包级 var 退化为「构造期缺省值」，测试仍可在 New 之前改它。
	interval := p.flushEvery
	if interval <= 0 {
		interval = flushInterval
	}
	p.mu.Unlock()
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				// 锁内只取快照，IO 在锁外（审查 C5-37）：失败时 flushNow 会把 dirty 放回去，
				// 下一轮 tick / 退出前还会再试（bug #1 的语义不变）。
				p.flushNow()
				// 顺带清理窗口表（审查 C5-67）：pruneWindowsLocked 原先只挂在
				// ScheduleModelWindow 里，不再出现 429 的部署里窗口表只增不减
				// （内存，落盘侧有过滤）。挂到 flusher tick 上零新增 goroutine。
				p.pruneWindowsTick()
			case <-stopCh:
				return
			}
		}
	}()
}

// Flush 同步把内存状态落盘（幂等：无变更不写盘）。供进程退出前调用。
func (p *Pool) Flush() { p.flushNow() }

// load 读取 state.json 到 p.byUID（文件不存在/损坏时静默从空状态开始）。
//
// 审查 C5-51：本函数调用 applyAccountsLocked / loadParkedLocked / loadWindowsLocked /
// applyRulesLocked 四个 **Locked 变体**，它们的注释都要求「调用方必须持 p.mu」，
// 而 load 自身此前不持锁 —— 目前只靠「New 单线程」成立，一旦 load 被复用于热重载
// 就是整池 map 并发写（fatal error: concurrent map writes）。这里显式加锁：
// New 期间无竞争，代价为零；契约也从「隐式单线程」变成「自带锁」。
func (p *Pool) load() {
	p.mu.Lock()
	defer p.mu.Unlock()
	raw, err := os.ReadFile(p.stateFp)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("WARN: [pool] state 不可读（以空状态启动）: %v", err)
		}
		return
	}
	// 每次尝试都解到**独立**结构（报告 #10）：主文件「前半段合法、后半段损坏」时
	// json.Unmarshal 已经填进去一部分字段，若继续往同一个 sf 里解 .bak，备份里没有的
	// 字段不会被清空 —— 恢复出来是两份文件的混合体（例如 credits 来自损坏文件、
	// 冷却来自备份）。
	var sf stateFile
	parseOK := json.Unmarshal(raw, &sf) == nil
	if !parseOK {
		// 主文件坏了 → 回退 .bak（同上）；两处都坏才按空池处理。
		if bak, berr := os.ReadFile(p.stateFp + ".bak"); berr == nil && len(bak) > 0 {
			var bf stateFile
			if json.Unmarshal(bak, &bf) == nil {
				log.Printf("WARN: [pool] state.json 解析失败，已回退 state.json.bak（%d 字节）", len(bak))
				sf = bf
				raw = bak
				parseOK = true
			}
		}
	}
	if !parseOK {
		// 损坏必须留档再启动（issue ISSUE-03）：此前静默 return，5 秒后 flusher 会把
		// 空状态写回 state.json —— 冷却时长、熔断计数、成本账本、credits、"已签到"
		// 全部不可恢复。另存一份 .corrupt-<unix> 至少还能人工捞。
		bak := fmt.Sprintf("%s.corrupt-%d", p.stateFp, time.Now().Unix())
		if rerr := os.Rename(p.stateFp, bak); rerr != nil {
			log.Printf("ERR: [pool] state.json 解析失败，且另存失败（%v）；本次以空状态启动，原文件保留", rerr)
		} else {
			log.Printf("ERR: [pool] state.json 解析失败，已另存 %s（本次以空状态启动）", bak)
		}
		return
	}
	p.applyAccountsLocked(sf.Accounts)
	p.loadParkedLocked(sf.Parked)
	p.loadWindowsLocked(sf.Windows, time.Now())
	p.applyRulesLocked(sf.ModelRules)
}

// loadParkedLocked 恢复暂存的号：复用 applyAccountsLocked 的整套清洗，再把结果挪进 parked。
// 同一个 uid 若也在 accounts 里（异常数据），以池里的为准。调用方持 p.mu。
func (p *Pool) loadParkedLocked(parked map[string]stateAccount) {
	if len(parked) == 0 {
		return
	}
	live := p.byUID
	p.byUID = map[string]*entry{}
	p.applyAccountsLocked(parked)
	restored := p.byUID
	p.byUID = live
	for uid, e := range restored {
		if _, inPool := live[uid]; inPool {
			continue
		}
		if len(p.parked) >= maxParked {
			// 审查 C5-55：此前静默丢弃剩余条目，重启后大批暂存号的禁用/分组/冷却
			// 状态凭空消失且无任何痕迹。至少留一条可对账的 WARN（每条只打一次，
			// 循环里用 continue 继续跑完，不影响其它条目）。
			log.Printf("WARN: [pool] 暂存区已满（%d 条），丢弃恢复条目 uid=%s（状态不再保留）", maxParked, logfmt.UID8(uid))
			continue
		}
		if p.parked == nil {
			p.parked = map[string]*entry{}
		}
		p.parked[uid] = e
	}
}

// applyRulesLocked 装载规则表并重算每个账号的运行态集合。调用方必须已持有 p.mu。
// 顺序上必须在 applyAccountsLocked 之后（账号引用先落地，再展开），但展开时不改写
// e.ruleIDs——否则「规则还没装载」会被当成「规则已被删除」，重启一次就丢引用。
func (p *Pool) applyRulesLocked(rules []ModelRule) {
	if p.rules == nil {
		p.rules = map[string]ModelRule{}
	}
	for _, r := range rules {
		if r.ID == "" || len(r.Models) == 0 {
			continue
		}
		if r.Mode != RuleModeAllow {
			r.Mode = RuleModeDeny
		}
		p.rules[r.ID] = r
	}
	p.recomputeRulesLocked()
}

// applyAccountsLocked 用持久化账号状态覆盖/插入 byUID（placeholder 凭证，Add 时换全）。
// 本地 load() 与 Redis 快照恢复共用；调用方必须已持有 p.mu。
func (p *Pool) applyAccountsLocked(accounts map[string]stateAccount) {
	now := time.Now()
	for uid, s := range accounts {
		// err_total 优先；旧文件的 err_count（连续错误）作一次性迁移源映射进来（二者取较大者，
		// 尽最大可能保留历史观测信号——旧语义下 err_count 也真实发生过错误，不应丢）。
		errTotal := s.ErrTotal
		if int64(s.ErrCount) > errTotal {
			errTotal = int64(s.ErrCount)
		}
		e := &entry{
			a:            &auth.Auth{UID: uid}, // placeholder，Add 时会换成完整凭证
			needAuth:     true,                 // 标记占位（bug P1-3）：拿到真实凭证前不参与路由
			remark:       s.Remark,
			group:        s.Group,
			ruleIDs:      append([]string(nil), s.ModelRules...),
			credits:      s.Credits,
			creditsTotal: s.CreditsTotal,
			// 余额已知标记（审查 C5-10）：旧文件没有该字段 → false，
			// reserveBlocked 回落 credits_total > 0 口径，与旧行为一致。
			creditsKnown:             s.CreditsKnown,
			creditsExpiring:          s.CreditsExpiring,
			creditsEarliestExpiry:    s.CreditsEarliestExpiry,
			creditsEarliestRemaining: s.CreditsEarliestRemaining,
			disabled:                 s.Disabled,
			paused:                   s.Paused,
			reason:                   s.Reason,
			disabledAt:               s.DisabledAt,
			until:                    s.Until,
			coolKind:                 s.CoolKind,
			successCount:             s.SuccessCount,
			errTotal:                 errTotal,
			lastErr:                  s.LastErr,
			lastSuccess:              s.LastSuccess,
			lastCheckinDay:           s.LastCheckinDay,
			tokenUsage:               s.TokenUsage,
			softStreak:               s.SoftStreak,
			sessionDeadFails:         s.SessionDeadFails,
			consecutiveFails:         s.ConsecutiveFails,
		}
		// 负余额钳 0（审查 C5-52）：SetCredits / SetCreditsDetailed 两个写入口都钳 0，
		// 唯独恢复路径直接 `credits: s.Credits` —— 脏文件/手改/旧版本写下的负数会
		// 带进权重与保底判定：weightOf 的 credits 项变负（权重可能 <= 0）、
		// floorBlocked 恒真（credits < floor 永远成立）→ 该号永久不出池，
		// 而且面板显示负余额，运维完全看不出是数据脏而不是真的欠费。
		if e.credits < 0 {
			e.credits = 0
		}
		// 到期快照按当前时刻惰性清洗：已过期、零剩余或超出总余额的脏数据不恢复。
		if e.creditsExpiring < 0 {
			e.creditsExpiring = 0
		}
		if e.creditsExpiring > e.credits {
			e.creditsExpiring = e.credits
		}
		if e.creditsEarliestRemaining < 0 || e.creditsEarliestRemaining > e.credits {
			e.creditsEarliestRemaining = 0
		}
		if e.creditsEarliestRemaining == 0 || e.creditsEarliestExpiry.IsZero() || !now.Before(e.creditsEarliestExpiry) {
			e.creditsEarliestExpiry = time.Time{}
			e.creditsEarliestRemaining = 0
		}
		// 熔断器持久化恢复：breakerUntil 未过期才恢复（过期不复活），retryCount 仅在
		// 熔断仍有效时保留（否则归零，不保留无用退避指数）。
		if s.BreakerUntil != nil && now.Before(*s.BreakerUntil) {
			e.breakerUntil = *s.BreakerUntil
			e.retryCount = s.RetryCount
		}
		// 连败降权：未过期才恢复（过期/零值不写不复活）。
		if s.DegradeUntil != nil && now.Before(*s.DegradeUntil) {
			e.degradeUntil = *s.DegradeUntil
		}
		// 模型级独立冷却（6004 重置墙钟 / 11102 负缓存）：惰性过滤已过期条目。
		if len(s.ModelCooldowns) > 0 {
			for m, mc := range s.ModelCooldowns {
				if mc.Until.IsZero() || !now.Before(mc.Until) {
					continue
				}
				if e.modelCooldowns == nil {
					e.modelCooldowns = map[string]modelCooldown{}
				}
				e.modelCooldowns[m] = modelCooldown{Until: mc.Until, ResetAt: mc.ResetAt, Reason: mc.Reason, AuditOnly: mc.AuditOnly}
			}
		}
		// 手动关闭 (账号, 模型)：**无过期过滤**——它是永久语义，
		// 与冷却（过期即弃）不同，不得被惰性清理顺手删掉。
		// 键在恢复时归一化（审查 C5-90）：旧文件里可能是 "GPT-4o" 或带 realm 前缀的
		// 形态，而读侧一律按 modelOffKey 查 —— 不迁移就等于把旧开关全部作废。
		// 归一后可能撞键（"GPT-4o" 与 "gpt-4o" 并存），保留 Since 更早的那条更符合
		// 「运维第一次关掉它」的语义。
		if len(s.ModelOff) > 0 {
			e.modelOff = map[string]modelOff{}
			for m, o := range s.ModelOff {
				key := modelOffKey(m)
				if key == "" {
					continue
				}
				if prev, dup := e.modelOff[key]; dup && prev.Since.Before(o.Since) {
					continue
				}
				e.modelOff[key] = modelOff{Since: o.Since, Note: o.Note}
			}
			if len(e.modelOff) == 0 {
				e.modelOff = nil
			}
		}
		// 成本账本：惰性过滤过期（modelCostTTL 外不恢复）+ 剔除结构破损条目
		// （负 per1k / 零 LastSeen——上游异常或旧文件手改产生的脏数据）。
		if len(s.ModelCosts) > 0 {
			for m, mc := range s.ModelCosts {
				if mc.LastSeen.IsZero() || now.Sub(mc.LastSeen) > modelCostTTL || mc.CostPer1k < 0 {
					continue
				}
				if e.modelCost == nil {
					e.modelCost = map[string]modelCostEntry{}
				}
				e.modelCost[m] = modelCostEntry{CostPer1k: mc.CostPer1k, LastSeen: mc.LastSeen, Samples: mc.Samples}
			}
		}
		p.byUID[uid] = e
	}
}

// adoptSnapshot 采用 Redis 快照为当前池状态，并置 dirty 让下一次落盘把它物化回本地
// state.json（否则快照只在内存生效，下次崩溃恢复又回到旧本地文件）。
func (p *Pool) adoptSnapshot(s snapshot) {
	p.mu.Lock()
	p.applySnapshotLocked(s)
	p.mu.Unlock()
	p.dirty.Store(true)
}

// applySnapshotLocked 用 Redis 快照覆盖内存状态（已在择新判定后采用）。调用方必须已持有 p.mu。
//
// 运行态保留（审查 C5-50）：重建 byUID 会把 inFlight / lastUsed / usedSeq 一起丢掉。
// 快照恢复发生在进程已开始接单之后时（例如 SetStore 晚于 SyncToDir 的装配顺序、
// 或运行期手动恢复），在途计数被清零 → 并发上限瞬间失效；随后在途请求的 Release
// 会把别人的名额扣掉（Release 的钳 0 只是掩盖，不是修复）。
// 因此对同名账号把三项运行态**从旧条目搬过来**：它们是本进程内的观测值，
// 快照里本来就没有，也没有比"当前内存"更权威的来源。
func (p *Pool) applySnapshotLocked(s snapshot) {
	prev := p.byUID
	p.byUID = map[string]*entry{}
	p.parked = nil
	p.applyAccountsLocked(s.Accounts)
	for uid, e := range p.byUID {
		if old, ok := prev[uid]; ok && old != nil {
			e.inFlight.Store(old.inFlight.Load())
			e.lastUsed = old.lastUsed
			e.usedSeq = old.usedSeq
		}
	}
	p.loadParkedLocked(s.Parked)
	p.loadWindowsLocked(s.Windows, time.Now())
	p.applyRulesLocked(s.ModelRules)
}

// snapshotBytes 把一份**已在锁内取好的** stateFile 序列化成 state.json 字节
// （纯内存操作，不碰磁盘、不持锁）。
//
// 为什么接收 sf 而不是自己再取一次（审查 C5-53）：此前 flushNowErr 先调
// stateOverviewLocked 拿镜像用的 sf，再调 snapshotLocked —— 后者内部又调一遍
// stateOverviewLocked，于是**写锁内把全池账号的 Status/成本账本/冷却账本构建两遍**。
// 账号多、模型冷却条目多时这是可见的 CPU 尖峰，而这段时间 pick/status/healthz 全部
// 排队。现在一次取态、两处复用。
//
// 为什么要拆开（审查 C5-37）：落盘 IO 必须在锁外做 —— MkdirAll、
// ReadFile(.bak 前置读)、ReplaceAtomic(.bak)、ReplaceAtomic(state.json)，每个都带
// fsync。磁盘慢或 bind mount 时单次可达几十毫秒，期间整个账号池被堵死：选号、冷却、
// 在途释放全部排队，表现为面板「偶发整秒卡顿」与请求尖刺。
func (p *Pool) snapshotBytes(sf stateFile) ([]byte, error) {
	raw, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// writeSnapshot 把快照字节落到 state.json（**不加 p.mu**：纯 IO）。
// 失败时把脏标记放回去，下一轮 tick / 退出前还会再试（bug #1）。
func (p *Pool) writeSnapshot(raw []byte) error {
	if p.stateFp == "" || raw == nil {
		return nil
	}
	if dir := filepath.Dir(p.stateFp); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	// 公共原子替换（含单文件 bind mount 的 EBUSY 兜底，bug #4）：
	// Docker 用 -v ./data/state.json:/app/data/state.json 时 rename 恒 EBUSY，
	// 原先直接失败 → 冷却/熔断/成本账本只留内存，重启全丢。
	// 先把**上一版**内容留一份 .bak（misakano7545 `4356a346`）：state.json 被外部
	// 截断/写坏时能回退，而不是整段运行态清零。
	if prev, rerr := os.ReadFile(p.stateFp); rerr == nil && len(prev) > 0 {
		// 只备份**合法**的上一版：state.json 可能是被外部截断/写坏的残骸，而我们刚刚
		// 才从 .bak 恢复出来 —— 这时把残骸写进 .bak，就等于把唯一的退路也污染掉。
		// 紧接着若主文件写入失败（磁盘满 / 单文件 bind mount EBUSY），两份都是坏的、
		// corrupt-* 也没生成，冷却截止、熔断计数、成本账本、credits、「已签到」全部
		// 不可恢复。
		if json.Valid(prev) {
			_ = auth.ReplaceAtomic(p.stateFp+".bak", prev, 0o600)
		} else {
			log.Printf("WARN: [pool] state.json 内容非法（%d 字节），跳过 .bak 覆盖：保留上一份可恢复备份", len(prev))
		}
	}
	if err := auth.ReplaceAtomic(p.stateFp, raw, 0o600); err != nil {
		p.notePersistFail(err)
		return err
	}
	if n := p.persistFails.Load(); n > 0 {
		// 从连续失败中恢复：打一条恢复日志，避免"错误打完却无人知道已恢复"。
		log.Printf("pool: state.json 落盘恢复（此前连续失败 %d 次）", n)
		p.persistFails.Store(0)
	}
	return nil
}

// mirrorToStore 把快照镜像到 Redis（fire-and-forget，**不加 p.mu**）。
// 与本地 state.json 并存作恢复备份；锁外做，避免 Redis 客户端抖动传导到账号池。
//
// W4-03：store 经 atomic.Pointer 读 —— SetStore 热切换与这里的无锁读并发时，
// 裸接口字段会读到撕裂值（nil 或旧 store），镜像写丢/写错目标。
func (p *Pool) mirrorToStore(sf stateFile) {
	store := p.storeSnapshot()
	if store == nil {
		return
	}
	raw, err := json.Marshal(snapshot{stateFile: sf, SavedAt: time.Now()})
	if err != nil {
		return
	}
	store.SaveState(raw)
}

// flushNow 周期性落盘入口（flusher / 退出前）：**只在有变更时**写。
func (p *Pool) flushNow() {
	if !p.dirty.Load() {
		return
	}
	_ = p.flushNowErr()
}

// flushNowErr 无条件写一次（保留原 saveLocked 的语义：调用方要的是「现在就落盘」，
// 例如面板删除账号、热重载剔除、以及直接调它的测试）。错误交给调用方。
// 锁内只取快照，IO 全在锁外（审查 C5-37）。
//
// C-02：整段「取快照 + 落盘 + 镜像」由独立的 flushMu 串行化。只把 p.mu 用在
// 取快照上（下面的注释解释了为什么不能持 p.mu 做 IO）并不够 —— 两个并发 flush
// 的快照之间没有全序，旧快照可能后落盘，把 state.json 写回上一版。
func (p *Pool) flushNowErr() error {
	p.flushMu.Lock()
	defer p.flushMu.Unlock()
	return p.flushLocked()
}

// flushLocked 是 flushNowErr 的本体；调用方必须已持有 p.flushMu（C-02）。
// 拆出来是给 Close 用的：Close 要先关 stopCh、再在同一把锁内做最后一次落盘，
// 不能经由 flushNowErr 二次抢锁（sync.Mutex 不可重入）。
func (p *Pool) flushLocked() error {
	p.mu.Lock()
	p.dirty.Store(false)
	// 一次取态，两处复用（审查 C5-53）：sf 同时供落盘序列化与 Redis 镜像，
	// 不再各取一遍（原实现写锁内构建两次全池状态）。
	sf := p.stateOverviewLocked()
	needDisk := p.stateFp != ""
	p.mu.Unlock()
	// 测试注入的同步点（C-02 变异验证）：位于「快照已取、尚未落盘」之间。
	// 生产路径 flushHook 恒 nil，零开销。
	if hook := p.flushHook.Load(); hook != nil {
		(*hook)()
	}
	var raw []byte
	if needDisk {
		b, err := p.snapshotBytes(sf)
		if err != nil {
			p.notePersistFail(err)
			p.dirty.Store(true)
			return err
		}
		raw = b
	}
	if werr := p.writeSnapshot(raw); werr != nil {
		p.dirty.Store(true)
		return werr
	}
	p.mirrorToStore(sf)
	return nil
}

// notePersistFail 记录一次本地 state.json 落盘失败，并按节流规则决定是否打日志：
// 首败（状态成功→失败）打完整错误、每 persistLogEvery 次连续失败打一条提醒、
// 其余连续失败静默（flusher 5s 一把，磁盘持续满时不刷屏）。
// 恢复成功的日志由 saveLocked 在成功路径统一打。与 redisstore 三处异步写的
// "失败仅打日志、不向上抛"范式对齐，但落盘失败对运维是盲区，故多一层节流（notification）。
func (p *Pool) notePersistFail(err error) {
	// W4-08：读-改-写一次完成（Load 之后再 Add 会让两条并发失败路径都读到旧值，
	// 日志节流的「每 N 次」判断失真）。
	n := p.persistFails.Add(1)
	if n == 1 {
		log.Printf("pool: state.json 落盘失败: %v", err)
	} else if n%persistLogEvery == 0 {
		log.Printf("pool: state.json 连续落盘失败 %d 次: %v", n, err)
	}
}

// stateOverviewLocked 收集当前内存状态为 stateFile（供落盘 + 快照镜像复用）。调用方必须已持 p.mu。
func (p *Pool) stateOverviewLocked() stateFile {
	now := time.Now()
	sf := stateFile{Accounts: map[string]stateAccount{}}
	for uid, e := range p.byUID {
		sf.Accounts[uid] = stateOf(e, now)
	}
	// 热重载暂存的号也落盘：进程在文件缺席期间重启，文件回来时禁用、暂停、分组照样复活。
	for uid, e := range p.parked {
		if sf.Parked == nil {
			sf.Parked = map[string]stateAccount{}
		}
		sf.Parked[uid] = stateOf(e, now)
	}
	sf.Windows = p.windowsStateLocked(now)
	// 规则表整体写出：规则本体 + 每个账号的引用都在一张 state.json 里，
	// 单文件快照才不会出现「账号指向了不存在的规则」。
	if len(p.rules) > 0 {
		ids := make([]string, 0, len(p.rules))
		for id := range p.rules {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		sf.ModelRules = make([]ModelRule, 0, len(ids))
		for _, id := range ids {
			sf.ModelRules = append(sf.ModelRules, p.rules[id])
		}
	}
	return sf
}

// stateOf 一个条目的持久化形态（落盘与快照镜像共用，池内号与暂存号同一套清洗规则）。
// 调用方持 p.mu。
func stateOf(e *entry, now time.Time) stateAccount {
	s := stateAccount{
		Remark:                   e.remark,
		Group:                    e.group,
		ModelRules:               append([]string(nil), e.ruleIDs...),
		Credits:                  e.credits,
		CreditsTotal:             e.creditsTotal,
		CreditsKnown:             e.creditsKnown, // 审查 C5-10：保底判据要跨重启保留
		Disabled:                 e.disabled,
		Paused:                   e.paused,
		SuccessCount:             e.successCount,
		ErrTotal:                 e.errTotal,
		LastSuccess:              e.lastSuccess,
		LastErr:                  e.lastErr,
		LastCheckinDay:           e.lastCheckinDay,
		TokenUsage:               e.tokenUsage,
		SoftStreak:               e.softStreak,
		SessionDeadFails:         e.sessionDeadFails,
		ConsecutiveFails:         e.consecutiveFails,
		CreditsExpiring:          e.creditsExpiring,
		CreditsEarliestExpiry:    e.creditsEarliestExpiry,
		CreditsEarliestRemaining: e.creditsEarliestRemaining,
	}
	// 账号级冷却字段：仅禁用或仍在冷却时限内落盘（与熔断/降权同款惰性清洗）。
	// 软冷却按墙钟到期、字段本身不清理，无条件落盘会让过期 reason 在 state.json 里
	// 长生不老并被重启原样拾回（issue #207-4）。
	if e.disabled || (!e.until.IsZero() && now.Before(e.until)) {
		s.Reason, s.Until, s.CoolKind = e.reason, e.until, e.coolKind
		s.DisabledAt = e.disabledAt
	}
	// 熔断截止：仅未过期才落盘（指针 nil 才能被 omitempty 真省略）。
	if !e.breakerUntil.IsZero() && now.Before(e.breakerUntil) {
		u := e.breakerUntil
		s.BreakerUntil = &u
		s.RetryCount = e.retryCount
	}
	// 连败降权截止：仅未过期才落盘。
	if !e.degradeUntil.IsZero() && now.Before(e.degradeUntil) {
		u := e.degradeUntil
		s.DegradeUntil = &u
	}
	// 模型级独立冷却：惰性过滤已过期条目（Hits 不落盘，重启后 11102 退避从基数重学）。
	if len(e.modelCooldowns) > 0 {
		for m, mc := range e.modelCooldowns {
			if mc.Until.IsZero() || !now.Before(mc.Until) {
				continue
			}
			if s.ModelCooldowns == nil {
				s.ModelCooldowns = map[string]stateModelCooldown{}
			}
			s.ModelCooldowns[m] = stateModelCooldown{Until: mc.Until, ResetAt: mc.ResetAt, Reason: mc.Reason, AuditOnly: mc.AuditOnly}
		}
	}
	// 手动关闭：全量写出（无过期概念，永久保留）。
	if len(e.modelOff) > 0 {
		s.ModelOff = map[string]stateModelOff{}
		for m, o := range e.modelOff {
			s.ModelOff[m] = stateModelOff{Since: o.Since, Note: o.Note}
		}
	}
	// 成本账本：惰性过滤过期观测（modelCostTTL 外不写——陈旧价格不复活）。
	if len(e.modelCost) > 0 {
		for m, mc := range e.modelCost {
			if mc.LastSeen.IsZero() || now.Sub(mc.LastSeen) > modelCostTTL {
				continue
			}
			if s.ModelCosts == nil {
				s.ModelCosts = map[string]stateModelCost{}
			}
			s.ModelCosts[m] = stateModelCost{CostPer1k: mc.CostPer1k, LastSeen: mc.LastSeen, Samples: mc.Samples}
		}
	}
	return s
}
