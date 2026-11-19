package server

// W2-01/02 回归：admin_api_key 必须每请求从 live 快照读取。
//
// 旧实现把 h.cfg.AdminAPIKey 捕获在构造期闭包里：
//   - 未配 admin key 时永远回退客户端 key；
//   - 面板轮换 admin key 只更新 live 快照，旧 key 在 server 侧继续有效。

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
)

func adminKeyStatus(t *testing.T, h *Handler, key string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestAdminKeyRotationUsesLiveSnapshot(t *testing.T) {
	live := livecfg.New(livecfg.Snapshot{APIKey: "client-key", AdminAPIKey: "admin-old"})
	h := NewHandler(Config{
		Pool:         testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		APIKey:       "client-key",
		AdminAPIKey:  "admin-old", // 构造期值：轮换后不应再被使用
		Live:         live,
		AdminEnabled: true,
	})

	if code := adminKeyStatus(t, h, "admin-old"); code != http.StatusOK {
		t.Fatalf("初始 admin key 应放行：code=%d", code)
	}

	// 面板「设置管理员密钥」：只更新 live 快照（与 main.go 的热应用路径一致）。
	live.Store(livecfg.Snapshot{APIKey: "client-key", AdminAPIKey: "admin-new"})

	if code := adminKeyStatus(t, h, "admin-old"); code != http.StatusUnauthorized {
		t.Fatalf("轮换后旧 key 必须 401，实际 code=%d（构造期快照未失效）", code)
	}
	if code := adminKeyStatus(t, h, "admin-new"); code != http.StatusOK {
		t.Fatalf("轮换后新 key 必须 200，实际 code=%d", code)
	}
	if code := adminKeyStatus(t, h, "client-key"); code != http.StatusUnauthorized {
		t.Fatalf("配了 admin key 后客户端 key 不该进 /status，实际 code=%d", code)
	}
}

// 未配 admin key 的旧部署：回退的客户端 key 也走 live 快照（api_key 热轮换同样生效）。
func TestAdminKeyFallbackUsesLiveAPIKey(t *testing.T) {
	live := livecfg.New(livecfg.Snapshot{APIKey: "client-old"})
	h := NewHandler(Config{
		Pool:   testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		APIKey: "client-old",
		Live:   live,
	})
	if code := adminKeyStatus(t, h, "client-old"); code != http.StatusOK {
		t.Fatalf("回退主 key 应放行：code=%d", code)
	}
	live.Store(livecfg.Snapshot{APIKey: "client-new"})
	if code := adminKeyStatus(t, h, "client-old"); code != http.StatusUnauthorized {
		t.Fatalf("api_key 轮换后旧值必须 401，实际 code=%d", code)
	}
	if code := adminKeyStatus(t, h, "client-new"); code != http.StatusOK {
		t.Fatalf("api_key 轮换后新值必须 200，实际 code=%d", code)
	}
}
