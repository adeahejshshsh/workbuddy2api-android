package credithist

// 第二轮代码审计（CODE-AUDIT-FULL.md credithist 段 / W2-CH-01、W2-CH-02）的回归用例。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// ---------------------------------------------------------------- W2-CH-01 ----

// 审查 W2-CH-01：解析失败此前只打一行日志就从空账本开始，下一次 Observe 会用
// 「空 snapshot + 新流水」原地覆盖原文件，历史永久丢失。
func TestCorruptLedgerIsBackedUpBeforeOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credit_history.json")
	if err := os.WriteFile(path, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	l := New(path, 10)
	if len(l.Read(0)) != 0 {
		t.Fatalf("损坏文件应按空账本启动, 得到 %d 条", len(l.Read(0)))
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("损坏文件应已被改名（否则下一次 Observe 会原地覆盖）")
	}
	matches, err := filepath.Glob(filepath.Join(dir, "credit_history.json.corrupt.*"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("应有且只有一个备份文件, got=%v err=%v", matches, err)
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil || string(raw) != `{not json` {
		t.Fatalf("备份内容应原样保留: %q err=%v", raw, err)
	}

	// 备份之后 Observe 仍能正常落一份新账本（不因备份而瘫痪）。
	l.Observe("u1", 100)
	l.Observe("u1", 150)
	raw2, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("新账本应已落盘: %v", err)
	}
	var f diskFile
	if err := json.Unmarshal(raw2, &f); err != nil {
		t.Fatalf("新账本不是合法 JSON: %v", err)
	}
	if f.Snapshot["u1"] != 150 || len(f.Entries) != 1 {
		t.Fatalf("新账本内容不对: %+v", f)
	}
}

// ---------------------------------------------------------------- W2-CH-02 ----

// 审查 W2-CH-02：写盘挪到锁外后，只有「最新那次变更」允许 rename —— 否则并发下
// 慢的那次会把新内容覆盖成旧的（内存正确、重启后少一条）。
func TestWriteSkipsStaleSequence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credit_history.json")
	l := New(path, 10)

	// 模拟：seq=2 的写已经排上队，seq=1 的旧内容迟到。
	l.mu.Lock()
	l.seq = 2
	l.mu.Unlock()

	stale := []byte(`{"version":1,"snapshot":{"u1":1},"entries":[]}`)
	l.write(stale, 1)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("过期序号的写不该落盘（会覆盖更新的内容）")
	}
	// 临时文件不能残留（里面是明文账本）。
	if m, _ := filepath.Glob(path + ".tmp.*"); len(m) != 0 {
		t.Fatalf("过期写的临时文件应被清理: %v", m)
	}

	fresh := []byte(`{"version":1,"snapshot":{"u1":2},"entries":[]}`)
	l.write(fresh, 2)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("最新序号的写应落盘: %v", err)
	}
	if string(got) != string(fresh) {
		t.Fatalf("落盘内容=%s want=%s", got, fresh)
	}
}

// Observe 走锁外落盘后，顺序与内容仍必须正确（连续两次变更 → 文件是最后状态）。
func TestObservePersistsLatestStateAfterUnlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credit_history.json")
	l := New(path, 10)
	l.Observe("u1", 100)
	l.Observe("u1", 250)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f diskFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if f.Snapshot["u1"] != 250 {
		t.Fatalf("落盘基线=%d want 250", f.Snapshot["u1"])
	}
	if len(f.Entries) != 1 || f.Entries[0].Delta != 150 {
		t.Fatalf("落盘流水=%+v", f.Entries)
	}
}
