package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 面板 GET 配置必须给敏感字段打码（审查 P0-1 次生项）：此前 getConfig 原样回显
// api_key / admin_api_key / upstash.token / alerting.secret / device_token / three_d_temp_tokens。
func TestGetConfigRedactsSecrets(t *testing.T) {
	p := New(Config{
		AdminAPIKey: "admin",
		APIKey:      "client",
		LoadConfig: func() (any, error) {
			return map[string]any{
				"api_key":           "sk-secret",
				"admin_api_key":     "admin-secret",
				"upstream":          map[string]any{"token": "up-token", "user_agent": "UA"},
				"alerting":          map[string]any{"secret": "hmac-secret", "webhook_url": "https://x/?token=1"},
				"features":          map[string]any{"three_d_temp_tokens": map[string]any{"u1": "ck_t_x"}},
				"device_token":      "dev-token",
				"soft_rate_seconds": 40,
			}, nil
		},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/panel/api/config", nil)
	req.Header.Set("Authorization", "Bearer admin")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("含密钥的响应必须 no-store，实际 %q", got)
	}
	body := rec.Body.String()
	for _, secret := range []string{"sk-secret", "admin-secret", "up-token", "hmac-secret", "dev-token", "ck_t_x"} {
		if strings.Contains(body, secret) {
			t.Errorf("响应里泄漏了 %q：%s", secret, body)
		}
	}
	if !strings.Contains(body, redactedValue) {
		t.Error("应出现掩码占位符")
	}
	// 非敏感字段必须原样可见（面板要能渲染表单）。
	if !strings.Contains(body, "UA") || !strings.Contains(body, "40") {
		t.Errorf("非敏感字段被误伤：%s", body)
	}
	var out struct {
		Config map[string]any `json:"config"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
}
