package reqlog

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPurgeClearsArchiveAndKeepsWriting 手动清空归档：删掉全部事件文件，之后照常写入。
//
// 关键在「之后照常写入」：清空是关文件再删，若不清 w.day/path，下一条事件会往
// 已删除的 inode 里追加——文件在磁盘上已不可见，日志从此静默丢失（prune 当初跳过
// 当前文件就是躲这个坑）。这里钉住「清空后新事件仍能读出来」。
func TestPurgeClearsArchiveAndKeepsWriting(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Enabled: true, Dir: dir, MaxBytes: 1 << 20, RetentionDays: 7})
	base := time.Now().Add(-time.Minute)
	for i, id := range []string{"a", "b"} {
		r.Record(Event{Time: base.Add(time.Duration(i) * time.Second), RequestID: id, Status: 200, OK: true})
	}

	res, err := r.Purge()
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if res.Files == 0 {
		t.Fatalf("应删掉至少一个归档文件: %+v", res)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".jsonl" {
			t.Fatalf("清空后不该残留归档文件: %s", e.Name())
		}
	}

	// 清空后写的新事件必须落进新文件并可读（旧 inode 已删，写进去等于丢）。
	r.Record(Event{Time: base.Add(3 * time.Second), RequestID: "after", Status: 200, OK: true})
	r.Close()
	rows, err := r.ReadArchive(10, Filter{})
	if err != nil {
		t.Fatalf("ReadArchive: %v", err)
	}
	if len(rows) != 1 || rows[0].RequestID != "after" {
		t.Fatalf("清空后应只读到新事件，实际 %+v", rows)
	}
}

// TestPurgeKeepsOtherFiles 只删 requests-*.jsonl，目录里其它文件不碰。
func TestPurgeKeepsOtherFiles(t *testing.T) {
	dir := t.TempDir()
	keep := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(keep, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := New(Config{Enabled: true, Dir: dir, MaxBytes: 1 << 20, RetentionDays: 7})
	r.Record(Event{Time: time.Now(), RequestID: "a", Status: 200, OK: true})
	if _, err := r.Purge(); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	r.Close()
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("非归档文件不应被删: %v", err)
	}
}

// TestPurgeDisabledArchiveIsNoop 归档关闭时清空是空操作、不报错（面板据此 200 + 0）。
func TestPurgeDisabledArchiveIsNoop(t *testing.T) {
	r := New(Config{})
	res, err := r.Purge()
	if err != nil || res.Files != 0 {
		t.Fatalf("归档关闭时清空应为空操作: %+v %v", res, err)
	}
}
