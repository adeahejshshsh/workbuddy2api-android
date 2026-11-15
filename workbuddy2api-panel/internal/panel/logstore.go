// logstore.go 面板「运行日志」的按天 JSONL 落盘、重启回填与按区间导出。
//
// 为什么需要（此前只有内存环 + 任务频道的一份 JSON 快照）：
//
//	环形缓冲每频道 500 条，对话一多就把上午的行冲掉；进程一重启全部归零。
//	于是「昨天下午那波 429 是什么时候开始的」这类问题，只能靠当时有没有人截屏。
//	事故复盘要看的是**过去几天的原始行**，这是一个纯粹的可观测性缺口。
//
// 与 reqlog 的分工（不要合并）：
//
//	reqlog      逐请求的**结构化元数据**（模型/账号/时延/结果），用于统计与筛选
//	写入约束：每行只走一次 write（O_APPEND 下单次 write 不与同机另一个进程的追加交错）。
//	不能用 bufio 攒批：缓冲满时一行会被拆进两次 write，与另一个进程的行交错成坏行；
//	进程被杀时缓冲里最多 48KB 也会丢，而事故现场恰恰在最后几行。
//	Append 只由 Ring 的后台 goroutine 调用，不在 log 输出路径上。
//	logstore    面板三频道（chat/task/sys）的**原始文本行**，用于照着日志读事故
//
// 与 Ring.SetTaskArchive 的关系：那份 JSON 只存任务频道、只存最近 500 条、跨天即丢。
// 本文件上线后由它接管回填（见 Ring.SetLogStore）；task-logs.json 保留为落盘目录
// 不可用时的兜底，不再作为主路径。
package panel

import (
	"archive/zip"
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/linguo2625469/workbuddy2api-panel/internal/cst"
)

const (
	// logStoreDefaultDays / logStoreDefaultMaxMB 配置缺省（与 reqlog 的默认量级对齐）。
	logStoreDefaultDays  = 7
	logStoreDefaultMaxMB = 50
	// logStoreBackfillMax 回填上限（条）。环路只有 500/频道，回填更多也留不住。
	logStoreBackfillMax = 3000
	// logStoreDailyCapMul / Min / Max 单日总写入上限（审查 W2-LS-01 的硬闸）：
	// 单文件上限只能约束「一个文件多大」，约束不了「一天能写多少」—— 序号用尽或
	// 写满上限后继续追加，磁盘就是无界的。这里按「单文件上限 × 4」推导，夹在
	// [64MiB, 512MiB]：默认配置（50MiB/文件）即 200MiB/天，保留 7 天 ≈ 1.4GiB 封顶。
	// 超限后**丢弃当日后续行并置 s.err**（宁缺日志，也不写满设备 —— Android 上
	// 写满 /data 是整机级故障）。
	logStoreDailyCapMul = 4
	logStoreDailyCapMin = 64 << 20
	logStoreDailyCapMax = 512 << 20
	// logStoreSeqMax 同日序号上限。序号本身不再限 99（旧实现在 99 个文件占满后
	// 退回基础文件继续追加，见 W2-LS-01）；这里留一个纯防御性上界，正常情况下
	// 单日上限先命中，永远到不了这个数。
	logStoreSeqMax = 1 << 16
	// logStoreMaxLine 单行读上限（超过即跳过该行并继续读后面的行，见 W2-LS-02）。
	logStoreMaxLine = 64 << 10
)

// LogStore 面板运行日志的按天落盘存储。目录不可用时降级为纯内存，绝不阻塞主流程。
type LogStore struct {
	mu            sync.Mutex
	dir           string
	retentionDays int
	maxBytes      int64 // 单文件上限（0 = 不限，测试会手工设成很小的值）
	// maxDayBytes 单日写入上限（0 = 由 maxBytes 推导，见 dailyCapLocked）。
	// 单独留字段是为了让用例能直接给一个小值验证「超限后丢弃」。
	maxDayBytes int64
	file        *os.File
	day         string // 当前文件对应的自然日（CST，与切天口径一致）
	seq         int    // 当前文件的同日序号（0 = panel-<day>.jsonl，1 = .2.jsonl …）
	written     int64  // 当前文件已写字节（上限判定用，不再逐行 stat）
	dayBytes    int64  // 当日累计写入字节（单日上限判定用）
	dropped     int64  // 当日因超上限被丢弃的行数（面板可展示）
	// frozen 当日已触发硬闸：后续行直接丢弃，避免每行一次 open/stat 风暴。
	// 换天时复位。
	frozen bool
	err    string // 最近一次写盘/丢弃原因（面板可提示）
}

