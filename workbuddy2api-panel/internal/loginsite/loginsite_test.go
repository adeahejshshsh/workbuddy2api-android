package loginsite

import (
	"net/url"
	"regexp"
	"testing"
)

// TestFor 覆盖站点归一化：已知 id 精确命中，空/未知回落国内站（老调用零回归）。
func TestFor(t *testing.T) {
	cases := []struct {
		id        string
		wantID    string
		wantRealm string
		wantAPI   string
	}{
		{id: "", wantID: SiteCN, wantRealm: RealmCN, wantAPI: "https://copilot.tencent.com"},
		{id: "cn", wantID: SiteCN, wantRealm: RealmCN, wantAPI: "https://copilot.tencent.com"},
		{id: "CN", wantID: SiteCN, wantRealm: RealmCN, wantAPI: "https://copilot.tencent.com"},
		{id: "intl", wantID: SiteIntl, wantRealm: RealmGlobal, wantAPI: "https://www.codebuddy.ai"},
		{id: "workbuddy", wantID: SiteWorkBuddy, wantRealm: RealmGlobal, wantAPI: "https://www.workbuddy.ai"},
		// 未知值不猜域，回落国内站。
		{id: "global", wantID: SiteCN, wantRealm: RealmCN, wantAPI: "https://copilot.tencent.com"},
		{id: "codebuddy.ai", wantID: SiteCN, wantRealm: RealmCN, wantAPI: "https://copilot.tencent.com"},
	}
	for _, c := range cases {
		s := For(c.id)
		if s.ID != c.wantID || s.Realm != c.wantRealm || s.API != c.wantAPI {
			t.Errorf("For(%q)={id:%s realm:%s api:%s} want {id:%s realm:%s api:%s}",
				c.id, s.ID, s.Realm, s.API, c.wantID, c.wantRealm, c.wantAPI)
		}
	}
}

// TestForRealm 覆盖 realm → 站点映射：global 保持历史语义（旧桌面端 workbuddy.ai），
// 官方国际站必须显式 SiteIntl——这是「老脚本零回归 + 新站点可选中」的分界。
func TestForRealm(t *testing.T) {
	cases := []struct {
		realm   string
		wantID  string
		wantAPI string
	}{
		{realm: "", wantID: SiteCN, wantAPI: "https://copilot.tencent.com"},
		{realm: "cn", wantID: SiteCN, wantAPI: "https://copilot.tencent.com"},
		{realm: "global", wantID: SiteWorkBuddy, wantAPI: "https://www.workbuddy.ai"},
		{realm: "GLOBAL", wantID: SiteCN, wantAPI: "https://copilot.tencent.com"}, // 大小写不敏感未覆盖：按未知回落
	}
	for _, c := range cases {
		s := ForRealm(c.realm)
		if s.ID != c.wantID || s.API != c.wantAPI {
			t.Errorf("ForRealm(%q)={id:%s api:%s} want {id:%s api:%s}", c.realm, s.ID, s.API, c.wantID, c.wantAPI)
		}
	}
}

// TestEndpoints 覆盖三端点拼接：必须同域且 platform 取站点自己的值。
// Origin 与 API 同域是国际站的约束（跨域 Origin 会被上游按非法请求拒绝）；
// 国内站是**已知例外**——API 在 copilot.tencent.com、Origin 在 www.codebuddy.cn
// （与官方客户端一致）。
//
// platform 取值来自各分发包 product.json 的 authentication.attributes.platform：
// CN=workbuddy（真机实测）、workbuddy.ai=workbuddy-ai（真机实测）、codebuddy.ai=CLI。
func TestEndpoints(t *testing.T) {
	for _, id := range []string{SiteCN, SiteIntl, SiteWorkBuddy} {
		s := For(id)
		state, token, account, origin := s.Endpoints()
		if want := s.API + "/v2/plugin/auth/state?platform=" + s.Platform; state != want {
			t.Errorf("%s state=%q want %q", id, state, want)
		}
		if want := s.API + "/v2/plugin/auth/token?state="; token != want {
			t.Errorf("%s token=%q want %q", id, token, want)
		}
		if want := s.API + "/v2/plugin/login/account?state="; account != want {
			t.Errorf("%s account=%q want %q", id, account, want)
		}
		apiHost := mustHost(t, s.API)
		originHost := mustHost(t, origin)
		if id == SiteCN {
			// 国内站跨域是既定形态，锁死具体值防被「统一同域」误改。
			if apiHost != "copilot.tencent.com" || originHost != "www.codebuddy.cn" {
				t.Errorf("cn 站点域被改动：api=%s origin=%s", apiHost, originHost)
			}
			continue
		}
		if apiHost != originHost {
			t.Errorf("%s Origin 与 API 不同域：api=%s origin=%s", id, apiHost, originHost)
		}
	}
}

