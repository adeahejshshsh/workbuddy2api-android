package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// W5-03 回归：内置密钥 + 存在 $wbEncrypted 文件 → 必须打启动横幅；
// 非内置密钥或没有加密文件 → 不打（避免把真正的告警淹掉）。
func TestW5_03AtRestKeyBanner(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "workbuddy-plain.json")
	if err := os.WriteFile(plain, []byte(`{"accessToken":"ck_x","uid":"u1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	enc := filepath.Join(dir, "workbuddy-enc.json")
	if err := os.WriteFile(enc, []byte(`{"auth":{"accessToken":{"$wbEncrypted":1,"envelope":"xx"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := encryptedAuthFileCount(dir); got != 1 {
		t.Fatalf("应只数到 1 个加密文件，实际 %d", got)
	}

	builtin := auth.AtRestKeySourceInfo{Source: "builtin", Builtin: true, Detail: "源码内置 wbBuiltinAtRestSecretKey"}
	banner := atRestKeyBanner(builtin, dir)
	for _, want := range []string{"W5-03", "1 个 auth 文件", "WB_AT_REST_KEY_FILE"} {
		if !strings.Contains(banner, want) {
			t.Errorf("横幅缺 %q: %s", want, banner)
		}
	}

	// 已经是私有密钥：不需要警告。
	envInfo := auth.AtRestKeySourceInfo{Source: "env", Builtin: false, Detail: "环境变量 WB_AT_REST_KEY"}
	if got := atRestKeyBanner(envInfo, dir); got != "" {
		t.Fatalf("非内置密钥不应打横幅: %s", got)
	}
	// 内置密钥但没有加密文件：也不需要警告。
	empty := t.TempDir()
	if got := atRestKeyBanner(builtin, empty); got != "" {
		t.Fatalf("没有 $wbEncrypted 文件不应打横幅: %s", got)
	}
}
