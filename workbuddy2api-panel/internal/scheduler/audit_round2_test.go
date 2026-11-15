// audit_round2_test.go 第二轮代码审计（CODE-AUDIT-FULL.md 10.13 / 10.14）剩余项的回归钉。
//
// 每条用例都钉住一条**曾经真实存在**的失效路径，注释里写明「改坏了会怎样」——
// 只断言「函数返回了」的用例对这类调度/并发问题没有价值（它们本来就是静默失效）。
package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/cst"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// ── C7-040 探针巡检的等待下限 ────────────────────────────────────────────────

// TestWindowProbeWaitHasFloor 到点时刻已过去时，等待必须被夹到下限（审查 C7-040）。
//
// 改坏了会怎样：NextModelWindowDue 与 DueModelWindows 判据不一致时返回过去的时刻，
// wait 被压成 0 → 巡检循环「查一次立刻重查」，100% CPU 空转且每轮扫一遍全池窗口。
func TestWindowProbeWaitHasFloor(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name    string
		hasNext bool
		next    time.Time
		want    time.Duration
	}{
		{"已过去一小时", true, now.Add(-time.Hour), windowProbeMinWait},
		{"正好现在", true, now, windowProbeMinWait},
		{"近到不值得睡", true, now.Add(10 * time.Millisecond), windowProbeMinWait},
		{"三秒后", true, now.Add(3 * time.Second), 3 * time.Second},
		{"一小时后 → 空闲兜底", true, now.Add(time.Hour), windowProbeIdle},
		{"没有下一个", false, time.Time{}, windowProbeIdle},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := windowProbeWait(now, c.hasNext, c.next)
			if got != c.want {
				t.Fatalf("wait = %v, want %v", got, c.want)
			}
			if got <= 0 {
				t.Fatalf("等待时长必须 > 0，否则巡检退化成忙循环（got %v）", got)
			}
		})
	}
}

// ── C7-039 「立即探针」必须可取消 ───────────────────────────────────────────

