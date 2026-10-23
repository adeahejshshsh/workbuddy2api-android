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
	if c.Pool.ModelFallback == nil {
		t.Error("pool.model_fallback 示例应存在（空对象即可），否则用户不知道有这张表")
	}
}
