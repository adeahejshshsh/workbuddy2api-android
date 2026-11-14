// three_d.go 3D 生成 REST 内核：账号池提交、原账号查询，MCP 也复用这里。
package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

const (
	threeDAttempts   = 3
	threeDTaskTTL    = 24 * time.Hour
	threeDTaskMax    = 2048
	threeDMaxBody    = 12 << 20
	threeDImageLimit = 6 << 20
	// threeDTaskMinPoll 两次真打上游之间的最小间隔（上游密集查询会限流）。
	threeDTaskMinPoll = 3 * time.Second
)

var threeDPollInterval = 5 * time.Second

// threeDProbeConfig 3D 探测器的参数。与视频各用一套额度，互不抢占。
var threeDProbeConfig = probeConfig{
	max: 5, budget: 30, retry: 30 * time.Second,
	keep: 10 * time.Minute, pause: 30 * time.Second, missed: 1024,
	label: "3d",
}

// threeDTaskOwner 任务台账条目：上游任务只对提交它的账号可见，查询必须回到同一账号。
//
// 租户模型：网关只有一个共享 API key，Auth 里也没有调用方 id，所以台账只记「提交账号」
// 而不记「调用方」——持有 key 即可查任意 job_id，与视频功能同一信任模型（README 已写明）。
type threeDTaskOwner struct {
	UID, Realm string
	Model      string
	Created    time.Time
	// Charged 已记账标记：查到终态时按 ResultCreditConsumed 记一次，重复查询不再记。
	Charged bool
	// 用量快照：上游 credit 只在第一次查到终态时回，之后重复查询不带，不记住就会丢账。
	Credit   float64
	HasUsage bool
	State    upstream.ThreeDState
	HasState bool
	// LastQuery 上次真打上游的时刻（最小间隔护栏）。
	LastQuery time.Time
}

// threeDTaskResult 台账里缓存的用量信息（给重复查询兜底）。
type threeDTaskResult struct {
	Credit   float64
	HasUsage bool
}

type threeDTaskStore struct {
	mu      sync.Mutex
	m       map[string]*threeDTaskOwner
	flights map[string]*threeDFlight
}

type threeDFlight struct {
	done  chan struct{}
	state upstream.ThreeDState
	err   error
}

func newThreeDTaskStore() *threeDTaskStore {
	return &threeDTaskStore{m: map[string]*threeDTaskOwner{}, flights: map[string]*threeDFlight{}}
}

func (s *threeDTaskStore) put(id, uid, realm, model string, state upstream.ThreeDState) {
	if s == nil || id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[id] = &threeDTaskOwner{UID: uid, Realm: realm, Model: model, Created: time.Now(), State: state, HasState: true}
	s.gcLocked()
}

// adopt 把「台账里没有、但某个账号确实能查到」的任务补进来（进程重启后的自愈路径）。
//
// charged 传「收养时任务是否已到终态」：已完成的任务多半在重启前被查过、记过账，再记一次就是
// 重复计入；还在跑的任务不可能在任何地方记过账，留到终态时照常记一次。
func (s *threeDTaskStore) adopt(id, uid, realm, model string, charged bool) {
	if s == nil || id == "" || uid == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[id]; ok {
		return
	}
	s.m[id] = &threeDTaskOwner{UID: uid, Realm: realm, Model: model, Created: time.Now(), Charged: charged}
}

