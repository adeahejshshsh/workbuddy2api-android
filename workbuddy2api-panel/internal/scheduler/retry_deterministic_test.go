package scheduler

// T-14 / 审查 W6T4-02 + W6T4-03（P1）：把「固定睡眠后反断言计数不变」换成确定性版本。
//
// 既有 checkin_retry_test.go 与 sweep_test.go 的负向断言形态是：
//
//	time.Sleep(120 * time.Millisecond)
//	if got := calls.Load() - before; got != 0 { t.Fatalf(...) }
//
// 它同时踩两个坑：
//  1. **慢机误红**：机器抖动超过窗口时，被测逻辑还没来得及「什么都不做」就被判红；
//  2. **快机漏检**：真实回归（补签窗口提前、暂停失效）也可能在窗口内没体现。
//
// 本文件不用任何「睡一会儿再看」的断言，改用两种确定性手段：
//   - **同步返回**：checkinRetryLoop 是纯同步函数（队列空或 ctx 取消即 return，
//     不 spawn goroutine）。它返回之后就不可能再有上游调用 —— 「返回后计数不变」
//     是结构性事实，不需要等；
//   - **事件通道**：sweep 的「panic 后族锁必须释放」用「执行体第二次被进入」作为
//     事件。执行体再次被进入本身就证明 RunTaskSweepNow 又拿到了 TaskSweep 锁
//     （拿不到就 return，根本不会调执行体），比轮询 2ms 抢锁干净。
//
// 保留的有界等待都是**正等待**（等一个必然发生的事件，上限 10s）：慢机只是更慢，
// 不会误红。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// withRetryDelays 把补签阶梯压到毫秒级（生产 10m/40m）并在用例结束还原。
//
// 与既有 shrinkRetryDelays 同形但独立命名：本文件不改既有用例，也不依赖它们。
func withRetryDelays(t *testing.T, ds ...time.Duration) {
	t.Helper()
	old := checkinRetryDelays
	checkinRetryDelays = ds
	t.Cleanup(func() { checkinRetryDelays = old })
}

// detCheckinUpstream 签到端点按脚本响应；其余端点给最小合法体。
//
// onCheckin 在**响应写出之前**调用，用于「上游主动取消 ctx」这类场景 —— 这是把
// 「取消发生在第几次调用之内」变成确定性事实的唯一手段（不靠睡眠赌时序）。
type detCheckinUpstream struct {
	failFirst int32
	status    int    // 失败时的 HTTP 状态（500=瞬时 / 400=确定性拒绝）
	body      string // 失败时的响应体
	calls     atomic.Int32
	onCheckin func()
	srv       *httptest.Server
}

func (f *detCheckinUpstream) start(t *testing.T) {
	t.Helper()
	if f.status == 0 {
		f.status = http.StatusInternalServerError
	}
	if f.body == "" {
		f.body = `{"code":10001,"msg":"签到失败，请稍后重试","requestId":"test"}`
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/daily-checkin") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"code":0,"data":{}}`)
			return
		}
		n := f.calls.Add(1)
		if f.onCheckin != nil {
			f.onCheckin()
		}
		if n <= f.failFirst {
			w.WriteHeader(f.status)
			_, _ = io.WriteString(w, f.body)
			return
		}
		_, _ = io.WriteString(w, `{"code":0,"msg":"ok","data":{}}`)
	}))
	t.Cleanup(f.srv.Close)
}

func (f *detCheckinUpstream) client() *upstream.Client {
	return &upstream.Client{
		HTTP: f.srv.Client(), ChatBaseCN: f.srv.URL, WebBaseCN: f.srv.URL, BillingBaseCN: f.srv.URL,
	}
}

// detCheckinEnv 一个 CN 个人号的调度器 + 假上游。
func detCheckinEnv(t *testing.T, f *detCheckinUpstream) (*Scheduler, *pool.Pool) {
	t.Helper()
	f.start(t)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999, Nickname: "确定性号"})
	return New(Config{Pool: p, Upstream: f.client()}), p
}

