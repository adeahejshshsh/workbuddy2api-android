package main

import "testing"

// 回归：面板曾把 logging.request_retention_days 以字符串提交，config.json 里落成
// "7"，下次启动 json.Unmarshal 到 int 直接失败 → 网关起不来。这里钉住容错解码。
func TestApplyJSONToleratesNumericStrings(t *testing.T) {
	c := Default()
	raw := []byte(`{"logging":{"request_retention_days":"7","request_archive_max_mb":"128"},
		"pool":{"max_in_flight":"5","credit_floor":"0"},
		"panel":{"package_detail_limit":"9"}}`)
	if err := ApplyJSON(raw, c); err != nil {
		t.Fatalf("字符串数字应被容错解析，得到错误: %v", err)
	}
	if c.Logging.RequestRetentionDays != 7 {
		t.Errorf("request_retention_days = %d want 7", c.Logging.RequestRetentionDays)
	}
	if c.Logging.RequestArchiveMaxMB != 128 {
		t.Errorf("request_archive_max_mb = %d want 128", c.Logging.RequestArchiveMaxMB)
	}
	if c.Pool.MaxInFlight != 5 {
		t.Errorf("max_in_flight = %d want 5", c.Pool.MaxInFlight)
	}
	if c.Panel.PackageDetailLimit != 9 {
		t.Errorf("package_detail_limit = %d want 9", c.Panel.PackageDetailLimit)
	}
}

// 正常数字照旧；非数字字符串仍然报错（不静默吞）。
func TestApplyJSONStillRejectsGarbage(t *testing.T) {
	c := Default()
	if err := ApplyJSON([]byte(`{"logging":{"request_retention_days":7}}`), c); err != nil {
		t.Fatalf("纯数字不该报错: %v", err)
	}
	if c.Logging.RequestRetentionDays != 7 {
		t.Fatalf("request_retention_days = %d", c.Logging.RequestRetentionDays)
	}
	if err := ApplyJSON([]byte(`{"logging":{"request_retention_days":"abc"}}`), Default()); err == nil {
		t.Fatal("非数字字符串应当仍然报错")
	}
}

// 不相关的字符串字段不能被"顺手改成数字"。
func TestCoerceDoesNotTouchOtherStrings(t *testing.T) {
	c := Default()
	if err := ApplyJSON([]byte(`{"api_key":"123","auth_dir":"/tmp/456"}`), c); err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if c.APIKey != "123" {
		t.Errorf("api_key 被改了: %q", c.APIKey)
	}
	if c.AuthDir != "/tmp/456" {
		t.Errorf("auth_dir 被改了: %q", c.AuthDir)
	}
}
