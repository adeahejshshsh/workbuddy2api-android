package panel

// 「清空归档」端点（issue #160）：删掉磁盘上的事件文件后，读取接口必须返回空列表。
// 这条用例的价值在**端到端**：只测 reqlog.Purge 不覆盖「面板路由 → 归档写循环 → 读回」这条链。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

func TestRequestLogsClearEndpoint(t *testing.T) {
	rl := reqlog.New(reqlog.Config{Enabled: true, Dir: t.TempDir(), RetentionDays: 7})
	rl.Record(reqlog.Event{Time: time.Now().Add(-time.Minute), RequestID: "a", Status: 200, OK: true})
	p := New(Config{Version: "test", APIKey: "k", RequestLog: rl})

	req := httptest.NewRequest("POST", "/panel/api/request_logs/clear", nil)
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("清空应 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"files":1`) {
		t.Fatalf("应报删除文件数, got %s", rec.Body.String())
	}

	req = httptest.NewRequest("GET", "/panel/api/request_logs?limit=10", nil)
	req.Header.Set("Authorization", "Bearer k")
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("读取应 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"entries":[]`) {
		t.Fatalf("清空后应读到空列表, got %s", rec.Body.String())
	}
}
