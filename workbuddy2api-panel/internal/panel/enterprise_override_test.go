package panel

// 面板「账号类型」开关 + 国际版任务查询入口。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func TestAccountEnterpriseOverrideEndpoint(t *testing.T) {
	dir := t.TempDir()
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		FilePath: filepath.Join(dir, "workbuddy-u1.json"), EnterpriseID: "e-1"}
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	pl := pool.New("")
	pl.Add(a)
	p := New(Config{Version: "test", Pool: pl})

	send := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/panel/api/accounts/u1/enterprise", strings.NewReader(body))
		req.SetPathValue("uid", "u1")
		p.accountEnterpriseOverride(rec, req)
		return rec
	}

	// 前置：带 enterpriseId → 企业版
	if !p.cfg.Pool.AuthByUID("u1").IsEnterprise() {
		t.Fatal("前置：应判企业版")
	}
	rec := send(`{"override":"personal"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		Override   string `json:"override"`
		Enterprise bool   `json:"enterprise"`
		Changed    bool   `json:"changed"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Override != "personal" || resp.Enterprise || !resp.Changed {
		t.Fatalf("resp=%+v want personal/false/changed", resp)
	}
	// 落盘：重启后仍生效
	raw, _ := os.ReadFile(a.FilePath)
	back, err := auth.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if back.EnterpriseOverride != "personal" || back.IsEnterprise() {
		t.Fatalf("落盘后 override=%q enterprise=%v", back.EnterpriseOverride, back.IsEnterprise())
	}

	// 非法值 → 400（不能静默当自动判定）
	rec = send(`{"override":"企业版账号"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法值应 400，got %d", rec.Code)
	}

	// 空 = 恢复自动判定
	rec = send(`{"override":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("清空 code=%d", rec.Code)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Override != "" || !resp.Enterprise {
		t.Fatalf("清空后应回自动（企业版）: %+v", resp)
	}
}

// 国际版任务：查询入口保留（200 + 空 capabilities），写操作仍 501。
func TestAccountTasksGlobalIsReadOnly(t *testing.T) {
	a := &auth.Auth{UID: "g1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		Domain: "https://www.workbuddy.ai"}
	_, _ = auth.BackfillRealmFor(a, "global")
	pl := pool.New("")
	pl.Add(a)
	// 上游任务端点没实现（404）：国际版分支必须优雅回 200 + 空列表 + 说明，
	// 而不是 502/panic —— 这正是「入口保留但不假装能用」的形态。
	up := &upstream.Client{
		HTTP: &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusNotFound,
				Header: http.Header{"Content-Type": []string{"application/json"}},
				Body:   io.NopCloser(strings.NewReader(`{"code":404,"msg":"not found"}`))}, nil
		})},
		ChatBaseCN: "https://cn.example", WebBaseCN: "https://cn.example",
		BillingBaseCN: "https://cn.example",
	}
	p := New(Config{Version: "test", Pool: pl, Upstream: up})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/panel/api/accounts/g1/tasks", nil)
	req.SetPathValue("uid", "g1")
	p.accountTasks(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("国际版任务查询应 200（入口保留），got %d body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		Realm        string          `json:"realm"`
		Capabilities map[string]bool `json:"capabilities"`
		Message      string          `json:"message"`
		Tasks        []any           `json:"tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body=%s err=%v", rec.Body, err)
	}
	if resp.Realm != "global" {
		t.Fatalf("realm=%q", resp.Realm)
	}
	if resp.Capabilities["accept"] || resp.Capabilities["claim"] || resp.Capabilities["automate"] {
		t.Fatalf("国际区写能力必须全 false: %+v", resp.Capabilities)
	}
	if resp.Message == "" {
		t.Fatal("应带「只读」说明")
	}

	// 接单仍被拒（写操作没有验证过的契约）
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/panel/api/accounts/g1/tasks/accept",
		io.NopCloser(strings.NewReader(`{"task_codes":["x"]}`)))
	req.SetPathValue("uid", "g1")
	p.accountTaskAccept(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("国际版接单应 501，got %d body=%s", rec.Code, rec.Body)
	}
}