// NewLogStore 创建存储并做一次保留期清理。dir 为空或建目录失败时返回 nil
// （调用方据此退回纯内存行为）。retentionDays<=0 回退 7，maxMB<=0 回退 50。
func NewLogStore(dir string, retentionDays, maxMB int) *LogStore {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	if retentionDays <= 0 {
		retentionDays = logStoreDefaultDays
	}
	if maxMB <= 0 {
		maxMB = logStoreDefaultMaxMB
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil
	}
	maxBytes := int64(maxMB) << 20
	s := &LogStore{
		dir: dir, retentionDays: retentionDays, maxBytes: maxBytes,
		maxDayBytes: dailyCapFor(maxBytes),
	}
	s.cleanup()
	return s
}

// dailyCapFor 由单文件上限推导单日上限（W2-LS-01 的硬闸）。
func dailyCapFor(maxBytes int64) int64 {
	if maxBytes <= 0 {
		return logStoreDailyCapMax
	}
	cap := maxBytes * logStoreDailyCapMul
	if cap < logStoreDailyCapMin {
		cap = logStoreDailyCapMin
	}
	if cap > logStoreDailyCapMax {
		cap = logStoreDailyCapMax
	}
	return cap
}

// dailyCapLocked 当前生效的单日上限；0 = 不限。
func (s *LogStore) dailyCapLocked() int64 {
	if s.maxDayBytes > 0 {
		return s.maxDayBytes
	}
	if s.maxBytes <= 0 {
		return 0
	}
	return dailyCapFor(s.maxBytes)
}

// Dir 落盘目录（导出时用于展示，测试也可断言）。
func (s *LogStore) Dir() string {
	if s == nil {
		return ""
	}
	return s.dir
}

// Error 最近一次写盘失败原因（空 = 正常）。面板 logsHandler 透出。
func (s *LogStore) Error() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// pathForDay 某自然日的日志文件路径。
func (s *LogStore) pathForDay(day string) string {
	return filepath.Join(s.dir, "panel-"+day+"."+logStoreExt)
}

// logStoreExt 文件扩展名（导出 ZIP 时按它挑文件）。
const logStoreExt = "jsonl"

// Append 追加一条日志；跨天自动切文件，超单文件上限时换下一个序号文件。
//
// 失败一律只记 s.err 不返回错误：日志系统不能反过来拖垮在处理的请求。
//
// 三道闸（W2-LS-01）：
//  1. 单文件上限 → 换**下一个序号**（.2/.3/…，序号不再限 99，旧实现在 99 个文件
//     占满后退回基础文件继续 O_APPEND，基础文件于是永久增长 + 每行一次 open/stat）；
//  2. 单日上限 → 丢弃当日后续行并置 s.err（磁盘无界增长的最终止损）；
//  3. 跨天 → 顺带跑一次保留期清理（常驻进程不重启也能删旧文件，W2-LS-05）。
func (s *LogStore) Append(e LogEntry) {
	if s == nil || strings.TrimSpace(e.Text) == "" {
		return
	}
	// 落盘前脱敏（W2-LS-03）：面板日志与导出 ZIP 是「对话内容/上游错误体」的持久
	// 副本，保留 7 天。body= 后的原文截断、Bearer 凭据打码 —— 落盘只保留排障需要的
	// 头部信息，不把用户原文与密钥写进可外发的包。
	e.Text = sanitizeLogText(e.Text)
	s.mu.Lock()
	defer s.mu.Unlock()

	day := e.TS.In(cst.Loc).Format("2006-01-02")
	if s.frozen && day == s.day {
		s.dropped++
		return
	}
	if s.file == nil || day != s.day {
		if !s.openDayLocked(day) {
			return
		}
	} else if s.maxBytes > 0 && s.written >= s.maxBytes {
		if !s.rotateSameDayLocked(day) {
			return
		}
	}
	line, err := json.Marshal(e)
	if err != nil {
		s.err = err.Error()
		return
	}
	line = append(line, '\n')
	if cap := s.dailyCapLocked(); cap > 0 && s.dayBytes+int64(len(line)) > cap {
		// 单日硬闸：此后当天的行全部丢弃，不再逐行尝试（否则每行一次 open/stat）。
		s.frozen = true
		s.dropped++
		s.err = fmt.Sprintf("单日日志量已达上限 %d 字节，当日后续行丢弃（W2-LS-01）", cap)
		return
	}
	n, err := s.file.Write(line) // 一行一次 write，见文件头
	s.written += int64(n)
	s.dayBytes += int64(n)
	if err != nil {
		s.err = err.Error()
		return
	}
	s.err = ""
}

