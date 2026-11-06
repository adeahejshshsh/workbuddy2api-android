// Responses 托管 image_generation：探测让模型自己写提示词 → 网关出图 → 成品作为
// image_generation_call 输出项交付（非流式插 output 头部、流式补 output_item 事件）。
package server

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// imageToolFake 按路径分流：chat 首轮回「模型要出图」，之后回正常答案；
// /v2/images 回上游形状的出图回执。
type imageToolFake struct {
	chatCalls  int
	chatBodies []string
	imgBodies  []string
	imgFail    bool
	cdn        string
}

func (f *imageToolFake) client() *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			if strings.HasPrefix(r.URL.Path, "/v2/images") {
				f.imgBodies = append(f.imgBodies, string(raw))
				if f.imgFail {
					return &http.Response{StatusCode: 400, Header: jsonHdr(),
						Body: io.NopCloser(strings.NewReader(`{"code":14401,"msg":"route config not found"}`))}, nil
				}
				return &http.Response{StatusCode: 200, Header: jsonHdr(),
					Body: io.NopCloser(strings.NewReader(`{"code":0,"msg":"OK","data":{"created":1,` +
						`"data":[{"url":"` + f.cdn + `/img.png"}],"usage":{"credit":5.71}}}`))}, nil
			}
			f.chatCalls++
			f.chatBodies = append(f.chatBodies, string(raw))
			if f.chatCalls == 1 {
				return &http.Response{StatusCode: 200, Header: sseHdr(),
					Body: io.NopCloser(strings.NewReader(imageToolCallStream()))}, nil
			}
			return &http.Response{StatusCode: 200, Header: sseHdr(),
				Body: io.NopCloser(strings.NewReader(sseOK))}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

// imageToolCallStream 探测轮：模型发起 image_generation(prompt=中秋电梯广告图)。
func imageToolCallStream() string {
	return `data: {"id":"cm1","object":"chat.completion.chunk","created":1,"model":"glm-5.2",` +
		`"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1",` +
		`"type":"function","function":{"name":"image_generation","arguments":"{\"prompt\":\"中秋电梯广告图\"}"}}]}}]}` + "\n\n" +
		`data: {"id":"cm1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"
}

func newImageToolHandler(f *imageToolFake) *Handler {
	p := testPoolWith(&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	return NewHandler(Config{Pool: p, Upstream: f.client()})
}

func imageToolCDN(t *testing.T) (string, []byte) {
	t.Helper()
	png := []byte("\x89PNG\r\n\x1a\n-fake-png")
	allowMediaLoopback(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, png
}

// TestResponsesImageGenerationToolNonStream 声明托管 image_generation：
// 一次探测（模型写提示词）→ 网关出图 → 成品作为 image_generation_call 出现在 output 首位。
func TestResponsesImageGenerationToolNonStream(t *testing.T) {
	cdn, png := imageToolCDN(t)
	f := &imageToolFake{cdn: cdn}
	h := newImageToolHandler(f)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","input":"画一张中秋电梯广告图","stream":false,`+
			`"tools":[{"type":"image_generation"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Output []map[string]any `json:"output"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("非 JSON 响应：%s", rec.Body.String())
	}
	if len(resp.Output) == 0 || resp.Output[0]["type"] != "image_generation_call" {
		t.Fatalf("output 首位应是 image_generation_call：%v", resp.Output)
	}
	item := resp.Output[0]
	if item["status"] != "completed" || item["revised_prompt"] != "中秋电梯广告图" {
		t.Fatalf("输出项形状不对：%v", item)
	}
	if raw, err := base64.StdEncoding.DecodeString(asString(item["result"])); err != nil || string(raw) != string(png) {
		t.Fatalf("result 不是成品图片的 base64：%v", err)
	}
	// 探测轮带的是 function 形态的 image_generation（模型才可能调用），出站模型名去前缀。
	var probe map[string]any
	_ = decodeJSON(f.chatBodies[0], &probe)
	if !bodyHasTool(probe, "image_generation") || probe["model"] != "glm-5.2" {
		t.Fatalf("探测轮模型/工具不对：model=%v tools=%v", probe["model"], probe["tools"])
	}
	// 最终调用不再带托管工具声明，且注入了「图已交付」。
	var final map[string]any
	_ = decodeJSON(f.chatBodies[len(f.chatBodies)-1], &final)
	if bodyHasTool(final, "image_generation") {
		t.Fatalf("最终调用仍带 image_generation：%v", final["tools"])
	}
	if last := lastMessage(final); !strings.Contains(last, "出图结果") || !strings.Contains(last, "中秋电梯广告图") {
		t.Fatalf("最终调用未注入出图结果：%v", last)
	}
	// 出图请求：裸模型名 + 模型自己写的提示词。
	if len(f.imgBodies) != 1 || !strings.Contains(f.imgBodies[0], `"prompt":"中秋电梯广告图"`) ||
		!strings.Contains(f.imgBodies[0], `"model":"hunyuan-image-alpha"`) {
		t.Fatalf("出图请求不对：%v", f.imgBodies)
	}
}