func (s *threeDTaskStore) get(id string) (threeDTaskOwner, bool) {
	if s == nil || id == "" {
		return threeDTaskOwner{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.m[id]
	if !ok {
		return threeDTaskOwner{}, false
	}
	return *o, true
}

func (s *threeDTaskStore) note(id string, state upstream.ThreeDState) {
	if s == nil || id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, ok := s.m[id]; ok {
		o.State, o.HasState, o.LastQuery = state, true, time.Now()
	}
}

// noteResult 记下这次终态的用量（幂等，可重复调用）。
func (s *threeDTaskStore) noteResult(id string, state upstream.ThreeDState) {
	if s == nil || id == "" || state.Credit <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, ok := s.m[id]; ok {
		o.Credit, o.HasUsage = state.Credit, true
	}
}

// result 取台账里缓存的用量；上游后续查询不再回 credit 时用它补齐回执。
func (s *threeDTaskStore) result(id string) (threeDTaskResult, bool) {
	if s == nil || id == "" {
		return threeDTaskResult{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.m[id]
	if !ok || !o.HasUsage {
		return threeDTaskResult{}, false
	}
	return threeDTaskResult{Credit: o.Credit, HasUsage: true}, true
}

// claimCharge 尝试把任务标成「已记账」。返回 true 表示本次调用赢得了记账权。
func (s *threeDTaskStore) claimCharge(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.m[id]
	if !ok || o.Charged {
		return false
	}
	o.Charged = true
	return true
}

// pollShared 合并并发的同一任务查询：上游对同一账号密集查询会限流，而 MCP 客户端常常
// 并发轮询同一个 job_id，所以这里做单飞（single-flight）。
//
// ctx 只影响**等待方**：自己的请求被取消时立刻返回，不再等别人；发起方照旧跑完。
func (s *threeDTaskStore) pollShared(ctx context.Context, id string, fn func() (upstream.ThreeDState, error)) (upstream.ThreeDState, error) {
	if s == nil {
		return fn()
	}
	s.mu.Lock()
	if s.flights == nil {
		s.flights = map[string]*threeDFlight{}
	}
	if f, ok := s.flights[id]; ok {
		s.mu.Unlock()
		select {
		case <-f.done:
			return f.state, f.err
		case <-ctx.Done():
			return upstream.ThreeDState{}, ctx.Err()
		}
	}
	f := &threeDFlight{done: make(chan struct{})}
	s.flights[id] = f
	s.mu.Unlock()

	func() {
		defer func() {
			if r := recover(); r != nil {
				f.state, f.err = upstream.ThreeDState{}, fmt.Errorf("3d poll panic: %v", r)
			}
			s.mu.Lock()
			delete(s.flights, id)
			s.mu.Unlock()
			close(f.done)
		}()
		f.state, f.err = fn()
	}()
	return f.state, f.err
}

func (s *threeDTaskStore) gcLocked() {
	if len(s.m) <= threeDTaskMax {
		// 容量内也可能有超龄的（长时间低负载）：顺手按 TTL 清一遍。
		cut := time.Now().Add(-threeDTaskTTL)
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
	drop := len(all) - threeDTaskMax*3/4
	for i := 0; i < drop; i++ {
		delete(s.m, all[i].id)
	}
}

func (h *Handler) threeDGenerations(w http.ResponseWriter, r *http.Request) {
	if !h.loadLive().ThreeDEnabled {
		writeThreeDDisabled(w)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, threeDMaxBody+1))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	if len(raw) > threeDMaxBody {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "3D 请求体不能超过 12 MiB")
		return
	}
	var req map[string]any
	if json.Unmarshal(raw, &req) != nil || req == nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	body, err := threeDUpstreamBody(req)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	model, _ := req["model"].(string)
	if model == "" {
		model = "3.1"
	}
	start := time.Now()
	st := &chatStat{start: start, model: "hy-3d", mode: "3d", toks: -1, budget: h.budget}
	if tr := requestTraceFrom(r); tr != nil {
		tr.stat = st
		st.requestID = tr.id
		st.clientIP, st.userAgent = tr.clientIP, tr.userAgent
	}
	defer st.done()
	wait, timeout := threeDWait(req)
	var lastErr error
	tried := map[string]bool{}
	for attempt := 0; attempt < threeDAttempts; attempt++ {
		a := h.cfg.Pool.PickExcludingForRealm(tried, "hy-3d", "cn")
		if a == nil {
			break
		}
		tried[a.UID] = true
		st.uid, st.nick, st.attempts = a.UID, a.Nickname, attempt+1
		data, err := h.cfg.Upstream.ThreeDGenerations(r.Context(), a, body)
		if err != nil {
			// 没有临时凭证是网关侧缺配置，换号也没用：如实回 503，不罚号、不重试。
			if errors.Is(err, upstream.ErrThreeDNoCredential) {
				writeThreeDNoCredential(w)
				return
			}
			lastErr = err
			var ue *upstream.Error
			if errors.As(err, &ue) {
				h.applyErrorPolicy(a.UID, ue.Kind, ue.Msg, "hy-3d", ue)
				if requestLevelFailFastKind(ue.Kind) {
					writeOpenAIError(w, ue.Status, "upstream_error", ue.Msg)
					return
				}
			}
			continue
		}
		state := upstream.ParseThreeDState(data, "")
		if state.JobID == "" {
			if state.ResultURL != "" {
				// 同步出结果的上游形态：没有 job 可查，直接回执（不记账：上游没回 credit 就无可记）。
				state.Status = "DONE"
				writeJSON(w, http.StatusOK, h.threeDPayload(state.JobID, state, model))
				return
			}
			lastErr = fmt.Errorf("3D 提交回执缺少 JobId：%s", clipText(string(data), 200))
			continue
		}
		if state.Status == "" {
			state.Status = "SUBMITTED"
		}
		h.threeDLedger.put(state.JobID, a.UID, "cn", model, state)
		log.Printf("3d %s: 任务已提交 job=%s", logfmt.Label(a.UID, a.Nickname), state.JobID)
		if !wait {
			writeJSON(w, http.StatusOK, h.threeDPayload(state.JobID, state, model))
			h.recordThreeDUsage(st, a.UID, state, start)
			return
		}
		state, err = h.awaitThreeD(r.Context(), a, state.JobID, timeout)
		// 客户端断连：不再补查、不写回执、不记账（任务本身还在上游跑，客户端可凭 job_id 再查）。
		if err != nil && r.Context().Err() != nil {
			return
		}
		if err != nil {
			state = upstream.ThreeDState{JobID: state.JobID, Status: "PROCESSING", ErrorText: err.Error()}
			h.threeDLedger.note(state.JobID, state)
			writeJSON(w, http.StatusAccepted, h.threeDPayload(state.JobID, state, model))
			return
		}
		// 超时但已拿到终态时必须按终态返回并记账，不能等下一次查询才补。
		if state.Done() {
			h.threeDLedger.note(state.JobID, state)
			h.recordThreeDUsage(st, a.UID, state, start)
			writeJSON(w, http.StatusOK, h.threeDPayload(state.JobID, state, model))
			return
		}
		h.threeDLedger.note(state.JobID, state)
		writeJSON(w, http.StatusAccepted, h.threeDPayload(state.JobID, state, model))
		return
	}
	if lastErr == nil {
		lastErr = errors.New("没有可用的 cn 账号")
	}
	writeOpenAIError(w, http.StatusBadGateway, "upstream_error", lastErr.Error())
}

// writeThreeDDisabled 3D 未开启：403（端点存在、只是没开），与 embeddings / watermark 同一口径。
func writeThreeDDisabled(w http.ResponseWriter) {
	writeOpenAIErrorHint(w, http.StatusForbidden, "three_d_disabled",
		"3D 生成未开启：请在面板配置里打开 features.three_d_enabled（默认关闭）",
		"开启后 POST /v1/3d/generations 与 MCP 工具 generate_3d / get_3d_task 一起可用；关闭时不影响其它接口。")
}

// writeThreeDNoCredential 拿不到 `ck_t_` 临时凭证：网关侧取凭证失败，不是账号问题。
func writeThreeDNoCredential(w http.ResponseWriter) {
	writeOpenAIErrorHint(w, http.StatusServiceUnavailable, "three_d_credential_missing",
		upstream.ErrThreeDNoCredential.Error(),
		"自动签发用账号 access token 调官方 /v2/api-keys 换 ck_t_（features.three_d_auto_token，默认开启）；"+
			"也可手工配置 features.three_d_temp_tokens 覆盖。静态 API Key 账号签不了，详见 README 的 3D 章节。")
}

func threeDUpstreamBody(req map[string]any) (map[string]any, error) {
	model, _ := req["model"].(string)
	if model == "" {
		model = "3.1"
	}
	if model != "3.0" && model != "3.1" {
		return nil, errors.New("model 必须是 3.0 或 3.1")
	}
	body := map[string]any{"Model": model}
	for input, target := range map[string]string{"prompt": "Prompt", "image_url": "ImageUrl", "image_base64": "ImageBase64", "multi_view_images": "MultiViewImages", "enable_pbr": "EnablePBR", "generate_type": "GenerateType", "polygon_type": "PolygonType", "result_format": "ResultFormat"} {
		if v, ok := req[input]; ok && v != nil && v != "" {
			body[target] = v
		}
	}
	if body["Prompt"] == nil && body["ImageUrl"] == nil && body["ImageBase64"] == nil && body["MultiViewImages"] == nil {
		return nil, errors.New("prompt、image_url、image_base64、multi_view_images 至少提供一种")
	}
	if encoded, _ := req["image_base64"].(string); encoded != "" {
		if comma := strings.Index(encoded, ","); strings.HasPrefix(encoded, "data:") && comma >= 0 {
			body["ImageBase64"] = encoded[comma+1:]
			encoded = body["ImageBase64"].(string)
		}
		if _, err := decodeThreeDImage(encoded); err != nil {
			return nil, err
		}
	}
	if views, ok := req["multi_view_images"]; ok && views != nil {
		items, ok := views.([]any)
		if !ok || len(items) == 0 {
			return nil, errors.New("multi_view_images 必须是非空数组")
		}
		for i, item := range items {
			switch v := item.(type) {
			case string:
				if v == "" {
					return nil, errors.New("multi_view_images 不能含空图片")
				}
				if _, err := decodeThreeDImage(v); err != nil {
					return nil, err
				}
				if strings.HasPrefix(v, "data:") {
					items[i] = v[strings.IndexByte(v, ',')+1:]
				}
			case map[string]any:
				for key, value := range v {
					if key == "ViewType" {
						continue
					}
					s, ok := value.(string)
					if !ok || s == "" {
						return nil, errors.New("multi_view_images 的图片地址/base64 必须是非空字符串")
					}
					if strings.HasPrefix(s, "data:") || (!strings.Contains(s, "://") && !strings.Contains(strings.ToLower(key), "url")) {
						if _, err := decodeThreeDImage(s); err != nil {
							return nil, err
						}
						if strings.HasPrefix(s, "data:") {
							v[key] = s[strings.IndexByte(s, ',')+1:]
						}
					}
				}
			default:
				return nil, errors.New("multi_view_images 元素必须是图片字符串或对象")
			}
		}
	}
	if n, ok := req["face_count"]; ok {
		faces, valid := numberToInt(n)
		if !valid || faces < 10000 || faces > 1500000 {
			return nil, errors.New("face_count 必须在 10000～1500000 之间")
		}
		body["FaceCount"] = faces
	}
	generateType, _ := req["generate_type"].(string)
	switch generateType {
	case "", "Normal", "LowPoly", "Geometry", "Sketch":
	default:
		return nil, errors.New("generate_type 只支持 Normal、LowPoly、Geometry、Sketch")
	}
	if generateType == "LowPoly" && model != "3.0" {
		return nil, errors.New("LowPoly 需要 model=3.0")
	}
	if generateType == "Sketch" && (body["Prompt"] == nil || (body["ImageUrl"] == nil && body["ImageBase64"] == nil)) {
		return nil, errors.New("Sketch 需要 prompt 以及 image_url 或 image_base64")
	}
	if body["MultiViewImages"] != nil && (body["Prompt"] != nil || (body["ImageUrl"] == nil && body["ImageBase64"] == nil)) {
		return nil, errors.New("multi_view_images 需要 image_url 或 image_base64，且不能与 prompt 同时提供")
	}
	polygon, _ := req["polygon_type"].(string)
	if polygon != "" && polygon != "triangle" && polygon != "quadrilateral" {
		return nil, errors.New("polygon_type 只支持 triangle、quadrilateral")
	}
	format, _ := req["result_format"].(string)
	if format != "" && format != "STL" && format != "USDZ" && format != "FBX" {
		return nil, errors.New("result_format 只支持 STL、USDZ、FBX（默认另有 OBJ/GLB）")
	}
	return body, nil
}

func decodeThreeDImage(encoded string) ([]byte, error) {
	if strings.HasPrefix(encoded, "data:") {
		comma := strings.IndexByte(encoded, ',')
		if comma < 0 || !strings.HasSuffix(strings.ToLower(encoded[:comma]), ";base64") {
			return nil, errors.New("图片 data URL 必须使用 base64 编码")
		}
		encoded = encoded[comma+1:]
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, errors.New("图片不是合法的 base64 数据")
	}
	if len(decoded) > threeDImageLimit {
		return nil, errors.New("单张图片解码后不能超过 6 MiB")
	}
	return decoded, nil
}

func threeDWait(req map[string]any) (bool, time.Duration) {
	if wait, ok := req["wait"].(bool); ok && !wait {
		return false, 0
	}
	wait, ok := req["wait"].(bool)
	if !ok || !wait {
		return false, 0
	}
	timeout := 10 * time.Minute
	if n, valid := numberToInt(req["timeout_seconds"]); valid && n > 0 {
		timeout = time.Duration(n) * time.Second
		if timeout > 10*time.Minute {
			timeout = 10 * time.Minute
		}
	}
	return true, timeout
}

func (h *Handler) awaitThreeD(ctx context.Context, a *auth.Auth, id string, timeout time.Duration) (upstream.ThreeDState, error) {
	deadline := time.Now().Add(timeout)
	for {
		state, err := h.threeDLedger.pollShared(ctx, id, func() (upstream.ThreeDState, error) {
			data, err := h.cfg.Upstream.ThreeDTask(ctx, a, id)
			if err != nil {
				return upstream.ThreeDState{}, err
			}
			return upstream.ParseThreeDState(data, id), nil
		})
		if err != nil {
			return state, err
		}
		h.threeDLedger.note(id, state)
		if state.Done() || time.Now().Add(threeDPollInterval).After(deadline) {
			return state, nil
		}
		timer := time.NewTimer(threeDPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return state, ctx.Err()
		case <-timer.C:
		}
	}
}

func (h *Handler) threeDTasks(w http.ResponseWriter, r *http.Request) {
	if !h.loadLive().ThreeDEnabled {
		writeThreeDDisabled(w)
		return
	}
	var id string
	if r.Method == http.MethodGet {
		id = strings.TrimPrefix(r.URL.Path, "/v1/3d/")
	} else {
		var req struct {
			JobID string `json:"job_id"`
			ID    string `json:"id"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req) != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
			return
		}
		id = req.JobID
		if id == "" {
			id = req.ID
		}
	}
	id = strings.TrimSpace(id)
	if id == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "job_id is required")
		return
	}
	start := time.Now()
	st := &chatStat{start: start, model: "hy-3d", mode: "3d", toks: -1, budget: h.budget}
	if tr := requestTraceFrom(r); tr != nil {
		tr.stat = st
		st.requestID = tr.id
		st.clientIP, st.userAgent = tr.clientIP, tr.userAgent
	}
	defer st.done()
	owner, ok := h.threeDLedger.get(id)
	if !ok {
		// 台账里没有（进程重启 / 超容量 / 别处提交）：有界探测自愈，与视频同一套机制。
		h.pollUnknownThreeDTask(r.Context(), w, st, id)
		return
	}
	if owner.HasState && owner.State.Done() {
		st.status, st.outcome = http.StatusOK, "success"
		h.recordThreeDUsage(st, owner.UID, owner.State, start)
		writeJSON(w, http.StatusOK, h.threeDPayload(id, owner.State, owner.Model))
		return
	}
	if time.Since(owner.LastQuery) < threeDTaskMinPoll && owner.HasState {
		st.status, st.outcome = http.StatusOK, "success"
		writeJSON(w, http.StatusOK, h.threeDPayload(id, owner.State, owner.Model))
		return
	}
	state, err := h.pollThreeD(r.Context(), id, owner.UID)
	if err != nil {
		if r.Context().Err() != nil {
			return // 客户端断连：不回写、不记账
		}
		if errors.Is(err, errThreeDOwnerGone) {
			writeOpenAIError(w, http.StatusNotFound, "task_not_found", err.Error())
			return
		}
		if errors.Is(err, upstream.ErrThreeDNoCredential) {
			writeThreeDNoCredential(w)
			return
		}
		if owner.HasState {
			st.status, st.outcome = http.StatusOK, "success"
			writeJSON(w, http.StatusOK, h.threeDPayload(id, owner.State, owner.Model))
			return
		}
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	h.threeDLedger.note(id, state)
	h.recordThreeDUsage(st, owner.UID, state, start)
	st.status, st.outcome = http.StatusOK, "success"
	writeJSON(w, http.StatusOK, h.threeDPayload(id, state, owner.Model))
}

// pollThreeD 回到提交账号查一次任务。账号已不在号池时返回 errThreeDOwnerGone：
// 上游任务只有它看得见，扇出到别的账号只会白打请求。
func (h *Handler) pollThreeD(ctx context.Context, id, uid string) (upstream.ThreeDState, error) {
	a := h.cfg.Pool.AuthByUID(uid)
	if a == nil {
		return upstream.ThreeDState{}, errThreeDOwnerGone
	}
	return h.threeDLedger.pollShared(ctx, id, func() (upstream.ThreeDState, error) {
		data, err := h.cfg.Upstream.ThreeDTask(ctx, a, id)
		if err != nil {
			return upstream.ThreeDState{}, err
		}
		return upstream.ParseThreeDState(data, id), nil
	})
}

var errThreeDOwnerGone = errors.New("提交此任务的账号已不在账号池")

// probeThreeDTask 3D 有界探测：命中就收养进台账并返回状态，否则返回探测错误。
func (h *Handler) probeThreeDTask(ctx context.Context, jobID string) (upstream.ThreeDState, *auth.Auth, error) {
	var state upstream.ThreeDState
	a, err := h.probeAccounts(&h.threeDProbe, jobID, func(a *auth.Auth) error {
		data, err := h.cfg.Upstream.ThreeDTask(ctx, a, jobID)
		if err != nil {
			if probeNotMine(err) {
				return errProbeNotMine
			}
			return err
		}
		state = upstream.ParseThreeDState(data, jobID)
		return nil
	})
	if err != nil {
		return upstream.ThreeDState{}, nil, err
	}
	// 已终态的多半在重启前被查过、记过账：收养时标已记账；还在跑的终态时照常记一次。
	h.threeDLedger.adopt(jobID, a.UID, a.Realm(), "", state.Done())
	probeAdoptLog(threeDProbeConfig.label, jobID, a)
	return state, a, nil
}

// pollUnknownThreeDTask 台账里没有的 job_id：有界探测（并发同一 id 只探一次），
// 找到就收养并按已知任务回执，找不到回 404。st 由调用方 defer st.done()。
func (h *Handler) pollUnknownThreeDTask(ctx context.Context, w http.ResponseWriter, st *chatStat, jobID string) {
	var (
		state upstream.ThreeDState
		a     *auth.Auth
		err   error
	)
	state, err = h.threeDLedger.pollShared(ctx, jobID, func() (upstream.ThreeDState, error) {
		state, a, err = h.probeThreeDTask(ctx, jobID)
		return state, err
	})
	if ctx.Err() != nil {
		return // 客户端断连：不回写
	}
	if err != nil {
		if errors.Is(err, upstream.ErrThreeDNoCredential) {
			writeThreeDNoCredential(w)
			return
		}
		var ue *upstream.Error
		if errors.As(err, &ue) && ue.Status == http.StatusTooManyRequests {
			// 探测撞上限流：如实回 429（客户端可退避），但不罚号。
			st.status, st.outcome = ue.Status, "http_error"
			writeOpenAIErrorHint(w, http.StatusTooManyRequests, "upstream_error", ue.Msg,
				"上游对同一账号的密集查询限流：隔几秒再查；提交时用 wait=false 拿 job_id 更省心")
			return
		}
		st.status, st.outcome = http.StatusNotFound, "http_error"
		hint := "任务只对提交它的账号可见，网关重启后需要重新探测；刚提交的任务稍后重试即可。"
		if errors.Is(err, errProbeSkipped) {
			hint = "这个 job_id 近期刚探测过或探测额度已用尽，网关没有再问上游；过一会儿再试"
		}
		writeOpenAIErrorHint(w, http.StatusNotFound, "task_not_found", "查不到此 3D 任务", hint)
		return
	}
	owner, _ := h.threeDLedger.get(jobID)
	st.uid, st.nick, st.model = a.UID, a.Nickname, owner.Model
	st.status, st.outcome = http.StatusOK, "success"
	h.threeDLedger.note(jobID, state)
	writeJSON(w, http.StatusOK, h.threeDPayload(jobID, state, owner.Model))
}

func (h *Handler) recordThreeDUsage(st *chatStat, uid string, state upstream.ThreeDState, start time.Time) {
	st.uid = uid
	if state.Credit <= 0 {
		return
	}
	// 先把用量记进台账（重复查询要用），再抢记账权：抢不到说明已记过，快照仍在。
	h.threeDLedger.noteResult(state.JobID, state)
	if !h.threeDLedger.claimCharge(state.JobID) {
		return
	}
	st.credit, st.hasCredit = state.Credit, true
	h.cfg.Pool.NoteModelCost(uid, "hy-3d", state.Credit, 0)
	if h.cfg.Usage != nil {
		h.cfg.Usage.Add(time.Now(), "cn", uid, "hy-3d", usage.Delta{
			Credit: state.Credit, HasCredit: true,
			LatencyMs: time.Since(start).Milliseconds(), HasLatency: true,
		}, false)
	}
}

// threeDPayload 组装回执。model 取台账里记下的提交版本；上游后续查询不再回 credit 时
// 用台账快照补齐，免得重复查询看不到积分明细。
func (h *Handler) threeDPayload(id string, state upstream.ThreeDState, model string) map[string]any {
	if model == "" {
		model = "3.1"
	}
	out := map[string]any{
		"id": id, "job_id": id, "object": "3d.task",
		"status": state.Status, "model": model,
	}
	if state.RequestID != "" {
		out["request_id"] = state.RequestID
	}
	if state.ResultURL != "" {
		out["result_url"] = state.ResultURL
	}
	if len(state.Result) > 0 {
		var raw any
		if json.Unmarshal(state.Result, &raw) == nil {
			out["raw_result"] = raw
			if m, ok := raw.(map[string]any); ok {
				if files, ok := m["ResultFile3Ds"]; ok {
					out["result_files"] = files
				}
			}
		}
	}
	credit := state.Credit
	if credit <= 0 && id != "" {
		if cached, ok := h.threeDLedger.result(id); ok {
			credit = cached.Credit
		}
	}
	if credit > 0 {
		out["usage"] = map[string]any{"credit": credit}
	}
	if state.ErrorText != "" {
		out["error"] = map[string]any{"message": state.ErrorText}
	}
	if state.Succeeded() {
		out["hint"] = "3D 产物是签名直链，通常约 24 小时有效；请尽快下载 GLB/OBJ 等文件保存。"
	} else if !state.Done() {
		out["hint"] = "使用 get_3d_task 按原 job_id 继续查询，不要重复提交。"
	}
	return out
}
