// Package scheduler 定时任务：签到 / 活跃上报 / 猫猫旅行 / token keepalive 四类独立排程。
// 签到成功后重新查余额，余额 > 0 的冷却账号自动解冻。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/cst"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// Config 调度器依赖。
//
// 任务开关用「禁用」命名而非「启用」：零值 Config 即四类任务都启用（hours 回落默认），
// 与引入开关前的行为逐字一致（老调用方/老测试无需改动）。
type Config struct {
	Pool     *pool.Pool
	Upstream *upstream.Client
	// AccountSpreadSeconds 账号遍历型任务的账号间最小间隔（秒），0 = 关闭。
	AccountSpreadSeconds int
	CheckinHours         []int // 默认 [9, 21]
	TravelHours          []int // 默认 [9,21]：一趟派出 + 一趟领奖闭环
	ActivityHours        []int // 默认 [10]
	KeepaliveHours       []int // 默认 [22]
	BlackcatHours        []int // 默认 [23]：夜猫子（23:00–08:00 计数窗口）
	GrowthHours          []int // 默认 [1]：成长任务队列（Sequential 族每日零点解锁一环，
	// 01:00 自动扫描+执行；避开零点整防解锁竞态）

	// ExpiringSoonWindow 快过期积分窗口：签到/余额刷新查余额时，把到期时间
	// <= now+window 的套餐余额标记为"快过期"（pool 据此做最早到期优先，见
	// entry.creditsEarliestExpiry）。<=0 时不做路由门槛；展示仍使用完整逐包数据。
	ExpiringSoonWindow time.Duration

	// CheckinDisabled 显式关闭签到排程（对应 config 的 schedule.checkin_enabled=false）。
	// 禁用后不再有任何签到时点。旅行不再搭签到便车（已剥离为独立排程）。
	CheckinDisabled bool
	// TravelDisabled 显式关闭猫猫旅行排程（schedule.travel_enabled=false）。
	TravelDisabled bool
	// ActivityDisabled 显式关闭活跃上报排程（schedule.activity_enabled=false）。
	ActivityDisabled bool
	// KeepaliveDisabled 显式关闭 token 保活排程（schedule.keepalive_enabled=false）。
	KeepaliveDisabled bool
	// BlackcatDisabled 显式关闭夜猫子排程（schedule.blackcat_enabled=false）。
	BlackcatDisabled bool
	// GrowthDisabled 显式关闭成长任务自动排程（schedule.growth_enabled=false）。
	GrowthDisabled bool

	// IncludeDisabledInTasks 让「保号类」四任务（签到 / 活跃上报 / token 保活 / 余额刷新）
	// 对已禁用（disabled）的账号也执行（schedule.include_disabled_in_tasks）。
	// 缺省 false = 保持「禁用的跳过」既有语义；打开后禁用号照常签到保号，但**仍不参与
	// 选号**——pool.pick 侧的 disabled 过滤与本开关无关。
	IncludeDisabledInTasks bool

	// GrowthHook 成长任务队列执行回调（panel.RunGrowthQueueOnce：扫描全部账号
	// 待办并执行，与面板「执行全部待办」按钮同管线）。调度器只管时点不管实现——
	// panel 在 scheduler 之后构造，用 SetGrowthHook 事后挂载；nil 时到点跳过。
	GrowthHook func()
}

// Scheduler 调度器。
type Scheduler struct {
	cfg Config

	// mu/adoptTried 领养当日失败记录：uid → 自然日（CST）。门槛未达的账号当日不再重试，
	// 避免同日多趟对上游重试轰炸；进程重启即清零（无需持久化）。
	mu sync.Mutex
	// taskLocks 每个任务族一把锁（bug P0-3）：同一族不允许两轮并发。
	// runBatch 与面板手动入口共用，key 见 Task* 常量。
	taskLocks  sync.Map // key: string → *sync.Mutex
	adoptTried map[string]string

	// schedMu 保护排程参数（时点/开关）；Reconfigure 可在运行期热改（面板保存配置时调用）。
	// rearmSchedule/rearmBalance 是「排程已变，立即重算」通知：Run 与余额刷新循环各自消费，
	// 分别用独立 channel（同 channel 被两个 select 消费会丢信号）。
	schedMu       sync.Mutex
	rearmSchedule chan struct{}
	rearmBalance  chan struct{}

	// balanceInterval 余额刷新间隔（纳秒，0=暂停）。atomic 读写：执行循环每轮读当前值，
	// SetBalanceInterval 可任意时刻热改（面板保存配置）。
	balanceInterval atomic.Int64
}

// New 构建。
func New(cfg Config) *Scheduler {
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{9, 21}
	}
	if len(cfg.TravelHours) == 0 {
		cfg.TravelHours = []int{9, 21}
	}
	if len(cfg.ActivityHours) == 0 {
		cfg.ActivityHours = []int{10}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{22}
	}
	if len(cfg.BlackcatHours) == 0 {
		cfg.BlackcatHours = []int{23}
	}
	// GrowthHours 也必须给默认值（bug #6）：其余五个槽位都有，唯独它没有 ——
	// 空值时 nextFire(now, nil) 返回零时间，被 `if sl.at.IsZero() { continue }`
	// 跳过，成长任务**永远不会被排到**且没有任何日志。main.go 传了 [1] 所以线上
	// 不显形，但任何直接 scheduler.New 的装配（测试/工具/二次开发）都会踩。
	if len(cfg.GrowthHours) == 0 {
		cfg.GrowthHours = []int{1}
	}
	return &Scheduler{
		cfg:           cfg,
		adoptTried:    make(map[string]string),
		rearmSchedule: make(chan struct{}, 1),
		rearmBalance:  make(chan struct{}, 1),
	}
}

