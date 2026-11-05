package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// pollModeFake 按阶段切换 /v2/videos/tasks 的回执：先 running，再（可选）限流 / 出片。
type pollModeFake struct {
	mu   sync.Mutex
	mode string // "running" | "done" | "limit"
}

func (p *pollModeFake) set(mode string) {
	p.mu.Lock()
	p.mode = mode
	p.mu.Unlock()
}

func (p *pollModeFake) get() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.mode
}

func newPollModeFake(mode string) (*pollModeFake, *videoFake) {
	m := &pollModeFake{mode: mode}
	f := &videoFake{on: func(c videoCall) (int, string) {
		switch c.Path {
		case "/v2/videos/generations":
			return 200, videoSubmitOK
		case "/v2/videos/tasks":
			switch m.get() {
			case "limit":
				return http.StatusTooManyRequests,
					`{"code":14003,"msg":"Too many requests. Please retry after a while"}`
			case "done":
				return 200, videoTaskDone
			default:
				return 200, videoTaskRunning
			}
		}
		return 404, `{"code":404,"msg":"unexpected"}`
	}}
	return m, f
}

func submitAsync(t *testing.T, h *Handler) {
	t.Helper()
	rec := postVideo(t, h, `{"prompt":"一只猫在草地上奔跑","seconds":4,"wait":false}`)
	if rec.Code != 200 {
		t.Fatalf("提交失败 code=%d body=%s", rec.Code, rec.Body)
	}
}

// ageQuery 把「上次真打上游」的时刻推回过去，绕过最小间隔护栏（测别的路径用）。
func ageQuery(h *Handler, id string, d time.Duration) {
	h.videoLedger.mu.Lock()
	defer h.videoLedger.mu.Unlock()
	if o, ok := h.videoLedger.m[id]; ok {
		o.LastQueryAt = time.Now().Add(-d)
	}
}

// TestVideoPollSingleFlight 并发查询同一个 task_id 只打一次上游：上游对同一账号的密集
// 查询会回 14003（Too many requests），MCP 客户端恰恰爱并发轮询。
func TestVideoPollSingleFlight(t *testing.T) {
	_, f := newPollModeFake("running")
	inner := f.on
	f.on = func(c videoCall) (int, string) {
		if c.Path == "/v2/videos/tasks" {
			time.Sleep(60 * time.Millisecond) // 让几个查询真的重叠
		}
		return inner(c)
	}
	h := NewHandler(Config{Pool: imagePool(), Upstream: f.client()})
	submitAsync(t, h)

	const n = 4
	var wg sync.WaitGroup
	codes := make([]int, n)
	bodies := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/videos/"+videoTestTaskID, nil))
			codes[i], bodies[i] = rec.Code, rec.Body.String()
		}(i)
	}
	wg.Wait()
	for i := range codes {
		if codes[i] != 200 {
			t.Fatalf("并发查询 #%d code=%d body=%s", i, codes[i], bodies[i])
		}
	}
	if got := f.count("/v2/videos/tasks"); got != 1 {
		t.Fatalf("并发查询同一任务只该打一次上游，实际 %d 次", got)
	}
}

// TestVideoPollThrottle 同一个任务 3 秒内只问一次上游：其余请求直接回台账快照，
// 免得客户端自己把账号打进 14003。
func TestVideoPollThrottle(t *testing.T) {
	_, f := newPollModeFake("running")
	h := NewHandler(Config{Pool: imagePool(), Upstream: f.client()})
	submitAsync(t, h)

	first := getVideo(t, h, videoTestTaskID)
	if first.Code != 200 {
		t.Fatalf("首次查询 code=%d body=%s", first.Code, first.Body)
	}
	if n := f.count("/v2/videos/tasks"); n != 1 {
		t.Fatalf("首次查询应打一次上游，实际 %d 次", n)
	}
	second := getVideo(t, h, videoTestTaskID)
	if second.Code != 200 {
		t.Fatalf("护栏内查询 code=%d body=%s", second.Code, second.Body)
	}
	if n := f.count("/v2/videos/tasks"); n != 1 {
		t.Fatalf("最小间隔内的第二次查询不该再打上游，实际 %d 次", n)
	}

	ageQuery(h, videoTestTaskID, 2*videoTaskMinPoll)
	if rec := getVideo(t, h, videoTestTaskID); rec.Code != 200 {
		t.Fatalf("过了护栏 code=%d body=%s", rec.Code, rec.Body)
	}
	if n := f.count("/v2/videos/tasks"); n != 2 {
		t.Fatalf("过了最小间隔应再打一次上游，实际 %d 次", n)
	}
}

