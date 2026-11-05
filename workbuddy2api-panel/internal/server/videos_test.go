package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 上游回执样例取自 2026-11-04 直连实测（cn 池）：提交回信封 data 的 id 在顶层，
// 轮询 completed 时带 data[].url + usage（output_video_tokens/credit/size）。
const (
	videoTestTaskID = "1312831544-WandVideo-abc123"

	videoSubmitOK = `{"code":0,"msg":"OK","data":{"id":"` + videoTestTaskID + `","status":"queued","created_at":1793801279,"request_id":"rid1"}}`

	videoTaskRunning = `{"code":0,"msg":"OK","data":{"id":"` + videoTestTaskID + `","status":"running","created_at":1793801279}}`

	videoTaskDone = `{"code":0,"msg":"OK","data":{"id":"` + videoTestTaskID + `","status":"completed",` +
		`"created_at":1793801279,"completed_at":1793801403,` +
		`"data":[{"url":"https://cos.example/v.mp4?sig=x"}],` +
		`"usage":{"output_video_duration":4,"output_video_tokens":200000,"total_tokens":200000,` +
		`"output_video_coefficient":1,"credit":57.14},"size":"768P"}}`

	videoTaskFailed = `{"code":0,"msg":"OK","data":{"id":"` + videoTestTaskID + `","status":"failed","msg":"content blocked"}}`

	videoBadDuration = `{"code":14407,"msg":"Create video failed with error: unsupported video duration: 1 seconds, supported range: 4-15 seconds"}`
)

// videoCall 一次上游调用（谁、哪个路径、什么 body）。
type videoCall struct {
	Path, Host, Body string
}

// videoFake 记录视频通道的上游调用，并按脚本回信封（on 为 nil 时回空 200）。
type videoFake struct {
	mu    sync.Mutex
	calls []videoCall
	on    func(c videoCall) (int, string)
}

func (f *videoFake) client() *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			c := videoCall{Path: r.URL.Path, Host: r.URL.Host, Body: string(raw)}
			f.mu.Lock()
			f.calls = append(f.calls, c)
			f.mu.Unlock()
			status, body := 200, ""
			if f.on != nil {
				status, body = f.on(c)
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:     "https://cn.example",
		BillingBaseCN:  "https://cn.example",
		ChatBaseGlobal: "https://global.example",
		GlobalEnabled:  true,
	}
}

func (f *videoFake) snapshot() []videoCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]videoCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *videoFake) count(path string) int {
	n := 0
	for _, c := range f.snapshot() {
		if c.Path == path {
			n++
		}
	}
	return n
}

// fastVideoPoll 把轮询间隔压到 1ms（默认 5s 是给真实上游的）。
func fastVideoPoll(t *testing.T) {
	t.Helper()
	old := videoPollInterval
	videoPollInterval = time.Millisecond
	t.Cleanup(func() { videoPollInterval = old })
}

func postVideo(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/videos/generations", strings.NewReader(body)))
	return rec
}

func getVideo(t *testing.T, h *Handler, id string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/videos/"+id, nil))
	return rec
}

// TestVideoUpstreamBody 客户端形态 → 上游形态：图换 image_url、比例/音频/分辨率进
// extra_parameters、wait/timeout_seconds 不下发、未知字段照旧透传。
func TestVideoUpstreamBody(t *testing.T) {
	got, err := videoUpstreamBody(map[string]any{
		"prompt":          "一只猫在草地上奔跑",
		"seconds":         float64(4),
		"aspect_ratio":    "16:9",
		"enable_audio":    false,
		"resolution":      "768P",
		"image":           "https://cos.example/first.png",
		"last_image":      "https://cos.example/last.png",
		"wait":            false,
		"timeout_seconds": float64(600),
		"negative_prompt": "模糊",
		"seed":            float64(42),
	}, "minimax-video-h3")
	if err != nil {
		t.Fatalf("videoUpstreamBody: %v", err)
	}
	if got["model"] != "minimax-video-h3" {
		t.Fatalf("model=%v want 裸名", got["model"])
	}
	if got["image_url"] != "https://cos.example/first.png" || got["last_image_url"] != "https://cos.example/last.png" {
		t.Fatalf("图生视频字段没转成 image_url/last_image_url：%v", got)
	}
	if _, ok := got["image"]; ok {
		t.Fatalf("上游不认 image，必须换名：%v", got)
	}
	if _, ok := got["wait"]; ok {
		t.Fatalf("wait 是网关自己的开关，不该下发：%v", got)
	}
	if _, ok := got["timeout_seconds"]; ok {
		t.Fatalf("timeout_seconds 是网关自己的开关，不该下发：%v", got)
	}
	if got["watermark"] != true {
		t.Fatalf("watermark 缺省应为 true（与插件一致），got %v", got["watermark"])
	}
	extra, _ := got["extra_parameters"].(map[string]any)
	if extra["aspect_ratio"] != "16:9" || extra["enable_audio"] != false || extra["resolution"] != "768P" {
		t.Fatalf("extra_parameters 组装错了：%v", extra)
	}
	if got["seed"] != float64(42) || got["negative_prompt"] != "模糊" {
		t.Fatalf("未知字段应原样透传：%v", got)
	}
	// 显式 watermark 可关
	got2, _ := videoUpstreamBody(map[string]any{"prompt": "x", "watermark": false}, "m")
	if got2["watermark"] != false {
		t.Fatalf("显式 watermark=false 被覆盖了：%v", got2)
	}
}

