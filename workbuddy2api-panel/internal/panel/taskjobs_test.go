package panel

// 账号级后台任务作业记录（taskjobs.go）。
//
// 这一层存在的理由只有两条，测试也就钉这两条：
//   1. 重启后历史还在，且「上次没跑完」必须如实显示 interrupted —— 不能装作还在跑；
//   2. 面板看到的作业终态与队列条目一致（同一处 mark 写两份，不许漂移）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func jobTestPanel(t *testing.T) (*Panel, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "task-jobs.json")
	p := New(Config{})
	if err := p.StartTaskJobs(path); err != nil {
		t.Fatalf("StartTaskJobs: %v", err)
	}
	t.Cleanup(p.StopTaskJobs)
	return p, path
}

func TestJobLifecycleStatuses(t *testing.T) {
	p, _ := jobTestPanel(t)

	id := p.jobBegin("u1", "growth", 3)
	p.jobItem("u1", id, "chat_5", "done", "5/5")
	p.jobItem("u1", id, "RichMeow_Chat", "error", "上游 500")
	p.jobEnd("u1", id, "")

	j, ok := p.taskJobs.get("u1")
	if !ok {
		t.Fatal("作业应存在")
	}
	if j.Status != "partial" {
		t.Fatalf("部分失败应是 partial，got %q", j.Status)
	}
	if j.Completed != 1 || j.Failed != 1 || j.Total != 3 {
		t.Fatalf("计数=done:%d failed:%d total:%d", j.Completed, j.Failed, j.Total)
	}
	if len(j.Results) != 2 || j.Results[1].Code != "RichMeow_Chat" {
		t.Fatalf("逐项结果没记全：%+v", j.Results)
	}
	if j.FinishedAt.IsZero() {
		t.Fatal("结束时间应写入")
	}
}

func TestJobEndStatusMatrix(t *testing.T) {
	cases := []struct {
		name string
		run  func(p *Panel, id string)
		want string
	}{
		{"全成功", func(p *Panel, id string) { p.jobItem("u", id, "a", "done", "") }, "done"},
		{"全失败", func(p *Panel, id string) { p.jobItem("u", id, "a", "error", "x") }, "error"},
		{"无可执行", func(p *Panel, id string) {}, "skipped"},
		{"显式错误", func(p *Panel, id string) {}, "error"},
	}
	for _, c := range cases {
		p, _ := jobTestPanel(t)
		id := p.jobBegin("u", "growth", 1)
		c.run(p, id)
		errMsg := ""
		if c.name == "显式错误" {
			errMsg = "内部错误"
		}
		p.jobEnd("u", id, errMsg)
		j, _ := p.taskJobs.get("u")
		if j.Status != c.want {
			t.Fatalf("%s: status=%q want %q", c.name, j.Status, c.want)
		}
	}
}

func TestJobEndIsIdempotentAndStaleIgnored(t *testing.T) {
	p, _ := jobTestPanel(t)
	id := p.jobBegin("u", "growth", 1)
	p.jobItem("u", id, "a", "done", "")
	p.jobEnd("u", id, "")
	// 迟到的旧作业回调不能覆盖新作业的终态。
	p.jobItem("u", "job_stale", "b", "error", "")
	p.jobEnd("u", "job_stale", "旧作业错误")
	j, _ := p.taskJobs.get("u")
	if j.Status != "done" || j.Failed != 0 {
		t.Fatalf("旧作业回调污染了新作业：%+v", j)
	}
}

func TestJobsPersistAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-jobs.json")

	p := New(Config{})
	if err := p.StartTaskJobs(path); err != nil {
		t.Fatal(err)
	}
	id := p.jobBegin("u1", "growth", 2)
	p.jobItem("u1", id, "chat_5", "done", "ok")
	p.jobEnd("u1", id, "")
	p.StopTaskJobs()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("落盘文件不存在: %v", err)
	}

	p2 := New(Config{})
	if err := p2.StartTaskJobs(path); err != nil {
		t.Fatal(err)
	}
	defer p2.StopTaskJobs()
	j, ok := p2.taskJobs.get("u1")
	if !ok || j.Status != "done" || len(j.Results) != 1 {
		t.Fatalf("重启后未恢复：%+v ok=%v", j, ok)
	}
}