func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Host
}

// TestCodeSemantics 覆盖官方登录码分类：只有 11217/12151 是 pending，
// 12005/11212/11216 是终态。混类会让「席位满」白等满 TTL 才报超时。
func TestCodeSemantics(t *testing.T) {
	pending := []int{CodeRetryFetchToken, CodeRetryFetchAccount}
	for _, c := range pending {
		if !CodePending(c) {
			t.Errorf("CodePending(%d)=false want true", c)
		}
		if CodeFatal(c) {
			t.Errorf("CodeFatal(%d)=true want false（pending 不是终态）", c)
		}
	}
	fatal := []int{CodeLicenseSeatLimit, CodeLicenseExpired, CodeTrialExpired}
	for _, c := range fatal {
		if !CodeFatal(c) {
			t.Errorf("CodeFatal(%d)=false want true", c)
		}
		if CodePending(c) {
			t.Errorf("CodePending(%d)=true want false（终态不该继续轮询）", c)
		}
	}
	// 其它码两类都不属（调用方按「可重试错误」处理）。
	for _, c := range []int{0, 10001, 11102, 14051, 99999} {
		if CodePending(c) || CodeFatal(c) {
			t.Errorf("code %d 不该被归入 pending 或 fatal", c)
		}
	}
}

// TestDecorateAuthURL 覆盖授权链接参数补齐（对齐官方 appendHostLoginJourneyParams /
// appendAuthUrlParams）：
//
//   - version：所有已复核站点都补（CN 5.9.3 / GL 5.9.4）——真机两侧链接都带它；
//     CN 授权页还会把它转给 console/auth/risk-context。
//   - loginSessionId：与 nonce 同一道闸，只对 platform=workbuddy（CN）补；真机国际版
//     链接（platform=workbuddy-ai）没有这个参数。
//   - ww_device_nonce：只对 platform=workbuddy 的站点且 nonce 非空时补。
func TestDecorateAuthURL(t *testing.T) {
	const base = "https://example.com/login?platform=CLI&state=abc"
	const sid = "caf6af35-8aa2-4037-89e8-c8a70ffbda5a"

	// CN 补 version + loginSessionId；原有参数一律不动。
	got := DecorateAuthURL(base, For(SiteCN), "", sid)
	if !containsParam(t, got, "version", ClientVersionWorkBuddyCN) {
		t.Errorf("cn 站未补 version: %s", got)
	}
	if !containsParam(t, got, "loginSessionId", sid) {
		t.Errorf("cn 站未补 loginSessionId: %s", got)
	}
	// 国际版：补 version（5.9.4），但不补 loginSessionId（真机链接里没有）。
	for _, id := range []string{SiteIntl, SiteWorkBuddy} {
		got := DecorateAuthURL(base, For(id), "", sid)
		u, err := url.Parse(got)
		if err != nil {
			t.Fatalf("%s: parse %q: %v", id, got, err)
		}
		if v := u.Query().Get("version"); v != ClientVersionWorkBuddyGL {
			t.Errorf("%s version=%q want %q", id, v, ClientVersionWorkBuddyGL)
		}
		if containsParam(t, got, "loginSessionId", "") {
			t.Errorf("%s 不该补 loginSessionId（真机 GL 链接没有）: %s", id, got)
		}
		if s := u.Query().Get("state"); s != "abc" {
			t.Errorf("%s state=%q want abc（原有参数被破坏）", id, s)
		}
		if p := u.Query().Get("platform"); p != "CLI" {
			t.Errorf("%s platform=%q want CLI（授权 URL 自带参数不得被改）", id, p)
		}
	}

	// platform=workbuddy 的站点（= 国内桌面端）带 nonce。
	if got := DecorateAuthURL(base, For(SiteCN), "nonce-1", ""); !containsParam(t, got, "ww_device_nonce", "nonce-1") {
		t.Errorf("cn 站（platform=workbuddy）未补 ww_device_nonce: %s", got)
	}
	for _, id := range []string{SiteIntl, SiteWorkBuddy} {
		if got := DecorateAuthURL(base, For(id), "nonce-1", ""); containsParam(t, got, "ww_device_nonce", "") {
			t.Errorf("%s 站不该补 ww_device_nonce: %s", id, got)
		}
	}
	// nonce 为空时一律不补（不伪造设备指纹）。
	if got := DecorateAuthURL(base, For(SiteCN), "", ""); containsParam(t, got, "ww_device_nonce", "") {
		t.Errorf("nonce 为空时不该补 ww_device_nonce: %s", got)
	}

	// 上游已带的值不覆盖（尊重上游口径）。
	if got := DecorateAuthURL(base+"&version=9.9.9&loginSessionId=up", For(SiteCN), "", sid); !containsParam(t, got, "version", "9.9.9") || !containsParam(t, got, "loginSessionId", "up") {
		t.Errorf("已有参数被覆盖: %s", got)
	}

	// 非法 URL 原样返回（不阻断登录）。
	if got := DecorateAuthURL("://bad", For(SiteCN), "", sid); got != "://bad" {
		t.Errorf("非法 URL 未原样返回: %q", got)
	}
}

