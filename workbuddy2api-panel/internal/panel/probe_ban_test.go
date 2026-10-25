package panel

// 封禁账号只读探活（上游 2026-10-24 `b58341f`）：
// 账号被 11140 判死后，唯一确认方式是反复 revive —— 每次 revive 都会让它重新参与
// 选号、立刻再撞 11140，可能加重风控。探活用余额端点（最轻的只读端点，不占在途、
// 不记连败、不参与选号），**完全不改池状态**。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const probeAPISIXHTML = `<html><head><title>401 Authorization Required</title></head><body>openresty</body></html>`

type probeReply struct {
	status int
	ct     string
	body   string
}

// newProbePanel 按路径分流：refresh 端点 vs 余额端点。
func newProbePanel(t *testing.T, balance, refresh probeReply) (*Panel, *pool.Pool) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rep := balance
		if strings.Contains(r.URL.Path, "refresh") {
			rep = refresh
		}
		if rep.ct != "" {
			w.Header().Set("Content-Type", rep.ct)
		}
		w.WriteHeader(rep.status)
		_, _ = w.Write([]byte(rep.body))
	}))
	t.Cleanup(srv.Close)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	pn := &Panel{cfg: Config{
		Pool:     p,
		Upstream: &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL},
	}}
	return pn, p
}

func doProbe(t *testing.T, pn *Panel) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/panel/api/accounts/u1/probe_ban", nil)
	req.SetPathValue("uid", "u1")
	rec := httptest.NewRecorder()
	pn.accountProbeBan(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	return got
}

func TestProbeBanLiftedWhenBalanceOK(t *testing.T) {
	pn, p := newProbePanel(t,
		probeReply{status: 200, ct: "application/json", body: `{"code":0,"data":{"Accounts":[]}}`},
		probeReply{status: 200, ct: "application/json", body: `{"code":0,"data":{}}`})
	got := doProbe(t, pn)
	if got["verdict"] != "lifted" {
		t.Fatalf("verdict=%v want lifted (body=%v)", got["verdict"], got)
	}
	if got["advice"] == nil || got["advice"] == "" {
		t.Fatalf("应带下一步动作: %v", got)
	}
	// 探测是观测行为：不得改池状态（否则等于变相 revive）。
	st, _ := p.Status("u1")
	if st.Disabled || st.Paused || st.Cooling {
		t.Fatalf("探活不该改池状态: %+v", st)
	}
}

func TestProbeBanStillBannedOnAccountFault(t *testing.T) {
	pn, _ := newProbePanel(t,
		probeReply{status: 400, ct: "application/json", body: `{"code":11140,"msg":"request illegal"}`},
		probeReply{status: 200, ct: "application/json", body: `{"code":0,"data":{}}`})
	got := doProbe(t, pn)
	if got["verdict"] != "still_banned" {
		t.Fatalf("verdict=%v want still_banned (body=%v)", got["verdict"], got)
	}
}

// ⭐ 核心：余额端点回 401 APISIX HTML 时，**封禁与登录失效的响应体完全一样**，
// 只能靠 refresh 求证。refresh 成功 → 会话仍活 → blocked（不是 session_dead）。
func TestProbeBanBlockedWhenRefreshStillWorks(t *testing.T) {
	pn, _ := newProbePanel(t,
		probeReply{status: 401, ct: "text/html", body: probeAPISIXHTML},
		probeReply{status: 200, ct: "application/json",
			body: `{"code":0,"data":{"accessToken":"new-at","refreshToken":"new-rt","expiresIn":7200}}`})
	got := doProbe(t, pn)
	if got["verdict"] != "blocked" {
		t.Fatalf("verdict=%v want blocked（refresh 成功说明会话仍活，401 只是被拦）body=%v", got["verdict"], got)
	}
}

// refresh 被应用层明确拒绝 → 登录态真失效，不是封禁问题。
func TestProbeBanSessionDeadWhenRefreshRejected(t *testing.T) {
	pn, _ := newProbePanel(t,
		probeReply{status: 401, ct: "text/html", body: probeAPISIXHTML},
		probeReply{status: 400, ct: "application/json", body: `{"error":"invalid_grant"}`})
	got := doProbe(t, pn)
	if got["verdict"] != "session_dead" {
		t.Fatalf("verdict=%v want session_dead body=%v", got["verdict"], got)
	}
}

// 上游 5xx：无明确结论，稍后再试（不能报「仍被封」误导运维继续等）。
func TestProbeBanInconclusiveOn5xx(t *testing.T) {
	pn, _ := newProbePanel(t,
		probeReply{status: 500, ct: "text/html", body: probeAPISIXHTML},
		probeReply{status: 200, ct: "application/json", body: `{"code":0,"data":{}}`})
	got := doProbe(t, pn)
	if got["verdict"] != "inconclusive" {
		t.Fatalf("verdict=%v want inconclusive body=%v", got["verdict"], got)
	}
}
