package reqlog

// 审查 10.14（W2-C7）reqlog 段落的回归用例。每条都对应一个审查编号：
//
//	C7-014 单条超大事件 → 每条都轮转、文件数无界   → TestArchiveTruncatesOversizedContext
//	C7-015 轮转路径 O(N²) os.Stat 探测             → TestArchiveRotationIndexIsSequential
//	C7-016/020 文件名用事件时区、prune 用本地时区   → TestArchiveFileDayUsesWriteTime
//	C7-017 保留期没有兜底、启动/滚动不清理          → TestArchiveStartupPrunesExpired /
//	                                                 TestArchiveRotationPrunesExpired /
//	                                                 TestArchiveRetentionDefaultDays
//	C7-018 窄 filter 回溯读完整目录                 → TestReadArchiveHonorsScanBudget
//	C7-019 尾部读 carry 拼接 O(k²)                  → 由 TestReadArchiveLongLines 覆盖
//	C7-021 Close 无超时                             → TestArchiveCloseReturnsPromptly
//	C7-026 单文件读失败静默 continue                → TestReadArchiveCountsReadErrors

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// archiveNames 列出目录里的归档文件名（测试断言用）。
func archiveNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读目录: %v", err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func mustWriteFile(t *testing.T, path string, body string, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写文件 %s: %v", path, err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("改 mtime %s: %v", path, err)
	}
}

// TestArchiveTruncatesOversizedContext 钉住 C7-014：
// 单条事件（大 context）超过 maxEventBytes 时必须被硬截断成 truncated 标记，
// 而不是原样写盘 —— 否则一条 2MB 的事件配上 1MB 的 FileMaxBytes 会让每条事件都
// 轮转出一个新文件，文件数/inode 无界增长。
func TestArchiveTruncatesOversizedContext(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Enabled: true, Dir: dir, FileMaxBytes: 1 << 20, MaxBytes: 1 << 30, RetentionDays: 7})
	big := strings.Repeat("x", 2<<20) // 2MiB 的 context
	for i := 0; i < 3; i++ {
		r.Record(Event{
			RequestID: "big-" + string(rune('a'+i)),
			Status:    200, OK: true, Outcome: OutcomeSuccess,
			Context: json.RawMessage(`{"messages":[{"role":"user","content":"` + big + `"}]}`),
		})
	}
	r.Close()

	names := archiveNames(t, dir)
	if len(names) != 1 {
		// 3 条 2MiB 的事件截断后每条只剩 ~200 字节，必须全部落在同一个文件里；
		// 截断失效时这里会看到 3 个文件（每条事件各轮转一次，正是 C7-014 的形态）。
		t.Fatalf("归档文件 = %v, want 1 个（截断后不该按事件轮转）", names)
	}
	var total int64
	for _, name := range names {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		total += st.Size()
		// 单个文件不该超过「上限 + 一条事件」，2MiB 的原文若原样落盘会到 2MB+。
		if st.Size() > (1<<20)+64<<10 {
			t.Fatalf("%s 大小 %d 超过 FileMaxBytes 太多（截断没生效）", name, st.Size())
		}
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), `"truncated":true`) {
			t.Fatalf("%s 里没有 truncated 标记：%.200s", name, body)
		}
	}
	if total > 3*((1<<20)+64<<10) {
		t.Fatalf("归档总量 %d 远超截断后的预期（3 条 ≈ 3MiB）", total)
	}
	if got := r.Snapshot().Archive.TruncatedWrites; got != 3 {
		t.Fatalf("truncated_writes = %d, want 3", got)
	}
}

// TestArchiveRotationIndexIsSequential 钉住 C7-015：轮转序号由内存自增维护，
// 文件名必须是 requests-<day>.jsonl → .1 → .2 …（而不是每次都从头 stat 探测）。
func TestArchiveRotationIndexIsSequential(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Enabled: true, Dir: dir, FileMaxBytes: 1, MaxBytes: 1 << 30, RetentionDays: 7})
	for i := 0; i < 4; i++ {
		r.Record(Event{RequestID: "e" + string(rune('a'+i)), Status: 200, OK: true, Outcome: OutcomeSuccess})
	}
	r.Close()

	day := time.Now().Format("2006-01-02")
	want := []string{
		"requests-" + day + ".jsonl",
		"requests-" + day + ".1.jsonl",
		"requests-" + day + ".2.jsonl",
		"requests-" + day + ".3.jsonl",
	}
	// os.ReadDir 是字典序（'.1' 排在 '.jsonl' 之前），所以按集合比较而不是按顺序。
	names := archiveNames(t, dir)
	if len(names) != len(want) {
		t.Fatalf("文件 = %v, want %v", names, want)
	}
	got := map[string]bool{}
	for _, n := range names {
		got[n] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Fatalf("文件 = %v, 缺 %s（轮转序号必须是 0,1,2,3 连续）", names, w)
		}
	}
}

