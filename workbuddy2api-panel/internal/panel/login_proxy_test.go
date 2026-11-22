package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/loginsite"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/proxypool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// loginFakeUpstream 伪造设备授权三端点（state / token / account）。其它路径一律回业务
// 错误，让登录后的签到/余额分支走「失败也不影响登录」的分支，不去碰真网。
func loginFakeUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		base := "http://" + r.Host
		switch {
		case strings.HasSuffix(r.URL.Path, "/v2/plugin/auth/state"):
			fmt.Fprintf(w, `{"code":0,"msg":"OK","data":{"state":"st-proxy","authUrl":"%s/login?state=st-proxy"}}`, base)
		case strings.HasSuffix(r.URL.Path, "/v2/plugin/auth/token"):
			fmt.Fprint(w, `{"code":0,"msg":"OK","data":{"accessToken":"at-1","refreshToken":"rt-1","expiresIn":3600}}`)
		case strings.HasSuffix(r.URL.Path, "/v2/plugin/login/account"):
			fmt.Fprint(w, `{"code":0,"msg":"OK","data":{"uid":"uid-proxy-1","nickname":"newbie"}}`)
		default:
			fmt.Fprint(w, `{"code":50000,"msg":"unsupported in test"}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// loginFakeProxy 是一个只会转发的 HTTP 代理，并记下被用了几次。
func loginFakeProxy(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !r.URL.IsAbs() {
			t.Errorf("不是代理请求（缺绝对 URI）: %s %s", r.Method, r.URL)
			http.Error(w, "not a proxy request", http.StatusBadRequest)
			return
		}
		atomic.AddInt32(&hits, 1)
		out, err := http.NewRequest(r.Method, r.URL.String(), r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		out.Header = r.Header.Clone()
		resp, err := http.DefaultTransport.RoundTrip(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// newLoginProxyEnv 装配「假上游 + 假代理 + 真号池 + 面板」。
func newLoginProxyEnv(t *testing.T) (p *Panel, m *proxypool.Manager, nodeID string, hits *int32) {
	t.Helper()
	up := loginFakeUpstream(t)
	proxy, hitCounter := loginFakeProxy(t)

	m, err := proxypool.New(filepath.Join(t.TempDir(), "proxy-pool.json"), nil, nil)
	if err != nil {
		t.Fatalf("proxy pool: %v", err)
	}
	t.Cleanup(m.Close)
	res, err := m.Import(proxy.URL)
	if err != nil || len(res) != 1 || res[0].ID == "" {
		t.Fatalf("import proxy: %v %+v", err, res)
	}
	nodeID = res[0].ID

	loginHTTP = &http.Client{Timeout: 5 * time.Second}
	t.Cleanup(func() { loginHTTP = &http.Client{Timeout: 30 * time.Second} })

	withTestSite(t, up.URL, loginsite.SiteCN)

	p = New(Config{
		Pool:      pool.New(""),
		ProxyPool: m,
		AuthDir:   filepath.Join(t.TempDir(), "auths"),
		Upstream: &upstream.Client{
			HTTP:          &http.Client{Timeout: 5 * time.Second},
			ChatBaseCN:    up.URL,
			BillingBaseCN: up.URL,
			WebBaseCN:     up.URL,
		},
	})
	return p, m, nodeID, hitCounter
}

// TestLoginStartAndPollBindPoolProxy 覆盖整条加号链路：
//
//	login/start  → 出口已从号池里挑定，auth/state 经代理发出（不是本机直连）
//	login/poll   → token/account 复用同一出口；账号落盘后写成「固定绑定」
//
// 这条是特性的全部承诺：加号的 IP 与之后做任务的 IP 是同一条。
func TestLoginStartAndPollBindPoolProxy(t *testing.T) {
	p, m, nodeID, hits := newLoginProxyEnv(t)

	rec := httptest.NewRecorder()
	p.loginStart(rec, httptest.NewRequest("POST", "/panel/api/login/start", strings.NewReader(`{"site":"cn"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("login/start code=%d body=%s", rec.Code, rec.Body)
	}
	var started map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatalf("bad json: %v (%s)", err, rec.Body)
	}
	if label, _ := started["proxy"].(string); label == "" || label == "直连" {
		t.Fatalf("login/start 的出口回显应为代理，got %q (%s)", label, rec.Body)
	}
	if got := atomic.LoadInt32(hits); got == 0 {
		t.Fatal("auth/state 没有经过代理（走到了本机直连）")
	}

	p.loginMu.Lock()
	sess, ok := p.logins["st-proxy"]
	p.loginMu.Unlock()
	if !ok {
		t.Fatal("state 未记录")
	}
	if sess.proxyID != nodeID {
		t.Fatalf("会话里的出口 proxyID=%q want %q", sess.proxyID, nodeID)
	}

	after := atomic.LoadInt32(hits)
	rec = httptest.NewRecorder()
	p.loginPoll(rec, httptest.NewRequest("GET", "/panel/api/login/poll?state=st-proxy", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("login/poll code=%d body=%s", rec.Code, rec.Body)
	}
	var done map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &done); err != nil {
		t.Fatalf("bad json: %v (%s)", err, rec.Body)
	}
	if done["done"] != true {
		t.Fatalf("登录未完成: %s", rec.Body)
	}
	if got := atomic.LoadInt32(hits); got <= after {
		t.Fatal("token/account 没有经过代理（换回了直连）")
	}

	snap := m.Snapshot()
	b, bound := snap.Bindings["uid-proxy-1"]
	if !bound || b.Mode != "fixed" || b.ProxyID != nodeID {
		t.Fatalf("账号没被钉在登录出口上: %+v（want fixed %s）", b, nodeID)
	}
	if label, _ := done["proxy"].(string); label == "" {
		t.Errorf("完成响应应回显绑定结果: %s", rec.Body)
	}
}

