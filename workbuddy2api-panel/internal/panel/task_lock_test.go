package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 批量 accept 与手动领奖必须取 per-account 锁（报告 #9）：
// 不取锁时它们会与 auto/队列并发，读到别处留下的中止标记 → 整批 504。
func TestTaskAcceptAllTakesAccountLock(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	pn := &Panel{cfg: Config{Pool: p, Upstream: upstream.New()}}

	if !pn.tryLockAccount("u1") {
		t.Fatal("测试前置：应当能拿到锁")
	}
	defer pn.unlockAccount("u1")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/accounts/u1/tasks/accept_all", nil)
	req.SetPathValue("uid", "u1")
	pn.taskAcceptAll(rec, req)
	if rec.Code != 409 {
		t.Fatalf("账号锁被占用时应回 409，实际 code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestAccountTaskClaimTakesAccountLock(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	pn := &Panel{cfg: Config{Pool: p, Upstream: upstream.New()}}

	if !pn.tryLockAccount("u1") {
		t.Fatal("测试前置：应当能拿到锁")
	}
	defer pn.unlockAccount("u1")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/accounts/u1/tasks/claim",
		strings.NewReader(`{"task_code":"chat5"}`))
	req.SetPathValue("uid", "u1")
	pn.accountTaskClaim(rec, req)
	if rec.Code != 409 {
		t.Fatalf("账号锁被占用时应回 409，实际 code=%d body=%s", rec.Code, rec.Body)
	}
}

// 批量 accept 必须按 20 条分片（报告 #10）：上游对 task_codes 数组长度无公开上限，
// 整批下发时任务多会被整批拒收。这里用 45 个任务验证分 3 批、每批 ≤20。
func TestTaskAcceptAllChunksBy20(t *testing.T) {
	var mu sync.Mutex
	var batchSizes []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			// mp 口径（带 X-Client-Platform 头）返回空列表：本用例只验 CN 侧分片。
			// 注意 taskAcceptAll 会**再**跑一轮 mp 接受，两轮都返回 45 条会得到
			// 6 批 [20 20 5 20 20 5] —— 那是正确行为，不是重复提交。
			if r.Header.Get("X-Client-Platform") != "" {
				w.Write([]byte(`{"code":0,"data":{"tasks":[]}}`))
				return
			}
			// 45 个未接受的任务
			var sb strings.Builder
			sb.WriteString(`{"code":0,"data":{"tasks":[`)
			for i := 0; i < 45; i++ {
				if i > 0 {
					sb.WriteString(",")
				}
				fmt.Fprintf(&sb, `{"code":"task_%02d","status":"not_accepted","target":1,"current":0}`, i)
			}
			sb.WriteString(`]}}`)
			w.Write([]byte(sb.String()))
			return
		}
		var body struct {
			TaskCodes []string `json:"task_codes"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		batchSizes = append(batchSizes, len(body.TaskCodes))
		mu.Unlock()
		w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	pn := &Panel{cfg: Config{
		Pool:     p,
		Upstream: &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL},
	}}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/accounts/u1/tasks/accept_all", nil)
	req.SetPathValue("uid", "u1")
	pn.taskAcceptAll(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(batchSizes) != 3 {
		t.Fatalf("45 个任务应分 3 批，实际 %d 批: %v", len(batchSizes), batchSizes)
	}
	total := 0
	for _, n := range batchSizes {
		if n > 20 {
			t.Fatalf("单批超过 20 条: %v", batchSizes)
		}
		total += n
	}
	if total != 45 {
		t.Fatalf("分批后总数 = %d want 45", total)
	}
}
