// taskcenter.go 面板「任务中心」：全账号任务扫描 + 执行队列（可配并发）+
// 成长任务队列。
//
// 语义：
//   - 扫描（scan_all）：并发拉取每账号的成长任务列表（默认+小程序口径），
//     汇总出"未完成且可自动化"的待办清单（只读，不执行）。
//   - 执行队列（run_queue + queue）：把待办项按账号分组排队执行——账号内
//     串行（复用 per-account 锁，与单任务/一键完成互斥），账号间并发
//     （concurrency 信号量限制，默认 1）。队列状态可轮询。
package panel

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// ---------------------------------------------------------------------------
// 扫描（只读）
// ---------------------------------------------------------------------------

// scanAccountItem 单账号扫描结果。
type scanAccountItem struct {
	UID       string          `json:"uid"`
	Nickname  string          `json:"nickname"`
	Growth    []upstream.Task `json:"growth,omitempty"`
	GrowthErr string          `json:"growth_error,omitempty"`
	// Skipped 本版判定"做不了、已跳过"的任务（无自动化动作 / 尝试型 / 反复无效）。
	// 为什么列出来而不是过滤掉：静默过滤会让用户以为"按钮没生效"（v1.20.0 的教训）。
	Skipped []skipRow `json:"skipped,omitempty"`
}

// skipRow 扫描页里"做不了、已跳过"的一行。
type skipRow struct {
	Code   string `json:"code"`
	Title  string `json:"title,omitempty"`
	Reason string `json:"reason"`
}

// growthPending 任务是否"未完成 + 可自动化 + 没被判做不了"。
//
// v1.22.0 起是**方法**：判定要带账号 —— "反复无效"是按 (账号,任务码) 记的，
// 同一个任务码在 A 账号能做、在 B 账号上游不计数。
func (p *Panel) growthPending(a *auth.Auth, t upstream.Task) bool {
	if t.Claimed {
		return false
	}
	// 上游锁定的任务不出待办：Sequential 族每日零点解锁一环，刚做完上一环时
	// 下一环以下发但 locked 形态出现在列表里——扫进队列只会 accept 不落账报
	// 失败（每日锁定窗口），零点解锁后自然回到待办。其余 locked（上游未开放）
	// 同语义：不该被自动化尝试。
	if t.Locked {
		return false
	}
	act := autoActionFor(t.TaskCode)
	if act == nil {
		// 达标未领也入队（队列执行后会自动领），但必须真有动作——否则队列执行
		// 时会因 autoActionFor 为 nil 直接报错。
		return false
	}
	if act.Attempt && !p.attemptAllowed() {
		// 尝试型（上游未证实可点亮）默认不排：它们正是"执行了也没效果"的来源。
		return false
	}
	if a != nil {
		if blocked, _ := p.skipBlocked(a.UID, t.TaskCode); blocked {
			return false
		}
	}
	return true
}

// skipReason 任务进不了待办、且**该让用户知道**的原因（空 = 不必提，如已领取/上游锁定）。
func (p *Panel) skipReason(a *auth.Auth, t upstream.Task) string {
	if t.Claimed || t.Locked {
		return ""
	}
	act := autoActionFor(t.TaskCode)
	switch {
	case act == nil:
		return "本移植版没有该任务的自动化动作，需在官方客户端完成"
	case act.Attempt && !p.attemptAllowed():
		return "尝试型任务（上游未证实可点亮），默认不执行；需要请 POST /panel/api/tasks/skip {\"allow_attempt\":true}"
	}
	if a != nil {
		if blocked, why := p.skipBlocked(a.UID, t.TaskCode); blocked {
			return "反复执行仍无进展，已跳过：" + why +
				"（7 天后自动重试，或点「清空跳过名单」立即重试）"
		}
	}
	return ""
}

