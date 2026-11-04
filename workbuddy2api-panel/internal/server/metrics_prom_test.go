package server

// GET /metrics（Prometheus 文本 0.0.4）：默认不注册，开了才有；输出要能被解析。
// 这条用例同时钉住「默认关闭」—— 指标含池健康分布，不该默认对外暴露。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestMetricsDisabledByDefault(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, APIKey: "k"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("Authorization", "Bearer k")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未开启时 /metrics 应 404, got %d", rec.Code)
	}
}

func TestMetricsEnabledExposesPoolAndRequests(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, APIKey: "k", MetricsEnabled: true})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("Authorization", "Bearer k")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics 应 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// 池维度指标必须在（不依赖任何请求）
	for _, want := range []string{"wb2api_pool_accounts_total", "wb2api_pool_accounts{"} {
		if !strings.Contains(body, want) {
			t.Errorf("缺指标 %s\n%s", want, body)
		}
	}
	// 不得泄露 uid（指标里只能有 realm/state 这类标签）
	if strings.Contains(body, "u1") {
		t.Errorf("指标里出现了账号标识：\n%s", body)
	}
}
