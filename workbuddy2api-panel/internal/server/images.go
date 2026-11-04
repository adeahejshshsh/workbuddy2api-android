// images.go 图像端点：POST /v1/images/generations（文生图）与 /v1/images/edits（图生图）。
//
// 上游图像模型不进**对话**列表（目录按 tag text-to-image 剔除，选它发对话会 11102），
// 所以模型名不走目录校验，按 realm 前缀路由到对应账号池：
//
//	cn:hunyuan-image-alpha         国内版（实测 200 + 签名 COS URL）
//	cn:hunyuan-image-alpha-edit    国内版图生图
//	global:hy-image-3.5            国际版（2026-10-22 实测 200 + credit 0.27/张）
//	global:gpt-image-2.5-sunburst  已下架（400 code=14401 route config not found）
//
// 裸名 → realm 的映射表在 upstream（imageRouteRealm），已下架模型表为 imageDeadModels；
// 两者都只写实测过的组合，未知模型照旧放行（不猜）。
//
// 请求体只把 model 换成裸名（前缀是网关路由协议，上游只认裸名），其余原样透传；
// multipart 请求（OpenAI SDK 的 images.edit 走这条）就地转成上游要的 JSON（图片转 data URL）。
// 回程把上游 data 原样写出——它已是 {created,data:[{url}],usage}，不转存、不改写。
package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// defaultImageModel 请求未带 model 时的缺省图像模型（国内版；realm 由前缀决定）。
const defaultImageModel = "cn:hunyuan-image-alpha"