// NewLoginSessionID 必须是 uuid v4 形态且每次不同（授权页会原样透传进回调地址）。
func TestNewLoginSessionID(t *testing.T) {
	a, b := NewLoginSessionID(), NewLoginSessionID()
	if a == "" || a == b {
		t.Fatalf("loginSessionId 应非空且每次不同: %q / %q", a, b)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(a) {
		t.Fatalf("不是 uuid v4 形态: %q", a)
	}
}

func containsParam(t *testing.T, raw, key, want string) bool {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	v, ok := u.Query()[key]
	if want == "" {
		return ok
	}
	return ok && len(v) == 1 && v[0] == want
}

// TestBaseForDomain 覆盖按 auth 文件 domain 选上游 base：国际域两套主机名并存，
// 用 realm 决定 base 会把一半账号打到非签发域。
func TestBaseForDomain(t *testing.T) {
	cases := []struct {
		domain string
		want   string
	}{
		{domain: "www.codebuddy.ai", want: BaseOfficial},
		{domain: "codebuddy.ai", want: BaseOfficial},
		{domain: "api.codebuddy.ai", want: BaseOfficial},
		{domain: "www.workbuddy.ai", want: BaseWorkBuddy},
		{domain: "workbuddy.ai", want: BaseWorkBuddy},
		{domain: "WWW.CODEBUDDY.AI", want: BaseOfficial}, // 大小写不敏感
		{domain: " www.codebuddy.ai ", want: BaseOfficial},
		// 国内域/空/非法 → 空串（调用方回落配置或默认，不猜域）。
		{domain: "", want: ""},
		{domain: "www.codebuddy.cn", want: ""},
		{domain: "copilot.tencent.com", want: ""},
		{domain: "evil-codebuddy.ai", want: ""}, // 后缀匹配不能误吞
	}
	for _, c := range cases {
		if got := BaseForDomain(c.domain); got != c.want {
			t.Errorf("BaseForDomain(%q)=%q want %q", c.domain, got, c.want)
		}
	}
}

// TestValidID 覆盖站点标识校验（CLI flag 解析用）。
func TestValidID(t *testing.T) {
	for _, ok := range []string{"cn", "intl", "workbuddy", "CN", " Intl "} {
		if !ValidID(ok) {
			t.Errorf("ValidID(%q)=false want true", ok)
		}
	}
	for _, bad := range []string{"", "global", "codebuddy", "www.codebuddy.ai"} {
		if ValidID(bad) {
			t.Errorf("ValidID(%q)=true want false", bad)
		}
	}
}

// TestClientUA 覆盖 UA 与 version 同源（升级版本号时两处不能漂移）。
func TestClientUA(t *testing.T) {
	want := "CLI/" + ClientVersion + " CodeBuddy/" + ClientVersion
	if ClientUA != want {
		t.Errorf("ClientUA=%q want %q", ClientUA, want)
	}
}

// TestEndpointHeaders 设备码三端点的出站头族必须与真机一致：
// 桌面端 RPC 族 UA、宽松 Accept、X-Product，且**不含**浏览器痕迹。
//
// 2026-10-30 双版本抓包：auth/state、auth/token、login/account 三条由桌面端
// node fetch 发出，逐条确认无 Origin / Referer / X-Requested-With；
// UA 是 WorkBuddy 桌面端 RPC 族，不是 `CLI/<ver> CodeBuddy/<ver>`。
//
// 三条端点的差异（x-domain / x-no-* / x-device-token / Bearer）见下面各自的子测试。
func TestEndpointHeaders(t *testing.T) {
	wantGLUA := "workbuddy-ai/" + ClientVersionWorkBuddyGL + " workbuddy-ai/" + ClientVersionWorkBuddyGL + " CLI/" + ClientVersionBuiltinCLIGL
	wantCNUA := "WorkBuddy/" + ClientVersionWorkBuddyCN + " WorkBuddy/" + ClientVersionWorkBuddyCN + " CLI/" + ClientVersionBuiltinCLICN

	cases := []struct {
		site   Site
		wantUA string
	}{
		{For(SiteCN), wantCNUA},
		{For(SiteIntl), wantGLUA},
		{For(SiteWorkBuddy), wantGLUA},
	}
	for _, c := range cases {
		h := c.site.EndpointHeaders()
		if got := h["User-Agent"]; got != c.wantUA {
			t.Errorf("%s User-Agent=%q want %q", c.site.ID, got, c.wantUA)
		}
		if got := h["X-Product"]; got != "SaaS" {
			t.Errorf("%s X-Product=%q want SaaS", c.site.ID, got)
		}
		if got := h["Accept"]; got != "application/json, text/plain, */*" {
			t.Errorf("%s Accept=%q want 宽松值", c.site.ID, got)
		}
		if got := h["X-Request-ID"]; len(got) != 32 {
			t.Errorf("%s X-Request-ID=%q want 32 hex", c.site.ID, got)
		}
		for _, k := range []string{"Origin", "Referer", "X-Requested-With"} {
			if v, ok := h[k]; ok {
				t.Errorf("%s 不应出现 %s=%q（真机 node fetch 无此头）", c.site.ID, k, v)
			}
		}
	}
}

// TestLoginEndpointHeaderVariants 三条端点的差异头（真机逐条核对）。
//
//	auth/state       x-domain + 4 个 x-no-* + x-custom-channel-id + x-device-token
//	auth/token       4 个 x-no-*（**唯一没有 x-domain 的端点**）
//	login/account    x-domain + Bearer + 3 个 x-no-*（无 x-no-authorization）
func TestLoginEndpointHeaderVariants(t *testing.T) {
	s := For(SiteCN)

	st := s.HeadersForState("v3:abc")
	for _, k := range []string{"X-Domain", "X-No-Authorization", "X-No-User-Id",
		"X-No-Enterprise-Id", "X-No-Department-Info", "X-Custom-Channel-Id", "X-Device-Token"} {
		if st[k] == "" {
			t.Errorf("auth/state 缺 %s", k)
		}
	}
	if got := st["X-Domain"]; got != "www.workbuddy.cn" {
		t.Errorf("auth/state X-Domain=%q want www.workbuddy.cn（前端主机名，非 API base）", got)
	}
	// 设备 token 取不到时**不发**该头（不伪造）。
	if got := s.HeadersForState("")["X-Device-Token"]; got != "" {
		t.Errorf("无设备 token 时不应发 X-Device-Token，实际 %q", got)
	}

	tk := s.HeadersForToken()
	if got := tk["X-Domain"]; got != "" {
		t.Errorf("auth/token 真机无 X-Domain，实际 %q", got)
	}
	for _, k := range []string{"X-No-Authorization", "X-No-User-Id", "X-No-Enterprise-Id", "X-No-Department-Info"} {
		if tk[k] != "true" {
			t.Errorf("auth/token %s=%q want true", k, tk[k])
		}
	}
	if tk["X-Device-Token"] != "" || tk["X-Custom-Channel-Id"] != "" {
		t.Errorf("auth/token 不应带设备头: %v", tk)
	}

	ac := s.HeadersForAccount("AT")
	if got := ac["Authorization"]; got != "Bearer AT" {
		t.Errorf("login/account Authorization=%q want Bearer AT", got)
	}
	if got := ac["X-Domain"]; got != "www.workbuddy.cn" {
		t.Errorf("login/account X-Domain=%q want www.workbuddy.cn", got)
	}
	if ac["X-No-Authorization"] != "" {
		t.Errorf("login/account 已带 Bearer，不应发 X-No-Authorization")
	}
	for _, k := range []string{"X-No-User-Id", "X-No-Enterprise-Id", "X-No-Department-Info"} {
		if ac[k] != "true" {
			t.Errorf("login/account %s=%q want true", k, ac[k])
		}
	}
}
