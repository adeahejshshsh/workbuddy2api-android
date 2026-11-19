package server

// W2-05 回归：/v2/chat/busy 的 models 条数必须有硬上限。
// 输出与 len(req.Models) 线性放大，1MiB body 可以塞进数万条 → 60-75MB 堆。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func postBusy(t *testing.T, h *Handler, models []string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{"models": models})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v2/chat/busy", bytes.NewReader(body)))
	return rec
}

func TestChatBusyRejectsTooManyModels(t *testing.T) {
	// Upstream 留空：顺带钉住 W2-13（modelList 的 nil 保护），否则这里先 panic。
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})})

	ok := make([]string, busyMaxModels)
	for i := range ok {
		ok[i] = "model-" + string(rune('a'+i%26))
	}
	if rec := postBusy(t, h, ok); rec.Code != http.StatusOK {
		t.Fatalf("%d 条应放行：code=%d body=%s", busyMaxModels, rec.Code, rec.Body)
	}

	over := make([]string, busyMaxModels+1)
	copy(over, ok)
	over[busyMaxModels] = "one-too-many"
	rec := postBusy(t, h, over)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("%d 条应 400，实际 code=%d body=%s", busyMaxModels+1, rec.Code, rec.Body)
	}
	var resp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("错误体不是 JSON: %v body=%s", err, rec.Body)
	}
	if resp.Error.Code != "invalid_request" {
		t.Fatalf("error.code=%q want invalid_request", resp.Error.Code)
	}
}
