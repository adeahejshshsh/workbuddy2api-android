package upstream

// 网关 401 求证口径（上游 2026-10-24 `21a6e45` 收敛）。
//
// 核心是第三条：refresh 端点同样挂在 /v2/* 上，边缘拦截时它自己也会回 HTML 页。
// 把「refresh 也失败」一律当死号，就等于在同一层再误杀一次 —— 这正是两次误杀的根因。

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// verifyUpstream 按状态码/内容回应 refresh 端点。
type verifyUpstream struct {
	status int
	ct     string
	body   string
	calls  int
}

func (u *verifyUpstream) client() *Client {
	return &Client{
		ChatBaseCN: "https://fake-upstream.test",
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			u.calls++
			return &http.Response{
				StatusCode: u.status,
				Header:     http.Header{"Content-Type": []string{u.ct}},
				Body:       io.NopCloser(strings.NewReader(u.body)),
			}, nil
		})},
	}
}

func verifyAuth(rt string) *auth.Auth {
	return &auth.Auth{UID: "u1", AccessToken: "at-old", RefreshToken: rt, ExpiresAt: 9999999999}
}

const verifyAPISIXHTML = `<html><head><title>401 Authorization Required</title></head><body>openresty</body></html>`

// refresh 成功 → Alive（会话活，401 只是那次被拦）。
func TestVerifyAliveOnRefreshOK(t *testing.T) {
	u := &verifyUpstream{status: 200, ct: "application/json",
		body: `{"code":0,"data":{"accessToken":"new-at","refreshToken":"new-rt","expiresIn":7200}}`}
	if got := u.client().VerifySessionByRefresh(verifyAuth("rt-old")); got != VerifyAlive {
		t.Fatalf("got %v want VerifyAlive", got)
	}
}

// refresh 被应用层明确拒绝 → Dead（token 真失效）。
func TestVerifyDeadOnAppReject(t *testing.T) {
	u := &verifyUpstream{status: 400, ct: "application/json", body: `{"error":"invalid_grant"}`}
	if got := u.client().VerifySessionByRefresh(verifyAuth("rt-old")); got != VerifyDead {
		t.Fatalf("got %v want VerifyDead", got)
	}
}

// ⭐ 核心：refresh 端点**自己**也被边缘拦（401 HTML）→ Inconclusive，不能判死。
func TestVerifyInconclusiveOnGateway401(t *testing.T) {
	u := &verifyUpstream{status: 401, ct: "text/html", body: verifyAPISIXHTML}
	if got := u.client().VerifySessionByRefresh(verifyAuth("rt-old")); got != VerifyInconclusive {
		t.Fatalf("got %v want VerifyInconclusive（把「拦截」当「判词」就是换个地方复发同一个误杀）", got)
	}
}

// 上游 5xx 抖动不是 token 失效的证据。
func TestVerifyInconclusiveOn5xx(t *testing.T) {
	u := &verifyUpstream{status: 502, ct: "text/html", body: verifyAPISIXHTML}
	if got := u.client().VerifySessionByRefresh(verifyAuth("rt-old")); got != VerifyInconclusive {
		t.Fatalf("got %v want VerifyInconclusive", got)
	}
}

// 429 限流同理：重试就好，不是死号。
func TestVerifyInconclusiveOn429(t *testing.T) {
	u := &verifyUpstream{status: 429, ct: "application/json", body: `{"code":1,"msg":"too many requests"}`}
	if got := u.client().VerifySessionByRefresh(verifyAuth("rt-old")); got != VerifyInconclusive {
		t.Fatalf("got %v want VerifyInconclusive", got)
	}
}

// 无 RT（静态 API Key）无从求证：不盲判死，也不发请求。
func TestVerifyInconclusiveWithoutRefreshToken(t *testing.T) {
	u := &verifyUpstream{status: 200, ct: "application/json", body: `{}`}
	if got := u.client().VerifySessionByRefresh(verifyAuth("")); got != VerifyInconclusive {
		t.Fatalf("got %v want VerifyInconclusive", got)
	}
	if u.calls != 0 {
		t.Fatalf("无 RT 时不该发请求，实际 %d 次", u.calls)
	}
}

// nil 防御：调用方传 nil 不该 panic。
func TestVerifyNilSafe(t *testing.T) {
	var c *Client
	if got := c.VerifySessionByRefresh(nil); got != VerifyInconclusive {
		t.Fatalf("got %v want VerifyInconclusive", got)
	}
	if got := (&Client{}).VerifySessionByRefresh(nil); got != VerifyInconclusive {
		t.Fatalf("got %v want VerifyInconclusive", got)
	}
}

// TestSessionDeadButUsable 判据只看 AT 是否还有效，不看上游文案/azp（issue #83）。
func TestSessionDeadButUsable(t *testing.T) {
	future := &auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: time.Now().Add(26 * 24 * time.Hour).Unix()}
	if !SessionDeadButUsable(future) {
		t.Error("AT 还剩 26 天 → 应判定仍可用")
	}
	expired := &auth.Auth{UID: "u2", AccessToken: "at", ExpiresAt: 1}
	if SessionDeadButUsable(expired) {
		t.Error("AT 已过期 → 不可用，应计 session dead")
	}
	noExpiry := &auth.Auth{UID: "u3", AccessToken: "at"} // ExpiresAt=0：NeedsRefresh 为 true
	if SessionDeadButUsable(noExpiry) {
		t.Error("无过期时间 → 按需刷新语义视为不可用")
	}
	if SessionDeadButUsable(nil) {
		t.Error("nil 应判 false")
	}
}
