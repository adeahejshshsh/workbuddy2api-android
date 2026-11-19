package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// W5-07 回归：登录 state 文件不能跟随符号链接。
// 旧实现 os.WriteFile 固定 $TMPDIR 路径：同机进程可预置同名链接，把写 state
// 变成覆盖任意可写文件；读取侧也会跟随链接。
func TestW5_07WriteLoginStateReplacesSymlinkNotTarget(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("SECRET-DO-NOT-TOUCH"), 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "wb2api-login-state.json")
	if err := os.Symlink(victim, statePath); err != nil {
		t.Skipf("symlink 不可用: %v", err)
	}

	if err := writeLoginState(statePath, []byte(`{"state":"s1","realm":"cn"}`)); err != nil {
		t.Fatalf("writeLoginState: %v", err)
	}
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "SECRET-DO-NOT-TOUCH" {
		t.Fatalf("符号链接目标被改写: %q", string(got))
	}
	info, err := os.Lstat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("state 路径应被替换为普通文件，实际 %s", info.Mode())
	}
}

func TestW5_07ReadLoginStateRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim.json")
	if err := os.WriteFile(victim, []byte(`{"state":"stolen"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "wb2api-login-state.json")
	if err := os.Symlink(victim, statePath); err != nil {
		t.Skipf("symlink 不可用: %v", err)
	}
	if _, err := readLoginState(statePath); err == nil {
		t.Fatal("读取符号链接应被 O_NOFOLLOW 拒绝")
	} else if !strings.Contains(err.Error(), "too many") && !strings.Contains(err.Error(), "not a regular") {
		// 不同平台 errno 文案不同，只要求不是成功；这里放宽到任意 error。
		t.Logf("拒绝原因（平台相关）: %v", err)
	}
}
