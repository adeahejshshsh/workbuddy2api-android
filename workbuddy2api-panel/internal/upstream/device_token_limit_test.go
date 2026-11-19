// device_token_limit_test.go W1-17 回归：token 文件的尺寸检查不能是「两步」。
//
// 缺陷：os.Stat 通过后 os.ReadFile 重新打开路径 —— 两次调用之间文件可被替换成
// 更大的文件；对 /dev/zero / FIFO 这类 size=0 的伪文件，Stat 永远通过，ReadFile
// 却能一直读到 OOM。现在改成「先打开 → 同一 fd 上 Stat → LimitReader 封顶」。
package upstream

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// 管道（FIFO）的 Stat.Size() 恒为 0：旧实现会把它整份读进内存。
func TestReadTrimmedFileCapsPipeWithZeroSize(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("当前平台没有 /proc/self/fd，无法用管道模拟 size=0 的伪文件")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer r.Close()
	payload := strings.Repeat("A", 64<<10)
	go func() {
		_, _ = w.Write([]byte(payload))
		_ = w.Close()
	}()

	got, rerr := readTrimmedFile(fmt.Sprintf("/proc/self/fd/%d", r.Fd()), 64)
	if rerr == nil {
		t.Fatalf("伪文件 size=0 时应由 LimitReader 兜住，实际读到 %d 字节", len(got))
	}
	if !errors.Is(rerr, errDeviceTokenTooLarge) {
		t.Fatalf("超限应返回 errDeviceTokenTooLarge，得到 %v", rerr)
	}
}

// 正常文件：恰好 maxLen 合法，maxLen+1 拒绝（边界不因 LimitReader 改动而漂移）。
func TestReadTrimmedFileBoundary(t *testing.T) {
	dir := t.TempDir()
	exact := strings.Repeat("b", 32)
	fp := dir + "/exact"
	if err := os.WriteFile(fp, []byte(exact), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := readTrimmedFile(fp, 32); err != nil || got != exact {
		t.Fatalf("恰好 maxLen 应可读：got=%q err=%v", got, err)
	}

	fp2 := dir + "/over"
	if err := os.WriteFile(fp2, []byte(exact+"x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readTrimmedFile(fp2, 32); !errors.Is(err, errDeviceTokenTooLarge) {
		t.Fatalf("maxLen+1 应拒绝，得到 %v", err)
	}
}
