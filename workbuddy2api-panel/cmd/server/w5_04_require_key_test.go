package main

import "testing"

// W5-04 回归：server.require_api_key 缺省 true（失败关闭），显式 false 才回到旧行为。
func TestW5_04RequireAPIKeyDefaultAndOverride(t *testing.T) {
	def := Default()
	if !def.Server.RequireAPIKey {
		t.Fatal("默认必须 require_api_key=true（空 api_key 时拒绝非回环）")
	}

	// 键缺席：保持默认 true（不能被 JSON 零值 false 悄悄降级）。
	absent, err := ParseConfig([]byte(`{"api_key":"k"}`))
	if err != nil {
		t.Fatalf("parse absent: %v", err)
	}
	if !absent.Server.RequireAPIKey {
		t.Fatal("配置里没写 require_api_key 时应保持 true")
	}

	// 显式 false：允许旧的全开行为（部署方自担风险，启动日志有 WARN）。
	off, err := ParseConfig([]byte(`{"api_key":"k","server":{"require_api_key":false}}`))
	if err != nil {
		t.Fatalf("parse false: %v", err)
	}
	if off.Server.RequireAPIKey {
		t.Fatal("显式 false 应生效")
	}
}
