package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestChatBusyLevels 钉住 /v2/chat/busy 的三种取值：
// 模型级冷却 → busy；有号可服务 → idle；目录里没有 → unknown。
//
// 真机依据（2026-10-21）：国区 5.8.3 客户端看到的是
// {"code":0,"msg":"OK","data":[{"model":"deepseek-v4.1-flash","busy_level":"idle"}]}，
// 聚合别名（fast-model 等）回 unknown。
func TestChatBusyLevels(t *testing.T) {
	// 目录缓存：只登记 deepseek-v4.1-flash（其余模型应判 unknown）。
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = []upstream.ModelInfo{{ID: "deepseek-v4.1-flash"}}
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
	defer func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids = nil
		dynamicModelsCache.fetched = time.Time{}
		dynamicModelsCache.Unlock()
	}()

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	})})

	post := func(body string) map[string]any {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v2/chat/busy", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("bad JSON: %v body=%s", err, rec.Body)
		}
		return resp
	}

	levels := func(resp map[string]any) map[string]string {
		out := map[string]string{}
		for _, it := range resp["data"].([]any) {
			m := it.(map[string]any)
			out[m["model"].(string)] = m["busy_level"].(string)
		}
		return out
	}

	resp := post(`{"models":["deepseek-v4.1-flash","no-such-model"]}`)
	if resp["code"].(float64) != 0 {
		t.Errorf("code=%v want 0", resp["code"])
	}
	got := levels(resp)
	if got["deepseek-v4.1-flash"] != busyLevelIdle {
		t.Errorf("可服务模型 busy_level=%q want idle", got["deepseek-v4.1-flash"])
	}
	if got["no-such-model"] != busyLevelUnknown {
		t.Errorf("未知模型 busy_level=%q want unknown", got["no-such-model"])
	}

	// 打上模型级冷却后应变 busy（客户端据此给选择器打「繁忙」）。
	p.CooldownModelBusy("u1", "deepseek-v4.1-flash", "14003 model busy")
	got = levels(post(`{"models":["deepseek-v4.1-flash"]}`))
	if got["deepseek-v4.1-flash"] != busyLevelBusy {
		t.Errorf("全池模型级冷却时 busy_level=%q want busy", got["deepseek-v4.1-flash"])
	}

	// 空请求体：返回空 data，不 panic、不回错。
	resp = post(`{"models":[]}`)
	if data, ok := resp["data"].([]any); !ok || len(data) != 0 {
		t.Errorf("空 models 应返回空 data: %v", resp["data"])
	}
}

// TestChatBusyBadJSON 非法 body → 400，不应 500。
func TestChatBusyBadJSON(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v2/chat/busy", strings.NewReader("{not json")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
}
