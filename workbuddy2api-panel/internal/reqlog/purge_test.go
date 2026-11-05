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

// TestPurgeAfterLoopStoppedReturns 写循环已经退出时清空必须立即返回错误，不能把调用方
// （面板的 HTTP handler）卡死。
//
// purgeCh 带缓冲时，「发进通道」和「done 已关」两个分支同时就绪，select 随机选；选中发送
// 就落进一个没人读的缓冲，<-out 永远等不到结果。无缓冲后发送成功即说明写循环已收下请求。
//
// 这里直接关 stop 让 run 收尾（而不是走 Close）：Close 会先置 closed，purge 在 enabled()
// 就早退了，缓冲这个坑被另一处守卫掩盖住 —— 那样这条用例就测不到 purgeCh 本身。
func TestPurgeAfterLoopStoppedReturns(t *testing.T) {
	r := New(Config{Enabled: true, Dir: t.TempDir(), MaxBytes: 1 << 20, RetentionDays: 7})
	if r.archive == nil || r.archive.done == nil {
		t.Fatal("归档未启用，用例前提不成立")
	}
	close(r.archive.stop) // 写循环收尾并关闭 done；不走 Close，所以 closed 仍为 false
	<-r.archive.done

	for i := 0; i < 20; i++ {
		errc := make(chan error, 1)
		go func() {
			_, err := r.Purge()
			errc <- err
		}()
		select {
		case err := <-errc:
			if err == nil {
				t.Fatal("写循环已停止时清空应返回错误")
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("第 %d 次清空卡住：写循环已退出，请求没人处理", i+1)
		}
	}
}