// ExpiringSoonWindow 返回当前快过期路由窗口（读取时与热配置写在 schedMu 下同步）。
func (s *Scheduler) ExpiringSoonWindow() time.Duration {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.cfg.ExpiringSoonWindow
}

// SetExpiringSoonWindow 热更新快过期路由窗口。窗口变化时清空池内旧快照，避免在下一轮
// 余额刷新覆盖前，继续用旧窗口得出的最早到期顺序选号。
func (s *Scheduler) SetExpiringSoonWindow(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.schedMu.Lock()
	changed := s.cfg.ExpiringSoonWindow != d
	s.cfg.ExpiringSoonWindow = d
	poolRef := s.cfg.Pool
	s.schedMu.Unlock()
	if changed && poolRef != nil {
		poolRef.ClearExpiringSnapshots()
	}
}

// Reconfigure 热更新排程参数（面板保存配置后调用）：改时点/开关并通知运行中的循环重算。
// 空 hours 视为「未配置」保留原值（与 config.normalize 的回落语义一致）。
// SetGrowthHook 挂载/替换成长任务队列回调（panel 构造晚于 scheduler，事后接线）。
func (s *Scheduler) SetGrowthHook(fn func()) {
	s.schedMu.Lock()
	s.cfg.GrowthHook = fn
	s.schedMu.Unlock()
}

// includeDisabledInTasks 返回保号类任务是否应覆盖禁用账号（配置快照，供循环开头取一次）。
func (s *Scheduler) includeDisabledInTasks() bool {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.cfg.IncludeDisabledInTasks
}

// SetIncludeDisabledInTasks 热更新「保号类任务是否覆盖禁用账号」
// （schedule.include_disabled_in_tasks）。
// 用独立 setter 而非并入 Reconfigure：后者已有 12 个位置参数，继续追加会让调用点难以
// 校对；本开关语义独立（只影响账号过滤，不影响时点），单独设值更清晰。
func (s *Scheduler) SetIncludeDisabledInTasks(v bool) {
	s.schedMu.Lock()
	s.cfg.IncludeDisabledInTasks = v
	s.schedMu.Unlock()
}

func (s *Scheduler) Reconfigure(checkinHours, travelHours, activityHours, keepaliveHours, blackcatHours, growthHours []int,
	checkinDisabled, travelDisabled, activityDisabled, keepaliveDisabled, blackcatDisabled, growthDisabled bool) {
	s.schedMu.Lock()
	if len(checkinHours) > 0 {
		s.cfg.CheckinHours = checkinHours
	}
	if len(travelHours) > 0 {
		s.cfg.TravelHours = travelHours
	}
	if len(activityHours) > 0 {
		s.cfg.ActivityHours = activityHours
	}
	if len(keepaliveHours) > 0 {
		s.cfg.KeepaliveHours = keepaliveHours
	}
	if len(blackcatHours) > 0 {
		s.cfg.BlackcatHours = blackcatHours
	}
	if len(growthHours) > 0 {
		s.cfg.GrowthHours = growthHours
	}
	s.cfg.CheckinDisabled = checkinDisabled
	s.cfg.TravelDisabled = travelDisabled
	s.cfg.ActivityDisabled = activityDisabled
	s.cfg.KeepaliveDisabled = keepaliveDisabled
	s.cfg.BlackcatDisabled = blackcatDisabled
	s.cfg.GrowthDisabled = growthDisabled
	s.schedMu.Unlock()
	poke(s.rearmSchedule)
	poke(s.rearmBalance)
}

// poke 非阻塞发一次唤醒信号（已有待处理信号则忽略，语义等价）。
func poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// nextFire 返回 now 之后最近的一个整点触发时间；hours 为本地小时（0-23）。
func nextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			// 跨日必须**重建日期**（BUG-04）：Add(24h) 加的是绝对时长，碰上夏令时
			// 切换日会偏一小时（春季次日 09:00 变 10:00、秋季变 08:00），
			// 夜猫子那种窗口任务会提前触发、整轮白跑还计一次失败。
			t = time.Date(now.Year(), now.Month(), now.Day()+1, h, 0, 0, 0, now.Location())
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// taskKind 调度任务类型。
type taskKind int

const (
	taskCheckin taskKind = iota
	taskTravel
	taskActivity
	taskKeepalive
	taskBlackcat
	taskGrowth
)

// 任务族键：面板包看不到未导出的 taskKind，手动入口按字符串共用同一把锁（bug P0-3）。
const (
	TaskCheckin   = "checkin"
	TaskTravel    = "travel"
	TaskActivity  = "activity"
	TaskKeepalive = "keepalive"
	TaskBlackcat  = "blackcat"
	TaskGrowth    = "growth"
	TaskBalance   = "balance"
)

// taskKindKey taskKind → 任务族键。
func taskKindKey(k taskKind) string {
	switch k {
	case taskCheckin:
		return TaskCheckin
	case taskTravel:
		return TaskTravel
	case taskActivity:
		return TaskActivity
	case taskKeepalive:
		return TaskKeepalive
	case taskBlackcat:
		return TaskBlackcat
	case taskGrowth:
		return TaskGrowth
	}
	return ""
}

