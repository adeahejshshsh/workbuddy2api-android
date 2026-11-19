package auth

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// W5-05 回归：LoadDirReport 只认普通文件且设 1MB 上限。
// 旧实现只判 !IsDir()：auths/ 下的 FIFO 会让 os.ReadFile 永久阻塞（进程卡在监听
// 端口之前），/dev/zero 符号链接或 GB 级文件直接 OOM。
func TestW5_05LoadDirReportRejectsNonRegularAndOversized(t *testing.T) {
	dir := t.TempDir()
	good := `{"accessToken":"ck_good_token_123","refreshToken":"rt","uid":"u-good","realm":"cn"}`
	if err := os.WriteFile(filepath.Join(dir, "workbuddy-good.json"), []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	// FIFO：旧代码会在这里永久阻塞，所以修复必须靠 lstat 类型判定拦在 ReadFile 之前。
	if err := syscall.Mkfifo(filepath.Join(dir, "workbuddy-fifo.json"), 0o600); err != nil {
		t.Logf("mkfifo 不可用（%v），跳过 FIFO 子断言", err)
	}
	if err := os.Symlink("/dev/zero", filepath.Join(dir, "workbuddy-zero.json")); err != nil {
		t.Logf("symlink 不可用（%v），跳过符号链接子断言", err)
	}
	big := filepath.Join(dir, "workbuddy-big.json")
	if err := os.WriteFile(big, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(big, (1<<20)+1); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "workbuddy-dir.json"), 0o700); err != nil {
		t.Fatal(err)
	}

	report, err := LoadDirReport(dir)
	if err != nil {
		t.Fatalf("LoadDirReport: %v", err)
	}
	if report.Files != 1 || len(report.Auths) != 1 {
		t.Fatalf("只应加载 1 个普通文件，实际 files=%d auths=%d", report.Files, len(report.Auths))
	}
	if report.Auths[0].UID != "u-good" {
		t.Fatalf("加载到错误账号: %q", report.Auths[0].UID)
	}
	joined := strings.Join(report.ParseError, "\n")
	for _, want := range []string{"workbuddy-fifo.json", "workbuddy-zero.json", "workbuddy-big.json"} {
		if !strings.Contains(joined, want) {
			t.Errorf("被跳过的 %s 应计入 ParseError（热重载据此只增不删），实际: %v", want, report.ParseError)
		}
	}
}
