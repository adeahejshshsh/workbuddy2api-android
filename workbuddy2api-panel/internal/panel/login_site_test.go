package panel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/loginsite"
)

// withTestSite 把站点解析临时指向 httptest 服务器（避免单测打真实上游），
// 测试结束恢复。返回的站点保留原 realm/ID/platform，仅替换 API 与 Origin。
func withTestSite(t *testing.T, base string, id string) {
	t.Helper()
	orig := siteResolver
	site := loginsite.For(id)
	site.API = base
	site.Origin = base
	siteResolver = func(want string) loginsite.Site {
		if loginsite.ValidID(want) && strings.EqualFold(want, id) {
			return site
		}
		if loginsite.ValidID(want) {
			// 其它站点也指向同一测试服务器（handler 只看是否命中测试 host）。
			other := loginsite.For(want)
			other.API = base
			other.Origin = base
			return other
		}
		return site
	}
	t.Cleanup(func() { siteResolver = orig })
}

// TestLoginStartRecordsSiteAndDecoratesURL 覆盖 login/start：返回的授权链接补
// version（真机 GL 链接带 version=5.9.4、不带 loginSessionId），且会话记录 site
// （poll 据此选端点，防止 codebuddy.ai 与 workbuddy.ai 混用）。
func TestLoginStartRecordsSiteAndDecoratesURL(t *testing.T) {
	var gotPlatform string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v2/plugin/auth/state") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		gotPlatform = r.URL.Query().Get("platform")
		base := "http://" + r.Host
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"code":0,"msg":"OK","data":{"state":"st-intl","authUrl":"%s/login?platform=CLI&state=st-intl"}}`, base)
	}))
	defer srv.Close()

	withTestSite(t, srv.URL, loginsite.SiteIntl)

	p := New(Config{})
	rec := httptest.NewRecorder()
	p.loginStart(rec, httptest.NewRequest("POST", "/panel/api/login/start",
		strings.NewReader(`{"site":"intl"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v (%s)", err, rec.Body)
	}
	if resp["site"] != "intl" {
		t.Errorf("site=%v want intl", resp["site"])
	}
	if resp["realm"] != "global" {
		t.Errorf("realm=%v want global", resp["realm"])
	}
	rawURL, _ := resp["url"].(string)
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("解析授权 URL 失败 %q: %v", rawURL, err)
	}
	if v := u.Query().Get("version"); v != loginsite.ClientVersionWorkBuddyGL {
		t.Errorf("授权链接 version=%q want %q：%q", v, loginsite.ClientVersionWorkBuddyGL, rawURL)
	}
	if s := u.Query().Get("loginSessionId"); s != "" {
		t.Errorf("国际版链接不该带 loginSessionId（真机 GL 链接没有）：%q", rawURL)
	}
	if gotPlatform != "workbuddy-ai" {
		t.Errorf("platform=%q want workbuddy-ai（intl 站点真机口径）", gotPlatform)
	}

	// 会话必须记住站点，否则 poll 会按 cn 端点去问，永远拿不到 token。
	p.loginMu.Lock()
	sess, ok := p.logins["st-intl"]
	p.loginMu.Unlock()
	if !ok {
		t.Fatal("state 未记录")
	}
	if sess.site != "intl" || sess.realm != "global" {
		t.Errorf("session={site:%s realm:%s} want {intl global}", sess.site, sess.realm)
	}
}

// TestLoginStartLegacyRealm 覆盖老前端兼容：只传 {"realm":"global"} 时映射到旧桌面端
// 站点（workbuddy.ai），保持历史语义不变。
func TestLoginStartLegacyRealm(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		base := "http://" + r.Host
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"code":0,"msg":"OK","data":{"state":"st-wb","authUrl":"%s/login?state=st-wb"}}`, base)
	}))
	defer srv.Close()

	withTestSite(t, srv.URL, loginsite.SiteWorkBuddy)

	p := New(Config{})
	rec := httptest.NewRecorder()
	p.loginStart(rec, httptest.NewRequest("POST", "/panel/api/login/start",
		strings.NewReader(`{"realm":"global"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["site"] != "workbuddy" {
		t.Errorf("site=%v want workbuddy（realm=global 兼容映射）", resp["site"])
	}
	if !strings.HasSuffix(gotPath, "/v2/plugin/auth/state") {
		t.Errorf("path=%q", gotPath)
	}
}

