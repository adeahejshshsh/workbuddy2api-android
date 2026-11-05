// videos_probe.go 台账里没有的 task_id 怎么查：有界探测。
//
// 任务只对提交它的账号可见，而上游任务 id 反推不出账号。台账命中时直接问那个账号；台账没有
// （进程重启、超出容量/时长、别处提交、或客户端乱填）时，唯一的办法是拿账号去试。早先的实现
// 对两个域的**每个**可用账号各打一次，而且查不到不留记忆——任何持有有效 key 的客户端换着花样
// 填 id，就能把请求放大 N 倍打满整池，触发上游 429。现在收紧成：
//
//  1. 负缓存：查不到的 id 在 videoProbeRetry 内直接回 404，不碰上游；条目保留 videoProbeKeep，
//     里面带游标，下一次放行时从上次停下的地方接着试，多次重试能轮遍整池而不是总问同几个号；
//  2. 每次最多问 videoProbeMax 个账号；
//  3. 全局限速：每分钟最多 videoProbeBudget 次探测请求，用尽则回 404 + 提示；
//  4. 探测遇到 404 / 403 / 400（不是这个账号的任务）才换下一个，其余错误（429 / 5xx / 网络）
//     立即停手并暂停全局探测 videoProbePause。**探测失败从不调用 applyErrorPolicy**：
//     探测是网关自己发起的盲查，失败说明不了账号的健康。
//
// 找到之后收养进台账（已终态的标已记账、还在跑的保持未记账，见 videoTaskStore.adopt），
// 后续查询直接走台账命中路径。
package server

import (
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
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

// errVideoProbeSkipped 探测被负缓存 / 限速 / 暂停挡掉，没有碰上游。
var errVideoProbeSkipped = errors.New("probe skipped")

// errVideoNotFound 探测过的账号都说不是自己的任务。
var errVideoNotFound = errors.New("task not found")

type videoMiss struct {
	retryAt, expire time.Time
	cursor          int
}

type videoProber struct {
	mu          sync.Mutex
	miss        map[string]*videoMiss
	winStart    time.Time
	winCount    int
	pausedUntil time.Time
}

// begin 判断能否探测 id，返回游标。被负缓存或暂停挡住返回 false。
func (p *videoProber) begin(id string, now time.Time) (cursor int, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if now.Before(p.pausedUntil) {
		return 0, false
	}
	if m := p.miss[id]; m != nil {
		if now.Before(m.retryAt) {
			return 0, false
		}
		return m.cursor, true
	}
	return 0, true
}

// reserve 从全局每分钟额度里取最多 n 次，返回实际取到的数量。
func (p *videoProber) reserve(n int, now time.Time) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if now.Sub(p.winStart) >= time.Minute {
		p.winStart, p.winCount = now, 0
	}
	if left := videoProbeBudget - p.winCount; n > left {
		n = left
	}
	if n < 0 {
		n = 0
	}
	p.winCount += n
	return n
}

// missed 记一次没找到：进入静默期，游标前进 advanced 步。
func (p *videoProber) missed(id string, advanced int, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.miss == nil {
		p.miss = map[string]*videoMiss{}
	}
	m := p.miss[id]
	if m == nil {
		if len(p.miss) >= videoProbeMissed {
			for k, v := range p.miss { // 先清过期的；仍满就随手丢一条，容量是硬上限
				if now.After(v.expire) {
					delete(p.miss, k)
				}
			}
			for k := range p.miss {
				if len(p.miss) < videoProbeMissed {
					break
				}
				delete(p.miss, k)
			}
		}
		m = &videoMiss{}
		p.miss[id] = m
	}
	m.cursor += advanced
	m.retryAt, m.expire = now.Add(videoProbeRetry), now.Add(videoProbeKeep)
}

func (p *videoProber) found(id string) {
	p.mu.Lock()
	delete(p.miss, id)
	p.mu.Unlock()
}

func (p *videoProber) pause(now time.Time) {
	p.mu.Lock()
	p.pausedUntil = now.Add(videoProbePause)
	p.mu.Unlock()
}

// probeCandidates 探测候选账号：未禁用的全部账号，按 UID 排序。
func (h *Handler) probeCandidates() []string {
	var uids []string
	for _, st := range h.cfg.Pool.List() { // List 已按 UID 排序
		if !st.Disabled {
			uids = append(uids, st.UID)
		}
	}
	return uids
}

// probeVideoTask 有界探测：成功则收养并返回状态；否则返回 errVideoProbeSkipped / errVideoNotFound
// 或终止探测的那个上游错误。
func (h *Handler) probeVideoTask(taskID string) (upstream.VideoTaskState, string, string, error) {
	now := time.Now()
	cursor, ok := h.videoProbe.begin(taskID, now)
	if !ok {
		return upstream.VideoTaskState{}, "", "", errVideoProbeSkipped
	}
	// 候选 = 两域所有未禁用的账号（按 UID 排序，游标才有稳定含义）；域未知，所以不分域。
	// 不用 AvailableUIDsForRealm：它会剔除冷却中 / 在途占满的账号，名单随时抖动，游标就失去意义，
	// 而提交任务的账号恰好忙着（刚提交过）的可能性并不低。
	uids := h.probeCandidates()
	if len(uids) == 0 {
		return upstream.VideoTaskState{}, "", "", errVideoNotFound
	}
	n := h.videoProbe.reserve(min(videoProbeMax, len(uids)), now)
	if n == 0 {
		return upstream.VideoTaskState{}, "", "", errVideoProbeSkipped
	}
	tried := 0
	for i := 0; i < n; i++ {
		tried++
		a := h.cfg.Pool.AuthByUID(uids[(cursor+i)%len(uids)])
		if a == nil {
			continue
		}
		data, err := h.cfg.Upstream.VideoTask(a, taskID)
		if err != nil {
			var ue *upstream.Error
			if errors.As(err, &ue) && (ue.Status == http.StatusNotFound ||
				ue.Status == http.StatusForbidden || ue.Status == http.StatusBadRequest) {
				continue // 不是这个账号的任务，换下一个
			}
			// 限流 / 5xx / 网络：盲查失败说明不了账号健康，不罚号；暂停全局探测，别在故障期火上浇油。
			h.videoProbe.pause(time.Now())
			h.videoProbe.missed(taskID, tried, time.Now())
			log.Printf("videos task=%q: 探测被上游拒绝，暂停探测 %s：%v", taskID, videoProbePause, err)
			return upstream.VideoTaskState{}, "", "", err
		}
		state := upstream.ParseVideoTask(data)
		if state.ID == "" {
			state.ID = taskID
		}
		// 已终态的多半在重启前被查过、记过账：收养时标已记账，免得重复计入；还在跑的出片时照常记。
		h.videoLedger.adopt(taskID, a.UID, a.Realm(), "", state.Done())
		h.videoProbe.found(taskID)
		log.Printf("videos task=%q: 台账里没有，探测命中账号 %s（已收养）", taskID, logfmt.Label(a.UID, a.Nickname))
		return state, a.UID, a.Nickname, nil
	}
	h.videoProbe.missed(taskID, tried, time.Now())
	return upstream.VideoTaskState{}, "", "", errVideoNotFound
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
