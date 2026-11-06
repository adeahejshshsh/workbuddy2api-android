package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// allowMediaLoopback 让 127.0.0.1 的 httptest 服务器通过产物域白名单（生产里只认 .myqcloud.com）。
func allowMediaLoopback(t *testing.T) {
	t.Helper()
	old := watermarkHostSuffixes
	watermarkHostSuffixes = []string{"127.0.0.1"}
	t.Cleanup(func() { watermarkHostSuffixes = old })
}

// setMediaLimits 把字节上限压小，免得真造几十 MB 的响应。
func setMediaLimits(t *testing.T, video, image int64) {
	t.Helper()
	ov, oi := videoContentMax, mediaFetchMax
	videoContentMax, mediaFetchMax = video, image
	t.Cleanup(func() { videoContentMax, mediaFetchMax = ov, oi })
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(io.Discard) })
	return &buf
}

// chunkedBody 写 n 段、每段 size 字节并 Flush：不带 Content-Length（分块传输，长度未知）。
func chunkedBody(w http.ResponseWriter, n, size int) {
	for i := 0; i < n; i++ {
		_, _ = w.Write(bytes.Repeat([]byte("v"), size))
		w.(http.Flusher).Flush()
	}
}

// —— 图片 response_format=b64_json（走完整的 POST /v1/images/generations） ——

// postB64Image 让上游回一条指向 url 的出图回执，返回网关对 b64_json 请求的回复里的第一项。
func postB64Image(ctx context.Context, t *testing.T, url string) map[string]any {
	t.Helper()
	f := &imageFake{status: 200, body: `{"code":0,"msg":"OK","data":{"created":1,"data":[{"url":"` + url +
		`"}],"usage":{"credit":5.71}}}`}
	h := NewHandler(Config{Pool: imagePool(), Upstream: f.client()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/images/generations",
		strings.NewReader(`{"model":"cn:hunyuan-image-alpha","prompt":"一只猫","response_format":"b64_json"}`)).WithContext(ctx))
	var obj struct {
		Data []map[string]any `json:"data"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &obj) != nil || len(obj.Data) != 1 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	return obj.Data[0]
}

// wantURLKept 取图失败时图已出、已扣费：保留 url，不给 b64_json。
func wantURLKept(t *testing.T, it map[string]any) {
	t.Helper()
	if it["url"] == nil || it["b64_json"] != nil {
		t.Fatalf("应保留 url、不给 b64_json：%v", it)
	}
}

// 直链主机不在上游产物域：不回读（服务端主动发请求必须收口），保留 url。
func TestB64JSONRefusesNonProductHost(t *testing.T) {
	var hits atomic.Int32
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("png"))
	}))
	defer cdn.Close()
	wantURLKept(t, postB64Image(context.Background(), t, cdn.URL+"/i.png"))
	if hits.Load() != 0 {
		t.Fatalf("白名单外的主机不该被访问，got %d 次", hits.Load())
	}
}

// 重定向不跟随：白名单只校验首跳主机，跟随一次就能被 302 带去任意地址。
func TestB64JSONDoesNotFollowRedirect(t *testing.T) {
	allowMediaLoopback(t)
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("secret"))
	}))
	defer target.Close()
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/x", http.StatusFound)
	}))
	defer cdn.Close()
	wantURLKept(t, postB64Image(context.Background(), t, cdn.URL+"/i.png"))
	if hits.Load() != 0 {
		t.Fatalf("重定向目标不该被访问，got %d 次", hits.Load())
	}
}

// 客户端断连（请求 ctx 取消）要中止取图：上游出站连接随之关闭，不替走掉的人把图拉完。
func TestB64JSONCanceledByRequestContext(t *testing.T) {
	allowMediaLoopback(t)
	started, gone, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
			close(gone)
		case <-release:
		}
	}))
	defer cdn.Close()
	defer close(release) // 先于 cdn.Close 执行：失败时别让 Close 等着挂起的处理器

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { postB64Image(ctx, t, cdn.URL+"/i.png"); close(done) }()
	<-started
	cancel()
	for _, c := range []chan struct{}{gone, done} {
		select {
		case <-c:
		case <-time.After(3 * time.Second):
			t.Fatal("请求 ctx 取消后取图没有中止")
		}
	}
}

// 超过上限不能静默截断成一段损坏的 base64：保留 url，并把原因写进日志。
// 长度已知（Content-Length）与未知（分块）两种都要拦住。
func TestB64JSONKeepsURLOverLimit(t *testing.T) {
	allowMediaLoopback(t)
	setMediaLimits(t, 1<<20, 16)
	cases := map[string]http.HandlerFunc{
		"known":   func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(bytes.Repeat([]byte("p"), 64)) },
		"chunked": func(w http.ResponseWriter, r *http.Request) { chunkedBody(w, 2, 32) },
	}
	for name, hd := range cases {
		t.Run(name, func(t *testing.T) {
			buf := captureLog(t)
			cdn := httptest.NewServer(hd)
			defer cdn.Close()
			wantURLKept(t, postB64Image(context.Background(), t, cdn.URL+"/i.png"))
			if !strings.Contains(buf.String(), "上限") {
				t.Fatalf("应把超限原因写进日志：%q", buf.String())
			}
		})
	}
}

// Responses 的 image_generation 工具是 fetchMedia 的第二个调用方，同样只认产物域。
func TestImageToolRefusesNonProductHost(t *testing.T) {
	var hits atomic.Int32
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("png"))
	}))
	defer cdn.Close()
	h := newImageToolHandler(&imageToolFake{cdn: cdn.URL})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","input":"画一张图","stream":false,"tools":[{"type":"image_generation"}]}`)))
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "image_generation_call") {
		t.Fatalf("code=%d 白名单外的直链不该产出图片项：%s", rec.Code, rec.Body)
	}
	if hits.Load() != 0 {
		t.Fatalf("白名单外的主机不该被访问，got %d 次", hits.Load())
	}
}

