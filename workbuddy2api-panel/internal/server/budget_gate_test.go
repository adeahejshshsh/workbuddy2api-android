package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 会真烧额度的入口必须都判积分闸（审查 B1）：此前 embeddings / search / fetch / 3D
// 既不判闸也不入账，设了 daily_budget 的部署可以靠它们绕过唯一的成本止损线。
func TestBudgetGateCoversAllSpendEndpoints(t *testing.T) {
	h := NewHandler(Config{
		Pool:              pool.New(""),
		Upstream:          upstream.New(),
		EmbeddingsEnabled: true,
		ThreeDEnabled:     true,
		DailyBudget:       1, // 日预算 1 credit
	})
	// 记满 2 credit = 必然拒（admit 的语义是「已用 >= 上限则拒」）。
	h.budget.add(2, true)

	for _, c := range []struct{ path, body string }{
		{"/v1/embeddings", `{"input":"hi","model":"bge-m3"}`},
		{"/v1/search", `{"query":"hi"}`},
		{"/v1/fetch", `{"url":"https://example.com"}`},
		{"/v1/3d/generations", `{"prompt":"cat"}`},
	} {
		req := httptest.NewRequest("POST", c.path, strings.NewReader(c.body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusTooManyRequests {
			t.Errorf("%s 预算耗尽时：code=%d want 429（没判闸就会去打上游）", c.path, rec.Code)
		}
	}
}
