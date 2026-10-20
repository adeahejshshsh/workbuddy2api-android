package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// bug P2-2 回归：/healthz 是「可服务」语义（池里没可用账号就 503），拿它做容器
// 健康检查会让"刚起来还没加账号"的容器永远 unhealthy，下游 depends_on:
// service_healthy 永久卡住。/livez 必须是恒 200 的存活探针。
func TestLivezAlwaysOKWithEmptyPool(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/livez", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200（空池也应存活）", rec.Code)
	}
	if got := rec.Header().Get("X-Service"); got != ServiceName {
		t.Errorf("X-Service=%q want %q", got, ServiceName)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("livez not json: %v body=%s", err, rec.Body)
	}
	if resp["ok"] != true || resp["service"] != ServiceName {
		t.Errorf("livez json=%v", resp)
	}
}

// 对照：/healthz 在空池仍应是 503（语义不能被 /livez 带偏）。
func TestHealthzStill503WhenEmpty(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503", rec.Code)
	}
}
