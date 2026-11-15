// audit_round2_test.go 第二轮代码审计（CODE-AUDIT-FULL 第 10.16 节 auth）剩余项的回归用例。
//
// 命名 TestC2_<编号>_<语义>，与报告编号一一对应。
package auth

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// W2-C2-29：凭据字段不得含控制字符（\r\n 会被下游拼进 HTTP 头 = 头注入原语），
// 也不接受超长值；正常 JWT / 静态 Key 必须照旧通过。
func TestC2_29_CredentialControlCharsRejected(t *testing.T) {
	bad := []string{
		`{"accessToken":"at-abc\r\nX-Injected: 1"}`,
		`{"accessToken":"at-abc\n"}`,
		`{"accessToken":"at-abc\u0000def"}`,
		`{"refreshToken":"rt-abc\r\nX: y","accessToken":"at-ok"}`,
	}
	for _, raw := range bad {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("含控制字符的凭据必须被拒: %q", raw)
		} else if !strings.Contains(err.Error(), "control character") {
			t.Errorf("错误应指明控制字符: %v", err)
		}
	}
	// 超长（> 8192 字节）拒绝。
	long := strings.Repeat("a", maxCredentialLen+1)
	if _, err := Parse([]byte(`{"accessToken":"` + long + `"}`)); err == nil {
		t.Fatal("超长 accessToken 必须被拒")
	}
	// 正常形态必须通过：JWT（含 . - _）、静态 Key 前缀、空 refreshToken。
	ok := []string{
		`{"accessToken":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sig-_x","refreshToken":"rt_x-1.2"}`,
		`{"accessToken":"ck_live_abcdef123456"}`,
		`{"accessToken":"pt_live_abcdef123456","refreshToken":""}`,
	}
	for _, raw := range ok {
		if _, err := Parse([]byte(raw)); err != nil {
			t.Errorf("合法凭据被拒: %q → %v", raw, err)
		}
	}
}

// W2-C2-37：含 $wbEncrypted 但整体不是合法 JSON 时，错误必须明确归因到「加密文件
// 解码失败」，而不是笼统的 storage_parse_error（处置方式完全不同）。
func TestC2_37_EncryptedDecodeErrorAttributed(t *testing.T) {
	raw := []byte(`{"auth":{"accessToken":{"$wbEncrypted":1,"envelope":"x"`)
	_, _, _, err := decryptWBEncrypted(raw)
	if err == nil {
		t.Fatal("损坏的加密文件必须报错")
	}
	if !strings.Contains(err.Error(), "at_rest_decode_failed") {
		t.Fatalf("错误未归因到加密解码: %v", err)
	}
	// 不含信封的普通文件保持「原样返回、不抢错误」的既有语义。
	plain := []byte(`{"accessToken":"at"}`)
	out, sealed, doc, err := decryptWBEncrypted(plain)
	if err != nil || sealed != nil || doc != nil || string(out) != string(plain) {
		t.Fatalf("普通文件不应走加密路径: out=%q sealed=%v doc=%v err=%v", out, sealed, doc, err)
	}
}

// W2-C2-25：`wbSealed` 由「信封数量」推导的耦合仍在，但**最坏后果**已被
// refuseStaleSnapshot 的明文降级守卫挡住 —— 这条用例把该防线钉死（改 sealed
// 判定时若把守卫一起改坏，这里会红）。
func TestC2_25_PlaintextDowngradeStillRefused(t *testing.T) {
	raw, err := os.ReadFile(wbVectorPath)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "auth.info")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	a := &Auth{AccessToken: "at-plain", RefreshToken: "rt-plain", ExpiresAt: 1800000000,
		UID: "uid-wb-1", FilePath: path} // 明文来源：wbSealed == nil
	if err := a.SaveAtomic(); !errors.Is(err, ErrEncryptedDowngrade) {
		t.Fatalf("明文快照覆盖加密文件应被拒，实际 %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(raw) {
		t.Fatal("加密文件被改写")
	}
}
