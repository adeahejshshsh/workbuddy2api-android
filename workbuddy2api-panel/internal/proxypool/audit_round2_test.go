// audit_round2_test.go 第二轮代码审计（CODE-AUDIT-FULL 第 10.17 节 proxypool）剩余项。
//
// 命名 TestC6_<编号>_<语义>，与报告编号一一对应。
package proxypool

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// W2-C6-18：Error 的 %v / %+v / %s 一律只输出脱敏 Reason，不能把底层 cause
// （典型是带 user:pass@host 的 *url.Error）带进日志；errors.Is/As 仍要能用。
func TestC6_18_ErrorFormatRedacted(t *testing.T) {
	cause := &url.Error{
		Op:  "Get",
		URL: "http://alice:s3cr3t@proxy.example.com:8080/",
		Err: fmt.Errorf("dial tcp: i/o timeout"),
	}
	err := &Error{ProxyID: "p1", Reason: "dial failed", cause: cause}
	for _, verb := range []string{"%v", "%+v", "%s", "%#v"} {
		got := fmt.Sprintf(verb, err)
		if strings.Contains(got, "s3cr3t") || strings.Contains(got, "alice") {
			t.Fatalf("%s 泄露了凭据: %s", verb, got)
		}
		if !strings.Contains(got, "dial failed") {
			t.Fatalf("%s 丢了 Reason: %s", verb, got)
		}
	}
	// errors.Is/As 仍走 Unwrap（脱敏不能破坏错误链）。
	if !IsError(err) {
		t.Fatal("errors.As 应仍能识别 *Error")
	}
	wrapped := fmt.Errorf("outer: %w", err)
	if !IsError(wrapped) {
		t.Fatal("包装后 errors.As 失效")
	}
}

// W2-C6-19：host 段策略 —— 只接受 IP 字面量或纯 ASCII 主机名；IDN / 怪字符拒绝。
func TestC6_19_ParseURLHostPolicy(t *testing.T) {
	ok := []string{
		"http://proxy.example.com:8080",
		"socks5://10.0.0.1:1080",
		"socks5h://[::1]:1080",
		"http://user:pass@my-proxy_1.example:3128",
	}
	for _, raw := range ok {
		if _, err := parseURL(raw); err != nil {
			t.Errorf("合法代理 URL 被拒: %s → %v", raw, err)
		}
	}
	bad := []string{
		"http://例子.com:8080",           // IDN（同形字面）
		"http://proxy%40evil.com:8080", // 百分号编码的 @
		"http://pro xy.com:8080",       // 空白
		"http://proxy\\evil.com:8080",  // 反斜杠
	}
	for _, raw := range bad {
		if _, err := parseURL(raw); err == nil {
			t.Errorf("异常 host 应被拒: %s", raw)
		}
	}
}

// W2-C6-17：随机/realm 模式下「没有启用节点 → 直连」的语义保留（issue #150），
// 但必须可观测（计数），不能静默。
func TestC6_17_DirectFallbackCounted(t *testing.T) {
	m := testManager(t, "")
	id := addProxy(t, m, "http://127.0.0.1:8080")
	// 手工停用唯一节点：与「一条代理都没配」同义（issue #150）。
	disabled := false
	if err := m.Update(id, Input{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if got := m.DirectFallbacks(); got != 0 {
		t.Fatalf("初始计数=%d want 0", got)
	}
	for i := 0; i < 3; i++ {
		n, useProxy, err := m.selectLocked("acct", "cn")
		if err != nil || useProxy || n.ID != "" {
			t.Fatalf("无启用节点应直连: node=%+v use=%v err=%v", n, useProxy, err)
		}
	}
	if got := m.DirectFallbacks(); got != 3 {
		t.Fatalf("直连回落计数=%d want 3（不可观测等于静默）", got)
	}
	// 显式 direct 绑定不算「回落」（那是运维明确选择），不应计入。
	if err := m.Bind("acct2", Binding{Mode: "direct"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.selectLocked("acct2", "cn"); err != nil {
		t.Fatal(err)
	}
	if got := m.DirectFallbacks(); got != 3 {
		t.Fatalf("显式 direct 不应计入回落: %d", got)
	}
}

// W2-C6-12（同类）：存量 URL 损坏时 Update 不能解引用 nil（面板接口 panic）。
func TestC6_12_UpdateToleratesBrokenStoredURL(t *testing.T) {
	m := testManager(t, "")
	id := addProxy(t, m, "http://127.0.0.1:8080")
	m.mu.Lock()
	for i := range m.data.Nodes {
		if m.data.Nodes[i].ID == id {
			m.data.Nodes[i].URL = "://broken-url"
		}
	}
	m.mu.Unlock()
	// 只改口令：旧实现会在 u.User 处解引用 nil。
	if err := m.Update(id, Input{Password: "new-pass"}); err == nil {
		t.Fatal("损坏的存量 URL 应报错而不是 panic")
	}
	// 提供合法新 URL 时应能自愈。
	if err := m.Update(id, Input{URL: "http://127.0.0.1:8081"}); err != nil {
		t.Fatalf("提供合法 URL 应能修复: %v", err)
	}
}

// W2-C6-44：出口画像的缓存新鲜度必须挡住时钟回拨（now 早于 At 时不能永远新鲜）。
func TestC6_44_ExitInfoFreshClockGuard(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	e := ExitInfo{IP: "1.2.3.4", At: now}
	if !e.fresh(now.Add(intelTTL / 2)) {
		t.Fatal("缓存期内应 fresh")
	}
	if e.fresh(now.Add(intelTTL)) {
		t.Fatal("超过 TTL 不应 fresh")
	}
	if e.fresh(now.Add(-intelTTL)) {
		t.Fatal("时钟回拨（now 早于 At）必须视为过期，否则画像永久冻结")
	}
	if (ExitInfo{}).fresh(now) {
		t.Fatal("零值 At 不应 fresh")
	}
}

// roundTripperFunc 把函数适配成 http.RoundTripper（用例内造回包用）。
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// W2-C6-42：出口画像查询不得跟随重定向（回包由待测代理转发，3xx 等于让外部
// 决定出站目标）。这里用计数 RT 证明「只发一次、不跟随」。
func TestC6_42_ExitInfoNoRedirectFollow(t *testing.T) {
	calls := 0
	rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return &http.Response{
				StatusCode: http.StatusFound,
				Header:     http.Header{"Location": []string{"http://169.254.169.254/latest/meta-data/"}},
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    r,
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"status":"success","query":"1.2.3.4"}`)),
			Request:    r,
		}, nil
	})
	info := fetchExitInfo(context.Background(), rt)
	if calls != 1 {
		t.Fatalf("跟随了重定向: RT 被调用 %d 次（应只发 1 次）", calls)
	}
	if info.IP != "" {
		t.Fatalf("3xx 应视为失败，got %+v", info)
	}
	if info.At.IsZero() {
		t.Fatal("失败也要写 At（否则每次探测都重查）")
	}
}
