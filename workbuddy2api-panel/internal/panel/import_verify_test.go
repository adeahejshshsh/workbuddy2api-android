package panel

// 覆盖已有凭据前必须先求证新凭据还活着（上游 e36ad07）。
//
// 起因：把一份旧导出重新导入时，文件里的 refresh token 早已被轮换；直接覆盖会把本来
// 能用的账号变成「要重新登录」，而 .bak 不会被自动回退。导入是唯一会用外部文件盖掉
// 工作凭据的路径，所以只接受「明确活着」—— dead（上游明确拒绝）与 inconclusive
// （网络/5xx/429 没判词）都保留原凭据。

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
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func TestImportCockpitKeepsCredentialWhenVerifyFails(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"refresh 被上游拒绝", http.StatusBadRequest, `{"code":12153,"msg":"Invalid refresh token"}`},
		{"上游 5xx 无法判定", http.StatusBadGateway, `{"code":0,"msg":"upstream bad gateway"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			refreshCalls := 0
			up := &upstream.Client{
				HTTP: &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
					if strings.HasSuffix(r.URL.Path, "/v2/plugin/auth/token/refresh") {
						refreshCalls++
						return &http.Response{
							StatusCode: tc.status,
							Header:     http.Header{"Content-Type": []string{"application/json"}},
							Body:       io.NopCloser(strings.NewReader(tc.body)),
						}, nil
					}
					return &http.Response{StatusCode: http.StatusNotFound,
						Body: io.NopCloser(strings.NewReader(`{}`))}, nil
				})},
				ChatBaseCN: "https://cn.example", WebBaseCN: "https://cn.example",
				BillingBaseCN: "https://cn.example",
			}
			pl := pool.New("")
			pn := New(Config{Pool: pl, Upstream: up, AuthDir: dir})

			// 池里已有这个账号，磁盘上那份凭据此刻还能用
			path := filepath.Join(dir, "workbuddy-u1.json")
			old := &auth.Auth{UID: "u1", AccessToken: "old-at", RefreshToken: "old-rt",
				ExpiresAt: time.Now().Add(72 * time.Hour).Unix(), FilePath: path}
			if err := old.SaveAtomic(); err != nil {
				t.Fatalf("prepare existing credential: %v", err)
			}
			pl.Add(old)

			var buf bytes.Buffer
			mw := multipart.NewWriter(&buf)
			fw, err := mw.CreateFormFile("file", "accounts.json")
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal([]map[string]any{{
				"uid": "u1", "access_token": "new-at", "refresh_token": "new-rt",
				"expires_at": time.Now().Add(time.Hour).UnixMilli(),
			}})
			if _, err := fw.Write(raw); err != nil {
				t.Fatal(err)
			}
			if err := mw.Close(); err != nil {
				t.Fatal(err)
			}
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
				Errors   []string `json:"errors"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode response: %v (%s)", err, rec.Body)
			}
			if got.Imported != 0 || got.Skipped != 1 {
				t.Fatalf("imported=%d skipped=%d want 0/1（%s）", got.Imported, got.Skipped, rec.Body)
			}
			if refreshCalls != 1 {
				t.Fatalf("refresh 校验调用 %d 次 want 1", refreshCalls)
			}
			saved, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(saved), "old-at") {
				t.Fatalf("原凭据被覆盖了：%s", saved)
			}
		})
	}
}

// 新账号（池里没有同 uid）不该为这次校验多打一次上游：导入几十个新号时那是几十次
// 无谓请求，还可能撞风控。
func TestImportCockpitNewAccountSkipsVerify(t *testing.T) {
	dir := t.TempDir()
	refreshCalls := 0
	up := &upstream.Client{
		HTTP: &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
			refreshCalls++
			return &http.Response{StatusCode: http.StatusNotFound,
				Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		})},
		ChatBaseCN: "https://cn.example", WebBaseCN: "https://cn.example",
		BillingBaseCN: "https://cn.example",
	}
	pn := New(Config{Pool: pool.New(""), Upstream: up, AuthDir: dir})

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "accounts.json")
	raw, _ := json.Marshal([]map[string]any{{
		"uid": "fresh", "access_token": "at", "refresh_token": "rt",
		"expires_at": time.Now().Add(time.Hour).Unix(),
	}})
	_, _ = fw.Write(raw)
	_ = mw.Close()

	req := httptest.NewRequest("POST", "/panel/api/import/cockpit", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)

	var got struct {
		Imported int `json:"imported"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Imported != 1 {
		t.Fatalf("新账号应直接导入，实际 imported=%d body=%s", got.Imported, rec.Body)
	}
	if refreshCalls != 0 {
		t.Fatalf("新账号不该触发凭据校验，实际打了 %d 次上游", refreshCalls)
	}
}
