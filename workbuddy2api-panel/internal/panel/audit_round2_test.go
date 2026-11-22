package panel

// audit_round2_test.go 第二轮代码审计（panel）修复项的回归钉。
//
// 覆盖（按审查编号）：
//   W2-LS-01  LogStore 同日轮转不再退回基础文件 + 单日硬闸 + 文件名数值排序
//   W2-LS-02  超长行只跳过该行，不再终止整个文件的读取
//   W2-LS-03  落盘前 body=/Bearer 脱敏
//   W2-LS-05  跨天顺带清理过期文件
//   W2C4-26/27/28/29  作业快照深拷贝、jobSkip 不覆盖 running、running 行不入 Results、迟到回调丢弃
//   W2C4-19  FilePath 为空的账号不被轮询当缺席清理
//   W2C4-46/45  SetLogStore 幂等、单行截断
//   W2-AU-02 livecfg 快照两字段都空时回落静态配置（否则面板静默匿名可达）
//   W2-MD-01/02/03  读改写串行化、大小写重复键、模型名白名单
//   W4F1-077 model_rules/apply 单请求 add+del
//   W2-SM-01/05  API Key 派生 uid 唯一化、长度上限
//   W2-IM-01/02  导入深度/条数上限
//   W2-LG-01  登录会话表上限
//   W2C4-15   tasks/queue 请求体收紧

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/cst"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// ---------------------------------------------------------------------------
// W2-LS-01 / 02 / 03 / 05：LogStore
// ---------------------------------------------------------------------------

