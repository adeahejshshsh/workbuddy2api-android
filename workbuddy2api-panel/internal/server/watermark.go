// watermark.go 水印裁切：POST /v1/images/strip-watermark（+ MCP 工具 strip_watermark）。
//
// 为什么单独开一个端点，而不是在生成时顺手裁：本网关对生成结果**原样透传**
// （只转 URL，不落字节）。要裁就得把图拉回来、解码、裁、再把字节回传——这是「后处理」，
// 与生成是两件事，只对想要彻底去水印的调用方有意义。因此做成开关 + 独立入口：
// features.watermark_strip_enabled 默认 false，关着时端点回 403（不是 404：让调用方
// 看得出功能存在、只是没开，与 /v1/embeddings 同一套口径）。
//
// 能力边界（刻意如此）：
//   - 只裁「底部整条纯白留白」，不做 inpainting、不擦水印像素。白留白里水印本来就
//     像素级不可见，裁掉留白是为了让成品按内容边缘收口（也顺手让水印物理消失）。
//   - 不保证模型一定留白：实测竖版 1024x1536 时杯子/猫能给 31~38% 纯白，深色桌面那
//     张只给了 1%。留白不足时本端点回 422 并给出实测数字，正确做法是先让 edit_image
//     把下方约 1/3 涂白（一次约 5.7 积分），再调一次本端点。
//   - 回读 URL 只允许上游产物域（见 watermarkHostSuffixes）：服务端主动发请求的地方
//     必须收口，否则等于给网关开一个任意 SSRF 入口。本地文件走 multipart 上传。
package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/media"
)

const (
	// watermarkMaxBytes 单次处理的图片字节上限（32 MiB：1792x1344 的 PNG 约 2~3MB，
	// 留足余量又不至于被大图打爆内存）。
	watermarkMaxBytes = 32 << 20
	// watermarkDefaultMinBlank 与 media.DefaultMinBlank 对齐，供文档/回执复用。
	watermarkDefaultMinBlank = media.DefaultMinBlank
)

// watermarkHostSuffixes 允许服务端回读的图片主机后缀（小写比较，含前导点）。
//
// 只收上游产物域（腾讯云 COS）。放开任意主机等于给网关加一个 SSRF 入口——这个端点
// 是「裁掉我们刚给你的那张图的水印」，URL 必然来自本网关自己的回执，不需要外网自由。
// 自己的图片可以用 data URL 或 multipart 上传。
var watermarkHostSuffixes = []string{".myqcloud.com"}

