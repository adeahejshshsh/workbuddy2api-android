package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeTasks struct{ block chan struct{} }

// fakeTasks 只实现本仓调度器实际有的六个入口（上游的 growth 依赖任务中心，
// 尚未移植，见 admin_tasks.go 文件头）。
func (f *fakeTasks) RunCheckinNow()        { <-f.block }
func (f *fakeTasks) RunActivityNow()       {}
func (f *fakeTasks) RunKeepaliveNow()      {}
func (f *fakeTasks) RunTravelNow()         {}
func (f *fakeTasks) RunBalanceRefreshNow() {}
func (f *fakeTasks) RunStreakBonusNow()    {}

func TestAdminTaskRunOnce(t *testing.T) {
	dir := t.TempDir()
	audit, err := NewAuditLog(filepath.Join(dir, "audit.log"), "k")
	if err != nil {
		t.Fatal(err)
	}
	block := make(chan struct{})
	h := NewHandler(Config{
		AdminEnabled: true,
		APIKey:       "k",
		Tasks:        &fakeTasks{block: block},
		Audit:        audit,
	})
	req := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/admin/tasks/checkin/run", nil)
		r.Header.Set("Authorization", "Bearer k")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if req().Code != http.StatusAccepted {
		t.Fatal("first run should be 202")
	}
	if req().Code != http.StatusConflict {
		t.Fatal("second run should be 409 while the first is blocked")
	}
	close(block)
	unknown := httptest.NewRequest(http.MethodPost, "/admin/tasks/school/run", nil)
	unknown.Header.Set("Authorization", "Bearer k")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, unknown)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown task %d", w.Code)
	}
	raw, err := os.ReadFile(audit.Path())
	if err != nil || !strings.Contains(string(raw), `"action":"task.run"`) {
		t.Fatalf("audit missing task.run: %v %s", err, raw)
	}
}

func TestMetricsOffIs404(t *testing.T) {
	h := NewHandler(Config{})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("metrics off got %d", w.Code)
	}
}

// growth 的宿主是面板而不是调度器（调度器只有排程时机），所以它经 Config.GrowthRunner
// 单独注入：没接面板时不出现（否则运维点一个恒空转的入口），接了才在已知名单里。
func TestAdminTaskGrowthRunner(t *testing.T) {
	dir := t.TempDir()
	audit, err := NewAuditLog(filepath.Join(dir, "audit.log"), "k")
	if err != nil {
		t.Fatal(err)
	}
	var ran int
	h := NewHandler(Config{
		AdminEnabled: true,
		APIKey:       "k",
		Tasks:        &fakeTasks{block: make(chan struct{})},
		GrowthRunner: func() { ran++ },
		Audit:        audit,
	})
	req := httptest.NewRequest(http.MethodPost, "/admin/tasks/growth/run", nil)
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	// 后台 goroutine：给它一点时间落地（Accepted 是立刻返回的语义）。
	for i := 0; i < 100 && ran == 0; i++ {
		time.Sleep(time.Millisecond)
	}
	if ran != 1 {
		t.Fatalf("growth 执行体应被调用一次，实际 %d", ran)
	}

	// 未注入执行体：growth 不在可跑名单里，但仍在「已知」名单里（报错文案要能列出来）。
	h2 := NewHandler(Config{AdminEnabled: true, APIKey: "k", Tasks: &fakeTasks{block: make(chan struct{})}})
	req2 := httptest.NewRequest(http.MethodPost, "/admin/tasks/growth/run", nil)
	req2.Header.Set("Authorization", "Bearer k")
	rec2 := httptest.NewRecorder()
	h2.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("未注入时应 404，实际 code=%d body=%s", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), "growth") {
		t.Fatalf("已知任务名单应包含 growth：%s", rec2.Body.String())
	}
}
