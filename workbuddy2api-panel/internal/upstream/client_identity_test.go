// client_identity_test.go 钉住 issue #257 的隔离口径：5.11.3 桌面端新增的
// X-Client-Version / X-Client-Platform / X-Client-Product 只发给国内版，国际版一律不发；
// 小程序口径（growth accept / claim）连国内版也不带这三头。
//
// 为什么必须有这条测试：两侧分发包不同（CN 5.11.3 / GL 5.9.4），国际版 app.asar 里
// 没有 proxy-env2 的 registerOutboundHeaderProvider，官方国际客户端从不发这三个头。
// 一旦有人把注入挪进 realm 无关的公共路径，国际链路就会多出一套官方没有的指纹，
// 而 official_fixture 那条对拍只覆盖 CN（且默认跳过），看不出来——所以这里对每个头
// 构造函数各断言一次两侧，并对小程序口径单独断言一次「撤干净」。
package upstream

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

var clientIdentityHeaders = []string{"X-Client-Version", "X-Client-Platform", "X-Client-Product"}

func cnProbeAuth() *auth.Auth {
	return &auth.Auth{UID: "u-cn", AccessToken: "AT", RefreshToken: "RT", Nickname: "cn"}
}

func globalProbeAuth() *auth.Auth {
	return &auth.Auth{UID: "u-gl", AccessToken: "AT", RefreshToken: "RT", Nickname: "gl",
		Domain: "www.workbuddy.ai"}
}

// builders 覆盖所有会打到首方 endpoint 的头构造函数（含手搓头的那几条路径）。
var builders = []struct {
	name string
	set  func(c *Client, req *http.Request, a *auth.Auth)
}{
	{"CommonHeaders", func(c *Client, req *http.Request, a *auth.Auth) { c.CommonHeaders(req, a) }},
	{"ChatHeaders", func(c *Client, req *http.Request, a *auth.Auth) { c.ChatHeaders(req, a, "", ChatMeta{}) }},
	{"BillingHeaders", func(c *Client, req *http.Request, a *auth.Auth) { c.BillingHeaders(req, a) }},
	{"RefreshHeaders", func(c *Client, req *http.Request, a *auth.Auth) { c.RefreshHeaders(req, a) }},
	{"busyHeaders", func(c *Client, req *http.Request, a *auth.Auth) { c.busyHeaders(req, a) }},
}

func TestClientIdentityRealmIsolation(t *testing.T) {
	c := New()
	req := func() *http.Request {
		return httptest.NewRequest("POST", "https://www.workbuddy.cn/v2/report", nil)
	}
	for _, b := range builders {
		t.Run(b.name, func(t *testing.T) {
			cn := req()
			b.set(c, cn, cnProbeAuth())
			for _, h := range clientIdentityHeaders {
				if cn.Header.Get(h) == "" {
					t.Errorf("%s 国内版缺 %s", b.name, h)
				}
			}
			if got := cn.Header.Get("X-Client-Version"); got != defaultClientVersion {
				t.Errorf("国内版 X-Client-Version=%q want %q", got, defaultClientVersion)
			}
			if got := cn.Header.Get("X-Client-Platform"); got != "windows" {
				t.Errorf("国内版 X-Client-Platform=%q want windows", got)
			}
			if got := cn.Header.Get("X-Client-Product"); got != "WorkBuddy" {
				t.Errorf("国内版 X-Client-Product=%q want WorkBuddy", got)
			}

			gl := req()
			b.set(c, gl, globalProbeAuth())
			for _, h := range clientIdentityHeaders {
				if got := gl.Header.Get(h); got != "" {
					t.Errorf("%s 国际版不该发 %s，实际=%q（两侧头集必须隔离）", b.name, h, got)
				}
			}
		})
	}
}

// TestClientIdentityNilAuth 匿名请求（无账号）不注入：没有 realm 就没有版本语义。
func TestClientIdentityNilAuth(t *testing.T) {
	c := New()
	req := httptest.NewRequest("POST", "https://www.workbuddy.cn/v2/report", nil)
	c.CommonHeaders(req, nil)
	for _, h := range clientIdentityHeaders {
		if got := req.Header.Get(h); got != "" {
			t.Errorf("nil 账号不该发 %s，实际=%q", h, got)
		}
	}
}