// seqPath 同一天第 seq 个日志文件的路径（seq<=0 = 基础文件）。
func (s *LogStore) seqPath(day string, seq int) string {
	if seq <= 0 {
		return s.pathForDay(day)
	}
	return filepath.Join(s.dir, fmt.Sprintf("panel-%s.%d.%s", day, seq, logStoreExt))
}

// openDayLocked 切到指定自然日：换天时重置单日计数/序号，并顺带清理过期文件。
// 返回 false 表示文件打不开（已置 s.err）。
func (s *LogStore) openDayLocked(day string) bool {
	newDay := day != s.day
	s.closeLocked()
	if newDay {
		s.dayBytes = 0
		s.seq = 0
		s.frozen = false
		s.err = ""
		// 跨天清理（W2-LS-05）：此前 cleanup 只在 NewLogStore 跑一次，常驻数月的
		// 进程里旧文件永不删除，与单日上限叠加后仍然能吃掉整块磁盘。
		if s.day != "" {
			s.cleanupLocked()
		}
	}
	s.day = day
	if !s.openFileLocked(s.seqPath(day, 0)) {
		return false
	}
	// 重启后当天的基础文件可能已经写满：直接进入序号轮转（否则又会长到两倍）。
	if s.maxBytes > 0 && s.written >= s.maxBytes {
		return s.rotateSameDayLocked(day)
	}
	return true
}

// rotateSameDayLocked 单文件超限：关掉当前文件，换下一个**未写满**的序号文件。
//
// 与旧实现的区别（W2-LS-01）：序号不再有 99 的硬上限，且判定用「文件已满」
// 而不是「文件不存在」—— 旧实现在 99 个文件都存在时找不到候选，path 保持基础
// 文件，于是 50MB 的基础文件被永久追加，同时每行一次 open+stat。
func (s *LogStore) rotateSameDayLocked(day string) bool {
	s.closeLocked()
	for s.seq+1 < logStoreSeqMax {
		s.seq++
		path := s.seqPath(day, s.seq)
		if info, err := os.Stat(path); err == nil && s.maxBytes > 0 && info.Size() >= s.maxBytes {
			continue // 该序号已写满：继续往后找，绝不退回基础文件
		}
		if s.openFileLocked(path) {
			s.day = day
			return true
		}
		return false
	}
	s.frozen = true
	s.day = day
	s.err = "当日日志文件序号用尽，后续行丢弃（W2-LS-01）"
	return false
}

// openFileLocked 以 O_APPEND 打开文件，并把既有大小算进上限（重启续写口径）。
func (s *LogStore) openFileLocked(path string) bool {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		s.err = err.Error()
		s.day = ""
		s.file = nil
		return false
	}
	// 续写已有文件时把既有大小算进上限，否则重启后同一天的文件会长到两倍。
	if info, statErr := f.Stat(); statErr == nil {
		s.written = info.Size()
	} else {
		s.written = 0
	}
	s.file = f
	return true
}

// closeLocked 关闭当前文件。调用方必须持 s.mu。
func (s *LogStore) closeLocked() {
	if s.file != nil {
		_ = s.file.Close()
		s.file = nil
	}
}

// Flush 现在没有「内存里未落盘的尾巴」可刷（每行一次 write，见文件头）。
// 排队的行在 Ring 那一层，导出前要刷的是 Ring.Flush()。
func (s *LogStore) Flush() {}

// Close 刷盘并关闭（进程退出时调用）。
func (s *LogStore) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeLocked()
	return nil
}

// files 返回目录内的日志文件，按（自然日, 同天序号）升序。
//
// 排序不能用纯字典序（W2-LS-01 附带）：panel-2026-11-09.10.jsonl 会排在
// panel-2026-11-09.2.jsonl 前面，回填/导出拿到的顺序就乱了。这里解析出序号按数值排。
func (s *LogStore) files() []string {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	type item struct {
		path string
		day  string
		seq  int
	}
	items := make([]item, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "panel-") || !strings.HasSuffix(name, "."+logStoreExt) {
			continue
		}
		day, seq := logFileParts(name)
		if day == "" {
			continue
		}
		items = append(items, item{path: filepath.Join(s.dir, name), day: day, seq: seq})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].day != items[j].day {
			return items[i].day < items[j].day
		}
		return items[i].seq < items[j].seq
	})
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.path)
	}
	return out
}