// firstAllowedGenerationImage 白名单兜底（issue #68 设计决策 4）：从最近一次目录
// 解析留存的出图模型里，按 cn 优先、global 其次返回第一个「白名单内且未被黑名单
// 隐藏」的 image 模型 (realm, 裸名)。白名单未启用 / 无目录 / 无命中 → false，
// 调用方维持默认名走拒绝路径（不做隐式放行）。
func (h *Handler) firstAllowedGenerationImage() (string, string, bool) {
	if !h.allowlistEnabled() || h.cfg.Upstream == nil {
		return "", "", false
	}
	realms := []string{"cn"}
	if h.cfg.GlobalEnabled {
		realms = append(realms, "global")
	}
	for _, realm := range realms {
		for _, mi := range h.cfg.Upstream.GenerationModels(realm) {
			if mi.GenerationKind != "image" {
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

// imageAttempts 失败换号上限（与 chat 路径同量级：图像按次人工触发，3 次足够绕开坏号）。
const imageAttempts = 3

// imagesGenerations / imagesEdits 共用实现；edit 决定上游路径与入参校验。
func (h *Handler) images(w http.ResponseWriter, r *http.Request, edit bool) {
	start := time.Now()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	req, err := imageRequest(r, raw, edit)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	// 出图也有「上下文」（prompt，编辑还有原图）：同一开关，同一处归档。
	if h.loadLive().RecordContext {
		if tr := requestTraceFrom(r); tr != nil {
			tr.context = archiveContext(raw)
		}
	}
	prompt, _ := req["prompt"].(string)
	if strings.TrimSpace(prompt) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			`prompt is required（例：{"model":"`+defaultImageModel+`","prompt":"一只猫"}）`)
		return
	}
	model, _ := req["model"].(string)
	named := strings.TrimSpace(model) != "" // 客户端是否显式点名了模型
	if !named {
		model = defaultImageModel
	}
	realm, bare := resolveModel(model)
	// models.allowed 白名单（issue #68）：与 chat 入口同款拒绝，不打上游、不占选号。
	// 未点名模型时先做白名单内兜底（设计决策 4）：默认模型不在白名单时改选
	// 白名单内第一个可用出图模型；白名单内无出图模型则维持默认名落入拒绝
	//（不做隐式放行）。文案与 chat 侧同口径，避免枚举探测。
	if !h.modelAllowed(realm, bare) {
		if !named {
			if fr, fb, ok := h.firstAllowedGenerationImage(); ok {
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
	// models.hidden 同一份黑名单也管出图入口：列表里已经隐藏的模型不该还能直接调。
	// （白名单兜底选出的模型也在此受检：白名单∩黑名单交集语义，隐藏的不例外。）
	if h.modelHidden(realm, bare) {
		writeOpenAIError(w, http.StatusBadRequest, "model_unavailable",
			"model is hidden by gateway configuration, try another model")
		return
	}
	req["model"] = bare
	out, err := json.Marshal(req)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	// 记账：与 chat 同一条管道（请求台账归档由 ServeHTTP 的 isChatEntry 接管，
	// 这里补账号/模型/用量/扣费 → stdout 流水行 + 面板「请求记录」+ 用量与成本账本）。
	st := &chatStat{start: start, model: bare, mode: imageMode(edit), toks: -1, budget: h.budget}
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
	for attempt := 0; attempt < imageAttempts; attempt++ {
		a := h.cfg.Pool.PickExcludingForRealm(tried, bare, realm)
		if a == nil {
			break
		}
		tried[a.UID] = true
		st.uid, st.nick = a.UID, a.Nickname
		st.attempts = attempt + 1

		var data []byte
		if edit {
			data, err = h.cfg.Upstream.ImageEdits(a, json.RawMessage(out))
		} else {
			data, err = h.cfg.Upstream.ImageGenerations(a, json.RawMessage(out))
		}
		if err != nil {
			lastErr = err
			var ue *upstream.Error
			if errors.As(err, &ue) {
				// 与 chat 同一套失败处置（冷却/熔断/模型负缓存/12153 计数）。
				h.applyErrorPolicy(a.UID, ue.Kind, ue.Msg, bare, ue)
				// 请求级/模型级错误与账号无关：审核与参数是请求本身的属性，14401/11102 是
				// 该模型在本域没有路由。换号必然同样失败——实测 14401 会打满全池账号，
				// 白烧上游调用；客户端还会把 502 当瞬时故障反复重试。立即透传原文回 400。
				if requestLevelFailFastKind(ue.Kind) {
					status := ue.Status
					if status < 400 {
						status = http.StatusBadRequest
					}
					st.status, st.outcome = status, "http_error"
					log.Printf("images model=%s realm=%s: 请求级错误 kind=%v，不再换号", bare, realm, ue.Kind)
					writeOpenAIError(w, status, "upstream_error", ue.Msg)
					return
				}
			}
			log.Printf("images %s model=%s attempt=%d: %v", logfmt.Label(a.UID, a.Nickname), bare, attempt+1, err)
			continue
		}
		lastErr = nil
		recordImageUsage(h, st, realm, a.UID, bare, modelRate, data, req, start)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
		return
	}

	status, code := http.StatusBadGateway, "upstream_error"
	var ue *upstream.Error
	switch {
	case len(tried) == 0: // 一个可用账号都没有：没有可重试的对象，明确说清楚
		status, code = http.StatusServiceUnavailable, "no_account"
		lastErr = fmt.Errorf("没有可用的 %s 账号（图像只在 %s 域出图）", realm, realm)
	case errors.As(lastErr, &ue) && ue.Status >= 400:
		status = ue.Status // 上游状态码（如 14401 的 400）比一律 502 更能说明问题
	}
	st.status = status
	st.outcome = ""
	log.Printf("images model=%s realm=%s: 全部尝试失败（%d 个账号）：%v", bare, realm, len(tried), lastErr)
	writeOpenAIError(w, status, code, lastErr.Error())
}

// imageMode 流水行的 mode 列。
func imageMode(edit bool) string {
	if edit {
		return "edit"
	}
	return "image"
}

// recordImageUsage 把上游 data 里的用量落到三个既有的账：流水行（st）、成本账本
// （NoteModelCost）、用量时序（usage.Recorder）。上游 usage 缺失时只记成功、不编数。
//
// 额外记一份**分档实扣**（upstream.RecordImageCharge，尺寸×画质）：两域计费模型不同
// （CN 每张固定 5.71，global 按输出图像 token 计费），混成单一均值会随用户选画质漂移，
// 面板必须能按档显示。尺寸/画质优先取上游回执（global 会回 size/quality），回执缺省
// 时回落请求体同名字段（CN 回执不带，但请求里带了）。
func recordImageUsage(h *Handler, st *chatStat, realm, uid, bare, modelRate string, data []byte, req map[string]any, start time.Time) {
	st.status = http.StatusOK
	st.outcome = "success"
	var env struct {
		Quality string `json:"quality"`
		Size    string `json:"size"`
		Usage   struct {
			InputTokens  int64   `json:"input_tokens"`
			OutputTokens int64   `json:"output_tokens"`
			TotalTokens  int64   `json:"total_tokens"`
			Credit       float64 `json:"credit"`
			ImageCounts  int64   `json:"output_image_counts"`
			ImageTokens  int64   `json:"output_image_tokens"`
			Coefficient  float64 `json:"output_image_coefficient"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(data, &env) // 上游省略 usage 时零值：下面按「有没有真值」决定是否记账
	u := env.Usage
	st.promptTokens, st.completionTokens, st.totalTokens = u.InputTokens, u.OutputTokens, u.TotalTokens
	if u.TotalTokens > 0 {
		st.toks = int(u.TotalTokens)
	}
	if u.Credit > 0 {
		st.credit, st.hasCredit = u.Credit, true
		h.cfg.Pool.NoteModelCost(uid, bare, u.Credit, int(u.TotalTokens))
		if h.cfg.Upstream != nil {
			size := env.Size
			if size == "" {
				size, _ = req["size"].(string)
			}
			quality := env.Quality
			if quality == "" {
				quality, _ = req["quality"].(string)
			}
			h.cfg.Upstream.RecordImageCharge(realm, bare, size, quality, u.Credit, u.ImageTokens, u.Coefficient)
		}
	}
	imgs := u.ImageCounts
	if imgs == 0 {
		imgs = 1
	}
	log.Printf("images %s model=%s: 出图 %d 张 credit=%.2f 用时=%s",
		logfmt.Label(uid, st.nick), bare, imgs, u.Credit, time.Since(start).Round(time.Millisecond))
	if h.cfg.Usage != nil {
		h.cfg.Usage.Add(time.Now(), realm, uid, bare, usage.Delta{
			PromptTokens:    u.InputTokens,
			HasPromptTokens: u.InputTokens > 0,
			TotalTokens:     u.TotalTokens,
			HasTotal:        u.TotalTokens > 0,
			LatencyMs:       time.Since(start).Milliseconds(),
			HasLatency:      true,
			Credit:          u.Credit,
			HasCredit:       u.Credit > 0,
			ModelRate:       modelRate,
		}, u.TotalTokens > 0)
	}
}

// imageRequest 解析请求体：JSON 原样用（只把 image 归一成数组）；multipart
// （OpenAI SDK 的 images.edit）就地转成上游要的 JSON，图片转 data URL。
func imageRequest(r *http.Request, raw []byte, edit bool) (map[string]any, error) {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		if !edit {
			return nil, errors.New("文生图请用 application/json（multipart 只用于图生图）")
		}
		return multipartToImageJSON(raw, ct)
	}
	var req map[string]any
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, errors.New("invalid JSON body")
	}
	if edit {
		// OpenAI 的 images.edit 用 image（单值或数组）；上游要 image 数组。
		imgs, _ := req["image"].([]any)
		if len(imgs) == 0 {
			if one, ok := req["image"].(string); ok && one != "" {
				imgs = []any{one}
			}
		}
		if len(imgs) == 0 {
			return nil, errors.New("图生图需要 image（data URL / URL，或 image 数组）")
		}
		req["image"] = imgs
	}
	return req, nil
}

// multipartToImageJSON multipart 表单 → 上游 JSON：文件字段转 data URL（image[] / mask），
// 其余字段按字符串收（n 转数字）。
func multipartToImageJSON(raw []byte, ct string) (map[string]any, error) {
	br := multipart.NewReader(strings.NewReader(string(raw)), boundaryOf(ct))
	form, err := br.ReadForm(int64(len(raw)) + 1024)
	if err != nil {
		return nil, fmt.Errorf("解析 multipart 失败: %w", err)
	}
	out := map[string]any{}
	for k, vs := range form.Value {
		if len(vs) == 0 {
			continue
		}
		if k == "n" {
			if n, err := strconv.Atoi(vs[0]); err == nil {
				out[k] = n
				continue
			}
		}
		out[k] = vs[0]
	}
	for field, files := range form.File {
		var list []any
		for _, fh := range files {
			du, err := fileToDataURL(fh)
			if err != nil {
				return nil, err
			}
			list = append(list, du)
		}
		switch field {
		case "mask":
			out["mask"] = list[0]
		case "image", "images":
			out["image"] = list
		default:
			out[field] = list
		}
	}
	if _, ok := out["image"]; !ok {
		return nil, errors.New("图生图需要 image 文件")
	}
	return out, nil
}

// fileToDataURL 读一个上传文件并包成 data URL（上游 edits 的入参形态，实测接受）。
func fileToDataURL(fh *multipart.FileHeader) (string, error) {
	f, err := fh.Open()
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 8<<20)) // 8MB 上限：上游对超大 base64 会拒
	if err != nil {
		return "", err
	}
	ct := fh.Header.Get("Content-Type")
	if ct == "" {
		ct = "image/png"
	}
	return "data:" + ct + ";base64," + base64.StdEncoding.EncodeToString(b), nil
}

// boundaryOf 从 Content-Type 里取 multipart boundary。
func boundaryOf(ct string) string {
	for _, part := range strings.Split(ct, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "boundary=") {
			return strings.Trim(part[len("boundary="):], `"`)
		}
	}
	return ""
}