// TryBeginTask 尝试占用某个任务族；false = 上一轮还在跑。
// 面板手动入口用它决定回 409 还是开跑。
func (s *Scheduler) TryBeginTask(key string) bool {
	if key == "" {
		return true
	}
	m, _ := s.taskLocks.LoadOrStore(key, &sync.Mutex{})
	return m.(*sync.Mutex).TryLock()
}

// TaskRunning 报告某任务族当前是否在跑（**不**占锁）。
//
// 实现是「试着 TryLock 再立刻释放」：拿得到 = 没人在跑；拿不到 = 有人在跑。
// 只用于状态展示（面板「余额刷新中」），不做任何调度决策，所以这点竞态无害。
func (s *Scheduler) TaskRunning(key string) bool {
	if key == "" || s == nil {
		return false
	}
	v, ok := s.taskLocks.Load(key)
	if !ok {
		return false
	}
	mu := v.(*sync.Mutex)
	if mu.TryLock() {
		mu.Unlock()
		return false
	}
	return true
}

// EndTask 释放 TryBeginTask 占用的任务族锁。
func (s *Scheduler) EndTask(key string) {
	if key == "" {
		return
	}
	if m, ok := s.taskLocks.Load(key); ok {
		m.(*sync.Mutex).Unlock()
	}
}

// nextWake 返回 now 之后最近的唤醒时刻，以及该时刻需要执行的全部任务。
// 多类任务若配到同一小时（如签到与旅行都含 9），该时刻多类任务需一并执行。
// 已显式禁用的任务不进候选（nextFire 对其零值返回零时间，nextWake 再跳过零时点）。
// 排程参数在 schedMu 下快照，与 Reconfigure 的并发写隔离。
func (s *Scheduler) nextWake(now time.Time) (time.Time, []taskKind) {
	s.schedMu.Lock()
	checkinHours, keepaliveHours, blackcatHours := s.cfg.CheckinHours, s.cfg.KeepaliveHours, s.cfg.BlackcatHours
	travelHours, activityHours := s.cfg.TravelHours, s.cfg.ActivityHours
	checkinOff, keepaliveOff, blackcatOff := s.cfg.CheckinDisabled, s.cfg.KeepaliveDisabled, s.cfg.BlackcatDisabled
	growthHours, growthOff := s.cfg.GrowthHours, s.cfg.GrowthDisabled
	travelOff, activityOff := s.cfg.TravelDisabled, s.cfg.ActivityDisabled
	s.schedMu.Unlock()

	type slot struct {
		at   time.Time
		kind taskKind
	}
	var slots []slot
	if !checkinOff {
		slots = append(slots, slot{nextFire(now, checkinHours), taskCheckin})
	}
	if !travelOff {
		slots = append(slots, slot{nextFire(now, travelHours), taskTravel})
	}
	if !activityOff {
		slots = append(slots, slot{nextFire(now, activityHours), taskActivity})
	}
	if !keepaliveOff {
		slots = append(slots, slot{nextFire(now, keepaliveHours), taskKeepalive})
	}
	if !blackcatOff {
		slots = append(slots, slot{nextFire(now, blackcatHours), taskBlackcat})
	}
	if !growthOff {
		slots = append(slots, slot{nextFire(now, growthHours), taskGrowth})
	}
	var earliest time.Time
	for _, sl := range slots {
		if sl.at.IsZero() {
			continue
		}
		if earliest.IsZero() || sl.at.Before(earliest) {
			earliest = sl.at
		}
	}
	if earliest.IsZero() {
		return time.Time{}, nil
	}
	var kinds []taskKind
	for _, sl := range slots {
		if !sl.at.IsZero() && sl.at.Equal(earliest) {
			kinds = append(kinds, sl.kind)
		}
	}
	return earliest, kinds
}

// wakeupGraceDelay 迟到唤醒补跑的派发前网络宽限：Windows Modern Standby exit 后
// 网络栈/DNS 1-2s 才恢复（issue #152 实测 dial tcp lookup no such host 与
// Kernel-Power 507 standby exit ≤1s 重合），宽限 5s 覆盖 90%+ 唤醒场景。
// 只对迟到补跑生效（准点触发零延迟），零配置。测试可缩短（与
// travelAccountDelay「测试可置 0」同口径）。
var wakeupGraceDelay = 5 * time.Second

// wakeupLateThreshold 迟到判定阈值：now 晚于槽位计划时刻超过 1s 才算迟到补跑。
// 毫秒级抖动（timer 正常触发的偏移量级）不算，避免准点触发被误宽限。
const wakeupLateThreshold = 1 * time.Second

// awaitWakeupGrace 迟到唤醒补跑派发前的网络宽限：槽位时刻已过点超过阈值
// （机器刚从睡眠唤醒）时先等满 wakeupGraceDelay 让网络栈/DNS 就绪再派发。
// 准点/阈值内抖动零延迟直接放行。ctx 取消立即返回 false（优雅停机不等宽限
// 睡满，本批放弃，下轮 nextWake 照旧从"现在"起算）。返回是否继续派发。
func awaitWakeupGrace(ctx context.Context, planned time.Time) bool {
	if late := time.Since(planned); late <= wakeupLateThreshold {
		return ctx.Err() == nil // 准点触发：零延迟放行
	}
	log.Printf("wakeup grace %s: late catch-up for slot %s", wakeupGraceDelay, planned.Format("15:04"))
	return sleepCtx(ctx, wakeupGraceDelay)
}