// TestLoginRouteDirectWhenPoolEmpty 没有节点可挑时必须回落直连，而不是把加号卡死：
// 没配代理池的部署照旧能加号（这也是旧行为的零回归路径）。
func TestLoginRouteDirectWhenPoolEmpty(t *testing.T) {
	up := loginFakeUpstream(t)
	withTestSite(t, up.URL, loginsite.SiteCN)

	m, err := proxypool.New(filepath.Join(t.TempDir(), "proxy-pool.json"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)

	loginHTTP = &http.Client{Timeout: 5 * time.Second}
	t.Cleanup(func() { loginHTTP = &http.Client{Timeout: 30 * time.Second} })

	p := New(Config{Pool: pool.New(""), ProxyPool: m, AuthDir: filepath.Join(t.TempDir(), "auths")})
	rec := httptest.NewRecorder()
	p.loginStart(rec, httptest.NewRequest("POST", "/panel/api/login/start", strings.NewReader(`{"site":"cn"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("空池登录应可用，code=%d body=%s", rec.Code, rec.Body)
	}
	var started map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &started)
	if label, _ := started["proxy"].(string); label != "直连" {
		t.Fatalf("空池出口回显应为 直连，got %q", label)
	}
	p.loginMu.Lock()
	sess := p.logins["st-proxy"]
	p.loginMu.Unlock()
	if sess.proxyID != "" {
		t.Fatalf("空池不该挑出节点，got %q", sess.proxyID)
	}
}

// TestLoginBindSwitchOff features.login_bind_proxy=false 时回到「登录直连」：
// 给出口不支持登录域名的部署留的退路。快照存的是取反的 LoginBindOff。
func TestLoginBindSwitchOff(t *testing.T) {
	m, err := proxypool.New(filepath.Join(t.TempDir(), "proxy-pool.json"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	if _, err := m.Import("http://127.0.0.1:9"); err != nil {
		t.Fatal(err)
	}

	holder := &livecfg.Holder{}
	p := New(Config{ProxyPool: m, Live: holder})
	if !p.loginBindEnabled() {
		t.Fatal("缺省应绑定出口")
	}
	if route := p.pickLoginRoute(); route.proxyID == "" {
		t.Fatal("缺省时应从池里挑一条出口")
	}

	holder.Store(livecfg.Snapshot{LoginBindOff: true})
	if p.loginBindEnabled() {
		t.Fatal("LoginBindOff=true 时应关闭")
	}
	if route := p.pickLoginRoute(); route.proxyID != "" || route.pool != nil {
		t.Fatalf("关闭后应返回零值路线（直连），got %+v", route)
	}
}