// TestVideosSubmitAsync 提交 + wait=false：一次上游调用、正确落点、正确 body，立刻回 task id。
func TestVideosSubmitAsync(t *testing.T) {
	f := &videoFake{on: func(c videoCall) (int, string) {
		if c.Path == "/v2/videos/generations" {
			return 200, videoSubmitOK
		}
		return 404, `{"code":404,"msg":"unexpected"}`
	}}
	h := NewHandler(Config{Pool: imagePool(), Upstream: f.client()})

	rec := postVideo(t, h, `{"prompt":"一只猫在草地上奔跑","seconds":4,"aspect_ratio":"16:9","wait":false}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("回程不是 JSON：%s", rec.Body)
	}
	if got["id"] != videoTestTaskID || got["task_id"] != videoTestTaskID {
		t.Fatalf("回执缺少 task id：%v", got)
	}
	if got["status"] != "queued" {
		t.Fatalf("status=%v want queued", got["status"])
	}
	calls := f.snapshot()
	if len(calls) != 1 {
		t.Fatalf("wait=false 只该提交一次，实际 %d 次：%v", len(calls), calls)
	}
	if calls[0].Path != "/v2/videos/generations" || calls[0].Host != "cn.example" {
		t.Fatalf("落点 %s%s want /v2/videos/generations @ cn.example", calls[0].Host, calls[0].Path)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(calls[0].Body), &sent)
	if sent["model"] != "minimax-video-h3" {
		t.Fatalf("缺省模型应是 minimax-video-h3（上游收裸名），got %v", sent["model"])
	}
	if sent["prompt"] != "一只猫在草地上奔跑" || sent["seconds"] != float64(4) {
		t.Fatalf("prompt/seconds 应原样：%v", sent)
	}
	extra, _ := sent["extra_parameters"].(map[string]any)
	if extra["aspect_ratio"] != "16:9" {
		t.Fatalf("aspect_ratio 应进 extra_parameters：%v", sent)
	}
	if _, ok := sent["wait"]; ok {
		t.Fatalf("wait 不该下发：%v", sent)
	}
}

// TestVideosSyncWaitsForCompletion 默认同步等：running→completed，回成品 URL 与实扣积分；
// 同一任务再查不会重复记账（台账 Charged 标记）。
func TestVideosSyncWaitsForCompletion(t *testing.T) {
	fastVideoPoll(t)
	var polls int
	f := &videoFake{on: func(c videoCall) (int, string) {
		switch c.Path {
		case "/v2/videos/generations":
			return 200, videoSubmitOK
		case "/v2/videos/tasks":
			polls++
			if polls == 1 {
				return 200, videoTaskRunning
			}
			return 200, videoTaskDone
		}
		return 404, `{"code":404,"msg":"unexpected"}`
	}}
	p := imagePool()
	h := NewHandler(Config{Pool: p, Upstream: f.client()})

	rec := postVideo(t, h, `{"prompt":"一只猫在草地上奔跑","seconds":4}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("回程不是 JSON：%s", rec.Body)
	}
	if got["status"] != "completed" {
		t.Fatalf("status=%v want completed；body=%s", got["status"], rec.Body)
	}
	videos, _ := got["data"].([]any)
	if len(videos) != 1 {
		t.Fatalf("data 应有一支成品：%v", got)
	}
	if u, _ := videos[0].(map[string]any)["url"].(string); u != "https://cos.example/v.mp4?sig=x" {
		t.Fatalf("成品 URL 不对：%v", videos[0])
	}
	if got["size"] != "768P" || got["seconds"] != float64(4) {
		t.Fatalf("size/seconds 应透出：%v", got)
	}
	usage, _ := got["usage"].(map[string]any)
	if usage["credit"] != 57.14 {
		t.Fatalf("usage.credit=%v want 57.14", usage["credit"])
	}
	// 记账：出片那一刻把实扣积分写进账号成本台账（NoteModelCost 需要 tokens>0，视频按
	// output_video_tokens 记）。
	st, ok := p.Status("c1")
	if !ok || len(st.ModelCosts) != 1 || st.ModelCosts[0].Model != "minimax-video-h3" {
		t.Fatalf("视频成本没记账：%+v", st.ModelCosts)
	}
	samples := st.ModelCosts[0].Samples

	// 再查一次：状态照旧，但不能再记一次账。
	rec2 := getVideo(t, h, videoTestTaskID)
	if rec2.Code != 200 {
		t.Fatalf("再查任务 code=%d body=%s", rec2.Code, rec2.Body)
	}
	st2, _ := p.Status("c1")
	if len(st2.ModelCosts) != 1 || st2.ModelCosts[0].Samples != samples {
		t.Fatalf("重复查询不该重复记账：before samples=%d after=%+v", samples, st2.ModelCosts)
	}
}