// TestLogStoreRotationNeverFallsBackToBaseFile W2-LS-01（核心）：
// 单文件超限必须换**新序号**，绝不能退回基础文件继续追加。
//
// 缺陷现场：旧实现只在 panel-<day>.2..99.jsonl 里找第一个不存在的序号；99 个
// 都占满后 path 保持基础文件 → 基础文件永久增长（磁盘写满）+ 每行一次 open/stat。
//
// 这里用 200 字节的单文件上限写 400 行（每文件约 3 行 → 100+ 个序号文件），
// 正好越过旧实现「2..99 全部占满」的临界点：旧代码此后把 path 留在基础文件上继续
// 追加，基础文件无界增长。
func TestLogStoreRotationNeverFallsBackToBaseFile(t *testing.T) {
	dir := t.TempDir()
	s := &LogStore{dir: dir, retentionDays: 7, maxBytes: 200}
	// 日期必须相对当下（与 logstore_test.go 的 dayAgo 同口径）：写死的 2026-11-09 一旦
	// 越过 retentionDays=7 的清理线，Append 跨天触发的 cleanup 会把这个文件删掉，
	// 后面的 Glob/Stat 就全部落空（实测 11-18 起必红）。
	day := dayAgo(2)
	ts := dayTS(day, 12)
	const lines = 400
	for i := 0; i < lines; i++ {
		s.Append(LogEntry{TS: ts, Ch: ChSys, Text: "0123456789012345678901234567890123456789"})
	}
	s.Close()

	all, err := filepath.Glob(filepath.Join(dir, "panel-"+day+"*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(all) < 100 {
		t.Fatalf("轮转文件数 = %d，want >100（必须越过旧实现 2..99 的临界点才能钉住 W2-LS-01）", len(all))
	}
	// ① 基础文件不许突破单文件上限（+1 行的余量）：这是「磁盘无界增长」的直接判据。
	base := filepath.Join(dir, "panel-"+day+".jsonl")
	info, err := os.Stat(base)
	if err != nil {
		t.Fatalf("基础文件缺失: %v", err)
	}
	// 余量按「一行」算：轮转在写入**前**判定（written>=maxBytes），所以单个文件最多
	// 超出一行的长度。288 字节 = 200 + 1 行，是正确实现的正常上界。
	if info.Size() > 200+200 {
		t.Fatalf("基础文件 %d 字节，已突破单文件上限 200（W2-LS-01：轮转退回基础文件）", info.Size())
	}
	// ② 一条都不许丢（单日上限 64MB 远未命中）。
	total := 0
	for _, path := range all {
		total += len(readLogFile(path))
	}
	if total != lines {
		t.Fatalf("轮转后总条数 = %d want %d（丢日志 = 丢事故现场）", total, lines)
	}
	// ③ 每个文件都不该超过上限太多（否则说明没轮转）。
	for _, path := range all {
		fi, serr := os.Stat(path)
		if serr != nil {
			continue
		}
		if fi.Size() > 200+200 {
			t.Fatalf("%s 有 %d 字节，超过单文件上限 200（未轮转）", filepath.Base(path), fi.Size())
		}
	}
}

// TestLogStoreDailyCapFreezesAndReports W2-LS-01（硬闸）：
// 单日总量超上限后必须丢弃并置 s.err，而不是继续写。
func TestLogStoreDailyCapFreezesAndReports(t *testing.T) {
	dir := t.TempDir()
	s := &LogStore{dir: dir, retentionDays: 7, maxBytes: 200, maxDayBytes: 400}
	defer s.Close()
	// 同 TestLogStoreRotationNeverFallsBackToBaseFile：日期必须相对当下，
	// 写死的 2026-11-09 一旦越过 retentionDays=7 的清理线就会被跨天 cleanup 删掉。
	day := dayAgo(2)
	ts := dayTS(day, 12)
	for i := 0; i < 100; i++ {
		s.Append(LogEntry{TS: ts, Ch: ChSys, Text: "0123456789012345678901234567890123456789"})
	}
	if s.dropped == 0 {
		t.Fatal("单日上限命中后应有丢弃计数（dropped=0）")
	}
	if s.Error() == "" {
		t.Fatal("单日上限命中后必须置 s.err（面板要能提示「日志已丢弃」）")
	}
	var total int64
	all, _ := filepath.Glob(filepath.Join(dir, "panel-"+day+"*.jsonl"))
	for _, path := range all {
		if fi, err := os.Stat(path); err == nil {
			total += fi.Size()
		}
	}
	if total > 400+80 {
		t.Fatalf("单日写入 %d 字节，超过 maxDayBytes=400（硬闸没生效）", total)
	}
	// 换天必须解冻（否则第二天再也不写日志）。
	next := dayAgo(1)
	s.Append(LogEntry{TS: dayTS(next, 1), Ch: ChSys, Text: "next day"})
	if got := readLogFile(filepath.Join(dir, "panel-"+next+".jsonl")); len(got) != 1 {
		t.Fatalf("换天后应恢复写入：%+v", got)
	}
}

// TestLogStoreFilesUseNumericSeqOrder W2-LS-01（排序）：
// panel-<day>.10.jsonl 必须排在 .2 之后（字典序会排到前面，回填顺序就乱了）。
func TestLogStoreFilesUseNumericSeqOrder(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"panel-2026-11-09.jsonl",
		"panel-2026-11-09.2.jsonl",
		"panel-2026-11-09.10.jsonl",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s := &LogStore{dir: dir, retentionDays: 7}
	got := s.filesSnapshot()
	if len(got) != 3 {
		t.Fatalf("文件数 = %d want 3", len(got))
	}
	want := []string{"panel-2026-11-09.jsonl", "panel-2026-11-09.2.jsonl", "panel-2026-11-09.10.jsonl"}
	for i, w := range want {
		if filepath.Base(got[i]) != w {
			t.Fatalf("第 %d 个文件 = %s want %s（序号要按数值排，不能按字典序）", i, filepath.Base(got[i]), w)
		}
	}
}

// TestLogStoreSkipsOverlongLineAndKeepsReading W2-LS-02：
// 一行超过读缓冲只跳过该行，后面的行必须照样读出来。
//
// 缺陷现场：bufio.Scanner 撞上超长 token 会返回 false 并终止整个文件的读取，
// 该行之后的内容全部丢失且没有任何提示。
func TestLogStoreSkipsOverlongLineAndKeepsReading(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "panel-2026-11-09.jsonl")
	huge, _ := json.Marshal(LogEntry{TS: dayTS("2026-11-09", 10), Ch: ChSys, Text: strings.Repeat("x", logStoreMaxLine+4096)})
	normal1, _ := json.Marshal(LogEntry{TS: dayTS("2026-11-09", 11), Ch: ChSys, Text: "before"})
	normal2, _ := json.Marshal(LogEntry{TS: dayTS("2026-11-09", 12), Ch: ChSys, Text: "after"})
	content := string(normal1) + "\n" + string(huge) + "\n" + string(normal2) + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got := readLogFile(path)
	if len(got) != 2 || got[0].Text != "before" || got[1].Text != "after" {
		t.Fatalf("超长行之后的日志被吞了：%+v", got)
	}
	entries, skipped := readLogFileReport(path)
	if len(entries) != 2 || skipped != 1 {
		t.Fatalf("应跳过 1 行超长行并保留 2 行：entries=%d skipped=%d", len(entries), skipped)
	}
}

