package panel

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// importVerifyEnv 搭一个池里已有 n 个账号（uid 为 u、ux、uxx…）的面板，refresh 端点走 refresh()。
func importVerifyEnv(t *testing.T, n int, refresh func() (int, string)) (*Panel, *pool.Pool, string) {
	t.Helper()
	dir := t.TempDir()
	up := &upstream.Client{
		HTTP: &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
			status, body := http.StatusNotFound, `{}`
			if strings.HasSuffix(r.URL.Path, "/v2/plugin/auth/token/refresh") {
				status, body = refresh()
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(body))}, nil
		})},
		ChatBaseCN: "https://cn.example", WebBaseCN: "https://cn.example", BillingBaseCN: "https://cn.example",
	}
	pl := pool.New("")
	for i := 0; i < n; i++ {
		uid := "u" + strings.Repeat("x", i)
		a := &auth.Auth{UID: uid, AccessToken: "old-at", RefreshToken: "old-rt",
			ExpiresAt: time.Now().Add(72 * time.Hour).Unix(), FilePath: filepath.Join(dir, "workbuddy-"+uid+".json")}
		if err := a.SaveAtomic(); err != nil {
			t.Fatal(err)
		}
		pl.Add(a)
	}
	return New(Config{Pool: pl, Upstream: up, AuthDir: dir}), pl, dir
}

// importVerifyPost 用 n 个账号（与 importVerifyEnv 的 uid 对应）的 cockpit 导出做一次真实导入。
func importVerifyPost(t *testing.T, pn *Panel, n int) (imported, skipped int, warnings []string) {
	t.Helper()
	var list []map[string]any
	for i := 0; i < n; i++ {
		list = append(list, map[string]any{"uid": "u" + strings.Repeat("x", i), "access_token": "new-at",
			"refresh_token": "new-rt", "expires_at": time.Now().Add(time.Hour).UnixMilli()})
	}
	raw, _ := json.Marshal(list)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "accounts.json")
	fw.Write(raw)
	mw.Close()
	req := httptest.NewRequest("POST", "/panel/api/import/cockpit", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var got struct {
		Imported int      `json:"imported"`
		Skipped  int      `json:"skipped"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v (%s)", err, rec.Body)
	}
	return got.Imported, got.Skipped, got.Warnings
}

const importRefreshOK = `{"code":0,"data":{"accessToken":"rotated-at","refreshToken":"rotated-rt","expiresIn":3600}}`

// 求证已把文件里的 refresh token 轮换：这时落盘失败不能丢掉新凭据。池里原有的那份
// 若与文件同源，refresh token 已随之作废，丢掉新值这个号就只能重新登录。
func TestImportCockpitKeepsRotatedCredentialInMemoryWhenSaveFails(t *testing.T) {
	pn, pl, dir := importVerifyEnv(t, 1, func() (int, string) { return http.StatusOK, importRefreshOK })
	// 目标文件位置被一个目录占着：rename 覆盖目录必然失败，模拟磁盘 / 权限问题。
	target := filepath.Join(dir, "workbuddy-u.json")
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(target, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}

	imported, skipped, warnings := importVerifyPost(t, pn, 1)
	if imported != 1 || skipped != 0 || len(warnings) != 1 {
		t.Fatalf("imported=%d skipped=%d warnings=%v want 1/0/1 条警告", imported, skipped, warnings)
	}
	if rt := pl.AuthByUID("u").RefreshTokenValue(); rt != "rotated-rt" {
		t.Fatalf("池里的 refresh token=%q，应已换成求证轮换后的 rotated-rt", rt)
	}
}

// 几百个已有号重导时，求证必须并发，且受 importVerifyConcurrency 约束。
func TestImportCockpitVerifiesOverwritesConcurrently(t *testing.T) {
	oldC := importVerifyConcurrency
	importVerifyConcurrency = 3
	t.Cleanup(func() { importVerifyConcurrency = oldC })

	var inFlight, peak atomic.Int32
	pn, _, _ := importVerifyEnv(t, 6, func() (int, string) {
		n := inFlight.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		time.Sleep(60 * time.Millisecond)
		inFlight.Add(-1)
		return http.StatusOK, importRefreshOK
	})
	imported, skipped, _ := importVerifyPost(t, pn, 6)
	if imported != 6 || skipped != 0 {
		t.Fatalf("imported=%d skipped=%d want 6/0", imported, skipped)
	}
	if p := peak.Load(); p < 2 || p > 3 {
		t.Fatalf("求证并发峰值=%d，应在 [2,3]（串行为 1，超限 >3）", p)
	}
}

// 总时限到点后，没轮到的号不再发 refresh，保留原凭据；已发出的求证照常提交。
func TestImportCockpitVerifyBudgetStopsStartingNewVerifications(t *testing.T) {
	oldC, oldB := importVerifyConcurrency, importVerifyBudget
	importVerifyConcurrency, importVerifyBudget = 1, 30*time.Millisecond
	t.Cleanup(func() { importVerifyConcurrency, importVerifyBudget = oldC, oldB })

	var calls atomic.Int32
	pn, pl, _ := importVerifyEnv(t, 3, func() (int, string) {
		calls.Add(1)
		time.Sleep(150 * time.Millisecond)
		return http.StatusOK, importRefreshOK
	})
	imported, skipped, _ := importVerifyPost(t, pn, 3)
	if imported != 1 || skipped != 2 || calls.Load() != 1 {
		t.Fatalf("imported=%d skipped=%d refresh=%d want 1/2/1", imported, skipped, calls.Load())
	}
	if at := pl.AuthByUID("uxx").AccessTokenValue(); at != "old-at" {
		t.Fatalf("没轮到的号凭据被改了：%q", at)
	}
}