// TestRunningJobMarkedInterruptedOnRestart：上次进程退出时还在跑 → 如实标 interrupted，
// 不能让它继续显示 running（运维会一直在等一个已经不存在的作业）。
func TestRunningJobMarkedInterruptedOnRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task-jobs.json")
	raw := `{"version":1,"jobs":[{"job_id":"job_x","uid":"u1","kind":"growth","status":"running",
		"started_at":"2026-11-08T10:00:00Z","updated_at":"2026-11-08T10:00:00Z","total":3,"completed":1,
		"current_task":"chat_5","results":[{"code":"a","status":"done","at":"2026-11-08T10:00:00Z"}]}]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(Config{})
	if err := p.StartTaskJobs(path); err != nil {
		t.Fatal(err)
	}
	defer p.StopTaskJobs()
	j, _ := p.taskJobs.get("u1")
	if j.Status != "interrupted" {
		t.Fatalf("status=%q want interrupted", j.Status)
	}
	if j.CurrentTask != "" {
		t.Fatalf("中断作业不该还挂着当前任务：%q", j.CurrentTask)
	}
	if len(j.Results) != 1 {
		t.Fatalf("已完成的逐项结果应保留：%+v", j.Results)
	}
	if j.FinishedAt.IsZero() {
		t.Fatal("中断时刻应写入 finished_at")
	}
}

func TestJobsCorruptFileStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task-jobs.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(Config{})
	if err := p.StartTaskJobs(path); err != nil {
		t.Fatalf("损坏文件不该让启动失败: %v", err)
	}
	defer p.StopTaskJobs()
	if got := len(p.taskJobs.snapshot()); got != 0 {
		t.Fatalf("应从空表开始，got %d", got)
	}
}

func TestTaskJobsEndpoints(t *testing.T) {
	p, _ := jobTestPanel(t)
	id := p.jobBegin("u1", "growth", 1)
	p.jobItem("u1", id, "chat_5", "done", "")
	p.jobEnd("u1", id, "")

	rec := httptest.NewRecorder()
	p.taskJobsList(rec, httptest.NewRequest(http.MethodGet, "/panel/api/tasks/jobs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list status=%d", rec.Code)
	}
	var list struct {
		Jobs []TaskJob `json:"jobs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Jobs) != 1 {
		t.Fatalf("list body=%s err=%v", rec.Body, err)
	}

	rec2 := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/panel/api/accounts/u1/tasks/job", nil)
	req.SetPathValue("uid", "u1")
	p.accountTaskJob(rec2, req)
	var one struct {
		Job *TaskJob `json:"job"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &one); err != nil || one.Job == nil || one.Job.JobID != id {
		t.Fatalf("account job body=%s err=%v", rec2.Body, err)
	}

	// 未知账号 → job:null（不是 404：前端直接用 null 判「没跑过」）。
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodGet, "/panel/api/accounts/nope/tasks/job", nil)
	req3.SetPathValue("uid", "nope")
	p.accountTaskJob(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Fatalf("未知账号 status=%d", rec3.Code)
	}
}

// TestJobSkipRecordsReason：锁被占而跳过的账号也要留痕，否则「今天什么都没做」与
// 「根本没排到它」在面板上分不清。
func TestJobSkipRecordsReason(t *testing.T) {
	p, _ := jobTestPanel(t)
	p.jobSkip("u1", "该账号有其它任务动作在执行，跳过")
	j, ok := p.taskJobs.get("u1")
	if !ok || j.Status != "skipped" || j.Message == "" {
		t.Fatalf("跳过记录不完整：%+v", j)
	}
	if j.FinishedAt.Sub(j.StartedAt) < 0 {
		t.Fatal("时间倒挂")
	}
}
