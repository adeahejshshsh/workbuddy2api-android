package scheduler

// 新手礼包必须「先查后领」：ClaimGift 对「已领过」也返回 (0, nil)（上游 200 且 body
// 无 credit 字段），盲领写法会给每个已领号**每天**打一行假的「🎊 新手礼包 +0c」，
// 还白打一次上游。这条用例按状态分别断言「有没有真的去领」。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

type giftFake struct {
	mu    sync.Mutex
	paths []string
	body  string // check-gift-claimed 的返回
}

func (g *giftFake) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.paths = append(g.paths, r.URL.Path)
		g.mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/check-gift-claimed") {
			w.Write([]byte(g.body))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/claim-gift") {
			w.Write([]byte(`{"code":0,"data":{"credit":1500}}`))
			return
		}
		w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (g *giftFake) called(sub string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, p := range g.paths {
		if strings.Contains(p, sub) {
			return true
		}
	}
	return false
}

func giftScheduler(t *testing.T, srvURL string) *Scheduler {
	t.Helper()
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	p := pool.New("")
	p.Add(a)
	up := &upstream.Client{HTTP: &http.Client{}, ChatBaseCN: srvURL, BillingBaseCN: srvURL, WebBaseCN: srvURL}
	return New(Config{Pool: p, Upstream: up})
}

// 已领过：查了状态但**不该**再去领（否则就是那条假日志 + 白打一次上游）。
func TestStreakGiftSkipsClaimWhenAlreadyClaimed(t *testing.T) {
	g := &giftFake{body: `{"code":0,"data":{"active":true,"claimed":true,"credit_num":1500,` +
		`"end_time":"2099-10-31 23:59:59"}}`}
	s := giftScheduler(t, g.server(t).URL)
	s.streakBonusAccount(s.cfg.Pool.AuthByUID("u1"))

	if !g.called("/check-gift-claimed") {
		t.Fatal("应先查礼包状态")
	}
	if g.called("/claim-gift") {
		t.Fatal("已领过还去领取（假日志 + 白打上游）")
	}
}

// 无活动：正常 200（active=false），同样不该去领。
func TestStreakGiftSkipsClaimWhenInactive(t *testing.T) {
	g := &giftFake{body: `{"code":0,"data":{"active":false,"claimed":false,"credit_num":0}}`}
	s := giftScheduler(t, g.server(t).URL)
	s.streakBonusAccount(s.cfg.Pool.AuthByUID("u1"))
	if g.called("/claim-gift") {
		t.Fatal("无活动时不该去领取")
	}
}

// 可领：必须真的去领。
func TestStreakGiftClaimsWhenPending(t *testing.T) {
	g := &giftFake{body: `{"code":0,"data":{"active":true,"claimed":false,"credit_num":1500,` +
		`"end_time":"2099-10-31 23:59:59"}}`}
	s := giftScheduler(t, g.server(t).URL)
	s.streakBonusAccount(s.cfg.Pool.AuthByUID("u1"))
	if !g.called("/claim-gift") {
		t.Fatal("可领时必须去领取")
	}
}
