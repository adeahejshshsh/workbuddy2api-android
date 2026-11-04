// admin_tasks.go 手动补跑排程。POST /admin/tasks/{name}/run，立刻 202，后台执行。
// 暴露的入口：调度器那六个（checkin / activity / keepalive / travel /
// balance_refresh / streak_bonus）+ 面板的 growth（任务中心成长队列）。
//
// growth 与那六个不是同一个宿主：调度器只有排程时机，执行体在面板
// （panel.RunGrowthQueueOnce，经 Config.GrowthRunner 注入）。所以它单列一个
// 可选字段——没接面板的部署（测试/裸用）不会因此多出一个恒空转的入口。
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

// adminTaskNames 已知任务名（拼「unknown task; known: …」用）。growth 只在接了
// GrowthRunner 时才真能跑，但名字要在这里——否则运维看到「unknown」会以为入口不存在。
var adminTaskNames = []string{
	"checkin", "activity", "keepalive", "travel", "balance_refresh", "streak_bonus", "growth",
}

func (h *Handler) taskRunners() map[string]func() {
	t := h.cfg.Tasks
	if t == nil {
		return nil
	}
	m := map[string]func(){
		"checkin":         t.RunCheckinNow,
		"activity":        t.RunActivityNow,
		"keepalive":       t.RunKeepaliveNow,
		"travel":          t.RunTravelNow,
		"balance_refresh": t.RunBalanceRefreshNow,
		"streak_bonus":    t.RunStreakBonusNow,
	}
	if h.cfg.GrowthRunner != nil {
		m["growth"] = h.cfg.GrowthRunner
	}
	return m
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
