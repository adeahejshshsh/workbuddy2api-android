package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// mcpCall 发一条 JSON-RPC 消息（走完整的 Handler：路由 + 鉴权 + 分发）。
func mcpCall(t *testing.T, h *Handler, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/mcp", strings.NewReader(body)))
	var out map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec, out
}

// mcpToolNames 从 tools/list 结果里取工具名集合。
func mcpToolNames(t *testing.T, result map[string]any) map[string]map[string]any {
	t.Helper()
	list, _ := result["tools"].([]any)
	names := map[string]map[string]any{}
	for _, raw := range list {
		tool, _ := raw.(map[string]any)
		name, _ := tool["name"].(string)
		names[name] = tool
	}
	return names
}

// TestMCPInitializeAndToolsList 握手 + 工具面：协议版本回显、能力声明、5 个工具齐全。
func TestMCPInitializeAndToolsList(t *testing.T) {
	h := NewHandler(Config{Pool: imagePool(), Upstream: (&imageFake{status: 200, body: imageOK}).client()})

	rec, out := mcpCall(t, h, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":`+
		`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`)
	if rec.Code != 200 {
		t.Fatalf("initialize code=%d body=%s", rec.Code, rec.Body)
	}
	if out["jsonrpc"] != "2.0" || out["id"] != float64(1) {
		t.Fatalf("回包信封不对：%v", out)
	}
	result, _ := out["result"].(map[string]any)
	if result["protocolVersion"] != "2025-06-18" {
		t.Fatalf("协议版本应回显客户端请求的：%v", result["protocolVersion"])
	}
	caps, _ := result["capabilities"].(map[string]any)
	if _, ok := caps["tools"]; !ok {
		t.Fatalf("必须声明 tools 能力：%v", caps)
	}
	info, _ := result["serverInfo"].(map[string]any)
	if info["name"] != "workbuddy2api" {
		t.Fatalf("serverInfo.name=%v", info["name"])
	}
	if s, _ := result["instructions"].(string); !strings.Contains(s, "generate_video") {
		t.Fatalf("instructions 应说明怎么用：%v", result["instructions"])
	}

	_, out = mcpCall(t, h, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	res, _ := out["result"].(map[string]any)
	names := mcpToolNames(t, res)
	for _, want := range []string{"generate_image", "edit_image", "generate_video", "get_video_task", "list_generation_models"} {
		if _, ok := names[want]; !ok {
			t.Fatalf("缺工具 %s（现有 %v）", want, names)
		}
	}
	// 生图的 inputSchema 必须把 prompt 标成 required：模型据此知道不能省。
	schema, _ := names["generate_image"]["inputSchema"].(map[string]any)
	req, _ := schema["required"].([]any)
	if len(req) != 1 || req[0] != "prompt" {
		t.Fatalf("generate_image.required=%v want [prompt]", schema["required"])
	}
}

// TestMCPProtocolVersionFallback 客户端报未支持的版本：回我们最新支持的版本，不报错。
func TestMCPProtocolVersionFallback(t *testing.T) {
	h := NewHandler(Config{Pool: imagePool()})
	_, out := mcpCall(t, h, `{"jsonrpc":"2.0","id":"a","method":"initialize","params":{"protocolVersion":"2099-01-01"}}`)
	result, _ := out["result"].(map[string]any)
	if result["protocolVersion"] != "2025-06-18" {
		t.Fatalf("未支持的版本应回落到 2025-06-18，got %v", result["protocolVersion"])
	}
}

// TestMCPNotificationsAccepted 通知（无 id）按规范只回 202、不回包。
//
// JSON-RPC 2.0：没有 id 的请求是通知，服务端一律不回应——不管方法认不认识，也不管是不是
// notifications/ 开头（客户端发来新版规范里的通知、或不带 id 的调用，都不能回一个 id:null 的错误）。
func TestMCPNotificationsAccepted(t *testing.T) {
	h := NewHandler(Config{Pool: imagePool()})
	for _, body := range []string{
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info"}}`,
		`{"jsonrpc":"2.0","method":"ping"}`,
		`{"jsonrpc":"2.0","method":"no/such/method","id":null}`,
	} {
		rec, _ := mcpCall(t, h, body)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("通知 %s code=%d want 202", body, rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Fatalf("通知 %s 不该有 body：%s", body, rec.Body)
		}
	}
}

// TestMCPMethodAndToolErrors 未知方法 -32601、未知工具 -32602、坏 JSON -32700。
func TestMCPMethodAndToolErrors(t *testing.T) {
	h := NewHandler(Config{Pool: imagePool()})

	_, out := mcpCall(t, h, `{"jsonrpc":"2.0","id":1,"method":"no/such"}`)
	errObj, _ := out["error"].(map[string]any)
	if errObj["code"] != float64(-32601) {
		t.Fatalf("未知方法应 -32601：%v", out)
	}

	_, out = mcpCall(t, h, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"nope","arguments":{}}}`)
	errObj, _ = out["error"].(map[string]any)
	if errObj["code"] != float64(-32602) {
		t.Fatalf("未知工具应 -32602：%v", out)
	}

	_, out = mcpCall(t, h, `{not json`)
	errObj, _ = out["error"].(map[string]any)
	if errObj["code"] != float64(-32700) {
		t.Fatalf("坏 JSON 应 -32700：%v", out)
	}

	// ping 必须有回执（客户端保活用）
	_, out = mcpCall(t, h, `{"jsonrpc":"2.0","id":3,"method":"ping"}`)
	if _, ok := out["result"]; !ok {
		t.Fatalf("ping 应回 result：%v", out)
	}
}

// TestMCPGetNotAllowed GET /mcp 不做 SSE 流：405 + Allow（客户端据此退回纯 POST）。
func TestMCPGetNotAllowed(t *testing.T) {
	h := NewHandler(Config{Pool: imagePool()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/mcp", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code=%d want 405 body=%s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Allow") != "POST" {
		t.Fatalf("Allow=%q want POST", rec.Header().Get("Allow"))
	}
}

// TestMCPGenerateImage 工具调用走的是同一条出图路径：上游真的被调、回执在 structuredContent。
func TestMCPGenerateImage(t *testing.T) {
	f := &imageFake{status: 200, body: imageOK}
	h := NewHandler(Config{Pool: imagePool(), Upstream: f.client()})

	_, out := mcpCall(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"generate_image",`+
		`"arguments":{"prompt":"一只猫","size":"1024x1024"}}}`)
	result, _ := out["result"].(map[string]any)
	if result["isError"] != false {
		t.Fatalf("出图失败：%v", result)
	}
	sc, _ := result["structuredContent"].(map[string]any)
	items, _ := sc["data"].([]any)
	if len(items) != 1 {
		t.Fatalf("structuredContent.data 应有一张图：%v", sc)
	}
	if u, _ := items[0].(map[string]any)["url"].(string); u != "https://cos.example/img.png?sig=x" {
		t.Fatalf("图片 URL 不对：%v", items[0])
	}
	content, _ := result["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content 应有一段文本：%v", result)
	}
	if txt, _ := content[0].(map[string]any)["text"].(string); !strings.Contains(txt, "cos.example") {
		t.Fatalf("content.text 应含结果 JSON：%v", txt)
	}
	if len(f.paths) != 1 || f.paths[0] != "/v2/images/generations" {
		t.Fatalf("MCP 出图应走 /v2/images/generations，实际 %v", f.paths)
	}
}

