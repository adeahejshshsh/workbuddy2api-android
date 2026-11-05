// mcp.go MCP（Model Context Protocol）服务端：POST /mcp，JSON-RPC 2.0，
// Streamable HTTP 传输的无状态子集（只回 application/json，不开 SSE 流）。
//
// 为什么放在网关里：生图/生视频本来就是「一个 API key + 一条 HTTP」的能力，
// 做成 MCP 工具面之后，Claude Desktop / Cursor / Cherry Studio / DSH 这类客户端
// 不必各自实现 WorkBuddy 的鉴权、选号、轮询与计费，接一个 URL 就能用。
//
// 实现范围（刻意只做这些）：
//
//	initialize / notifications/initialized / ping     握手与保活
//	tools/list / tools/call                           工具面（见 mcpTools）
//
// 没做的：resources、prompts、sampling、notifications/tools/list_changed、
// SSE 推送、会话（Mcp-Session-Id）。无状态意味着水平扩容和多实例并置都安全——
// 代价是客户端断开后网关不会主动通知进度，长任务靠 get_video_task 轮询。
//
// 兼容性说明：initialize 会回显客户端要求的协议版本（2024-11-05 / 2025-03-26 /
// 2025-06-18 三版都收），其它版本回我们自己支持的最新版，让客户端自行决定是否继续。
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
)

// mcpProtocolVersions 支持的 MCP 协议版本（新→旧）。客户端报的版本在表里就原样回显。
var mcpProtocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// BuildVersion 二进制版本串（cmd/server 用同值覆盖 appVersion；MCP serverInfo 用）。
// 保持 "dev" 时面板与 MCP 都显示 dev——本地构建不注入版本，与发布流程一致。
var BuildVersion = "dev"

// mcpServerName MCP serverInfo.name（客户端会拿它当工具名前缀）。
const mcpServerName = "workbuddy2api"

// mcpInstructions initialize 回执里的使用说明：客户端通常把它塞进系统提示词，
// 所以这里只写「怎么用」和「钱怎么花」，不写实现细节。
const mcpInstructions = `WorkBuddy 媒体网关（生图 / 改图 / 生视频）。
- 生图：generate_image（文生图）、edit_image（图生图/改图），同步返回图片 URL。
- 生视频：generate_video 是异步任务。默认阻塞等到出片（minimax 约 2 分钟、kling 约 8 分钟），
  wait=false 则立刻返回 task_id，再用 get_video_task 查。
- 成品都是签名直链（图片约 24 小时、视频约 12 小时有效），需要长期保存请自行下载。
- 模型名带域前缀：cn: 国内池、global: 国际池。不确定有什么模型就先调 list_generation_models。
- 去水印（可选功能，默认关闭）：strip_watermark 把成品图底部的纯白留白整条裁掉，
  右下角「AI生成 / WORKBUDD>.」水印随之消失。本地处理、不扣积分；留白不足会回 isError
  并给出实测像素数，此时先用 edit_image 把下方约 1/3 涂白再调一次。
- 每次生成都真实扣上游积分（生图约 5.7/张、视频 4 秒约 57），别拿它当免费接口刷。`

// —— JSON-RPC 2.0 线格式 ——

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// mcpMethodNotAllowed GET/DELETE /mcp：本服务不提供 SSE 流与显式会话终止。
// Streamable HTTP 规范允许在这种情况下回 405，客户端会退回纯 POST 模式。
func (h *Handler) mcpMethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", "POST")
	writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed",
		"本服务的 MCP 端点只支持 POST（JSON-RPC 2.0，无 SSE 流、无显式会话）")
}

