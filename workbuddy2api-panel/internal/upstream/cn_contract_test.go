// cn_contract_test.go CN 客户端请求契约回归：5.11.3 的 X-Client-* 三头进黄金集
// （上游 96265d7 对 cn_contract_test.go 的改动，按本仓结构重写）。
//
// 上游那份 cn_contract_test.go 是「逐路径出站头键集精确等于真机抓包」的全量黄金集；
// 本仓没有那份文件，且本仓的头集与 2026-10-28 的 5.9.3 抓包早已有意偏离（见
// fingerprint_593_test.go / official_fixture_test.go 各自的记录）。照搬一张「精确等于
// 某次抓包」的表，只会把当前实现现状抄成黄金值，看着是门禁、实际是快照。
//
// 所以这里只钉**本次改动引入的契约**，三条都是可判真假的事实：
//  1. 六条首方路径的国内版出站头里，X-Client-Version / X-Client-Platform /
//     X-Client-Product 齐带，取值 5.11.3 / windows / WorkBuddy（真机 5.11.3 抓包）；
//  2. 这三头是**唯一**的 x-client-* 头（`X-Client-IP` 之类别的族不许混进来）；
//  3. 国际版一条都不带（隔离口径，理由见 headers.go 的 injectClientIdentityHeaders）。
//
// 前两条对每条路径都断言：漏一条（某路径没接注入）或多一条（有人往公共路径搬）都红。
package upstream

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// cnContractCase 一条路径 + 取得它出站头的方式（builder 或真打一次生产函数）。
type cnContractCase struct {
	path string
	// viaBuilder 为 true 时 headers() 直接返回构造函数拼好的头（不发网络请求）；
	// 否则由生产函数真发一次请求，头从抓包服务器上取。
	headers func(t *testing.T, c *Client, a *auth.Auth) http.Header
}

// cnContractCases 六条 issue #257 抓包覆盖的路径。
// captured 的键是 "uid|path"：同一条路径 CN / GL 各打一次，不能互相覆盖。
func cnContractCases(captured map[string]http.Header) []cnContractCase {
	get := func(t *testing.T, a *auth.Auth, path string) http.Header {
		t.Helper()
		h, ok := captured[a.UID+"|"+path]
		if !ok {
			t.Fatalf("未捕获到 %s（uid=%s）的出站请求", path, a.UID)
		}
		return h
	}
	// getSuffix 按后缀找：同一条链路的 CN / global 路径前缀不同（global 先试无 /v2 形态，
	// 404 再回落带 /v2 的那条），头集与契约无关，取到任一条即可。
	getSuffix := func(t *testing.T, a *auth.Auth, suffix string) http.Header {
		t.Helper()
		for key, h := range captured {
			if strings.HasPrefix(key, a.UID+"|") && strings.HasSuffix(key, suffix) {
				return h
			}
		}
		t.Fatalf("未捕获到 *%s（uid=%s）的出站请求", suffix, a.UID)
		return nil
	}
	return []cnContractCase{
		{path: "/v2/chat/completions", headers: func(t *testing.T, c *Client, a *auth.Auth) http.Header {
			req := httptest.NewRequest("POST", "https://copilot.tencent.com/v2/chat/completions", nil)
			c.ChatHeaders(req, a, "", ChatMeta{})
			return req.Header
		}},
		{path: "/v2/report", headers: func(t *testing.T, c *Client, a *auth.Auth) http.Header {
			if err := c.ReportChatActivityModel(a, "conv-contract", "req-contract", "deepseek-v4.1-flash", ""); err != nil {
				t.Fatalf("/v2/report: %v", err)
			}
			return get(t, a, "/v2/report")
		}},
		{path: "/v2/chat/busy", headers: func(t *testing.T, c *Client, a *auth.Auth) http.Header {
			if _, err := c.ModelBusy(a, []string{"hy3"}); err != nil {
				t.Fatalf("/v2/chat/busy: %v", err)
			}
			return get(t, a, "/v2/chat/busy")
		}},
		{path: "/v2/plugin/auth/token/refresh", headers: func(t *testing.T, c *Client, a *auth.Auth) http.Header {
			req := httptest.NewRequest("POST", "https://www.codebuddy.cn/v2/plugin/auth/token/refresh", nil)
			c.RefreshHeaders(req, a)
			return req.Header
		}},
		{path: "/v2/billing/meter/checkin-activity-status", headers: func(t *testing.T, c *Client, a *auth.Auth) http.Header {
			if _, err := c.GetCheckinActivityStatus(a); err != nil {
				t.Fatalf("checkin-activity-status: %v", err)
			}
			return getSuffix(t, a, "/checkin-activity-status")
		}},
		{path: "/v3/config", headers: func(t *testing.T, c *Client, a *auth.Auth) http.Header {
			_, _, _ = c.fetchV3ConfigModelMap(a, "")
			return get(t, a, "/v3/config")
		}},
	}
}

func TestCNContractClientIdentity(t *testing.T) {
	captured := map[string]http.Header{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-User-Id") + "|" + r.URL.Path
		if _, seen := captured[key]; !seen {
			captured[key] = r.Header.Clone()
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/v2/chat/busy"):
			w.Write([]byte(`{"code":0,"msg":"OK","data":[{"model":"hy3","busy_level":"low"}]}`))
		default:
			w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
		}
	}))
	defer srv.Close()

	cn := &auth.Auth{UID: "00000000-0000-4000-8000-000000000002", AccessToken: "AT", RefreshToken: "RT", Nickname: "fixture"}
	gl := &auth.Auth{UID: "u-gl", AccessToken: "AT", RefreshToken: "RT", Nickname: "gl", Domain: "www.workbuddy.ai"}

	newClient := func() *Client {
		c := New()
		c.HTTP = srv.Client()
		c.ChatHTTP = srv.Client()
		c.ChatBaseCN = srv.URL
		c.BillingBaseCN = srv.URL
		c.WebBaseCN = srv.URL
		// 国际版走另一组 base：不指过来就会真打 www.workbuddy.ai（外网）。
		c.ChatBaseGlobal = srv.URL
		c.BillingBaseGlobal = srv.URL
		return c
	}

	for _, tc := range cnContractCases(captured) {
		t.Run(strings.TrimPrefix(tc.path, "/"), func(t *testing.T) {
			c := newClient()
			h := tc.headers(t, c, cn)
			want := map[string]string{
				"X-Client-Version":  defaultClientVersion,
				"X-Client-Platform": "windows",
				"X-Client-Product":  "WorkBuddy",
			}
			for k, v := range want {
				if got := h.Get(k); got != v {
					t.Errorf("国内版 %s 的 %s=%q want %q", tc.path, k, got, v)
				}
			}
			// 只有这三头：`X-Client-IP` 之类别的族不许被顺手带进来（它们各自有归属路径）。
			for k := range h {
				if !strings.HasPrefix(strings.ToLower(k), "x-client-") {
					continue
				}
				if _, ok := want[k]; !ok {
					t.Errorf("%s 多出非契约的 %s 头（x-client-* 只该有那三个）", tc.path, k)
				}
			}

			// 国际版：同一路径一个都不发（5.9.4 分发包里没有这层注入）。
			glClient := newClient()
			glH := tc.headers(t, glClient, gl)
			for k := range want {
				if got := glH.Get(k); got != "" {
					t.Errorf("国际版 %s 不该发 %s，实际=%q", tc.path, k, got)
				}
			}
		})
	}
}
