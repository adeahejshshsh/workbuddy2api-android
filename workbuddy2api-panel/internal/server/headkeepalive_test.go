package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// committedWriter 返回一个已经提交过响应头（写过一条注释帧）的 sseCommitWriter。
func committedWriter(rec *httptest.ResponseRecorder) *sseCommitWriter {
	c := &sseCommitWriter{ResponseWriter: rec}
	_, _ = c.Write([]byte(": keepalive\n\n"))
	return c
}

// TestCommittedErrorBecomesSSEFrame 响应头已提交时，错误必须降级成协议内 error 帧。
func TestCommittedErrorBecomesSSEFrame(t *testing.T) {
	rec := httptest.NewRecorder()
	k := committedWriter(rec)
	writeOpenAIErrorHint(k, http.StatusServiceUnavailable, "no_healthy_account", "all accounts busy", "hint-x")

	out := rec.Body.String()
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("缺 [DONE]: %q", out)
	}
	// 取出最后一条 data 帧，确认是 error 对象且带 gateway_hint。
	var got map[string]any
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "data: {") {
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &got); err != nil {
				t.Fatalf("错误帧不是合法 JSON: %v", err)
			}
		}
	}
	e, ok := got["error"].(map[string]any)
	if !ok {
		t.Fatalf("错误帧缺 error 对象: %v", got)
	}
	if e["code"] != "no_healthy_account" || e["gateway_hint"] != "hint-x" {
		t.Fatalf("错误帧字段不对: %v", e)
	}
}

// TestUncommittedErrorStaysJSON 没有补过注释时，仍然回 503 + JSON（旧行为不变）。
func TestUncommittedErrorStaysJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	k := &sseCommitWriter{ResponseWriter: rec}
	writeOpenAIErrorHint(k, http.StatusServiceUnavailable, "no_healthy_account", "all accounts busy", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("未提交时应回 503，实际 %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type 应为 JSON，实际 %q", ct)
	}
}
