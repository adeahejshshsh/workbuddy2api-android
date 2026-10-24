package server

// 网关 401 的误杀防护：先 refresh 求证，再决定是否计 session dead（issue #66）。
//
// 背景（2026-10-24 实案）：两个账号各累计 3 次「网关 401 invalid_token」（APISIX
// 的 HTML 页、无业务信封）后被禁用，但人工 revive 后立刻恢复正常（连续 3 次请求
// 全 200）——说明那批 401 是网关侧瞬时拦截，不是 token 失效。401 本身分不出来：
// 两种成因的响应体完全一样，只有 refresh 的结果能区分。

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const gateway401HTML = "<html><head><title>401 Authorization Required</title></head>" +
	"<body><center><h1>401 Authorization Required</h1></center><hr><center>openresty</center></body></html>"

// gateway401Upstream 按路径区分 chat / refresh 并分别应答。
// 不用 newFakeUpstream：它只按 Authorization 头回固定响应，无法区分这两者。
type gateway401Upstream struct {
	mu        sync.Mutex
	chatN     int
	refreshN  int
	refreshOK bool // true=瞬时拦截（refresh 成功）；false=token 真失效（refresh 被拒）
	// refreshBlocked 模拟「refresh 端点也被边缘拦」：回 502 HTML 页，既非成功也非
	// 应用层判词。这种情形若计入 session dead，就是换了个地方复发同一个误杀。
	refreshBlocked bool
}

func (u *gateway401Upstream) client() *upstream.Client {
	return &upstream.Client{
		// ChatBaseCN 必须设：doRefresh 用它拼 refresh 端点，空值会拼出无 host 的 URL。
		ChatBaseCN: "https://fake-upstream.test",
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			isRefresh := strings.Contains(r.URL.Path, "/refresh")
			u.mu.Lock()
			if isRefresh {
				u.refreshN++
			} else {
				u.chatN++
			}
			ok := u.refreshOK
			blocked := u.refreshBlocked
			u.mu.Unlock()

			reply := func(status int, ct, body string) (*http.Response, error) {
				return &http.Response{
					StatusCode: status,
					Header:     http.Header{"Content-Type": []string{ct}},
					Body:       io.NopCloser(strings.NewReader(body)),
				}, nil
			}
			if isRefresh {
				if blocked {
					return reply(502, "text/html", gateway401HTML)
				}
				if ok {
					return reply(200, "application/json",
						`{"code":0,"data":{"accessToken":"new-at","refreshToken":"new-rt","expiresIn":7200}}`)
				}
				return reply(400, "application/json", `{"error":"invalid_grant"}`)
			}
			return reply(401, "text/html", gateway401HTML)
		})},
	}
}

func (u *gateway401Upstream) counts() (chat, refresh int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.chatN, u.refreshN
}

// newGateway401Handler FilePath 必须设：refresh 成功路径要落盘，无路径会打 ERR。
func newGateway401Handler(t *testing.T, u *gateway401Upstream) *Handler {
	t.Helper()
	a := &auth.Auth{
		UID: "gw1", AccessToken: "at-gw1", RefreshToken: "rt-gw1", ExpiresAt: 9999999999,
		FilePath: filepath.Join(t.TempDir(), "workbuddy-gw1.json"),
	}
	return NewHandler(Config{Pool: testPoolWith(a), Upstream: u.client(), MaxRotate: 1})
}

// TestGateway401TransientNotDisabled ⭐ 核心：refresh 成功 → 判为瞬时拦截，
// 不得禁用账号，且 session dead 计数被清零、新 token 落盘。
func TestGateway401TransientNotDisabled(t *testing.T) {
	up := &gateway401Upstream{refreshOK: true}
	h := newGateway401Handler(t, up)

	// 连续 3 次网关 401：旧逻辑第 3 次就禁用。
	for i := 0; i < 3; i++ {
		doFallbackChat(h, "deepseek-v4.1-flash")
	}

	st, ok := h.cfg.Pool.Status("gw1")
	if !ok {
		t.Fatal("账号应仍在池中")
	}
	if st.Disabled {
		t.Fatalf("refresh 成功时不应禁用账号（%d 次 401 后已被禁用）", 3)
	}
	if st.SessionDeadFails != 0 {
		t.Fatalf("session dead 计数应清零，实际 %d", st.SessionDeadFails)
	}
	if _, refreshN := up.counts(); refreshN == 0 {
		t.Fatal("未观察到 refresh 请求 —— 401 分支没走求证")
	}
	// 落盘：上游已轮换 RT，不写盘则重启拿旧 RT 刷新 = 账号真死。
	raw, err := os.ReadFile(h.cfg.Pool.AuthByUID("gw1").FilePath)
	if err != nil {
		t.Fatalf("refresh 成功后应落盘: %v", err)
	}
	if !strings.Contains(string(raw), "new-rt") {
		t.Fatalf("落盘内容应含新 refreshToken，实际 %s", raw)
	}
}

// TestGateway401RefreshBlockedNotDisables refresh 端点也被边缘拦（502 HTML）时，
// 既非成功也非应用层判词 → 不得计死。这是最危险的分支：把「拦截」当「判词」，
// 就等于换个地方复发同一个误杀。
func TestGateway401RefreshBlockedNotDisables(t *testing.T) {
	up := &gateway401Upstream{refreshBlocked: true}
	h := newGateway401Handler(t, up)

	for i := 0; i < 3; i++ {
		doFallbackChat(h, "deepseek-v4.1-flash")
	}

	st, ok := h.cfg.Pool.Status("gw1")
	if !ok {
		t.Fatal("账号应仍在池中")
	}
	if st.Disabled {
		t.Fatal("refresh 被边缘拦截（非明确判词）时不应禁用账号")
	}
	if st.SessionDeadFails != 0 {
		t.Fatalf("session dead 计数应保持 0，实际 %d", st.SessionDeadFails)
	}
}

// TestGateway401RealDeadStillDisables refresh 被应用层拒绝 → token 确实失效，
// 连续达阈值仍应禁用（不因本次修改放过真死号）。
func TestGateway401RealDeadStillDisables(t *testing.T) {
	up := &gateway401Upstream{refreshOK: false}
	h := newGateway401Handler(t, up)

	for i := 0; i < 3; i++ {
		doFallbackChat(h, "deepseek-v4.1-flash")
	}

	st, ok := h.cfg.Pool.Status("gw1")
	if !ok {
		t.Fatal("账号应仍在池中（禁用不等于删除）")
	}
	if !st.Disabled {
		t.Fatalf("refresh 也失败时，连续 3 次 401 应禁用账号，实际 disabled=%v", st.Disabled)
	}
}
