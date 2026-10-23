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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	if t.Claimed || t.Claimable {
		// 已达标未领奖也不进队列（bug 3）：单任务入口与 runAutoAll 用的都是
		// taskDone()（Claimed || Claimable），只有队列这条漏了 —— 于是「进度已满
		// 但还没点领取」的任务进队列后会把整条动作链再跑一遍（expert_5 是 5 次
		// 真实对话 + 5 组召唤事件），最后才领奖，白烧上游配额。
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
				"（7 天后自动重试；或在该账号的「任务」弹窗点「清空跳过名单」立即重试）"
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
			"要重试请在该账号的「任务」弹窗点「清空跳过名单」", d.skipped))
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
	// 按 **rune** 截断（报告 #9）：s[:140] 是字节切片，中文正好跨界时切出非法 UTF-8，
	// 经 json.Marshal 变成替换字符。与 logfmt.Truncate 同口径。
	if r := []rune(s); len(r) > 140 {
		s = string(r[:140]) + "…"
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
scanLoop:
	for i, st := range states {
		if st.Disabled {
			continue
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			// 客户端走了：直接跳出循环（bug #11）。此前用 continue 只跳过当前账号，
			// for 会把剩余账号全空转一遍（每次立刻再命中 ctx.Done），纯浪费且拖住返回。
			break scanLoop
		}
		wg.Add(1)
		go func(i int, uid string) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() { // panic 兜底（bug #14）
				if r := recover(); r != nil {
					log.Printf("ERR: [panel] tasksScanAll panic uid=%s: %v", uid, r)
					items[i].GrowthErr = "内部错误"
				}
			}()
			a := p.cfg.Pool.AuthByUID(uid)
			if a == nil {
				return
			}
			it := &items[i]
			it.UID, it.Nickname = uid, a.NicknameValue()
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
					// 达标未领奖：**直接领**，不进动作链（misakano7545 `f89f6fc0`）。
					// 这类任务此前被 growthPending 排除，又不在动作白名单里，
					// 于是奖励一直挂着直到下次执行同一任务（可能几天后）。
					// 奖励已经到手，只差点击 —— 不需要再跑一遍动作链。
					if t.Claimable && !t.Claimed {
						if credit, _, _, cerr := p.cfg.Upstream.ClaimReward(a, t.TaskCode); cerr == nil {
							// 奖励已到账，面板积分要跟着动（报告 #12）：此前不刷，
							// 得等下一轮余额刷新才变，用户以为没领到。
							p.refreshCreditsAsync(a)
							log.Printf("panel: 扫描期补领 uid=%s code=%s +%dc", uid, t.TaskCode, credit)
						} else {
							log.Printf("panel: 扫描期补领失败 uid=%s code=%s: %v", uid, t.TaskCode, cerr)
						}
						continue
					}
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
					seen[strings.ToLower(t.TaskCode)] = true
				}
				for _, t := range mpTasks {
					if seen[strings.ToLower(t.TaskCode)] {
						continue
					}
					seen[strings.ToLower(t.TaskCode)] = true
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
		Concurrency int   `json:"concurrency"`
		Growth      *bool `json:"growth"`
	}
	// 解析失败必须回 400（bug A3）：以前 `_ = ...Decode(...)` 把错误丢掉，
	// 畸形/空 JSON 也照样按默认值（concurrency=1, growth=true）真的启动一轮扫描与执行，
	// 前端只看到"已启动"，参数为什么不生效完全查不到，运维也丢掉 400 这条诊断线索。
	// 空 body 是合法输入（按默认值跑），所以只放过 io.EOF。
	if derr := json.NewDecoder(r.Body).Decode(&body); derr != nil && !errors.Is(derr, io.EOF) {
		writeErr(w, http.StatusBadRequest, "invalid json: "+derr.Error())
		return
	}
	// 指针区分「未传」与「显式 false」（bug 15）：此前 `if !body.Growth { body.Growth = true }`
	// 让调用方永远关不掉 growth 队列，字段形同虚设。
	growth := true
	if body.Growth != nil {
		growth = *body.Growth
	}
	if body.Concurrency < 1 {
		body.Concurrency = 1
	}
	if body.Concurrency > 4 {
		body.Concurrency = 4
	}
	// 后台队列必须与请求 ctx 解耦（BUG-4）：startGrowthQueue 是 `go runQueueItems` 后
	// 立即返回，handler 一写完响应，net/http 就会 cancel r.Context() —— 而派发循环里
	// `select { case sem<-…: case <-ctx.Done(): }` 两个分支同时就绪时会**随机**命中
	// ctx.Done()，剩下的账号全被标成 skipped。表现为「点一次只跑前几个账号」。
	// 定时任务入口本来就传 Background，这里对齐。
	started, total, seq, msg, lvl := p.startGrowthQueue(context.Background(), body.Concurrency, growth)
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
func (p *Panel) startGrowthQueue(ctx context.Context, concurrency int, growth bool) (started bool, total int, seq int, msg string, level string) {
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

	// 扫描段兜底（bug B10）：先置位再干活，若扫描期间 panic（上游异常数据、
	// 装配缺依赖、空指针…），q.running 会永久为 true —— 之后「执行全部待办」
	// 永远 409「队列正在执行中」，growth_hours 定时任务也不再推进，只能重启进程。
	scanOK := false
	defer func() {
		if scanOK {
			return // 交给 runQueueItems 的 defer 复位
		}
		if rec := recover(); rec != nil {
			log.Printf("panel: 队列扫描 panic（已复位，队列可用）: %v", rec)
			// 命名返回值此时仍是零值（BUG-14）：不回填的话前端会弹一条空 toast
			msg = "队列扫描阶段异常（已复位，队列可用）"
			level = "bad"
		}
		q.mu.Lock()
		q.running = false
		q.startedAt = time.Time{}
		q.mu.Unlock()
	}()

	// 扫描待办（复用扫描逻辑的拉取部分）。
	states := p.cfg.Pool.List()

	// 分组按 states 下标原位写入（bug 8）：此前各 goroutine 各自 append，完成顺序即
	// 入队顺序 —— 同一批账号两次执行，队列顺序与账号间起跑顺序都不一样，而注释
	// 承诺的是「账号分组，保持顺序」。按下标写互不冲突，wg.Wait 后顺序拼接。
	acctsByIdx := make([]queueAccount, len(states))
	hasGrow := make([]bool, len(states))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var dg queueDiag
	// 并发上限（bug #7）：每个账号打 2 次上游（ListTasks + ListTasksMP），46 个号
	// 裸并发就是 92 个请求连发 —— 正是仓库多处专门防范的 WAF 频控形态。
	// 与同文件 tasksScanAll 的 sem=4 对齐。
	scanSem := make(chan struct{}, 4)
dispatch:
	for idx, st := range states {
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
		// 抢槽要能被取消（bug 8）：裸阻塞时客户端断开后仍要等满 4 个槽位才继续，
		// 最长要等一轮上游 ListTasks/ListTasksMP。panel 其它批量路径都已改成 select+ctx。
		select {
		case scanSem <- struct{}{}:
		case <-ctx.Done():
			break dispatch
		}
		wg.Add(1)
		go func(idx int, a *auth.Auth) {
			defer wg.Done()
			defer func() { <-scanSem }()
			defer func() { // panic 兜底（bug #14）
				if r := recover(); r != nil {
					log.Printf("ERR: [panel] growth scan panic uid=%s: %v", a.UID, r)
				}
			}()
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
					dg.errs = append(dg.errs, a.NicknameValue()+"："+errBrief(err))
					mu.Unlock()
				} else {
					// seen 必须登记**全量**任务码，而不是只登记进了 one.grow 的（bug 10）：
					// 默认口径里被判成 locked/claimed/noauto/skipped 的任务不会进 one.grow，
					// mp 列表再出现同一码时会被重判一遍 —— skipped 因此被双计，
					// 用户看到「跳过 6 个」而实际只有 3 个。与 loadTaskMap 同口径。
					seen := make(map[string]bool, len(tasks))
					for _, t := range tasks {
						seen[strings.ToLower(t.TaskCode)] = true
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
						for _, t := range mpTasks {
							if seen[strings.ToLower(t.TaskCode)] {
								continue
							}
							seen[strings.ToLower(t.TaskCode)] = true
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
			acctsByIdx[idx] = one
			hasGrow[idx] = len(one.grow) > 0
		}(idx, a)
	}
	wg.Wait()

	// 按 states 原顺序拼回（稳定）：跳过的下标本来就是零值，hasGrow 挡住它们。
	var accts []queueAccount
	for i := range acctsByIdx {
		if hasGrow[i] {
			accts = append(accts, acctsByIdx[i])
		}
	}
	// 组装队列（账号分组，保持顺序）。
	var items []queueItem
	for _, one := range accts {
		for _, t := range one.grow {
			items = append(items, queueItem{UID: one.a.UID, Nickname: one.a.NicknameValue(), Kind: "growth", Code: t.TaskCode, Status: "pending"})
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

	scanOK = true // 成功交给 runQueueItems 负责复位
	go p.runQueueItems(ctx, accts, items, concurrency, seq)
	log.Printf("panel: 队列启动：%d 项（并发 %d，成长 %v）", len(items), concurrency, growth)
	return true, len(items), seq, "", "ok"
}

// RunGrowthQueueOnce 调度器 growth 时点回调（sch.SetGrowthHook 挂载）：与
// 「执行全部待办」按钮完全同管线（成长，串行并发 1）。Sequential 族
// 每日零点解锁一环，此前只能手动扫描推进；此回调让链条每天自动走一环。
// 异步执行（startGrowthQueue 启动 goroutine 即返），已在跑/无待办安全跳过。
func (p *Panel) RunGrowthQueueOnce() {
	started, total, _, _, _ := p.startGrowthQueue(context.Background(), 1, true)
	if started {
		log.Printf("panel: 定时成长任务队列已启动（%d 项）", total)
	}
}

// runQueueItems 队列执行主体：按账号分组，账号内串行（per-account 锁），
// 账号间并发（信号量）。每项结果写回队列状态。
func (p *Panel) runQueueItems(ctx context.Context, accts []queueAccount, items []queueItem, concurrency, seq int) {
	q := p.queue()
	defer func() {
		q.mu.Lock()
		q.running = false
		// startedAt 也要清（BUG-17）：只在「无可执行待办」分支清的话，正常跑完后
		// /panel/api/queue 会长期返回 started:true, running:false。
		q.startedAt = time.Time{}
		// 未执行的条目不许永远停在 pending（BUG-16）：断连/中止时把剩余项标成
		// skipped，否则前端轮询会一直看到它们 pending。seq 守卫防写脏新一轮。
		if q.seq == seq {
			for i := range q.items {
				if q.items[i].Status == "pending" {
					q.items[i].Status = "skipped"
					q.items[i].Message = "队列中止（客户端断开或已停止），未执行"
				}
			}
		}
		q.mu.Unlock()
		log.Printf("panel: 队列执行结束（共 %d 项）", len(items))
	}()

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
dispatchQueue:
	for _, one := range accts {
		// 抢槽前置（bug-09，与 packages / syncNicknames 同口径），
		// 并且要能被断连/停机打断（BUG-16）：裸阻塞时并发槽被长任务占满，
		// 派发循环就卡在 channel 上，客户端断开也不退出。
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break dispatchQueue
		}
		wg.Add(1)
		go func(one queueAccount) {
			defer wg.Done()
			// 每个账号一层 panic 兜底（bug #1）：这些 goroutine 不是 net/http 起的，
			// Go 不会替它们 recover —— 上游返回形状一变就可能 panic，死的是整个
			// 网关进程，而不是"这次任务失败"。队列状态由外层 defer 复位。
			defer func() {
				if r := recover(); r != nil {
					log.Printf("ERR: [panel] queue account=%s panic: %v", one.a.UID, r)
					p.queueSet(q, seq, one.a.UID, func(it *queueItem) {
						it.Status = "error"
						it.Message = fmt.Sprintf("内部错误：%v", r)
					})
				}
			}()
			defer func() { <-sem }()
			// per-account 互斥：与单任务/一键完成共用一把锁。
			if !p.tryLockAccount(one.a.UID) {
				p.queueSet(q, seq, one.a.UID, func(it *queueItem) {
					it.Status, it.Message = "skipped", "该账号有其它任务动作在执行，跳过"
				})
				return
			}
			defer p.unlockAccount(one.a.UID)
			// 开始前清掉可能残留的中止标记（bug B9）：队列路径此前不调 clearAborted，
			// 上一轮 watchAbort 的竞态残留会让本任务在第一次 sleepAB 就退出。
			clearAborted(one.a.UID)
			// 前置：批量接受尚未接受的任务。上游对 not_accepted 的任务不计数——
			// 面板「一键完成」一直有这步，队列路径此前漏了（表现为上报 200 但进度
			// 一直 not_accepted、无法领奖）。失败不阻塞（行为事件才是进度判据）。
			if accepted := p.acceptPendingTasks(one.a); accepted > 0 {
				if !sleepAB(one.a.UID, reportGap) {
					return // 断连：本账号不再继续
				}
			}
			// 用**入参快照**遍历，不再每轮读 q.items 的 slice header（bug #5）：
			// 那个读没有持锁，与 queueMarkAt 的元素写、以及下一轮 startGrowthQueue
			// 对 q.items 的整体替换构成数据竞争（-race 可复现），极端时序下按下标
			// 写入会落到新一轮队列的条目上。下标写入本身另有 seq 守卫。
			for i := range items {
				uid, kind, code := items[i].UID, items[i].Kind, items[i].Code
				if uid != one.a.UID {
					continue
				}
				p.queueMarkAt(seq, i, "running", "")
				var msg string
				var progressed bool
				var err error
				switch kind {
				case "growth":
					msg, progressed, err = p.runGrowthQueued(one.a, code)
				}
				if err != nil {
					p.queueMarkAt(seq, i, "error", err.Error())
				} else {
					p.queueMarkAt(seq, i, "done", msg)
				}
				switch queueVerdictOf(err, progressed) {
				case verdictReadbackFailed:
					// 回读失败（bug A4）：动作已经执行过，只是拿不到执行后的进度。
					// **既不计入跳过名单、也不清零计数** —— 上游一次 5xx/超时就白吃
					// 1/3 次机会，三次抖动后好账号 + 可做任务被砍出待办 7 天。
					p.queueMarkAt(seq, i, "error", msg+"；"+err.Error())
					log.Printf("panel: 任务回读失败 uid=%s code=%s（本轮不计入跳过名单）: %v",
						one.a.UID, code, err)
				case verdictNotApplicable:
					// 该账号没有这个任务：只标记，不计数（报告 #7）。
					p.queueMarkAt(seq, i, "skipped", msg)
				case verdictNoProgress:
					// 执行了但没进展 → 记一笔；连续 skipFailLimit 次就把该 (账号,任务码)
					// 砍出待办，别再天天"做了也没用"（7 天后自动放回，或一键清空）。
					why := "进度未变化（上游未计入这次行为）"
					if err != nil {
						why = errBrief(err)
					}
					// 用生效任务码记账（报告 #13）：runGrowthQueued 内部按
					// effectiveTaskCode 读回/领奖（按域换码的任务如 WorkBuddy_Valorant），
					// 而单任务入口也按生效码记 —— 这里用原始 code 会写出两条不同的
					// 跳过记录，同一任务被记两次「做不了」。
					added, n := p.skipNote(one.a.UID, p.effectiveTaskCode(one.a, code), why)
					note := fmt.Sprintf("（连续无效 %d/%d）", n, skipFailLimit)
					if added {
						note = fmt.Sprintf("（连续 %d 次无效 → 已跳过：不再出现在待办里，7 天后自动重试，"+
							"或在该账号的「任务」弹窗点「清空跳过名单」立即重试）", n)
					}
					if err != nil {
						p.queueMarkAt(seq, i, "error", err.Error()+note)
					} else {
						p.queueMarkAt(seq, i, "done", msg+note)
					}
					log.Printf("panel: 任务无效 uid=%s code=%s %s（%s）", one.a.UID, code, note, why)
				default:
					// 用生效任务码清零（review #6）：上面 skipNote 写的是
					// effectiveTaskCode，这里用原始 code 删不掉那条记录，
					// 按域换码的任务（WorkBuddy_Valorant）失败计数永不清零，
					// 只能等 7 天自然过期。
					p.skipReset(one.a.UID, p.effectiveTaskCode(one.a, code))
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

// queueMarkAt 按索引更新队列条目状态（条目数组固定不再增删）。
// queueMarkAt 按下标写队列状态；seq 与当前队列不符时丢弃（bug #5）。
//
// 为什么必须守卫：q.items 会被下一轮 startGrowthQueue 整体替换，此时旧一轮的
// 下标要么越界、要么指到新一轮的别的条目上 —— 两种都是错的，直接丢弃最安全。
func (p *Panel) queueMarkAt(seq, i int, status, msg string) {
	q := p.queue()
	q.mu.Lock()
	if q.seq != seq || i < 0 || i >= len(q.items) {
		q.mu.Unlock()
		return
	}
	q.items[i].Status, q.items[i].Message = status, msg
	q.mu.Unlock()
}

// queueSet 按 uid 批量改状态。
// queueSet 按 UID 改条目状态；seq 与当前队列不符时丢弃（bug 7）。
//
// 为什么需要守卫：上一轮队列的 goroutine 还没退出、下一轮 startGrowthQueue 已经
// 把 q.items 整体替换时，这个晚到的标记会把**新一轮**里同 UID 的条目全改成
// skipped —— 用户看到「刚点执行就全部跳过」，而实际上一条都没跑。
// queueMarkAt 早就有这个守卫，同文件的 queueSet 漏了。
func (p *Panel) queueSet(q *queueState, seq int, uid string, fn func(*queueItem)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.seq != seq {
		return
	}
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
	// 按 20 条分片（报告 #10）：上游对 task_codes 数组长度无公开上限，整批下发时
	// 任务多会被整批拒收（只记一条 error，后续动作照跑）。面板单任务入口与
	// runAutoAll 早就分片了，这里漏了。
	accErr := error(nil)
	for i := 0; i < len(codes); i += 20 {
		end := i + 20
		if end > len(codes) {
			end = len(codes)
		}
		if e := p.cfg.Upstream.AcceptTasks(a, codes[i:end]); e != nil {
			accErr = e
			break
		}
	}
	if err := accErr; err != nil {
		log.Printf("panel: 队列 accept uid=%s: %v（不阻塞）", a.UID, err)
		return 0
	}
	log.Printf("panel: 队列 accept uid=%s: 已接受 %d 个任务", a.UID, len(codes))
	return len(codes)
}

// errTaskReadback 回读失败（动作已执行，但拿不到执行后的进度）。
//
// 与动作失败严格区分：动作失败是「做了没用」，回读失败是「不知道有没有用」。
// 后者绝不能喂跳过名单（bug A4）—— 上游一次列表 5xx/超时抖动就白吃
// 1/3 次机会，三次抖动后好账号 + 可做任务被砍出待办 7 天，而它什么都没做错。
var errTaskReadback = errors.New("上游回读失败")

// errTaskAbsent 「该账号没有这个任务」：既不是失败也不是没进展，**不该计入跳过名单**。
// 此前它返回 (msg, false, nil)，被 queueVerdictOf 归成 verdictNoProgress ——
// 3 次后把「本来就不存在的任务」记成「做不了」，7 天内不再尝试（报告 #7）。
var errTaskAbsent = errors.New("该账号无此任务")

// queueVerdict 队列一项执行结果的归宿。
type queueVerdict int

const (
	verdictProgress       queueVerdict = iota // 有进展（或已领奖）→ 清零跳过计数
	verdictNoProgress                         // 执行了但没进展 → 计入跳过名单
	verdictReadbackFailed                     // 回读失败 → 两样都不做（不知道 ≠ 没做）
	verdictNotApplicable                      // 该账号没有这个任务 → 不计入跳过名单
)

// queueVerdictOf 把 (err, progressed) 判成三种归宿。
//
// 抽成纯函数是为了让 bug A4 的判据可以单测：跳过名单一旦被回读抖动污染，
// 要等 7 天或者用户手动清空才恢复，靠人工回归太难发现。
func queueVerdictOf(err error, progressed bool) queueVerdict {
	switch {
	case progressed:
		// 已知有进展（进度前进或已领奖）：不需要猜，直接清零计数。
		return verdictProgress
	case errors.Is(err, errTaskReadback):
		return verdictReadbackFailed
	case errors.Is(err, errTaskAbsent):
		// 任务本来就不在 → 既不算进展也不算无效，别喂跳过名单。
		return verdictNotApplicable
	default:
		// 动作失败与「执行了但没进展」同归：都是确凿的"这次没换来东西"。
		return verdictNoProgress
	}
}

// runGrowthQueued 执行一项队列任务（动作 + 回读 + 自动领奖；与 accountTaskAuto
// 同语义，结果以文字返回）。
// v1.22.0 起多返回一个 progressed：本次执行到底有没有换来进展（进度前进 / 领到奖）。
// 调用方用它喂跳过名单 —— "执行了但没进展"正是要把该任务砍出待办的判据；
// 回读失败（errTaskReadback）则是第三档，既不计入跳过名单也不清零计数。
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
		return "该账号无此任务", false, errTaskAbsent
	}
	isMP := isMPTaskCode(code)
	if before.Claimed {
		return "已完成（已领取）", true, nil
	}
	// 已达标未领奖：直接领，不重跑动作链（bug 3，与 growthPending 的过滤同口径）
	if before.Claimable {
		var credit, energy int64
		var already bool
		var cerr error
		if isMP {
			credit, energy, already, cerr = p.cfg.Upstream.ClaimRewardMP(a, code)
		} else {
			credit, energy, already, cerr = p.cfg.Upstream.ClaimReward(a, code)
		}
		if cerr == nil {
			if !already {
				p.refreshCreditsAsync(a)
			}
			return fmt.Sprintf("已达标，直接领奖 +%d 分 +%d 能", credit, energy), true, nil
		}
	}
	msg, err := act.run(p, a)
	if err != nil {
		return "", false, err
	}
	var after *upstream.Task
	var rerr error
	if isMP {
		after, rerr = p.taskByCodeMP(a, code)
	} else {
		after, rerr = p.taskByCodeWaiting(a, code)
	}
	// 回读失败 ≠ 没进展（bug A4）：动作已经执行过，只是拿不到执行后的状态。
	// 以前 rerr 被 `_` 丢掉 → after==nil → progressed 恒 false → 调用方记一次
	// 「连续无效」，上游每抖动一次吃 1/3 次机会，三次后该 (账号,任务) 被跳过 7 天。
	// 用哨兵错误把这种「不知道」交给调用方，由它决定不计入跳过名单。
	if rerr != nil {
		return msg, false, fmt.Errorf("%w: %v（动作已执行，但拿不到执行后的进度）", errTaskReadback, rerr)
	}
	claimed := false
	if after != nil && after.Claimable {
		var credit, energy int64
		var already bool
		var cerr error
		if isMP {
			credit, energy, already, cerr = p.cfg.Upstream.ClaimRewardMP(a, code)
		} else {
			credit, energy, already, cerr = p.cfg.Upstream.ClaimReward(a, code)
		}
		// 领取成功 = 无错误（bug #6）：此前要求 credit>0||energy>0，于是"领到道具
		// 但 0 分 0 能"被记成 progressed=false 并喂给跳过名单，连续 3 次后该
		// (账号,任务) 被跳过 7 天 —— 明明领到了却被系统判成"做不了"。
		if cerr == nil {
			if already {
				msg += "；奖励此前已领取"
			} else if credit > 0 || energy > 0 {
				msg += fmt.Sprintf("；自动领奖 +%d 分 +%d 能", credit, energy)
			} else {
				msg += "；已领取（道具/次数奖励）"
			}
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
	ctx := r.Context()
dispatchVouchers:
	for i, st := range accts {
		if st.Disabled {
			continue // 未占位，行末统一压掉
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		// 抢槽前置（bug-11）：与 tasksScanAll / startGrowthQueue / packages 同款，
		// 别再"先起 N 个 goroutine 再排队"。
		// 并且要能被断连打断（bug 20）：裸阻塞时用户关掉页面，handler 仍会把剩余
		// 账号全部查完 —— packages 早改成 select+ctx 了，这里漏了。
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break dispatchVouchers
		}
		wg.Add(1)
		go func(i int, a *auth.Auth) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() { // panic 兜底（bug #14）
				if r := recover(); r != nil {
					log.Printf("ERR: [panel] schoolVouchers panic uid=%s: %v", a.UID, r)
					out[i] = row{UID: a.UID, Nickname: a.NicknameValue(), Err: "内部错误"}
				}
			}()
			it := row{UID: a.UID, Nickname: a.NicknameValue()}
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

// ClaimSweepOnce 扫一遍全池，把「已达标未领」的成长任务奖励补领掉（上游 PR #205）。
//
// 与 #204 的区别：#204 是「执行动作前先补领一次」，#205 是**独立的周期清扫**——
// 任务进度是异步计分的，达标那一刻往往没人点按钮，奖励会一直挂着直到下次执行
// 同一任务（可能几天后）。这里主动扫，不依赖任何动作触发。
//
// 门控：global / 企业版没有成长体系（上游 403），直接跳过。
func (p *Panel) ClaimSweepOnce(ctx context.Context) (accounts, claimed, failed int) {
	accts := p.cfg.Pool.List()
	sem := make(chan struct{}, 3)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, st := range accts {
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.IsGlobal() || a.IsEnterpriseSession() {
			continue
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return
		}
		wg.Add(1)
		go func(a *auth.Auth) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					log.Printf("ERR: [panel] claim sweep panic uid=%s: %v", a.UID, r)
				}
			}()
			mu.Lock()
			accounts++
			mu.Unlock()
			// 两套口径都要扫：默认任务列表 + 小程序任务列表
			var tasks []upstream.Task
			if list, err := p.cfg.Upstream.ListTasks(a); err == nil {
				tasks = append(tasks, list...)
			}
			if list, err := p.cfg.Upstream.ListTasksMP(a); err == nil {
				tasks = append(tasks, list...)
			}
			seen := map[string]bool{}
			for _, t := range tasks {
				// 查询与写入必须同一口径（报告 #11）：写入用小写键，查询用原大小写
				// 时同一任务码的两种写法会各自通过 → 同一奖励被重复发起领奖。
				if seen[strings.ToLower(t.TaskCode)] || t.Claimed || !t.Claimable {
					continue
				}
				seen[strings.ToLower(t.TaskCode)] = true
				var cerr error
				if isMPTaskCode(t.TaskCode) {
					_, _, _, cerr = p.cfg.Upstream.ClaimRewardMP(a, t.TaskCode)
				} else {
					_, _, _, cerr = p.cfg.Upstream.ClaimReward(a, t.TaskCode)
				}
				mu.Lock()
				if cerr == nil {
					claimed++
				} else {
					failed++
				}
				if cerr == nil {
					// 同上（报告 #12）：清扫领到奖也要刷积分。
					p.refreshCreditsAsync(a)
				}
				mu.Unlock()
				if cerr == nil {
					log.Printf("panel: 补领成功 uid=%s code=%s", a.UID, t.TaskCode)
				} else {
					log.Printf("panel: 补领失败 uid=%s code=%s: %v", a.UID, t.TaskCode, cerr)
				}
			}
		}(a)
	}
	wg.Wait()
	return
}

// claimSweep 手动触发一次补领清扫（面板按钮/定时任务共用同一实现）。
func (p *Panel) claimSweep(w http.ResponseWriter, r *http.Request) {
	// 任务族互斥复用调度器的锁（Panel 自身没有这层），无调度器时（测试/裸用）不锁。
	const sweepKey = "claim_sweep"
	if p.cfg.Scheduler != nil {
		if !p.cfg.Scheduler.TryBeginTask(sweepKey) {
			writeErr(w, http.StatusConflict, "补领清扫正在执行中")
			return
		}
		defer p.cfg.Scheduler.EndTask(sweepKey)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	accounts, claimed, failed := p.ClaimSweepOnce(ctx)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "accounts": accounts, "claimed": claimed, "failed": failed,
	})
}
