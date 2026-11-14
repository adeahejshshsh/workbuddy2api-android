package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestConfigExampleIsValid 守卫仓库根目录的 config.example.json 是合法 JSON、
// 且能被 Config 结构解析（键名拼错/漏逗号都不会被编译期发现）。
//
// 来由：PR #55 加 features.sanitize_brand_scope 时漏了上一行的逗号，
// 文件变成非法 JSON 而 CI 全绿——因为它只在运行时被 `cp config.example.json
// config.json` 的人读到，报错现场离提交很远。这个测试把现场挪回 CI。
func TestConfigExampleIsValid(t *testing.T) {
	path := filepath.Join("..", "..", "config.example.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读不到 %s: %v", path, err)
	}
	if !json.Valid(raw) {
		t.Fatalf("%s 不是合法 JSON", path)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("%s 无法解析为 Config: %v", path, err)
	}
	// 新增的对外配置项必须在示例里可见，否则用户只能从 README 反推。
	if !c.Features.SanitizeBlacklistFingerprints {
		t.Error("features.sanitize_blacklist_fingerprints 示例值应为 true")
	}
	// models.allowed：示例里要能让用户看到白名单开关（nil = 键缺失，无从发现）。
	if c.Models.Allowed == nil {
		t.Error("models.allowed 示例应存在（空数组即可），否则用户不知道有白名单")
	}
	if c.Pool.ModelFallback == nil {
		t.Error("pool.model_fallback 示例应存在（空对象即可），否则用户不知道有这张表")
	}
	// 11140 连坐暂停是「出事才知道存在」的配置项：示例里必须给出来，否则用户只能
	// 从 README 反推，而这条恰恰是风控扫荡时最该知道能不能关的开关。
	if c.Upstream.RestrictionHoldSeconds != 1800 {
		t.Errorf("upstream.restriction_hold_seconds 示例值应为 1800，实际 %d", c.Upstream.RestrictionHoldSeconds)
	}
	// 6020/6021 排队等待：示例必须给出，否则用户不知道「排队会先在网关内等一轮」。
	if !c.Features.BusyRetry || c.Features.BusyWaitSeconds != 60 {
		t.Errorf("features.busy_retry / busy_wait_seconds 示例应为 true / 60: %+v", c.Features)
	}
	// DeferredToolCall 参数修复是实验特性：示例里必须显式给成 false（默认关），
	// 否则用户不知道有这个开关，也无从判断「为什么它没生效」。
	var example struct {
		Features map[string]json.RawMessage `json:"features"`
	}
	if err := json.Unmarshal(raw, &example); err != nil {
		t.Fatal(err)
	}
	if _, ok := example.Features["repair_deferred_tool_args"]; !ok {
		t.Error("features.repair_deferred_tool_args 示例键缺失")
	}
	if c.Features.RepairDeferredToolArgs {
		t.Error("features.repair_deferred_tool_args 示例应显式关闭")
	}
}