// TestWindowProbeNowCtxIsCancellable ctx 取消要能打断「立即探针」（审查 C7-039）。
//
// 改坏了会怎样：面板按钮的 HTTP 请求被挂住最长 8×60s ≈ 8 分钟（客户端/反代早就
// 超时），停机时也没人能打断它 —— 因为它用的是 context.Background()。
func TestWindowProbeNowCtxIsCancellable(t *testing.T) {
	release := make(chan struct{})
	p, s, f := windowEnv(t, func(int) (int, string) {
		<-release // 模拟上游挂住不吐帧
		return http.StatusOK, ""
	})
	t.Cleanup(func() { close(release) })
	// 登记一个「真的已到点」的窗口：ScheduleModelWindow 只收未来时刻，所以先按
	// 未来登记，再用 RecordWindowProbe 把 ResetAt 改到过去（探针自己的簿记不做时间校验）。
	dueWindow(t, p, "glm-5.3")
	p.RecordWindowProbe("w1", "glm-5.3", time.Now().Add(-time.Second), 0, "", time.Now())
	if len(p.DueModelWindows(time.Now())) != 1 {
		t.Fatal("窗口没被登记成「已到点」，用例前置条件不成立")
	}

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(80 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	s.RunModelWindowProbeNowCtx(ctx)
	elapsed := time.Since(start)
	if elapsed > 3*time.Second {
		t.Fatalf("ctx 取消后仍等了 %v（应立刻返回）", elapsed)
	}
	// 上游必须真的被请求过：否则「快速返回」可能只是「没有待处理窗口」。
	if f.calls == 0 {
		t.Fatal("上游没被调用 —— 用例没有覆盖到探针的取消路径")
	}
}

// TestWindowProbeNowHasBoundedBudget 无 ctx 的历史入口必须有硬预算（审查 C7-039）。
//
// 它不能再用 context.Background() 无限挂：一次点击的最长等待要有个上限。
func TestWindowProbeNowHasBoundedBudget(t *testing.T) {
	if windowProbeManualBudget <= 0 || windowProbeManualBudget > 10*time.Minute {
		t.Fatalf("立即探针预算 = %v，应在 (0, 10m] 内", windowProbeManualBudget)
	}
	// 池/上游都没装配时立即返回 0，且不 panic。
	if got := New(Config{}).RunModelWindowProbeNow(); got != 0 {
		t.Fatalf("未装配时应返回 0，got %d", got)
	}
}

// ── C7-041 探针循环启动幂等 ─────────────────────────────────────────────────

// TestStartModelWindowProbeIdempotent 重复装配不能起第二个巡检循环（审查 C7-041）。
//
// 改坏了会怎样：两个循环同时扫同一批窗口，每次探针都是**真实计费**的上游请求，
// 而 probeWindow 的族锁只会让后到的那个「抢不到锁、静默失败」，白扫一轮全池。
func TestStartModelWindowProbeIdempotent(t *testing.T) {
	_, s, _ := windowEnv(t, func(int) (int, string) {
		return http.StatusOK, "data: [DONE]\n\n"
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartModelWindowProbe(ctx)
	s.StartModelWindowProbe(ctx)
	s.StartModelWindowProbe(ctx)
	if got := s.probeLoops.Load(); got != 1 {
		t.Fatalf("探针循环起了 %d 个，want 1", got)
	}
}

// ── C7-042 超长帧按「未重置」重试，不删窗口 ─────────────────────────────────

// TestWindowProbeTooLongFrameRetries 单帧超过 Scanner 上限时不能当成探针失败（审查 C7-042）。
//
// 改坏了会怎样：1MB 上限被超长帧撞到 → Scanner 报错 → 归到「其它错误」→ 直接删窗口
// 记录，该窗口当天不再尝试，日志只有一行 "token too long"。归到「仍限流」这一档则
// 走 15s 重试：既不清冷却、也不丢窗口。
func TestWindowProbeTooLongFrameRetries(t *testing.T) {
	huge := strings.Repeat("x", (1<<20)+4096)
	body := `data: {"choices":[{"delta":{"content":"` + huge + `"}}]}` + "\n\ndata: [DONE]\n\n"
	p, s, _ := windowEnv(t, func(int) (int, string) { return http.StatusOK, body })
	reset := dueWindow(t, p, "glm-5.3")

	if got := s.runWindowProbes(t.Context(), p.DueModelWindows(reset.Add(time.Second))); got != 1 {
		t.Fatalf("应发出 1 次探针，got %d", got)
	}
	w := p.ModelWindows()
	if len(w) != 1 {
		t.Fatalf("超长帧不该删窗口记录，got %+v", w)
	}
	if w[0].Attempts != 1 {
		t.Fatalf("应记为「未重置、重试 1 次」，Attempts=%d", w[0].Attempts)
	}
}

// ── C7-043 空 choices 心跳帧不算「已开窗」 ──────────────────────────────────

// TestWindowProbeEmptyChoicesIsNotOpen 空 delta 的 choices 帧不能判开窗（审查 C7-043）。
//
// 改坏了会怎样：上游用 `{"choices":[{"delta":{}}]}` 做心跳时被判「额度窗口已重置」→
// 清掉 6004 冷却 + 删窗口记录，可额度其实没回来，下一条真实请求继续 429，而这次
// 再没人盯着那条窗口了（记录已被删）。
func TestWindowProbeEmptyChoicesIsNotOpen(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{}}]}\n\ndata: [DONE]\n\n"
	p, s, _ := windowEnv(t, func(int) (int, string) { return http.StatusOK, body })
	reset := dueWindow(t, p, "glm-5.3")
	p.CooldownSoftForModel("w1", time.Minute, reset, "glm-5.3", "6004 rate limited")

	if got := s.runWindowProbes(t.Context(), p.DueModelWindows(reset.Add(time.Second))); got != 1 {
		t.Fatalf("应发出 1 次探针，got %d", got)
	}
	st, ok := p.Status("w1")
	if !ok {
		t.Fatal("账号不应消失")
	}
	for _, m := range st.RateLimitedModels {
		if m.Model == "glm-5.3" {
			return // 正确：冷却还在（没被空心跳帧清掉）
		}
	}
	t.Fatal("空 choices 帧把 6004 冷却清掉了 —— 额度其实没恢复")
}

// ── C7-054 抽奖次数夹上限 ───────────────────────────────────────────────────

