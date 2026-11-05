package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// wmTestPNG 造一张「上半棋盘内容 + 下方纯白留白」的 PNG（w 宽、h 高、contentH 行内容）。
func wmTestPNG(t *testing.T, w, h, contentH int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if y < contentH {
				if (x+y)%2 == 0 {
					img.SetRGBA(x, y, color.RGBA{R: 12, G: 24, B: 36, A: 255})
				} else {
					img.SetRGBA(x, y, color.RGBA{R: 240, G: 236, B: 232, A: 255})
				}
				continue
			}
			img.SetRGBA(x, y, color.RGBA{R: 255, G: 255, B: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

// wmNoBlankPNG 造一张满幅图案（底部没有留白）的 PNG。
func wmNoBlankPNG(t *testing.T) []byte {
	return wmTestPNG(t, 40, 120, 120)
}

func wmDataURL(raw []byte) string {
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)
}

func postWatermark(t *testing.T, h *Handler, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/images/strip-watermark", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec, out
}

func wmErrorCode(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

func wmErrorHint(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	hint, _ := e["gateway_hint"].(string)
	return hint
}

func TestWatermarkStripDisabledByDefault(t *testing.T) {
	h := NewHandler(Config{})
	rec, out := postWatermark(t, h, `{"image":"`+wmDataURL(wmTestPNG(t, 40, 120, 80))+`","min_blank_px":20}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("默认关闭应回 403，实际 %d body=%s", rec.Code, rec.Body)
	}
	if code := wmErrorCode(out); code != "watermark_strip_disabled" {
		t.Fatalf("错误码应为 watermark_strip_disabled，实际 %q", code)
	}
	if hint := wmErrorHint(out); !strings.Contains(hint, "strip_watermark") {
		t.Fatalf("应给出开启说明，实际 gateway_hint=%q", hint)
	}
}

func TestWatermarkStripDataURL(t *testing.T) {
	h := NewHandler(Config{WatermarkStripEnabled: true})
	img := wmTestPNG(t, 40, 120, 80)
	rec, out := postWatermark(t, h, `{"image":"`+wmDataURL(img)+`","min_blank_px":20}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("应回 200，实际 %d body=%s", rec.Code, rec.Body)
	}
	if out["object"] != "image.strip" {
		t.Fatalf("object=%v", out["object"])
	}
	if out["out_height"] != float64(80) || out["blank_px"] != float64(40) || out["boundary"] != float64(80) {
		t.Fatalf("裁切数字不对：out_height=%v blank_px=%v boundary=%v", out["out_height"], out["blank_px"], out["boundary"])
	}
	if out["safe"] != true {
		t.Fatalf("safe=%v", out["safe"])
	}
	du, _ := out["data_url"].(string)
	if !strings.HasPrefix(du, "data:image/png;base64,") {
		t.Fatalf("data_url 前缀不对：%.40s", du)
	}
	usage, _ := out["usage"].(map[string]any)
	if usage["credit"] != float64(0) {
		t.Fatalf("本地后处理不该扣积分：%v", usage["credit"])
	}
}

func TestWatermarkStripMetaOnly(t *testing.T) {
	h := NewHandler(Config{WatermarkStripEnabled: true})
	rec, out := postWatermark(t, h, `{"image":"`+wmDataURL(wmTestPNG(t, 40, 120, 80))+`","min_blank_px":20,"return":"meta"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("应回 200，实际 %d", rec.Code)
	}
	if _, ok := out["data_url"]; ok {
		t.Fatal("return=meta 时不该回图字节")
	}
	if out["blank_ratio"] == nil || out["blank_lum"] == nil {
		t.Fatalf("meta 回执仍要带实测数字：%v", out)
	}
}

func TestWatermarkStripNoBlankReturns422(t *testing.T) {
	h := NewHandler(Config{WatermarkStripEnabled: true})
	rec, out := postWatermark(t, h, `{"image":"`+wmDataURL(wmNoBlankPNG(t))+`","min_blank_px":20}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("留白不足应回 422，实际 %d body=%s", rec.Code, rec.Body)
	}
	if code := wmErrorCode(out); code != "no_blank_strip" {
		t.Fatalf("错误码应为 no_blank_strip，实际 %q", code)
	}
	hint := wmErrorHint(out)
	if !strings.Contains(hint, "edit_image") {
		t.Fatalf("应提示先用 edit_image 涂白，实际 gateway_hint=%q", hint)
	}
	if !strings.Contains(hint, "原图 40x120") {
		t.Fatalf("应带实测数字，实际 gateway_hint=%q", hint)
	}
}

func TestWatermarkStripMissingImage(t *testing.T) {
	h := NewHandler(Config{WatermarkStripEnabled: true})
	rec, out := postWatermark(t, h, `{"min_blank_px":20}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 image 应回 400，实际 %d", rec.Code)
	}
	if code := wmErrorCode(out); code != "invalid_request" {
		t.Fatalf("错误码应为 invalid_request，实际 %q", code)
	}
}

func TestWatermarkStripRejectsForeignHost(t *testing.T) {
	h := NewHandler(Config{WatermarkStripEnabled: true})
	rec, out := postWatermark(t, h, `{"image":"https://evil.example/x.png","min_blank_px":20}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非上游产物域应回 400，实际 %d body=%s", rec.Code, rec.Body)
	}
	if code := wmErrorCode(out); code != "invalid_request" {
		t.Fatalf("错误码应为 invalid_request，实际 %q", code)
	}
}

func TestWatermarkStripFetchAllowedHost(t *testing.T) {
	img := wmTestPNG(t, 40, 120, 80)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(img)
	}))
	defer srv.Close()

	old := watermarkHostSuffixes
	// 白名单按 Hostname（不含端口）匹配，所以这里给主机名而不是 host:port。
	watermarkHostSuffixes = []string{"127.0.0.1"}
	defer func() { watermarkHostSuffixes = old }()

	h := NewHandler(Config{WatermarkStripEnabled: true})
	rec, out := postWatermark(t, h, `{"image":"`+srv.URL+`/a.png","min_blank_px":20}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("允许的主机应回 200，实际 %d body=%s", rec.Code, rec.Body)
	}
	if out["out_height"] != float64(80) {
		t.Fatalf("out_height=%v", out["out_height"])
	}
}

// 白名单只校验首跳主机：跟随 302 就等于让允许域把请求带去任意地址（SSRF），
// 所以回读客户端必须不跟随重定向。
func TestWatermarkStripDoesNotFollowRedirect(t *testing.T) {
	img := wmTestPNG(t, 40, 120, 80)
	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(img)
	}))
	defer inner.Close()
	outer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, inner.URL+"/x.png", http.StatusFound)
	}))
	defer outer.Close()

	old := watermarkHostSuffixes
	watermarkHostSuffixes = []string{"127.0.0.1"}
	defer func() { watermarkHostSuffixes = old }()

	h := NewHandler(Config{WatermarkStripEnabled: true})
	rec, out := postWatermark(t, h, `{"image":"`+outer.URL+`/a.png"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("重定向应被拒绝（400），实际 %d body=%s", rec.Code, rec.Body)
	}
	if msg, _ := out["error"].(map[string]any)["message"].(string); !strings.Contains(msg, "302") {
		t.Fatalf("错误应点明 302，实际 %q", msg)
	}
}

func TestWatermarkStripMultipart(t *testing.T) {
	img := wmTestPNG(t, 40, 120, 80)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("image", "a.png")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write(img); err != nil {
		t.Fatalf("写 multipart 文件: %v", err)
	}
	if err := mw.WriteField("min_blank_px", "20"); err != nil {
		t.Fatalf("WriteField: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("mw.Close: %v", err)
	}

	h := NewHandler(Config{WatermarkStripEnabled: true})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/images/strip-watermark", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("multipart 应回 200，实际 %d body=%s", rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("回执不是 JSON：%v", err)
	}
	if out["out_height"] != float64(80) {
		t.Fatalf("out_height=%v", out["out_height"])
	}
}

func TestWatermarkStripNotImage(t *testing.T) {
	h := NewHandler(Config{WatermarkStripEnabled: true})
	bad := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("这不是图片"))
	rec, out := postWatermark(t, h, `{"image":"`+bad+`","min_blank_px":20}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非图片应回 400，实际 %d body=%s", rec.Code, rec.Body)
	}
	if code := wmErrorCode(out); code != "invalid_image" {
		t.Fatalf("错误码应为 invalid_image，实际 %q", code)
	}
}

// TestMCPStripWatermarkGated 水印工具随开关出现：关着时 5 个工具，开着时多一个 strip_watermark。
func TestMCPStripWatermarkGated(t *testing.T) {
	off := NewHandler(Config{Pool: imagePool(), Upstream: (&imageFake{status: 200, body: imageOK}).client()})
	_, out := mcpCall(t, off, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	result, _ := out["result"].(map[string]any)
	if names := mcpToolNames(t, result); names["strip_watermark"] != nil {
		t.Fatal("默认关闭时 strip_watermark 不该出现在 tools/list")
	}

	on := NewHandler(Config{
		Pool:                  imagePool(),
		Upstream:              (&imageFake{status: 200, body: imageOK}).client(),
		WatermarkStripEnabled: true,
	})
	_, out = mcpCall(t, on, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	result, _ = out["result"].(map[string]any)
	tool := mcpToolNames(t, result)["strip_watermark"]
	if tool == nil {
		t.Fatal("开启后 strip_watermark 应出现在 tools/list")
	}
	schema, _ := tool["inputSchema"].(map[string]any)
	required, _ := schema["required"].([]any)
	if len(required) != 1 || required[0] != "image" {
		t.Fatalf("strip_watermark.required 应为 [image]：%v", required)
	}
}

// TestMCPStripWatermarkCall 工具调用回 MCP image content + 文本元数据。
func TestMCPStripWatermarkCall(t *testing.T) {
	h := NewHandler(Config{
		Pool:                  imagePool(),
		Upstream:              (&imageFake{status: 200, body: imageOK}).client(),
		WatermarkStripEnabled: true,
	})
	args, err := json.Marshal(map[string]any{
		"image":        wmDataURL(wmTestPNG(t, 40, 120, 80)),
		"min_blank_px": 20,
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	body := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"strip_watermark","arguments":` + string(args) + `}}`
	rec, out := mcpCall(t, h, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	result, _ := out["result"].(map[string]any)
	if result == nil {
		t.Fatalf("缺 result：%v", out)
	}
	if result["isError"] == true {
		t.Fatalf("不该是工具错误：%v", result)
	}
	content, _ := result["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("应回 image + text 两段内容，实际 %d", len(content))
	}
	first, _ := content[0].(map[string]any)
	if first["type"] != "image" || first["mimeType"] != "image/png" {
		t.Fatalf("第一段应是 PNG image content：%v", first)
	}
	if data, _ := first["data"].(string); len(data) < 100 {
		t.Fatalf("image content 的 base64 太短：%d", len(data))
	}
	structured, _ := result["structuredContent"].(map[string]any)
	if structured["out_height"] != float64(80) {
		t.Fatalf("structuredContent.out_height=%v", structured["out_height"])
	}
	if _, ok := structured["data_url"]; ok {
		t.Fatal("structuredContent 不该再带 data_url（字节已经进了 image content）")
	}
}
