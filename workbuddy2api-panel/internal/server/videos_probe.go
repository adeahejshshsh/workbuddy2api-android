// videos_probe.go 视频任务的有界探测：台账里没有的 task_id 怎么查。
//
// 通用机制（负缓存、游标、全局限速、暂停、绝不罚号）见 probe.go；本文件只负责
// 「用哪个上游调用去试」和「命中后怎么收养」。
package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const (
	videoProbeMax    = 5                // 单次探测最多问几个账号
	videoProbeBudget = 30               // 每分钟全局最多几次探测请求
	videoProbeRetry  = 30 * time.Second // 同一 id 查不到之后的静默期
	videoProbeKeep   = 10 * time.Minute // 负缓存条目（含游标）的保留时长
	videoProbePause  = 30 * time.Second // 探测撞上限流 / 故障后的全局暂停
	videoProbeMissed = 1024             // 负缓存容量上限（防乱填 id 撑内存）
)

// videoProbeConfig 视频探测器的参数（与 3D 各用一套，互不抢占额度）。
var videoProbeConfig = probeConfig{
	max: videoProbeMax, budget: videoProbeBudget, retry: videoProbeRetry,
	keep: videoProbeKeep, pause: videoProbePause, missed: videoProbeMissed,
	label: "videos",
}

// errVideoProbeSkipped / errVideoNotFound 是历史命名，保留给既有调用方与测试。
var (
	errVideoProbeSkipped = errProbeSkipped
	errVideoNotFound     = errProbeNotFound
)

// probeVideoTask 有界探测：成功则收养并返回状态；否则返回 errVideoProbeSkipped / errVideoNotFound
// 或终止探测的那个上游错误。
func (h *Handler) probeVideoTask(taskID string) (upstream.VideoTaskState, string, string, error) {
	var state upstream.VideoTaskState
	a, err := h.probeAccounts(&h.videoProbe, taskID, func(a *auth.Auth) error {
		data, err := h.cfg.Upstream.VideoTask(a, taskID)
		if err != nil {
			if probeNotMine(err) {
				return errProbeNotMine
			}
			return err
		}
		state = upstream.ParseVideoTask(data)
		if state.ID == "" {
			state.ID = taskID
		}
		return nil
	})
	if err != nil {
		return upstream.VideoTaskState{}, "", "", err
	}
	// 已终态的多半在重启前被查过、记过账：收养时标已记账，免得重复计入；还在跑的出片时照常记。
	h.videoLedger.adopt(taskID, a.UID, a.Realm(), "", state.Done())
	probeAdoptLog(videoProbeConfig.label, taskID, a)
	return state, a.UID, a.Nickname, nil
}

// pollUnknownVideoTask 台账里没有的 task_id：有界探测（并发同一 id 只探一次），找到就按已知任务回，
// 找不到回 404。st 由调用方 defer st.done()。
func (h *Handler) pollUnknownVideoTask(w http.ResponseWriter, st *chatStat, taskID string) {
	state, uid, nick, err := h.videoLedger.pollShared(taskID, func() (upstream.VideoTaskState, string, string, error) {
		return h.probeVideoTask(taskID)
	})
	if err != nil {
		var ue *upstream.Error
		if errors.As(err, &ue) && ue.Status == http.StatusTooManyRequests {
			// 探测撞上限流：如实回 429（客户端可退避），但不罚号——盲查的失败不代表账号有问题。
			st.status, st.outcome = ue.Status, "http_error"
			writeOpenAIErrorHint(w, http.StatusTooManyRequests, "upstream_error", ue.Msg,
				"上游对同一账号的密集查询限流：隔几秒再查；提交时用 wait=false 拿 task_id 更省心")
			return
		}
		st.status, st.outcome = http.StatusNotFound, "http_error"
		hint := videoNotFoundHint
		if errors.Is(err, errVideoProbeSkipped) {
			hint = "这个 task_id 近期刚探测过或探测额度已用尽，网关没有再问上游；过一会儿再试"
		}
		writeVideoNotFound(w, taskID, hint)
		return
	}
	// 刚收养：域按命中的账号来。已终态的收养时已标已记账，所以这里不记账；还在跑的出片时由台账命中路径记。
	owner, _ := h.videoLedger.get(taskID)
	st.uid, st.nick, st.model = uid, nick, owner.Model
	st.status, st.outcome = http.StatusOK, "success"
	h.videoLedger.noteState(taskID, state)
	writeJSON(w, http.StatusOK, videoTaskPayload(taskID, state, requestedModel(owner.Model, owner.Realm), h.videoLedger))
}
