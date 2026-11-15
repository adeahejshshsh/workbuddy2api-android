package scheduler

// T-18 / 审查 W5T4-03（P1）+ W5T4-07 + W5T4-31。
//
// 既有 accountpace_test.go 里唯一的「四族都走 accountPace」守卫是**读源码字符串
// 计数**（`strings.Count(src, "plan.wait(ctx") != 1`）。它的判别力是反的：
//   - 一次 gofmt 换行、把 st.UID 改名、把三处调用抽成循环 → 用例误红；
//   - 真正的回归（某族退回不节流，或 accountPace 被改成直接 return true）只要
//     那几行字符串还在，用例照样绿。
//
// 这里补的是**行为级**版本：给四族各配 3 个账号 + 一个「账号间节流 1 小时」的
// 配置，用带 600ms 期限的 ctx 跑一遍。正确实现下四族都必须**卡在第一个账号之前**
// （上游调用数 = 0，且必须跑满 ctx 期限）；任何一族绕过 accountPace 都会立刻把
// 3 个号全部打完（调用数 = 3）→ 变红。
//
// 为什么用 AccountSpreadSeconds 而不是改 xxxAccountDelay 包级变量：后者被
// zz_testinit_test.go 的 init() 永久置 0（审查 W6T4-01），且改写它属于全局污染。
// AccountSpreadSeconds 走 Config，天然隔离，顺带把 W5T4-31 那条「配置项覆盖族级
// fallback」的零覆盖分支也钉住。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// countingUpstream 记下每一次出站调用（路径 + 次数）的假上游。
//
// 全部回 200 + `{"code":0,"data":{}}`：本文件关心的是「有没有发出去」，不是
// 业务语义；让调用尽量成功可以避免被上游错误分类带偏（例如 12153 会触发禁用）。
type countingUpstream struct {
	srv   *httptest.Server
	calls atomic.Int32
	mu    sync.Mutex
	paths []string
}

