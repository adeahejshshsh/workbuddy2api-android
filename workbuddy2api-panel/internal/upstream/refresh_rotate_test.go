package upstream

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// bug #2 回归：上游 refresh 只回 refreshToken（缺 accessToken）时，rotate 后的
// refreshToken 必须**落盘**。三个调用点（handler/scheduler/signin）在 error 分支
// 都直接放弃、不 SaveAtomic，若不在这里写回磁盘，重启后读回的是已被上游作废的
// 旧 refreshToken —— 该账号从此再也刷新不了，只能重新登录。
func TestRefreshRotatePersistsEvenWithoutAccessToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 只回 refreshToken，accessToken 缺失（畸形响应）
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"refreshToken":"rt-new","domain":"workbuddy.cn"}}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u1.json")
	a := &auth.Auth{
		UID: "u1", AccessToken: "at-old", RefreshToken: "rt-old",
		ExpiresAt: 9999999999, FilePath: fp,
	}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	err := c.RefreshToken(a)
	if err == nil {
		t.Fatal("缺 accessToken 应当返回错误（调用方据此换号/告警）")
	}

	// 内存里已更新
	if a.RefreshTokenValue() != "rt-new" {
		t.Fatalf("内存 refreshToken = %q, want rt-new", a.RefreshTokenValue())
	}
	// 磁盘上也必须已更新 —— 这才是本 bug 的核心
	raw, rerr := os.ReadFile(fp)
	if rerr != nil {
		t.Fatalf("read auth file: %v", rerr)
	}
	if !strings.Contains(string(raw), "rt-new") {
		t.Fatalf("rotate 后的 refreshToken 没落盘（重启后会读回已作废的旧值）:\n%s", raw)
	}
}
