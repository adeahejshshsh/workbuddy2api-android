package panel

// 补领清扫（tasksweep.go）的判据与门控。
//
// 这套清扫是「不跑行为链路、只 accept + claim」的轻量兜底，最容易出错的地方不是
// HTTP，而是**选谁**：领了没达标的会一直 400、领了已领的会白打上游、漏了 mp 专属码
// 会让 Sequential 族奖励永远挂着。下面按条钉住。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// sweepFake 记录清扫期间的每一次上游调用：任务码 → 走的是哪条口径。
type sweepFake struct {
	mu       sync.Mutex
	accepted []string // accept 收到的任务码（顺序不重要）
	claimed  []string // claim 路径里的任务码，前缀标口径：web: / mp:
	lists    int      // 列表被请求的次数
}

func (f *sweepFake) snapshot() (accepted, claimed []string, lists int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.accepted...), append([]string(nil), f.claimed...), f.lists
}

// newSweepPanel 造一个只有 u1 的面板，上游用脚本化假服务：
//   - 默认列表（无 mp 头）：返回 tasks
//   - mp 列表（X-Client-Platform: miniprogram）：返回 mpTasks
//   - accept：记录码
//   - claim：记录「口径:码」
func newSweepPanel(t *testing.T, tasks, mpTasks string) (*Panel, *sweepFake) {
	t.Helper()
	fake := &sweepFake{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isMP := r.Header.Get("X-Client-Platform") == "miniprogram"
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/activity/growth/tasks":
			fake.mu.Lock()
			fake.lists++
			fake.mu.Unlock()
			if isMP {
				_, _ = io.WriteString(w, mpTasks)
			} else {
				_, _ = io.WriteString(w, tasks)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/v2/activity/growth/tasks/accept":
			var body struct {
				TaskCodes []string `json:"task_codes"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			fake.mu.Lock()
			fake.accepted = append(fake.accepted, body.TaskCodes...)
			fake.mu.Unlock()
			_, _ = io.WriteString(w, `{"code":0,"msg":"OK","data":{}}`)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/activity/growth/tasks/") &&
			strings.HasSuffix(r.URL.Path, "/claim"):
			// 两种领奖口径的**路径完全相同**（/activity/growth/tasks/{code}/claim），
			// 区别只在 base 与 mp 头：ClaimRewardMP=chat base + X-Client-Platform，
			// ClaimReward=web base 无该头。假服务同源部署，所以只能按头分流。
			code := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/activity/growth/tasks/"), "/claim")
			tag := "web:"
			if isMP {
				tag = "mp:"
			}
			fake.mu.Lock()
			fake.claimed = append(fake.claimed, tag+code)
			fake.mu.Unlock()
			_, _ = io.WriteString(w, `{"code":0,"msg":"OK","data":{"already_claimed":false,"credit":100}}`)
		default:
			t.Errorf("意外请求 %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	pn := &Panel{cfg: Config{
		Pool:     p,
		Upstream: &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL},
	}}
	return pn, fake
}

func authOf(t *testing.T, pn *Panel) *auth.Auth {
	t.Helper()
	a := pn.cfg.Pool.AuthByUID("u1")
	if a == nil {
		t.Fatal("测试前置：账号不在池里")
	}
	return a
}

// TestSweepAcceptsPendingOnly：accept 只挑「未领 / 未锁定 / 未达标 / 状态还不是
// accepted|completed」的码。
func TestSweepAcceptsPendingOnly(t *testing.T) {
	tasks := `{"code":0,"data":{"tasks":[
		{"task_code":"pending1","target":5,"current":1},
		{"task_code":"reached","target":5,"current":5},
		{"task_code":"locked","target":5,"current":1,"locked":true},
		{"task_code":"claimed","target":5,"current":5,"accept_status":"claimed","claimed":true},
		{"task_code":"already_accepted","target":5,"current":1,"accept_status":"accepted"}
	]}}`
	pn, fake := newSweepPanel(t, tasks, `{"code":0,"data":{"tasks":[]}}`)
	if _, _, err := pn.sweepAccount(authOf(t, pn)); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	accepted, _, _ := fake.snapshot()
	if len(accepted) != 1 || accepted[0] != "pending1" {
		t.Fatalf("accept=%v want [pending1]", accepted)
	}
}

// TestSweepClaimsClaimableOnly：claim 只挑 claimable 且未领未锁的；已领/未达标/
// 锁定一律不碰（领了会白打上游，且锁定码上游一定拒）。
func TestSweepClaimsClaimableOnly(t *testing.T) {
	tasks := `{"code":0,"data":{"tasks":[
		{"task_code":"ok","target":5,"current":5},
		{"task_code":"not_reached","target":5,"current":1},
		{"task_code":"locked","target":5,"current":5,"locked":true},
		{"task_code":"claimed","target":5,"current":5,"accept_status":"claimed"}
	]}}`
	// 已领的唯一判据是 accept_status=="claimed"：上游下发的裸 claimed 键不解析
	// （parseGrowthTasks 只认 accept_status），写它等于没写。
	pn, fake := newSweepPanel(t, tasks, `{"code":0,"data":{"tasks":[]}}`)
	_, claimed, err := pn.sweepAccount(authOf(t, pn))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	_, gotClaimed, _ := fake.snapshot()
	if claimed != 1 || len(gotClaimed) != 1 || gotClaimed[0] != "web:ok" {
		t.Fatalf("claimed=%d 请求=%v want 1 条 web:ok", claimed, gotClaimed)
	}
}

// TestSweepRoutesMPOnlyCodesToMPClaim：只在 mp 列表出现的码必须走 mp 领奖
// （Web 域对这些码 400，会变成每轮一条 WARN 且永远领不到）。
func TestSweepRoutesMPOnlyCodesToMPClaim(t *testing.T) {
	tasks := `{"code":0,"data":{"tasks":[{"task_code":"Model_chat","target":1,"current":1}]}}`
	mp := `{"code":0,"data":{"tasks":[
		{"task_code":"Model_chat","target":1,"current":1},
		{"task_code":"Sequential_Tasks_1","target":1,"current":1}
	]}}`
	pn, fake := newSweepPanel(t, tasks, mp)
	_, claimed, err := pn.sweepAccount(authOf(t, pn))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	_, gotClaimed, _ := fake.snapshot()
	if claimed != 2 {
		t.Fatalf("claimed=%d want 2（Model_chat + Sequential_Tasks_1）", claimed)
	}
	var web, mpCount int
	for _, c := range gotClaimed {
		switch {
		case c == "web:Model_chat":
			web++
		case c == "mp:Sequential_Tasks_1":
			mpCount++
		case strings.HasPrefix(c, "mp:Model_chat"):
			t.Fatalf("默认口径也有的码不该走 mp 领奖：%s", c)
		}
	}
	if web != 1 || mpCount != 1 {
		t.Fatalf("口径分配 web=%d mp=%d 请求=%v want 各 1", web, mpCount, gotClaimed)
	}
}

// TestSweepDedupesCaseInsensitively：同一任务码的两种大小写只领一次（报告 #11：
// 去重键大小写不一致会让同一奖励被重复发起领取）。
func TestSweepDedupesCaseInsensitively(t *testing.T) {
	tasks := `{"code":0,"data":{"tasks":[{"task_code":"Chat_5","target":1,"current":1}]}}`
	mp := `{"code":0,"data":{"tasks":[{"task_code":"chat_5","target":1,"current":1}]}}`
	pn, fake := newSweepPanel(t, tasks, mp)
	_, claimed, err := pn.sweepAccount(authOf(t, pn))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	_, gotClaimed, _ := fake.snapshot()
	if claimed != 1 || len(gotClaimed) != 1 {
		t.Fatalf("claimed=%d 请求=%v want 只领一次", claimed, gotClaimed)
	}
}

// TestRunTaskSweepSkipsBusyAccount：账号正在跑任务动作（锁被占）→ 本轮让位，
// 一个上游请求都不发。
func TestRunTaskSweepSkipsBusyAccount(t *testing.T) {
	pn, fake := newSweepPanel(t,
		`{"code":0,"data":{"tasks":[{"task_code":"ok","target":1,"current":1}]}}`,
		`{"code":0,"data":{"tasks":[]}}`)
	if !pn.tryLockAccount("u1") {
		t.Fatal("测试前置：应当能拿到锁")
	}
	defer pn.unlockAccount("u1")
	swept, msg := pn.RunTaskSweepNow()
	if swept != 0 {
		t.Fatalf("swept=%d want 0（锁被占）msg=%s", swept, msg)
	}
	if _, _, lists := fake.snapshot(); lists != 0 {
		t.Fatalf("让位时不该请求上游列表，实际 %d 次", lists)
	}
	if !strings.Contains(msg, "跳过 1") {
		t.Fatalf("摘要应体现跳过数：%s", msg)
	}
}

// TestRunTaskSweepSkipsGlobalAndDisabled：国际区 / 禁用号 / 企业版一个请求都不发。
func TestRunTaskSweepSkipsGlobalAndDisabled(t *testing.T) {
	fake := &sweepFake{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		fake.lists++
		fake.mu.Unlock()
		_, _ = io.WriteString(w, `{"code":0,"data":{"tasks":[]}}`)
	}))
	t.Cleanup(srv.Close)

	p := pool.New("")
	p.Add(&auth.Auth{UID: "cn1", AccessToken: "at", ExpiresAt: 9999999999})
	// global 号：Domain 指到 workbuddy.ai → Realm()==global。
	p.Add(&auth.Auth{UID: "gl1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "https://api.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "dis1", AccessToken: "at", ExpiresAt: 9999999999})
	p.Disable("dis1", "测试")
	pn := &Panel{cfg: Config{
		Pool:     p,
		Upstream: &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL},
	}}
	swept, msg := pn.RunTaskSweepNow()
	if swept != 1 {
		t.Fatalf("swept=%d want 1（只有 cn1）msg=%s", swept, msg)
	}
}

// TestTaskProgressReached：判据与上游同口径（进度满 / accept_status=completed /
// status=complete），且不看 Claimed —— accept 阶段恰恰要认「已达标未领取」。
func TestTaskProgressReached(t *testing.T) {
	cases := []struct {
		name string
		t    upstream.Task
		want bool
	}{
		{"进度满", upstream.Task{Target: 5, Current: 5}, true},
		{"进度未满", upstream.Task{Target: 5, Current: 4}, false},
		{"目标为 0（无进度概念）", upstream.Task{Target: 0, Current: 0}, false},
		{"accept_status=completed", upstream.Task{AcceptStatus: "completed"}, true},
		{"status=complete（大小写不敏感）", upstream.Task{Status: "Complete"}, true},
		{"已领但进度满仍算达标", upstream.Task{Target: 1, Current: 1, Claimed: true}, true},
	}
	for _, c := range cases {
		tt := c.t
		if got := taskProgressReached(&tt); got != c.want {
			t.Fatalf("%s: got %v want %v", c.name, got, c.want)
		}
	}
	if taskProgressReached(nil) {
		t.Fatal("nil 任务不该算达标")
	}
}