// TestCheckinRetrySkipsAlreadyDoneDeterministic 已签账号不进补签循环。
//
// 不变量：CheckinDoneToday 为真时，checkinRetryLoop 打 0 次上游。
// 确定性来源：循环是同步的 —— 返回即意味着「这一批的每一轮都已走完」，返回后再断言
// 计数，不存在「再等 120ms 看看」的窗口。
func TestCheckinRetrySkipsAlreadyDoneDeterministic(t *testing.T) {
	withRetryDelays(t, time.Millisecond)
	f := &detCheckinUpstream{failFirst: 0}
	s, p := detCheckinEnv(t, f)
	p.NoteCheckinDone("u1") // 用户在手机上签了 / 上一轮补签成功

	s.checkinRetryLoop(context.Background(), []string{"u1"}) // 同步：返回即终态

	if got := f.calls.Load(); got != 0 {
		t.Fatalf("已签账号被补签打了 %d 次上游（返回后计数即为终值）", got)
	}
}

// TestCheckinRetryAbandonsNonTransientDeterministic 非瞬时业务拒绝：一轮就放弃，
// 不占满补签阶梯、也不再打上游。
//
// 不变量：400 + 业务码（企业号不支持/参数错）不属于瞬时错误 → 该号本轮出队。
// 为什么必须放弃：重试不会有不同结果，只是每天多打几轮无效请求（白撞风控面）。
func TestCheckinRetryAbandonsNonTransientDeterministic(t *testing.T) {
	withRetryDelays(t, time.Millisecond, time.Millisecond, time.Millisecond)
	f := &detCheckinUpstream{
		failFirst: 1 << 30, // 永远失败
		status:    http.StatusBadRequest,
		body:      `{"code":11101,"msg":"bad params"}`,
	}
	s, p := detCheckinEnv(t, f)

	s.checkinRetryLoop(context.Background(), []string{"u1"}) // 同步返回

	if got := f.calls.Load(); got != 1 {
		t.Fatalf("非瞬时错误打了 %d 次上游 want 1（一轮即放弃，不该重试）", got)
	}
	if p.CheckinDoneToday("u1") {
		t.Fatal("400 被当成签到成功")
	}
	s.retryMu.Lock()
	pending := len(s.retryPending)
	s.retryMu.Unlock()
	if pending != 0 {
		t.Fatalf("循环退出后队列仍留 %d 个账号（下一轮会被重复追打）", pending)
	}
}

// TestCheckinRetryCancelledUpstreamStopsImmediately ctx 取消后不得再打上游。
//
// 确定性来源：由上游 handler 在**第一次调用的响应写出前**取消 ctx。于是「取消发生在
// 第 1 次调用之内」是事实而不是时序赌注：随后的内联重试与后续轮次都必然看到已取消的
// ctx，循环同步返回。因此 calls 必须**恰好 1**。
func TestCheckinRetryCancelledUpstreamStopsImmediately(t *testing.T) {
	withRetryDelays(t, time.Millisecond, time.Millisecond, time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &detCheckinUpstream{failFirst: 1 << 30, onCheckin: cancel}
	s, p := detCheckinEnv(t, f)

	s.checkinRetryLoop(ctx, []string{"u1"}) // 同步返回

	if got := f.calls.Load(); got != 1 {
		t.Fatalf("取消后仍打了 %d 次上游 want 1（内联重试/后续轮次没看 ctx）", got)
	}
	if p.CheckinDoneToday("u1") {
		t.Fatal("取消不该把账号记为已签到")
	}
}

// TestCheckinRetrySucceedsOnLaterRound 瞬时失败在后续轮次补签成功并落 checkin_done。
//
// 不变量：补签成功后 ① 账号被标记为今天已签（否则 21:00 会再签一次）
// ② 队列整批出队 ③ 循环自然结束（同步返回）。
//
// 计数不写死上限：内联重试阶梯由 upstream 包决定（首次 + 2 次内联），这里只断言
// 「跨过了一整轮失败之后才成功」，避免把别人的重试次数焊进本用例。
func TestCheckinRetrySucceedsOnLaterRound(t *testing.T) {
	withRetryDelays(t, time.Millisecond, time.Millisecond, time.Millisecond)
	f := &detCheckinUpstream{failFirst: 3} // 第一轮（含内联重试）全失败
	s, p := detCheckinEnv(t, f)

	s.checkinRetryLoop(context.Background(), []string{"u1"})

	if !p.CheckinDoneToday("u1") {
		t.Fatalf("补签没有落 checkin_done（调用 %d 次）", f.calls.Load())
	}
	if got := f.calls.Load(); got < 4 {
		t.Fatalf("调用 %d 次：应至少是一整轮失败（3 次）后第 4 次成功", got)
	}
	s.retryMu.Lock()
	pending := len(s.retryPending)
	s.retryMu.Unlock()
	if pending != 0 {
		t.Fatalf("成功后队列仍有 %d 个账号", pending)
	}
}

// TestTaskSweepPanicReleasesTaskLockDeterministic 首轮 panic 后：循环存活 + 族锁已释放。
//
// 判据是**事件**而不是等待：执行体第二次被进入，只能发生在 RunTaskSweepNow 又一次
// 成功 TryBeginTask(TaskSweep) 之后（拿不到锁就直接 return，根本不会调执行体）。
// 既有用例是「cancel 后每 2ms 抢一次锁，2s 超时」——慢机上抢不到就误红，而这里
// 10s 只是「事件始终没发生」的兜底上限。
func TestTaskSweepPanicReleasesTaskLockDeterministic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var runs atomic.Int32
	second := make(chan struct{}, 1)
	s := New(Config{SweepRunner: func() (int, string) {
		if runs.Add(1) == 1 {
			panic("boom") // 上游形状异常把执行体打崩
		}
		select {
		case second <- struct{}{}:
		default:
		}
		return 0, "ok"
	}})
	s.StartTaskSweep(ctx, 5*time.Millisecond)

	select {
	case <-second:
	case <-time.After(10 * time.Second):
		t.Fatalf("首轮 panic 后清扫没有恢复（runs=%d）：要么循环被 panic 打死，"+
			"要么 TaskSweep 族锁没释放（面板「立即补领」会恒 409）", runs.Load())
	}
	cancel()
}

