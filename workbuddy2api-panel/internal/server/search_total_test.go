package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// total_results 是上游的命中总数，可以大于本页条数：丢弃无法改写的条目时只能扣掉丢弃数，
// 不能覆盖成本页剩余条数（否则「共 5 条」被谎报成「共 1 条」）；下限取剩余条数。
func TestSearchCompatDropAdjustsTotalByDroppedCount(t *testing.T) {
	for _, tc := range []struct {
		name, totalField string
		want             string // 空 = 响应里不应出现 total_results
	}{
		{"total 大于本页条数", `"total_results":5,`, "4"},
		{"total 小于剩余条数时取下限", `"total_results":0,`, "1"},
		{"上游没给 total 就不凭空造", ``, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"query":"","results":[` +
				`{"title":"","url":"","snippet":"x"},` +
				`{"title":"正常","url":"https://x.example/1","snippet":"y"}],` + tc.totalField + `"response_time_ms":1}`
			f := &searchFake{status: 200, body: body}
			h := NewHandler(Config{
				Pool: testPoolWith(
					&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
				),
				Upstream: f.client(),
			})
			req := httptest.NewRequest("POST", "/v1/search", strings.NewReader(`{"query":"q","count":8}`))
			req.Header.Set("User-Agent", dshUA)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			var got struct {
				Results []map[string]any `json:"results"`
				Total   json.RawMessage  `json:"total_results"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("响应不是 JSON: %v (%s)", err, rec.Body.String())
			}
			if len(got.Results) != 1 {
				t.Fatalf("应只剩 1 条: %v", got.Results)
			}
			if string(got.Total) != tc.want {
				t.Fatalf("total_results=%q want %q (%s)", got.Total, tc.want, rec.Body.String())
			}
		})
	}
}