// watermarkHTTPClient 回读图片用的客户端（超时兜底，不复用 upstream 的指纹传输层：
// 那是给 api.codebuddy 的，指向 COS 只会把不该带的头带出去）。
var watermarkHTTPClient = &http.Client{
	Timeout: 60 * time.Second,
	// 不跟随重定向：白名单只校验首跳主机，跟随一次就能被 302 带去任意地址（SSRF）。
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// watermarkRequest 端点请求体（JSON；multipart 走同名表单字段）。
type watermarkRequest struct {
	// Image 图片：上游回执里的 URL、data URL（data:image/png;base64,...）或裸 base64。
	Image string `json:"image"`
	// MinBlank 判定「够裁」的最小留白高度；<=0 用默认 90（水印最高约 80px + 边距）。
	MinBlank int `json:"min_blank_px"`
	// KeepBlank 裁完在内容下方保留的留白像素；默认 0（贴着内容边裁）。
	KeepBlank int `json:"keep_blank"`
	// Format 输出格式 png/jpeg；空 = 跟随输入。
	Format string `json:"format"`
	// Quality JPEG 质量 1-100；默认 92。
	Quality int `json:"quality"`
	// Return "data_url"（默认，回图像字节）或 "meta"（只回数字，不回字节）。
	Return string `json:"return"`
}

// watermarkStrip 裁掉图片底部的水印留白。
func (h *Handler) watermarkStrip(w http.ResponseWriter, r *http.Request) {
	if !h.loadLive().WatermarkStripEnabled {
		writeOpenAIErrorHint(w, http.StatusForbidden, "watermark_strip_disabled",
			"水印裁切端点未开启：请在面板配置里打开 features.watermark_strip_enabled（默认关闭）",
			"开启后本端点与 MCP 工具 strip_watermark 一起出现；关闭时不影响任何生成接口。")
		return
	}

	req, err := parseWatermarkRequest(r)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if strings.TrimSpace(req.Image) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			"image is required（上游回执里的 URL、data URL 或裸 base64；本地文件用 multipart 的 image 字段）")
		return
	}

	raw, err := h.loadWatermarkImage(r, req.Image)
	if err != nil {
		var tooBig *watermarkTooLargeError
		if errors.As(err, &tooBig) {
			writeOpenAIError(w, http.StatusRequestEntityTooLarge, "invalid_request", err.Error())
			return
		}
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	res, err := media.Strip(raw, media.Options{
		MinBlank:  req.MinBlank,
		KeepBlank: req.KeepBlank,
		Format:    req.Format,
		Quality:   req.Quality,
	})
	if err != nil {
		var nb *media.NoBlankError
		switch {
		case errors.As(err, &nb):
			writeOpenAIErrorHint(w, http.StatusUnprocessableEntity, "no_blank_strip", err.Error(),
				fmt.Sprintf("先让 edit_image 把画面下方约 1/3 涂成纯白（提示词示例：把图片最下方约三分之一区域涂成纯白色空白留白，不要改变其余画面），再调本端点；本次回执：%s",
					watermarkNumbers(nb.Result)))
		case errors.Is(err, media.ErrTooLarge):
			writeOpenAIError(w, http.StatusRequestEntityTooLarge, "invalid_image", err.Error())
		case errors.Is(err, media.ErrNotImage), errors.Is(err, media.ErrUniform):
			writeOpenAIError(w, http.StatusBadRequest, "invalid_image", err.Error())
		default:
			writeOpenAIError(w, http.StatusInternalServerError, "strip_failed", err.Error())
		}
		return
	}

	body := map[string]any{
		"object":      "image.strip",
		"width":       res.Width,
		"height":      res.Height,
		"out_width":   res.OutWidth,
		"out_height":  res.OutHeight,
		"boundary":    res.Boundary,
		"blank_px":    res.BlankPx,
		"blank_ratio": round4(res.BlankRatio),
		"blank_lum":   round1(res.BlankLum),
		"min_blank":   res.MinBlank,
		"white_lum":   round1(res.WhiteLum),
		"safe":        res.Safe,
		"format":      res.Format,
		"bytes_in":    len(raw),
		"bytes_out":   len(res.Data),
		"usage": map[string]any{
			// 本端点只做本地解码/裁剪，不打上游、不扣积分：显式回 0，省得调用方对账时瞎猜。
			"credit": 0,
			"hint":   "本地后处理，不消耗上游积分",
		},
	}
	if strings.EqualFold(strings.TrimSpace(req.Return), "meta") {
		writeJSON(w, http.StatusOK, body)
		return
	}
	body["data_url"] = dataURL(res.Format, res.Data)
	writeJSON(w, http.StatusOK, body)
}

// parseWatermarkRequest 解析 JSON 或 multipart 请求。
func parseWatermarkRequest(r *http.Request) (watermarkRequest, error) {
	ct := r.Header.Get("Content-Type")
	if mt, _, _ := mime.ParseMediaType(ct); mt == "multipart/form-data" {
		return parseWatermarkMultipart(r)
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, watermarkMaxBytes+1))
	if err != nil {
		return watermarkRequest{}, fmt.Errorf("read body: %v", err)
	}
	if len(raw) > watermarkMaxBytes {
		return watermarkRequest{}, &watermarkTooLargeError{limit: watermarkMaxBytes}
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return watermarkRequest{}, errors.New("请求体为空：需要 {\"image\": \"<url|data url|base64>\"}")
	}
	var req watermarkRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return watermarkRequest{}, fmt.Errorf("invalid json: %v", err)
	}
	return req, nil
}