// TestArchiveFileDayUsesWriteTime 钉住 C7-016 / C7-020：
// 归档文件名按**写入时刻的本地日**分桶，不按事件自带的时区。
//
// 否则：① 跨零点的迟到/乱序事件会让 day 反复切换（每来一条就 closeFile+ReadDir+
// OpenFile）；② prune 用 time.Now() 的本地日 cutoff 去比事件时区的文件名，边界日
// 整天的归档被多删或少删一天。
func TestArchiveFileDayUsesWriteTime(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Enabled: true, Dir: dir, MaxBytes: 1 << 20, RetentionDays: 7})
	// 事件时间故意给 3 天前的 UTC：按事件时间分桶会落到另一个日期。
	old := time.Now().UTC().AddDate(0, 0, -3)
	r.Record(Event{Time: old, RequestID: "late", Status: 200, OK: true, Outcome: OutcomeSuccess})
	r.Close()

	want := "requests-" + time.Now().Format("2006-01-02") + ".jsonl"
	names := archiveNames(t, dir)
	if len(names) != 1 || names[0] != want {
		t.Fatalf("归档文件 = %v, want [%s]（文件名必须按写入时刻的本地日）", names, want)
	}
	// 事件时间本身仍要原样落盘（面板按事件时间排序）。
	rows, err := r.ReadArchive(10, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].Time.Equal(old) {
		t.Fatalf("事件时间被改写：%+v want %v", rows, old)
	}
}

// TestArchiveStartupPrunesExpired 钉住 C7-017：New() 时就清理超出保留期的旧文件，
// 不等 1s ticker 的首轮 prune（重启后到那一轮之间旧归档仍在，换挂载卷恢复时更久）。
func TestArchiveStartupPrunesExpired(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	expired := filepath.Join(dir, "requests-"+now.AddDate(0, 0, -30).Format("2006-01-02")+".jsonl")
	fresh := filepath.Join(dir, "requests-"+now.AddDate(0, 0, -1).Format("2006-01-02")+".jsonl")
	mustWriteFile(t, expired, "old\n", now.AddDate(0, 0, -30))
	mustWriteFile(t, fresh, "new\n", now.AddDate(0, 0, -1))

	r := New(Config{Enabled: true, Dir: dir, MaxBytes: 1 << 30, RetentionDays: 7})
	defer r.Close()

	if _, err := os.Stat(expired); !os.IsNotExist(err) {
		t.Fatalf("启动清理没有删掉 30 天前的归档（err=%v）", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("保留期内的归档被误删：%v", err)
	}
}

// TestArchiveRotationPrunesExpired 钉住「每次滚动都清理」：
// 进程运行期间出现的过期文件（例如日期跨天）在第一次轮转时就被清掉，
// 不必等 ticker 的 10 分钟节流。
func TestArchiveRotationPrunesExpired(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Enabled: true, Dir: dir, FileMaxBytes: 64, MaxBytes: 1 << 30, RetentionDays: 7})
	defer r.Close()

	now := time.Now()
	stale := filepath.Join(dir, "requests-"+now.AddDate(0, 0, -20).Format("2006-01-02")+".jsonl")
	mustWriteFile(t, stale, "old\n", now.AddDate(0, 0, -20))

	for i := 0; i < 4; i++ {
		r.Record(Event{RequestID: "rot-" + string(rune('a'+i)), Status: 200, OK: true, Outcome: OutcomeSuccess})
	}
	// 轮转发生在写循环里：等到过期文件消失（最多 3s）即可。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(stale); os.IsNotExist(err) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("滚动没有清理过期归档文件")
}

