package upstream

import "testing"

// 回执样例来自 2026-11-04 直连实测（cn 池 minimax-video-h3 4s / kling-video-v3 4s）。
const (
	videoSubmitRaw = `{"id":"1312831544-WandVideo-9f2c","status":"queued","created_at":1793801279,"request_id":"8f0c"}`
	videoTaskRaw   = `{"id":"1312831544-WandVideo-9f2c","status":"completed","created_at":1793801279,` +
		`"completed_at":1793801403,"data":[{"url":"https://aigc-output-video.cos.example/x_0.mp4?q-signature=abc"}],` +
		`"usage":{"output_tokens":200000,"output_video_tokens":200000,"total_tokens":200000,` +
		`"output_video_duration":4,"output_video_coefficient":1,"credit":57.14},"size":"768P"}`
	videoTaskFailRaw = `{"id":"t-fail","status":"failed","msg":"content blocked by safety checker"}`
)

func TestParseVideoSubmit(t *testing.T) {
	id, status := ParseVideoSubmit([]byte(videoSubmitRaw))
	if id != "1312831544-WandVideo-9f2c" {
		t.Fatalf("任务 id 应在顶层（不是 data.id）：%q", id)
	}
	if status != "queued" {
		t.Fatalf("status=%q want queued", status)
	}
	// 兼容个别网关形态的 task_id 字段
	if id, _ := ParseVideoSubmit([]byte(`{"task_id":"t-2","status":"pending"}`)); id != "t-2" {
		t.Fatalf("task_id 字段应兼容，got %q", id)
	}
	// 坏 JSON 不能 panic，也不能编 id
	if id, status := ParseVideoSubmit([]byte(`<html>`)); id != "" || status != "" {
		t.Fatalf("坏 JSON 应回空：%q %q", id, status)
	}
}

func TestParseVideoTaskDone(t *testing.T) {
	st := ParseVideoTask([]byte(videoTaskRaw))
	if st.Status != "completed" || !st.Done() || !st.Succeeded() {
		t.Fatalf("终态解析错：%+v", st)
	}
	if len(st.URLs) != 1 || st.URLs[0] != "https://aigc-output-video.cos.example/x_0.mp4?q-signature=abc" {
		t.Fatalf("成品 URL 解析错：%v", st.URLs)
	}
	if st.Credit != 57.14 || st.Tokens != 200000 || st.Duration != 4 || st.Size != "768P" {
		t.Fatalf("用量解析错：%+v", st)
	}
	if st.CompletedAt != 1793801403 {
		t.Fatalf("completed_at=%d", st.CompletedAt)
	}
}

func TestParseVideoTaskFailedAndRunning(t *testing.T) {
	fail := ParseVideoTask([]byte(videoTaskFailRaw))
	if !fail.Done() || fail.Succeeded() {
		t.Fatalf("failed 应 Done 但不算成功：%+v", fail)
	}
	if fail.ErrorText == "" {
		t.Fatalf("failed 应带出上游原因：%+v", fail)
	}
	running := ParseVideoTask([]byte(`{"id":"t","status":"running"}`))
	if running.Done() {
		t.Fatal("running 不是终态")
	}
	// 顶层 url 形态（宽容解析）
	alt := ParseVideoTask([]byte(`{"id":"t","status":"completed","video_url":"https://x/v.mp4"}`))
	if len(alt.URLs) != 1 || alt.URLs[0] != "https://x/v.mp4" {
		t.Fatalf("顶层 video_url 应识别：%v", alt.URLs)
	}
}

// TestGenerationModelsVideoRouteFilter 视频模型与图片同一条口径：只在实测有路由的域列出，
// 别的域不列（列出来等于引诱客户端选一个必然 400 的模型）。
func TestGenerationModelsVideoRouteFilter(t *testing.T) {
	c := New()
	c.stashGenerationModels("cn", []ModelInfo{
		{ID: "minimax-video-h3", Tags: []string{"text-to-video", "image-to-video"}},
		{ID: "kling-video-v3", Tags: []string{"text-to-video", "image-to-video"}},
		{ID: "hunyuan-image-alpha", Tags: []string{"text-to-image"}},
		{ID: "hy-image-3.5", Tags: []string{"text-to-image"}},
		{ID: "unknown-video-model", Tags: []string{"text-to-video"}},
	})
	cn := map[string]string{}
	for _, m := range c.GenerationModels("cn") {
		cn[m.ID] = m.GenerationKind
	}
	if cn["minimax-video-h3"] != "video" || cn["kling-video-v3"] != "video" {
		t.Fatalf("cn 应列出两个实测视频模型：%v", cn)
	}
	if cn["hunyuan-image-alpha"] != "image" {
		t.Fatalf("cn 的出图模型不能被视频逻辑弄丢：%v", cn)
	}
	if _, ok := cn["hy-image-3.5"]; ok {
		t.Fatalf("hy-image-3.5 只有 global 路由，不该出现在 cn：%v", cn)
	}
	if cn["unknown-video-model"] != "video" {
		t.Fatalf("未知视频模型照旧列出（不猜路由）：%v", cn)
	}

	c.stashGenerationModels("global", []ModelInfo{
		{ID: "minimax-video-h3", Tags: []string{"text-to-video"}},
		{ID: "hy-image-3.5", Tags: []string{"text-to-image"}},
	})
	// 2026-11-05 复测推翻旧前提：minimax-video-h3 在 global 池也有路由
	// （global 提交 4s 真出片，27.4 credit），所以 global 目录列出它是对的。
	global := map[string]string{}
	for _, m := range c.GenerationModels("global") {
		global[m.ID] = m.GenerationKind
	}
	if global["minimax-video-h3"] != "video" {
		t.Fatalf("global 也有 minimax 路由，应列出：%v", global)
	}
}