// wallclockCheckStep 墙钟校验段长：等待槽位时单次 timer 的最大时长，每段醒来用
// 墙钟重判是否到点。值是「时点精度」与「空闲唤醒频率」的折中——60s 段内时点
// 偏差上限 60s，对签到/保活类任务足够。
const wallclockCheckStep = time.Minute

// slotWake waitSlot 的三态结果。
type slotWake int

const (
	slotFired  slotWake = iota // 墙钟已到达计划时点：补跑本批
	slotRearm                  // 排程已变（Reconfigure）：上层重算下一次唤醒
	slotCancel                 // ctx 取消：上层优雅退出
)

// waitSlot 分段等待到 next 的**墙钟**时刻（next 由 nextFire 用 time.Date 构造、
// 不携带单调读数，time.Until 对它是纯墙钟差）。
//
// 为什么不一把 time.NewTimer(time.Until(next)) 睡到底：timer 的等待基于单调时钟，
// macOS / Windows Modern Standby 睡眠会冻结它——睡眠时长不足整个等待时，fire
// 被顺延「睡眠时长」（墙钟已过点、timer 还要继续等），时点被错过且不会立即补跑；
// 睡眠时长超过整个等待时倒是无害的（唤醒瞬间 timer 到期，awaitWakeupGrace 补跑）。
// 分段睡、每段醒来用墙钟重判，把冻结的影响限制在一段之内：睡眠结束后的第一段
// 末尾必然发现「墙钟已越过时点」并立即补跑，偏差上限 = step + 睡眠落段余量。
//
// ctx 取消 / rearmSchedule（在线改配置重排）在每段的 select 里随时返回，段长
// 不影响两者响应性。返回三态见 slotWake。
func (s *Scheduler) waitSlot(ctx context.Context, next time.Time, step time.Duration) slotWake {
	for {
		wallRemain := time.Until(next)
		if wallRemain <= 0 {
			return slotFired
		}
		d := wallRemain
		if d > step {
			d = step
		}
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return slotCancel
		case <-s.rearmSchedule:
			timer.Stop()
			return slotRearm
		case <-timer.C:
			// 段末回到循环顶用墙钟重判：正常推进时若干段后到点；单调时钟被
			// 睡眠冻结时，墙钟大幅前进，至多一段之后即到点补跑。
		}
	}
}

// Run 主循环，阻塞直到 ctx 取消。
// Reconfigure 触发 rearmSchedule 时提前唤醒重算（新时点/开关立即生效）。
func (s *Scheduler) Run(ctx context.Context) {
	for {
		// 用北京时间算时点（misakano7545 `025cc107`）：上游的「今天」是北京自然日，
		// 而网关可能跑在 UTC 主机上 —— 配 activity_hours=[10] 实际会在 CST 18:00 跑。
		next, kinds := s.nextWake(cst.Now())
		if next.IsZero() {
			// 四类任务全部禁用：不空转，等重排通知（在线改配置重新启用）或退出信号。
			select {
			case <-ctx.Done():
				return
			case <-s.rearmSchedule:
				continue
			}
		}
		switch s.waitSlot(ctx, next, wallclockCheckStep) {
		case slotCancel:
			return
		case slotRearm:
			continue // 排程已变：重算下一次唤醒
		case slotFired:
			// 到点任务在排程时确定（不依赖唤醒时刻的小时数），迟到唤醒也不会漏跑。
			// 迟到唤醒（睡眠跨过槽位时刻，timer 在唤醒瞬间才到期）先等网络宽限：
			// 唤醒瞬间 DNS 未就绪，零宽限派发等于把唯一一次补跑机会打在注定失败
			// 的窗口里（issue #152）；准点触发零延迟不受影响。
			if !awaitWakeupGrace(ctx, next) {
				return // ctx 取消：放弃本批，优雅退出
			}
			// 唤醒时全部并行派发：每类一个 goroutine，慢任务族（如活跃上报
			// 多号 × 间隔 ≈ 数分钟睡眠）不再阻塞同槽其他任务族；返回前等全部
			// 任务收尾（下一轮 nextWake 照旧从"现在"起算，多轮重叠的风险与
			// 串行版相同——nextWake 只挑现在之后的时点）。
			// 异步派发（bug #13）：runBatch 末尾 wg.Wait() 会阻塞主循环，主循环随后用
			// time.Now() 重算下一次唤醒 —— 若这批任务跨过了下一个时点，那次直接被跳过
			// （checkin 末尾串跑 streak 管家 + 800ms/号节流，46 号就是数分钟）。
			// 改为后台跑，主循环立刻回到 waitSlot 继续等下一个时点；
			// 同族任务重入由 runBatch 内部的任务族互斥语义承担（各族自身幂等）。
			go s.runBatch(ctx, kinds)
		}
	}
}

