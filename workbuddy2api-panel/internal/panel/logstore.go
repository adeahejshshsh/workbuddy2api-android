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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/cst"
)

const (
	// logStoreDefaultDays / logStoreDefaultMaxMB 配置缺省（与 reqlog 的默认量级对齐）。
	logStoreDefaultDays  = 7
	logStoreDefaultMaxMB = 50
	// logStoreFlushBytes 攒到这么多字节才落盘一次。
	// 不逐行 flush：日志是高频写入，每行一次 syscall 会在长流式会话里明显吃掉 CPU。
	logStoreFlushBytes = 48 << 10
	// logStoreBackfillMax 回填上限（条）。环路只有 500/频道，回填更多也留不住。
	logStoreBackfillMax = 3000
)

// LogStore 面板运行日志的按天落盘存储。目录不可用时降级为纯内存，绝不阻塞主流程。
type LogStore struct {
	mu            sync.Mutex
	dir           string
	retentionDays int
	maxBytes      int64
	file          *os.File
	bw            *bufio.Writer
	day           string // 当前文件对应的自然日（CST，与切天口径一致）
	written       int64  // 当前文件已写字节（上限判定用）
	err           string // 最近一次写盘失败原因（面板可提示）
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
	s := &LogStore{dir: dir, retentionDays: retentionDays, maxBytes: int64(maxMB) << 20}
	s.cleanup()
	return s
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

// Append 追加一条日志；跨天自动切文件，超单文件上限时截断重开。
//
// 失败一律只记 s.err 不返回错误：日志系统不能反过来拖垮在处理的请求。
func (s *LogStore) Append(e LogEntry) {
	if s == nil || strings.TrimSpace(e.Text) == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	day := e.TS.In(cst.Loc).Format("2006-01-02")
	needRotate := s.file == nil || day != s.day
	// 单文件超上限：同一天内换个序号文件继续写，而不是覆盖（否则当天日志只剩一半）。
	// 只在「跨天」或「已经超限」时轮转；上限是软约束，避免逐行 stat。
	if !needRotate && s.maxBytes > 0 && s.written >= s.maxBytes {
		needRotate = true
	}
	if needRotate {
		s.rotateLocked(day)
		if s.file == nil {
			return
		}
	}
	line, err := json.Marshal(e)
	if err != nil {
		s.err = err.Error()
		return
	}
	line = append(line, '\n')
	n, err := s.bw.Write(line)
	s.written += int64(n)
	if err != nil {
		s.err = err.Error()
		return
	}
	if s.bw.Buffered() >= logStoreFlushBytes {
		if err := s.bw.Flush(); err != nil {
			s.err = err.Error()
			return
		}
	}
	s.err = ""
}

// rotateLocked 关闭当前文件并打开新的一天（fileName 带序号避让超限轮转）。
// 调用方必须持 s.mu。
func (s *LogStore) rotateLocked(day string) {
	s.closeLocked()
	path := s.pathForDay(day)
	// 同一天里因为超限轮转出来的后续文件：panel-<day>.2.jsonl、.3.jsonl …
	if s.day == day {
		for i := 2; i < 100; i++ {
			candidate := filepath.Join(s.dir, fmt.Sprintf("panel-%s.%d.%s", day, i, logStoreExt))
			if _, statErr := os.Stat(candidate); os.IsNotExist(statErr) {
				path = candidate
				break
			}
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		s.err = err.Error()
		s.day = ""
		return
	}
	// 续写已有文件时把既有大小算进上限，否则重启后同一天的文件会长到两倍。
	if info, statErr := f.Stat(); statErr == nil {
		s.written = info.Size()
	} else {
		s.written = 0
	}
	s.file = f
	s.bw = bufio.NewWriterSize(f, 64<<10)
	s.day = day
}

// closeLocked 刷盘并关闭当前文件。调用方必须持 s.mu。
func (s *LogStore) closeLocked() {
	if s.bw != nil {
		if err := s.bw.Flush(); err != nil && s.err == "" {
			s.err = err.Error()
		}
		s.bw = nil
	}
	if s.file != nil {
		_ = s.file.Close()
		s.file = nil
	}
}

// Flush 立即刷盘（导出前调用，保证文件里有内存中尚未落盘的尾巴）。
func (s *LogStore) Flush() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bw != nil {
		if err := s.bw.Flush(); err != nil {
			s.err = err.Error()
		}
	}
}

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

// files 返回目录内的日志文件，按文件名（即日期）升序。
func (s *LogStore) files() []string {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "panel-") || !strings.HasSuffix(name, "."+logStoreExt) {
			continue
		}
		out = append(out, filepath.Join(s.dir, name))
	}
	sort.Strings(out)
	return out
}

// dayOfFile 从文件名取自然日（panel-2026-11-09.jsonl / panel-2026-11-09.2.jsonl）。
func dayOfFile(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), "."+logStoreExt)
	base = strings.TrimPrefix(base, "panel-")
	if len(base) < 10 {
		return ""
	}
	return base[:10]
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
	// 先把内存里的尾巴刷下去，否则回填会漏掉重启前最后几行。
	if s.bw != nil {
		_ = s.bw.Flush()
	}
	files := s.files()
	s.mu.Unlock()

	var out []LogEntry
	for i := len(files) - 1; i >= 0 && len(out) < limit; i-- {
		chunk := readLogFile(files[i])
		// 从最新文件往前拼，插到前面保持全局升序。
		if need := limit - len(out); len(chunk) > need {
			chunk = chunk[len(chunk)-need:]
		}
		out = append(chunk, out...)
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// readLogFile 读一个 JSONL 文件，跳过坏行。
func readLogFile(path string) []LogEntry {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []LogEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e LogEntry
		if json.Unmarshal([]byte(line), &e) != nil || e.Text == "" {
			continue
		}
		out = append(out, e)
	}
	return out
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
	zw := zip.NewWriter(w)
	written := 0
	for _, path := range s.files() {
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
	sc := bufio.NewScanner(src)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	bw := bufio.NewWriterSize(dst, 64<<10)
	defer bw.Flush()
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var probe struct {
			TS time.Time `json:"ts"`
		}
		// 只解析 ts：整行反序列化再重排是白费（一行最多几 KB，但一天上万行）。
		if json.Unmarshal(line, &probe) != nil {
			continue
		}
		if probe.TS.Before(from) || probe.TS.After(to) {
			continue
		}
		if _, err := bw.Write(line); err != nil {
			return err
		}
		if err := bw.WriteByte('\n'); err != nil {
			return err
		}
	}
	return nil
}

// cleanup 删除保留期之外的文件（启动时跑一次）。
func (s *LogStore) cleanup() {
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
