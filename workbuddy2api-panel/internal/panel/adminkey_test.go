// adminkey_test.go 管理员密钥自举入口的回归钉。
//
// 守的是 2026-11-16 那个用户反馈的形态：P0-1 之后没配 admin_api_key 的部署
// 什么都点不动，而面板里没有任何地方能填这个值（门锁上、钥匙锁在屋里）。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// newBootstrapPanel 构造一个「只有客户端 key、没有 admin key」的面板（老部署的形态），
// SaveConfig 只记下提交的原始 JSON，不落盘。
func newBootstrapPanel(saved *[]byte) *Panel {
	return New(Config{
		Version: "test",
		APIKey:  "client-key-123456",
		SaveConfig: func(raw []byte) ([]string, error) {
			if saved != nil {
				*saved = append([]byte(nil), raw...)
			}
			return nil, nil
		},
	})
}

func postAdminKey(t *testing.T, p *Panel, remote, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/panel/api/admin_key", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	// 带上当前生效的 key：真实浏览器手里就有它（老部署是客户端 key，配过之后是 admin key）。
	// 不带就成了「先被 401 挡下」，测不到本文件要守的那几条判据。
	if k, _ := p.apiKey(); k != "" {
		r.Header.Set("Authorization", "Bearer "+k)
	}
	// httptest 默认 RemoteAddr 是 192.0.2.1:1234（非回环），这里按用例显式给。
	r.RemoteAddr = remote
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, r)
	return rec
}

// 核心路径：本机 + 还没配过 → 写进配置，且**不会被 admin 门禁先挡掉**。
// 后半句才是这个入口存在的理由：POST /panel/api/config 在这条路径上是 403。
func TestAdminKeyBootstrapFromLoopback(t *testing.T) {
	var saved []byte
	p := newBootstrapPanel(&saved)
	body := `{"admin_api_key":"0123456789abcdef0123"}`
	r := httptest.NewRequest(http.MethodPost, "/panel/api/admin_key", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer client-key-123456") // 老部署手里就这把
	r.RemoteAddr = "127.0.0.1:51234"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s want 200", rec.Code, rec.Body.String())
	}
	if len(saved) == 0 {
		t.Fatal("没有调用 SaveConfig")
	}
	var got map[string]string
	if err := json.Unmarshal(saved, &got); err != nil {
		t.Fatalf("SaveConfig 收到非法 JSON: %v (%s)", err, saved)
	}
	if got["admin_api_key"] != "0123456789abcdef0123" {
		t.Fatalf("落盘值 = %q want 0123456789abcdef0123", got["admin_api_key"])
	}
}

// 同一条路径在没有 admin key 时必须**不被 panelSensitivePath 拦下** —— 这是
// 「门锁上、钥匙锁在屋里」的唯一解法，改这条等于把用户重新关在门外。
func TestAdminKeyBootstrapPathIsNotSensitive(t *testing.T) {
	if panelSensitivePath(http.MethodPost, "/panel/api/admin_key") {
		t.Fatal("admin_key 引导入口被列进敏感接口：老部署将无法自举管理员密钥")
	}
	// 但 /panel/api/admin/ 前缀仍然是敏感的（别把前缀规则一起放开）。
	if !panelSensitivePath(http.MethodPost, "/panel/api/admin/anything") {
		t.Fatal("/panel/api/admin/ 前缀必须仍然敏感")
	}
}

// 非回环一律拒绝：否则拿到客户端 key 的远端就能自举管理员密钥，等于把 P0-1 又打开。
func TestAdminKeyBootstrapRefusesNonLoopback(t *testing.T) {
	var saved []byte
	p := newBootstrapPanel(&saved)
	rec := postAdminKey(t, p, "192.0.2.9:4433", `{"admin_api_key":"0123456789abcdef0123"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d want 403（非回环必须拒绝）", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "admin_key_local_only") {
		t.Fatalf("错误码应可辨识: %s", rec.Body.String())
	}
	if len(saved) != 0 {
		t.Fatal("被拒绝的请求不该落盘")
	}
}

// X-Forwarded-For 是客户端可写的：伪造它不该放行。
func TestAdminKeyBootstrapIgnoresForwardedFor(t *testing.T) {
	p := newBootstrapPanel(nil)
	r := httptest.NewRequest(http.MethodPost, "/panel/api/admin_key",
		strings.NewReader(`{"admin_api_key":"0123456789abcdef0123"}`))
	r.Header.Set("Content-Type", "application/json")
	if k, _ := p.apiKey(); k != "" {
		r.Header.Set("Authorization", "Bearer "+k)
	}
	r.Header.Set("X-Forwarded-For", "127.0.0.1")
	r.RemoteAddr = "192.0.2.9:4433"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d want 403（XFF 不能当判据）", rec.Code)
	}
}

// 已经配过 → 引导入口关闭（轮换走设置页，那里要求当前 admin key）。
func TestAdminKeyBootstrapRefusesWhenDedicated(t *testing.T) {
	p := New(Config{
		Version:     "test",
		APIKey:      "client-key-123456",
		AdminAPIKey: "existing-admin-key",
		SaveConfig:  func([]byte) ([]string, error) { return nil, nil },
	})
	rec := postAdminKey(t, p, "127.0.0.1:51234", `{"admin_api_key":"0123456789abcdef0123"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "admin_key_already_set") {
		t.Fatalf("错误码应可辨识: %s", rec.Body.String())
	}
}

// 校验：太短 / 带空白 / 与客户端 key 相同，都必须拒。
func TestAdminKeyBootstrapValidation(t *testing.T) {
	cases := []struct{ name, body string }{
		{"太短", `{"admin_api_key":"short"}`},
		{"空", `{"admin_api_key":""}`},
		{"带空白", `{"admin_api_key":"0123456789 abcdef0123"}`},
		{"与客户端 key 相同", `{"admin_api_key":"client-key-123456"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var saved []byte
			p := newBootstrapPanel(&saved)
			rec := postAdminKey(t, p, "127.0.0.1:51234", c.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d want 400", rec.Code)
			}
			if len(saved) != 0 {
				t.Fatal("校验失败不该落盘")
			}
		})
	}
}

// 已配过 admin key 的部署：overview 报 admin_key_set=true，前端据此不显示横幅。
func TestOverviewReportsAdminKeySet(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k", AdminAPIKey: "a", Pool: pool.New("")})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil)
	req.Header.Set("Authorization", "Bearer a")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var d map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if v, ok := d["admin_key_set"].(bool); !ok || !v {
		t.Fatalf("admin_key_set = %v want true", d["admin_key_set"])
	}
}