// parseWatermarkMultipart 走 multipart：image 是文件字段（不落盘、直接读进内存），
// 其余同名表单字段当参数。给「本机有文件、手上没有公网 URL」的场景用。
func parseWatermarkMultipart(r *http.Request) (watermarkRequest, error) {
	if err := r.ParseMultipartForm(watermarkMaxBytes); err != nil {
		return watermarkRequest{}, fmt.Errorf("parse multipart: %v", err)
	}
	req := watermarkRequest{
		MinBlank:  formInt(r, "min_blank_px"),
		KeepBlank: formInt(r, "keep_blank"),
		Quality:   formInt(r, "quality"),
		Format:    r.FormValue("format"),
		Return:    r.FormValue("return"),
	}
	file, _, err := r.FormFile("image")
	if err == nil {
		defer file.Close()
		raw, err := io.ReadAll(io.LimitReader(file, watermarkMaxBytes+1))
		if err != nil {
			return watermarkRequest{}, fmt.Errorf("read image: %v", err)
		}
		if len(raw) > watermarkMaxBytes {
			return watermarkRequest{}, &watermarkTooLargeError{limit: watermarkMaxBytes}
		}
		req.Image = dataURL(detectImageFormat(raw), raw)
		return req, nil
	}
	// 没有文件字段就退回表单里的 image 字符串（URL / data URL）。
	req.Image = r.FormValue("image")
	if strings.TrimSpace(req.Image) == "" {
		return watermarkRequest{}, errors.New("multipart 里没有 image 字段（文件或 URL/data URL 字符串都行）")
	}
	return req, nil
}

func formInt(r *http.Request, key string) int {
	v := strings.TrimSpace(r.FormValue(key))
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
}

// watermarkTooLargeError 图片超过字节上限。
type watermarkTooLargeError struct{ limit int }

func (e *watermarkTooLargeError) Error() string {
	return fmt.Sprintf("图片超过 %d MiB 上限：先用 edit_image 缩放，或自己裁好再传", e.limit>>20)
}

// loadWatermarkImage 把 image 字段解析成图片字节：data URL / 裸 base64 / 允许域内的 URL。
func (h *Handler) loadWatermarkImage(r *http.Request, spec string) ([]byte, error) {
	spec = strings.TrimSpace(spec)
	lower := strings.ToLower(spec)
	switch {
	case strings.HasPrefix(lower, "data:"):
		return decodeDataURL(spec)
	case strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "https://"):
		return fetchWatermarkImage(r, spec)
	}
	// 其余一律当裸 base64（客户端已经把图读进内存时省掉 data: 前缀）。
	cleaned := strings.Map(func(c rune) rune {
		if c == '\n' || c == '\r' || c == ' ' || c == '\t' {
			return -1
		}
		return c
	}, spec)
	raw, err := base64.StdEncoding.DecodeString(cleaned)
	if err != nil {
		if raw, err2 := base64.RawStdEncoding.DecodeString(cleaned); err2 == nil {
			return raw, nil
		}
		return nil, fmt.Errorf("image 既不是 URL、也不是合法 base64：%v", err)
	}
	return raw, nil
}

