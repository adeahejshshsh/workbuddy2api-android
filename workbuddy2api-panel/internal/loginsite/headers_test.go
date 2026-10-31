package loginsite

// 设备码三端点的头族金样本（2026-10-30 双版本真机抓包口径）。
//
// 这三个头族此前没有任何覆盖 —— 上一批改 platform/UA/浏览器头时只能靠肉眼核，
// 而它们的差异恰恰是最容易被「统一成同一套」抹平的：X-Domain 只有两个端点有、
// 只有 POST 带 Content-Type、x-no-* 是三个还是四个、channel-id 恒 unknown。

import (
	"strings"
	"testing"
)

// isHex32 真机 X-Request-ID 形态：32 位纯 hex，无毫秒前缀。
func isHex32(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

func TestHeadersForState(t *testing.T) {
	cn := ForRealm(RealmCN)
	h := cn.HeadersForState("")
	// 公共头
	if h["X-Product"] != "SaaS" {
		t.Errorf("X-Product=%q want SaaS", h["X-Product"])
	}
	if !strings.HasPrefix(h["User-Agent"], "WorkBuddy/5.9.3") {
		t.Errorf("CN UA=%q 应以 WorkBuddy/5.9.3 开头", h["User-Agent"])
	}
	if !isHex32(h["X-Request-ID"]) {
		t.Errorf("X-Request-ID=%q 应为 32 位 hex", h["X-Request-ID"])
	}
	// POST 才带 Content-Type
	if h["Content-Type"] != "application/json" {
		t.Errorf("auth/state 应带 Content-Type，得到 %q", h["Content-Type"])
	}
	// X-Domain 是「哪个前端在调用」，不是 API base 主机名（CN 打 copilot.tencent.com）
	if h["X-Domain"] != "www.workbuddy.cn" {
		t.Errorf("CN X-Domain=%q want www.workbuddy.cn", h["X-Domain"])
	}
	if h["X-Custom-Channel-Id"] != "unknown" {
		t.Errorf("X-Custom-Channel-Id=%q want 字面量 unknown", h["X-Custom-Channel-Id"])
	}
	// 四个 x-no-*，值是字符串 "true" 不是 "1"
	for _, k := range []string{"X-No-Authorization", "X-No-User-Id", "X-No-Enterprise-Id", "X-No-Department-Info"} {
		if h[k] != "true" {
			t.Errorf("%s=%q want \"true\"", k, h[k])
		}
	}
	// 浏览器语义头一个都不该有
	for _, k := range []string{"Origin", "Referer", "X-Requested-With"} {
		if v, ok := h[k]; ok {
			t.Errorf("三端点恒无 %s，得到 %q", k, v)
		}
	}
	// 没给设备 token 就不发（不伪造）
	if _, ok := h["X-Device-Token"]; ok {
		t.Error("deviceToken 为空时不该发 X-Device-Token")
	}
	// 给了才发
	if got := cn.HeadersForState("dt-1")["X-Device-Token"]; got != "dt-1" {
		t.Errorf("X-Device-Token=%q want dt-1", got)
	}
	// GL 站
	if gl := ForRealm(RealmGlobal).HeadersForState(""); gl["X-Domain"] != "www.workbuddy.ai" {
		t.Errorf("GL X-Domain=%q want www.workbuddy.ai", gl["X-Domain"])
	} else if !strings.HasPrefix(gl["User-Agent"], "workbuddy-ai/5.9.4") {
		t.Errorf("GL UA=%q 应以 workbuddy-ai/5.9.4 开头", gl["User-Agent"])
	}
}

func TestHeadersForToken(t *testing.T) {
	h := ForRealm(RealmCN).HeadersForToken()
	// 唯一没有 X-Domain 的端点
	if v, ok := h["X-Domain"]; ok {
		t.Errorf("auth/token 不该有 X-Domain，得到 %q", v)
	}
	// 两条 GET 都不带 Content-Type
	if v, ok := h["Content-Type"]; ok {
		t.Errorf("auth/token 不该有 Content-Type，得到 %q", v)
	}
	if _, ok := h["X-Custom-Channel-Id"]; ok {
		t.Error("auth/token 不该有 X-Custom-Channel-Id")
	}
	if _, ok := h["X-Device-Token"]; ok {
		t.Error("auth/token 不该有 X-Device-Token")
	}
	for _, k := range []string{"X-No-Authorization", "X-No-User-Id", "X-No-Enterprise-Id", "X-No-Department-Info"} {
		if h[k] != "true" {
			t.Errorf("%s=%q want \"true\"", k, h[k])
		}
	}
	if h["Accept"] == "" || h["User-Agent"] == "" {
		t.Error("公共头缺失")
	}
}

func TestHeadersForAccount(t *testing.T) {
	h := ForRealm(RealmGlobal).HeadersForAccount("at-1")
	if h["X-Domain"] != "www.workbuddy.ai" {
		t.Errorf("login/account 应带 X-Domain，得到 %q", h["X-Domain"])
	}
	// 已带 Bearer：唯一缺的是「账号信息还没落库」
	if h["X-No-Authorization"] != "" {
		t.Errorf("login/account 不该有 X-No-Authorization，得到 %q", h["X-No-Authorization"])
	}
	for _, k := range []string{"X-No-User-Id", "X-No-Enterprise-Id", "X-No-Department-Info"} {
		if h[k] != "true" {
			t.Errorf("%s=%q want \"true\"", k, h[k])
		}
	}
	if h["Authorization"] != "Bearer at-1" {
		t.Errorf("Authorization=%q want Bearer at-1", h["Authorization"])
	}
	// 空 token 不发 Authorization（不伪造）
	if _, ok := ForRealm(RealmCN).HeadersForAccount("")["Authorization"]; ok {
		t.Error("accessToken 为空时不该发 Authorization")
	}
}

// 每次调用都要新的 request id（真机每条一个）。
func TestHeadersRequestIDUnique(t *testing.T) {
	s := ForRealm(RealmCN)
	a, b := s.HeadersForToken()["X-Request-ID"], s.HeadersForToken()["X-Request-ID"]
	if a == b {
		t.Errorf("两条请求的 X-Request-ID 相同（%s），应每次新生成", a)
	}
}