// TestClientIdentityFollowsOverride 三个头随 config 的 client_version 覆盖一起走，
// 不允许留一个「头里报 5.11.3、UA 里报别的」的错位版本。
func TestClientIdentityFollowsOverride(t *testing.T) {
	c := New()
	c.ClientVersion = "5.9.3"
	req := httptest.NewRequest("POST", "https://www.workbuddy.cn/v2/report", nil)
	c.CommonHeaders(req, cnProbeAuth())
	if got := req.Header.Get("X-Client-Version"); got != "5.9.3" {
		t.Errorf("X-Client-Version=%q want 覆盖值 5.9.3", got)
	}
	if ua := req.Header.Get("User-Agent"); ua != "WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/"+defaultCliVersion {
		t.Errorf("UA=%q 与 X-Client-Version 不一致", ua)
	}
}

// TestClientIdentityDesktopReport 手搓头的 /v2/report 也必须带上（真机 7/7）。
func TestClientIdentityDesktopReport(t *testing.T) {
	var cnHdr, glHdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-User-Id") == "u-cn" {
			cnHdr = r.Header.Clone()
		} else {
			glHdr = r.Header.Clone()
		}
		w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.ChatBaseCN = srv.URL
	c.BillingBaseCN = srv.URL
	c.WebBaseCN = srv.URL
	// 国际版走另一组 base：不指过来就会真打 www.workbuddy.ai（外网）。
	c.ChatBaseGlobal = srv.URL
	c.BillingBaseGlobal = srv.URL
	if err := c.ReportDesktopEvent(cnProbeAuth(), DesktopEvent{"eventCode": "appearance_skin_apply"}); err != nil {
		t.Fatalf("CN report: %v", err)
	}
	if err := c.ReportDesktopEvent(globalProbeAuth(), DesktopEvent{"eventCode": "appearance_skin_apply"}); err != nil {
		t.Fatalf("GL report: %v", err)
	}
	if cnHdr == nil || glHdr == nil {
		t.Fatalf("没捕获到两侧请求 cn=%v gl=%v", cnHdr != nil, glHdr != nil)
	}
	for _, h := range clientIdentityHeaders {
		if cnHdr.Get(h) == "" {
			t.Errorf("CN /v2/report 缺 %s", h)
		}
		if got := glHdr.Get(h); got != "" {
			t.Errorf("GL /v2/report 不该发 %s，实际=%q", h, got)
		}
	}
}

// TestClientIdentityNotOnMPPaths 小程序口径的请求只带平台段，不带桌面端那三头。
//
// 这些路径（growth 任务 accept / claim）先调 BillingHeaders（CN 号会把
// X-Client-Version/Platform/Product 打上），再覆盖 X-Client-Platform: miniprogram。
// 不撤三头就会发出 `5.11.3 / miniprogram / WorkBuddy`：桌面端不会说 miniprogram，
// 小程序也不会自报 5.11.3 与 WorkBuddy（真机小程序是 2.4.0 / mp-weixin / workbuddy-mp）。
func TestClientIdentityNotOnMPPaths(t *testing.T) {
	var mu sync.Mutex
	var seen []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		mu.Unlock()
		if strings.Contains(r.URL.Path, "/claim") {
			w.Write([]byte(`{"code":0,"msg":"OK","data":{"already_claimed":false,"credit":100,"energy":5}}`))
			return
		}
		w.Write([]byte(`{"code":0,"msg":"OK","data":[{"task_code":"chat_5","current":1,"target":1}]}`))
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.ChatBaseCN = srv.URL
	c.BillingBaseCN = srv.URL
	c.WebBaseCN = srv.URL
	a := cnProbeAuth()
	if err := c.AcceptTasksMP(a, []string{"Sequential_Tasks_1"}); err != nil {
		t.Fatalf("AcceptTasksMP: %v", err)
	}
	if _, _, _, err := c.ClaimRewardMP(a, "Sequential_Tasks_1"); err != nil {
		t.Fatalf("ClaimRewardMP: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("应发两次上游请求，实际 %d", len(seen))
	}
	for i, h := range seen {
		if got := h.Get("X-Client-Platform"); got != mpPlatform {
			t.Errorf("第 %d 次请求 X-Client-Platform=%q want %q", i+1, got, mpPlatform)
		}
		for _, k := range []string{"X-Client-Version", "X-Client-Product"} {
			if got := h.Get(k); got != "" {
				t.Errorf("第 %d 次请求（小程序口径）不该带 %s，实际=%q", i+1, k, got)
			}
		}
	}
}
