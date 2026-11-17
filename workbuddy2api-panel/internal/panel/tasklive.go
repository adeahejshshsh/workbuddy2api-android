// tasklive.go 面板「实时任务」：一次性读回所有账号当前的成长任务进度。
//
// 为什么要它：本仓 v1.23.0 下线了任务中心的「扫描待办 / 执行队列」界面，账号任务
// 弹窗又只在打开时拉一次 —— 面板上没有任何「全局任务进度」的常驻视图，看板永远滞后。
// 本文件提供**只读的实时视图**：直接向每个账号查一次任务表，按 TTL 缓存后交给前端
// 定时轮询，不需要任何手动动作。
//
// 与既有两种数据源的区别（接口仍在，只是界面已下线）：
//   - tasks/jobs：后台任务**执行结果**（要有作业跑过才有内容）；
//   - tasks/scan_all + tasks/queue：手动触发的一次性扫描，跑完才有列表。
//
// 口径：每个账号每轮只请求一次，失败不重试、不换口径。
//   - 国内版调小程序口径（`ListTasksMP`）。上游 38dca39 的注释说它是默认口径的超集，
//     但本仓 0a2a55f 实测任务中心改版后它成了**子集**（默认列表独有的任务如
//     Library_read 不在其中）——所以单号任务弹窗（tasks.go 的 accountTasks）一直是
//     默认 + mp 两份合并。实时卡这里刻意**不**合并：一轮就是「启用账号数」次上游读，
//     两份都调会翻倍；要看全量任务走单号弹窗，看板只负责「一眼看到谁还有事可做」。
//   - 国际版只调默认口径（`ListTasks`）。小程序头对国际账号没有验证过的契约，单号
//     任务弹窗同样明确不对国际账号探 mp。
//
// 缓存的意义：浏览器每轮轮询都打一次上游，多开页面/多人看会成倍放大。TTL 内直接
// 复用；`fresh=1`（手动刷新按钮用）也有下限，上一轮结束不足 taskLiveFreshFloor 时
// 仍回缓存，过了下限才穿透。
package panel

import (
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// taskLiveTTL 实时任务缓存有效期。取值考虑两点：上游任务进度是分钟级聚合的，
// 再密也不会更新；而一轮是「启用账号数」次上游 GET，一个打开的面板每秒的请求量 =
// 账号数 / 缓存时长 —— 28 个号、45 秒一轮就是 0.6 次/秒的持续请求，压到 60 秒
// 才有意义（上游 #259 的量化）。
const taskLiveTTL = 60 * time.Second

// taskLiveConcurrency 一轮实时查询的账号并发（与券码/活动页同档，避免打爆上游）。
const taskLiveConcurrency = 4

// taskLiveFreshFloor fresh=1 的下限。变量只为让测试能压缩时间，生产不改。
var taskLiveFreshFloor = 15 * time.Second

// taskLiveWait 单个请求最多等一轮多久，比常见反代 60s 读超时短。变量只为让测试能
// 压缩时间，生产不改。
var taskLiveWait = 25 * time.Second

// liveTaskAccount 单账号的实时任务行。
type liveTaskAccount struct {
	UID      string          `json:"uid"`
	Nickname string          `json:"nickname"`
	Realm    string          `json:"realm"`
	Tasks    []upstream.Task `json:"tasks,omitempty"`
	// Pending  未完成的任务数（前端排序用）
	Pending int `json:"pending"`
	// Claimable 可领取的任务数
	Claimable int `json:"claimable"`
	// Claimed 已领取的任务数
	Claimed int    `json:"claimed"`
	Err     string `json:"error,omitempty"`
}

// liveCache 实时任务快照（挂在 Panel 上，随面板实例走）。零值可用。
type liveCache struct {
	mu   sync.Mutex
	at   time.Time // 最近一轮完成的时刻；零值 = 还没跑完过
	rows []liveTaskAccount
	// running 非 nil 表示有一轮在后台跑，轮次结束时关闭。同一时刻只跑一轮：多标签页
	// 同时轮询时共用它，而不是各自再打一遍所有账号。
	running chan struct{}
}

// taskLiveSnapshot 取实时任务快照，必要时在后台起一轮。
//
// 一轮在后台 goroutine 里跑、不绑请求：账号多、个别上游挂住（单次最长 120s）时，一轮
// 可以比反代超时和 30s 更久。请求最多等 taskLiveWait，之后带着上一份快照返回
// （refreshing=true），不会被反代截断，也不会让后来的请求排队等到 503。
//
// 返回 waited=true 表示本次请求等过一轮；refreshing=true 表示返回时仍有一轮在跑
// （返回的是上一份快照，从没跑完过则为空）。
func (p *Panel) taskLiveSnapshot(fresh bool) (rows []liveTaskAccount, at time.Time, waited, refreshing bool) {
	c := &p.taskLive
	c.mu.Lock()
	age := time.Since(c.at)
	stale := c.at.IsZero() || age >= taskLiveTTL || (fresh && age >= taskLiveFreshFloor)
	if stale && c.running == nil {
		done := make(chan struct{})
		c.running = done
		go p.runTaskLiveRound(c, done)
	}
	// 没有快照可给，或用户明确要新的：等这一轮（有上限）。其余情况直接回现有快照，
	// 过期的由后台那一轮顶上，下一次读取就是新的。
	done := c.running
	wait := done != nil && (c.at.IsZero() || fresh)
	c.mu.Unlock()

	if wait {
		waited = true
		select {
		case <-done:
		case <-time.After(taskLiveWait):
		}
	}

	c.mu.Lock()
	rows, at, refreshing = c.rows, c.at, c.running != nil
	c.mu.Unlock()
	return rows, at, waited, refreshing
}

// runTaskLiveRound 跑一轮并落快照。panic 兜底：这个 goroutine 不是 net/http 起的，
// Go 不会替它 recover —— 一次 panic 会留下一个永不关闭的 running，之后每个请求都
// 白等 taskLiveWait。宁可落一份空快照（TTL 后自然重试）。
func (p *Panel) runTaskLiveRound(c *liveCache, done chan struct{}) {
	var rows []liveTaskAccount
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panel: 实时任务查询 panic: %v", rec)
			}
		}()
		rows = p.collectLiveTasks()
	}()
	c.mu.Lock()
	c.rows, c.at, c.running = rows, time.Now(), nil
	c.mu.Unlock()
	close(done)
}

