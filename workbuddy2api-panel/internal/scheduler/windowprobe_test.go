// windowprobe_test.go 额度窗口探针的执行侧（借鉴 ardeyouxipianyi/workbuddy2api-hub 的
// wb_window_probe.py）。
//
// 钉住四条处置路径：探针成功 → 清冷却 + 删记录；429 给更晚的重置 → 换墙钟；
// 429 给同样/更早的时刻 → 计数 + 推后重试；持续未放开到上限 → 放弃；其它错误只试一次。
// 外加边界：账号已删 → 直接丢弃；停用/暂停/积分闸门拦下的号 → 不唤醒上游。
package scheduler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/cst"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// windowFake 一个按「第几次调用」决定响应的假上游。
type windowFake struct {
	calls int
	reply func(n int) (status int, body string)
}

func (f *windowFake) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls++
		status, body := f.reply(f.calls)
		if status == http.StatusOK {
			w.Header().Set("Content-Type", "text/event-stream")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

// windowEnv 一套最小装配：一个账号 + 假上游 + 已登记的到点窗口。
func windowEnv(t *testing.T, reply func(n int) (int, string)) (*pool.Pool, *Scheduler, *windowFake) {
	t.Helper()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "w1", AccessToken: "at-w1", ExpiresAt: 9999999999})
	f := &windowFake{reply: reply}
	srv := f.server(t)
	t.Cleanup(srv.Close)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return p, New(Config{Pool: p, Upstream: up}), f
}

// dueWindow 登记一个「已经到点」的窗口：登记入口只收未来的重置时刻（那是它的正确
// 语义——过去的重置不该登记），所以先把 ResetAt 设近，再用 now 往后推来模拟到点。
func dueWindow(t *testing.T, p *pool.Pool, model string) time.Time {
	t.Helper()
	now := time.Now()
	reset := now.Add(time.Minute)
	if !p.ScheduleModelWindow("w1", model, reset, true, now) {
		t.Fatal("登记窗口失败")
	}
	return reset
}

// TestWindowProbeSuccessOpensWindow 探针 2xx → 该 (账号,模型) 的 6004 冷却被清、
// 窗口记录被删。这是本机制的最终目的：窗口已经从重置那一刻开始计时，无需再等。
func TestWindowProbeSuccessOpensWindow(t *testing.T) {
	p, s, f := windowEnv(t, func(int) (int, string) {
		return http.StatusOK, "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\ndata: [DONE]\n\n"
	})
	reset := dueWindow(t, p, "glm-5.3")
	// 制造一条真实形态的 6004 冷却：Until 对齐上游重置墙钟。
	p.CooldownSoftForModel("w1", time.Minute, reset, "glm-5.3", "6004 rate limited")

	// 到点：查询时刻推过 reset。
	got := s.runWindowProbes(t.Context(), p.DueModelWindows(reset.Add(time.Second)))
	if got != 1 {
		t.Fatalf("应发出 1 次探针，got %d", got)
	}
	if f.calls != 1 {
		t.Fatalf("上游应被调用 1 次，got %d", f.calls)
	}
	if w := p.ModelWindows(); len(w) != 0 {
		t.Fatalf("成功后窗口记录应删除，got %+v", w)
	}
	// 冷却已清：限额台账里不该再有这个模型（RateLimitedModels 只列未到期的
	// 模型级冷却；它消失即「该模型在这个号上重新可选」）。
	st, ok := p.Status("w1")
	if !ok {
		t.Fatal("账号不应消失")
	}
	for _, m := range st.RateLimitedModels {
		if m.Model == "glm-5.3" {
			t.Fatalf("探针成功后限额台账不该还留着 glm-5.3: %+v", m)
		}
	}
}

// TestWindowProbe429WithLaterReset 上游又回 429 并给了更晚的重置 → 换成新墙钟、
// 计数归零（这是新一轮等待，不是「还没放开」）。
func TestWindowProbe429WithLaterReset(t *testing.T) {
	later := time.Now().Add(2 * time.Hour)
	body := fmt.Sprintf(`{"code":6004,"msg":"将在 %s UTC+8 重置"}`, later.In(cst.Loc).Format("2006-01-02 15:04:05"))
	p, s, f := windowEnv(t, func(int) (int, string) { return http.StatusTooManyRequests, body })
	reset := dueWindow(t, p, "glm-5.3")
	p.CooldownSoftForModel("w1", time.Minute, reset, "glm-5.3", "6004 rate limited")

	if got := s.runWindowProbes(t.Context(), p.DueModelWindows(reset.Add(time.Second))); got != 1 {
		t.Fatalf("应发出 1 次探针，got %d", got)
	}
	if f.calls != 1 {
		t.Fatalf("上游应被调用 1 次，got %d", f.calls)
	}
	w := p.ModelWindows()
	if len(w) != 1 {
		t.Fatalf("窗口应保留（还在等下一轮），got %+v", w)
	}
	if w[0].Attempts != 0 {
		t.Fatalf("新墙钟应把计数归零，got attempts=%d", w[0].Attempts)
	}
	if !w[0].ResetAt.After(time.Now().Add(time.Hour)) {
		t.Fatalf("ResetAt 应换成上游新给的时刻，got %v", w[0].ResetAt)
	}
}

