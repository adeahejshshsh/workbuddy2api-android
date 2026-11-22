package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

// TestFreeEndpointsRecordedInPanel 免费端点（检索/抓取）必须进请求台账（issue #275）。
//
// 它们不进 isChatEntry 时没有任何记录：面板看不到调用量、失败率、耗时，也筛不出来源，
// X-Request-Id 也不回。8 天 22573 条归档里 /v1/search 与 /v1/fetch 一条都没有，
// 而同期用户自己发起了数十次调用。
func TestFreeEndpointsRecordedInPanel(t *testing.T) {
	cases := []struct{ path, body, up string }{
		{"/v1/search", `{"query":"玉林 电梯 维保","max_results":2}`, searchOKBody},
		{"/v1/fetch", `{"url":"https://x.example/1"}`, `{"url":"https://x.example/1","content":"正文"}`},
		// DeepSeek 官方形状的别名打的是同一份实现，同样必须在台账里：
		// 漏掉它们等于给同一能力留一个「面板查不到」的后门。
		{"/v1/tools/search", `{"query":"玉林 电梯 维保","max_results":2}`, searchOKBody},
		{"/v1/tools/fetch", `{"url":"https://x.example/1"}`, `{"url":"https://x.example/1","content":"正文"}`},
	}
	for _, c := range cases {
		f := &searchFake{status: 200, body: c.up}
		reqLog := reqlog.New(reqlog.Config{})
		h := NewHandler(Config{
			Pool: testPoolWith(
				&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
			),
			Upstream:         f.client(),
			RequestLog:       reqLog,
			RecordClientInfo: true,
		})
		req := httptest.NewRequest("POST", c.path, strings.NewReader(c.body))
		req.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
		req.Header.Set("User-Agent", "free-endpoint-test/1.0")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != 200 {
			t.Fatalf("%s code=%d body=%s", c.path, rec.Code, rec.Body)
		}
		if got := rec.Header().Get("X-Request-Id"); !strings.HasPrefix(got, "req-") {
			t.Errorf("%s 应回 X-Request-Id，实际 %q", c.path, got)
		}
		var got *reqlog.Event
		for _, e := range reqLog.Snapshot().Recent {
			if e.Path == c.path {
				e := e
				got = &e
			}
		}
		if got == nil {
			t.Fatalf("%s 没有进请求台账（面板对免费端点仍是盲区）", c.path)
		}
		// 0 积分是这类端点的正常值；要的是「有一条、状态与来源都对」。
		if got.Status != 200 || !got.OK || got.Credit != 0 {
			t.Errorf("%s 事件 status=%d ok=%v credit=%v", c.path, got.Status, got.OK, got.Credit)
		}
		if got.ClientIP != "203.0.113.9" || got.UserAgent != "free-endpoint-test/1.0" {
			t.Errorf("%s 来源未采集：ip=%q ua=%q", c.path, got.ClientIP, got.UserAgent)
		}
		if got.DurationMs < 1 {
			t.Errorf("%s 耗时应 >=1ms，实际 %d", c.path, got.DurationMs)
		}
	}
}
