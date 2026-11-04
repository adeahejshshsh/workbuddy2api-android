package server

// /admin 操作审计：一次操作落一行 JSONL。两条硬要求——
//   ① **绝不写明文 API Key**（只写 8 位十六进制指纹），否则审计文件本身成了凭证泄露源；
//   ② 写失败不能让操作失败（审计是旁路，不是门禁）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuditLogRecordsWithoutLeakingKey(t *testing.T) {
	const key = "super-secret-key"
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "audit.log") // 目录不存在也要能建
	al, err := NewAuditLog(path, key)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{AdminEnabled: true, APIKey: key, Audit: al})

	req := httptest.NewRequest(http.MethodPost, "/admin/tasks/checkin/run",
		strings.NewReader(`{"reason":"手动补跑"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		// 没接 Tasks → 503；审计照样要落一行（失败也是操作结果）
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), key) {
		t.Fatalf("审计文件不得出现明文 API Key：%s", raw)
	}
	var e struct {
		Action string `json:"action"`
		Target string `json:"target"`
		Status int    `json:"status"`
		Key    string `json:"key"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &e); err != nil {
		t.Fatalf("不是合法 JSONL：%v (%s)", err, raw)
	}
	if e.Action != "task.run" || e.Target != "checkin" || e.Status != http.StatusServiceUnavailable {
		t.Fatalf("审计字段不对：%+v", e)
	}
	if e.Key == "" || e.Key == key || len(e.Key) != 8 {
		t.Fatalf("key 应是 8 位指纹，实际 %q", e.Key)
	}
	if e.Reason != "手动补跑" {
		t.Fatalf("应记录操作理由，实际 %q", e.Reason)
	}
}

// 审计关闭（Audit=nil）时链路照常工作，不 panic、不写文件。
func TestAuditDisabledPassthrough(t *testing.T) {
	h := NewHandler(Config{AdminEnabled: true, APIKey: "k"})
	req := httptest.NewRequest(http.MethodPost, "/admin/tasks/checkin/run", nil)
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}