// TestStreakLotteryChancesClamped 上游返回的抽奖次数必须有硬上限（审查 C7-054）。
//
// 改坏了会怎样：上游字段异常/解析错位（例如回 9999）→ 网关对同一个号连发上万次
// 抽奖请求，一次就把号打进上游风控。
func TestStreakLotteryChancesClamped(t *testing.T) {
	var draws atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/activity/growth/streak"):
			// 空档位：不触发兑换，只走「抽奖」这一段。
			w.Write([]byte(`{"code":0,"msg":"ok","data":{"makeup_cards":{"balance":0},"redemption_status":{"tiers":[]}}}`))
		case strings.HasSuffix(r.URL.Path, "/activity/growth/heatmap"):
			w.Write([]byte(`{"code":0,"msg":"ok","data":{"cells":[]}}`))
		case strings.HasSuffix(r.URL.Path, "/activity/growth/lottery/summary"):
			w.Write([]byte(`{"code":0,"msg":"ok","data":{"chances":9999}}`))
		case strings.HasSuffix(r.URL.Path, "/activity/growth/lottery/draw"):
			draws.Add(1)
			w.Write([]byte(`{"code":0,"msg":"ok","data":{"prize":"x"}}`))
		default:
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		}
	}))
	t.Cleanup(srv.Close)

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	p.Add(a)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.streakBonusAccount(a)

	if got := draws.Load(); got != maxLotteryDrawPerRound {
		t.Fatalf("抽奖 %d 次，want 夹到 %d 次（上游返回 9999）", got, maxLotteryDrawPerRound)
	}
}

// TestStreakGrowthStreakQueriedOnce 连登总览每号每轮只查一次（审查 C7-057）。
//
// 改坏了会怎样：makeupYesterday 内部又查一次 → 46 个号每天白多 46 次公网往返。
func TestStreakGrowthStreakQueriedOnce(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/activity/growth/streak"):
			calls.Add(1)
			w.Write([]byte(`{"code":0,"msg":"ok","data":{"makeup_cards":{"balance":1},"redemption_status":{"tiers":[]}}}`))
		case strings.HasSuffix(r.URL.Path, "/activity/growth/heatmap"):
			// 昨日漏签：会走 makeupYesterday，历史实现里这里会再查一次 streak。
			yesterday := cst.Now().AddDate(0, 0, -1).Format("2006-01-02")
			w.Write([]byte(`{"code":0,"msg":"ok","data":{"cells":[{"date":"` + yesterday + `","score":0}]}}`))
		case strings.HasSuffix(r.URL.Path, "/activity/growth/lottery/summary"):
			w.Write([]byte(`{"code":0,"msg":"ok","data":{"chances":0}}`))
		default:
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		}
	}))
	t.Cleanup(srv.Close)

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	p.Add(a)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.streakBonusAccount(a)

	if got := calls.Load(); got != 1 {
		t.Fatalf("GrowthStreakFull 被调 %d 次，want 1（补签应复用已取到的总览）", got)
	}
}

// ── C7-065 本轮超时后不能整批塌缩到同一秒 ───────────────────────────────────

// TestSpreadLateRoundKeepsSpacing 计划时刻已成过去时，后面的号按相对间距顺延（审查 C7-065）。
//
// 改坏了会怎样：本轮自身耗时（每号节流 + 上游慢）超过窗口后，剩余账号的 at 全成了
// 过去 → 每个 wait 立即放行 → 后半批在**同一秒**连发，摊开在最需要它的时候失效。
// 用例把锚点放在一小时前模拟「本轮已经落后」，再断言相邻两个号的等待量仍是它们
// 计划上的间距（而不是 0）。
func TestSpreadLateRoundKeepsSpacing(t *testing.T) {
	uids := []string{"a", "b", "c", "d", "e", "f"}
	anchor := time.Now().Add(-time.Hour)
	// 3 秒窗口：偏移是整秒，6 个号必然覆盖 0/1/2 三个桶，最后一批间距 = 2s。
	plan := newAccountSpread(taskCheckin, uids, 3*time.Second, 0, anchor)
	if plan == nil {
		t.Fatal("窗口 > 0 应生成计划")
	}
	list := make([]pool.Status, len(uids))
	for i, uid := range uids {
		list[i] = pool.Status{UID: uid}
	}
	ordered := plan.order(list)
	span := plan.at[ordered[len(ordered)-1].UID].Sub(plan.at[ordered[0].UID])
	if span <= 0 {
		t.Skipf("哈希把全部账号挤到同一秒（span=%v），本用例不适用", span)
	}

	start := time.Now()
	for _, st := range ordered {
		if !plan.wait(t.Context(), st.UID) {
			t.Fatal("wait 不该返回 false")
		}
	}
	elapsed := time.Since(start)
	if elapsed < span-150*time.Millisecond {
		t.Fatalf("整批只用了 %v（计划跨度 %v）—— 计划过期后塌缩成同一秒", elapsed, span)
	}
	// 第一个号必须立即放行（它自己的时刻早就过了），漂移量应≈锚点到现在。
	if plan.drift < 30*time.Minute {
		t.Fatalf("drift = %v，应≈1h（本轮已落后一小时）", plan.drift)
	}
}