// TestMCPGenerateVideoAndTaskMCP 生视频（wait=false）+ 用 get_video_task 收片，
// 证明 MCP 与 REST 共用同一份任务台账。
func TestMCPGenerateVideoAndTaskMCP(t *testing.T) {
	fastVideoPoll(t)
	f := &videoFake{on: func(c videoCall) (int, string) {
		switch c.Path {
		case "/v2/videos/generations":
			return 200, videoSubmitOK
		case "/v2/videos/tasks":
			return 200, videoTaskDone
		}
		return 404, `{"code":404,"msg":"unexpected"}`
	}}
	h := NewHandler(Config{Pool: imagePool(), Upstream: f.client()})

	_, out := mcpCall(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"generate_video",`+
		`"arguments":{"prompt":"一只猫在跑","seconds":4,"wait":false}}}`)
	result, _ := out["result"].(map[string]any)
	sc, _ := result["structuredContent"].(map[string]any)
	if sc["task_id"] != videoTestTaskID {
		t.Fatalf("应立刻回 task_id：%v", result)
	}
	if sc["status"] != "queued" {
		t.Fatalf("status=%v want queued", sc["status"])
	}

	// 用 MCP 查任务：台账里已有，直接命中提交它的账号。
	_, out = mcpCall(t, h, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_video_task",`+
		`"arguments":{"task_id":"`+videoTestTaskID+`"}}}`)
	result, _ = out["result"].(map[string]any)
	sc, _ = result["structuredContent"].(map[string]any)
	if sc["status"] != "completed" {
		t.Fatalf("查任务 status=%v want completed：%v", sc["status"], result)
	}
	videos, _ := sc["data"].([]any)
	if len(videos) != 1 {
		t.Fatalf("应有成品 URL：%v", sc)
	}

	// 参数缺失：task_id 是必须的，坏参数走 JSON-RPC 错误而不是 isError。
	_, out = mcpCall(t, h, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_video_task","arguments":{}}}`)
	errObj, _ := out["error"].(map[string]any)
	if errObj["code"] != float64(-32602) {
		t.Fatalf("缺 task_id 应 -32602：%v", out)
	}
}