func newCountingUpstream(t *testing.T) *countingUpstream {
	t.Helper()
	c := &countingUpstream{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.calls.Add(1)
		c.mu.Lock()
		c.paths = append(c.paths, r.URL.Path)
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"data":{}}`)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *countingUpstream) client() *upstream.Client {
	return &upstream.Client{
		HTTP: c.srv.Client(), ChatBaseCN: c.srv.URL, WebBaseCN: c.srv.URL, BillingBaseCN: c.srv.URL,
	}
}

func (c *countingUpstream) snapshot() (int32, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls.Load(), append([]string(nil), c.paths...)
}

// paceAccounts 造 n 个「四族门控全过」的 CN 个人号。
//
// 门控清单（任一不满足都会让账号被 continue，从而让「调用数 = 0」变成假绿）：
// checkin 要 RefreshTokenValue；travel 要 RefreshTokenValue + 非 global + 个人版；
// activity 要 AccessTokenValue + 非企业会话 + 个人版；keepalive 要 RefreshTokenValue。
func paceAccounts(n int) *pool.Pool {
	p := pool.New("")
	for i := 0; i < n; i++ {
		p.Add(&auth.Auth{
			UID:          string(rune('a' + i)),
			AccessToken:  "at-live",
			RefreshToken: "rt-live",
			ExpiresAt:    time.Now().Add(72 * time.Hour).Unix(),
			Nickname:     "pace",
		})
	}
	return p
}

// TestAccountPaceWiredForAllFourFamilies 四族都必须走 accountPace（行为级接线守卫）。
func TestAccountPaceWiredForAllFourFamilies(t *testing.T) {
	const (
		accounts = 3
		deadline = 600 * time.Millisecond
	)
	families := []struct {
		name string
		run  func(*Scheduler, context.Context)
	}{
		{"checkin", func(s *Scheduler, ctx context.Context) { s.RunCheckinNowCtx(ctx, false) }},
		{"travel", func(s *Scheduler, ctx context.Context) { s.RunTravelNowCtx(ctx, false) }},
		{"activity", func(s *Scheduler, ctx context.Context) { s.runActivity(ctx, false) }},
		{"keepalive", func(s *Scheduler, ctx context.Context) { s.RunKeepaliveNowCtx(ctx, false) }},
	}
	for _, f := range families {
		t.Run(f.name, func(t *testing.T) {
			up := newCountingUpstream(t)
			s := New(Config{
				Pool:     paceAccounts(accounts),
				Upstream: up.client(),
				// 账号间节流 1 小时：远大于 ctx 期限，于是「有没有节流」是可判定的。
				AccountSpreadSeconds: 3600,
			})
			ctx, cancel := context.WithTimeout(context.Background(), deadline)
			defer cancel()
			start := time.Now()
			f.run(s, ctx)
			elapsed := time.Since(start)

			calls, paths := up.snapshot()
			if calls != 0 {
				t.Fatalf("%s 族在账号间节流生效时仍打了 %d 次上游（%v）——该族绕过了 "+
					"accountPace，默认部署会全池同秒连发（WAF 403 自激链的起点）",
					f.name, calls, paths)
			}
			// 必须真的被节流卡住到 ctx 到期：若因为门控把账号全跳过而「立刻返回」，
			// 上一条断言会假绿（调用数同样是 0）。
			if elapsed < deadline/2 {
				t.Fatalf("%s 族 %v 就返回了（ctx 期限 %v）：账号被门控全部跳过，"+
					"本用例没有测到节流（fixture 失效）", f.name, elapsed, deadline)
			}
			if ctx.Err() == nil {
				t.Fatalf("%s 族在 ctx 未到期时返回，且期间零调用：行为无法解释", f.name)
			}
		})
	}
}

// TestAccountDelaySpreadSecondsOverridesFamilyFallback W5T4-31：配了
// account_spread_seconds 时，四族的族级 fallback 必须被**统一覆盖**；没配时逐字回落。
//
// 这是纯函数断言（不睡），钉住两个方向：
//   - 覆盖优先级：Seconds 赢过每个族自己的 800ms（否则运维填了配置却只对部分族生效）；
//   - 回落保真：未配置时返回的必须是**传进来的** fallback（若写死成某个常量，
//     zz_testinit 把四个变量置 0 的「测试免等待」会失效 → 整包真睡分钟级）。
func TestAccountDelaySpreadSecondsOverridesFamilyFallback(t *testing.T) {
	on := New(Config{AccountSpreadSeconds: 2})
	for _, c := range []struct {
		name string
		fb   time.Duration
	}{
		{"checkin", checkinAccountDelay},
		{"travel", travelAccountDelay},
		{"activity", activityAccountDelay},
		{"keepalive", keepaliveAccountDelay},
		{"零值", 0},
	} {
		if got := on.accountDelay(c.fb); got != 2*time.Second {
			t.Errorf("%s: accountDelay(%v)=%v want 2s（account_spread_seconds 必须覆盖族级 fallback）",
				c.name, c.fb, got)
		}
	}

	off := New(Config{})
	for _, fb := range []time.Duration{800 * time.Millisecond, 0, 1234 * time.Millisecond} {
		if got := off.accountDelay(fb); got != fb {
			t.Errorf("未配置 spread 时 accountDelay(%v)=%v，必须原样回落", fb, got)
		}
	}
}

// TestSpreadClampsOverflowToWindowEnd W5T4-07：账号数 × 最小间隔逼近窗口时，
// 末尾账号的偏移被钳到窗口最后一秒（spread.go 的 shortcut）。
//
// 该退化分支此前无用例（既有 TestSpreadRespectsMinGap 用 10 号 × 90s / 30min，
// 占用 15min 远未越界）。这里把「允许重叠」写成显式断言：不 panic、不越界、
// 不丢账号，且末尾确实重叠 —— 真到那一步该调窗口/间隔，而不是靠隐式行为兜着。
func TestSpreadClampsOverflowToWindowEnd(t *testing.T) {
	const accounts = 20
	uids := make([]string, accounts)
	for i := range uids {
		uids[i] = string(rune('a'+i)) + "-uid"
	}
	window, gap := 10*time.Minute, 120*time.Second // 600s 窗口、120s 间隔：最多塞 5 个
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	plan := newAccountSpread(taskTravel, uids, window, gap, now)
	if plan == nil {
		t.Fatal("窗口 > 0 时应生成计划")
	}
	if len(plan.at) != accounts {
		t.Fatalf("计划里只有 %d 个账号 want %d（钳制不该丢账号：丢掉的号当天不跑）", len(plan.at), accounts)
	}

	secs := int64(window / time.Second)
	distinct := map[int64]struct{}{}
	maxOff := int64(-1)
	for _, uid := range uids {
		at, ok := plan.at[uid]
		if !ok {
			t.Fatalf("%s 不在计划里", uid)
		}
		off := int64(at.Sub(now) / time.Second)
		if off < 0 || off >= secs {
			t.Fatalf("%s 偏移 %ds 越出窗口 [0,%d)", uid, off, secs)
		}
		distinct[off] = struct{}{}
		if off > maxOff {
			maxOff = off
		}
	}
	if maxOff != secs-1 {
		t.Fatalf("最大偏移 %ds want %ds（越界钳到窗口末尾这一秒）", maxOff, secs-1)
	}
	// 20 个号 / 600s 窗口 / 120s 间隔 → 至少 15 个必然落在末尾那一秒上。
	if len(distinct) >= accounts {
		t.Fatalf("偏移去重后 %d 个（共 %d 号）：钳制没生效，末尾本该重叠", len(distinct), accounts)
	}
	slots := int(secs/int64(gap/time.Second)) + 1
	if len(distinct) > slots {
		t.Fatalf("去重后仍有 %d 个不同偏移，超出窗口能容纳的 %d 个", len(distinct), slots)
	}
}

// TestSpreadPlanNilOnManualTrigger 手动入口（on=false）与未配窗口都必须得到 nil 计划。
//
// 不变量：面板手动触发要的是**立刻跑完**，不能被摊开窗口散在半小时里；
// 而未配窗口（默认部署）时也不能生成计划 —— 否则 wait 会等一个不存在的偏移。
// 计划为 nil 时 wait 立即放行、order 原样返回（既有 spread_test 已覆盖该分支）。
func TestSpreadPlanNilOnManualTrigger(t *testing.T) {
	up := newCountingUpstream(t)
	s := New(Config{Pool: paceAccounts(2), Upstream: up.client(), AccountSpreadMinutes: 30})
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	if p := s.spreadPlan(taskCheckin, false, now); p != nil {
		t.Fatal("手动触发（on=false）不该生成摊开计划")
	}
	if !s.spreadEnabled() {
		t.Fatal("配了 30 分钟窗口时 spreadEnabled 应为 true")
	}
	if p := s.spreadPlan(taskCheckin, true, now); p == nil {
		t.Fatal("配了窗口且 on=true 时应生成计划")
	}

	noWin := New(Config{Pool: paceAccounts(2), Upstream: up.client()})
	if noWin.spreadEnabled() {
		t.Fatal("未配窗口时 spreadEnabled 应为 false")
	}
	if p := noWin.spreadPlan(taskCheckin, true, now); p != nil {
		t.Fatal("未配窗口时不该生成计划（spreadPlan 必须回落 nil → 走族级节流）")
	}
}

// TestSpreadWaitHonoursContextCancel 摊开等待必须可被 ctx 取消打断。
//
// 停机时若等满窗口偏移，SIGTERM 后的收尾预算（run.sh 的 8s）会被吃掉 ——
// 这条同时是 plan.wait 的唯一直接行为断言（既有用例只经 accountPace 间接覆盖）。
func TestSpreadWaitHonoursContextCancel(t *testing.T) {
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	plan := newAccountSpread(taskCheckin, []string{"u1"}, 30*time.Minute, 0, now)
	if plan == nil {
		t.Fatal("应生成计划")
	}
	// 计划外的账号：立即放行（不该等）。
	if !plan.wait(context.Background(), "not-in-plan") {
		t.Fatal("计划外账号应立即放行")
	}
	// 计划内且偏移在未来：ctx 已取消 → 必须立刻返回 false。
	// （偏移恰好为 0 时 sleepCtx 走 d<=0 分支，同样因 ctx.Err()!=nil 返回 false。）
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if plan.wait(ctx, "u1") {
		t.Fatal("ctx 已取消但 wait 返回 true：停机时要等满窗口偏移")
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("ctx 已取消却等了 %v 才返回", el)
	}
}