// runBatch 并行派发一批任务（同一唤醒时刻的多类任务），等全部完成返回。
// ctx 取消时由各任务内部的可取消等待快速收尾。
func (s *Scheduler) runBatch(ctx context.Context, kinds []taskKind) {
	var wg sync.WaitGroup
	for _, k := range kinds {
		wg.Add(1)
		go func(k taskKind) {
			defer wg.Done()
			// 任务族互斥（bug P0-3）：此前 runBatch 完全没锁，而签到/活跃一轮在慢上游
			// 下可达几十分钟，跨过下一个时点就两批并发跑同一批账号 —— 重复签到、
			// 重复上报、日志与状态互相覆盖，还更容易触发上游 WAF 频控。
			key := taskKindKey(k)
			if !s.TryBeginTask(key) {
				log.Printf("scheduler: %s 上一轮仍在执行，跳过本轮", key)
				return
			}
			defer s.EndTask(key)
			// panic 兜底：这些 goroutine 不是 net/http 起的，Go 不会替它们 recover，
			// 一次 panic 打死的是整个网关（与上一轮修的任务队列同类问题）。
			defer func() {
				if r := recover(); r != nil {
					log.Printf("ERR: scheduler: %s 执行 panic: %v", key, r)
				}
			}()
			switch k {
			case taskCheckin:
				s.RunCheckinNowCtx(ctx)
			case taskTravel:
				s.RunTravelNowCtx(ctx)
			case taskActivity:
				s.runActivity(ctx)
			case taskKeepalive:
				s.RunKeepaliveNowCtx(ctx)
			case taskBlackcat:
				s.RunBlackcatNowCtx(ctx)
			case taskGrowth:
				// 成长任务队列：回调在 panel 侧异步启动（返回不等执行完），nil 未挂载则跳过。
				s.schedMu.Lock()
				hook := s.cfg.GrowthHook
				s.schedMu.Unlock()
				if hook != nil {
					hook()
				}
			}
		}(k)
	}
	wg.Wait()
}

// sleepCtx 可取消的等待：ctx 取消立即返回 false（优雅停机不必等限速睡醒），
// 等满返回 true。d<=0 立即放行。
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

// RunCheckinNow 立即对所有账号执行签到 + 余额刷新 + 解冻。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用账号默认跳过——
// 若 schedule.include_disabled_in_tasks 打开则一并执行（禁用只关选号，不停保号）。
// 旅行已从签到剥离为独立排程（travel_hours），不再搭签到便车。
// 末尾追加连登管家（streak.go）：可兑换档位自动兑换 + 抽奖次数自动抽完——
// 连登兑换按天数解锁，挂在每日签到后即「到天数那天自动完成兑换→抽奖闭环」。
// checkinAccountDelay 全量签到的账号间节流（issue ISSUE-04）。
// 每个号两次上游调用，46 号部署一次签到就是 92 个请求连发 → 上游频控 → WAF 403
// → 账号软冷却 → 次日再撞，形成自激。与 travel/activity 的 800ms 同口径；
// 测试把它置 0。
var checkinAccountDelay = 800 * time.Millisecond

func (s *Scheduler) RunCheckinNow() {
	s.RunCheckinNowCtx(context.Background())
}

// RunCheckinNowCtx 带 ctx 的全量签到（bug #11）：账号间节流用传入 ctx，
// SIGTERM / 停机时不必把剩余账号的 0.8s 全睡完（此前传 Background，
// sleepCtx 永远返回 true，`if !... { return }` 是死代码）。
func (s *Scheduler) RunCheckinNowCtx(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	expiringSoon := s.ExpiringSoonWindow()
	includeDisabled := s.includeDisabledInTasks()
	first := true
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled && !includeDisabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		// D4 门控：realm=global 账号无签到体系，直接跳过（不发起任何上游调用，避免风控）。
		// 经 auth.Realm() 统一判定：逃生门（global.enabled=false）下 global 账号被降级为 cn、
		// 按 CN 处理——这是逃生门的刻意语义（纯 CN 部署锁死一切 global），与引用处一致。
		if a.IsGlobal() {
			continue
		}
		// 企业版无签到体系：上游对 POST /v2/billing/meter/daily-checkin 直接
		// 400 code 10001「企业账号不支持该操作」。跳过签到，但**不 continue**——
		// 下方余额查询照常跑（企业额度走 get-enterprise-user-usage 口径，且是
		// credit_floor 判定与面板额度展示的唯一数据源）。
		// 账号间节流：第一个号不等，之后每个号之间等一次。
		if !first && checkinAccountDelay > 0 {
			if !sleepCtx(ctx, checkinAccountDelay) {
				return // 停机/取消：不再把剩余账号的节流全睡完
			}
		}
		first = false
		if !a.IsEnterprise() {
			if err := s.cfg.Upstream.DailyCheckinCtx(ctx, a); err != nil {
				// "今天已签到"是幂等成功（上游对重复签到返回 code!=0），不再当失败打 error 行。
				if upstream.IsAlreadyCheckin(err) {
					s.cfg.Pool.NoteCheckinDone(st.UID)
					log.Printf("checkin %s: 今天已签到（幂等）", logfmt.Label(st.UID, st.Nickname))
				} else {
					log.Printf("checkin %s: %v", logfmt.Label(st.UID, st.Nickname), err)
				}
				// 其余业务错误也继续走余额查询
			} else {
				// 首次签到成功此前静默——排查「签到到底跑没跑」时无迹可循（幂等行只在
				// 重复触发时出现），成功也落一行。
				s.cfg.Pool.NoteCheckinDone(st.UID)
				log.Printf("checkin %s: 签到成功", logfmt.Label(st.UID, st.Nickname))
			}
		}
		// 分桶查余额：配置窗口内的积分单独标记，同时记录最早未来到期批次。
		remain, total, expiring, earliestAt, earliestRemaining, err := s.cfg.Upstream.UserResourceDetailedWithExpiryCtx(ctx, a, expiringSoon)
		if err != nil {
			log.Printf("user-resource %s: %v", logfmt.Label(st.UID, st.Nickname), err)
			continue
		}
		s.cfg.Pool.ReenableIfCredits(st.UID, remain, total)
		s.cfg.Pool.SetCreditsDetailed(st.UID, remain, total, expiring, earliestAt, earliestRemaining)
	}
	// 连登管家与主循环共用 ctx：账号间节流被取消（停机/客户端断连）时整轮都不会执行。
	// 此前静默跳过，日志里看不出「连登这轮为什么没跑」（报告 #13）。
	if ctx.Err() != nil {
		log.Printf("scheduler: 签到主循环已取消，连登管家本轮跳过: %v", ctx.Err())
	} else {
		s.RunStreakBonusNowCtx(ctx)
	}
}