// TestArchiveRetentionDefaultDays 钉住「保留 N 天（可配，缺省 7）」：
// RetentionDays<=0 必须回落 7，而不是让 cutoff 落在今天把当天归档删光。
func TestArchiveRetentionDefaultDays(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	today := filepath.Join(dir, "requests-"+now.Format("2006-01-02")+".jsonl")
	mustWriteFile(t, today, "keep\n", now)

	r := New(Config{Enabled: true, Dir: dir, MaxBytes: 1 << 30}) // RetentionDays 缺省
	defer r.Close()
	if got := r.archive.cfg.RetentionDays; got != defaultRetentionDays {
		t.Fatalf("RetentionDays = %d, want %d", got, defaultRetentionDays)
	}
	if _, err := os.Stat(today); err != nil {
		t.Fatalf("RetentionDays<=0 时当天的归档被删掉了：%v", err)
	}
	// 7 天前的文件仍在保留期内，8 天前的应当被清。
	inRange := filepath.Join(dir, "requests-"+now.AddDate(0, 0, -6).Format("2006-01-02")+".jsonl")
	outRange := filepath.Join(dir, "requests-"+now.AddDate(0, 0, -8).Format("2006-01-02")+".jsonl")
	mustWriteFile(t, inRange, "x\n", now.AddDate(0, 0, -6))
	mustWriteFile(t, outRange, "x\n", now.AddDate(0, 0, -8))
	r.archive.prune()
	if _, err := os.Stat(inRange); err != nil {
		t.Fatalf("6 天前的归档被误删：%v", err)
	}
	if _, err := os.Stat(outRange); !os.IsNotExist(err) {
		t.Fatalf("8 天前的归档没被清掉（err=%v）", err)
	}
}

// TestReadArchiveHonorsScanBudget 钉住 C7-018：窄 filter 命中稀疏时不再回溯读完整目录，
// 超过扫描预算就返回已读到的部分（面板只展示 limit 条，少读几个文件无感）。
func TestReadArchiveHonorsScanBudget(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	// 三个日期各一条不匹配 filter 的事件 + 最新文件一条匹配的。
	for i := 0; i < 3; i++ {
		day := now.AddDate(0, 0, -i-1).Format("2006-01-02")
		mustWriteFile(t, filepath.Join(dir, "requests-"+day+".jsonl"),
			`{"request_id":"old-`+day+`","status":200,"ok":true,"outcome":"success","model":"other"}`+"\n", now)
	}
	today := "requests-" + now.Format("2006-01-02") + ".jsonl"
	mustWriteFile(t, filepath.Join(dir, today),
		`{"request_id":"hit","status":200,"ok":true,"outcome":"success","model":"glm-5.3"}`+"\n", now)

	oldFiles, oldBytes := readScanFileBudget, readScanByteBudget
	readScanFileBudget = 1
	t.Cleanup(func() { readScanFileBudget, readScanByteBudget = oldFiles, oldBytes })

	r := New(Config{Enabled: true, Dir: dir, MaxBytes: 1 << 30, RetentionDays: 7})
	defer r.Close()
	rows, err := r.ReadArchive(50, Filter{Model: "glm"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].RequestID != "hit" {
		t.Fatalf("预算内应只读到最新文件的那条：%+v", rows)
	}
}

// TestReadArchiveLongLines 钉住 C7-019：单行跨越很多读块时（carry 反复拼接），
// 尾部读仍然只解析一次、且不把超长行当事件。这里用 1MiB 的单行（合法上限内）
// 触发多次跨块拼接。
func TestReadArchiveLongLines(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	// 900KiB 的 user_agent 字段：仍低于 maxEventBytes（合法归档行），但远超单块
	// 64KiB，跨十几个块拼接 —— 正是 C7-019 里 carry 反复复制的那条路径。
	pad := strings.Repeat("u", 900<<10)
	line := `{"request_id":"long","status":200,"ok":true,"outcome":"success","model":"glm-5.3","user_agent":"` + pad + `"}`
	mustWriteFile(t, filepath.Join(dir, "requests-"+now.Format("2006-01-02")+".jsonl"), line+"\n", now)

	r := New(Config{Enabled: true, Dir: dir, MaxBytes: 1 << 30, RetentionDays: 7})
	defer r.Close()
	rows, err := r.ReadArchive(5, Filter{Model: "glm"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].RequestID != "long" || len(rows[0].UserAgent) != 900<<10 {
		t.Fatalf("跨块长行没被正确还原：rows=%d ua=%d", len(rows), func() int {
			if len(rows) == 1 {
				return len(rows[0].UserAgent)
			}
			return -1
		}())
	}
}

// TestReadArchiveCountsReadErrors 钉住 C7-026：单文件读失败不再静默 continue，
// 而是计数并出现在 ArchiveStats.ReadErrors 里（否则「面板少数据」永远查不出原因）。
func TestReadArchiveCountsReadErrors(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	// 指向不存在目标的符号链接：ReadDir 能看到、IsDir() 为 false、os.Open 必失败。
	broken := filepath.Join(dir, "requests-"+now.AddDate(0, 0, -1).Format("2006-01-02")+".jsonl")
	if err := os.Symlink(filepath.Join(dir, "missing-target"), broken); err != nil {
		t.Skipf("本机不支持创建符号链接：%v", err)
	}
	mustWriteFile(t, filepath.Join(dir, "requests-"+now.Format("2006-01-02")+".jsonl"),
		`{"request_id":"ok","status":200,"ok":true,"outcome":"success","model":"glm-5.3"}`+"\n", now)

	r := New(Config{Enabled: true, Dir: dir, MaxBytes: 1 << 30, RetentionDays: 7})
	defer r.Close()
	rows, err := r.ReadArchive(10, Filter{Model: "glm"})
	if err != nil {
		t.Fatalf("单个坏文件不该让整页失败：%v", err)
	}
	if len(rows) != 1 || rows[0].RequestID != "ok" {
		t.Fatalf("好文件的事件应当照常返回：%+v", rows)
	}
	if got := r.Snapshot().Archive.ReadErrors; got == 0 {
		t.Fatal("坏文件没有被计数（read_errors 仍为 0）")
	}
}

// TestArchiveCloseReturnsPromptly 钉住 C7-021：Close 有上限，不会因为写循环卡住
// 而把进程退出无限拖住（这里只验证正常路径不被 2s 超时误伤）。
func TestArchiveCloseReturnsPromptly(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Enabled: true, Dir: dir, MaxBytes: 1 << 20, RetentionDays: 7})
	r.Record(Event{RequestID: "flush", Status: 200, OK: true, Outcome: OutcomeSuccess})
	start := time.Now()
	r.Close()
	if d := time.Since(start); d > closeFlushTimeout {
		t.Fatalf("Close 用了 %s，超过 closeFlushTimeout=%s", d, closeFlushTimeout)
	}
	rows, err := r.ReadArchive(5, Filter{})
	if err != nil || len(rows) != 1 || rows[0].RequestID != "flush" {
		t.Fatalf("Close 前的最后一批事件没落盘：rows=%+v err=%v", rows, err)
	}
}