// filesSnapshot 取文件列表快照（持锁）。导出用（W2-LS-04）：不持锁读目录与
// rotateLocked 的创建/关闭并发，导出的「最后一天」可能缺末尾几行。
func (s *LogStore) filesSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.files()
}

// logFileParts 解析 panel-<day>[.<seq>].jsonl → (day, seq)。畸形名返回空 day。
func logFileParts(name string) (string, int) {
	base := strings.TrimSuffix(name, "."+logStoreExt)
	base = strings.TrimPrefix(base, "panel-")
	if len(base) < 10 {
		return "", 0
	}
	day := base[:10]
	if len(base) == 10 {
		return day, 0
	}
	if base[10] != '.' {
		return "", 0
	}
	seq, err := strconv.Atoi(base[11:])
	if err != nil || seq <= 0 {
		return "", 0
	}
	return day, seq
}

// dayOfFile 从文件名取自然日（panel-2026-11-09.jsonl / panel-2026-11-09.2.jsonl）。
func dayOfFile(path string) string {
	day, _ := logFileParts(filepath.Base(path))
	return day
}

// Backfill 读最近的文件，按时间升序返回最多 limit 条（limit<=0 回退上限）。
//
// 从**最新**的文件倒着读、读够就停：回填的目的是「重启后还能看到最近的日志」，
// 不是重建全部历史。读失败的文件跳过（一个坏文件不该让面板日志整页空掉）。
func (s *LogStore) Backfill(limit int) []LogEntry {
	if s == nil {
		return nil
	}
	if limit <= 0 || limit > logStoreBackfillMax {
		limit = logStoreBackfillMax
	}
	s.mu.Lock()
	files := s.files()
	s.mu.Unlock()

	var out []LogEntry
	skippedTotal := 0
	for i := len(files) - 1; i >= 0 && len(out) < limit; i-- {
		chunk, skipped := readLogFileReport(files[i])
		skippedTotal += skipped
		// 从最新文件往前拼，插到前面保持全局升序。
		if need := limit - len(out); len(chunk) > need {
			chunk = chunk[len(chunk)-need:]
		}
		out = append(chunk, out...)
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	if skippedTotal > 0 {
		// 超长行被跳过要留痕（W2-LS-02）：此前 Scanner 撞上 1MB 行会静默终止整个
		// 文件的读取，回填与导出都「少一段」且没有任何提示。
		s.mu.Lock()
		s.err = fmt.Sprintf("回填跳过 %d 行超长日志（单行 >%d 字节）", skippedTotal, logStoreMaxLine)
		s.mu.Unlock()
	}
	return out
}

// scanLogLines 逐行读取（含 \n），超长行跳过并继续读后续行。
//
// 用 bufio.Reader.ReadSlice 而不是 bufio.Scanner：Scanner 撞到超长 token 会返回
// false 并**终止整个文件**的读取（W2-LS-02），而 ReadSlice 返回 ErrBufferFull 时
// 还能把这一行抽干后接着读下一行。返回被跳过的超长行数。
func scanLogLines(f *os.File, fn func(line []byte)) (int, error) {
	br := bufio.NewReaderSize(f, logStoreMaxLine)
	skipped := 0
	for {
		line, rerr := br.ReadSlice('\n')
		if rerr == bufio.ErrBufferFull {
			// 一行超过读缓冲：抽干到行尾，跳过这一行（保留半截 JSON 没有意义），
			// 关键是**不要让这一行吃掉文件剩下的内容**。
			for rerr == bufio.ErrBufferFull {
				_, rerr = br.ReadSlice('\n')
			}
			skipped++
			if rerr == io.EOF {
				return skipped, nil
			}
			if rerr != nil {
				return skipped, rerr
			}
			continue
		}
		if len(line) > 0 {
			fn(line)
		}
		if rerr == io.EOF {
			return skipped, nil
		}
		if rerr != nil {
			return skipped, rerr
		}
	}
}

// readLogFile 读一个 JSONL 文件，跳过坏行（签名保持给既有用例用）。
func readLogFile(path string) []LogEntry {
	out, _ := readLogFileReport(path)
	return out
}

// readLogFileReport 读文件并回报被跳过的超长行数（W2-LS-02）。
func readLogFileReport(path string) ([]LogEntry, int) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0
	}
	defer f.Close()
	var out []LogEntry
	skipped, _ := scanLogLines(f, func(line []byte) {
		s := strings.TrimSpace(string(line))
		if s == "" {
			return
		}
		var e LogEntry
		if json.Unmarshal([]byte(s), &e) != nil || e.Text == "" {
			return
		}
		out = append(out, e)
	})
	return out, skipped
}

