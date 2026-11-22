package proxypool

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newLoginTestManager 造一个不落盘、不带健康探测的号池。
func newLoginTestManager(t *testing.T) *Manager {
	t.Helper()
	m, err := New("", http.DefaultTransport.(*http.Transport).Clone(), &net.Dialer{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	t.Cleanup(m.Close)
	return m
}

// TestPickForLoginNeverFallsBackToDirectUnlessPoolIsEmpty 钉住加号出口的三档：
// 有健康 → 只挑健康；没健康但有启用 → 仍从启用里挑（宁可报代理错误也不露本机 IP）；
// 一条启用的都没有 → 空串（调用方直连，不让没配代理的部署加不了号）。
func TestPickForLoginNeverFallsBackToDirectUnlessPoolIsEmpty(t *testing.T) {
	m := newLoginTestManager(t)
	if got := m.PickForLogin(); got != "" {
		t.Fatalf("空池应返回空串（= 直连），got %q", got)
	}
	res, err := m.Import("http://127.0.0.1:1\nhttp://127.0.0.1:2")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(res) != 2 || res[0].ID == "" || res[1].ID == "" {
		t.Fatalf("import 结果异常: %+v", res)
	}

	// 只有一个健康：随机也只会挑到它，绝不挑不健康的那个。
	m.mu.Lock()
	m.health[res[0].ID] = health{status: "healthy"}
	m.mu.Unlock()
	for i := 0; i < 20; i++ {
		if got := m.PickForLogin(); got != res[0].ID {
			t.Fatalf("有健康节点时应只挑它（%s），got %s", res[0].ID, got)
		}
	}

	// 都不健康：仍在启用节点里挑——这是刻意选择，直连才是要避免的。
	m.mu.Lock()
	m.health[res[0].ID] = health{status: "unhealthy"}
	m.mu.Unlock()
	seen := map[string]bool{}
	for i := 0; i < 40; i++ {
		seen[m.PickForLogin()] = true
	}
	if seen[""] {
		t.Fatalf("有启用节点时不该回落直连: %v", seen)
	}
	for id := range seen {
		if id != res[0].ID && id != res[1].ID {
			t.Fatalf("挑到了池外节点 %q", id)
		}
	}

	// 全部停用：与空池同义。
	disabled := false
	for _, r := range res {
		if err := m.Update(r.ID, Input{Enabled: &disabled}); err != nil {
			t.Fatalf("disable %s: %v", r.ID, err)
		}
	}
	if got := m.PickForLogin(); got != "" {
		t.Fatalf("全停用应返回空串，got %q", got)
	}
}

// TestDoPinnedSendsThroughNamedNode 钉住「登录走指定节点」：请求必须经该节点的
// 代理转发（代理侧看到绝对 URI），而不是从本机直连出去。这是整个特性的核心承诺，
// 一旦 doVia 被改成「失败即直连」，这条会立刻红。
func TestDoPinnedSendsThroughNamedNode(t *testing.T) {
	var proxied int
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !r.URL.IsAbs() {
			t.Errorf("不是代理请求（绝对 URI 缺失）: %s", r.URL)
		}
		proxied++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"msg":"OK","data":{"state":"s1"}}`)
	}))
	defer proxy.Close()

	m := newLoginTestManager(t)
	res, err := m.Import(proxy.URL)
	if err != nil || len(res) != 1 || res[0].ID == "" {
		t.Fatalf("import: %v %+v", err, res)
	}

	req, err := http.NewRequest(http.MethodPost, "http://upstream.invalid/v2/plugin/auth/state?platform=workbuddy", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := m.DoPinned(&http.Client{Timeout: 5 * time.Second}, req, res[0].ID)
	if err != nil {
		t.Fatalf("DoPinned: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if proxied != 1 {
		t.Fatalf("代理只收到 %d 次请求，want 1（说明没走节点）", proxied)
	}
	if !strings.Contains(string(body), `"state":"s1"`) {
		t.Fatalf("响应体不对: %s", body)
	}

	// 空 id = 明确的直连请求（唯一允许直连的入口）。
	if _, err := m.DoPinned(&http.Client{Timeout: 5 * time.Second}, req.Clone(req.Context()), ""); err == nil {
		t.Fatal("空 id 走直连时应当真的去连 upstream.invalid 而失败")
	}
}

// TestDoPinnedMissingNodeFailsLoudly 节点不存在必须报 *Error，绝不静默回落直连：
// 悄悄直连等于把本机 IP 交给上游，还把「这号绑了哪条出口」变成谎话。
func TestDoPinnedMissingNodeFailsLoudly(t *testing.T) {
	m := newLoginTestManager(t)
	req, err := http.NewRequest(http.MethodGet, "http://upstream.invalid/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.DoPinned(&http.Client{Timeout: 5 * time.Second}, req, "no-such-node")
	if err == nil {
		t.Fatal("不存在的节点应返回错误")
	}
	if !IsError(err) {
		t.Fatalf("应返回 *proxypool.Error，got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "proxy not found") {
		t.Fatalf("错误文本应可诊断: %v", err)
	}
}

// TestNodeLabelHidesCredentials 日志标签不能带 URL 里的密码：加号的日志会打印
// 「出口=代理 <label>」，凭据一旦进了日志就等于写进了别人的 scrollback 与日志文件。
func TestNodeLabelHidesCredentials(t *testing.T) {
	m := newLoginTestManager(t)
	res, err := m.Import("http://user:TOPSECRET@127.0.0.1:8080#Office")
	if err != nil || len(res) != 1 {
		t.Fatalf("import: %v %+v", err, res)
	}
	label := m.NodeLabel(res[0].ID)
	if strings.Contains(label, "TOPSECRET") || strings.Contains(label, "user@") {
		t.Fatalf("标签泄露了节点凭据: %q", label)
	}
	if label != "Office" {
		t.Fatalf("有名字的节点应优先用名字，got %q", label)
	}
	if got := m.NodeLabel("no-such-node"); got != "no-such-" {
		t.Fatalf("已删节点的标签应退回 ID 前缀，got %q", got)
	}
}