// RunActivityNow 立即对池内所有可用账号执行一次对话活跃上报。
// 禁用账号默认跳过（schedule.include_disabled_in_tasks 打开时一并上报）；
// 无 AccessToken 的跳过；账号间限速 activityAccountDelay。
// CN 与 global 账号**都上报**（PR #45 实测国际版 /v2/report 在 workbuddy.ai 上
// code=0 OK，点亮连登）；一条上报同时点亮 growth 连登 + 解锁 first_buddy 任务。
// 上报成功后续跑 streak 自检（checkActivityStreak）：回读连登天数，发现
// 「上报 200 但 streak 没涨」的静默丢弃（只读 oracle，不做重试）。
// RunActivityNow 是无 ctx 的外部入口（面板/测试一次性触发）；排程主循环走
// runActivity（ctx 取消时立即放弃剩余账号，不等限速睡满）。
func (s *Scheduler) RunActivityNow() {
	s.runActivity(context.Background())
}

// runActivity 活跃上报遍历，随 ctx 取消立即退出。
func (s *Scheduler) runActivity(ctx context.Context) {
	first := true
	includeDisabled := s.includeDisabledInTasks()
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled && !includeDisabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessTokenValue() == "" {
			continue
		}
		if s.skipBehaviorTask(a) {
			continue
		}
		// 企业版跳过活跃上报：/v2/report 本身返回 200（不报错），但它唯一的作用是
		// 点亮 growth 连登天数 / 解锁 first_buddy 领养——这两者对企业号都是 403
		// （「growth system is only available for personal users」），即**零收益**。
		// 该类"静默无效"最易被忽略（日志无异常），故显式跳过，不给上游多发一次请求。
		if a.IsEnterprise() {
			continue
		}
		// global 账号同样上报（PR #45 实测国际版 /v2/report 在 workbuddy.ai 上 code=0 OK，
		// 点亮连登）；realmBase 路由/头由 upstream.billingJSON/BillingHeaders 按 realm 切，
		// 无需改动 upstream。此处曾按「D4 门控：global 无活跃体系」跳过 global，实测该
		// 判断不成立——国际版 /v2/report 可用，跳过即国际版账号永远点不亮连登（上游
		// a190252 同口径修复）。checkin/travel 的 global 门控不受影响，仍跳过。
		if !first {
			if !sleepCtx(ctx, s.accountDelay(activityAccountDelay)) {
				return // 优雅停机：不等限速睡满，剩余账号下轮再报
			}
		}
		first = false
		// 拼上账号标识（bug #12）：原来是纯毫秒时间戳，46 个号里同毫秒处理 2 个以上
		// 就拿到**同一个** conversation id —— 上游按 conversation 聚合活跃事件，
		// 重复 id 可能被去重/合并，表现为"报了 N 个号，连登只涨了一部分"。
		cid := fmt.Sprintf("wb2api-%d-%s", time.Now().UnixMilli(), logfmt.UID8(st.UID))
		if err := s.cfg.Upstream.ReportChatActivity(a, cid, ""); err != nil {
			log.Printf("activity %s: %v", logfmt.Label(a.UID, a.NicknameValue()), err)
			continue
		}
		s.checkActivityStreak(a) // 上报成功 → 回读 streak 自检
	}
}

// checkActivityStreak 上报成功后回读连登天数（只读 oracle，发现静默失败）。
// 背景：REPORT-active-map.md §2 实测「上报 200 但静默丢弃」（缺 userId 时 progress 不动），
// 上报 200 ≠ streak 计分——需要回读验证闭环。
// 异常检测口径：days==0 → warn（report OK but streak.days=0 (silent drop?)）；
// GET 失败 → warn 但不影响主流程（上报本身已成功，按天幂等，不做重试）。
// 日志每号一行、一眼可 grep：`activity %s: streak days=%d`（成功也打，方便对账）。
// 返回 true 表示「上报 OK 但 streak 可疑」（days==0 或回读失败），供测试断言。
func (s *Scheduler) checkActivityStreak(a *auth.Auth) bool {
	days, err := s.cfg.Upstream.GrowthStreak(a)
	if err != nil {
		log.Printf("activity %s: streak check failed (report OK): %v", logfmt.Label(a.UID, a.NicknameValue()), err)
		return true
	}
	if days == 0 {
		log.Printf("activity %s: report OK but streak.days=0 (silent drop?)", logfmt.Label(a.UID, a.NicknameValue()))
		return true
	}
	log.Printf("activity %s: streak days=%d", logfmt.Label(a.UID, a.NicknameValue()), days)
	return false
}

