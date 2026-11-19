package credithist

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// W5-09 回归：load 必须拒绝非普通文件与超大文件（此前 FIFO 会永久阻塞、
// /dev/zero 符号链接 / GB 级文件直接 OOM）。
func TestW5_09LoadRejectsNonRegularAndOversized(t *testing.T) {
	t.Run("FIFO", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "credit-history.json")
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Skipf("mkfifo 不可用: %v", err)
		}
		// 旧实现会卡在 os.ReadFile；测试能走到下一行就说明类型闸生效。
		l := New(path, 10)
		if got := l.Read(0); len(got) != 0 {
			t.Fatalf("FIFO 不应被加载，实际 %d 条", len(got))
		}
	})

	t.Run("SYMLINK", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "credit-history.json")
		if err := os.Symlink("/dev/zero", path); err != nil {
			t.Skipf("symlink 不可用: %v", err)
		}
		l := New(path, 10)
		if got := l.Read(0); len(got) != 0 {
			t.Fatalf("/dev/zero 符号链接不应被加载，实际 %d 条", len(got))
		}
	})

	t.Run("OVERSIZED", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "credit-history.json")
		// 内容本身是**合法 JSON**（尾部用空格撑到上限以上）：这样如果大小闸失效，
		// 文件会被正常解析出 snapshot["u1"]=100，测试必须能抓到，而不是靠
		// 「JSON 坏了也会备份」蒙混过关。
		body := `{"version":1,"snapshot":{"u1":100},"entries":[]}`
		if err := os.WriteFile(path, []byte(body+strings.Repeat(" ", 8<<20)), 0o600); err != nil {
			t.Fatal(err)
		}
		l := New(path, 10)
		if got := l.Read(0); len(got) != 0 {
			t.Fatalf("超大文件不应被加载，实际 %d 条", len(got))
		}
		if _, ok := l.snapshot["u1"]; ok {
			t.Fatal("超大文件被解析了（大小闸失效）")
		}
		// 必须留备份：否则下一次 Observe 会用空账本原地覆盖，历史彻底消失。
		matches, _ := filepath.Glob(path + ".corrupt.*")
		if len(matches) == 0 {
			t.Fatal("超大账本应被备份为 .corrupt.* 后再从空账本启动")
		}
	})
}

// W5-06 回归：落盘必须 fsync 临时文件、rename 后 fsync 目录。
// 旧实现只 WriteFile+rename，掉电可能留下长度正确、内容全 0 的账本。
func TestW5_06WriteFsyncsFileAndDir(t *testing.T) {
	oldFile, oldDir := syncFileHook, syncDirHook
	fileSyncs, dirSyncs := 0, 0
	syncFileHook = func(f *os.File) error { fileSyncs++; return f.Sync() }
	syncDirHook = func(dir string) error { dirSyncs++; return oldDir(dir) }
	t.Cleanup(func() { syncFileHook, syncDirHook = oldFile, oldDir })

	path := filepath.Join(t.TempDir(), "credit-history.json")
	l := New(path, 10)
	l.Observe("u1", 100) // 首次观察也会落盘（建基线）
	if fileSyncs == 0 {
		t.Fatal("落盘前必须 fsync 临时文件（W5-06）")
	}
	if dirSyncs == 0 {
		t.Fatal("rename 后必须 fsync 目录（W5-06）")
	}
	// 顺带确认内容真落盘了（不是只调了钩子）。
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), `"u1"`) {
		t.Fatalf("账本内容未落盘: err=%v raw=%q", err, string(raw))
	}
}