// queueDiag 「执行全部待办」扫描阶段的诊断账：为什么某项没进待办。
//
// 为什么要有它：这些原因以前全被吞掉，界面上只剩一句「全部账号没有待办任务」——
// 用户看到的就是"点了没效果"，既不知道是**拉取失败**、还是被**每日锁**、
// 还是这个任务码本移植版**没有对应的自动化动作**（后两种都不该被当成故障）。
type queueDiag struct {
	errs    []string // 拉取任务列表失败（账号名 + 错误摘要）
	locked  int      // 上游今日锁定（Sequential 族零点解锁一环）
	claimed int      // 已领取
	noauto  int      // 本移植版暂无该任务码的自动化动作
	gated   int      // 不参与：已停用 / global / 企业版账号
	skipped int      // 本版判定做不了而跳过：尝试型（默认不排）+ 反复无效自动跳过
}

// explain 出人话原因 + 给前端定弹窗颜色（"err" = 确实出问题了，"ok" = 单纯没事可做）。
func (d queueDiag) explain() (string, string) {
	level := "ok"
	var parts []string
	if n := len(d.errs); n > 0 {
		level = "err"
		if n <= 3 {
			parts = append(parts, fmt.Sprintf("%d 个账号拉取任务失败：%s", n, strings.Join(d.errs, "；")))
		} else {
			parts = append(parts, fmt.Sprintf("%d 个账号拉取任务失败：%s 等", n, strings.Join(d.errs[:3], "；")))
		}
	}
	if d.locked > 0 {
		parts = append(parts, fmt.Sprintf("%d 项处于每日锁定（Sequential 族零点解锁，建议 0 点后再点）", d.locked))
	}
	if d.claimed > 0 {
		parts = append(parts, fmt.Sprintf("%d 项已领取", d.claimed))
	}
	if d.noauto > 0 {
		parts = append(parts, fmt.Sprintf("%d 项本移植版暂无自动化动作（可在账号页手动处理）", d.noauto))
	}
	if d.gated > 0 {
		parts = append(parts, fmt.Sprintf("%d 个账号不参与（停用 / global / 企业版）", d.gated))
	}
	if d.skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d 项已跳过（尝试型 / 反复执行仍无进展）—— 只做有把握的，"+
			"要重试请点任务中心「清空跳过名单」", d.skipped))
	}
	if len(parts) == 0 {
		return "全部账号没有待办任务", level
	}
	return "没有可执行待办 —— " + strings.Join(parts, "；"), level
}

// errBrief 错误摘要：压平空白（上游报错常带 HTML/JSON，换行会把提示撑成多行）
// 并截断到 140 字符。
func errBrief(err error) string {
	if err == nil {
		return ""
	}
	s := strings.Join(strings.Fields(err.Error()), " ")
	if len(s) > 140 {
		s = s[:140] + "…"
	}
	return s
}

