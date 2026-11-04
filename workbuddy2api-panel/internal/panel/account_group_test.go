package panel

// 账号分组端点（issue #159）：写回服务端 state.json，供面板按组筛选与组内批量。
// 纯管理维度 —— 不参与选号 / 计费 / 鉴权，这里只钉住「改得动、uid 打错要 404」。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

func TestAccountGroupEndpoint(t *testing.T) {
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p := New(Config{Version: "test", APIKey: "k", Pool: pl})
	call := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer k")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec
	}

	if rec := call("/panel/api/accounts/u1/group", `{"group":"批量A"}`); rec.Code != http.StatusOK {
		t.Fatalf("设置分组应 200, got %d (%s)", rec.Code, rec.Body.String())
	} else if !strings.Contains(rec.Body.String(), `"group":"批量A"`) {
		t.Fatalf("应回显落库后的分组, got %s", rec.Body.String())
	}
	if rec := call("/panel/api/accounts/nope/group", `{"group":"A"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("未知 uid 应 404, got %d", rec.Code)
	}
	if rec := call("/panel/api/accounts/u1/group", `not-json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("坏 JSON 应 400, got %d", rec.Code)
	}
	// 空串 = 取消分组（幂等）
	if rec := call("/panel/api/accounts/u1/group", `{"group":""}`); rec.Code != http.StatusOK {
		t.Fatalf("清空分组应 200, got %d", rec.Code)
	}
}
