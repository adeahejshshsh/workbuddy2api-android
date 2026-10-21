package reqlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// bug #11：尾部读取必须与全量读的最后 N 条一致（含跨 64KiB 分块的半行拼接）。
func TestReadFileTailMatchesFullRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests-2026-10-21.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 60; i++ {
		line, _ := json.Marshal(Event{RequestID: "e" + strconv.Itoa(i), Time: base.Add(time.Duration(i) * time.Second)})
		if _, err := f.Write(append(line, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()

	full, err := readFile(path, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{1, 7, 59, 60, 100} {
		got, err := readFileTail(path, Filter{}, n)
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		want := n
		if want > len(full) {
			want = len(full)
		}
		if len(got) != want {
			t.Fatalf("n=%d 读到 %d 条，期望 %d", n, len(got), want)
		}
		tail := full[len(full)-want:]
		for i := range got {
			if got[i].RequestID != tail[i].RequestID {
				t.Fatalf("n=%d 第 %d 条是 %s，期望 %s（尾部读取顺序必须与全量一致）",
					n, i, got[i].RequestID, tail[i].RequestID)
			}
		}
	}
	// 过滤条件同样要生效（不能为了快就把 filter 丢了）
	got, err := readFileTail(path, Filter{Model: "no-such-model"}, 10)
	if err != nil || len(got) != 0 {
		t.Fatalf("过滤后 got=%d err=%v，期望 0 条", len(got), err)
	}
}

// bug #14：环写下标覆盖后，Snapshot 的顺序与淘汰语义必须仍然正确。
func TestRecentRingOrderAfterWrap(t *testing.T) {
	r := New(Config{})
	for i := 0; i < recentLimit+5; i++ {
		r.Record(Event{RequestID: "r" + strconv.Itoa(i), Status: 200, OK: true, Outcome: OutcomeSuccess})
	}
	s := r.Snapshot()
	if len(s.Recent) != recentLimit {
		t.Fatalf("Recent 长度 = %d，期望 %d", len(s.Recent), recentLimit)
	}
	if s.Recent[0].RequestID != "r"+strconv.Itoa(recentLimit+4) {
		t.Errorf("最新一条 = %s，期望 r%d（对外契约是新→旧）", s.Recent[0].RequestID, recentLimit+4)
	}
	if last := s.Recent[len(s.Recent)-1].RequestID; last != "r5" {
		t.Errorf("最旧一条 = %s，期望 r5（前 5 条应被环覆盖淘汰）", last)
	}
}

// bug #8：保留期必须按文件名日期判断 —— 目录被整体拷贝后 mtime 全是"现在"，
// 按 mtime 判断会让旧归档永不过期。
func TestPruneUsesFilenameDate(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "requests-2020-01-01.jsonl")
	if err := os.WriteFile(oldPath, []byte(`{"request_id":"x"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := New(Config{Enabled: true, Dir: dir, RetentionDays: 7, MaxBytes: 1 << 30})
	defer r.Close()
	r.archive.prune()
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("按文件名日期已过期（2020-01-01）的归档没被删，err=%v", err)
	}
}