// TestSpreadOnTimePlanStillImmediate 计划未过期时 wait 不该被 drift 改动（审查 C7-065 回归面）。
func TestSpreadOnTimePlanStillImmediate(t *testing.T) {
	now := time.Now()
	plan := newAccountSpread(taskCheckin, []string{"a"}, time.Minute, 0, now.Add(-time.Hour))
	// 手动把该号的计划时刻挪到「刚刚过去」：不产生滞后，后续等待量不变。
	plan.at["a"] = time.Now().Add(-time.Millisecond)
	start := time.Now()
	if !plan.wait(t.Context(), "a") {
		t.Fatal("wait 应返回 true")
	}
	if el := time.Since(start); el > 200*time.Millisecond {
		t.Fatalf("已到点的账号应立即放行，实际等了 %v", el)
	}
}

// ── C7-070 网页腿整批预算 ───────────────────────────────────────────────────

// TestWebActivityBatchBudgetStopsLegs 整批预算用完就不再跑网页腿（审查 C7-070）。
//
// 改坏了会怎样：46 个国际号 × (180s + 30s 收尾) ≈ 2.7 小时串行占着 TaskActivity
// 族锁 → 下一个 activity 时点整轮被「上一轮仍在执行」静默跳过。
//
// 这里把预算压到 0（确定性触发「已用完」分支），并断言**活跃上报照旧**——
// 两条腿在上游各自独立，缺一条不该连带丢掉另一条。
func TestWebActivityBatchBudgetStopsLegs(t *testing.T) {
	withGlobalSwitch(t)
	f := newWebAgentFake(t)
	p := pool.New("")
	for _, uid := range []string{"g1", "g2", "g3"} {
		p.Add(globalAccount(t, uid))
	}
	up := &upstream.Client{HTTP: f.srv.Client(), GlobalEnabled: true,
		ChatBaseCN: f.srv.URL, BillingBaseCN: f.srv.URL,
		ChatBaseGlobal: f.srv.URL, BillingBaseGlobal: f.srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	old := webActivityBatchBudget
	webActivityBatchBudget = 0 // 预算立刻用完
	t.Cleanup(func() { webActivityBatchBudget = old })

	s.runActivity(t.Context(), false)

	if f.sawPath("/console/as/conversations/") {
		t.Fatalf("预算已用完却仍建了网页会话: %v", f.paths)
	}
	// 上报腿必须照常跑完 3 个号。
	reports := 0
	f.mu.Lock()
	for _, path := range f.paths {
		if strings.HasSuffix(path, "/report") {
			reports++
		}
	}
	f.mu.Unlock()
	if reports != 3 {
		t.Fatalf("活跃上报 %d 次，want 3（网页腿的预算不该连带跳过上报）: %v", reports, f.paths)
	}
}

// TestWebActivityBatchBudgetAllowsLegs 预算充足时网页腿照常跑（防止把开关写死）。
func TestWebActivityBatchBudgetAllowsLegs(t *testing.T) {
	withGlobalSwitch(t)
	f := newWebAgentFake(t)
	p := pool.New("")
	p.Add(globalAccount(t, "g1"))
	up := &upstream.Client{HTTP: f.srv.Client(), GlobalEnabled: true,
		ChatBaseCN: f.srv.URL, BillingBaseCN: f.srv.URL,
		ChatBaseGlobal: f.srv.URL, BillingBaseGlobal: f.srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.runActivity(t.Context(), false)
	if !f.sawPath("/console/as/conversations/") {
		t.Fatalf("预算充足时网页腿应照常执行: %v", f.paths)
	}
}

// ── C7-071 网页腿当日表的清理 ───────────────────────────────────────────────

// TestWebLegDonePrunesOldDays 写入时顺手清掉非今日条目（审查 C7-071）。
//
// 改坏了会怎样：表只写不删，长期运行 + 账号轮换让它随历史 uid 数单调增长。
func TestWebLegDonePrunesOldDays(t *testing.T) {
	s := New(Config{})
	s.webLeg.done = map[string]string{
		"old":   "2020-01-01",
		"today": cst.Now().Format("2006-01-02"),
	}
	s.markWebActivityDone("new")
	if _, ok := s.webLeg.done["old"]; ok {
		t.Fatalf("旧日期条目没被清理: %+v", s.webLeg.done)
	}
	if _, ok := s.webLeg.done["new"]; !ok {
		t.Fatalf("新条目没写进去: %+v", s.webLeg.done)
	}
	if _, ok := s.webLeg.done["today"]; !ok {
		t.Fatalf("当日条目被误删: %+v", s.webLeg.done)
	}
}

// ── C7-003 TaskRunning 不得抢锁 ─────────────────────────────────────────────

// TestTaskRunningDoesNotStealTaskLock 状态查询不能抢走任务族锁（审查 C7-003）。
//
// 改坏了会怎样：面板每 2s 轮询一次 TaskRunning，TryLock 探测在「锁恰好空着」的
// 瞬间会把锁抢走一瞬；正好与排程的 TryBeginTask 撞车时后者返回 false → 该族整轮
// 被跳过、只留一行 log，且当天不重试（missedToday 认为已跑过）。
func TestTaskRunningDoesNotStealTaskLock(t *testing.T) {
	s := New(Config{})
	if s.TaskRunning(TaskCheckin) {
		t.Fatal("没人跑时应为 false")
	}
	if !s.TryBeginTask(TaskCheckin) {
		t.Fatal("首次应拿得到")
	}
	if !s.TaskRunning(TaskCheckin) {
		t.Fatal("持锁期间 TaskRunning 应为 true（面板要显示「进行中」）")
	}
	s.EndTask(TaskCheckin)
	if s.TaskRunning(TaskCheckin) {
		t.Fatal("释放后应为 false")
	}

	// 并发压测：轮询线程不停查状态，主线程每次 TryBeginTask 都必须成功。
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = s.TaskRunning(TaskCheckin)
			}
		}
	}()
	const rounds = 2000
	failed := 0
	for i := 0; i < rounds; i++ {
		if s.TryBeginTask(TaskCheckin) {
			s.EndTask(TaskCheckin)
			continue
		}
		failed++
	}
	close(stop)
	wg.Wait()
	if failed != 0 {
		t.Fatalf("%d/%d 次 TryBeginTask 被 TaskRunning 抢锁打断（状态查询不该占锁）", failed, rounds)
	}
}