// RunKeepaliveNow 立即对所有账号刷新 token；session 死亡的自动禁用。
// 禁用账号默认跳过；若 schedule.include_disabled_in_tasks 打开，禁用号也会续期 token
// （这是轮换用法下把闲置号保持可用的关键），但**不再对已禁用的号重复计数 12153**——
// 它已经是终态，再计一次只会打出一行「— 禁用」的误导日志。
// 12153 禁用走 Pool.NoteSessionDead 的**连续计数**语义：一次刷新失败不再立即杀号，
// 连续 sessionDeadThreshold 次（3 次）才禁用（P0-1：13 个 disabled 号全是历史误判）。
// 刷新成功 → ClearSessionDead 清计数（错误判定的账号有复活路径）。
// keepaliveAccountDelay 账号间节流（变量供测试缩短）。
// 签到/旅行/活跃/夜猫子/连登兑换都有同款延迟，唯独 keepalive 是裸循环 ——
// 46 个号在 22:00 一次性连发 46 个 refresh，正是仓库多处注释列为 WAF 频控
// 触发条件的形态（bug 7）。
var keepaliveAccountDelay = 800 * time.Millisecond

// accountDelay 账号遍历型任务的账号间等待：配置了 schedule.account_spread_seconds
// 就用它，否则回落**该任务自己的**延迟变量（misakano7545 `3dfe46c3` issue #45）。
//
// fallback 必须按任务传入，不能统一成一个常量：测试靠把 activityAccountDelay /
// travelAccountDelay 置 0 来免等待，统一后测试会**真的睡**（整包跑成分钟级）。
func (s *Scheduler) accountDelay(fallback time.Duration) time.Duration {
	if s.cfg.AccountSpreadSeconds > 0 {
		return time.Duration(s.cfg.AccountSpreadSeconds) * time.Second
	}
	return fallback
}

// RunKeepaliveNow 无 ctx 版本（保留给旧调用方）。
func (s *Scheduler) RunKeepaliveNow() { s.RunKeepaliveNowCtx(context.Background()) }

// RunKeepaliveNowCtx 带 ctx：停机时可打断，账号之间做节流。
func (s *Scheduler) RunKeepaliveNowCtx(ctx context.Context) {
	includeDisabled := s.includeDisabledInTasks()
	first := true
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled && !includeDisabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		if !first && keepaliveAccountDelay > 0 {
			if !sleepCtx(ctx, keepaliveAccountDelay) {
				return // 停机/取消：立刻退出
			}
		}
		first = false
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			log.Printf("keepalive %s: %v", logfmt.Label(st.UID, st.Nickname), err)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead && !st.Disabled {
				if s.cfg.Pool.NoteSessionDead(st.UID) {
					log.Printf("keepalive %s: 连续 %d 次 12153 session dead — 禁用", logfmt.Label(st.UID, st.Nickname), pool.SessionDeadThreshold())
				}
			}
			continue
		}
		s.cfg.Pool.ClearSessionDead(st.UID) // 刷新成功清误判计数，失败不该累计
		if err := a.SaveAtomic(); err != nil {
			log.Printf("keepalive %s save: %v", logfmt.Label(st.UID, st.Nickname), err)
		}
	}
}

// RunBalanceRefreshNow 并发查询余额并更新池内 credits。
// 账号范围：默认跳过禁用账号；schedule.include_disabled_in_tasks 打开时一并刷新
// （轮换用法下据此判断下一个该启用谁）。注意 ReenableIfCredits 对 disabled 是
// no-op，所以本开关**不会**导致禁用号被自动解冻——它只让已有账号的积分保持新鲜。
// 解冻语义与签到一致（ReenableIfCredits：余额 > 0 的冷却账号自动解冻），
// 但不做签到、不刷新 token——只让"积分"这个观测量保持新鲜。
// 供两类入口复用：后台周期任务（StartBalanceRefresh）与面板手动全量刷新。
func (s *Scheduler) RunBalanceRefreshNow() { s.RunBalanceRefreshNowCtx(context.Background()) }

