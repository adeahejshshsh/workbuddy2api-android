package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// W5-03 回归：at-rest 密钥来源必须可被面板/status 读出，且只暴露来源元数据，
// 绝不返回密钥材料本身。
func TestW5_03AtRestKeySource(t *testing.T) {
	oldKey, oldInfo := wbAtRestSecretKey, atRestKeyInfo
	t.Cleanup(func() { wbAtRestSecretKey, atRestKeyInfo = oldKey, oldInfo })

	t.Run("builtin", func(t *testing.T) {
		t.Setenv("WB_AT_REST_KEY", "")
		t.Setenv("WB_AT_REST_KEY_FILE", "")
		if got := resolveAtRestSecretKey(); got != wbBuiltinAtRestSecretKey {
			t.Fatalf("无覆盖时应回落内置密钥")
		}
		info := AtRestKeySource()
		if !info.Builtin || info.Source != "builtin" {
			t.Fatalf("来源应为 builtin，实际 %+v", info)
		}
		if strings.Contains(info.Detail, wbBuiltinAtRestSecretKey) {
			t.Fatal("来源信息泄露了密钥材料")
		}
	})

	t.Run("env", func(t *testing.T) {
		t.Setenv("WB_AT_REST_KEY", "tenant-secret")
		t.Setenv("WB_AT_REST_KEY_FILE", "")
		if got := resolveAtRestSecretKey(); got != "tenant-secret" {
			t.Fatalf("env 覆盖未生效: %q", got)
		}
		info := AtRestKeySource()
		if info.Builtin || info.Source != "env" {
			t.Fatalf("来源应为 env，实际 %+v", info)
		}
		if strings.Contains(info.Detail, "tenant-secret") {
			t.Fatal("来源信息泄露了密钥材料")
		}
	})

	t.Run("file", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "at-rest.key")
		if err := os.WriteFile(p, []byte("file-secret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("WB_AT_REST_KEY", "")
		t.Setenv("WB_AT_REST_KEY_FILE", p)
		if got := resolveAtRestSecretKey(); got != "file-secret" {
			t.Fatalf("文件覆盖未生效: %q", got)
		}
		info := AtRestKeySource()
		if info.Builtin || info.Source != "file" || !strings.Contains(info.Detail, p) {
			t.Fatalf("来源应为 file（含路径），实际 %+v", info)
		}
		if strings.Contains(info.Detail, "file-secret") {
			t.Fatal("来源信息泄露了密钥材料")
		}
	})
}
