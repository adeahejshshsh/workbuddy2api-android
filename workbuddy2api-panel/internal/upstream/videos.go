// videos.go 出视频 / 查任务：POST {chatBase}/v2/videos/generations 与 /v2/videos/tasks。
//
// 协议来源：WorkBuddy 桌面端 VideoService（DSH 插件 src/video-gen.ts 注释标注 2026-10-25
// 真机实测，二进制 WorkBuddy/resources/app.asar.unpacked/cli/dist/codebuddy-lite-wb.mjs），
// 并由本仓库 2026-11-04 直连 cn 池复核（minimax-video-h3 / kling-video-v3 各跑通一次）：
//
// 提交 POST /v2/videos/generations
//
//	{prompt, model, seconds, negative_prompt, watermark:true,
//	 image_url / last_image_url（图生视频、首尾帧）,
//	 extra_parameters:{resolution, enable_audio, aspect_ratio（仅文生视频）}}
//	→ 信封 {code,msg,data}，data 解包后形如
//	  {"id":"1312831544-WandVideo-<hex>","status":"queued","created_at":1793801279,
//	   "request_id":"<hex>"}
//
// 查询 POST /v2/videos/tasks {task_id}
//
//	→ data 解包后形如
//	  {"id":..,"status":"completed","created_at":..,"completed_at":..,
//	   "data":[{"url":"https://.../xxx_0.mp4?q-signature=..."}],
//	   "usage":{"output_video_duration":4,"output_video_tokens":200000,
//	            "total_tokens":200000,"output_video_coefficient":1,"credit":57.14},
//	   "size":"768P"}
//
// 两个容易踩错的点：
//   - 任务 id 在**顶层**（信封 data 的 id），不是 data.data.id。插件侧看到的是完整信封的
//     data.id；网关侧 growthJSON 已经把信封拆掉，所以顶层 id 就是 task id——同一字段两种视角。
//   - 成品是 COS 签名直链（实测 q-sign-time 跨度 43200s ≈ 12h），与图片通道同款：网关只转发
//     URL，不转存字节。想让链接长期可用请客户端自己落盘。
//
// 实测计费（2026-11-04，cn 池）：minimax-video-h3 4s = 57.14 credit、size=768P、约 124s 出片；
// kling-video-v3 请求 4s 实际出 5s = 102.86 credit、回执不带 size、约 473s 出片（慢一个量级）。
// 时长/模型越界由上游拒：400 code=14407「unsupported video duration: 1 seconds,
// supported range: 4-15 seconds」、「Video model [x] route config not found」；缺 prompt
// 也是上游拒（code=10001）。网关不做每模型区间校验，只做转发与账号处置。
package upstream