// —— 视频 /content ——

// contentRig 一台假的成品 CDN + 一个把任务查询指向它的网关。
type contentRig struct {
	h   *Handler
	f   *videoFake
	cdn *httptest.Server
}

func videoDoneBody(url string) string {
	return `{"code":0,"msg":"OK","data":{"id":"` + videoTestTaskID + `","status":"completed",` +
		`"created_at":1793801279,"completed_at":1793801403,"data":[{"url":"` + url + `"}],` +
		`"usage":{"output_video_duration":4,"output_video_tokens":200000,"total_tokens":200000,` +
		`"output_video_coefficient":1,"credit":57.14},"size":"768P"}}`
}

// newContentRig 台账里已有任务 videoTestTaskID（账号 a01，只有提交时的 queued 快照），
// 上游查询回「已出片」，直链指向 cdnHandler。
func newContentRig(t *testing.T, cdnHandler http.HandlerFunc) *contentRig {
	t.Helper()
	allowMediaLoopback(t)
	rig := &contentRig{cdn: httptest.NewServer(cdnHandler)}
	t.Cleanup(rig.cdn.Close)
	rig.f = &videoFake{on: func(c videoCall) (int, string) {
		if c.Path == "/v2/videos/tasks" {
			return 200, videoDoneBody(rig.cdn.URL + "/v.mp4?sig=x")
		}
		return 404, videoNotMine
	}}
	rig.h = NewHandler(Config{Pool: probePool(1, ""), Upstream: rig.f.client()})
	rig.h.videoLedger.put(videoTestTaskID, "a01", "cn", "minimax-video-h3")
	rig.h.videoLedger.noteState(videoTestTaskID, videoTaskStateFromSubmit("queued"))
	return rig
}