// Export 把 [from, to] 区间内的日志打成 ZIP 写到 w，返回写入的文件数。
//
// 按天选文件（文件名即日期），再按行内的 TS 精确裁剪——这样跨天区间不会带进
// 区间外的行，且不必把全部历史读进内存两遍（每行独立判定，直接流式写 zip）。
func (s *LogStore) Export(w *os.File, from, to time.Time) (int, error) {
	if s == nil {
		return 0, fmt.Errorf("日志落盘未启用")
	}
	s.Flush()
	// 文件列表在锁内取快照（W2-LS-04）：导出与超限轮转并发时，不持锁读目录
	// 会让「最后一天」少末尾几行。
	zw := zip.NewWriter(w)
	written := 0
	for _, path := range s.filesSnapshot() {
		day := dayOfFile(path)
		if day == "" {
			continue
		}
		fileDay, err := time.ParseInLocation("2006-01-02", day, cst.Loc)
		if err != nil {
			continue
		}
		// 快速跳过完全落在区间外的日子（一天一个文件，含超限轮转的序号文件）。
		if fileDay.AddDate(0, 0, 1).Before(from) || fileDay.After(to) {
			continue
		}
		if err := copyFilteredToZip(zw, path, filepath.Base(path), from, to); err != nil {
			_ = zw.Close()
			return written, err
		}
		written++
	}
	if err := zw.Close(); err != nil {
		return written, err
	}
	return written, nil
}

// copyFilteredToZip 把一个日志文件里落在区间内的行写进 zip。
func copyFilteredToZip(zw *zip.Writer, path, name string, from, to time.Time) error {
	src, err := os.Open(path)
	if err != nil {
		return nil // 单个文件读不了不拖累整体导出
	}
	defer src.Close()
	dst, err := zw.Create(name)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(dst, 64<<10)
	defer bw.Flush()
	_, scanErr := scanLogLines(src, func(line []byte) {
		if len(line) == 0 {
			return
		}
		var probe struct {
			TS time.Time `json:"ts"`
		}
		// 只解析 ts：整行反序列化再重排是白费（一行最多几 KB，但一天上万行）。
		if json.Unmarshal(line, &probe) != nil {
			return
		}
		if probe.TS.Before(from) || probe.TS.After(to) {
			return
		}
		if _, werr := bw.Write(line); werr != nil {
			return
		}
		_ = bw.WriteByte('\n')
	})
	// 读错误（含超长行被跳过）不该让整次导出失败：已经写进去的行仍然有价值。
	_ = scanErr
	return nil
}

// cleanup 删除保留期之外的文件（启动时跑一次；跨天时由 Append 顺带再跑，W2-LS-05）。
func (s *LogStore) cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked()
}

// cleanupLocked 删除保留期之外的文件。调用方持 s.mu。
func (s *LogStore) cleanupLocked() {
	cutoff := cst.Now().AddDate(0, 0, -s.retentionDays).Format("2006-01-02")
	for _, path := range s.files() {
		day := dayOfFile(path)
		// 文件名为空（畸形名）不删：宁可留着让人自己看，也不误删别人的文件。
		if day == "" || day >= cutoff {
			continue
		}
		_ = os.Remove(path)
	}
}

// logBodyKeep 落盘保留的 body= 值字节数（W2-LS-03）。
const logBodyKeep = 200

// logBearerRe 匹配 "Bearer <凭据>"，用于把密钥换成 ***。
var logBearerRe = regexp.MustCompile(`(Bearer\s+)[A-Za-z0-9._\-]{8,}`)

// sanitizeLogText 落盘前的最小脱敏（W2-LS-03）：
//
//   - "Bearer <token>" → "Bearer ***"：日志里偶尔带 Authorization 原文，
//     导出包外发即等于交出密钥；
//   - "body=" 之后的内容截到 200 字节：上游错误体常回带命中的用户原文/长 base64，
//     完整落盘就把日志目录变成对话内容的持久副本。
//
// 只做这两条：其余字段（uid/模型/状态码）是排障必需，不能一刀切。
func sanitizeLogText(s string) string {
	if s == "" {
		return s
	}
	s = logBearerRe.ReplaceAllString(s, "${1}***")
	if i := strings.Index(s, "body="); i >= 0 {
		head := s[:i+len("body=")]
		tail := s[i+len("body="):]
		if len(tail) > logBodyKeep {
			s = head + truncateRunes(tail, logBodyKeep) + "…(truncated)"
		}
	}
	return s
}

// truncateRunes 按字节上限截断并回退到 UTF-8 rune 边界（不切出半个汉字）。
func truncateRunes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