// TestArchiveStatsCacheInvalidatedOnRotation 钉住 C7-023 的缓存契约：
// 统计在 5s 内复用（面板每 2s 轮询不该每次都 ReadDir + 全量 Stat），
// 但**新文件出现时必须立刻失效** —— 否则面板的「N 个文件」看起来卡住不动。
func TestArchiveStatsCacheInvalidatedOnRotation(t *testing.T) {
	dir := t.TempDir()
	w := &archiveWriter{
		cfg:  Config{Enabled: true, Dir: dir, RetentionDays: 7, MaxBytes: 1 << 30, FileMaxBytes: 1 << 20},
		done: make(chan struct{}),
	}
	defer w.closeFile()

	day := time.Now().Format("2006-01-02")
	if err := w.openFile(day, false); err != nil {
		t.Fatalf("openFile: %v", err)
	}
	if _, _, ok := w.cachedStats(); ok {
		t.Fatal("首次不应命中缓存")
	}
	files, bytes := w.scanStats()
	if files != 1 {
		t.Fatalf("扫描到 %d 个文件, want 1", files)
	}
	w.storeStats(files, bytes)
	if got, _, ok := w.cachedStats(); !ok || got != 1 {
		t.Fatalf("缓存未生效：files=%d ok=%v", got, ok)
	}
	// 轮转出新文件 → 缓存必须失效（openFile 里 invalidateStats）。
	if err := w.openFile(day, true); err != nil {
		t.Fatalf("openFile(rotate): %v", err)
	}
	if _, _, ok := w.cachedStats(); ok {
		t.Fatal("新文件出现后统计缓存仍被命中（面板会看到卡住的文件数）")
	}
}

// TestArchivePurgeAfterCloseReturnsImmediately 钉住 C7-022 的对外语义：
// 归档已停止时 Purge 必须立刻返回，不能卡在没人读的 purgeCh 上把 HTTP 连接挂死。
func TestArchivePurgeAfterCloseReturnsImmediately(t *testing.T) {
	r := New(Config{Enabled: true, Dir: t.TempDir(), RetentionDays: 7})
	r.Record(Event{RequestID: "x", Status: 200, OK: true, Outcome: OutcomeSuccess})
	r.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := r.Purge(); err != nil {
			t.Logf("Purge 返回错误（可接受）：%v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close 之后 Purge 卡住了（写循环已退出，没人读 purgeCh）")
	}
}
