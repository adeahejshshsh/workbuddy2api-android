// taskjobs.go 账号级后台任务作业记录（上游 `taskjobs.go` 的增量版本）。
//
// 上游把「账号后台任务」做成了一套独立作业服务（per-account 作业、结果数组、
// 跨重启恢复、自己的 goroutine 与并发信号量），执行体也搬进去了。本仓已有
// taskcenter.go 的队列执行体（账号内串行、账号间并发、per-account 锁、跳过名单、
// 回读判定），把执行体整体替换的收益不抵风险 —— 所以这里只补**作业记录**这一层：
//
//   - 每轮队列执行给每个账号开一个作业，记录当前项、完成/失败/跳过数与逐项结果；
//   - 落盘到 <state 同目录>/task-jobs.json，重启后仍在（这是「重启丢历史」的解法）；
//   - 启动时把上次停在 pending/running 的作业标成 interrupted —— **这是最重要的
//     一条**：不标就等于告诉运维「它还在跑」，而进程早就换了；
//   - 暴露 GET /panel/api/tasks/jobs（最近一轮全账号）与
//     GET /panel/api/accounts/{uid}/tasks/job（单账号最新作业），与上游同名同路径。
//
// 与执行体的唯一耦合是三处 hook（begin/item/end），执行逻辑一行没动。
package panel

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const (
	// maxTaskJobs 保留的作业数（按最后更新倒序截断）：一天两轮、每轮 40 账号，
	// 200 条约覆盖一周，足够回答「昨天那个号跑到哪一步」。
	maxTaskJobs = 200
	// maxTaskJobResults 单作业保留的逐项结果上限：单账号任务码通常 < 60。
	maxTaskJobResults = 64
	// taskJobFlushEvery 落盘节流：队列每完成一项都会标脏，逐项写盘没必要
	// （而且任务本身是分钟级），2 秒一次足够，停机时强制 flush。
	taskJobFlushEvery = 2 * time.Second
)

// TaskJobResult 单条任务结果。
type TaskJobResult struct {
	Code    string    `json:"code"`
	Status  string    `json:"status"` // done | error | skipped
	Message string    `json:"message,omitempty"`
	At      time.Time `json:"at"`
}

// TaskJob 一个账号的一轮后台任务作业。
type TaskJob struct {
	JobID   string `json:"job_id"`
	UID     string `json:"uid"`
	Kind    string `json:"kind"`   // growth
	Status  string `json:"status"` // running | done | partial | error | interrupted | skipped
	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`

	StartedAt  time.Time `json:"started_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`

	CurrentTask string          `json:"current_task,omitempty"`
	Total       int             `json:"total"`
	Completed   int             `json:"completed"`
	Failed      int             `json:"failed"`
	Skipped     int             `json:"skipped"`
	Results     []TaskJobResult `json:"results"`
}

// taskJobFile 落盘格式（带版本号，便于以后加字段时兼容）。
type taskJobFile struct {
	Version int       `json:"version"`
	Jobs    []TaskJob `json:"jobs"`
}

// jobStore 作业记录存储。map 键是 uid（每账号只保留最新一条，与上游同口径：
// 「这个号现在/最近在跑什么」比「历史上跑过几百轮」有用，后者在请求记录里已有）。
type jobStore struct {
	mu    sync.Mutex
	path  string
	jobs  map[string]*TaskJob
	dirty bool
	stop  chan struct{}
	once  sync.Once
}

