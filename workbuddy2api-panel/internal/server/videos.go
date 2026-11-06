// videos.go 视频生成网关：把上游 /v2/videos/* 的「异步任务」包装成 OpenAI 风格接口。
//
//	POST /v1/videos/generations   提交（缺省同步等到出片，wait=false 立刻返回 task id）
//	GET  /v1/videos/{task_id}     查任务
//	POST /v1/videos/tasks         查任务（POST 形态，兼容只肯发 POST 的客户端/MCP）
//
// 三件事必须说清楚，否则用户会在别处踩到：
//
//  1. **任务只对提交它的账号可见**（上游按账号隔离）。上游 task id 前缀（如 1312831544）
//     不能反推账号，Auth 结构里也没有数字租户 id —— 所以网关自己记一份 task→账号台账
//     （videoTasks）。进程重启后台账清空，此时查询会退化成有界探测（videos_probe.go）并重新收养。
//  2. **成品是签名直链**（COS，实测 12h 有效），网关只透出 URL，不转存字节——与出图同口径。
//  3. **计费发生在出片完成那一刻**：提交不扣钱，轮询到 completed 才按 usage.credit 记账。
//     同一任务重复查询不会重复记账（台账里的 Charged 标记）。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

const (
	// defaultVideoModel 未点名模型时的缺省视频模型（实测最快、最便宜：4s/57.14 积分/124s）。
	defaultVideoModel = "cn:minimax-video-h3"
	// videoAttempts 提交阶段失败换号上限（与出图同量级）。
	videoAttempts = 3
	// videoWaitDefault 同步等待上限：kling 实测 473s，留一倍余量。
	videoWaitDefault = 15 * time.Minute
	// videoWaitMax 客户端能要求的最大等待（参数 timeout_seconds）。
	videoWaitMax = 30 * time.Minute
	// videoTaskTTL 台账保留时长：要盖住「提交后隔几小时回来取片」的用法。
	videoTaskTTL = 12 * time.Hour
	// videoTaskMax 台账容量上限，超出按最早创建淘汰（进程内存，非持久化）。
	videoTaskMax = 2048
	// videoMaxBody 请求体上限（prompt + 可能的 data URL 首帧，1MB 够，避免被塞视频）。
	videoMaxBody = 4 << 20
)

// videoPollInterval 轮询间隔。minimax 实测 124s、kling 实测 473s 出片，5s 一跳够用。
// 用 var 而不是 const：测试把它压到 1ms，避免同步等待用例真睡 5 秒。
var videoPollInterval = 5 * time.Second

// videoTaskMinPoll 同一任务两次真打上游的最小间隔。实测上游按账号限流：同一账号 2 秒内
// 连查 4 次就回 14003（Too many requests），所以客户端再密集轮询也只在本地等。
const videoTaskMinPoll = 3 * time.Second

// videoRateLimitBackoff 撞上 14003 之后的冷却：这段时间一律回快照，不碰上游（实测限流会
// 持续十几秒，是账号级的，自罚一遍没意义）。
const videoRateLimitBackoff = 30 * time.Second

// videoTaskOwner 任务台账条目：上游任务只对提交它的账号可见，查询必须回到同一账号。
type videoTaskOwner struct {
	UID     string
	Realm   string
	Model   string
	Created time.Time
	// Charged 已记账标记：轮询到 completed 时按 usage.credit 记一次，重复查询不再记。
	// 收养来的条目也用它：收养时任务已到终态就视为已记账（见 adopt）。
	Charged bool
	// 出片用量快照。上游 usage 只在**第一次**查到 completed 时回一次，之后重复查询没有 usage
	// （kling 连 size 都不回）；不记住的话，客户端复查时这笔账会凭空消失。
	Credit   float64
	Tokens   int64
	Duration int64
	Size     string
	HasUsage bool
	// 最近一次看到的状态快照。终态（completed / failed）之后不再打上游——上游 usage 只在
	// 第一次回，密集查询还会 429——直接用这份快照回客户端（直链有效期与台账 TTL 都是 12h）。
	State    upstream.VideoTaskState
	HasState bool
	StateAt  time.Time
	// LastQueryAt 上次真打上游的时刻（最小间隔护栏）。
	LastQueryAt time.Time
	// LimitedUntil 上游限流后的冷却截止时刻。
	LimitedUntil time.Time
}

// videoTaskResult 台账里缓存的出片信息（给重复查询兜底）。
type videoTaskResult struct {
	Credit   float64
	Tokens   int64
	Duration int64
	Size     string
}

// videoTaskStore 进程内的 task → 账号台账（带容量/时长淘汰）。
type videoTaskStore struct {
	mu sync.Mutex
	m  map[string]*videoTaskOwner
	// flights 在途查询：同一个 task_id 并发来查时只让一个真打上游，其余等它的结果。
	flights map[string]*videoFlight
}

func newVideoTaskStore() *videoTaskStore {
	return &videoTaskStore{m: map[string]*videoTaskOwner{}, flights: map[string]*videoFlight{}}
}

func (s *videoTaskStore) put(id, uid, realm, model string) {
	if s == nil || id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[id] = &videoTaskOwner{UID: uid, Realm: realm, Model: model, Created: time.Now()}
	s.gcLocked()
}

