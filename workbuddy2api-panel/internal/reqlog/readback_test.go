package reqlog

import (
	"path/filepath"
	"testing"
	"time"
)

// 复现「请求日志只显示一条」：写入 5 条事件后立刻读回，应当拿到 5 条。
// 注意不调用 Close —— 面板读的是运行中的归档，不能依赖关闭时的 flush。
func TestReadArchiveWhileRunning(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Enabled: true, Dir: dir, FileMaxBytes: 1 << 20})
	defer r.Close()

	base := time.Now().Add(-time.Minute)
	for i := 0; i < 5; i++ {
		r.Record(Event{Time: base.Add(time.Duration(i) * time.Second), Model: "m", Status: 200})
	}
	// 写循环是异步的：给它一点时间落盘 + flush
	deadline := time.Now().Add(2 * time.Second)
	got := 0
	for time.Now().Before(deadline) {
		rows, err := r.ReadArchive(10, Filter{})
		if err != nil {
			t.Fatalf("ReadArchive: %v", err)
		}
		got = len(rows)
		if got == 5 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	rows, _ := r.ReadArchive(10, Filter{})
	t.Fatalf("读回 %d 条（want 5）: %+v", len(rows), rows)
}

// 同一秒内写入多条同样要能读回（文件轮转/命名不重复）。
func TestReadArchiveSameSecond(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Enabled: true, Dir: dir, FileMaxBytes: 300}) // 强制频繁轮转
	defer r.Close()
	now := time.Now()
	for i := 0; i < 6; i++ {
		r.Record(Event{Time: now, Model: "m", Status: 200, Path: "/v1/chat/completions"})
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rows, _ := r.ReadArchive(10, Filter{})
		if len(rows) == 6 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	rows, _ := r.ReadArchive(10, Filter{})
	names, _ := filepath.Glob(filepath.Join(dir, "requests-*.jsonl"))
	t.Fatalf("读回 %d 条（want 6），文件 %v", len(rows), names)
}