// StartTaskJobs 载入作业记录并启动落盘节流；path 为空 = 只在内存里记（测试/嵌入用法）。
func (p *Panel) StartTaskJobs(path string) error {
	p.jobMu.Lock()
	defer p.jobMu.Unlock()
	if p.taskJobs != nil {
		return nil // 幂等：重复调用不重置已有作业
	}
	s := &jobStore{path: path, jobs: map[string]*TaskJob{}, stop: make(chan struct{})}
	if path != "" {
		if err := s.load(); err != nil {
			return err
		}
	}
	// 上次进程退出时还在跑的作业：如实标 interrupted，别让运维以为它还在跑。
	now := time.Now()
	changed := 0
	for _, j := range s.jobs {
		if j.Status == "running" {
			j.Status = "interrupted"
			j.Message = "进程上次退出时该作业未完成（重启后不会再继续）"
			j.CurrentTask = ""
			j.UpdatedAt, j.FinishedAt = now, now
			changed++
		}
	}
	if changed > 0 {
		s.dirty = true
		if err := s.flushLocked(); err != nil {
			log.Printf("WARN: [panel] task jobs 恢复标记落盘失败: %v", err)
		}
	}
	p.taskJobs = s
	go s.loop()
	return nil
}

// StopTaskJobs 停止节流循环并强制落盘（进程退出前调用）。
func (p *Panel) StopTaskJobs() {
	p.jobMu.Lock()
	s := p.taskJobs
	p.jobMu.Unlock()
	if s == nil {
		return
	}
	s.once.Do(func() {
		close(s.stop)
		s.mu.Lock()
		err := s.flushLocked()
		s.mu.Unlock()
		if err != nil {
			log.Printf("WARN: [panel] task jobs 停机落盘失败: %v", err)
		}
	})
}

func (s *jobStore) loop() {
	t := time.NewTicker(taskJobFlushEvery)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.mu.Lock()
			if s.dirty {
				if err := s.flushLocked(); err != nil {
					log.Printf("WARN: [panel] task jobs 落盘失败（本次仅内存生效）: %v", err)
				}
			}
			s.mu.Unlock()
		}
	}
}

func (s *jobStore) load() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 首次运行：空表，不是错误
		}
		return err
	}
	var f taskJobFile
	if err := json.Unmarshal(raw, &f); err != nil {
		// 损坏的作业文件不该让面板起不来（与 task_skip 同口径）：
		// 记一条 WARN 后从空表开始，下一个周期就会覆盖成合法内容。
		log.Printf("WARN: [panel] task jobs 文件损坏，从空表开始: %v", err)
		return nil
	}
	for i := range f.Jobs {
		j := f.Jobs[i]
		if j.UID == "" {
			continue
		}
		s.jobs[j.UID] = &j
	}
	return nil
}

func (s *jobStore) flushLocked() error {
	if s.path == "" {
		s.dirty = false
		return nil
	}
	list := make([]TaskJob, 0, len(s.jobs))
	for _, j := range s.jobs {
		list = append(list, *j)
	}
	sort.Slice(list, func(a, b int) bool { return list[a].UpdatedAt.After(list[b].UpdatedAt) })
	if len(list) > maxTaskJobs {
		list = list[:maxTaskJobs]
	}
	raw, err := json.MarshalIndent(taskJobFile{Version: 1, Jobs: list}, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	if err := auth.ReplaceAtomic(s.path, raw, 0o600); err != nil {
		return err
	}
	s.dirty = false
	return nil
}

// newJobID 作业 ID：时间前缀 + 随机后缀，日志里可直接按前缀定位。
func newJobID(now time.Time) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "job_" + now.Format("20060102T150405") + "_" + hex.EncodeToString(b[:])
}

// jobBegin 为一个账号开新作业（kind 目前只有 growth），返回作业 ID。
func (p *Panel) jobBegin(uid, kind string, total int) string {
	p.jobMu.Lock()
	s := p.taskJobs
	p.jobMu.Unlock()
	if s == nil {
		return ""
	}
	now := time.Now()
	j := &TaskJob{
		JobID: newJobID(now), UID: uid, Kind: kind, Status: "running",
		StartedAt: now, UpdatedAt: now, Total: total,
	}
	s.mu.Lock()
	s.jobs[uid] = j
	s.dirty = true
	s.mu.Unlock()
	return j.JobID
}