// TestTaskSweepPauseBlocksOnRearmNotTimer 暂停（interval=0）后循环必须停在唤醒通道上，
// 而不是继续按旧间隔跑。
//
// 确定性来源：执行体被 gate 卡住 → 暂停的 poke 一定落在缓冲区里 → 放行后循环回到顶部
// 读到 cur=0、消费 poke、然后停在 rearm 上。此时**没有武装的 timer**，任何一轮都不可能
// 再发生；两次「等 poke 被消费」（正等待，上限 10s）就是它确实停在那里的证据。
//
// 若 cur<=0 的暂停分支失效（例如改成 cur<0），循环会以 ~0 延迟继续调用执行体，
// runs 立刻变成 2 → 变红。这个方向的误红不存在：正确实现下 runs 永不增加，
// 而失效实现的延迟是 0ms 量级，不需要任何「睡够多久」的假设。
func TestTaskSweepPauseBlocksOnRearmNotTimer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var runs atomic.Int32
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	s := New(Config{SweepRunner: func() (int, string) {
		runs.Add(1)
		select {
		case entered <- struct{}{}:
		default:
		}
		<-gate // 卡住，让「暂停」发生在执行体之外
		return 0, "ok"
	}})
	// 清理：放行可能仍卡在 gate 上的执行体（暂停失效时会出现），避免 goroutine 泄漏。
	defer func() {
		for i := 0; i < 4; i++ {
			select {
			case gate <- struct{}{}:
			default:
			}
		}
	}()

	s.StartTaskSweep(ctx, 5*time.Millisecond)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("清扫循环 10s 内一轮都没跑")
	}

	s.SetTaskSweepInterval(0) // 暂停：此刻循环卡在 gate 上，poke 留在缓冲区
	gate <- struct{}{}        // 放行第一轮

	if !drainPoke(t, s, 10*time.Second) {
		t.Fatalf("暂停后循环没有消费唤醒信号（runs=%d）：它没停在 rearm 分支上", runs.Load())
	}
	// 再唤醒一次并等它被消费：循环完成了一整轮「读到 cur=0 → 回到 rearm」。
	s.SetTaskSweepInterval(0)
	if !drainPoke(t, s, 10*time.Second) {
		t.Fatalf("第二次唤醒未被消费（runs=%d）", runs.Load())
	}
	if got := runs.Load(); got != 1 {
		t.Fatalf("暂停后仍跑了 %d 轮 want 1：interval=0 的暂停分支没生效", got)
	}
}

// drainPoke 等到 rearmSweep 的待处理信号被消费（正等待：等一个必然发生的事件）。
func drainPoke(t *testing.T, s *Scheduler, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if len(s.rearmSweep) == 0 {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}