// TestResponsesImageGenerationToolStream 流式：图片以 output_item.added/done 补发，
// 且 response.completed 的 output 里也带上它。
func TestResponsesImageGenerationToolStream(t *testing.T) {
	cdn, _ := imageToolCDN(t)
	f := &imageToolFake{cdn: cdn}
	h := newImageToolHandler(f)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","input":"画一张中秋电梯广告图","stream":true,`+
			`"tools":[{"type":"image_generation"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var done map[string]any
	var completed map[string]any
	for _, block := range strings.Split(rec.Body.String(), "\n\n") {
		typ, data := sseType(block)
		var obj map[string]any
		if json.Unmarshal([]byte(data), &obj) != nil {
			continue
		}
		switch typ {
		case "response.output_item.done":
			if it, _ := obj["item"].(map[string]any); asString(it["type"]) == "image_generation_call" {
				done = it
			}
		case "response.completed":
			completed, _ = obj["response"].(map[string]any)
		}
	}
	if done == nil || done["status"] != "completed" || asString(done["result"]) == "" {
		t.Fatalf("流里没有完整的 image_generation_call：%v", done)
	}
	if completed == nil {
		t.Fatal("没有 response.completed")
	}
	output, _ := completed["output"].([]any)
	if len(output) == 0 {
		t.Fatalf("completed 的 output 为空：%v", completed)
	}
	if first, _ := output[0].(map[string]any); asString(first["type"]) != "image_generation_call" {
		t.Fatalf("completed 的 output 首位应是图片：%v", output)
	}
}

// TestResponsesImageGenerationToolFailsSoft 出图失败不毁整轮：照常回到文本回答，
// 但注入的是「出图失败」，模型不能假装已生成。
func TestResponsesImageGenerationToolFailsSoft(t *testing.T) {
	f := &imageToolFake{cdn: "https://127.0.0.1:1", imgFail: true}
	h := newImageToolHandler(f)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","input":"画一张中秋电梯广告图","stream":false,`+
			`"tools":[{"type":"image_generation"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "image_generation_call") {
		t.Fatalf("出图失败不该给图片输出项：%s", rec.Body.String())
	}
	var final map[string]any
	_ = decodeJSON(f.chatBodies[len(f.chatBodies)-1], &final)
	if last := lastMessage(final); !strings.Contains(last, "出图接口不可用") {
		t.Fatalf("失败要如实注入：%v", last)
	}
}

// sseType 取一帧 SSE 的 event 名与 data 行（只服务于本文件的断言）。
func sseType(block string) (string, string) {
	var typ, data string
	for _, line := range strings.Split(block, "\n") {
		if v, ok := strings.CutPrefix(line, "event: "); ok {
			typ = v
		}
		if v, ok := strings.CutPrefix(line, "data: "); ok {
			data = v
		}
	}
	return typ, data
}
