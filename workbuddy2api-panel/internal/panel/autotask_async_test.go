package panel

// 一键完成的异步形态（`?async=1`）。
//
// 这条路的必要性来自一个真实故障：手机端点「一键完成」后，同步请求把客户端
// 那条连接占住几十秒到十几分钟，客户端其余请求排在同一条通道上，于是整屏
// 一个数据都刷新不出来。异步形态的契约就三条，测试也只钉这三条：
//   1. 立刻 202 + 作业 ID，不等流水线；
//   2. 期间账号锁仍被占（重复点击 409，不会跑两轮）；
//   3. 流水线结束（含 panic）一定落作业终态并放锁 —— 否则账号被永久卡死。

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

func asyncAutoPanel(t *testing.T) *Panel {
	t.Helper()
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p := New(Config{Version: "test", APIKey: "k", Pool: pl})
	if err := p.StartTaskJobs(filepath.Join(t.TempDir(), "task-jobs.json")); err != nil {
		t.Fatalf("StartTaskJobs: %v", err)
	}
	t.Cleanup(p.StopTaskJobs)
	return p
}

func postAutoAll(p *Panel, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// waitJobFinished 等作业离开 running；超时直接失败（放锁/落终态是硬契约）。
func waitJobFinished(t *testing.T, p *Panel, uid string) TaskJob {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if j, ok := p.taskJobs.get(uid); ok && j.Status != "running" {
			return j
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("作业 20s 内没有终态（流水线卡住或没落终态）")
	return TaskJob{}
}

func TestAutoAllAsyncAcceptsImmediately(t *testing.T) {
	p := asyncAutoPanel(t)
	rec := postAutoAll(p, "/panel/api/accounts/u1/tasks/auto_all?async=1")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("异步形态应 202, got %d (%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"async":true`) || !strings.Contains(body, `"job_id":"`) {
		t.Fatalf("响应应带 async 与 job_id, got %s", body)
	}
	j, ok := p.taskJobs.get("u1")
	if !ok {
		t.Fatal("应立即有作业记录（前端靠它轮询进度）")
	}
	if j.Status != "running" || j.Kind != "growth" || j.UID != "u1" {
		t.Fatalf("作业初始态应为 running/growth/u1, got %+v", j)
	}
	// 期间账号锁必须被占：否则重复点击会跑两轮。
	if rec2 := postAutoAll(p, "/panel/api/accounts/u1/tasks/auto_all?async=1"); rec2.Code != http.StatusConflict {
		t.Fatalf("流水线在执行中，第二次点击应 409, got %d", rec2.Code)
	}
	j = waitJobFinished(t, p, "u1")
	// 测试里没有上游（Upstream 为 nil），流水线必然以内部错误收场 —— 重点是
	// 它落了终态并放了锁，而不是把账号挂死。
	if j.Status != "error" {
		t.Fatalf("无上游时应落 error, got %q（%s）", j.Status, j.Error)
	}
	if j.FinishedAt.IsZero() {
		t.Fatal("终态必须带 finished_at")
	}
	if !p.tryLockAccount("u1") {
		t.Fatal("流水线结束后账号锁应已释放")
	}
	p.unlockAccount("u1")
}

// 同步形态保持原契约（网页面板在用）：不带 async 参数时不能让客户端拿到 202。
func TestAutoAllSyncKeepsContract(t *testing.T) {
	p := asyncAutoPanel(t)
	rec := postAutoAll(p, "/panel/api/accounts/u1/tasks/auto_all")
	if rec.Code == http.StatusAccepted {
		t.Fatalf("同步形态不应 202, got %d", rec.Code)
	}
	// 同步形态不写作业记录（那是异步形态的增量），只钉两件事：不是 202，
	// 以及流水线结束后锁一定放掉（放锁在 goroutine 里，可能与 handler 返回稍有先后）。
	deadline := time.Now().Add(20 * time.Second)
	for {
		if p.tryLockAccount("u1") {
			p.unlockAccount("u1")
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("同步形态结束后账号锁一直没释放")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 逐项回调：每产出结果就 emit 一次，异步形态靠它把进度写进作业记录。
func TestRunAutoAllWithEmitsEachItem(t *testing.T) {
	p := asyncAutoPanel(t)
	// 企业上下文走最早的那条短路（不碰上游），正好可以验证 emit 与返回值同步。
	a := &auth.Auth{UID: "u1", AccessToken: "at", EnterpriseID: "e-1"}
	var emitted []string
	res := p.runAutoAllWith(a, func(it map[string]any) {
		code, _ := it["task_code"].(string)
		emitted = append(emitted, code)
	})
	if len(res) != len(emitted) || len(emitted) == 0 {
		t.Fatalf("emit 次数应与结果条数一致：res=%d emit=%d", len(res), len(emitted))
	}
	if emitted[0] != "(pipeline)" {
		t.Fatalf("企业上下文应先产出一条 (pipeline) 跳过项, got %v", emitted)
	}
}
