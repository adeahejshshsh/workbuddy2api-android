package loginsite

// 第二轮代码审计（CODE-AUDIT-FULL.md loginsite 段 / W2-LS2-01）的回归用例。

import (
	"net/url"
	"strings"
	"testing"
)

// 审查 W2-LS2-01：上游给的授权链接只认 https —— 面板会把它交给「在浏览器打开」，
// 非 https 的 scheme（javascript:/data:/http:）等于一次脚本执行或明文降级。
func TestDecorateAuthURLRejectsNonHTTPS(t *testing.T) {
	for _, raw := range []string{
		"javascript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"http://example.test/login?state=abc",
		"//example.test/login?state=abc", // 协议相对：没有 scheme
	} {
		if got := DecorateAuthURL(raw, ForRealm("cn"), "n", "s"); got != "" {
			t.Errorf("DecorateAuthURL(%q)=%q，非 https 应拒绝", raw, got)
		}
	}
	// 正常 https 链接照旧补齐参数。
	got := DecorateAuthURL("https://example.test/login?state=abc", ForRealm("cn"), "", "")
	if !strings.HasPrefix(got, "https://example.test/login?") {
		t.Fatalf("https 链接被误拒: %q", got)
	}
}

// 审查（透传保真）：已有参数必须**原样**保留 —— 顺序、编码、畸形参数都不动，
// 新参数只追加在末尾。此前用 q.Encode() 会按字典序重排 + 重新编码 + 丢掉畸形参数，
// 而授权页会把这条链接原样转给 risk-context（参数顺序属于出站指纹）。
func TestDecorateAuthURLPreservesExistingQuery(t *testing.T) {
	const rawQuery = "platform=workbuddy&state=a%2Bb&traceparent=00-abc-def-01&bad=%zz"
	raw := "https://example.test/login?" + rawQuery
	got := DecorateAuthURL(raw, ForRealm("cn"), "nonce-1", "sess-1")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(u.RawQuery, rawQuery+"&") {
		t.Fatalf("原参数被改写（顺序/编码/畸形参数都该保留）:\n got=%q\nwant 前缀 %q", u.RawQuery, rawQuery)
	}
	for _, want := range []string{"version=", "loginSessionId=sess-1", "ww_device_nonce=nonce-1"} {
		if !strings.Contains(u.RawQuery, want) {
			t.Errorf("缺追加参数 %q: %s", want, u.RawQuery)
		}
	}
}
