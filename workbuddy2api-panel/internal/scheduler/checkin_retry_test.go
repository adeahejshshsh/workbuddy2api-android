package scheduler

import (
	"context"
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

// flakyCheckinUpstream 前 failFirst 次签到返回线上实测的失败形态
// （HTTP 500 + code 10001「签到失败，请稍后重试」），之后返回成功。
type flakyCheckinUpstream struct {
	failFirst  int32
	checkinNew atomic.Int32 // 总调用次数（含 DailyCheckin 的内联重试）
	activityOK atomic.Bool  // 活动状态查询是否可用（置 false 模拟 500）
}

func (f *flakyCheckinUpstream) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			n := f.checkinNew.Add(1)
			if n <= f.failFirst {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(`{"code":10001,"msg":"签到失败，请稍后重试","requestId":"test"}`))
				return
			}
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/checkin-activity-status"):
			// 返回「活动生效且今天未签」，让流程走真正的签到写调用。
			w.Write([]byte(`{"code":0,"data":{"active":true,"todayCheckedIn":false,"streakDays":1,"endTime":"2026-12-31"}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":1000,"CycleCapacityRemain":500,"CycleCapacityUsed":0}]}}}}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
}

func newCheckinTestScheduler(t *testing.T, f *flakyCheckinUpstream) (*Scheduler, *pool.Pool, *httptest.Server) {
	t.Helper()
	srv := f.server()
	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999, Nickname: "测试号"}
	p.Add(a)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, CheckinHours: []int{9, 21}, KeepaliveHours: []int{22}})
	return s, p, srv
}

// shrinkRetryDelays 把补签节奏压到毫秒级（生产 10m/40m）。
func shrinkRetryDelays(t *testing.T, ds ...time.Duration) {
	t.Helper()
	old := checkinRetryDelays
	checkinRetryDelays = ds
	t.Cleanup(func() { checkinRetryDelays = old })
}

// 瞬时失败（500 + 10001「请稍后重试」）→ 主批失败后自动补签，补签成功落 checkin_done。
//
// 线上现场：2026-11-11 09:00 两个 CN 账号各撞一次该形态，内联的 2s/4s 三次重试全败，
// 之后要等 21:00 才有下一次机会；手动在 09:20 重签立刻成功并 +100 积分——正是补签要覆盖的窗口。
func TestCheckinDeferredRetrySucceeds(t *testing.T) {
	shrinkRetryDelays(t, 30*time.Millisecond, 60*time.Millisecond)
	f := &flakyCheckinUpstream{failFirst: 3} // 主批 3 次调用全失败（首次 + 2 次内联重试）
	s, p, srv := newCheckinTestScheduler(t, f)
	defer srv.Close()

	s.RunCheckinNow()
	if p.CheckinDoneToday("u1") {
		t.Fatal("主批三次全败，此刻不该记为已签到（前提失败）")
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if p.CheckinDoneToday("u1") {
			if calls := f.checkinNew.Load(); calls < 4 {
				t.Fatalf("补签调用数=%d，期望主批 3 次 + 补签至少 1 次", calls)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("补签窗口内没有签成（调用 %d 次）", f.checkinNew.Load())
}

// 已经签成的账号不再补签（用户在手机上签了、或上一轮补签成功）。
func TestCheckinRetrySkipsAlreadyDone(t *testing.T) {
	shrinkRetryDelays(t, 20*time.Millisecond)
	f := &flakyCheckinUpstream{failFirst: 0}
	_, p, srv := newCheckinTestScheduler(t, f)
	defer srv.Close()
	p.NoteCheckinDone("u1")

	s := New(Config{Pool: p, Upstream: &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}})
	before := f.checkinNew.Load()
	s.checkinRetryLoop(context.Background(), []string{"u1"})
	time.Sleep(120 * time.Millisecond)
	if got := f.checkinNew.Load() - before; got != 0 {
		t.Fatalf("已签账号被补签打了 %d 次上游", got)
	}
}

// ctx 取消 → 补签 goroutine 立刻退出，不再打上游。
//
// 等待窗口刻意远大于取消期限（5s 对 1s）：否则「不可取消的 sleep」也能在 2s 后返回、
// 与 t.Fatal 的计时器赛跑，变异跑不出来。
func TestCheckinRetryStopsOnContextCancel(t *testing.T) {
	shrinkRetryDelays(t, 5*time.Second)
	f := &flakyCheckinUpstream{failFirst: 0}
	_, p, srv := newCheckinTestScheduler(t, f)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	s := New(Config{Pool: p, Upstream: &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}})
	done := make(chan struct{})
	go func() { s.checkinRetryLoop(ctx, []string{"u1"}); close(done) }()
	time.Sleep(20 * time.Millisecond) // 让 goroutine 进到等待里
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ctx 取消后补签没有退出（会拖住优雅停机）")
	}
	time.Sleep(100 * time.Millisecond)
	if f.checkinNew.Load() != 0 {
		t.Fatalf("取消后仍打了上游 %d 次", f.checkinNew.Load())
	}
}

// 非瞬时的业务拒绝不该进补签队列：重试不会有不同结果，只是每天多打几轮无效请求。
func TestCheckinNonTransientErrorNotQueued(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"500 稍后重试", &upstream.Error{Kind: upstream.ErrServer, Status: 500, Msg: `{"code":10001,"msg":"签到失败，请稍后重试"}`}, true},
		{"500 code 10000", &upstream.Error{Kind: upstream.ErrServer, Status: 500, Msg: "API request failed with status code: 500"}, true},
		{"429 请求处理中", &upstream.Error{Kind: upstream.ErrSoftRate, Status: 429, Msg: "请求处理中"}, true},
		{"400 参数错", &upstream.Error{Kind: upstream.ErrBadParams, Status: 400, Msg: `{"code":11101,"msg":"bad"}`}, false},
		{"400 企业号不支持", &upstream.Error{Kind: upstream.ErrBadParams, Status: 400, Msg: `{"code":10001,"msg":"企业账号不支持该操作"}`}, false},
	}
	for _, c := range cases {
		if got := upstream.IsTransientBilling(c.err); got != c.want {
			t.Errorf("%s: IsTransientBilling=%v want %v", c.name, got, c.want)
		}
	}
}

// 补签去重：手动签到与排程签到各自失败时会各排一次补签，同一个号不能被两条队列同时
// 追打（白撞上游风控）。
func TestQueueCheckinRetryDedupes(t *testing.T) {
	s := &Scheduler{}
	if got := s.queueCheckinRetry([]string{"u1", "u1", "u2"}); len(got) != 2 {
		t.Fatalf("同批内应去重成 2 个，实际 %v", got)
	}
	if got := s.queueCheckinRetry([]string{"u1", "u3"}); len(got) != 1 || got[0] != "u3" {
		t.Fatalf("已在队列里的 u1 不该重复排，实际 %v", got)
	}
	// 循环退出后整批出队：下一轮失败还能再排。
	s.retryMu.Lock()
	for uid := range map[string]bool{"u1": true, "u2": true, "u3": true} {
		delete(s.retryPending, uid)
	}
	s.retryMu.Unlock()
	if got := s.queueCheckinRetry([]string{"u1"}); len(got) != 1 {
		t.Fatalf("出队后应能重新排队，实际 %v", got)
	}
}