// fetchWatermarkImage 回读上游产物图（域名白名单 + 字节上限）。
func fetchWatermarkImage(r *http.Request, spec string) ([]byte, error) {
	u, err := url.Parse(spec)
	if err != nil {
		return nil, fmt.Errorf("image URL 解析失败：%v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("image URL 只支持 http/https")
	}
	if !watermarkHostAllowed(u.Hostname()) {
		return nil, fmt.Errorf("image URL 主机不受支持：%s。只接受上游产物域（%s），自己的图片请用 data URL 或 multipart 上传",
			u.Hostname(), strings.Join(watermarkHostSuffixes, " / "))
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, spec, nil)
	if err != nil {
		return nil, fmt.Errorf("构造回读请求失败：%v", err)
	}
	resp, err := watermarkHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("回读图片失败：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("回读图片失败：HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > watermarkMaxBytes {
		return nil, &watermarkTooLargeError{limit: watermarkMaxBytes}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, watermarkMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("回读图片失败：%v", err)
	}
	if len(raw) > watermarkMaxBytes {
		return nil, &watermarkTooLargeError{limit: watermarkMaxBytes}
	}
	return raw, nil
}

// watermarkHostAllowed 主机是否在允许回读的后缀表里（大小写不敏感，逐条后缀匹配）。
func watermarkHostAllowed(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	for _, suffix := range watermarkHostSuffixes {
		suffix = strings.ToLower(suffix)
		if host == strings.TrimPrefix(suffix, ".") || strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// decodeDataURL 解 data:image/...;base64,xxx。
func decodeDataURL(spec string) ([]byte, error) {
	comma := strings.IndexByte(spec, ',')
	if comma < 0 {
		return nil, errors.New("data URL 缺少逗号分隔：应为 data:image/png;base64,<数据>")
	}
	meta := strings.ToLower(spec[:comma])
	if !strings.Contains(meta, ";base64") {
		return nil, errors.New("data URL 只支持 base64 编码（data:image/png;base64,...）")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.Map(func(c rune) rune {
		if c == '\n' || c == '\r' || c == ' ' || c == '\t' {
			return -1
		}
		return c
	}, spec[comma+1:]))
	if err != nil {
		return nil, fmt.Errorf("data URL base64 解码失败：%v", err)
	}
	return raw, nil
}

// dataURL 反向拼一个 data URL（回执用）。
func dataURL(format string, raw []byte) string {
	mimeType := "image/png"
	if format == "jpeg" {
		mimeType = "image/jpeg"
	}
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(raw)
}

// detectImageFormat 只按魔数认 png/jpeg（multipart 上传时用来拼 data URL，不猜别的）。
func detectImageFormat(raw []byte) string {
	if len(raw) >= 8 && bytes.Equal(raw[:8], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}) {
		return "png"
	}
	if len(raw) >= 3 && raw[0] == 0xFF && raw[1] == 0xD8 && raw[2] == 0xFF {
		return "jpeg"
	}
	return "png"
}

// watermarkNumbers 把实测数字拼成一句话，用于错误提示（调用方据此决定重画还是补白）。
func watermarkNumbers(res media.Result) string {
	return fmt.Sprintf("原图 %dx%d，底部留白 %dpx（占 %.1f%%），留白平均亮度 %.1f",
		res.Width, res.Height, res.BlankPx, res.BlankRatio*100, res.BlankLum)
}

func round1(v float64) float64 { return float64(int64(v*10+0.5)) / 10 }
func round4(v float64) float64 { return float64(int64(v*10000+0.5)) / 10000 }

// mcpStripWatermark 把裁切端点做成 MCP 工具。
//
// 与 mcpProxyCall 的差别只有一处：图片按 MCP 规范的 image content 回（客户端直接
// 渲染），数字走 structuredContent——同一份 base64 不在 JSON 里再传一遍。
func (h *Handler) mcpStripWatermark(ctx context.Context, args map[string]any) (any, *mcpRPCError) {
	if !h.loadLive().WatermarkStripEnabled {
		return mcpToolError("水印裁切未开启：网关配置里的 features.watermark_strip_enabled 仍是 false（默认关闭），这条工具不可用。"), nil
	}
	if v, ok := args["image"].(string); !ok || strings.TrimSpace(v) == "" {
		return nil, &mcpRPCError{Code: -32602, Message: "Invalid params: image is required"}
	}
	// 强制回字节：MCP 侧要把字节塞进 image content。
	call := make(map[string]any, len(args)+1)
	for k, v := range args {
		call[k] = v
	}
	call["return"] = "data_url"
	body, err := json.Marshal(call)
	if err != nil {
		return nil, &mcpRPCError{Code: -32602, Message: "Invalid params: " + err.Error()}
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/images/strip-watermark", strings.NewReader(string(body))).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.watermarkStrip(rec, req)
	data := rec.Body.Bytes()
	if rec.Code >= 400 {
		return mcpToolError(strings.TrimSpace(string(data))), nil
	}

	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return mcpToolError("裁切回执解析失败：" + err.Error()), nil
	}
	url, _ := payload["data_url"].(string)
	delete(payload, "data_url")
	mimeType := "image/png"
	if f, _ := payload["format"].(string); f == "jpeg" {
		mimeType = "image/jpeg"
	}
	b64 := ""
	if i := strings.IndexByte(url, ','); i >= 0 {
		b64 = url[i+1:]
	}
	return map[string]any{
		"content": []map[string]any{
			{"type": "image", "data": b64, "mimeType": mimeType},
			{"type": "text", "text": prettyJSON(payload)},
		},
		"structuredContent": payload,
		"isError":           false,
	}, nil
}

// mcpToolError 工具业务失败的统一回执（给模型看原因，不是 JSON-RPC 错误）。
func mcpToolError(msg string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": msg}},
		"isError": true,
	}
}