func (s *videoTaskStore) get(id string) (videoTaskOwner, bool) {
	if s == nil {
		return videoTaskOwner{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.m[id]
	if !ok {
		return videoTaskOwner{}, false
	}
	return *o, true
}

// adopt 把「台账里没有、但某个账号确实能查到」的任务补进来（进程重启后的自愈路径）。
//
// charged 传「收养时任务是否已到终态」：已完成的任务多半在重启前被查过、记过账，再记一次就是
// 重复计入；还在跑的任务不可能在任何地方记过账，留到出片时照常记一次。
func (s *videoTaskStore) adopt(id, uid, realm, model string, charged bool) {
	if s == nil || id == "" || uid == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[id]; ok {
		return
	}
	s.m[id] = &videoTaskOwner{UID: uid, Realm: realm, Model: model, Created: time.Now(), Charged: charged}
}

// claimCharge 尝试把任务标成「已记账」。返回 true 表示本次调用赢得了记账权
// （并发查询同一任务时只有一个会记账），false 表示已经记过。
func (s *videoTaskStore) claimCharge(id string) bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.m[id]
	if !ok {
		return true // 台账里没有：无标记可用，交由调用方自行按 id 去重语义处理
	}
	if o.Charged {
		return false
	}
	o.Charged = true
	return true
}

// noteResult 记下这次出片的用量/尺寸（幂等，可重复调用）。
func (s *videoTaskStore) noteResult(id string, st upstream.VideoTaskState) {
	if s == nil || id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.m[id]
	if !ok {
		return
	}
	if st.Credit > 0 || st.Tokens > 0 {
		o.Credit, o.Tokens, o.Duration, o.HasUsage = st.Credit, st.Tokens, st.Duration, true
	}
	if st.Size != "" {
		o.Size = st.Size
	}
}

// result 取台账里缓存的出片信息；上游后续查询不再回 usage 时用它补齐回执。
func (s *videoTaskStore) result(id string) (videoTaskResult, bool) {
	if s == nil || id == "" {
		return videoTaskResult{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.m[id]
	if !ok {
		return videoTaskResult{}, false
	}
	if !o.HasUsage && o.Size == "" {
		return videoTaskResult{}, false
	}
	return videoTaskResult{Credit: o.Credit, Tokens: o.Tokens, Duration: o.Duration, Size: o.Size}, true
}

// markQuery 记下这次真打上游的时刻（频率护栏用）。
func (s *videoTaskStore) markQuery(id string) {
	if s == nil || id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, ok := s.m[id]; ok {
		o.LastQueryAt = time.Now()
	}
}

// pollWait 还要等多久才允许再打上游（0 = 现在就能打）：取「最小间隔」与「限流冷却」的较大者。
func (s *videoTaskStore) pollWait(id string) time.Duration {
	if s == nil || id == "" {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.m[id]
	if !ok {
		return 0
	}
	now := time.Now()
	if o.LimitedUntil.After(now) {
		return o.LimitedUntil.Sub(now)
	}
	if !o.LastQueryAt.IsZero() {
		if d := videoTaskMinPoll - now.Sub(o.LastQueryAt); d > 0 {
			return d
		}
	}
	return 0
}

// noteLimited 上游限流后记一个冷却窗口。
func (s *videoTaskStore) noteLimited(id string, d time.Duration) {
	if s == nil || id == "" || d <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, ok := s.m[id]; ok {
		o.LimitedUntil = time.Now().Add(d)
	}
}

// noteState 记下最近一次看到的任务状态。
func (s *videoTaskStore) noteState(id string, st upstream.VideoTaskState) {
	if s == nil || id == "" || st.Status == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.m[id]
	if !ok {
		return
	}
	o.State, o.HasState, o.StateAt = st, true, time.Now()
}

// cachedState 取台账里的状态快照。
func (s *videoTaskStore) cachedState(id string) (upstream.VideoTaskState, time.Time, bool) {
	if s == nil || id == "" {
		return upstream.VideoTaskState{}, time.Time{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.m[id]
	if !ok || !o.HasState {
		return upstream.VideoTaskState{}, time.Time{}, false
	}
	return o.State, o.StateAt, true
}

// videoFlight 一次在途的上游查询（并发查询同一任务时共享它的结果）。
type videoFlight struct {
	done  chan struct{}
	state upstream.VideoTaskState
	uid   string
	nick  string
	err   error
}

// pollShared 合并并发的同一任务查询：上游对同一账号密集查询会回 14003（Too many requests），
// 而 MCP 客户端常常并发轮询同一个 task_id，所以这里做单飞（single-flight）。
func (s *videoTaskStore) pollShared(id string, fn func() (upstream.VideoTaskState, string, string, error)) (upstream.VideoTaskState, string, string, error) {
	if s == nil {
		return fn()
	}
	s.mu.Lock()
	if s.flights == nil {
		s.flights = map[string]*videoFlight{}
	}
	if f, ok := s.flights[id]; ok {
		s.mu.Unlock()
		<-f.done
		return f.state, f.uid, f.nick, f.err
	}
	f := &videoFlight{done: make(chan struct{})}
	s.flights[id] = f
	s.mu.Unlock()

	func() {
		defer func() {
			if r := recover(); r != nil {
				f.err = fmt.Errorf("video poll panic: %v", r)
			}
			s.mu.Lock()
			delete(s.flights, id)
			s.mu.Unlock()
			close(f.done)
		}()
		f.state, f.uid, f.nick, f.err = fn()
	}()
	return f.state, f.uid, f.nick, f.err
}

func (s *videoTaskStore) gcLocked() {
	if len(s.m) <= videoTaskMax {
		// 容量内也可能有超龄的（长时间低负载）：顺手按 TTL 清一遍。
		cut := time.Now().Add(-videoTaskTTL)
		for id, o := range s.m {
			if o.Created.Before(cut) {
				delete(s.m, id)
			}
		}
		return
	}
	// 超容量：按创建时间淘汰最旧的一批（留 3/4 容量，避免每次都全量排序）。
	type pair struct {
		id string
		at time.Time
	}
	all := make([]pair, 0, len(s.m))
	for id, o := range s.m {
		all = append(all, pair{id, o.Created})
	}
	for i := 1; i < len(all); i++ { // 插入排序：容量上限内，n 很小
		for j := i; j > 0 && all[j].at.Before(all[j-1].at); j-- {
			all[j], all[j-1] = all[j-1], all[j]
		}
	}
	drop := len(all) - videoTaskMax*3/4
	for i := 0; i < drop; i++ {
		delete(s.m, all[i].id)
	}
}

// firstAllowedGenerationVideo 白名单兜底（与首个可用出图模型同一条设计）：白名单启用时，
// 缺省视频模型不在白名单就改选白名单内第一个 video 模型。
func (h *Handler) firstAllowedGenerationVideo() (string, string, bool) {
	if !h.allowlistEnabled() || h.cfg.Upstream == nil {
		return "", "", false
	}
	realms := []string{"cn"}
	if h.cfg.GlobalEnabled {
		realms = append(realms, "global")
	}
	for _, realm := range realms {
		for _, mi := range h.cfg.Upstream.GenerationModels(realm) {
			if mi.GenerationKind != "video" {
				continue
			}
			if h.modelHidden(realm, mi.ID) {
				continue
			}
			if !h.modelAllowed(realm, mi.ID) {
				continue
			}
			return realm, mi.ID, true
		}
	}
	return "", "", false
}

// generationModel 在目录留存的生成模型里按 (realm, 裸名) 找一条；找不到返回 false
// （目录还没拉到 / 模型不在目录里——此时放行给上游判，不替上游拒绝）。
func (h *Handler) generationModel(realm, bare string) (upstream.ModelInfo, bool) {
	if h.cfg.Upstream == nil {
		return upstream.ModelInfo{}, false
	}
	for _, mi := range h.cfg.Upstream.GenerationModels(realm) {
		if mi.ID == bare {
			return mi, true
		}
	}
	return upstream.ModelInfo{}, false
}

// videos 提交出视频任务：POST /v1/videos/generations。
func (h *Handler) videos(w http.ResponseWriter, r *http.Request) {
	// 积分闸：出视频单次几十 credit，比出图贵一个量级，同样必须 admit（理由见
	// images.go 同一处注释：放在 handler 里而不是 ServeHTTP，避免未鉴权请求读到水位）。
	if !h.admitBudget(w, r) {
		return
	}
	start := time.Now()
	raw, err := io.ReadAll(io.LimitReader(r.Body, videoMaxBody))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	req, err := videoRequest(raw)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if h.loadLive().RecordContext {
		if tr := requestTraceFrom(r); tr != nil {
			tr.context = archiveContext(raw)
		}
	}
	prompt, _ := req["prompt"].(string)
	if strings.TrimSpace(prompt) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			`prompt is required（例：{"model":"`+defaultVideoModel+`","prompt":"一只猫在草地上奔跑","seconds":4}）`)
		return
	}
	model, _ := req["model"].(string)
	named := strings.TrimSpace(model) != ""
	if !named {
		model = defaultVideoModel
	}
	realm, bare := resolveModel(model)
	if !h.modelAllowed(realm, bare) {
		if !named {
			if fr, fb, ok := h.firstAllowedGenerationVideo(); ok {
				realm, bare = fr, fb
			} else {
				writeOpenAIError(w, http.StatusBadRequest, "model_unavailable",
					"model is not in the gateway allowlist, try another model")
				return
			}
		} else {
			writeOpenAIError(w, http.StatusBadRequest, "model_unavailable",
				"model is not in the gateway allowlist, try another model")
			return
		}
	}
	if h.modelHidden(realm, bare) {
		writeOpenAIError(w, http.StatusBadRequest, "model_unavailable",
			"model is hidden by gateway configuration, try another model")
		return
	}
	// 目录已知的模型做一次种类校验：拿生图模型来出视频，上游会回 14407「route config
	// not found」，那句话对用户毫无信息量——这里直接点明。目录里没有的模型放行给上游。
	if mi, ok := h.generationModel(realm, bare); ok && mi.GenerationKind != "video" {
		writeOpenAIError(w, http.StatusBadRequest, "model_unavailable",
			fmt.Sprintf("%s 不是视频模型（kind=%s）；视频模型：cn:minimax-video-h3 / cn:kling-video-v3",
				bare, mi.GenerationKind))
		return
	}

	syncWait, waitSet := h.videoWait(req)
	body, err := videoUpstreamBody(req, bare)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	out, err := json.Marshal(body)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	// 记账与出图同一条管道：请求台账由 ServeHTTP 的 isChatEntry 接管，这里补
	// 账号/模型/用量/扣费（扣费在出片完成时补记，提交本身不扣钱）。
	st := &chatStat{start: start, model: bare, mode: "video", toks: -1, budget: h.budget}
	if tr := requestTraceFrom(r); tr != nil {
		tr.stat = st
		st.requestID = tr.id
		st.clientIP, st.userAgent = tr.clientIP, tr.userAgent
	}
	defer st.done()

	modelRate := ""
	if h.cfg.Upstream != nil {
		modelRate = h.cfg.Upstream.ModelRate(realm, bare)
	}
	tried := map[string]bool{}
	var lastErr error
	for attempt := 0; attempt < videoAttempts; attempt++ {
		a := h.cfg.Pool.PickExcludingForRealm(tried, bare, realm)
		if a == nil {
			break
		}
		tried[a.UID] = true
		st.uid, st.nick = a.UID, a.Nickname
		st.attempts = attempt + 1

		data, err := h.cfg.Upstream.VideoGenerations(a, json.RawMessage(out))
		if err != nil {
			lastErr = err
			var ue *upstream.Error
			if errors.As(err, &ue) {
				h.applyErrorPolicy(a.UID, ue.Kind, ue.Msg, bare, ue)
				if requestLevelFailFastKind(ue.Kind) {
					status := ue.Status
					if status < 400 {
						status = http.StatusBadRequest
					}
					st.status, st.outcome = status, "http_error"
					log.Printf("videos model=%s realm=%s: 请求级错误 kind=%v，不再换号", bare, realm, ue.Kind)
					writeOpenAIError(w, status, "upstream_error", ue.Msg)
					return
				}
			}
			log.Printf("videos %s model=%s attempt=%d: %v", logfmt.Label(a.UID, a.Nickname), bare, attempt+1, err)
			continue
		}

		taskID, taskStatus := upstream.ParseVideoSubmit(data)
		if taskID == "" {
			// 上游 200 但没给任务 id：这不是「成功」，别让客户端拿着空 id 去轮询。
			lastErr = fmt.Errorf("上游提交回执缺少任务 id：%s", clipText(string(data), 200))
			log.Printf("videos %s model=%s: %v", logfmt.Label(a.UID, a.Nickname), bare, lastErr)
			continue
		}
		st.status, st.outcome = http.StatusOK, "success"
		h.videoLedger.put(taskID, a.UID, realm, bare)
		seed := videoTaskStateFromSubmit(taskStatus)
		if seed.Status == "" {
			seed.Status = "queued"
		}
		h.videoLedger.noteState(taskID, seed)
		log.Printf("videos %s model=%s: 任务已提交 task=%s status=%s 用时=%s",
			logfmt.Label(a.UID, a.Nickname), bare, taskID, taskStatus, time.Since(start).Round(time.Millisecond))

		if !waitSet || syncWait <= 0 {
			writeJSON(w, http.StatusOK, videoTaskPayload(taskID, videoTaskStateFromSubmit(taskStatus), model, h.videoLedger))
			return
		}
		final, perr := h.awaitVideo(r.Context(), a, taskID, syncWait)
		if r.Context().Err() != nil {
			// 客户端已断开：任务留在台账里，之后可凭 task_id 查询；连接没人读了，不必再回包。
			st.status, st.outcome = 499, reqlog.OutcomeInterrupted
			log.Printf("videos task=%s: 客户端断开，停止同步等待（任务已提交，可继续查询）", taskID)
			return
		}
		if perr != nil {
			// 等待失败（账号失效/网络抖）：任务其实已提交，不能让客户端以为要重提。
			log.Printf("videos task=%s 等待失败：%v（任务已提交，可继续查询）", taskID, perr)
			pending := upstream.VideoTaskState{Status: "queued"}
			writeJSON(w, http.StatusAccepted, videoTaskPayload(taskID, pending, model, h.videoLedger))
			return
		}
		if !final.Done() {
			writeJSON(w, http.StatusAccepted, videoTaskPayload(taskID, final, model, h.videoLedger))
			return
		}
		if !final.Succeeded() {
			st.status, st.outcome = http.StatusBadGateway, "upstream_failed"
			writeOpenAIErrorHint(w, http.StatusBadGateway, "video_failed",
				"上游出片失败："+final.ErrorText, "任务 "+taskID+" 已终止（failed），可重新提交")
			return
		}
		// 同步等到出片也要先抢记账权：事后用 GET /v1/videos/{id} 复查不能重复扣一次。
		if h.videoLedger.claimCharge(taskID) {
			h.recordVideoUsage(st, realm, a.UID, bare, modelRate, taskID, final, start)
		}
		writeJSON(w, http.StatusOK, videoTaskPayload(taskID, final, model, h.videoLedger))
		return
	}

	status, code := http.StatusBadGateway, "upstream_error"
	var ue *upstream.Error
	switch {
	case len(tried) == 0:
		status, code = http.StatusServiceUnavailable, "no_account"
		lastErr = fmt.Errorf("没有可用的 %s 账号（视频只在 %s 域出片）", realm, realm)
	case errors.As(lastErr, &ue) && ue.Status >= 400:
		status = ue.Status
	}
	st.status, st.outcome = status, ""
	log.Printf("videos model=%s realm=%s: 提交全部失败（%d 个账号）：%v", bare, realm, len(tried), lastErr)
	writeOpenAIError(w, status, code, lastErr.Error())
}

// videoRequest 解析提交请求体：必须是 JSON 对象（视频没有 multipart 形态）。
func videoRequest(raw []byte) (map[string]any, error) {
	var req map[string]any
	if err := json.Unmarshal(raw, &req); err != nil || req == nil {
		return nil, errors.New("invalid JSON body（视频生成只收 application/json）")
	}
	return req, nil
}

// videoUpstreamBody 把「客户端形态」改成上游要的形态：
//
//	model 换裸名；prompt/seconds/negative_prompt/watermark 留在顶层；
//	image → image_url、last_image → last_image_url（图生视频 / 首尾帧）；
//	resolution / enable_audio / aspect_ratio 收进 extra_parameters
//	（客户端直接给 extra_parameters 时以它为准，再补顶层同名字段）；
//	wait / timeout_seconds 是网关自己的开关，不下发；
//	其余未知字段原样透传（不猜上游新参数，也不替它校验）。
//
// watermark 缺省 true：插件侧一直显式带 true，这里对齐同默认，客户端可关掉。
func videoUpstreamBody(req map[string]any, bare string) (map[string]any, error) {
	out := map[string]any{"model": bare}
	extra := map[string]any{}
	if m, ok := req["extra_parameters"].(map[string]any); ok {
		for k, v := range m {
			extra[k] = v
		}
	}
	for _, k := range []string{"resolution", "aspect_ratio", "enable_audio"} {
		if v, ok := req[k]; ok && v != nil && v != "" {
			extra[k] = v
		}
	}
	if v, ok := req["watermark"].(bool); ok {
		out["watermark"] = v
	} else {
		out["watermark"] = true
	}
	if s, _ := req["image"].(string); s != "" {
		out["image_url"] = s
	}
	if s, _ := req["last_image"].(string); s != "" {
		out["last_image_url"] = s
	}
	// OpenAI Videos 的入参形态：参考图在 input_reference.image_url，时长是字符串 "4"，
	// 尺寸是 "1280x720"。上游要的是顶层 image_url + 数字秒 + aspect_ratio。
	if ref, ok := req["input_reference"].(map[string]any); ok {
		if u, _ := ref["image_url"].(string); u != "" && out["image_url"] == nil {
			out["image_url"] = u
		}
		delete(req, "input_reference")
	}
	if s, ok := req["seconds"].(string); ok {
		if n, err := strconv.Atoi(s); err == nil {
			req["seconds"] = n
		}
	}
	if s, _ := req["size"].(string); s != "" {
		// shortcut: 只从宽高比反推 aspect_ratio；OpenAI 的像素高度（1280x720）推不出
		// 上游的 768P/480P 档位，缺省档交给上游，客户端显式给 resolution 时另走上面那条。
		if _, given := extra["aspect_ratio"]; !given {
			if ar := aspectFromSize(s); ar != "" {
				extra["aspect_ratio"] = ar
			}
		}
		delete(req, "size")
	}
	for k, v := range req {
		switch k {
		case "extra_parameters", "resolution", "aspect_ratio", "enable_audio",
			"wait", "timeout_seconds", "model", "image", "last_image":
			continue
		}
		if v == nil {
			continue
		}
		out[k] = v
	}
	if len(extra) > 0 {
		out["extra_parameters"] = extra
	}
	return out, nil
}

// aspectFromSize "1280x720"（宽 x 高）→ "16:9"。认不出的尺寸返回空串（不猜）。
func aspectFromSize(s string) string {
	var w, h int
	if n, _ := fmt.Sscanf(s, "%dx%d", &w, &h); n != 2 || w <= 0 || h <= 0 {
		return ""
	}
	switch {
	case w == h:
		return "1:1"
	case w > h:
		return "16:9"
	}
	return "9:16"
}

// videoWait 解析网关自己的等待开关：
//
//	wait:false            → 立刻返回 task id（不等）
//	timeout_seconds: 1..1800 → 等待上限
//	都没给                 → 同步等到出片（缺省 15 分钟）
func (h *Handler) videoWait(req map[string]any) (time.Duration, bool) {
	if v, ok := req["wait"].(bool); ok && !v {
		return 0, false
	}
	wait := videoWaitDefault
	if v, ok := req["timeout_seconds"]; ok {
		if n, ok := numberToInt(v); ok && n > 0 {
			wait = time.Duration(n) * time.Second
			if wait > videoWaitMax {
				wait = videoWaitMax
			}
		}
	}
	return wait, true
}

// awaitVideo 轮询到终态或超时。返回最后一次状态（未终态时 Done()=false）。
// ctx 取消（客户端断开）时立即停手并返回 ctx.Err()：没人等结果了，再问上游只是白占账号的查询额度。
func (h *Handler) awaitVideo(ctx context.Context, a *auth.Auth, taskID string, wait time.Duration) (upstream.VideoTaskState, error) {
	deadline := time.Now().Add(wait)
	var last upstream.VideoTaskState
	for {
		state, _, _, err := h.videoLedger.pollShared(taskID, func() (upstream.VideoTaskState, string, string, error) {
			h.videoLedger.markQuery(taskID)
			data, err := h.cfg.Upstream.VideoTask(a, taskID)
			if err != nil {
				return upstream.VideoTaskState{}, a.UID, a.Nickname, err
			}
			st := upstream.ParseVideoTask(data)
			st.ID = taskID
			return st, a.UID, a.Nickname, nil
		})
		if err != nil {
			return last, err
		}
		last = state
		h.videoLedger.noteState(taskID, last)
		if last.Done() {
			return last, nil
		}
		if time.Now().Add(videoPollInterval).After(deadline) {
			return last, nil
		}
		t := time.NewTimer(videoPollInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return last, ctx.Err()
		case <-t.C:
		}
	}
}

// videoTask 查任务：GET /v1/videos/{task_id}。与 videoTasks（POST 形态）共用实现。
func (h *Handler) videoTask(w http.ResponseWriter, r *http.Request) {
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/videos/"), "/")
	if id == "" || id == "tasks" || id == "generations" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "缺少 task_id：GET /v1/videos/{task_id}")
		return
	}
	h.pollVideoTask(w, r, id)
}

// videoTasks POST /v1/videos/tasks {"task_id": "..."} —— 有些客户端（含部分 MCP 桥）
// 只肯发 POST，给同一件事留一个入口。
func (h *Handler) videoTasks(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var req struct {
		TaskID string `json:"task_id"`
		ID     string `json:"id"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	id := strings.TrimSpace(req.TaskID)
	if id == "" {
		id = strings.TrimSpace(req.ID)
	}
	if id == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "task_id is required")
		return
	}
	h.pollVideoTask(w, r, id)
}

// videoContent GET /v1/videos/{task_id}/content —— OpenAI Videos 的成品下载口。
//
// 上游给的是签名直链（COS，约 12h 有效），本网关不落盘：把字节流原样转给客户端，省掉自己再存
// 一份 3 MB 的 mp4。任务的定位、单飞、频率护栏与首次出片记账全部复用 GET /v1/videos/{id} 的
// pollVideoTask，不存在第二套：
//   - 状态快照只在内存里，提交时存的是 queued、重启后更是空的；没有已出片的快照就按查询路径问一次
//     所属账号，所以「上游已经出片、网关还停在旧快照」的任务不会被误判成没出片；
//   - 台账里没有的 id 不探测（noProbe）：取字节是已知任务的事，不该多一个盲查上游的入口，
//     重启后的任务先用 GET /v1/videos/{id} 收养即可。
//
// 任务没出片回 409。
func (h *Handler) videoContent(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/videos/"), "/"), "/content")
	h.pollVideoTask(w, r, id, videoPollOpts{noProbe: true, serve: h.serveVideoContent})
}

// serveVideoContent 把已查到的任务状态变成成品字节流（pollVideoTask 的 serve 回调）。
//
// 不静默截断：长度已知且超限就在写响应头之前报 502；长度未知（分块 / 被压缩）时边转边数，
// 超限或读断了就中止连接，让客户端看到一次失败而不是一个被截短的 mp4。只在长度已知时才发
// Content-Length，且发的就是将要写出的长度。
func (h *Handler) serveVideoContent(w http.ResponseWriter, r *http.Request, st *chatStat, taskID string, state upstream.VideoTaskState) {
	fail := func(status int, code, msg, hint string) {
		st.status, st.outcome = status, "http_error"
		writeOpenAIErrorHint(w, status, code, msg, hint)
	}
	recheck := "GET /v1/videos/" + taskID + " 复查"
	if !state.Succeeded() {
		msg := "任务还没有出片"
		if state.Done() {
			msg = "任务已失败，没有成品"
		}
		fail(http.StatusConflict, "video_not_ready", msg, "GET /v1/videos/"+taskID+" 查到 completed 再来取字节")
		return
	}
	if len(state.URLs) == 0 {
		fail(http.StatusBadGateway, "upstream_error", "任务已完成但上游没给成品直链", recheck)
		return
	}
	resp, err := openMedia(r.Context(), state.URLs[0])
	if errors.Is(err, errMediaBusy) {
		w.Header().Set("Retry-After", "5")
		fail(http.StatusServiceUnavailable, "media_busy", err.Error(), "稍后重试，或直接用 GET /v1/videos/"+taskID+" 里的直链下载")
		return
	}
	if err != nil {
		hint := recheck
		var se *mediaStatusError
		if errors.As(err, &se) {
			hint = "签名直链一般 12 小时有效，过期请用 " + recheck
		}
		log.Printf("videos task=%q: 取成品失败：%v", taskID, err)
		fail(http.StatusBadGateway, "upstream_error", "取成品失败："+err.Error(), hint)
		return
	}
	defer resp.Body.Close()
	n := resp.ContentLength // -1 = 长度未知；0 是确实为空，两者不能混
	switch {
	case n == 0:
		fail(http.StatusBadGateway, "upstream_error", "成品直链回了空内容", recheck)
		return
	case n > videoContentMax:
		fail(http.StatusBadGateway, "upstream_error",
			fmt.Sprintf("成品 %d 字节，超过转发上限 %d MiB，不转发；请直接用 GET /v1/videos/%s 里的直链下载", n, videoContentMax>>20, taskID), "")
		return
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "video/mp4"
	}
	w.Header().Set("Content-Type", ct)
	if n > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return // HEAD 只要头：不读正文，deferred Close 直接放弃这次下载
	}
	// 拷贝出错分两种：客户端自己走了（写失败，ctx 已取消）记 499，不算上游故障；上游读短 / 超时记 502。
	copyFailed := func(written int64, err error) {
		if r.Context().Err() != nil {
			st.status, st.outcome = 499, reqlog.OutcomeInterrupted
			return
		}
		st.status, st.outcome = http.StatusBadGateway, "http_error"
		log.Printf("videos task=%q: 成品转发中断（已转 %d 字节，上限 %d MiB）：%v", taskID, written, videoContentMax>>20, err)
	}
	if n > 0 {
		// 长度已知：传输层保证读到的不会比声明的多；读短了 net/http 会掐断连接，客户端同样看到失败。
		if written, err := io.Copy(w, resp.Body); err != nil {
			copyFailed(written, err)
		}
		return
	}
	written, err := io.Copy(w, io.LimitReader(resp.Body, videoContentMax+1))
	if written > videoContentMax {
		err = &mediaTooLargeError{limit: videoContentMax}
	}
	if err != nil {
		copyFailed(written, err)
		panic(http.ErrAbortHandler) // 响应头已发出，只能掐断连接；不能让分块响应正常收尾
	}
}

// videoNotFoundHint 查不到任务时的提示（台账未命中 / 所属账号已不在号池 / 账号说不是它的任务）。
const videoNotFoundHint = "任务只对提交它的账号可见；网关重启后台账会清空，仍可用同一 task_id 重试（会有限次地逐个账号探测，见 videos_probe.go）"

func writeVideoNotFound(w http.ResponseWriter, taskID, hint string) {
	writeOpenAIErrorHint(w, http.StatusNotFound, "task_not_found", "查不到任务 "+taskID, hint)
}

// queryVideoTask 问台账里记着的那个账号一次。
func (h *Handler) queryVideoTask(taskID string, a *auth.Auth) (upstream.VideoTaskState, string, string, error) {
	data, err := h.cfg.Upstream.VideoTask(a, taskID)
	if err != nil {
		return upstream.VideoTaskState{}, a.UID, a.Nickname, err
	}
	state := upstream.ParseVideoTask(data)
	if state.ID == "" {
		state.ID = taskID
	}
	return state, a.UID, a.Nickname, nil
}

// videoPollOpts pollVideoTask 的可选行为（缺省 = GET /v1/videos/{id} 的行为）。
type videoPollOpts struct {
	// noProbe 台账里没有的 id 直接 404，不做有界探测（/content 用：取字节只对已知任务有意义）。
	noProbe bool
	// serve 拿到状态后替代「回任务 JSON」的出口。
	serve func(w http.ResponseWriter, r *http.Request, st *chatStat, taskID string, state upstream.VideoTaskState)
}

// pollVideoTask 查一次任务状态；completed 时（首次）补记扣费。
//
// 台账里没有的任务走有界探测（videos_probe.go）；台账命中后分四条路径：
//  1. 台账里已有终态快照 → 直接回缓存，不打上游（上游 usage 只回一次，密集查询还会 429）；
//  2. 频率护栏（最小间隔 / 限流冷却）内的查询 → 回快照，不打上游；
//  3. 正常查询 → 单飞合并并发请求，成功则记快照、首次出片记账；
//  4. 撞上上游限流（429）→ 记冷却窗口，有旧快照就顶上，没有才回 429 + 提示。
func (h *Handler) pollVideoTask(w http.ResponseWriter, r *http.Request, taskID string, opts ...videoPollOpts) {
	var opt videoPollOpts
	if len(opts) > 0 {
		opt = opts[0]
	}
	// reply 拿到状态后的出口：缺省回任务 JSON；/content 换成转发字节（见 videoContent）。
	reply := func(st *chatStat, state upstream.VideoTaskState, model string) {
		if opt.serve != nil {
			opt.serve(w, r, st, taskID, state)
			return
		}
		writeJSON(w, http.StatusOK, videoTaskPayload(taskID, state, model, h.videoLedger))
	}
	start := time.Now()
	st := &chatStat{start: start, model: "", mode: "video", toks: -1, budget: h.budget}
	if tr := requestTraceFrom(r); tr != nil {
		tr.stat = st
		st.requestID = tr.id
		st.clientIP, st.userAgent = tr.clientIP, tr.userAgent
	}
	defer st.done()

	owner, known := h.videoLedger.get(taskID)
	if !known {
		if opt.noProbe {
			st.status, st.outcome = http.StatusNotFound, "http_error"
			writeVideoNotFound(w, taskID, videoNotFoundHint)
			return
		}
		h.pollUnknownVideoTask(w, st, taskID)
		return
	}
	realm := owner.Realm
	if realm == "" {
		realm = "cn"
	}

	// 1) 终态快照：completed / failed 之后结果不会变，回缓存最省也最稳。
	if cached, _, ok := h.videoLedger.cachedState(taskID); ok && cached.Done() {
		st.uid, st.model = owner.UID, owner.Model
		st.status, st.outcome = http.StatusOK, "success"
		reply(st, cached, requestedModel(owner.Model, realm))
		return
	}

	// 2) 频率护栏：同一任务的最小间隔没到、或刚被上游限流过，就直接回快照，不再碰上游。
	if h.videoLedger.pollWait(taskID) > 0 {
		if cached, _, ok := h.videoLedger.cachedState(taskID); ok {
			st.uid, st.model = owner.UID, owner.Model
			st.status, st.outcome = http.StatusOK, "throttled"
			reply(st, cached, requestedModel(owner.Model, realm))
			return
		}
	}

	// 所属账号已被移出号池：不扇出到别的账号（任务只有它看得见），直接 404。
	acct := h.cfg.Pool.AuthByUID(owner.UID)
	if acct == nil {
		st.status, st.outcome = http.StatusNotFound, "http_error"
		writeVideoNotFound(w, taskID, videoNotFoundHint)
		return
	}

	// 2) 正常查询（并发同一任务只打一次上游）。
	state, uid, nick, err := h.videoLedger.pollShared(taskID, func() (upstream.VideoTaskState, string, string, error) {
		h.videoLedger.markQuery(taskID)
		return h.queryVideoTask(taskID, acct)
	})
	if err == nil {
		st.uid, st.nick, st.model = uid, nick, owner.Model
		st.status, st.outcome = http.StatusOK, "success"
		h.videoLedger.noteState(taskID, state)
		if state.Succeeded() && h.videoLedger.claimCharge(taskID) {
			h.recordVideoUsage(st, realm, uid, owner.Model, h.modelRate(realm, owner.Model), taskID, state, start)
		}
		reply(st, state, requestedModel(owner.Model, realm))
		return
	}

	var ue *upstream.Error
	// 3) 上游限流：有快照就顶上（客户端至少拿到上一次状态），没有才回 429。
	if errors.As(err, &ue) && ue.Status == http.StatusTooManyRequests {
		st.uid, st.nick = uid, nick
		h.videoLedger.noteLimited(taskID, videoRateLimitBackoff)
		if cached, _, ok := h.videoLedger.cachedState(taskID); ok {
			st.model = owner.Model
			st.status, st.outcome = http.StatusOK, "rate_limited_cached"
			reply(st, cached, requestedModel(owner.Model, realm))
			return
		}
		st.status, st.outcome = ue.Status, "http_error"
		writeOpenAIErrorHint(w, http.StatusTooManyRequests, "upstream_error", ue.Msg,
			"上游对同一账号的密集查询限流：隔几秒再查；提交时用 wait=false 拿 task_id 更省心")
		return
	}

	// 查询失败（含上面的 429）一律不调 applyErrorPolicy：查任务只是轮询，失败说明不了账号能不能
	// 干活；按账号级冷却处理，会因为一个视频被查得勤就把这个号踢出对话的选号。
	status, code := http.StatusNotFound, "task_not_found"
	if errors.As(err, &ue) && ue.Status >= 400 {
		status, code = ue.Status, "upstream_error"
	}
	st.status, st.outcome = status, "http_error"
	log.Printf("videos task=%s: 查询失败：%v", taskID, err)
	writeOpenAIErrorHint(w, status, code, "查不到任务 "+taskID, videoNotFoundHint)
}

// modelRate 取模型费率（上游 ModelRate），Client 为空时给空串。
func (h *Handler) modelRate(realm, bare string) string {
	if h.cfg.Upstream == nil {
		return ""
	}
	return h.cfg.Upstream.ModelRate(realm, bare)
}

// requestedModel 回执里的 model 字段：有裸名就补回 realm 前缀，没有就留空。
func requestedModel(bare, realm string) string {
	if bare == "" {
		return ""
	}
	return realm + ":" + bare
}

// recordVideoUsage 出片完成时补记扣费（与出图同一套账：流水行 + 成本账本 + 用量时序）。
func (h *Handler) recordVideoUsage(st *chatStat, realm, uid, bare, modelRate, taskID string, state upstream.VideoTaskState, start time.Time) {
	if bare != "" {
		st.model = bare
	}
	h.videoLedger.noteResult(taskID, state)
	st.status, st.outcome = http.StatusOK, "success"
	if state.Tokens > 0 {
		st.totalTokens = state.Tokens
		st.toks = int(state.Tokens)
	}
	if state.Credit > 0 {
		st.credit, st.hasCredit = state.Credit, true
		h.cfg.Pool.NoteModelCost(uid, bare, state.Credit, int(state.Tokens))
	}
	log.Printf("videos %s model=%s: 出片 task=%s %ds credit=%.2f 用时=%s",
		logfmt.Label(uid, st.nick), bare, taskID, state.Duration, state.Credit,
		time.Since(start).Round(time.Millisecond))
	if h.cfg.Usage != nil {
		h.cfg.Usage.Add(time.Now(), realm, uid, bare, usage.Delta{
			TotalTokens: state.Tokens,
			HasTotal:    state.Tokens > 0,
			LatencyMs:   time.Since(start).Milliseconds(),
			HasLatency:  true,
			Credit:      state.Credit,
			HasCredit:   state.Credit > 0,
			ModelRate:   modelRate,
		}, state.Tokens > 0)
	}
}

// videoTaskStateFromSubmit 把提交回执的 status 包成状态对象（wait=false 时用）。
func videoTaskStateFromSubmit(status string) upstream.VideoTaskState {
	return upstream.VideoTaskState{Status: status}
}

// videoTaskPayload 统一回执形态（提交 / 查询共用），字段对齐 OpenAI Videos 的
// Video 资源（POST /videos 返回的那个对象）：id / object / status / progress /
// created_at / completed_at / size / seconds / error，剩下的 data / videos / usage /
// task_id / hint 是本网关加的自解释字段，OpenAI 客户端会忽略它们。
//
//	{id, object:"video", task_id, status, progress, model, created_at, completed_at,
//	 data:[{url}], videos:[{url}], size, seconds, usage:{credit,output_video_tokens,output_video_duration}, hint}
//
// status 一律归一成 OpenAI 的四态；seconds 是**字符串**（OpenAI 就这么定义的，
// 给数字会让严格反序列化的 SDK 直接报错）；error 是对象不是字符串，同理。
func videoTaskPayload(taskID string, state upstream.VideoTaskState, model string, ledger *videoTaskStore) map[string]any {
	// 上游只回一次 usage：兜底用台账里记下的数，别让重复查询少一笔账。
	if res, ok := ledger.result(taskID); ok {
		if state.Credit <= 0 && res.Credit > 0 {
			state.Credit, state.Tokens = res.Credit, res.Tokens
			if state.Duration <= 0 {
				state.Duration = res.Duration
			}
		}
		if state.Size == "" {
			state.Size = res.Size
		}
	}
	out := map[string]any{
		"id":         taskID,
		"object":     "video",
		"task_id":    taskID,
		"status":     openAIVideoStatus(state.Status),
		"progress":   videoProgress(state),
		"created_at": state.CreatedAt,
		// OpenAI 的 Video 资源必带这几个键；本网关不知道的给 null，别让严格客户端缺字段。
		"expires_at":            nil,
		"remixed_from_video_id": nil,
		"error":                 nil,
	}
	if model != "" {
		out["model"] = model
	}
	if state.CompletedAt > 0 {
		out["completed_at"] = state.CompletedAt
	}
	if state.Size != "" {
		out["size"] = state.Size
	}
	if state.Duration > 0 {
		out["seconds"] = strconv.FormatInt(state.Duration, 10) // OpenAI 定义成字符串，见函数头
	}
	if len(state.URLs) > 0 {
		items := make([]map[string]any, 0, len(state.URLs))
		for _, u := range state.URLs {
			items = append(items, map[string]any{"url": u})
		}
		out["data"] = items
		out["videos"] = items
	}
	if state.Credit > 0 {
		out["usage"] = map[string]any{
			"credit":                   state.Credit,
			"output_video_tokens":      state.Tokens,
			"output_video_duration":    state.Duration,
			"output_video_coefficient": state.Coefficient,
		}
	}
	switch {
	case state.Succeeded():
		out["hint"] = "视频地址是签名直链（约 12 小时有效）；需要长期保存请自行落盘，或 GET /v1/videos/" + taskID + "/content 取字节。"
	case state.Done():
		out["error"] = map[string]any{"code": "video_failed", "message": state.ErrorText}
	default:
		out["hint"] = "任务未完成：GET /v1/videos/" + taskID + " 继续查（或 POST /v1/videos/tasks {\"task_id\":\"" + taskID + "\"}）"
	}
	return out
}

// openAIVideoStatus 把上游状态归一到 OpenAI Videos 的四态（queued / in_progress /
// completed / failed）；客户端是按这四态分支的，上游的 succeeded / running 它们不认。
func openAIVideoStatus(s string) string {
	switch s {
	case "", "queued", "pending", "created":
		return "queued"
	case "completed", "succeeded", "success":
		return "completed"
	case "failed", "error":
		return "failed"
	}
	return "in_progress"
}

// videoProgress 粗略进度：上游只给状态不给百分比，出片了就是 100，其余 0。
//
// shortcut: 没有真实百分比，只在 completed 时给 100；上游哪天回 progress 字段再改这里。
func videoProgress(state upstream.VideoTaskState) int {
	if state.Succeeded() {
		return 100
	}
	return 0
}

// clipText 截断超长文本（错误信息里带上游原文时用，避免把整段回执刷进日志）。
func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// numberToInt JSON 数字（float64）转 int。
func numberToInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}
