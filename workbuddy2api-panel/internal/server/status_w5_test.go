package server

// W5-03 / W5-04 接线回归：
//   - /status 暴露 at_rest_key（builtin/env/file）与 require_api_key；
//   - 空 api_key 的放行/拒绝统一走 httpauth.AllowEmptyKey()，server 侧不再自己判空。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
)

func statusJSON(t *testing.T, h *Handler) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	h.status(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status code=%d body=%s", rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("status 不是 JSON: %v body=%s", err, rec.Body)
	}
	return out
}

func TestStatusAtRestKeySourceThreeStates(t *testing.T) {
	old := atRestKeySource
	t.Cleanup(func() { atRestKeySource = old })

	cases := []struct {
		source  string
		builtin bool
	}{
		{"builtin", true},
		{"env", false},
		{"file", false},
	}
	for _, c := range cases {
		tc := c
		t.Run(tc.source, func(t *testing.T) {
			atRestKeySource = func() auth.AtRestKeySourceInfo {
				return auth.AtRestKeySourceInfo{Source: tc.source, Builtin: tc.builtin, Detail: "detail-" + tc.source}
			}
			h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})})
			out := statusJSON(t, h)
			if got, _ := out["at_rest_key"].(string); got != tc.source {
				t.Fatalf("at_rest_key=%q want %q", got, tc.source)
			}
			if got, _ := out["at_rest_key_builtin"].(bool); got != tc.builtin {
				t.Fatalf("at_rest_key_builtin=%v want %v", got, tc.builtin)
			}
			if got, _ := out["at_rest_key_detail"].(string); got != "detail-"+tc.source {
				t.Fatalf("at_rest_key_detail=%q", got)
			}
		})
	}
}

func TestStatusRequireAPIKeyReflectsAllowEmptyKey(t *testing.T) {
	old := httpauth.AllowEmptyKey()
	t.Cleanup(func() { httpauth.SetAllowEmptyKey(old) })
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})})

	httpauth.SetAllowEmptyKey(true)
	if got, _ := statusJSON(t, h)["require_api_key"].(bool); got {
		t.Fatal("AllowEmptyKey=true 时 require_api_key 应为 false")
	}
	httpauth.SetAllowEmptyKey(false)
	if got := statusJSON(t, h)["require_api_key"]; got != true {
		t.Fatalf("AllowEmptyKey=false 时 require_api_key 应为 true，got %v", got)
	}
}

// 空 api_key + 失败关闭：非回环 401，回环（Android 内嵌/本机自托管）放行。
func TestEmptyAPIKeyUsesHTTPAuthPolicy(t *testing.T) {
	old := httpauth.AllowEmptyKey()
	t.Cleanup(func() { httpauth.SetAllowEmptyKey(old) })
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})})

	httpauth.SetAllowEmptyKey(false)
	remote := httptest.NewRequest(http.MethodGet, "/status", nil)
	remote.RemoteAddr = "192.0.2.7:4321"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, remote)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("非回环 + 空 key 应 401，实际 code=%d body=%s", rec.Code, rec.Body)
	}

	loop := httptest.NewRequest(http.MethodGet, "/status", nil)
	loop.RemoteAddr = "127.0.0.1:4321"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, loop)
	if rec.Code != http.StatusOK {
		t.Fatalf("回环 + 空 key 应放行，实际 code=%d body=%s", rec.Code, rec.Body)
	}

	httpauth.SetAllowEmptyKey(true)
	remote2 := httptest.NewRequest(http.MethodGet, "/status", nil)
	remote2.RemoteAddr = "192.0.2.7:4321"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, remote2)
	if rec.Code != http.StatusOK {
		t.Fatalf("AllowEmptyKey=true 时非回环也应放行（未启用鉴权），实际 code=%d", rec.Code)
	}
}
