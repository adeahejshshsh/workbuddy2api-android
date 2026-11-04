package server

// 运维面（/status、/admin/*）必须能与客户端面分离鉴权。
//
// 此前一把 api_key 同时打通 /v1/*（发给下游客户端与 Agent）、/status（回全池账号
// 清单）、/admin/*（能对所有账号触发上游动作）—— 任何一次客户端 key 泄漏都等于
// 全池清单 + 运维写操作一起泄漏。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func adminKeyHandler() *Handler {
	return NewHandler(Config{
		Pool:         testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		APIKey:       "client-key",
		AdminAPIKey:  "admin-key",
		AdminEnabled: true,
	})
}

// 配了 admin_api_key 后：客户端 key 打不开 /status 与 /admin，admin key 可以。
func TestAdminKeySeparatesSurfaces(t *testing.T) {
	h := adminKeyHandler()

	cases := []struct {
		name string
		path string
	}{
		{"status", "/status"},
		{"admin task", "/admin/tasks/checkin/run"},
	}
	for _, c := range cases {
		t.Run(c.name+" 客户端 key 应被拒", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader("{}"))
			if c.path == "/status" {
				req = httptest.NewRequest(http.MethodGet, c.path, nil)
			}
			req.Header.Set("Authorization", "Bearer client-key")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("code=%d want 401（客户端 key 不该能进运维面）body=%s", rec.Code, rec.Body.String())
			}
		})
		t.Run(c.name+" admin key 应放行", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader("{}"))
			if c.path == "/status" {
				req = httptest.NewRequest(http.MethodGet, c.path, nil)
			}
			req.Header.Set("Authorization", "Bearer admin-key")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code == http.StatusUnauthorized {
				t.Fatalf("admin key 被拒：code=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// 没配 admin_api_key 时回退旧行为（主 key 可用）——不能让既有脚本静默失效。
func TestAdminKeyFallsBackToMainKey(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		APIKey:   "client-key",
		Upstream: nil,
	})
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.Header.Set("Authorization", "Bearer client-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("未配 admin key 时应回退主 key：code=%d", rec.Code)
	}
}