// TestWindowProbe429NotYetOpen 429 给的时刻没有更晚（上游其实还没放开）→ 计数 +1，
// ResetAt 推到重试时刻，不删记录。
func TestWindowProbe429NotYetOpen(t *testing.T) {
	past := time.Now().Add(-time.Minute)
	body := fmt.Sprintf(`{"code":6004,"msg":"将在 %s UTC+8 重置"}`, past.In(cst.Loc).Format("2006-01-02 15:04:05"))
	p, s, f := windowEnv(t, func(int) (int, string) { return http.StatusTooManyRequests, body })
	reset := dueWindow(t, p, "glm-5.3")

	if got := s.runWindowProbes(t.Context(), p.DueModelWindows(reset.Add(time.Second))); got != 1 {
		t.Fatalf("应发出 1 次探针，got %d", got)
	}
	if f.calls != 1 {
		t.Fatalf("上游应被调用 1 次，got %d", f.calls)
	}
	w := p.ModelWindows()
	if len(w) != 1 {
		t.Fatalf("窗口应保留，got %+v", w)
	}
	if w[0].Attempts != 1 {
		t.Fatalf("attempts=%d want 1", w[0].Attempts)
	}
	if w[0].ResetAt.Before(time.Now().Add(windowProbeRetryDelay / 2)) {
		t.Fatalf("ResetAt 应推到重试时刻附近，got %v（离现在 %v）", w[0].ResetAt, time.Until(w[0].ResetAt))
	}
}

// TestWindowProbeGivesUp 持续「还没放开」到上限 → 删记录（下一次真实 429 会重新登记）。
func TestWindowProbeGivesUp(t *testing.T) {
	p, s, f := windowEnv(t, func(int) (int, string) {
		return http.StatusTooManyRequests, `{"code":6004,"msg":"使用量已超出频率限制"}`
	})
	reset := dueWindow(t, p, "glm-5.3")
	now := reset.Add(time.Second)
	for i := 0; i < windowProbeMaxAttempts; i++ {
		due := p.DueModelWindows(now)
		if len(due) == 0 {
			// 已经被推到重试时刻之后：手动把查询时刻推过去，模拟下一次巡检。
			now = now.Add(windowProbeRetryDelay + time.Second)
			due = p.DueModelWindows(now)
		}
		if len(due) == 0 {
			t.Fatalf("第 %d 轮拿不到到点窗口（记录被提前删了？）", i+1)
		}
		s.runWindowProbes(t.Context(), due)
		now = now.Add(windowProbeRetryDelay + time.Second)
	}
	if w := p.ModelWindows(); len(w) != 0 {
		t.Fatalf("连续 %d 次未重置应放弃，got %+v", windowProbeMaxAttempts, w)
	}
	if f.calls < windowProbeMaxAttempts {
		t.Fatalf("上游应被调用至少 %d 次，got %d", windowProbeMaxAttempts, f.calls)
	}
}

// TestWindowProbeDropsUnknownAccount 账号已不在池中 → 直接丢弃记录、不发请求。
// 否则这条记录会被每轮巡检取出来，白跑一遍还永远清不掉。
func TestWindowProbeDropsUnknownAccount(t *testing.T) {
	p, s, f := windowEnv(t, func(int) (int, string) { return http.StatusOK, "" })
	reset := dueWindow(t, p, "glm-5.3")
	p.Remove("w1")

	if got := s.runWindowProbes(t.Context(), p.DueModelWindows(reset.Add(time.Second))); got != 0 {
		t.Fatalf("账号不在池中时不该发探针，got %d", got)
	}
	if f.calls != 0 {
		t.Fatalf("上游不该被调用，got %d", f.calls)
	}
	if w := p.ModelWindows(); len(w) != 0 {
		t.Fatalf("记录应被丢弃，got %+v", w)
	}
}