// tasksLive GET /panel/api/tasks/live?fresh=1
//
// 返回所有启用账号的当前任务表。账号级失败只标在对应行（error 字段），
// 不影响其它账号——一个号的 token 过期不该让整个看板空掉。
func (p *Panel) tasksLive(w http.ResponseWriter, r *http.Request) {
	rows, at, waited, refreshing := p.taskLiveSnapshot(r.URL.Query().Get("fresh") == "1")
	age := 0
	if !at.IsZero() {
		age = int(time.Since(at).Seconds())
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "cached": !waited || refreshing, "age_sec": age, "refreshing": refreshing,
		"ttl_sec": int(taskLiveTTL.Seconds()), "accounts": rows,
	})
}

// collectLiveTasks 并发查所有启用账号的任务表。
func (p *Panel) collectLiveTasks() []liveTaskAccount {
	stats := p.cfg.Pool.List()
	out := make([]liveTaskAccount, len(stats))
	sem := make(chan struct{}, taskLiveConcurrency)
	var wg sync.WaitGroup

	for i, st := range stats {
		if st.Disabled {
			continue
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		wg.Add(1)
		go func(i int, a *auth.Auth) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = liveTasksOf(p.cfg.Upstream, a)
		}(i, a)
	}
	wg.Wait()

	res := make([]liveTaskAccount, 0, len(out))
	for _, it := range out {
		if it.UID != "" {
			res = append(res, it)
		}
	}
	return res
}

// liveTasksOf 读一个账号的任务表并统计三类计数。
//
// 国内号只发小程序口径一次；国际号只发默认口径。失败不重试、不换口径：重试会在
// 上游抖动或一批号失效（封号潮）时把一轮的请求数翻倍，而换口径不会让 401 变成功。
func liveTasksOf(c *upstream.Client, a *auth.Auth) liveTaskAccount {
	it := liveTaskAccount{UID: a.UID, Nickname: a.Nickname, Realm: a.Realm()}
	if !a.SupportsPersonalEndpoints() {
		it.Err = growthNotApplicableMsg
		return it
	}
	var tasks []upstream.Task
	var err error
	if a.IsGlobal() {
		tasks, err = c.ListTasks(a)
	} else {
		tasks, err = c.ListTasksMP(a)
	}
	if err != nil {
		it.Err = err.Error()
		return it
	}
	for _, t := range tasks {
		switch {
		case t.Claimed:
			it.Claimed++
		case t.Claimable:
			it.Claimable++
		default:
			it.Pending++
		}
	}
	// 只给还有事可做的账号带任务表：全部领完的号占了绝大多数，把它们的任务表每轮都
	// 序列化、前端每轮重画几千行是纯开销；计数仍在，需要细看走单号任务弹窗。
	if it.Pending+it.Claimable > 0 {
		it.Tasks = tasks
	}
	return it
}
