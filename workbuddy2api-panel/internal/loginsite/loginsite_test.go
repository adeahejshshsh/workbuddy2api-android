package loginsite

// 官方登录码语义与授权链接参数（对齐 CLI 2.174.0）。
//
// 这两样一旦错，用户看到的是「加账号一直转圈」或「登录页拿到旧版」——
// 没有可登录的真实账号时，只能靠这些表驱动用例把判据钉死。

import (
	"strings"
	"testing"
)

func TestCodePending(t *testing.T) {
	for _, c := range []int{CodeRetryFetchToken, CodeRetryFetchAccount} {
		if !CodePending(c) {
			t.Errorf("CodePending(%d)=false want true", c)
		}
		if CodeFatal(c) {
			t.Errorf("CodeFatal(%d)=true，pending 码不该被当终态", c)
		}
	}
}

func TestCodeFatal(t *testing.T) {
	for _, c := range []int{CodeLicenseSeatLimit, CodeLicenseExpired, CodeTrialExpired} {
		if !CodeFatal(c) {
			t.Errorf("CodeFatal(%d)=false want true", c)
		}
		if CodePending(c) {
			t.Errorf("CodePending(%d)=true，终态码不该继续轮询", c)
		}
	}
}

// 不在码表里的码一律**不算终态**：上游新增中间态码时，宁可多转一会儿，
// 也不要把「还没登完」误报成「账号不能用」。
func TestCodeUnknownIsNotFatal(t *testing.T) {
	for _, c := range []int{0, 1, 9999, 11218, 12006, -1} {
		if CodeFatal(c) {
			t.Errorf("CodeFatal(%d)=true，未知码不该判终态", c)
		}
		if CodePending(c) {
			t.Errorf("CodePending(%d)=true，未知码不属于已知 pending 码", c)
		}
	}
}

func TestForRealm(t *testing.T) {
	if s := ForRealm("global"); s.Realm != RealmGlobal || s.API != "https://www.workbuddy.ai" {
		t.Fatalf("global → %+v", s)
	}
	for _, r := range []string{"", "cn", "CN", "  ", "bogus"} {
		if s := ForRealm(r); s.Realm != RealmCN || s.API != "https://copilot.tencent.com" {
			t.Fatalf("ForRealm(%q) → %+v，应回落国内站", r, s)
		}
	}
}

func TestEndpoints(t *testing.T) {
	state, token, account, origin := ForRealm("cn").Endpoints()
	for _, want := range []string{"/v2/plugin/auth/state?platform=workbuddy", "/v2/plugin/auth/token?state=", "/v2/plugin/login/account?state="} {
		if !strings.Contains(state+token+account, want) {
			t.Errorf("缺端点 %q", want)
		}
	}
	if origin != "https://www.codebuddy.cn" {
		t.Errorf("cn origin=%q", origin)
	}
	// 国内站的 API 与 Origin 不同域，这是官方口径（授权页在 codebuddy.cn）。
	if strings.Contains(state, "codebuddy.cn") {
		t.Errorf("state 端点应打 API 域，不该是 Origin 域: %s", state)
	}
}

func TestDecorateAuthURLAddsVersion(t *testing.T) {
	got := DecorateAuthURL("https://example.test/login?state=abc", ForRealm("cn"), "")
	if !strings.Contains(got, "version="+ClientVersion) {
		t.Fatalf("缺 version: %s", got)
	}
	if !strings.Contains(got, "state=abc") {
		t.Fatalf("原有参数丢了: %s", got)
	}
}

// 上游已经给了 version 就不覆盖（以官方返回为准）。
func TestDecorateAuthURLKeepsExistingVersion(t *testing.T) {
	got := DecorateAuthURL("https://example.test/login?version=9.9.9", ForRealm("cn"), "")
	if strings.Contains(got, ClientVersion) {
		t.Fatalf("不该覆盖已有 version: %s", got)
	}
	if !strings.Contains(got, "version=9.9.9") {
		t.Fatalf("已有 version 被改: %s", got)
	}
}

// ww_device_nonce 只对 platform=workbuddy 的站点加，且必须由部署侧提供 —— 不伪造设备指纹。
//
// 2026-10-30 抓包修正 `?platform=` 口径后，**CN 站才是 platform=workbuddy**
// （旧实现硬编码 "CLI"）；GL 站是 workbuddy-ai，官方对它不加 nonce。
func TestDecorateAuthURLNonce(t *testing.T) {
	base := "https://example.test/login?state=abc"
	// CN 站 = platform=workbuddy → 给了 nonce 就加
	if got := DecorateAuthURL(base, ForRealm("cn"), "n-123"); !strings.Contains(got, "ww_device_nonce=n-123") {
		t.Fatalf("cn 站（platform=workbuddy）应带 nonce: %s", got)
	}
	// GL 站 = platform=workbuddy-ai → 不加
	if got := DecorateAuthURL(base, ForRealm("global"), "n-123"); strings.Contains(got, "ww_device_nonce") {
		t.Fatalf("global 站（platform=workbuddy-ai）不该带 ww_device_nonce: %s", got)
	}
	// 没给就不加（不伪造）
	if got := DecorateAuthURL(base, ForRealm("global"), ""); strings.Contains(got, "ww_device_nonce") {
		t.Fatalf("没给 nonce 不该加: %s", got)
	}
}

// 解析失败原样返回：宁可少一个参数，不要弄坏链接。
func TestDecorateAuthURLBadURLPassthrough(t *testing.T) {
	bad := "://not a url"
	if got := DecorateAuthURL(bad, ForRealm("cn"), ""); got != bad {
		t.Fatalf("坏 URL 应原样返回，got %q", got)
	}
	if got := DecorateAuthURL("", ForRealm("cn"), ""); got != "" {
		t.Fatalf("空串应原样返回，got %q", got)
	}
}
