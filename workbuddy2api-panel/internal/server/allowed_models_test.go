package server

// 模型白名单（上游 2026-10-27 `bd6f6cb`，issue #68）。
//
// 与 models.hidden 对称：「不在白名单 = 对本网关不存在」。生效点必须与黑名单共用
// 同一批判据（列表 / capacity / 点名拒绝）—— 漏一处就等于白名单可被绕过。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestModelAllowed(t *testing.T) {
	cases := []struct {
		name  string
		list  []string
		realm string
		bare  string
		want  bool
	}{
		{"未配置=全放行", nil, "cn", "glm-5.2", true},
		{"全空条目=未启用", []string{"", "  "}, "cn", "glm-5.2", true},
		{"裸名两域都放行", []string{"glm-5.2"}, "cn", "glm-5.2", true},
		{"裸名 global 也放行", []string{"glm-5.2"}, "global", "glm-5.2", true},
		{"不在列表=拒", []string{"glm-5.2"}, "cn", "deepseek-v4", false},
		{"cn: 前缀只放 cn", []string{"cn:glm-5.2"}, "cn", "glm-5.2", true},
		{"cn: 前缀不放 global", []string{"cn:glm-5.2"}, "global", "glm-5.2", false},
		{"global: 前缀只放 global", []string{"global:glm-5.2"}, "cn", "glm-5.2", false},
		{"大小写不敏感（与 hidden 同口径）", []string{"GLM-5.2"}, "cn", "glm-5.2", true},
		{"前缀大小写也容忍", []string{"CN:glm-5.2"}, "cn", "glm-5.2", true},
		{"含冒号的裸名不当前缀", []string{"qwen:7b"}, "global", "qwen:7b", true},
	}
	for _, c := range cases {
		h := NewHandler(Config{Pool: testPoolWith(), ModelsAllowed: c.list})
		if got := h.modelAllowed(c.realm, c.bare); got != c.want {
			t.Errorf("%s: modelAllowed(%q,%q)=%v want %v", c.name, c.realm, c.bare, got, c.want)
		}
	}
}

// 点名未在白名单的模型 → 400 model_unavailable，且不打上游。
func TestChatRejectsModelOutsideAllowlist(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		t.Error("白名单外的模型不该打上游")
		return 500, "", false
	})
	h := NewHandler(Config{
		Pool:          testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:      up,
		ModelsAllowed: []string{"glm-5.2"},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "model_unavailable") {
		t.Fatalf("body=%s", rec.Body)
	}
	// 文案不得区分「不存在」与「未在白名单」：那会把白名单变成模型枚举探测口。
	if strings.Contains(rec.Body.String(), "allowlist") || strings.Contains(rec.Body.String(), "allowed") {
		t.Fatalf("文案泄露了白名单存在: %s", rec.Body)
	}
}

// 白名单外的模型不得出现在 /v1/models 列表里。
func TestModelsListFiltersByAllowlist(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[{"id":"glm-5.2"},{"id":"deepseek-v4"}]}}`, false
	})
	h := NewHandler(Config{
		Pool:          testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:      up,
		ModelsAllowed: []string{"glm-5.2"},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "deepseek-v4") {
		t.Fatalf("白名单外的模型不该出现在列表: %s", rec.Body)
	}
}