// mcpPost MCP 端点：一个 HTTP 请求 = 一条（或一批拆开后的）JSON-RPC 消息。
func (h *Handler) mcpPost(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		h.mcpRespond(w, nil, nil, &mcpRPCError{Code: -32700, Message: "Parse error: empty body"})
		return
	}
	var req mcpRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		h.mcpRespond(w, nil, nil, &mcpRPCError{Code: -32700, Message: "Parse error: " + err.Error()})
		return
	}
	if req.JSONRPC != "" && req.JSONRPC != "2.0" {
		h.mcpRespond(w, req.ID, nil, &mcpRPCError{Code: -32600, Message: "Invalid Request: jsonrpc 必须是 \"2.0\""})
		return
	}
	// 没有 id 的是通知（notification）：按规范不能回包，只回 202。
	notification := len(req.ID) == 0 || string(req.ID) == "null"

	switch req.Method {
	case "initialize":
		h.mcpRespond(w, req.ID, h.mcpInitialize(req.Params), nil)
	case "notifications/initialized", "notifications/cancelled", "notifications/progress",
		"notifications/roots/list_changed":
		w.WriteHeader(http.StatusAccepted)
	case "ping":
		h.mcpRespond(w, req.ID, map[string]any{}, nil)
	case "tools/list":
		h.mcpRespond(w, req.ID, map[string]any{"tools": h.mcpTools()}, nil)
	case "tools/call":
		result, rpcErr := h.mcpCallTool(req.Params)
		h.mcpRespond(w, req.ID, result, rpcErr)
	case "resources/list":
		h.mcpRespond(w, req.ID, map[string]any{"resources": []any{}}, nil)
	case "resources/templates/list":
		h.mcpRespond(w, req.ID, map[string]any{"resourceTemplates": []any{}}, nil)
	case "prompts/list":
		h.mcpRespond(w, req.ID, map[string]any{"prompts": []any{}}, nil)
	default:
		h.mcpRespond(w, req.ID, nil, &mcpRPCError{
			Code:    -32601,
			Message: fmt.Sprintf("Method not found: %s", req.Method),
		})
	}
	_ = notification
}

// mcpRespond 统一回包：result 与 err 二选一；id 原样回显（通知已在调用处提前返回）。
func (h *Handler) mcpRespond(w http.ResponseWriter, id json.RawMessage, result any, rpcErr *mcpRPCError) {
	out := map[string]any{"jsonrpc": "2.0"}
	if len(id) == 0 {
		out["id"] = nil
	} else {
		out["id"] = id
	}
	if rpcErr != nil {
		out["error"] = rpcErr
	} else {
		out["result"] = result
	}
	writeJSON(w, http.StatusOK, out)
}

// mcpInitialize 握手：回显客户端协议版本（认识的），声明能力，带使用说明。
func (h *Handler) mcpInitialize(params json.RawMessage) map[string]any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)
	version := mcpProtocolVersions[0]
	for _, v := range mcpProtocolVersions {
		if v == p.ProtocolVersion {
			version = v
			break
		}
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities": map[string]any{
			// 工具面固定（不热更），listChanged 恒 false——客户端不必监听工具变更。
			"tools": map[string]any{"listChanged": false},
		},
		"serverInfo": map[string]any{
			"name":    mcpServerName,
			"version": BuildVersion,
		},
		"instructions": mcpInstructions,
	}
}

// mcpCallTool 执行一次工具调用。返回 (result, rpcErr)：工具业务失败走 result.isError
// （内容照旧给模型看），只有参数结构坏了才回 JSON-RPC 错误。
func (h *Handler) mcpCallTool(params json.RawMessage) (any, *mcpRPCError) {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &mcpRPCError{Code: -32602, Message: "Invalid params: " + err.Error()}
	}
	if strings.TrimSpace(p.Name) == "" {
		return nil, &mcpRPCError{Code: -32602, Message: "Invalid params: 缺少工具名 name"}
	}
	args := p.Arguments
	if args == nil {
		args = map[string]any{}
	}
	switch p.Name {
	case "list_generation_models":
		return h.mcpListModels(args), nil
	case "generate_image":
		return h.mcpProxyCall(args, "/v1/images/generations", func(w http.ResponseWriter, r *http.Request) { h.images(w, r, false) })
	case "edit_image":
		return h.mcpProxyCall(args, "/v1/images/edits", func(w http.ResponseWriter, r *http.Request) { h.images(w, r, true) })
	case "generate_video":
		return h.mcpProxyCall(args, "/v1/videos/generations", h.videos)
	case "get_video_task":
		return h.mcpVideoTask(args)
	case "strip_watermark":
		return h.mcpStripWatermark(args)
	}
	return nil, &mcpRPCError{Code: -32602, Message: "Invalid params: 未知工具 " + p.Name}
}

