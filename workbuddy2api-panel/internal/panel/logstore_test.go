// logstore_test.go 运行日志落盘 / 回填 / 导出的回归钉。
//
// 这些用例守的是「事故现场还在」这件事：内存环每频道 500 条、重启即归零，
// 落盘是唯一能让「昨天下午那波 429」还可查的东西。
package panel

import (
	"archive/zip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/cst"
)

// dayTS 指定自然日的某个时刻（CST）。
func dayTS(day string, hour int) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15", day+" "+string(rune('0'+hour/10))+string(rune('0'+hour%10)), cst.Loc)
	if err != nil {
		panic(err)
	}
	return t
}

// TestLogStoreWritesPerDayFiles 跨天必须切文件，且文件名按 CST 自然日。
func TestLogStoreWritesPerDayFiles(t *testing.T) {
	dir := t.TempDir()
	s := NewLogStore(dir, 7, 0)
	if s == nil {
		t.Fatal("NewLogStore 返回 nil")
	}
	defer s.Close()
	s.Append(LogEntry{TS: dayTS("2026-11-08", 23), Ch: ChChat, Text: "row-a"})
	s.Append(LogEntry{TS: dayTS("2026-11-09", 1), Ch: ChSys, Text: "row-b"})
	s.Flush()

	for _, day := range []string{"2026-11-08", "2026-11-09"} {
		path := filepath.Join(dir, "panel-"+day+".jsonl")
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s 未落盘: %v", day, err)
		}
	}
	// 读回来确认内容与频道都在（导出的可读性依赖这两个字段）。
	got := readLogFile(filepath.Join(dir, "panel-2026-11-09.jsonl"))
	if len(got) != 1 || got[0].Text != "row-b" || got[0].Ch != ChSys {
		t.Fatalf("11-09 文件内容 = %+v", got)
	}
}

// TestLogStoreRotatesOnMaxBytes 单文件超上限时换序号文件续写，而不是覆盖。
//
// 反向代价：若实现改成覆盖，当天日志只剩最后几行——而这恰恰是流量暴增、
// 最需要看全天日志的那一天。
func TestLogStoreRotatesOnMaxBytes(t *testing.T) {
	dir := t.TempDir()
	// 上限 1MiB 太大测不出来，直接用小值：NewLogStore 的 maxMB 是 MiB 单位，
	// 这里绕过构造函数手工给一个小上限。
	s := &LogStore{dir: dir, retentionDays: 7, maxBytes: 200}
	s.cleanup()
	ts := dayTS("2026-11-09", 12)
	for i := 0; i < 20; i++ {
		s.Append(LogEntry{TS: ts, Ch: ChSys, Text: "0123456789012345678901234567890123456789"})
	}
	s.Close()

	first := filepath.Join(dir, "panel-2026-11-09.jsonl")
	second := filepath.Join(dir, "panel-2026-11-09.2.jsonl")
	if _, err := os.Stat(first); err != nil {
		t.Fatalf("首个文件缺失: %v", err)
	}
	if _, err := os.Stat(second); err != nil {
		t.Fatalf("超限后未轮转到 .2 文件: %v", err)
	}
	// 上限是 200 字节（约 2 行），20 条会摊到多个序号文件里——统计**全部**匹配
	// 文件，断言一条都没丢。丢日志等于丢事故现场，这是这条用例唯一的重点。
	all, err := filepath.Glob(filepath.Join(dir, "panel-2026-11-09*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, path := range all {
		total += len(readLogFile(path))
	}
	if total != 20 {
		t.Fatalf("轮转后总条数 = %d want 20（共 %d 个文件；丢日志 = 丢事故现场）", total, len(all))
	}
}

// TestLogStoreBackfillNewestFirst 回填按时间升序返回最近的 limit 条。
func TestLogStoreBackfillNewestFirst(t *testing.T) {
	dir := t.TempDir()
	s := NewLogStore(dir, 7, 0)
	for i := 0; i < 5; i++ {
		s.Append(LogEntry{TS: dayTS("2026-11-08", 10).Add(time.Duration(i) * time.Minute), Ch: ChSys, Text: "old-" + string(rune('0'+i))})
	}
	for i := 0; i < 3; i++ {
		s.Append(LogEntry{TS: dayTS("2026-11-09", 10).Add(time.Duration(i) * time.Minute), Ch: ChSys, Text: "new-" + string(rune('0'+i))})
	}
	s.Close()

	// 重新打开（模拟重启），回填只看落盘文件。
	fresh := NewLogStore(dir, 7, 0)
	got := fresh.Backfill(4)
	if len(got) != 4 {
		t.Fatalf("回填条数 = %d want 4", len(got))
	}
	if got[0].Text != "old-4" || got[3].Text != "new-2" {
		t.Fatalf("回填顺序/内容 = %v ~ %v want old-4 ~ new-2（升序取最近）", got[0].Text, got[3].Text)
	}
	// 升序必须是严格成立的：环里的顺序就是面板展示顺序。
	for i := 1; i < len(got); i++ {
		if got[i].TS.Before(got[i-1].TS) {
			t.Fatalf("回填未按时间升序: %v 在 %v 之前", got[i].TS, got[i-1].TS)
		}
	}
}

// TestLogStoreExportFiltersByRange 导出按区间逐行裁剪，且只含区间内的行。
func TestLogStoreExportFiltersByRange(t *testing.T) {
	dir := t.TempDir()
	s := NewLogStore(dir, 7, 0)
	defer s.Close() // 不关会一直占着文件句柄，TempDir 清理在 Windows 上会失败
	base := dayTS("2026-11-09", 8)
	for i := 0; i < 6; i++ {
		s.Append(LogEntry{TS: base.Add(time.Duration(i) * time.Hour), Ch: ChSys, Text: "line-" + string(rune('0'+i))})
	}
	s.Append(LogEntry{TS: dayTS("2026-11-07", 8), Ch: ChSys, Text: "too-old"})

	tmp, err := os.CreateTemp(t.TempDir(), "export-*.zip")
	if err != nil {
		t.Fatal(err)
	}
	defer tmp.Close()
	files, err := s.Export(tmp, base.Add(time.Hour), base.Add(3*time.Hour))
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if files != 1 {
		t.Fatalf("导出文件数 = %d want 1（只该含 11-09 那天）", files)
	}
	entries, err := zip.OpenReader(tmp.Name())
	if err != nil {
		t.Fatalf("打开 zip: %v", err)
	}
	defer entries.Close()
	var lines []string
	for _, f := range entries.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		var rec struct{ Text string }
		dec := json.NewDecoder(rc)
		for dec.More() {
			if err := dec.Decode(&rec); err != nil {
				break
			}
			lines = append(lines, rec.Text)
		}
		rc.Close()
	}
	want := []string{"line-1", "line-2", "line-3"}
	if len(lines) != len(want) {
		t.Fatalf("区间内行数 = %d want %d（%v）", len(lines), len(want), lines)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("第 %d 行 = %q want %q", i, lines[i], want[i])
		}
	}
}