// TestWindowProbeBlockedAccountsNotWoken 探针是真实计费的对话请求，与选号同一套闸门：
// 停用、暂停、触及保留积分、低于积分保底且模型收费的号都不该被探针唤醒，上游零调用；
// 记录删掉（真实请求照常会开窗），不留着每 15 秒空转。
func TestWindowProbeBlockedAccountsNotWoken(t *testing.T) {
	cases := map[string]func(p *pool.Pool){
		"disabled": func(p *pool.Pool) { p.Disable("w1", "手工停用") },
		"paused":   func(p *pool.Pool) { p.Pause("w1") },
		"reserve": func(p *pool.Pool) {
			p.SetReserveCredits(100)
			p.SetCredits("w1", 50, 1000)
		},
		"floor": func(p *pool.Pool) {
			p.SetCreditFloor(100)
			p.SetCredits("w1", 50, 1000)
			p.SetModelRateOf(func(_, _ string) string { return "1.00" })
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			p, s, f := windowEnv(t, func(int) (int, string) { return http.StatusOK, "" })
			reset := dueWindow(t, p, "glm-5.3")
			setup(p)

			if got := s.runWindowProbes(t.Context(), p.DueModelWindows(reset.Add(time.Second))); got != 0 {
				t.Fatalf("不该发探针，got %d", got)
			}
			if f.calls != 0 {
				t.Fatalf("上游不该被调用，got %d", f.calls)
			}
			if w := p.ModelWindows(); len(w) != 0 {
				t.Fatalf("记录应删除，got %+v", w)
			}
		})
	}
}

// TestWindowProbeOtherErrorsDoNotRetry 非限流的失败（5xx、200 流里的拦截帧）只试一次：
// 探针每次都是计费请求，不该为它按 15 秒一次连打 20 次。拦截帧还不能算「打通」而清掉冷却。
func TestWindowProbeOtherErrorsDoNotRetry(t *testing.T) {
	cases := map[string]func(int) (int, string){
		"5xx": func(int) (int, string) { return http.StatusInternalServerError, `{"code":500}` },
		"200-error-frame": func(int) (int, string) {
			return http.StatusOK, "data: {\"code\":11140,\"msg\":\"request illegal\"}\n\ndata: [DONE]\n\n"
		},
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			p, s, f := windowEnv(t, reply)
			reset := dueWindow(t, p, "glm-5.3")
			p.CooldownSoftForModel("w1", time.Minute, reset, "glm-5.3", "6004 rate limited")

			s.runWindowProbes(t.Context(), p.DueModelWindows(reset.Add(time.Second)))
			if f.calls != 1 {
				t.Fatalf("应只试一次，got %d", f.calls)
			}
			if w := p.ModelWindows(); len(w) != 0 {
				t.Fatalf("失败后应删记录，不再重试，got %+v", w)
			}
			if name == "200-error-frame" {
				st, _ := p.Status("w1")
				found := false
				for _, m := range st.RateLimitedModels {
					found = found || m.Model == "glm-5.3"
				}
				if !found {
					t.Fatal("错误帧不是打通，不该清掉 6004 冷却")
				}
			}
		})
	}
}

// TestWindowProbeLaterResetSurvivesLongBodyAndStreamFrame 重置时刻拿不到就会丢掉新一轮：
// 429 的报文把重置文案排在 200 字节之后（*upstream.Error.Msg 已被截断），或者上游把 6004
// 放进 HTTP 200 的错误帧里——两种形态都必须换成新墙钟，而不是计数重试后放弃。
func TestWindowProbeLaterResetSurvivesLongBodyAndStreamFrame(t *testing.T) {
	later := time.Now().Add(2 * time.Hour).In(cst.Loc).Format("2006-01-02 15:04:05")
	msg := fmt.Sprintf(`{"code":6004,"msg":"%s将在 %s UTC+8 重置"}`, strings.Repeat("x", 300), later)
	frame := fmt.Sprintf("data: {\"code\":6004,\"msg\":\"使用量已超出频率限制，将在 %s UTC+8 重置\"}\n\n", later)
	cases := map[string]func(int) (int, string){
		"429-long-body": func(int) (int, string) { return http.StatusTooManyRequests, msg },
		"200-frame":     func(int) (int, string) { return http.StatusOK, frame },
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			p, s, _ := windowEnv(t, reply)
			reset := dueWindow(t, p, "glm-5.3")
			s.runWindowProbes(t.Context(), p.DueModelWindows(reset.Add(time.Second)))
			w := p.ModelWindows()
			if len(w) != 1 {
				t.Fatalf("窗口应保留等下一轮，got %+v", w)
			}
			if w[0].Attempts != 0 || !w[0].ResetAt.After(time.Now().Add(time.Hour)) {
				t.Fatalf("应换成上游新给的重置时刻并清零计数，got %+v", w[0])
			}
		})
	}
}

// TestWindowProbeBodyMinimal 探针请求体必须是**最小**的：max_tokens 小、只要一条
// user 消息、stream=true（上游只认流式）。这条一旦漂移，探针会变成真实的额度消耗。
func TestWindowProbeBodyMinimal(t *testing.T) {
	raw, err := windowProbeBody("glm-5.3")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Model     string           `json:"model"`
		Stream    bool             `json:"stream"`
		MaxTokens int              `json:"max_tokens"`
		Messages  []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Model != "glm-5.3" || !got.Stream || got.MaxTokens != windowProbeMaxTokens {
		t.Fatalf("探针体形态漂移: %+v", got)
	}
	if len(got.Messages) != 1 || got.Messages[0]["role"] != "user" {
		t.Fatalf("探针应只有一条 user 消息: %+v", got.Messages)
	}
}