// jobItem 记录一项的结局（按作业 ID 匹配，旧作业的迟到回调直接丢弃）。
func (p *Panel) jobItem(uid, jobID, code, status, msg string) {
	p.jobMu.Lock()
	s := p.taskJobs
	p.jobMu.Unlock()
	if s == nil || jobID == "" {
		return
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.jobs[uid]
	if j == nil || j.JobID != jobID {
		return
	}
	if len(j.Results) < maxTaskJobResults {
		j.Results = append(j.Results, TaskJobResult{Code: code, Status: status, Message: msg, At: now})
	}
	switch status {
	case "done":
		j.Completed++
	case "error":
		j.Failed++
	case "skipped":
		j.Skipped++
	}
	j.CurrentTask = code
	j.UpdatedAt = now
	s.dirty = true
}

// jobEnd 结束作业：按逐项结果归并终态（全成 = done；有失败 = partial；全失败 = error）。
func (p *Panel) jobEnd(uid, jobID, errMsg string) {
	p.jobMu.Lock()
	s := p.taskJobs
	p.jobMu.Unlock()
	if s == nil || jobID == "" {
		return
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.jobs[uid]
	if j == nil || j.JobID != jobID {
		return
	}
	if j.Status != "running" {
		return // 已终结（panic 路径会显式先调一次，defer 里的这次是空操作）
	}
	j.CurrentTask = ""
	j.UpdatedAt, j.FinishedAt = now, now
	switch {
	case errMsg != "":
		j.Status = "error"
		j.Error = errMsg
	case j.Completed == 0 && j.Failed == 0:
		j.Status = "skipped"
		if j.Message == "" {
			j.Message = "本账号无可执行待办"
		}
	case j.Failed > 0 && j.Completed > 0:
		j.Status = "partial"
	case j.Failed > 0:
		j.Status = "error"
		j.Error = "全部待办失败"
	default:
		j.Status = "done"
	}
	s.dirty = true
}

// jobSkip 账号因锁被占等原因没跑：也留一条记录（否则面板上「这个号今天什么都没做」
// 与「根本没排到它」分不清）。
func (p *Panel) jobSkip(uid, reason string) {
	p.jobMu.Lock()
	s := p.taskJobs
	p.jobMu.Unlock()
	if s == nil {
		return
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[uid] = &TaskJob{
		JobID: newJobID(now), UID: uid, Kind: "growth", Status: "skipped",
		StartedAt: now, UpdatedAt: now, FinishedAt: now, Message: reason,
	}
	s.dirty = true
}

// snapshot 返回按更新时间倒序的作业副本。
func (s *jobStore) snapshot() []TaskJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TaskJob, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, *j)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].UpdatedAt.After(out[b].UpdatedAt) })
	return out
}

func (s *jobStore) get(uid string) (TaskJob, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[uid]
	if !ok {
		return TaskJob{}, false
	}
	return *j, true
}

// taskJobsList GET /panel/api/tasks/jobs：最近一轮全账号作业（按更新时间倒序）。
func (p *Panel) taskJobsList(w http.ResponseWriter, r *http.Request) {
	p.jobMu.Lock()
	s := p.taskJobs
	p.jobMu.Unlock()
	if s == nil {
		writeErr(w, http.StatusNotImplemented, "task jobs api not available")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "jobs": s.snapshot()})
}

// accountTaskJob GET /panel/api/accounts/{uid}/tasks/job：单账号最新作业。
func (p *Panel) accountTaskJob(w http.ResponseWriter, r *http.Request) {
	p.jobMu.Lock()
	s := p.taskJobs
	p.jobMu.Unlock()
	if s == nil {
		writeErr(w, http.StatusNotImplemented, "task jobs api not available")
		return
	}
	uid := r.PathValue("uid")
	if j, ok := s.get(uid); ok {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job": j})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job": nil})
}
