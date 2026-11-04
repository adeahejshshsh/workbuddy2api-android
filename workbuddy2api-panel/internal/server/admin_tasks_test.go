package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