// TestLogStoreCleanupDropsExpired 保留期外的文件必须清掉（否则磁盘只增不减）。
func TestLogStoreCleanupDropsExpired(t *testing.T) {
	dir := t.TempDir()
	oldDay := cst.Now().AddDate(0, 0, -30).Format("2006-01-02")
	oldPath := filepath.Join(dir, "panel-"+oldDay+".jsonl")
	if err := os.WriteFile(oldPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	keepDay := cst.Now().Format("2006-01-02")
	keepPath := filepath.Join(dir, "panel-"+keepDay+".jsonl")
	if err := os.WriteFile(keepPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewLogStore(dir, 7, 0)
	if s == nil {
		t.Fatal("NewLogStore 返回 nil")
	}
	defer s.Close()
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("30 天前的文件应被清理: %v", err)
	}
	if _, err := os.Stat(keepPath); err != nil {
		t.Fatalf("当天文件不该被清理: %v", err)
	}
}

// TestRingBackfillsFromLogStore 挂上 store 后，重启的环必须能看到旧行。
//
// 这是「重启后还能查昨天」的端到端断言：只测 LogStore 自己的 Backfill 不够，
// 真正决定面板有没有内容的是一环接不接得上。
func TestRingBackfillsFromLogStore(t *testing.T) {
	dir := t.TempDir()
	store := NewLogStore(dir, 7, 0)
	store.Append(LogEntry{TS: cst.Now().Add(-time.Hour), Ch: ChChat, Text: "| #1 | old row"})
	store.Close()

	// 模拟重启：新 store + 新环，只靠落盘文件回填。
	store2 := NewLogStore(dir, 7, 0)
	defer store2.Close()
	r := NewRing(10)
	if err := r.SetLogStore(store2); err != nil {
		t.Fatalf("SetLogStore: %v", err)
	}
	snap := r.Snapshot()
	if len(snap) != 1 || snap[0].Text != "| #1 | old row" {
		t.Fatalf("回填后的环 = %+v want 一条旧 chat 行", snap)
	}
	// 新写入必须照常入环 + 落盘（回填不能把写路径挂掉）。
	r.Write([]byte("2026-11-09 10:00:00 fresh line\n"))
	store2.Flush()
	if got := r.Snapshot(); len(got) != 2 {
		t.Fatalf("新写入后环条数 = %d want 2", len(got))
	}
	// 落盘文件里必须出现刚才那行（回填的旧行同一天也在同一个文件里，故按内容找）。
	found := false
	for _, entry := range readLogFile(filepath.Join(dir, "panel-"+cst.Now().Format("2006-01-02")+".jsonl")) {
		// Ring.Write 只剥 log 包形态的时间戳（斜杠分隔），所以按包含判定。
		if strings.Contains(entry.Text, "fresh line") {
			found = true
		}
	}
	if !found {
		t.Fatal("新写入未落盘（回填不能把写路径挂掉）")
	}
}
