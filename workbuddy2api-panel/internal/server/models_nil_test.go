package server

// W2-13 回归：Upstream == nil 时 /v1/models 不许 panic。
//
// modelList 直接解引用 h.cfg.Upstream.HTTP（ContextWindowListingV4 /
// MaxOutputTokensListingV4）与 FetchGlobalModelInfos，而 NewHandler 允许
// Upstream 为空（只挂 MCP/面板的进程与单测）。同文件 generationModelEntries
// 早有 nil 保护，modelList 漏了同一条。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestModelListWithoutUpstreamDoesNotPanic(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var out struct {
		Data []any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %v body=%s", err, rec.Body)
	}
	if out.Data == nil {
		t.Fatalf("data 应是空数组而不是 null：%s", rec.Body)
	}
	if len(out.Data) != 0 {
		t.Fatalf("没有 Upstream 时不该编造模型条目：%d", len(out.Data))
	}
}
