package panel

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/cst"
)

// 回归：Ring.Write 是 log 的输出，调用方还持着 logger 的锁，所以 Write 里不能等磁盘。
// 这里把 store 的锁攥在手里模拟「磁盘卡住」：Write 必须照样返回，放开后行才落盘。
//
// 变异判据：把 Write 改回直接 store.Append（锁内落盘），本用例超时红。
func TestRingWriteDoesNotWaitForDisk(t *testing.T) {
	dir := t.TempDir()
	store := NewLogStore(dir, 7, 0)
	defer store.Close()
	r := NewRing(10)
	if err := r.SetLogStore(store); err != nil {
		t.Fatal(err)
	}

	store.mu.Lock() // 磁盘卡住
	done := make(chan struct{})
	go func() {
		r.Write([]byte("2026/11/09 10:00:00 stuck line\n"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		store.mu.Unlock()
		t.Fatal("Ring.Write 被落盘卡住了（日志输出路径上不能等磁盘）")
	}
	if got := r.Snapshot(); len(got) != 1 {
		store.mu.Unlock()
		t.Fatalf("行应已入环：%+v", got)
	}
	store.mu.Unlock()

	r.Flush()
	day := cst.Now().Format("2006-01-02")
	got := readLogFile(filepath.Join(dir, "panel-"+day+".jsonl"))
	if len(got) != 1 || got[0].Text != "stuck line" {
		t.Fatalf("放开后行应落盘：%+v", got)
	}
}

// 回归：每行一次 write，写下去就在文件里。bufio 攒批要攒够 48KB 才落盘，没有定时刷：
// 流量低时行在内存里躺几小时，进程被杀时缓冲里的行全丢（事故现场恰恰在最后几行）；
// 缓冲边界也可能把一行拆进两次 write，与同目录另一个进程的追加交错。
//
// 变异判据：Append 改回 bufio（带 64KB 缓冲），本用例红。
func TestLogStoreAppendIsUnbuffered(t *testing.T) {
	dir := t.TempDir()
	s := NewLogStore(dir, 7, 0)
	defer s.Close()
	s.Append(LogEntry{TS: cst.Now(), Ch: ChSys, Text: "no buffer"})
	day := cst.Now().Format("2006-01-02")
	if got := readLogFile(filepath.Join(dir, "panel-"+day+".jsonl")); len(got) != 1 {
		t.Fatalf("Append 返回后行应已在文件里（不能留在缓冲里）：%+v", got)
	}
}
