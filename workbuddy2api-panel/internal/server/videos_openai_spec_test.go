// OpenAI Videos 资源形状（2026-09-24 关停，但形状仍在各 SDK 里）：字段名、类型、
// 状态四态，以及 /v1/videos 与 /v1/videos/{id}/content 两个标准路径。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 状态归一 + 类型：seconds 必须是字符串、error 必须是对象、progress 必须有。
func TestVideoPayloadMatchesOpenAISpec(t *testing.T) {
	cases := []struct {
		state  upstream.VideoTaskState
		status string
	}{
		{upstream.VideoTaskState{Status: "queued"}, "queued"},
		{upstream.VideoTaskState{Status: "running"}, "in_progress"},
		{upstream.VideoTaskState{Status: "succeeded"}, "completed"},
		{upstream.VideoTaskState{Status: "completed"}, "completed"},
		{upstream.VideoTaskState{Status: "failed", ErrorText: "内容审核未通过"}, "failed"},
	}
	for _, c := range cases {
		got := videoTaskPayload("t1", c.state, "cn:minimax-video-h3", &videoTaskStore{})
		if got["status"] != c.status {
			t.Fatalf("state=%q status=%v want %s", c.state.Status, got["status"], c.status)
		}
		if got["object"] != "video" {
			t.Fatalf("object=%v want video", got["object"])
		}
		if _, ok := got["progress"].(int); !ok {
			t.Fatalf("progress 必须是数字，实际 %#v", got["progress"])
		}
		if got["remixed_from_video_id"] != nil || got["expires_at"] != nil {
			t.Fatalf("未知字段要给 null，实际 %#v", got)
		}
	}
	// 出片：seconds 是字符串、progress=100、error 是 null
	done := videoTaskPayload("t2", upstream.VideoTaskState{
		Status: "completed", Duration: 4, Size: "768P", Credit: 57.14,
		URLs: []string{"https://cos.example/v.mp4"},
	}, "cn:minimax-video-h3", &videoTaskStore{})
	if s, ok := done["seconds"].(string); !ok || s != "4" {
		t.Fatalf("seconds=%#v want \"4\"（OpenAI 定义成字符串）", done["seconds"])
	}
	if done["progress"] != 100 || done["error"] != nil {
		t.Fatalf("progress=%v error=%v want 100/nil", done["progress"], done["error"])
	}
	// 失败：error 必须是对象（{code,message}），不能是裸字符串
	failed := videoTaskPayload("t3", upstream.VideoTaskState{Status: "failed", ErrorText: "boom"}, "", &videoTaskStore{})
	e, ok := failed["error"].(map[string]any)
	if !ok || e["message"] != "boom" {
		t.Fatalf("error=%#v want {code,message}", failed["error"])
	}
}

// POST /v1/videos 走 OpenAI 入参：seconds 是字符串、size 是 WxH、参考图在 input_reference。
func TestVideoOpenAIPathAcceptsSpecBody(t *testing.T) {
	fastVideoPoll(t)
	f := &videoFake{on: func(c videoCall) (int, string) {
		switch c.Path {
		case "/v2/videos/generations":
			return 200, `{"code":0,"data":{"id":"task-1","status":"queued"}}`
		case "/v2/videos/tasks":
			return 200, `{"code":0,"data":{"id":"task-1","status":"completed",` +
				`"video_url":"https://cos.example/v.mp4","usage":{"credit":57.14}}}`
		}
		return 404, videoNotMine
	}}
	h := NewHandler(Config{Pool: probePool(2, ""), Upstream: f.client()})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/videos", strings.NewReader(
		`{"model":"cn:minimax-video-h3","prompt":"一只猫","seconds":"4","size":"1280x720",`+
			`"input_reference":{"image_url":"https://img.example/a.png"},"wait":true}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["object"] != "video" || out["status"] != "completed" {
		t.Fatalf("回执不是 Video 资源：%s", rec.Body)
	}
	var sent map[string]any
	for _, c := range f.snapshot() {
		if c.Path == "/v2/videos/generations" {
			_ = json.Unmarshal([]byte(c.Body), &sent)
		}
	}
	if sec, ok := sent["seconds"].(float64); !ok || sec != 4 {
		t.Fatalf("上游收到的 seconds=%#v want 数字 4", sent["seconds"])
	}
	if sent["image_url"] != "https://img.example/a.png" {
		t.Fatalf("input_reference 没转成 image_url：%#v", sent["image_url"])
	}
	if _, bad := sent["size"]; bad {
		t.Fatalf("size 不该下发给上游：%#v", sent)
	}
	ep, _ := sent["extra_parameters"].(map[string]any)
	if ep["aspect_ratio"] != "16:9" {
		t.Fatalf("extra_parameters=%#v want aspect_ratio 16:9", sent["extra_parameters"])
	}
}

// GET /v1/videos/{id}/content 取字节；没出片回 409，台账里没有回 404。
func TestVideoContentEndpoint(t *testing.T) {
	mp4 := []byte("\x00\x00\x00\x18ftypmp42fake-bytes")
	allowMediaLoopback(t)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write(mp4)
	}))
	defer cdn.Close()

	h := NewHandler(Config{Pool: probePool(1, ""), Upstream: (&videoFake{on: func(videoCall) (int, string) { return 200, videoTaskRunning }}).client()})
	h.videoLedger.put("done-1", "a01", "cn", "minimax-video-h3")
	h.videoLedger.noteState("done-1", upstream.VideoTaskState{Status: "completed", URLs: []string{cdn.URL + "/v.mp4"}})
	h.videoLedger.put("run-1", "a01", "cn", "minimax-video-h3")
	h.videoLedger.noteState("run-1", upstream.VideoTaskState{Status: "running"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/videos/done-1/content", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != string(mp4) {
		t.Fatalf("code=%d body=%q want 200 + 原样字节", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "video/mp4" {
		t.Fatalf("Content-Type=%q", ct)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/videos/run-1/content", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("没出片 code=%d want 409 body=%s", rec.Code, rec.Body)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/videos/ghost/content", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知任务 code=%d want 404 body=%s", rec.Code, rec.Body)
	}
}

// GET /v1/videos/{id} 仍是查任务（不被 /content 那条吃掉）。
func TestVideoTaskPathUnaffectedByContent(t *testing.T) {
	f := &videoFake{on: func(c videoCall) (int, string) {
		return 200, `{"code":0,"data":{"id":"task-9","status":"running"}}`
	}}
	h := NewHandler(Config{Pool: probePool(1, ""), Upstream: f.client()})
	h.videoLedger.put("task-9", "a01", "cn", "minimax-video-h3")

	rec := getVideo(t, h, "task-9")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"id":"task-9"`) {
		t.Fatalf("code=%d body=%s want 200 + 任务资源（不是 /content 的 409/404）", rec.Code, rec.Body)
	}
}