// TestLoginPollFatalLicenseCode 覆盖许可类终态码：上游回 12005（席位满）时必须
// 立即返回 fatal=true + 上游 msg，并清掉会话（不再让前端每 3s 空转到 TTL）。
func TestLoginPollFatalLicenseCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":12005,"msg":"license seat limit reached"}`)
	}))
	defer srv.Close()

	withTestSite(t, srv.URL, loginsite.SiteCN)

	p := New(Config{})
	p.logins = map[string]loginSession{"st1": {created: time.Now(), realm: "cn", site: "cn"}}
	rec := httptest.NewRecorder()
	p.loginPoll(rec, httptest.NewRequest("GET", "/panel/api/login/poll?state=st1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad json: %v (%s)", err, rec.Body)
	}
	if got["fatal"] != true {
		t.Fatalf("fatal=%v want true（12005 应终态，不该继续轮询）: %s", got["fatal"], rec.Body)
	}
	if code, _ := got["code"].(float64); int(code) != 12005 {
		t.Errorf("code=%v want 12005", got["code"])
	}
	if msg, _ := got["message"].(string); !strings.Contains(msg, "license seat limit reached") {
		t.Errorf("message=%q 应透出上游原话", msg)
	}
	// 会话已作废：后续 poll 应 410（前端停止轮询）。
	p.loginMu.Lock()
	_, still := p.logins["st1"]
	p.loginMu.Unlock()
	if still {
		t.Error("终态后会话未清除")
	}
}

// TestLoginPollPendingCodeKeepsPolling 覆盖 pending 码：11217（还没登完）必须
// done=false 且**不**标 fatal，否则用户还没在浏览器点完就被判失败。
func TestLoginPollPendingCodeKeepsPolling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":11217,"msg":"login ing..."}`)
	}))
	defer srv.Close()

	withTestSite(t, srv.URL, loginsite.SiteCN)

	p := New(Config{})
	p.logins = map[string]loginSession{"st2": {created: time.Now(), realm: "cn", site: "cn"}}
	rec := httptest.NewRecorder()
	p.loginPoll(rec, httptest.NewRequest("GET", "/panel/api/login/poll?state=st2", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad json: %v (%s)", err, rec.Body)
	}
	if got["done"] != false {
		t.Errorf("done=%v want false", got["done"])
	}
	if got["fatal"] == true {
		t.Errorf("11217 是 pending，不该判终态：%s", rec.Body)
	}
	// 会话保留，前端可继续轮询。
	p.loginMu.Lock()
	_, still := p.logins["st2"]
	p.loginMu.Unlock()
	if !still {
		t.Error("pending 时不该清会话")
	}
}

// TestLogPollOnceDedupes 覆盖轮询日志节流：同一上游结果只记一行（前端 3s 一轮，
// 逐轮落盘会把 15 分钟刷成 300 行），结果变化后再记一行，会话消失后不再记。
func TestLogPollOnceDedupes(t *testing.T) {
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })

	p := New(Config{})
	p.logins = map[string]loginSession{"st-abcdef12": {created: time.Now(), realm: "cn", site: "cn"}}

	lines := func() int { return strings.Count(buf.String(), "登录轮询") }

	p.logPollOnce("st-abcdef12", "code=11217 msg=login ing...")
	p.logPollOnce("st-abcdef12", "code=11217 msg=login ing...")
	if got := lines(); got != 1 {
		t.Fatalf("同一上游结果记了 %d 行，want 1: %q", got, buf.String())
	}
	if !strings.Contains(buf.String(), "st-abcde...") {
		t.Errorf("日志缺 state 前缀（定位要用）: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "login ing...") {
		t.Errorf("日志缺上游原文: %q", buf.String())
	}

	p.logPollOnce("st-abcdef12", "http_error: upstream 502")
	if got := lines(); got != 2 {
		t.Fatalf("上游结果变化后应再记一行，got %d: %q", got, buf.String())
	}

	// 会话已清（超时 / 已落盘）：迟到的一次 poll 不该再记。
	delete(p.logins, "st-abcdef12")
	p.logPollOnce("st-abcdef12", "code=11217 msg=late")
	if got := lines(); got != 2 {
		t.Fatalf("会话消失后仍记了日志，got %d: %q", got, buf.String())
	}
}