// TestVideosFailedTaskReturns502 上游 failed：502 + hint，且不记账。
func TestVideosFailedTaskReturns502(t *testing.T) {
	fastVideoPoll(t)
	f := &videoFake{on: func(c videoCall) (int, string) {
		if c.Path == "/v2/videos/generations" {
			return 200, videoSubmitOK
		}
		return 200, videoTaskFailed
	}}
	p := imagePool()
	h := NewHandler(Config{Pool: p, Upstream: f.client()})
	rec := postVideo(t, h, `{"prompt":"x","seconds":4}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d want 502 body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "content blocked") {
		t.Fatalf("应带上游失败原因：%s", rec.Body)
	}
	if st, _ := p.Status("c1"); len(st.ModelCosts) != 0 {
		t.Fatalf("失败任务不该记账：%+v", st.ModelCosts)
	}
}

// TestVideosBadDurationFailsFast 上游 400（时长越界）是请求级错误：只打一次上游、
// 原样回 400，不换号、不记账号惩罚。
func TestVideosBadDurationFailsFast(t *testing.T) {
	f := &videoFake{on: func(c videoCall) (int, string) { return 400, videoBadDuration }}
	p := imagePool()
	h := NewHandler(Config{Pool: p, Upstream: f.client()})
	rec := postVideo(t, h, `{"prompt":"x","seconds":1}`)
	if rec.Code != 400 {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "supported range: 4-15") {
		t.Fatalf("应透出上游原文：%s", rec.Body)
	}
	if n := f.count("/v2/videos/generations"); n != 1 {
		t.Fatalf("请求级错误不该换号重试，实际打了 %d 次", n)
	}
}

// TestVideosMissingPrompt 缺 prompt 在本地就 400，不打上游（不烧积分）。
func TestVideosMissingPrompt(t *testing.T) {
	f := &videoFake{}
	h := NewHandler(Config{Pool: imagePool(), Upstream: f.client()})
	rec := postVideo(t, h, `{"seconds":4}`)
	if rec.Code != 400 {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "prompt is required") {
		t.Fatalf("应该是缺 prompt 的提示：%s", rec.Body)
	}
	if n := len(f.snapshot()); n != 0 {
		t.Fatalf("缺 prompt 不该打上游，实际 %d 次", n)
	}
}

// TestVideosUnknownTask 查不存在的任务：每个可用账号试一次，最终 404 + 可操作 hint。
func TestVideosUnknownTask(t *testing.T) {
	f := &videoFake{on: func(c videoCall) (int, string) {
		return 404, `{"code":404,"msg":"task not found"}`
	}}
	h := NewHandler(Config{Pool: imagePool(), Upstream: f.client()})
	rec := getVideo(t, h, "nonexistent-task")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d want 404 body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "gateway_hint") {
		t.Fatalf("404 应带 hint（说明任务只对提交它的账号可见）：%s", rec.Body)
	}
	if n := f.count("/v2/videos/tasks"); n == 0 {
		t.Fatal("台账里没有的任务应按账号逐个试，实际没打上游")
	}
}

// TestVideosTaskLedger 台账语义：put/get/adopt/claimCharge（只有第一次能拿到记账权）。
func TestVideosTaskLedger(t *testing.T) {
	s := newVideoTaskStore()
	if _, ok := s.get("t1"); ok {
		t.Fatal("空台账不该命中")
	}
	s.put("t1", "c1", "cn", "minimax-video-h3")
	o, ok := s.get("t1")
	if !ok || o.UID != "c1" || o.Realm != "cn" || o.Model != "minimax-video-h3" {
		t.Fatalf("put/get 不一致：%+v ok=%v", o, ok)
	}
	if !s.claimCharge("t1") {
		t.Fatal("第一次 claimCharge 应该成功")
	}
	if s.claimCharge("t1") {
		t.Fatal("第二次 claimCharge 必须失败（防重复记账）")
	}
	s.adopt("t2", "g1", "global", "kling-video-v3", false)
	if o, ok := s.get("t2"); !ok || o.UID != "g1" {
		t.Fatalf("adopt 没写进台账：%+v ok=%v", o, ok)
	}
	s.adopt("t2", "c9", "cn", "x", false) // 已存在就不覆盖（收养只补缺）
	if o, _ := s.get("t2"); o.UID != "g1" {
		t.Fatalf("adopt 覆盖了已有条目：%+v", o)
	}
}

// TestModelsListVideoEntries /v1/models 里视频模型要以 video_generation 单列，
// modalities.output 是 video（不是 text/image）。
func TestModelsListVideoEntries(t *testing.T) {
	resetDynamicModelsCache()
	t.Cleanup(resetDynamicModelsCache)
	catalog := func() (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[` +
			`{"id":"glm-5.2","maxInputTokens":200000,"maxOutputTokens":32000},` +
			`{"id":"minimax-video-h3","name":"MiniMax-H3","tags":["text-to-video","image-to-video"]},` +
			`{"id":"kling-video-v3","name":"Kling-Video-V3","tags":["text-to-video","image-to-video"]},` +
			`{"id":"hunyuan-image-alpha","name":"Hunyuan Image Alpha","tags":["text-to-image"]}],` +
			`"agents":[{"name":"cli","models":["glm-5.2"]}]}}`, false
	}
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return catalog() }),
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	var videoEntry map[string]any
	for _, m := range resp.Data {
		if m["id"] == "cn:minimax-video-h3" {
			videoEntry = m
		}
	}
	if videoEntry == nil {
		t.Fatalf("视频模型没出现在 /v1/models：%+v", resp.Data)
	}
	if videoEntry["video_generation"] != true {
		t.Fatalf("视频条目应带 video_generation=true：%v", videoEntry)
	}
	if _, ok := videoEntry["image_generation"]; ok {
		t.Fatalf("视频条目不该带 image_generation：%v", videoEntry)
	}
	mod, _ := videoEntry["modalities"].(map[string]any)
	out, _ := mod["output"].([]any)
	if len(out) != 1 || out[0] != "video" {
		t.Fatalf("modalities.output 应是 [video]：%v", videoEntry["modalities"])
	}
}