// TestMCPToolBusinessErrorIsError 上游拒绝属于「工具执行失败」：result.isError=true，
// 错误原文进 content（模型能读到），而不是 JSON-RPC error。
func TestMCPToolBusinessErrorIsError(t *testing.T) {
	h := NewHandler(Config{Pool: imagePool(), Upstream: (&videoFake{on: func(videoCall) (int, string) {
		return 400, videoBadDuration
	}}).client()})
	_, out := mcpCall(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"generate_video",`+
		`"arguments":{"prompt":"x","seconds":1}}}`)
	result, _ := out["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("上游 400 应标成 isError：%v", out)
	}
	content, _ := result["content"].([]any)
	txt, _ := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(txt, "supported range: 4-15") {
		t.Fatalf("错误原文应进 content：%q", txt)
	}
}

// TestMCPListModels 列模型工具：读目录旁路留存，给出带前缀的 id 与 kind，不产生上游请求。
func TestMCPListModels(t *testing.T) {
	resetDynamicModelsCache()
	t.Cleanup(resetDynamicModelsCache)
	catalog := func() (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[` +
			`{"id":"glm-5.2","maxInputTokens":200000,"maxOutputTokens":32000},` +
			`{"id":"minimax-video-h3","name":"MiniMax-H3","tags":["text-to-video","image-to-video"]},` +
			`{"id":"hunyuan-image-alpha","name":"Hunyuan Image Alpha","tags":["text-to-image"]}],` +
			`"agents":[{"name":"cli","models":["glm-5.2"]}]}}`, false
	}
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return catalog() }),
	})
	// 先触发一次目录探测（等价于客户端先看过 /v1/models）。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))

	_, out := mcpCall(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_generation_models","arguments":{"kind":"video"}}}`)
	result, _ := out["result"].(map[string]any)
	sc, _ := result["structuredContent"].(map[string]any)
	models, _ := sc["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("kind=video 应只有一条（%v）：%v", models, sc)
	}
	m, _ := models[0].(map[string]any)
	if m["id"] != "cn:minimax-video-h3" || m["kind"] != "video" {
		t.Fatalf("列出的视频模型不对：%v", m)
	}
	notes, _ := sc["notes"].([]any)
	if len(notes) == 0 {
		t.Fatalf("应带计费/有效期说明：%v", sc)
	}
}