import (
	"encoding/json"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 视频通道（chatBase 域，与 images/growth 同 base 同头）。
const (
	videoGenerationsPath = "/v2/videos/generations"
	videoTasksPath       = "/v2/videos/tasks"
)

// VideoGenerations 提交出视频任务，返回上游信封（data.id 即任务 id）。
func (c *Client) VideoGenerations(a *auth.Auth, body any) (json.RawMessage, error) {
	return c.growthJSON(a, http.MethodPost, videoGenerationsPath, body)
}

// VideoTask 查询视频任务：{task_id} → data.status / data[].url。
//
// 任务只能被**提交它的账号**看到，故调用方必须传入同一账号（见 server 侧任务台账）。
func (c *Client) VideoTask(a *auth.Auth, taskID string) (json.RawMessage, error) {
	return c.growthJSON(a, http.MethodPost, videoTasksPath, map[string]any{"task_id": taskID})
}

// VideoTaskState 轮询回执的解析结果（宽容解析：上游缺字段按零值，不编造）。
type VideoTaskState struct {
	ID          string
	Status      string // queued / running / completed / failed（原样保留，不归一化）
	CreatedAt   int64
	CompletedAt int64
	// URLs 成品地址（签名直链，completed 才有，可能多段）。
	URLs []string
	// Size 分辨率档位（如 "768P"；kling 回执不带）。
	Size string
	// 用量：视频按 output_video_tokens 计费，credit 为实扣积分。
	Duration    int64
	Tokens      int64
	Coefficient float64
	Credit      float64
	// ErrorText 上游失败原因（msg 或 error 字段原文）。
	ErrorText string
}

// Done 报告是否终态（completed/failed，兼容历史拼写 succeeded/success）。
func (s VideoTaskState) Done() bool {
	switch s.Status {
	case "completed", "failed", "succeeded", "success":
		return true
	}
	return false
}

// Succeeded 报告终态是否成功。
func (s VideoTaskState) Succeeded() bool {
	switch s.Status {
	case "completed", "succeeded", "success":
		return true
	}
	return false
}

// ParseVideoTask 解析轮询回执（入参是 growthJSON 返回的、已解包的 data 原文）。
//
// 宽容点：成品地址支持 data[].url（实测形态）与顶层 url/video_url；usage 缺失时全零
// （调用方据此不记账）。
func ParseVideoTask(raw []byte) VideoTaskState {
	var st VideoTaskState
	var env struct {
		ID          string          `json:"id"`
		TaskID      string          `json:"task_id"`
		Status      string          `json:"status"`
		CreatedAt   int64           `json:"created_at"`
		CompletedAt int64           `json:"completed_at"`
		Size        string          `json:"size"`
		URL         string          `json:"url"`
		VideoURL    string          `json:"video_url"`
		Msg         string          `json:"msg"`
		Message     string          `json:"message"`
		Error       json.RawMessage `json:"error"`
		Data        []struct {
			URL string `json:"url"`
		} `json:"data"`
		Usage struct {
			Duration    int64   `json:"output_video_duration"`
			Tokens      int64   `json:"output_video_tokens"`
			Coefficient float64 `json:"output_video_coefficient"`
			Credit      float64 `json:"credit"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return st
	}
	st.ID = env.ID
	if st.ID == "" {
		st.ID = env.TaskID
	}
	st.Status = env.Status
	st.CreatedAt, st.CompletedAt = env.CreatedAt, env.CompletedAt
	st.Size = env.Size
	st.Duration = env.Usage.Duration
	st.Tokens = env.Usage.Tokens
	st.Coefficient = env.Usage.Coefficient
	st.Credit = env.Usage.Credit
	st.ErrorText = env.Msg
	if st.ErrorText == "" {
		st.ErrorText = env.Message
	}
	if st.ErrorText == "" && len(env.Error) > 0 {
		// error 可能是字符串或对象（{"message":...}）：两种都取原文。
		var s string
		if json.Unmarshal(env.Error, &s) == nil {
			st.ErrorText = s
		} else {
			var obj struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(env.Error, &obj) == nil {
				st.ErrorText = obj.Message
			}
		}
	}
	for _, d := range env.Data {
		if d.URL != "" {
			st.URLs = append(st.URLs, d.URL)
		}
	}
	if len(st.URLs) == 0 {
		if env.VideoURL != "" {
			st.URLs = append(st.URLs, env.VideoURL)
		} else if env.URL != "" {
			st.URLs = append(st.URLs, env.URL)
		}
	}
	return st
}

// ParseVideoSubmit 从提交回执取 task id 与初始状态（顶层 id 优先，兼容 task_id 字段）。
func ParseVideoSubmit(raw []byte) (taskID, status string) {
	var env struct {
		ID     string `json:"id"`
		TaskID string `json:"task_id"`
		Status string `json:"status"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return "", ""
	}
	taskID = env.ID
	if taskID == "" {
		taskID = env.TaskID
	}
	return taskID, env.Status
}

// videoRouteRealm 已实测的「视频模型 → 有路由的域」。
//
// 与 imageRouteRealm 同一条教训：目录里出现的媒体模型不一定在本域有路由，列给客户端等于
// 引诱它选一个必然 400 的模型。只写**实测过**的组合，未知模型照旧列出（不猜）。
//
// 2026-11-04 实测（cn 池直连）：
//
//	cn:minimax-video-h3  4s → 200, credit 57.14, size 768P
//	cn:kling-video-v3    4s → 200, credit 102.86（实际出 5s）
//	cn:not-a-video-model → 400 code=14407「Video model [...] route config not found」
//
// 2026-11-05 复测：minimax-video-h3 与 kling-video-v3 在 **cn / global 两池都有路由**
// （global:kling 时长下限 3s、global:minimax 4s，cn 都是 4s；global 出片落在 ap-hongkong
// COS，4s 768P = 27.4 credit）。所以当前没有「只在一个域可用」的视频模型；
// 出现单域模型时在这里写 model -> 唯一有路由的域，列表就自动只在那一域出现。
//
// 必须是 nil 而不是 map[string]string{}：实测写成空字面量时，同包的 imageRouteRealm
// 在整个进程里读成空表（/v1/models 把 global 的 hy-image-3.5 列进 cn，三条路由断言
// 全红），换成 nil 后复现不出。读 nil map 返回零值，语义与空表相同（= 无单域模型）。
var videoRouteRealm map[string]string

// videoDeadModels 上游已下架、两域都没有路由的视频模型（同 imageDeadModels 口径：
// 只写实测下架的；模型回来时删一行即可）。
var videoDeadModels = map[string]bool{}