// tasksScanAll 扫描全部账号：成长任务（未完成+可自动化，含 mp 口径合并）。
// 只读操作，并发拉取（账号数个位数）。
func (p *Panel) tasksScanAll(w http.ResponseWriter, r *http.Request) {
	states := p.cfg.Pool.List()
	items := make([]scanAccountItem, len(states))
	var wg sync.WaitGroup
	// 并发闸门：账号多时「每账号一个 goroutine」等于同时向上游打几十上百个请求
	// （上游按账号维度限流，打了也只是排队挨 429）。4 路足够，且尊重请求取消。
	sem := make(chan struct{}, 4)
	ctx := r.Context()
	for i, st := range states {
		if st.Disabled {
			continue
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done(): // 客户端走了：不再起新任务（continue 结束本轮 for）
			continue
		}
		wg.Add(1)
		go func(i int, uid string) {
			defer wg.Done()
			defer func() { <-sem }()
			a := p.cfg.Pool.AuthByUID(uid)
			if a == nil {
				return
			}
			it := &items[i]
			it.UID, it.Nickname = uid, a.Nickname
			// D4 门控：global 账号无 CN 成长任务体系，不发起任何上游调用。
			if a.IsGlobal() {
				return
			}
			// 企业版门控：同上——企业版无个人成长体系（GET /v2/activity/growth/tasks
			// 上游 403「growth system is only available for personal users」）。
			if a.IsEnterprise() {
				return
			}
			if tasks, err := p.cfg.Upstream.ListTasks(a); err != nil {
				it.GrowthErr = err.Error()
			} else {
				for _, t := range tasks {
					if p.growthPending(a, t) {
						it.Growth = append(it.Growth, t)
						continue
					}
					// 做不了的（无动作 / 尝试型 / 反复无效）不静默消失：列出来并写明原因
					if r := p.skipReason(a, t); r != "" {
						it.Skipped = append(it.Skipped, skipRow{Code: t.TaskCode, Title: t.Title, Reason: r})
					}
				}
			}
			// 小程序口径任务（school_season 校园日 / Sequential_Tasks_1 小程序首对话）
			// 仅在 mp 头列表下发，与默认口径不重叠——合并进待办列表；mp 列表失败
			// 静默（无 mp 任务的部署/活动结束时零影响）。
			if mpTasks, err := p.cfg.Upstream.ListTasksMP(a); err == nil {
				seen := map[string]bool{}
				for _, t := range it.Growth {
					seen[t.TaskCode] = true
				}
				for _, t := range mpTasks {
					if seen[t.TaskCode] {
						continue
					}
					seen[t.TaskCode] = true
					if p.growthPending(a, t) {
						it.Growth = append(it.Growth, t)
						continue
					}
					if r := p.skipReason(a, t); r != "" {
						it.Skipped = append(it.Skipped, skipRow{Code: t.TaskCode, Title: t.Title, Reason: r})
					}
				}
			}
		}(i, st.UID)
	}
	wg.Wait()
	pending := 0
	for _, it := range items {
		pending += len(it.Growth)
	}
	log.Printf("panel: 队列扫描完成：全部账号待办 %d 项", pending)
	// 过滤空行（bug #2）：items 是按 len(states) 预分配的，被禁用账号与 ctx 取消的
	// 分支用 continue 跳过，留下 {"uid":"","nickname":""} 的零值条目 —— 前端按数组
	// 渲染就是一行空账号。schoolVouchers 早就做了同样的过滤，这里漏了。
	res := make([]scanAccountItem, 0, len(items))
	for _, it := range items {
		if it.UID != "" {
			res = append(res, it)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": res, "pending_count": pending})
}

// ---------------------------------------------------------------------------
// 执行队列
// ---------------------------------------------------------------------------

// queueItem 队列执行单元。
type queueItem struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Kind     string `json:"kind"` // growth
	Code     string `json:"code"`
	Status   string `json:"status"` // pending | running | done | skipped | error
	Message  string `json:"message,omitempty"`
}

// queueState 队列运行状态。Seq 每次启动 +1——前端只渲染"自己启动的那一轮"，
// 执行结束后的残留 items 不会覆盖后续的扫描结果视图。
type queueState struct {
	mu        sync.Mutex
	running   bool
	startedAt time.Time
	items     []queueItem
	conc      int
	seq       int
}

// Panel 队列字段在 Panel 结构体上（panel.go）由 initQueue 惰性初始化；
// 这里集中访问器，避免改动 New 构造链。
func (p *Panel) queue() *queueState {
	p.queueOnce.Do(func() { p.q = &queueState{} })
	return p.q
}

// tasksRunQueue 启动执行队列：{concurrency:1-4, growth:bool, school:bool}。
// 先做一次扫描，把全部待办项排队（growth 按账号内 autoActions 顺序执行，
// school 逐账号跑闭环），账号内串行、账号间受并发信号量约束。
func (p *Panel) tasksRunQueue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Concurrency int  `json:"concurrency"`
		Growth      bool `json:"growth"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if !body.Growth {
		body.Growth = true
	}
	if body.Concurrency < 1 {
		body.Concurrency = 1
	}
	if body.Concurrency > 4 {
		body.Concurrency = 4
	}
	started, total, seq, msg, lvl := p.startGrowthQueue(body.Concurrency, body.Growth)
	switch {
	case seq == -1:
		writeErr(w, http.StatusConflict, msg)
	case !started:
		// level 让前端把"真出错"和"单纯没事可做"弹成两种颜色（以前全是绿色 ok，
		// 拉取失败也显示成"没有待办任务" —— 那才是"点了没效果"的观感来源）。
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": false, "message": msg, "level": lvl})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true, "total": total, "seq": seq})
	}
}

// startGrowthQueue 扫描全部账号待办并启动队列（HTTP「执行全部待办」与调度器
// growth 时点共用核心）。返回 (started, total, seq, msg, level)：seq==-1 表示队列
// 已在执行（冲突）；started=false 时 msg 为**讲清楚为什么**的说明（level 供前端
// 决定弹窗颜色：err=确实出问题了，ok=单纯没事可做）。并发夹取
// [1,4]；growth 开关同 HTTP 入参语义。
func (p *Panel) startGrowthQueue(concurrency int, growth bool) (started bool, total int, seq int, msg string, level string) {
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > 4 {
		concurrency = 4
	}
	q := p.queue()
	q.mu.Lock()
	if q.running {
		q.mu.Unlock()
		return false, 0, -1, "队列正在执行中（可在任务中心查看进度）", "ok"
	}
	// 先占位：扫描（数秒级网络耗时）期间若并发再次触发，直接命中上面的 running
	// 判拒，避免两个 goroutine 同时启动互相覆盖 q.items/q.seq。无待办时回滚。
	q.running = true
	q.startedAt = time.Now()
	q.mu.Unlock()

	// 扫描待办（复用扫描逻辑的拉取部分）。
	states := p.cfg.Pool.List()

	var accts []queueAccount
	var wg sync.WaitGroup
	var mu sync.Mutex
	var dg queueDiag
	for _, st := range states {
		if st.Disabled {
			mu.Lock()
			dg.gated++
			mu.Unlock()
			continue
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			mu.Lock()
			dg.gated++
			mu.Unlock()
			continue
		}
		wg.Add(1)
		go func(a *auth.Auth) {
			defer wg.Done()
			one := queueAccount{a: a}
			// D4 门控：global 账号无 CN 成长任务体系，不发起任何上游调用。
			if a.IsGlobal() {
				mu.Lock()
				dg.gated++
				mu.Unlock()
				return
			}
			// 企业版门控：同上（企业版无个人成长体系，任务端点上游一律 403）。
			if a.IsEnterprise() {
				mu.Lock()
				dg.gated++
				mu.Unlock()
				return
			}
			if growth {
				tasks, err := p.cfg.Upstream.ListTasks(a)
				if err != nil {
					// 关键：以前这里静默吞掉 → 界面只会说"没有待办任务"，
					// 看着就像按钮没生效。现在记进诊断账，会原样报给用户。
					mu.Lock()
					dg.errs = append(dg.errs, a.Nickname+"："+errBrief(err))
					mu.Unlock()
				} else {
					for _, t := range tasks {
						if p.growthPending(a, t) {
							one.grow = append(one.grow, t)
							continue
						}
						mu.Lock()
						switch {
						case t.Claimed:
							dg.claimed++
						case t.Locked:
							dg.locked++
						case autoActionFor(t.TaskCode) == nil:
							dg.noauto++
						case p.skipReason(a, t) != "":
							dg.skipped++ // 尝试型 / 反复无效：本版判定做不了
						}
						mu.Unlock()
					}
					// 合并小程序口径待办（与 tasksScanAll 同口径：mp 列表是默认口径
					// 超集，按 code 去重；失败静默）。此前此处漏合并——扫描显示
					// mp 待办而队列报"无可执行待办"。
					if mpTasks, mpErr := p.cfg.Upstream.ListTasksMP(a); mpErr == nil {
						seen := map[string]bool{}
						for _, t := range one.grow {
							seen[t.TaskCode] = true
						}
						for _, t := range mpTasks {
							if seen[t.TaskCode] {
								continue
							}
							seen[t.TaskCode] = true
							if p.growthPending(a, t) {
								one.grow = append(one.grow, t)
								continue
							}
							if p.skipReason(a, t) != "" {
								mu.Lock()
								dg.skipped++
								mu.Unlock()
							}
						}
					}
					sort.Slice(one.grow, func(i, j int) bool { // 按 autoActions 顺序（依赖前置）
						return autoActionIndex(one.grow[i].TaskCode) < autoActionIndex(one.grow[j].TaskCode)
					})
				}
			}
			if len(one.grow) > 0 {
				mu.Lock()
				accts = append(accts, one)
				mu.Unlock()
			}
		}(a)
	}
	wg.Wait()

	// 组装队列（账号分组，保持顺序）。
	var items []queueItem
	for _, one := range accts {
		for _, t := range one.grow {
			items = append(items, queueItem{UID: one.a.UID, Nickname: one.a.Nickname, Kind: "growth", Code: t.TaskCode, Status: "pending"})
		}
	}
	if len(items) == 0 {
		msg, level = dg.explain()
		log.Printf("panel: 队列启动：无可执行待办（%s）", msg)
		q.mu.Lock()
		q.running = false
		q.startedAt = time.Time{}
		q.mu.Unlock()
		return false, 0, 0, msg, level
	}

	q.mu.Lock()
	q.items = items
	q.conc = concurrency
	q.seq++
	seq = q.seq
	q.mu.Unlock()

	go p.runQueueItems(accts, items, concurrency)
	log.Printf("panel: 队列启动：%d 项（并发 %d，成长 %v）", len(items), concurrency, growth)
	return true, len(items), seq, "", "ok"
}

// RunGrowthQueueOnce 调度器 growth 时点回调（sch.SetGrowthHook 挂载）：与
// 「执行全部待办」按钮完全同管线（成长，串行并发 1）。Sequential 族
// 每日零点解锁一环，此前只能手动扫描推进；此回调让链条每天自动走一环。
// 异步执行（startGrowthQueue 启动 goroutine 即返），已在跑/无待办安全跳过。
func (p *Panel) RunGrowthQueueOnce() {
	started, total, _, _, _ := p.startGrowthQueue(1, true)
	if started {
		log.Printf("panel: 定时成长任务队列已启动（%d 项）", total)
	}
}

// runQueueItems 队列执行主体：按账号分组，账号内串行（per-account 锁），
// 账号间并发（信号量）。每项结果写回队列状态。
func (p *Panel) runQueueItems(accts []queueAccount, items []queueItem, concurrency int) {
	q := p.queue()
	defer func() {
		q.mu.Lock()
		q.running = false
		q.mu.Unlock()
		log.Printf("panel: 队列执行结束（共 %d 项）", len(items))
	}()

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, one := range accts {
		wg.Add(1)
		go func(one queueAccount) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// per-account 互斥：与单任务/一键完成共用一把锁。
			if !p.tryLockAccount(one.a.UID) {
				p.queueSet(q, one.a.UID, func(it *queueItem) {
					it.Status, it.Message = "skipped", "该账号有其它任务动作在执行，跳过"
				})
				return
			}
			defer p.unlockAccount(one.a.UID)
			// 前置：批量接受尚未接受的任务。上游对 not_accepted 的任务不计数——
			// 面板「一键完成」一直有这步，队列路径此前漏了（表现为上报 200 但进度
			// 一直 not_accepted、无法领奖）。失败不阻塞（行为事件才是进度判据）。
			if accepted := p.acceptPendingTasks(one.a); accepted > 0 {
				if !sleepAB(one.a.UID, reportGap) {
					return // 断连：本账号不再继续
				}
			}
			for i := range q.items {
				uid, kind, code := q.snapshotAt(i)
				if uid != one.a.UID {
					continue
				}
				p.queueMarkAt(i, "running", "")
				var msg string
				var progressed bool
				var err error
				switch kind {
				case "growth":
					msg, progressed, err = p.runGrowthQueued(one.a, code)
				}
				if err != nil {
					p.queueMarkAt(i, "error", err.Error())
				} else {
					p.queueMarkAt(i, "done", msg)
				}
				// 执行了但没进展 → 记一笔；连续 skipFailLimit 次就把该 (账号,任务码)
				// 砍出待办，别再天天"做了也没用"（7 天后自动放回，或一键清空）。
				if err != nil || !progressed {
					why := "进度未变化（上游未计入这次行为）"
					if err != nil {
						why = errBrief(err)
					}
					added, n := p.skipNote(one.a.UID, code, why)
					note := fmt.Sprintf("（连续无效 %d/%d）", n, skipFailLimit)
					if added {
						note = fmt.Sprintf("（连续 %d 次无效 → 已跳过：不再出现在待办里，7 天后自动重试，"+
							"或点「清空跳过名单」立即重试）", n)
					}
					if err != nil {
						p.queueMarkAt(i, "error", err.Error()+note)
					} else {
						p.queueMarkAt(i, "done", msg+note)
					}
					log.Printf("panel: 任务无效 uid=%s code=%s %s（%s）", one.a.UID, code, note, why)
				} else {
					p.skipReset(one.a.UID, code) // 有效 → 清掉历史失败计数
				}
				if !sleepAB(one.a.UID, reportGap) {
					return
				}
			}
		}(one)
	}
	wg.Wait()
}

// queueAccount 队列执行的账号单元（runQueueItems 参数）。
type queueAccount struct {
	a    *auth.Auth
	grow []upstream.Task
}

// snapshotAt 锁内读条目三元组（避免锁外持有指针）。
func (q *queueState) snapshotAt(i int) (uid, kind, code string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.items[i].UID, q.items[i].Kind, q.items[i].Code
}

// queueMarkAt 按索引更新队列条目状态（条目数组固定不再增删）。
func (p *Panel) queueMarkAt(i int, status, msg string) {
	q := p.queue()
	q.mu.Lock()
	q.items[i].Status, q.items[i].Message = status, msg
	q.mu.Unlock()
}

// queueSet 按 uid 批量改状态。
func (p *Panel) queueSet(q *queueState, uid string, fn func(*queueItem)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.items {
		if q.items[i].UID == uid {
			fn(&q.items[i])
		}
	}
}

// acceptPendingTasks 批量接受该账号未接受的任务，返回接受的个数（失败返回 0 不阻塞）。
func (p *Panel) acceptPendingTasks(a *auth.Auth) int {
	tasks, err := p.cfg.Upstream.ListTasks(a)
	if err != nil {
		return 0
	}
	var codes []string
	for _, t := range tasks {
		if !t.Claimed && !t.Locked && t.AcceptStatus != "accepted" && t.AcceptStatus != "completed" {
			codes = append(codes, t.TaskCode)
		}
	}
	if len(codes) == 0 {
		return 0
	}
	if err := p.cfg.Upstream.AcceptTasks(a, codes); err != nil {
		log.Printf("panel: 队列 accept uid=%s: %v（不阻塞）", a.UID, err)
		return 0
	}
	log.Printf("panel: 队列 accept uid=%s: 已接受 %d 个任务", a.UID, len(codes))
	return len(codes)
}

// runGrowthQueued 执行单个成长任务（动作 + 回读 + 自动领奖；与
// accountTaskAuto 同语义，结果以文字返回）。
// runGrowthQueued 执行一项队列任务。
// v1.22.0 起多返回一个 progressed：本次执行到底有没有换来进展（进度前进 / 领到奖）。
// 调用方用它喂跳过名单 —— "执行了但没进展"正是要把该任务砍出待办的判据。
func (p *Panel) runGrowthQueued(a *auth.Auth, code string) (string, bool, error) {
	act := autoActionFor(code)
	if act == nil {
		return "", false, fmt.Errorf("任务 %s 无自动动作", code)
	}
	// taskByCode 已双口径（mp 专属码自动回落 mp 列表）。
	before, err := p.taskByCode(a, code)
	if err != nil {
		return "", false, err
	}
	if before == nil {
		return "该账号无此任务", false, nil
	}
	isMP := isMPTaskCode(code)
	if before.Claimed {
		return "已完成（已领取）", true, nil
	}
	msg, err := act.run(p, a)
	if err != nil {
		return "", false, err
	}
	var after *upstream.Task
	if isMP {
		after, _ = p.taskByCodeMP(a, code)
	} else {
		after, _ = p.taskByCodeWaiting(a, code)
	}
	claimed := false
	if after != nil && after.Claimable {
		var credit, energy int64
		var cerr error
		if isMP {
			credit, energy, cerr = p.cfg.Upstream.ClaimRewardMP(a, code)
		} else {
			credit, energy, cerr = p.cfg.Upstream.ClaimReward(a, code)
		}
		if cerr == nil && (credit > 0 || energy > 0) {
			msg += fmt.Sprintf("；自动领奖 +%d 分 +%d 能", credit, energy)
			claimed = true
		}
	}
	progressed := claimed
	if after != nil {
		msg += "（进度 " + taskProgressText(after) + "）"
		// 「执行过了，但还是未完成」的判据就是这三行：进度有没有动、够不够领、领到没有。
		// 以前只印一个进度快照，于是"跑完了进度没动"看起来和"没事可做"一样。
		switch {
		case after.Claimed:
			msg += " ✓ 已领取"
			progressed = true
		case after.Current > before.Current:
			msg += fmt.Sprintf(" ✅ 进度 +%d", after.Current-before.Current)
			if after.Target > 0 && after.Current < after.Target {
				msg += fmt.Sprintf("，还差 %d 次", after.Target-after.Current)
			}
			progressed = true
		case after.Current == before.Current:
			msg += " ⚠️ 进度未变化：上游没有计入这次行为（可能未到结算时间 / 行为未被认可）"
		}
	}
	log.Printf("panel: 队列 growth uid=%s code=%s progressed=%v: %s", a.UID, code, progressed, msg)
	return msg, progressed, nil
}

// tasksQueueStatus 队列状态（轮询用）。
func (p *Panel) tasksQueueStatus(w http.ResponseWriter, r *http.Request) {
	q := p.queue()
	q.mu.Lock()
	defer q.mu.Unlock()
	items := make([]queueItem, len(q.items))
	copy(items, q.items)
	writeJSON(w, http.StatusOK, map[string]any{
		"running":    q.running,
		"total":      len(items),
		"conc":       q.conc,
		"started":    !q.startedAt.IsZero(),
		"started_at": q.startedAt,
		"seq":        q.seq,
		"items":      items,
	})
}

// schoolVouchers 我的券码：逐 CN 账号查开学季 /vouchers（3 并发，与 packages
// 同款限流），失败只在对应账号标 error。global 账号无开学季，不发上游调用。
func (p *Panel) schoolVouchers(w http.ResponseWriter, r *http.Request) {
	accts := p.cfg.Pool.List()
	type row struct {
		UID      string                   `json:"uid"`
		Nickname string                   `json:"nickname"`
		Vouchers []upstream.SchoolVoucher `json:"vouchers"`
		Err      string                   `json:"error,omitempty"`
	}
	out := make([]row, len(accts))
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, st := range accts {
		if st.Disabled {
			continue // 未占位，行末统一压掉
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
			it := row{UID: a.UID, Nickname: a.Nickname}
			switch {
			case a.IsGlobal():
				it.Err = "global realm（无开学季活动）"
			default:
				vs, err := p.cfg.Upstream.SchoolVouchers(a)
				if err != nil {
					it.Err = err.Error()
				} else {
					it.Vouchers = vs
				}
			}
			out[i] = it
		}(i, a)
	}
	wg.Wait()
	res := make([]row, 0, len(out))
	for _, it := range out {
		if it.UID != "" {
			res = append(res, it)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": res})
}