// TestLogStoreSanitizesBodyAndBearer W2-LS-03：
// 落盘的文本不许带完整上游 body 与 Bearer 凭据（导出 ZIP 会原样外发）。
func TestLogStoreSanitizesBodyAndBearer(t *testing.T) {
	dir := t.TempDir()
	s := NewLogStore(dir, 7, 0)
	defer s.Close()
	long := strings.Repeat("用户原文", 200) // 远超 200 字节
	s.Append(LogEntry{TS: cst.Now(), Ch: ChSys,
		Text: "WARN: upstream 401 Authorization: Bearer abcdefghijklmnop body=" + long})
	day := cst.Now().Format("2006-01-02")
	got := readLogFile(filepath.Join(dir, "panel-"+day+".jsonl"))
	if len(got) != 1 {
		t.Fatalf("落盘条数 = %d want 1", len(got))
	}
	text := got[0].Text
	if strings.Contains(text, "abcdefghijklmnop") {
		t.Fatalf("Bearer 凭据未打码：%q", text)
	}
	if !strings.Contains(text, "Bearer ***") {
		t.Fatalf("Bearer 应替换成 ***：%q", text)
	}
	if len(text) > 400 {
		t.Fatalf("body= 未截断，落盘长度 %d（应为 200 字节量级）", len(text))
	}
	if !strings.HasPrefix(text, "WARN: upstream 401") {
		t.Fatalf("脱敏不该破坏行首排障信息：%q", text)
	}
}