// mcpProxyCall 把工具参数当请求体打到既有的网关入口上，回执原样转成 MCP 内容。
//
// 这样做是刻意的：MCP 与 REST 走**同一条**选号 / 失败换号 / 计费 / 台账路径，不会出现
// 「REST 计费、MCP 不计费」这种双账本。代价是绕过了 HTTP 层（不经 ServeHTTP），
// 所以 MCP 调用没有请求台账行——面板里要看的话，REST 那一层照旧有。
func (h *Handler) mcpProxyCall(args map[string]any, path string, fn http.HandlerFunc) (any, *mcpRPCError) {
	body, err := json.Marshal(args)
	if err != nil {
		return nil, &mcpRPCError{Code: -32602, Message: "Invalid params: " + err.Error()}
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	fn(rec, req)

	data := rec.Body.Bytes()
	status := rec.Code
	if status == 0 {
		status = http.StatusOK
	}
	// 2xx：把回执同时塞进 structuredContent（机器读）与 text（模型读）。
	var parsed any
	text := strings.TrimSpace(string(data))
	if json.Unmarshal(data, &parsed) == nil {
		text = prettyJSON(parsed)
	}
	content := []map[string]any{{"type": "text", "text": text}}
	if status >= 400 {
		return map[string]any{"content": content, "isError": true}, nil
	}
	return map[string]any{
		"content":           content,
		"structuredContent": parsed,
		"isError":           false,
	}, nil
}

// mcpVideoTask 查视频任务：复用 POST /v1/videos/tasks 的实现（含台账与计费）。
func (h *Handler) mcpVideoTask(args map[string]any) (any, *mcpRPCError) {
	id, _ := args["task_id"].(string)
	if strings.TrimSpace(id) == "" {
		// 宽容一点：也认 id / video_id 两个常见写法，省得模型猜参数名。
		if v, _ := args["id"].(string); strings.TrimSpace(v) != "" {
			id = v
		} else if v, _ := args["video_id"].(string); strings.TrimSpace(v) != "" {
			id = v
		}
	}
	if strings.TrimSpace(id) == "" {
		return nil, &mcpRPCError{Code: -32602, Message: "Invalid params: task_id is required"}
	}
	body, _ := json.Marshal(map[string]any{"task_id": id})
	req := httptest.NewRequest(http.MethodPost, "/v1/videos/tasks", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.videoTasks(rec, req)
	return mcpWrapResponse(rec), nil
}

func mcpWrapResponse(rec *httptest.ResponseRecorder) map[string]any {
	data := rec.Body.Bytes()
	var parsed any
	text := strings.TrimSpace(string(data))
	if json.Unmarshal(data, &parsed) == nil {
		text = prettyJSON(parsed)
	}
	status := rec.Code
	if status == 0 {
		status = http.StatusOK
	}
	out := map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": status >= 400,
	}
	if status < 400 {
		out["structuredContent"] = parsed
	}
	return out
}

// mcpListModels 列出可用的媒体模型（读目录旁路留存，零上游请求，不花钱）。
func (h *Handler) mcpListModels(args map[string]any) any {
	kind, _ := args["kind"].(string)
	kind = strings.TrimSpace(kind)
	realms := []string{"cn"}
	if h.cfg.GlobalEnabled {
		realms = append(realms, "global")
	}
	type item struct {
		ID          string   `json:"id"`
		Kind        string   `json:"kind"`
		Tags        []string `json:"tags,omitempty"`
		Realm       string   `json:"realm"`
		Vendor      string   `json:"vendor,omitempty"`
		Name        string   `json:"name,omitempty"`
		SupportsImg bool     `json:"supports_image_input,omitempty"`
	}
	var items []item
	if h.cfg.Upstream != nil {
		for _, realm := range realms {
			for _, mi := range h.cfg.Upstream.GenerationModels(realm) {
				if kind != "" && mi.GenerationKind != kind {
					continue
				}
				if h.modelHidden(realm, mi.ID) || !h.modelAllowed(realm, mi.ID) {
					continue
				}
				items = append(items, item{
					ID: realm + ":" + mi.ID, Kind: mi.GenerationKind, Tags: mi.Tags,
					Realm: realm, Vendor: mi.Vendor, Name: mi.Name, SupportsImg: mi.SupportsImages,
				})
			}
		}
	}
	payload := map[string]any{
		"models": items,
		"notes": []string{
			"图片：generate_image（文生图）/ edit_image（图生图），模型名带 cn: / global: 前缀。",
			"视频：generate_video（异步，默认等到出片），秒数范围由上游定（实测 4-15 秒）。",
			"成品是签名直链：图片约 24h、视频约 12h 有效。",
			"生图约 5.7 积分/张（cn:hunyuan-image-alpha），视频 4 秒约 57 积分（cn:minimax-video-h3）。",
		},
	}
	return map[string]any{
		"content":           []map[string]any{{"type": "text", "text": prettyJSON(payload)}},
		"structuredContent": payload,
		"isError":           false,
	}
}

// prettyJSON 给模型看的紧凑可读 JSON（缩进两格；失败回退原文）。
func prettyJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// mcpTools 工具面。描述写清楚「值多少钱、有什么坑」，模型据此自己就能选对参数——
// 少一次试错就少花一次积分。
func (h *Handler) mcpTools() []map[string]any {
	tools := []map[string]any{
		{
			"name":  "list_generation_models",
			"title": "列出可用的生图/生视频模型",
			"description": "返回本网关当前可用的媒体模型（带域前缀的 model 名、kind、tags）以及计费说明。" +
				"不确定用哪个模型、或者想知道有没有视频模型时先调它；不产生上游请求，不花钱。",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"kind": map[string]any{
						"type":        "string",
						"enum":        []string{"image", "video"},
						"description": "只列某一类；省略表示两类都列",
					},
				},
			},
		},
		{
			"name":  "generate_image",
			"title": "文生图",
			"description": "根据提示词生成图片，同步返回图片 URL（签名直链，约 24 小时有效）。" +
				"缺省模型 cn:hunyuan-image-alpha（约 5.7 积分/张）。" +
				"想要中文海报/插画时把风格、构图、光线写进 prompt，一次说清比反复重试便宜。",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"prompt"},
				"properties": map[string]any{
					"prompt": map[string]any{"type": "string", "description": "画面描述（中文或英文都行）"},
					"model": map[string]any{
						"type":        "string",
						"description": "模型名，如 cn:hunyuan-image-alpha；省略用默认模型。国际池模型前缀是 global:",
					},
					"size": map[string]any{
						"type":        "string",
						"description": "尺寸，如 1024x1024 / 1024x1536 / 1536x1024；省略由上游默认",
					},
					"quality": map[string]any{"type": "string", "description": "画质档位（部分模型支持）"},
					"n":       map[string]any{"type": "integer", "minimum": 1, "description": "生成张数，默认 1"},
				},
			},
		},
		{
			"name":  "edit_image",
			"title": "图生图 / 改图",
			"description": "在给定图片上按提示词修改（改风格、加/去元素、换背景）。" +
				"image 传公网可访问的图片 URL 或 data URL（data:image/png;base64,...）。" +
				"缺省模型 cn:hunyuan-image-alpha-edit。",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"prompt", "image"},
				"properties": map[string]any{
					"prompt": map[string]any{"type": "string", "description": "要怎么改"},
					"image": map[string]any{
						"description": "输入图：单个 URL/data URL，或它们的数组",
						"anyOf": []any{
							map[string]any{"type": "string"},
							map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						},
					},
					"model": map[string]any{"type": "string", "description": "改图模型名，省略用默认"},
					"size":  map[string]any{"type": "string", "description": "输出尺寸"},
					"mask":  map[string]any{"type": "string", "description": "可选蒙版图（URL/data URL）"},
				},
			},
		},
		{
			"name":  "generate_video",
			"title": "生成视频（异步任务）",
			"description": "提交视频生成任务。默认**阻塞等到出片**（cn:minimax-video-h3 约 2 分钟、" +
				"cn:kling-video-v3 约 8 分钟）；wait=false 立刻返回 task_id，再用 get_video_task 查。" +
				"时长由上游限（实测 4-15 秒）；文生视频可给 aspect_ratio，图生视频传 image（首帧 URL）。" +
				"成品是签名直链（约 12 小时有效）。4 秒约 57 积分（minimax）/ 5 秒约 103 积分（kling）。",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"prompt"},
				"properties": map[string]any{
					"prompt": map[string]any{"type": "string", "description": "画面与动作描述；镜头、光线、节奏写清楚"},
					"image": map[string]any{
						"type":        "string",
						"description": "图生视频的首帧：公网图片 URL 或 data URL；给了就是 image-to-video",
					},
					"last_image": map[string]any{"type": "string", "description": "尾帧图（首尾帧模式，可选）"},
					"model": map[string]any{
						"type":        "string",
						"description": "cn:minimax-video-h3（默认、快）或 cn:kling-video-v3（慢、贵）",
					},
					"seconds": map[string]any{
						"type":        "integer",
						"minimum":     1,
						"maximum":     60,
						"description": "时长（秒）。上游实测只接受 4-15，越界会被上游拒（错误原文会回给你）",
					},
					"resolution": map[string]any{"type": "string", "description": "分辨率档位，如 768P / 1080P（各模型不同）"},
					"aspect_ratio": map[string]any{
						"type":        "string",
						"enum":        []string{"16:9", "9:16", "1:1"},
						"description": "画面比例（仅文生视频生效）",
					},
					"negative_prompt": map[string]any{"type": "string", "description": "不希望出现的内容"},
					"enable_audio":    map[string]any{"type": "boolean", "description": "是否生成音轨，默认 true"},
					"watermark":       map[string]any{"type": "boolean", "description": "是否加水印，默认 true"},
					"wait": map[string]any{
						"type":        "boolean",
						"description": "true（默认）=等到出片再返回；false=立刻返回 task_id",
					},
					"timeout_seconds": map[string]any{
						"type":        "integer",
						"description": "阻塞等待上限（秒），默认 600，最大 1800；超时回 task_id 让你接着查",
					},
				},
			},
		},
		{
			"name":  "get_video_task",
			"title": "查视频任务",
			"description": "按 task_id 查进度。status=completed 时 data/videos 里是成品 URL；" +
				"status=failed 时 error 是上游原因。任务只对提交它的账号可见——本网关自己管台账，" +
				"你用同一个 URL 查就行。",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"task_id"},
				"properties": map[string]any{
					"task_id": map[string]any{"type": "string", "description": "generate_video 返回的 task_id"},
				},
			},
		},
	}
	// 水印裁切是开关功能（features.watermark_strip_enabled，默认关闭）：
	// 关着时不出现在工具面里，免得模型以为能用、白试一次。
	if h.loadLive().WatermarkStripEnabled {
		tools = append(tools, map[string]any{
			"name":  "strip_watermark",
			"title": "裁掉右下角水印（可选功能）",
			"description": "把成品图底部的纯白留白整条裁掉，右下角「AI生成 / WORKBUDD>.」水印随之消失" +
				"（本地解码裁剪，不打上游、不扣积分）。前提是画面底部真有一条纯白留白：" +
				"生成时就在 prompt 里写「画面最下方三分之一是完全纯白留白，不要有任何物体、阴影、反光、文字」。" +
				"留白不够时回 isError 并给出实测像素数——此时先用 edit_image 把下方约 1/3 涂白，再调一次本工具。" +
				"image 传 generate_image 回执里的 URL（只接受上游产物域）或 data URL。",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"image"},
				"properties": map[string]any{
					"image": map[string]any{
						"type":        "string",
						"description": "图片 URL（上游产物域，如 COS 直链）或 data URL / 裸 base64",
					},
					"min_blank_px": map[string]any{
						"type":        "integer",
						"description": "判定「留白够裁」的最小高度，默认 90（水印最高约 80px + 下边距 10px）",
					},
					"keep_blank": map[string]any{
						"type":        "integer",
						"description": "裁完在内容下方保留的留白像素，默认 0（贴着内容边裁）；想留白边做版式给 20~60",
					},
					"format": map[string]any{
						"type":        "string",
						"enum":        []string{"png", "jpeg"},
						"description": "输出格式；省略跟随输入。PNG 无损但体积大，JPEG 可配 quality",
					},
					"quality": map[string]any{
						"type":        "integer",
						"description": "JPEG 质量 1-100，默认 92（format=jpeg 时有效）",
					},
				},
			},
		})
	}
	return tools
}