// ── C7-006 停机要等在飞批次（限时） ─────────────────────────────────────────

// TestWaitBatchesHonoursTimeout 停机等待有上限，批次收尾后立即返回（审查 C7-006）。
func TestWaitBatchesHonoursTimeout(t *testing.T) {
	s := New(Config{})
	s.batchWG.Add(1)

	start := time.Now()
	s.waitBatches(30 * time.Millisecond)
	if el := time.Since(start); el < 20*time.Millisecond || el > time.Second {
		t.Fatalf("超时等待 = %v，应在 30ms 量级（不能被慢批次无限拖住）", el)
	}

	s.batchWG.Done()
	start = time.Now()
	s.waitBatches(time.Second)
	if el := time.Since(start); el > 300*time.Millisecond {
		t.Fatalf("批次已收尾应立即返回，实际等了 %v", el)
	}
}

// ── C7-007 / C7-075 后台循环启动幂等 ────────────────────────────────────────

// TestBackgroundLoopsStartOnce 余额刷新与补领清扫重复装配只起一个循环（审查 C7-007/075）。
//
// 改坏了会怎样：多个循环并存 → 同一周期把全池请求扇出多遍，每遍都是真实上游调用。
func TestBackgroundLoopsStartOnce(t *testing.T) {
	s := New(Config{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.StartBalanceRefresh(ctx, time.Hour)
	s.StartBalanceRefresh(ctx, time.Hour)
	if got := s.balanceLoops.Load(); got != 1 {
		t.Fatalf("余额刷新循环起了 %d 个，want 1", got)
	}

	s.StartTaskSweep(ctx, time.Hour)
	s.StartTaskSweep(ctx, time.Hour)
	if got := s.sweepLoops.Load(); got != 1 {
		t.Fatalf("补领清扫循环起了 %d 个，want 1", got)
	}
}

// ── C7-074 清扫抖动上界 ─────────────────────────────────────────────────────

// TestSweepJitterClamped 抖动千分比夹到 [0,500]（审查 C7-074）。
//
// 改坏了会怎样：传 2000 → jitterFrac=2 → delay 算成负数再被钳 0 → 清扫循环变成
// 零间隔忙循环，对全池连发列表请求。
func TestSweepJitterClamped(t *testing.T) {
	s := New(Config{})
	s.SetTaskSweepJitter(2000)
	if got := s.sweepJitterFrac(); got != 0.5 {
		t.Fatalf("抖动比例 = %v，want 0.5（上界 500‰）", got)
	}
	s.SetTaskSweepJitter(-1)
	if got := s.sweepJitterFrac(); got != 0 {
		t.Fatalf("负值应夹到 0，got %v", got)
	}
}

// ── C7-081 / C7-082 401 求证的退避 ──────────────────────────────────────────

// refreshCount healFake 的 refresh 调用次数（healFake 在 balance_401_test.go）。
func (f *healFake) refreshCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refresh
}

// TestAuthHealSessionDeadButUsableBacksOff 「RT 死但 AT 可用」也要记退避（审查 C7-081）。
//
// 改坏了会怎样：这类号每 30 分钟一轮的余额刷新都会再打一次 refresh，一天 ~48 次
// 注定失败的上游写请求（还是轮换 RT 的接口）。
func TestAuthHealSessionDeadButUsableBacksOff(t *testing.T) {
	f := &healFake{sessionDie: true}
	srv := f.server(t)
	// ExpiresAt 远未来 → SessionDeadButUsable 为 true（AT 还能干活）。
	a := &auth.Auth{UID: "u1", AccessToken: "at-old", RefreshToken: "rt-1", ExpiresAt: 9999999999}
	s := healScheduler(t, srv.URL, a)

	s.RunBalanceRefreshNow()
	s.RunBalanceRefreshNow()

	if got := f.refreshCount(); got != 1 {
		t.Fatalf("refresh 被调 %d 次，want 1（第二次应被 %s 退避挡住）", got, authHealBackoff)
	}
}

// TestAuthHealPrunesExpired 退避表写入时清理过期条目（审查 C7-082）。
func TestAuthHealPrunesExpired(t *testing.T) {
	s := New(Config{})
	s.authHealAt = map[string]time.Time{
		"stale": time.Now().Add(-2 * authHealBackoff),
		"fresh": time.Now(),
	}
	s.noteAuthHeal("u1")
	if _, ok := s.authHealAt["stale"]; ok {
		t.Fatalf("过期条目没被清理: %+v", s.authHealAt)
	}
	if _, ok := s.authHealAt["fresh"]; !ok {
		t.Fatalf("未过期的条目被误删: %+v", s.authHealAt)
	}
	if _, ok := s.authHealAt["u1"]; !ok {
		t.Fatalf("本次求证没记上: %+v", s.authHealAt)
	}
}

// ── C7-002 / C7-085 国际号签到后仍要刷余额 ──────────────────────────────────

// TestCheckinGlobalAlsoRefreshesBalance 国际号签到不再 `continue` 掉余额查询
// （审查 C7-002 / 10.13 P-07）。
//
// 改坏了会怎样：global 号的权威余额只由 5 分钟余额刷新覆盖，签到轮次跳过余额查询
// → 余额循环被关停/被族锁跳过时，credit_floor 与面板额度长期陈旧。
func TestCheckinGlobalAlsoRefreshesBalance(t *testing.T) {
	withGlobalSwitch(t)
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/checkin-activity-status"):
			w.Write([]byte(`{"code":0,"msg":"ok","data":{"active":false}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource-summary"):
			w.Write([]byte(`{"code":0,"msg":"ok","data":{"Packages":[]}}`))
		default:
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		}
	}))
	t.Cleanup(srv.Close)

	p := pool.New("")
	p.Add(globalAccount(t, "g1"))
	up := &upstream.Client{HTTP: srv.Client(), GlobalEnabled: true,
		ChatBaseCN: srv.URL, BillingBaseCN: srv.URL,
		ChatBaseGlobal: srv.URL, BillingBaseGlobal: srv.URL}
	s := New(Config{Pool: p, Upstream: up, CheckinHours: []int{9, 21}})

	s.RunCheckinNowCtx(t.Context(), false)

	mu.Lock()
	defer mu.Unlock()
	sawCheckin, sawBalance := false, false
	for _, path := range paths {
		if strings.HasSuffix(path, "/checkin-activity-status") {
			sawCheckin = true
		}
		if strings.HasSuffix(path, "/get-user-resource-summary") {
			sawBalance = true
		}
	}
	if !sawCheckin {
		t.Fatalf("国际号没走签到分支: %v", paths)
	}
	if !sawBalance {
		t.Fatalf("国际号签到后没有查余额（continue 没去掉）: %v", paths)
	}
}

// ── C7-048 旅行是否覆盖禁用号 ───────────────────────────────────────────────

// TestTravelIncludesDisabledWhenConfigured 打开 include_disabled_in_tasks 后禁用号也旅行
// （审查 C7-048）。此前旅行恒跳过禁用号，与签到/活跃/保活/余额四族不一致。
func TestTravelIncludesDisabledWhenConfigured(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: `{"id":1}`, state: `{"state":"idle","daily_limit_reached":true}`}
	srv := stub.server()
	t.Cleanup(srv.Close)

	s, p := newTravelScheduler(t, srv, "u1")
	p.Disable("u1", "test")

	s.RunTravelNowCtx(t.Context(), false)
	if n := stub.statusCalls.Load(); n != 0 {
		t.Fatalf("默认不该覆盖禁用号，status 被调 %d 次", n)
	}

	s.SetIncludeDisabledInTasks(true)
	s.RunTravelNowCtx(t.Context(), false)
	if n := stub.statusCalls.Load(); n == 0 {
		t.Fatal("打开 include_disabled_in_tasks 后禁用号应参与旅行")
	}
}

// ── C7-062 补签也要账号间节流 ───────────────────────────────────────────────

// TestCheckinRetryPacesAccounts 补签循环逐号之间有节流（审查 C7-062 / 10.13 P-24）。
//
// 改坏了会怎样：一批几十个号同时瞬时失败时，补签轮次变成全速连发 —— 正是本仓
// 多处防范的 WAF 频控形态。
func TestCheckinRetryPacesAccounts(t *testing.T) {
	f := &flakyCheckinUpstream{failFirst: 1000}
	s, p, srv := newCheckinTestScheduler(t, f)
	t.Cleanup(srv.Close)
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at2", RefreshToken: "rt2", ExpiresAt: 9999999999})

	shrinkRetryDelays(t, 5*time.Millisecond)
	oldDelay := checkinAccountDelay
	checkinAccountDelay = 60 * time.Millisecond
	t.Cleanup(func() { checkinAccountDelay = oldDelay })

	start := time.Now()
	s.checkinRetryLoop(t.Context(), []string{"u1", "u2"})
	elapsed := time.Since(start)
	if elapsed < 55*time.Millisecond {
		t.Fatalf("两个号只用了 %v —— 补签没有账号间节流", elapsed)
	}
}

// ── C7-093 补跑标记要用 WriteKeep（损坏可回退 .bak） ────────────────────────

// TestRunStampsSurviveCorruption 标记文件掉电变全 0 时从 .bak 回退（审查 C7-093）。
//
// 改坏了会怎样：用 safefile.Write 就没有 .bak，读成空表 = 重启再多补一整轮
// 全池签到/保活（每次都是真实上游调用）。
func TestRunStampsSurviveCorruption(t *testing.T) {
	f := filepath.Join(t.TempDir(), "run_stamps.json")
	s := New(Config{RunStampFile: f})
	now := time.Now()
	s.markRan([]taskKind{taskCheckin}, now)
	s.markRan([]taskKind{taskTravel}, now) // 第二次写入把第一份内容留成 .bak

	// 掉电现场：正文长度正确但内容全 0。
	if err := os.WriteFile(f, make([]byte, 8), 0o600); err != nil {
		t.Fatal(err)
	}
	if !s.ranToday(taskCheckin, now) {
		t.Fatal("应从 .bak 回退读到 checkin 标记（否则重启会多补一整轮签到）")
	}
}

// ── C7-056 连登管家是否覆盖禁用号 ───────────────────────────────────────────

// TestStreakIncludesDisabledWhenConfigured 打开 include_disabled_in_tasks 后禁用号也
// 兑换/抽奖（审查 C7-056），此前恒跳过，与其余四族不一致。
func TestStreakIncludesDisabledWhenConfigured(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/activity/growth/streak") {
			calls.Add(1)
		}
		w.Write([]byte(`{"code":0,"msg":"ok","data":{"makeup_cards":{"balance":0},"redemption_status":{"tiers":[]}}}`))
	}))
	t.Cleanup(srv.Close)

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Disable("u1", "test")
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.RunStreakBonusNowCtx(t.Context())
	if got := calls.Load(); got != 0 {
		t.Fatalf("默认不该覆盖禁用号，GrowthStreakFull 被调 %d 次", got)
	}

	s.SetIncludeDisabledInTasks(true)
	s.RunStreakBonusNowCtx(t.Context())
	if got := calls.Load(); got == 0 {
		t.Fatal("打开 include_disabled_in_tasks 后禁用号应参与连登兑换")
	}
}

// ── C7-083 余额摊开的抖动 key 必须随轮次变化 ────────────────────────────────

// TestBalanceStaggerKeyVariesByRound 同一账号在不同轮次必须得到不同的偏移（审查 C7-083）。
//
// 改坏了会怎样：key 恒定 → 每轮第 i 个账号永远落在同一相对位置，上游看到「同 IP 上
// N 个号以完全相同的顺序与间隔刷新」，防机械节拍的目标落空。
func TestBalanceStaggerKeyVariesByRound(t *testing.T) {
	window := time.Minute
	round1 := time.Unix(1_800_000_000, 0)

	same1 := spreadStaggerDelay(window, balanceStaggerKey("u1", round1))
	same1b := spreadStaggerDelay(window, balanceStaggerKey("u1", round1))
	if same1 != same1b {
		t.Fatalf("同一轮内偏移应稳定：%v vs %v", same1, same1b)
	}
	diff := 0
	for i := 1; i <= 4; i++ {
		if spreadStaggerDelay(window, balanceStaggerKey("u1", round1.Add(time.Duration(i)*time.Minute))) != same1 {
			diff++
		}
	}
	if diff == 0 {
		t.Fatal("跨轮偏移完全没变 —— 轮次没进 key")
	}
	if balanceStaggerKey("u1", round1) == balanceStaggerKey("u2", round1) {
		t.Fatal("不同账号的 key 不该相同")
	}
}

// ── C7-080 401 求证后的落盘必须带重试 ──────────────────────────────────────

// TestBalanceAuthHealSavesWithRetry 求证成功后用 SaveAtomicRetry（审查 C7-080）。
//
// 落盘失败在单测里难以注入，这里按本仓既有的「接线断言」风格钉住调用点：
// RT 已在服务端轮换，一次单发写失败就意味着重启后这个号要重新登录。
func TestBalanceAuthHealSavesWithRetry(t *testing.T) {
	raw := mustReadFile(t, "balance_401.go")
	src := string(raw)
	if !strings.Contains(src, "a.SaveAtomicRetry()") {
		t.Fatal("balance_401.go 未用 SaveAtomicRetry —— 与 keepalive 路径口径不一，落盘失败会丢新 RT")
	}
	if strings.Contains(src, "a.SaveAtomic()") {
		t.Fatal("balance_401.go 仍在用单次 SaveAtomic")
	}
}
