// logexport_test.go 导出端点的端到端断言（HTTP → 落盘文件 → ZIP）。
//
// 只测 LogStore.Export 不够：真正决定「能不能把日志发我」的是这条 HTTP 路径——
// 落盘未启用时的提示、空区间的 404、区间上限钳制、文件名与 Content-Type。
package panel

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/cst"
)

// newExportPanel 造一个挂了落盘存储的面板（只测日志导出，不需要 upstream）。
func newExportPanel(t *testing.T) (*Panel, *LogStore) {
	t.Helper()
	p := New(Config{Version: "test", AuthDir: t.TempDir()})
	store := NewLogStore(t.TempDir(), 7, 0)
	if store == nil {
		t.Fatal("NewLogStore 返回 nil")
	}
	t.Cleanup(func() { store.Close() })
	if err := p.Logs().SetLogStore(store); err != nil {
		t.Fatalf("SetLogStore: %v", err)
	}
	return p, store
}

// TestLogsExportDisabled 落盘未启用时回 501 且说清原因。
//
// 用 501 而不是 404：功能存在、只是没开（与 embeddings 关闭时的口径一致），
// 404 会让人以为端点根本不存在。
func TestLogsExportDisabled(t *testing.T) {
	p := New(Config{Version: "test", AuthDir: t.TempDir()})
	rec := httptest.NewRecorder()
	p.logsExportHandler(rec, httptest.NewRequest(http.MethodGet, "/panel/api/logs/export?days=3", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d want 501", rec.Code)
	}
}

// TestLogsExportServesZip 正常路径：回 200 + application/zip + 附件文件名。
func TestLogsExportServesZip(t *testing.T) {
	p, store := newExportPanel(t)
	store.Append(LogEntry{TS: cst.Now().Add(-time.Minute), Ch: ChSys, Text: "export me"})
	store.Flush()

	rec := httptest.NewRecorder()
	p.logsExportHandler(rec, httptest.NewRequest(http.MethodGet, "/panel/api/logs/export?days=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d want 200（body=%s）", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
		t.Fatalf("Content-Type = %q want application/zip", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd == "" || cd[:10] != "attachment" {
		t.Fatalf("Content-Disposition = %q（浏览器要靠它下载而不是内联渲染）", cd)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("响应体为空")
	}
}

// TestLogsExportEmptyRange404 区间内没有落盘文件时回 404 而不是空 zip。
//
// 空 zip 会被当成「导出成功但没内容」，人去翻配置；404 + 文案能直接指向原因。
func TestLogsExportEmptyRange404(t *testing.T) {
	p, _ := newExportPanel(t)
	// 未来区间：一定没有文件。
	rec := httptest.NewRecorder()
	// RFC3339 的时区偏移里带 "+"，裸拼进 query 会被解析成空格（服务端随即报 400），
	// 必须编码——这也是给 curl 用户的提醒：from/to 记得 urlencode。
	req := httptest.NewRequest(http.MethodGet,
		"/panel/api/logs/export?from="+url.QueryEscape(time.Now().Add(24*time.Hour).Format(time.RFC3339))+
			"&to="+url.QueryEscape(time.Now().Add(48*time.Hour).Format(time.RFC3339)), nil)
	p.logsExportHandler(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d want 404（body=%s）", rec.Code, rec.Body.String())
	}
}

// TestLogsExportClampsToRetention days 超过保留期时按保留期封顶。
//
// 反向代价：不封顶等于用一个查询参数绕过保留策略——运维以为删干净了，
// 实际上还能导出来。
func TestLogsExportClampsToRetention(t *testing.T) {
	p := New(Config{Version: "test", AuthDir: t.TempDir()})
	store := NewLogStore(t.TempDir(), 3, 0) // 保留 3 天
	if store == nil {
		t.Fatal("NewLogStore 返回 nil")
	}
	t.Cleanup(func() { store.Close() })
	if err := p.Logs().SetLogStore(store); err != nil {
		t.Fatal(err)
	}
	store.Append(LogEntry{TS: cst.Now().Add(-time.Minute), Ch: ChSys, Text: "today"})
	store.Flush()

	// days=999 仍应成功（只是被钳到 3 天），不该因为参数越界直接报错。
	rec := httptest.NewRecorder()
	p.logsExportHandler(rec, httptest.NewRequest(http.MethodGet, "/panel/api/logs/export?days=999", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d want 200（days 应被钳制而不是拒绝）", rec.Code)
	}
}

// TestLogsExportRejectsBadRange 非法参数要明确报 400。
func TestLogsExportRejectsBadRange(t *testing.T) {
	p, _ := newExportPanel(t)
	cases := []string{
		"/panel/api/logs/export?days=0",
		"/panel/api/logs/export?days=abc",
		"/panel/api/logs/export?from=2026-11-09",
		"/panel/api/logs/export?from=2026-11-09T00:00:00%2B08:00&to=2026-11-08T00:00:00%2B08:00",
	}
	for _, target := range cases {
		rec := httptest.NewRecorder()
		p.logsExportHandler(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s → status %d want 400", target, rec.Code)
		}
	}
}