// TestLogStoreCleanupRunsOnDayRollover W2-LS-05：
// 常驻进程跨天时要顺带清理保留期外的文件（此前只在启动时跑一次）。
func TestLogStoreCleanupRunsOnDayRollover(t *testing.T) {
	dir := t.TempDir()
	oldDay := cst.Now().AddDate(0, 0, -30).Format("2006-01-02")
	oldPath := filepath.Join(dir, "panel-"+oldDay+".jsonl")
	if err := os.WriteFile(oldPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 故意用 NewLogStore 之后再手工造旧文件：模拟「进程已经跑了很久」。
	s := NewLogStore(dir, 7, 0)
	defer s.Close()
	if err := os.WriteFile(oldPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.Append(LogEntry{TS: cst.Now(), Ch: ChSys, Text: "today"})
	s.Append(LogEntry{TS: cst.Now().AddDate(0, 0, 1), Ch: ChSys, Text: "tomorrow"})
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("跨天时应清理 30 天前的文件: %v", err)
	}
}

// ---------------------------------------------------------------------------
// W2C4-26/27/28/29：作业记录
// ---------------------------------------------------------------------------

// TestJobSnapshotDeepCopiesResults W2C4-26：
// 快照必须深拷贝 Results —— 否则锁外 json.Marshal 与执行体的 append 写同一底层数组。
func TestJobSnapshotDeepCopiesResults(t *testing.T) {
	p, _ := jobTestPanel(t)
	id := p.jobBegin("u1", "growth", 4)
	p.jobItem("u1", id, "a", "done", "")
	snap := p.taskJobs.snapshot()
	if len(snap) != 1 || len(snap[0].Results) != 1 {
		t.Fatalf("快照内容不对：%+v", snap)
	}
	before := snap[0].Results[0].Code
	// 快照之后继续写：快照里的 Results 不能被改动（浅拷贝会共享底层数组）。
	p.jobItem("u1", id, "b", "done", "")
	p.jobItem("u1", id, "c", "done", "")
	if len(snap[0].Results) != 1 || snap[0].Results[0].Code != before {
		t.Fatalf("快照的 Results 被后续写入改写（浅拷贝）：%+v", snap[0].Results)
	}
	if got := len(p.taskJobs.snapshot()[0].Results); got != 3 {
		t.Fatalf("新快照应有 3 条结果，got %d", got)
	}
}

// TestJobSkipDoesNotClobberRunningJob W2C4-27：
// 撞锁跳过不许覆盖同账号**正在运行**的作业记录。
func TestJobSkipDoesNotClobberRunningJob(t *testing.T) {
	p, _ := jobTestPanel(t)
	id := p.jobBegin("u1", "growth", 3)
	p.jobItem("u1", id, "a", "done", "")
	p.jobSkip("u1", "该账号有其它任务动作在执行，跳过")
	j, ok := p.taskJobs.get("u1")
	if !ok || j.JobID != id || j.Status != "running" {
		t.Fatalf("运行中的作业被 jobSkip 覆盖了：%+v", j)
	}
	// 作业正常收尾后，跳过记录才允许落（此时已不是 running）。
	p.jobEnd("u1", id, "")
	p.jobSkip("u1", "已跑完一轮，跳过")
	if j, _ := p.taskJobs.get("u1"); j.Status != "skipped" {
		t.Fatalf("作业结束后 jobSkip 应能写入，got %q", j.Status)
	}
}

// TestJobItemIgnoresRunningRowsAndLateCallbacks W2C4-28/29：
// running 行不入 Results、不抬 Total；作业终结后的迟到回调直接丢弃。
func TestJobItemIgnoresRunningRowsAndLateCallbacks(t *testing.T) {
	p, _ := jobTestPanel(t)
	id := p.jobBegin("u1", "growth", 3)
	p.jobItem("u1", id, "a", "running", "")
	j, _ := p.taskJobs.get("u1")
	if len(j.Results) != 0 || j.Total != 3 {
		t.Fatalf("running 行不该入 Results/抬 Total：results=%d total=%d", len(j.Results), j.Total)
	}
	if j.CurrentTask != "a" {
		t.Fatalf("running 应更新 CurrentTask，got %q", j.CurrentTask)
	}
	p.jobItem("u1", id, "a", "done", "")
	p.jobEnd("u1", id, "")
	// 迟到回调：作业已 done，不许再追加结果或改计数。
	p.jobItem("u1", id, "b", "error", "迟到的失败")
	j, _ = p.taskJobs.get("u1")
	if len(j.Results) != 1 || j.Failed != 0 || j.Status != "done" {
		t.Fatalf("作业终结后仍被写入：results=%d failed=%d status=%s", len(j.Results), j.Failed, j.Status)
	}
}

// ---------------------------------------------------------------------------
// W2C4-19：FilePath 为空的账号
// ---------------------------------------------------------------------------

// TestReloadKeepsAccountsWithoutFilePath W2C4-19：
// 没有磁盘文件的池内账号按「无法判定」处理，不许计入缺席后被 ParkMany 移出。
func TestReloadKeepsAccountsWithoutFilePath(t *testing.T) {
	dir := t.TempDir()
	pl := pool.New("")
	p := New(Config{Pool: pl, AuthDir: dir})
	p.SetReloadConfig(0, 2)
	// 内存态账号：FilePath 为空（例如 apikey/嵌入用法，或落盘失败只入内存的号）。
	pl.Add(&auth.Auth{UID: "mem1", AccessToken: "at", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	for round := 1; round <= 5; round++ {
		res := &ReloadResult{}
		p.reloadMu.Lock()
		p.removeMissingLocked(nil, false, res)
		p.reloadMu.Unlock()
		if res.Removed != 0 {
			t.Fatalf("第 %d 轮把 FilePath 为空的号移出了池（res=%+v）", round, res)
		}
	}
	if _, ok := pl.Status("mem1"); !ok {
		t.Fatal("FilePath 为空的账号被清理掉了（W2C4-19）")
	}
}

// ---------------------------------------------------------------------------
// W2C4-46 / 45：Ring
// ---------------------------------------------------------------------------

// TestRingSetLogStoreIsIdempotent W2C4-46：重复挂载不许二次回填（环里出现重复行）。
func TestRingSetLogStoreIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	store := NewLogStore(dir, 7, 0)
	store.Append(LogEntry{TS: cst.Now(), Ch: ChSys, Text: "only once"})
	store.Close()
	store2 := NewLogStore(dir, 7, 0)
	defer store2.Close()

	r := NewRing(10)
	if err := r.SetLogStore(store2); err != nil {
		t.Fatal(err)
	}
	if err := r.SetLogStore(store2); err != nil {
		t.Fatal(err)
	}
	if got := r.Snapshot(); len(got) != 1 {
		t.Fatalf("重复 SetLogStore 后环条数 = %d want 1（二次回填会造成重复行）", len(got))
	}
}

// TestRingTruncatesOverlongLine W2C4-45：单行必须截断，内存不按「条数 × 单行长度」放大。
func TestRingTruncatesOverlongLine(t *testing.T) {
	r := NewRing(4)
	if _, err := r.Write([]byte(strings.Repeat("y", ringMaxLineBytes*4) + "\n")); err != nil {
		t.Fatal(err)
	}
	snap := r.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("环条数 = %d want 1", len(snap))
	}
	if len(snap[0].Text) > ringMaxLineBytes+32 {
		t.Fatalf("单行长度 %d，超过上限 %d（W2C4-45）", len(snap[0].Text), ringMaxLineBytes)
	}
}

// ---------------------------------------------------------------------------
// W2-AU-02：apiKey 回落
// ---------------------------------------------------------------------------

// TestPanelAuthFallsBackToStaticKeyWhenSnapshotEmpty W2-AU-02：
// livecfg 快照两个字段都空时，必须回落到静态配置 —— 否则结构体零值语义
// （key=="" 即不鉴权）会让面板静默变成匿名可达。
func TestPanelAuthFallsBackToStaticKeyWhenSnapshotEmpty(t *testing.T) {
	p := New(Config{
		Pool:   pool.New(""),
		APIKey: "static-key",
		Live:   livecfg.New(livecfg.Snapshot{}), // 漏填 APIKey 的快照
	})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/overview", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("快照为空时应回落静态 key 并要求鉴权：code=%d want 401（匿名可达 = W2-AU-02）", rec.Code)
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/panel/api/overview", nil)
	req.Header.Set("Authorization", "Bearer static-key")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("静态 key 应放行：code=%d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// W2-MD-01 / 02 / 03：配置读改写
// ---------------------------------------------------------------------------

// TestModelEffortReadModifyWriteIsSerialized W2-MD-01：
// 两次并发设置必须串行落盘（否则后写者覆盖前者，用户另一项设置静默丢失）。
func TestModelEffortReadModifyWriteIsSerialized(t *testing.T) {
	var (
		inFlight    int32
		maxInFlight int32
		mu          sync.Mutex
		saves       int
	)
	p := New(Config{
		LoadConfig: func() (any, error) {
			return map[string]any{"thinking": map[string]any{"model_defaults": map[string]any{}}}, nil
		},
		SaveConfig: func(raw []byte) ([]string, error) {
			n := atomic.AddInt32(&inFlight, 1)
			for {
				cur := atomic.LoadInt32(&maxInFlight)
				if n <= cur || atomic.CompareAndSwapInt32(&maxInFlight, cur, n) {
					break
				}
			}
			time.Sleep(15 * time.Millisecond) // 放大竞态窗口
			atomic.AddInt32(&inFlight, -1)
			mu.Lock()
			saves++
			mu.Unlock()
			return nil, nil
		},
	})
	var wg sync.WaitGroup
	for _, id := range []string{"cn:a", "cn:b"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/panel/api/model_effort",
				strings.NewReader(`{"id":"`+id+`","effort":"high"}`))
			p.setModelEffort(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("%s: code=%d body=%s", id, rec.Code, rec.Body)
			}
		}(id)
	}
	wg.Wait()
	if got := atomic.LoadInt32(&maxInFlight); got != 1 {
		t.Fatalf("并发进入 SaveConfig 的最大深度 = %d want 1（读改写没有串行化，W2-MD-01）", got)
	}
	if saves != 2 {
		t.Fatalf("落盘次数 = %d want 2", saves)
	}
}

// TestModelEffortDropsCaseVariantKey W2-MD-02：
// 写入小写键前先删掉 EqualFold 相同的旧键，避免同一模型两条档位并存。
func TestModelEffortDropsCaseVariantKey(t *testing.T) {
	var saved map[string]any
	p := New(Config{
		LoadConfig: func() (any, error) {
			return map[string]any{"thinking": map[string]any{"model_defaults": map[string]any{
				"cn:GLM-5.2": "low", // 用户历史写法
				"glm-5.2":    "high",
			}}}, nil
		},
		SaveConfig: func(raw []byte) ([]string, error) {
			return nil, json.Unmarshal(raw, &saved)
		},
	})
	rec := httptest.NewRecorder()
	p.setModelEffort(rec, httptest.NewRequest("POST", "/panel/api/model_effort",
		strings.NewReader(`{"id":"cn:glm-5.2","effort":"max"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	thinking, _ := saved["thinking"].(map[string]any)
	got, _ := thinking["model_defaults"].(map[string]any)
	if _, dup := got["cn:GLM-5.2"]; dup {
		t.Fatalf("大小写重复键未清理：%v", got)
	}
	if got["cn:glm-5.2"] != "max" {
		t.Fatalf("精确键应为 max：%v", got)
	}
	if got["glm-5.2"] != "high" {
		t.Fatalf("裸名条目应原样保留：%v", got)
	}
}

// TestModelHiddenRejectsIllegalModelName W2-MD-03：模型名必须过白名单。
func TestModelHiddenRejectsIllegalModelName(t *testing.T) {
	saved := false
	p := New(Config{
		LoadConfig: func() (any, error) { return map[string]any{}, nil },
		SaveConfig: func([]byte) ([]string, error) { saved = true; return nil, nil },
	})
	for _, bad := range []string{
		"cn:glm 5.2",                     // 空格
		"cn:" + strings.Repeat("a", 200), // 超长
		"cn:glm\u0000x",                  // 控制字符
	} {
		rec := httptest.NewRecorder()
		p.setModelHidden(rec, httptest.NewRequest("POST", "/panel/api/model_hidden",
			strings.NewReader(`{"id":"`+strings.ReplaceAll(bad, "\u0000", "\\u0000")+`","hidden":true}`)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("id=%q code=%d want 400（W2-MD-03）", bad, rec.Code)
		}
	}
	if saved {
		t.Fatal("非法模型名仍落盘了")
	}
}

// ---------------------------------------------------------------------------
// W4F1-077：model_rules/apply 单请求 add + del
// ---------------------------------------------------------------------------

// newRulePanel 造一个带规则与两个账号的面板。
func newRulePanel(t *testing.T) (*Panel, *pool.Pool) {
	t.Helper()
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	pl.Add(&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	if _, err := pl.UpsertRule(pool.ModelRule{ID: "r1", Name: "贵模型", Mode: "deny", Models: []string{"m"}}); err != nil {
		t.Fatalf("UpsertRule: %v", err)
	}
	return New(Config{Pool: pl}), pl
}

// TestModelRuleApplyDeltaInOneRequest W4F1-077：
// 一次请求同时挂上 add、摘掉 del —— 前端此前分两条请求发，第一条成功、第二条失败
// 时挂载已部分生效而界面报「保存失败」，用户再点一次会把已生效的部分重发。
func TestModelRuleApplyDeltaInOneRequest(t *testing.T) {
	p, pl := newRulePanel(t)
	if _, err := pl.ApplyRule("r1", []string{"u2"}, true); err != nil {
		t.Fatalf("预置挂载: %v", err)
	}
	rec := httptest.NewRecorder()
	p.modelRuleApply(rec, httptest.NewRequest("POST", "/panel/api/model_rules/apply",
		strings.NewReader(`{"id":"r1","add":["u1"],"del":["u2"]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		Added   int `json:"added"`
		Removed int `json:"removed"`
		Changed int `json:"changed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if resp.Added != 1 || resp.Removed != 1 || resp.Changed != 2 {
		t.Fatalf("响应计数 = %+v want added=1 removed=1 changed=2", resp)
	}
	asg := pl.RuleAssignments()
	if len(asg["u1"]) != 1 || asg["u1"][0] != "r1" {
		t.Fatalf("u1 应挂上 r1：%v", asg)
	}
	if _, still := asg["u2"]; still {
		t.Fatalf("u2 应摘掉 r1：%v", asg)
	}
}

// TestModelRuleApplyDeltaUnknownRuleChangesNothing W4F1-077（原子性）：
// 规则不存在时一条都不许动（存在性校验在任何写入之前）。
func TestModelRuleApplyDeltaUnknownRuleChangesNothing(t *testing.T) {
	p, pl := newRulePanel(t)
	rec := httptest.NewRecorder()
	p.modelRuleApply(rec, httptest.NewRequest("POST", "/panel/api/model_rules/apply",
		strings.NewReader(`{"id":"nope","add":["u1"],"del":["u2"]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", rec.Code)
	}
	if asg := pl.RuleAssignments(); len(asg) != 0 {
		t.Fatalf("失败请求改动了挂载：%v", asg)
	}
}

// TestModelRuleBatchCaps W2-MD-04：批量数组必须有上限。
func TestModelRuleBatchCaps(t *testing.T) {
	p, _ := newRulePanel(t)
	many := make([]string, modelRuleMaxBatch+1)
	for i := range many {
		many[i] = "m"
	}
	body, _ := json.Marshal(map[string]any{"id": "r1", "name": "x", "mode": "deny", "models": many})
	rec := httptest.NewRecorder()
	p.modelRuleSave(rec, httptest.NewRequest("POST", "/panel/api/model_rules", strings.NewReader(string(body))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("超限 models code=%d want 400", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// W2-SM-01 / 05：API Key
// ---------------------------------------------------------------------------

// TestAPIKeyDerivedUIDUniqueOnTruncation W2-SM-01：
// 长 id 截断后必须带 key 的短哈希，否则两个前缀相同的 key 会撞同一个 uid，
// 第二个静默覆盖第一个的凭据文件。
func TestAPIKeyDerivedUIDUniqueOnTruncation(t *testing.T) {
	prefix := strings.Repeat("a", 90) // 远超 apiKeyUIDMaxLen
	k1 := "ck_" + prefix + ".secret-one"
	k2 := "ck_" + prefix + ".secret-two"
	u1, err := resolveAPIKeyUID("", k1)
	if err != nil {
		t.Fatal(err)
	}
	u2, err := resolveAPIKeyUID("", k2)
	if err != nil {
		t.Fatal(err)
	}
	if u1 == u2 {
		t.Fatalf("两个不同 key 派生出同一个 uid %q（W2-SM-01：后写者会静默覆盖前者的凭据）", u1)
	}
	for _, uid := range []string{u1, u2} {
		if len(uid) > apiKeyUIDMaxLen {
			t.Fatalf("uid 长度 %d 超过上限 %d", len(uid), apiKeyUIDMaxLen)
		}
		if !validImportUID(uid) {
			t.Fatalf("uid %q 不满足导入 uid 字符白名单（拼文件名的前置条件）", uid)
		}
	}
}

// TestAddAPIKeyRejectsOversizedToken W2-SM-05：超长 key 直接拒，不进日志/探测。
func TestAddAPIKeyRejectsOversizedToken(t *testing.T) {
	p := New(Config{Pool: pool.New(""), AuthDir: t.TempDir()})
	big := "ck_" + strings.Repeat("x", apiKeyMaxLen+10)
	body, _ := json.Marshal(map[string]string{"access_token": big})
	rec := httptest.NewRecorder()
	p.addAPIKey(rec, httptest.NewRequest("POST", "/panel/api/accounts/add_apikey", strings.NewReader(string(body))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// W2-IM-01 / 02：导入形状
// ---------------------------------------------------------------------------

// TestParseImportRejectsDeepNesting W2-IM-01：
// 深嵌套必须在解码前拒掉 —— 它会打穿 goroutine 栈（fatal error，recover 无效）。
func TestParseImportRejectsDeepNesting(t *testing.T) {
	deep := strings.Repeat("[", importMaxDepth+5) + strings.Repeat("]", importMaxDepth+5)
	if _, err := parseImportPayload([]byte(deep)); err == nil {
		t.Fatal("深嵌套没有被拒（W2-IM-01）")
	} else if !strings.Contains(err.Error(), "嵌套过深") {
		t.Fatalf("错误信息应说明嵌套过深，got %v", err)
	}
	// 正常形状必须照常解析（别把合法文件挡掉）。
	ok := `[{"id":"u1","uid":"u1","access_token":"at","refresh_token":"rt"}]`
	if got, err := parseImportPayload([]byte(ok)); err != nil || len(got) != 1 {
		t.Fatalf("正常导入被挡：%v %v", got, err)
	}
}

// TestParseImportRejectsTooManyEntries W2-IM-01/02：条数上限（同时兜住后置动作队列）。
func TestParseImportRejectsTooManyEntries(t *testing.T) {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i <= importMaxEntries; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"id":"u","uid":"u","access_token":"at","refresh_token":"rt"}`)
	}
	b.WriteString("]")
	if _, err := parseImportPayload([]byte(b.String())); err == nil {
		t.Fatalf("超过 %d 条没有拒绝（W2-IM-01/02）", importMaxEntries)
	}
}

// ---------------------------------------------------------------------------
// W2-LG-01：登录会话表上限
// ---------------------------------------------------------------------------

// TestLoginStartRejectsWhenSessionTableFull W2-LG-01：
// 会话表满时必须先拒（429），不能先去打一次上游 auth/state。
func TestLoginStartRejectsWhenSessionTableFull(t *testing.T) {
	p := New(Config{Pool: pool.New("")})
	p.loginMu.Lock()
	for i := 0; i < loginMaxSessions; i++ {
		p.logins["state-"+string(rune('a'+i%26))+string(rune('a'+i/26))] = loginSession{created: time.Now(), realm: "cn"}
	}
	p.loginMu.Unlock()
	if len(p.logins) < loginMaxSessions {
		t.Fatalf("预置会话数 = %d want %d", len(p.logins), loginMaxSessions)
	}
	rec := httptest.NewRecorder()
	// 上游为 nil：容量检查在打上游之前，所以不该 panic / 不该访问网络。
	p.loginStart(rec, httptest.NewRequest("POST", "/panel/api/login/start", strings.NewReader(`{}`)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("会话表满时 code=%d want 429（W2-LG-01）", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// W2C4-15：tasks/queue 请求体
// ---------------------------------------------------------------------------

// TestTasksRunQueueRejectsOversizedBody W2C4-15：
// 队列启动接口必须先 limitJSONBody，超大 body 回 400 而不是被解码。
func TestTasksRunQueueRejectsOversizedBody(t *testing.T) {
	p := New(Config{Pool: pool.New(""), AuthDir: t.TempDir()})
	huge := `{"concurrency":1,"pad":"` + strings.Repeat("z", panelMaxJSONBytes+1024) + `"}`
	rec := httptest.NewRecorder()
	p.tasksRunQueue(rec, httptest.NewRequest("POST", "/panel/api/tasks/queue", strings.NewReader(huge)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("超大 body code=%d want 400（W2C4-15）", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// 第二批：autotask / campaign / errors / import 的小修
// ---------------------------------------------------------------------------

// TestRefreshCreditsAsyncBoundedBySemaphore W2-AT-02：
// 领奖后的异步余额刷新必须有在飞上限（批量领奖 + 上游变慢时不许线性堆 goroutine）。
func TestRefreshCreditsAsyncBoundedBySemaphore(t *testing.T) {
	var calls int32
	up := &upstream.Client{
		HTTP: &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
			atomic.AddInt32(&calls, 1)
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`)),
				Header: http.Header{}, Request: r}, nil
		})},
		BillingBaseCN: "https://cn.example", WebBaseCN: "https://cn.example", ChatBaseCN: "https://cn.example",
	}
	p := New(Config{Pool: pool.New(""), Upstream: up})
	// 把 8 个槽全占满：后续调用必须**直接跳过**（不阻塞、不堆 goroutine）。
	for i := 0; i < cap(p.creditRefreshSem); i++ {
		p.creditRefreshSem <- struct{}{}
	}
	p.refreshCreditsAsync(&auth.Auth{UID: "u1", AccessToken: "at"})
	time.Sleep(20 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("在飞上限已满仍发起了 %d 次上游调用（W2-AT-02）", got)
	}
	// 释放一个槽后应能正常发起（不能把功能一起关掉）。
	<-p.creditRefreshSem
	p.refreshCreditsAsync(&auth.Auth{UID: "u1", AccessToken: "at"})
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got == 0 {
		t.Fatal("槽位释放后仍不发起刷新（功能被关掉了）")
	}
}

// TestCampaignClaimRejectsTooManyUIDs W2-CP-02：uids 数组必须有上限。
func TestCampaignClaimRejectsTooManyUIDs(t *testing.T) {
	p := New(Config{Pool: pool.New("")})
	uids := make([]string, campaignMaxUIDs+1)
	for i := range uids {
		uids[i] = "u"
	}
	body, _ := json.Marshal(map[string]any{"module": "m", "code": "c", "uids": uids})
	rec := httptest.NewRecorder()
	p.campaignClaim(rec, httptest.NewRequest("POST", "/panel/api/campaign/claim", strings.NewReader(string(body))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("超限 uids code=%d want 400", rec.Code)
	}
}

// TestCampaignClaimWithoutUpstreamReturns503 W2-CP-04：
// 上游缺失时回 503，不许 nil 解引用 panic。
func TestCampaignClaimWithoutUpstreamReturns503(t *testing.T) {
	p := New(Config{Pool: pool.New("")})
	rec := httptest.NewRecorder()
	p.campaignClaim(rec, httptest.NewRequest("POST", "/panel/api/campaign/claim",
		strings.NewReader(`{"module":"m","code":"c","dry_run":true}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503", rec.Code)
	}
}

// TestAggregateErrorsBackfillsCode W2-ER-01：
// 首行无 code、后续同类行有 code 时，聚合行必须补上 code（否则按码检索失效）。
func TestAggregateErrorsBackfillsCode(t *testing.T) {
	now := time.Now()
	rows := AggregateErrors([]LogLine{
		{Text: `WARN: [server] transport error acct=u1 model=m1: dial timeout`, TS: now},
		{Text: `WARN: [server] transport error acct=u1 model=m1: dial timeout "code":5001`, TS: now},
	})
	if len(rows) != 1 {
		t.Fatalf("聚合行数 = %d want 1：%+v", len(rows), rows)
	}
	if rows[0].Code != "5001" {
		t.Fatalf("聚合行 Code = %q want 5001（W2-ER-01：后续行的 code 要补上）", rows[0].Code)
	}
	if rows[0].Count != 2 {
		t.Fatalf("Count = %d want 2", rows[0].Count)
	}
}

// TestSanitizeNicknameStripsControlChars W2-IM-03：
// 导入昵称里的换行/控制字符必须去掉（否则可以伪造整行日志），并截断到上限。
func TestSanitizeNicknameStripsControlChars(t *testing.T) {
	got := sanitizeNickname("  老王\nWARN: 伪造的一行\x00\t" + strings.Repeat("长", 100) + "  ")
	if strings.ContainsAny(got, "\n\r\t\x00") {
		t.Fatalf("控制字符未清理：%q", got)
	}
	if n := len([]rune(got)); n > importNicknameMaxRunes {
		t.Fatalf("昵称长度 %d 超过上限 %d", n, importNicknameMaxRunes)
	}
	if !strings.HasPrefix(got, "老王WARN") {
		t.Fatalf("清理后应保留可见正文：%q", got)
	}
}
