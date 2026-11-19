package main

import "testing"

// W5-16 回归：审计指纹优先用 admin_api_key，未配置时才回退 api_key。
func TestW5_16AuditLogKeyPrefersAdminKey(t *testing.T) {
	cfg := &Config{APIKey: "client-key", AdminAPIKey: "admin-key"}
	if got := auditLogKey(cfg); got != "admin-key" {
		t.Fatalf("应优先用 admin_api_key，实际 %q", got)
	}
	cfg.AdminAPIKey = ""
	if got := auditLogKey(cfg); got != "client-key" {
		t.Fatalf("未配 admin key 应回退 api_key，实际 %q", got)
	}
	if got := auditLogKey(nil); got != "" {
		t.Fatalf("nil 配置应返回空串，实际 %q", got)
	}
}