// finish 把内存快照直接置为已出片：与「是否问所属账号」无关的用例用它，免得被快照新旧牵着走。
func (r *contentRig) finish() {
	r.h.videoLedger.noteState(videoTestTaskID, upstream.VideoTaskState{
		ID: videoTestTaskID, Status: "completed", URLs: []string{r.cdn.URL + "/v.mp4?sig=x"},
	})
}

func serveBytes(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = io.WriteString(w, body)
	}
}

func getContent(h *Handler, id string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/videos/"+id+"/content", nil))
	return rec
}

// 内存快照停在提交时的 queued（或重启后根本没有）：/content 要像查询一样问一次所属账号，
// 而不是把上游早已出片的任务当成「没出片」回 409。首次出片的记账仍只发生一次。
func TestVideoContentQueriesOwnerWhenSnapshotStale(t *testing.T) {
	rig := newContentRig(t, serveBytes("MP4DATA"))

	rec := getContent(rig.h, videoTestTaskID)
	if rec.Code != 200 || rec.Body.String() != "MP4DATA" {
		t.Fatalf("code=%d body=%q want 200 + 成品字节", rec.Code, rec.Body)
	}
	// 快照已更新：再取字节、再查任务都不再打上游，也不重复记账。
	if rec := getContent(rig.h, videoTestTaskID); rec.Code != 200 {
		t.Fatalf("第二次取字节 code=%d", rec.Code)
	}
	if rec := getVideo(t, rig.h, videoTestTaskID); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"credit":57.14`) {
		t.Fatalf("查任务 code=%d body=%s", rec.Code, rec.Body)
	}
	if n := rig.f.count("/v2/videos/tasks"); n != 1 {
		t.Fatalf("应只问一次所属账号，got %d", n)
	}
	st, _ := rig.h.cfg.Pool.Status("a01")
	if len(st.ModelCosts) != 1 || st.ModelCosts[0].Samples != 1 {
		t.Fatalf("首次出片应恰好记一次账：%+v", st.ModelCosts)
	}
}

// 还没出片回 409，不回字节；台账里没有的 id 回 404 且不触发盲查上游。
func TestVideoContentNotReadyAndUnknown(t *testing.T) {
	rig := newContentRig(t, serveBytes("MP4DATA"))
	rig.f.on = func(c videoCall) (int, string) { return 200, videoTaskRunning }
	if rec := getContent(rig.h, videoTestTaskID); rec.Code != http.StatusConflict || strings.Contains(rec.Body.String(), "MP4DATA") {
		t.Fatalf("code=%d body=%s want 409", rec.Code, rec.Body)
	}
	before := len(rig.f.snapshot())
	if rec := getContent(rig.h, "no-such-task"); rec.Code != http.StatusNotFound {
		t.Fatalf("未知 id code=%d want 404", rec.Code)
	}
	if n := len(rig.f.snapshot()); n != before {
		t.Fatalf("未知 id 不该探测上游，多了 %d 次调用", n-before)
	}
}

// 成品长度已知且超限：写响应头之前就失败，不发 Content-Length、不吐一截字节。
func TestVideoContentOverLimitKnownLengthFailsBeforeHeaders(t *testing.T) {
	setMediaLimits(t, 16, 1<<20)
	rig := newContentRig(t, serveBytes(strings.Repeat("v", 64)))
	rig.finish()
	rec := getContent(rig.h, videoTestTaskID)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d want 502 body=%s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "vvvv") || rec.Header().Get("Content-Type") == "video/mp4" {
		t.Fatalf("不该吐出任何成品字节：%q", rec.Body)
	}
}

// 长度未知（分块）又超限：只能中途掐断连接，让客户端读到错误而不是一个被截短的 mp4。
func TestVideoContentOverLimitUnknownLengthAborts(t *testing.T) {
	setMediaLimits(t, 16, 1<<20)
	rig := newContentRig(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		chunkedBody(w, 2, 32)
	})
	rig.finish()
	srv := httptest.NewServer(rig.h)
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(srv.URL + "/v1/videos/" + videoTestTaskID + "/content")
	if err == nil {
		defer resp.Body.Close()
		if resp.Header.Get("Content-Length") != "" {
			t.Fatalf("长度未知时不该发 Content-Length：%q", resp.Header.Get("Content-Length"))
		}
		if _, err = io.ReadAll(resp.Body); err == nil {
			t.Fatal("超限的未知长度成品必须以错误收尾，实际 EOF 正常")
		}
	}
	// 必须是网关主动掐断，而不是服务端一声不吭、客户端等到超时。
	if ne, ok := err.(interface{ Timeout() bool }); ok && ne.Timeout() {
		t.Fatalf("应被网关掐断连接，实际是客户端超时：%v", err)
	}
}

// HEAD 只要响应头：不能把整段成品从直链拉一遍再丢掉。直链在吐完头之后一直卡着，GET 会等它，
// HEAD 必须立刻返回。
func TestVideoContentHeadDoesNotReadBody(t *testing.T) {
	rig := newContentRig(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", "1048576")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	rig.finish()
	srv := httptest.NewServer(rig.h)
	defer srv.Close()

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Head(srv.URL + "/v1/videos/" + videoTestTaskID + "/content")
	if err != nil {
		t.Fatalf("HEAD 应立刻回头部，实际：%v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Length") != "1048576" {
		t.Fatalf("HEAD code=%d content-length=%q", resp.StatusCode, resp.Header.Get("Content-Length"))
	}
}

// 与图片回读同一套出站收口：白名单外的主机、重定向都不去，回 502。
func TestVideoContentRefusesOffHostAndRedirect(t *testing.T) {
	t.Run("off-host", func(t *testing.T) {
		var hits atomic.Int32
		rig := newContentRig(t, func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			_, _ = w.Write([]byte("x"))
		})
		rig.finish()
		watermarkHostSuffixes = []string{".myqcloud.com"} // 撤销 rig 放开的回环白名单（Cleanup 会还原）
		if rec := getContent(rig.h, videoTestTaskID); rec.Code != http.StatusBadGateway || hits.Load() != 0 {
			t.Fatalf("code=%d hits=%d want 502 且不访问", rec.Code, hits.Load())
		}
	})
	t.Run("redirect", func(t *testing.T) {
		var hits atomic.Int32
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			_, _ = w.Write([]byte("secret"))
		}))
		defer target.Close()
		rig := newContentRig(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/x", http.StatusFound)
		})
		rig.finish()
		rec := getContent(rig.h, videoTestTaskID)
		if rec.Code != http.StatusBadGateway || hits.Load() != 0 || strings.Contains(rec.Body.String(), "secret") {
			t.Fatalf("code=%d hits=%d want 502 且不跟随", rec.Code, hits.Load())
		}
	})
}

// 签名直链里的 q-signature 不该出现在回执里（也不进日志）：连不上直链时的错误文本不带 URL。
func TestVideoContentErrorOmitsSignedURL(t *testing.T) {
	rig := newContentRig(t, serveBytes("x"))
	rig.finish()
	rig.cdn.Close() // 直链连不上
	rec := getContent(rig.h, videoTestTaskID)
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "sig=x") {
		t.Fatalf("code=%d 回执不该带签名直链：%s", rec.Code, rec.Body)
	}
}

// 上游把任务判失败：/content 回 409 说明已失败，不去拉直链。
func TestVideoContentFailedTaskIs409(t *testing.T) {
	rig := newContentRig(t, serveBytes("x"))
	rig.f.on = func(c videoCall) (int, string) { return 200, videoTaskFailed }
	rec := getContent(rig.h, videoTestTaskID)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "失败") {
		t.Fatalf("code=%d body=%s want 409 + 已失败", rec.Code, rec.Body)
	}
}