// RunBalanceRefreshNowCtx 带 ctx 的余额刷新（bug B14）：sem 抢占与账号间节流都可取消，
// 否则 SIGTERM 时可能卡在 sem 上直到超时。
func (s *Scheduler) RunBalanceRefreshNowCtx(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	var wg sync.WaitGroup
	// 并发闸门（review P1-11）：此前每个账号一个 goroutine，46 个号就是 46 路并发
	// 打上游；余额刷新默认每 5 分钟一轮，多账号部署下会与 chat 请求争同一个出口
	// IP 的 WAF 频控窗口（仓库里多处为 WAF 403 做了专门设计，这里却是裸并发）。
	// 与面板 packages/syncNicknames 的 sem=3 同口径。
	sem := make(chan struct{}, 3)
	expiringSoon := s.ExpiringSoonWindow()
	includeDisabled := s.includeDisabledInTasks()
dispatch:
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled && !includeDisabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		// 取消时不要提前 return（bug #10）：已经起飞的 goroutine 还在跑并占着 sem，
		// 提前返回会让下一轮再建一个新 sem，老一批与新一批并发叠加 = 闸门失效。
		// 这里只跳出派发循环，末尾统一 wg.Wait()。
		select {
		case sem <- struct{}{}: // 限并发：满 3 路就等，不堆 goroutine
		case <-ctx.Done():
			break dispatch
		}
		wg.Add(1)
		go func(a *auth.Auth, uid string) {
			defer wg.Done()
			defer func() { <-sem }()
			// 内层 goroutine 同样要兜底（bug 1）：否则单个号的响应解析异常 = 整进程退出。
			defer func() {
				if r := recover(); r != nil {
					log.Printf("ERR: scheduler: balance uid=%s panic: %v", uid, r)
				}
			}()
			remain, total, expiring, earliestAt, earliestRemaining, err := s.cfg.Upstream.UserResourceDetailedWithExpiryCtx(ctx, a, expiringSoon)
			if err != nil {
				log.Printf("balance %s: %v", logfmt.Label(uid, a.NicknameValue()), err)
				return
			}
			s.cfg.Pool.ReenableIfCredits(uid, remain, total)
			s.cfg.Pool.SetCreditsDetailed(uid, remain, total, expiring, earliestAt, earliestRemaining)
		}(a, st.UID)
	}
	wg.Wait()
}

// StartBalanceRefresh 后台周期性余额刷新（独立 ticker goroutine，ctx 取消即停）。
// interval<=0 = 起步即暂停，但**循环照样创建**（bug P0-2）：此前直接 return，
// 协程根本没建，之后面板把 schedule.balance_refresh_enabled 打开并保存时，
// SetBalanceInterval 只 poke(rearmBalance) —— 那个 channel 没有任何监听者，
// 余额永不后台刷新，只能重启进程。循环体本身已经能处理 cur<=0（等 rearm），
// 所以这里不需要提前返回。
// 独立于 Run 的小时制排程：余额是分钟级观测量，不值得为它扩展 nextFire 的粒度。
// 运行期可用 SetBalanceInterval 热改间隔（下一轮生效）。
func (s *Scheduler) StartBalanceRefresh(ctx context.Context, interval time.Duration) {
	if interval < 0 {
		interval = 0
	}
	s.balanceInterval.Store(int64(interval))
	go func() {
		// 用哨兵初值而不是 0（报告 #12）：启动时就暂停（interval=0）时
		// `cur != logged` 恒假，「暂停中显示 0s」那条日志永远打不出来，
		// 与注释描述相反。用 -1 保证第一次一定打。
		logged := time.Duration(-1)
		for {
			cur := time.Duration(s.balanceInterval.Load())
			if cur != logged {
				log.Printf("scheduler: 余额后台刷新每 %s（暂停中显示 0s）", cur)
				logged = cur
			}
			if cur <= 0 {
				// 被热改暂停：等重排通知（重新启用时唤醒）或退出。
				select {
				case <-ctx.Done():
					return
				case <-s.rearmBalance:
					continue
				}
			}
			timer := time.NewTimer(cur)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-s.rearmBalance:
				timer.Stop() // 间隔已变：立刻按新值重算
			case <-timer.C:
				// 与面板「刷新余额」共用任务族锁（bug P0-3）：两路同时扇出会双倍打上游。
				if s.TryBeginTask(TaskBalance) {
					// EndTask 必须 defer + 整条链要 recover（bug 1）：`RunBalanceRefreshNowCtx`
					// 里只要 panic 一次（上游响应形状变化、索引越界），任务族锁就永久不释放
					// —— 面板「刷新余额」此后恒 409、后台周期刷新彻底停摆，而且没有任何日志。
					// 同理：这个 goroutine 不是 net/http 起的，panic 无人 recover 就是整个
					// 进程退出（同仓 panel 的各条后台路径都加了兜底，唯独这里漏了）。
					func() {
						defer s.EndTask(TaskBalance)
						defer func() {
							if r := recover(); r != nil {
								log.Printf("ERR: scheduler: balance refresh panic: %v", r)
							}
						}()
						// 必须用 Ctx 版本（bug-05）：无 ctx 版固定 Background，
						// SIGTERM 后 sem 抢占与账号间请求都不会被打断，最多拖 46×30s，
						// 把最后一次 Flush 挤进强杀窗口（正是 run.sh 8s 预算要保护的）。
						s.RunBalanceRefreshNowCtx(ctx)
					}()
				}
			}
		}
	}()
}

// SetBalanceInterval 热改余额刷新间隔；<=0 表示暂停循环（面板关闭该开关时）。
func (s *Scheduler) SetBalanceInterval(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.balanceInterval.Store(int64(d))
	poke(s.rearmBalance)
}

// skipBehaviorTask 企业上下文（企业版 / 企业切换会话）不做**行为类**任务
// （上游 PR #162）：上游 /v2/report 对这类会话返回 200 却**静默丢弃**，
// 任务进度永不跳动、连登/签到无效，跑了纯粹白烧配额且没有任何报错。
// 额度查询、保活、签到照常（它们有显式错误或读端点可用）。
func (s *Scheduler) skipBehaviorTask(a *auth.Auth) bool {
	if a == nil || !a.IsEnterpriseSession() {
		return false
	}
	log.Printf("scheduler: %s 处于企业上下文（token_source=enterprise_switch），跳过行为类任务",
		logfmt.Label(a.UID, a.NicknameValue()))
	return true
}