// 上游 usage 只回一次：第二次查询没有 usage/size，回执仍要带上第一次记下的账，
// 否则客户端复查时会以为这笔扣费消失了。
const videoTaskDoneNoUsage = `{"id":"1312831544-WandVideo-abc123","status":"completed",` +
	`"created_at":1793801279,"completed_at":1793801403,` +
	`"data":[{"url":"https://cos.example/out.mp4?sig=y"}]}`

func TestVideoRepeatPollKeepsUsage(t *testing.T) {
	fastVideoPoll(t)
	polls := 0
	f := &videoFake{on: func(c videoCall) (int, string) {
		switch c.Path {
		case "/v2/videos/generations":
			return 200, videoSubmitOK
		case "/v2/videos/tasks":
			polls++
			if polls == 1 {
				return 200, videoTaskDone
			}
			return 200, videoTaskDoneNoUsage
		}
		return 404, `{"code":404,"msg":"unexpected"}`
	}}
	h := NewHandler(Config{Pool: imagePool(), Upstream: f.client()})
	rec := postVideo(t, h, `{"prompt":"一只猫","seconds":4,"wait":false}`)
	var sub map[string]any
	json.Unmarshal(rec.Body.Bytes(), &sub)
	taskID, _ := sub["task_id"].(string)

	var first, second map[string]any
	rec = getVideo(t, h, taskID)
	json.Unmarshal(rec.Body.Bytes(), &first)
	rec = getVideo(t, h, taskID)
	json.Unmarshal(rec.Body.Bytes(), &second)

	use, _ := first["usage"].(map[string]any)
	if use == nil || use["credit"] != 57.14 {
		t.Fatalf("首次查询应带 credit：%v", first)
	}
	use2, _ := second["usage"].(map[string]any)
	if use2 == nil || use2["credit"] != 57.14 {
		t.Fatalf("重复查询仍应带 credit（上游这次没回 usage）：%v", second)
	}
	if second["size"] != "768P" || second["seconds"] != float64(4) {
		t.Fatalf("重复查询应从台账补齐 size/seconds：%v", second)
	}
	// 只该记一次账
	st, _ := h.cfg.Pool.Status("c1")
	if len(st.ModelCosts) != 1 || st.ModelCosts[0].Samples != 1 {
		t.Fatalf("应只记一次账：%+v", st.ModelCosts)
	}
}
