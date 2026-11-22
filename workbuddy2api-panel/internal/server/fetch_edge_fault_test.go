package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestFetchEdgeFaultStopsRotatingAfterFirstAccount 域名/边缘级失败（15021 域名路由
// 策略拒绝、15022 上游浏览器连不上目标站）不轮转账号：同一个 URL 换任何号结果一致，
// 换号只白烧名额、给健康号喂连败（issue #280 实测：一次 fetch 轮了 3 个 CN 号，
// 三行报文完全相同）。
//
// 修复前每域固定打满 searchAttempts=3 个号；修复后本域只打一次就退出，跨域兜底仍由
// fetch() 负责（本用例池里只有 CN 号，故没有第二域可试）。
func TestFetchEdgeFaultStopsRotatingAfterFirstAccount(t *testing.T) {
	for _, tc := range []struct{ code, msg string }{
		{"15021", "edgebrowser: API error: domain rejected by via routing policy"},
		{"15022", "edgebrowser: API error: net::ERR_CONNECTION_REFUSED"},
	} {
		body := `{"code":` + tc.code + `,"msg":"` + tc.msg + `"}`
		f := &searchFake{status: 422, body: body}
		p := testPoolWith(
			&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
			&auth.Auth{UID: "c2", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
			&auth.Auth{UID: "c3", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
		)
		h := NewHandler(Config{Pool: p, Upstream: f.client()})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/fetch",
			strings.NewReader(`{"url":"https://docs.example.com/x"}`)))

		if len(f.paths) != 1 {
			t.Fatalf("%s: 上游打了 %d 次 want 1（边缘级错误不该换号）", tc.code, len(f.paths))
		}
		if rec.Code == 200 {
			t.Fatalf("%s: 不该成功，body=%s", tc.code, rec.Body)
		}
		// 不罚号：喂连败会让「连续抓几个抓不到的域名」把整池降到出池。
		for _, uid := range []string{"c1", "c2", "c3"} {
			st, _ := p.Status(uid)
			if st.Cooling || st.Disabled || st.ConsecutiveFails != 0 || !st.DegradeUntil.IsZero() {
				t.Errorf("%s/%s: 边缘级错误不得罚号: %+v", tc.code, uid, st)
			}
		}
	}
}
