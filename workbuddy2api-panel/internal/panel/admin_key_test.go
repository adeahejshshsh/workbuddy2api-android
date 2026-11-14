package panel

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// 面板门禁必须与「发给下游客户端的 api_key」分开：客户端 key 一旦泄漏，不能等于
// 拿到全池 refresh token / 改写整份配置 / 删号停号的能力（审查 P0-1）。
func TestPanelClientKeyCannotReachSensitiveAPIs(t *testing.T) {
	p := New(Config{Pool: pool.New(""), APIKey: "client-key", AdminAPIKey: "admin-key"})
	sensitive := []struct{ method, path string }{
		{"GET", "/panel/api/export/accounts"},
		{"GET", "/panel/api/config"},
		{"POST", "/panel/api/config"},
		{"POST", "/panel/api/import/cockpit"},
		{"POST", "/panel/api/login/start"},
		{"POST", "/panel/api/accounts/u1/remove"},
		{"POST", "/panel/api/campaign/claim"},
	}
	for _, c := range sensitive {
		// 客户端 key：必须被拒（管理员密钥不匹配 → 401）。
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(c.method, c.path, nil)
		req.Header.Set("Authorization", "Bearer client-key")
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s 用客户端 key：code=%d want 401", c.method, c.path, rec.Code)
		}
		// 管理员 key：鉴权通过（具体状态码取决于实现，401/403 即视为没通过门禁）。
		rec = httptest.NewRecorder()
		req = httptest.NewRequest(c.method, c.path, nil)
		req.Header.Set("Authorization", "Bearer admin-key")
		p.ServeHTTP(rec, req)
		if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
			t.Errorf("%s %s 用管理员 key：code=%d 不该被门禁拦下", c.method, c.path, rec.Code)
		}
	}
	// 配了管理员密钥后，**整个面板**只认它：客户端 key 连只读页也进不来。
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/panel/api/overview", nil)
	req.Header.Set("Authorization", "Bearer client-key")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("配了 admin_api_key 后客户端 key 读 overview：code=%d want 401", rec.Code)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/panel/api/overview", nil)
	req.Header.Set("Authorization", "Bearer admin-key")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("管理员 key 读 overview：code=%d want 200", rec.Code)
	}
}

// 没配 admin_api_key 时：敏感接口一律 403（不是回落到 api_key 放行）。
func TestPanelSensitiveAPIsRefuseWithoutAdminKey(t *testing.T) {
	p := New(Config{Pool: pool.New(""), APIKey: "client-key"})
	for _, c := range []struct{ method, path string }{
		{"GET", "/panel/api/export/accounts"},
		{"GET", "/panel/api/config"},
		{"POST", "/panel/api/import/cockpit"},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(c.method, c.path, nil)
		req.Header.Set("Authorization", "Bearer client-key")
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s 未配 admin_api_key：code=%d want 403", c.method, c.path, rec.Code)
		}
	}
}

// 两把 key 都没配 = 全仓「空 key 不鉴权」语义：面板可用（运维有意关鉴权时保持行为）。
func TestPanelOpenWhenNoKeysConfigured(t *testing.T) {
	p := New(Config{Pool: pool.New("")})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/overview", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("无 key 配置时 overview：code=%d want 200", rec.Code)
	}
}
