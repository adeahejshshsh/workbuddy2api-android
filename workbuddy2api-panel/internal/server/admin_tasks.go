// admin_tasks.go 手动补跑排程。POST /admin/tasks/{name}/run，立刻 202，后台执行。
// 暴露的是本仓调度器**实际有**的六个入口：checkin / activity / keepalive / travel /
// balance_refresh / streak_bonus。
//
// 上游另有 growth（任务中心）一档，本仓尚未移植面板后台作业系统
// （panel/taskjobs.go），没有对应执行体，故不列出——列一个恒空转的入口比没有更糟。
package server

import (
	"net/http"
	"strings"
)

// TaskRunner 各 Run*Now 里本仓实际有的六个。*scheduler.Scheduler 满足它。
type TaskRunner interface {
	RunCheckinNow()
	RunActivityNow()
	RunKeepaliveNow()
	RunTravelNow()
	RunBalanceRefreshNow()
	RunStreakBonusNow()
}

var adminTaskNames = []string{
	"checkin", "activity", "keepalive", "travel", "balance_refresh", "streak_bonus",
}

func (h *Handler) taskRunners() map[string]func() {
	t := h.cfg.Tasks
	if t == nil {
		return nil
	}
	return map[string]func(){
		"checkin":         t.RunCheckinNow,
		"activity":        t.RunActivityNow,
		"keepalive":       t.RunKeepaliveNow,
		"travel":          t.RunTravelNow,
		"balance_refresh": t.RunBalanceRefreshNow,
		"streak_bonus":    t.RunStreakBonusNow,
	}
}

func (h *Handler) adminTaskRun(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	runners := h.taskRunners()
	if runners == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "unavailable", "task runner not wired")
		return
	}
	run, ok := runners[name]
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "not_found",
			"unknown task: "+name+"; known: "+strings.Join(adminTaskNames, ", "))
		return
	}
	h.taskMu.Lock()
	if h.taskRunning[name] {
		h.taskMu.Unlock()
		writeOpenAIError(w, http.StatusConflict, "busy", "task already running: "+name)
		return
	}
	if h.taskRunning == nil {
		h.taskRunning = map[string]bool{}
	}
	h.taskRunning[name] = true
	h.taskMu.Unlock()

	go func() {
		defer func() {
			h.taskMu.Lock()
			delete(h.taskRunning, name)
			h.taskMu.Unlock()
		}()
		run()
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"task": name, "status": "started"})
}