// TestVideoCompletedServedFromLedger 出片后重复查询走台账快照：不再打上游，但回执照样带
// usage/size/seconds（上游 usage 只在第一次回），且不重复记账。
func TestVideoCompletedServedFromLedger(t *testing.T) {
	m, f := newPollModeFake("done")
	h := NewHandler(Config{Pool: imagePool(), Upstream: f.client()})
	submitAsync(t, h)

	first := getVideo(t, h, videoTestTaskID)
	if first.Code != 200 {
		t.Fatalf("首次查询 code=%d body=%s", first.Code, first.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(first.Body.Bytes(), &got); err != nil {
		t.Fatalf("回程不是 JSON：%s", first.Body)
	}
	if got["status"] != "completed" {
		t.Fatalf("status=%v want completed", got["status"])
	}
	usage, _ := got["usage"].(map[string]any)
	if usage["credit"] != 57.14 {
		t.Fatalf("首次查询应带 credit 57.14：%v", got)
	}

	m.set("done")
	second := getVideo(t, h, videoTestTaskID)
	if second.Code != 200 {
		t.Fatalf("复查 code=%d body=%s", second.Code, second.Body)
	}
	var got2 map[string]any
	if err := json.Unmarshal(second.Body.Bytes(), &got2); err != nil {
		t.Fatalf("复查回程不是 JSON：%s", second.Body)
	}
	usage2, _ := got2["usage"].(map[string]any)
	if usage2["credit"] != 57.14 {
		t.Fatalf("复查也要带 credit（走台账快照）：%v", got2)
	}
	if got2["size"] != "768P" {
		t.Fatalf("复查 size 应来自台账：%v", got2)
	}
	if n := f.count("/v2/videos/tasks"); n != 1 {
		t.Fatalf("出片后复查不该再打上游，实际 %d 次", n)
	}
	st, _ := h.cfg.Pool.Status("c1")
	if len(st.ModelCosts) != 1 || st.ModelCosts[0].Samples != 1 {
		t.Fatalf("只该记一次账：%+v", st.ModelCosts)
	}
}

// TestVideoRateLimitedServedFromCache 撞上上游限流：有快照就顶上（客户端至少拿到上一次
// 状态），同时记一个冷却窗口，后续查询不再碰上游。
func TestVideoRateLimitedServedFromCache(t *testing.T) {
	m, f := newPollModeFake("running")
	h := NewHandler(Config{Pool: imagePool(), Upstream: f.client()})
	submitAsync(t, h)

	if rec := getVideo(t, h, videoTestTaskID); rec.Code != 200 {
		t.Fatalf("先查一次（running）code=%d body=%s", rec.Code, rec.Body)
	}
	m.set("limit")
	ageQuery(h, videoTestTaskID, 2*videoTaskMinPoll) // 绕开最小间隔，真去打一次被限流
	rec := getVideo(t, h, videoTestTaskID)
	if rec.Code != 200 {
		t.Fatalf("限流时应回缓存 200，实际 code=%d body=%s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("回程不是 JSON：%s", rec.Body)
	}
	if got["status"] != "running" {
		t.Fatalf("缓存状态应是 running：%v", got)
	}
	calls := f.count("/v2/videos/tasks")
	// 冷却窗口内的再次查询：直接回快照，不再打上游（否则等于自己揍自己）。
	if rec := getVideo(t, h, videoTestTaskID); rec.Code != 200 {
		t.Fatalf("冷却窗口内查询 code=%d body=%s", rec.Code, rec.Body)
	}
	if n := f.count("/v2/videos/tasks"); n != calls {
		t.Fatalf("冷却窗口内不该再打上游：%d → %d", calls, n)
	}
}

// TestVideoRateLimitUnknownTask 台账里没有的任务（不是本网关提交的 / 重启后第一次查）撞上
// 限流时，没有快照可顶，只能把 429 + 提示如实回给客户端。
func TestVideoRateLimitUnknownTask(t *testing.T) {
	_, f := newPollModeFake("limit")
	h := NewHandler(Config{Pool: imagePool(), Upstream: f.client()})

	rec := getVideo(t, h, "1312831544-WandVideo-not-ours")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("无快照时限流应回 429，实际 code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "gateway_hint") {
		t.Fatalf("429 应带 gateway_hint：%s", rec.Body)
	}
}
