package panel

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A4（BUG-REVIEW-2026-10-21）：回读失败不能被判成「没进展」。
//
// 判据一旦判错，代价是 7 天：skipNote 每抖动一次记 1/3 次「连续无效」，
// 三次上游抖动就把好账号 + 可做任务砍出待办，靠人工回归几乎发现不了。
func TestQueueVerdictOf(t *testing.T) {
	readback := fmt.Errorf("%w: 502 bad gateway", errTaskReadback)
	cases := []struct {
		name       string
		err        error
		progressed bool
		want       queueVerdict
	}{
		{"有进展", nil, true, verdictProgress},
		{"没进展", nil, false, verdictNoProgress},
		{"动作失败", errors.New("boom"), false, verdictNoProgress},
		{"回读失败", readback, false, verdictReadbackFailed},
		{"回读失败但已确认有进展", readback, true, verdictProgress},
	}
	for _, c := range cases {
		if got := queueVerdictOf(c.err, c.progressed); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// A3：畸形 JSON 必须回 400，不许静默按默认值真的启动一轮扫描与执行。
func TestTasksRunQueueRejectsBadJSON(t *testing.T) {
	p := newTestPanel()
	req := httptest.NewRequest("POST", "/panel/api/tasks/run_queue", strings.NewReader(`{"concurrency":`))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400（body=%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid json") {
		t.Errorf("body=%s 希望说明 invalid json", rec.Body.String())
	}
}

// C2：静态资源必须带 no-cache + ETag，且条件请求回 304 空体。
//
// 为什么钉这条：app.js 与二进制同体、URL 又固定，浏览器启发式缓存会继续用旧 JS，
// 升级后表现为「新后端 + 旧前端」——新字段不显示、按钮报 400，用户以为后端坏了。
func TestStaticAssetCacheValidation(t *testing.T) {
	p := newTestPanel()
	etags := make(map[string]string, 2)
	for _, path := range []string{"/panel/", "/panel/app.js"} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s code=%d want 200", path, rec.Code)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("%s Cache-Control=%q want no-cache", path, got)
		}
		et := rec.Header().Get("ETag")
		if et == "" {
			t.Fatalf("%s 缺 ETag", path)
		}
		etags[path] = et

		rec2 := httptest.NewRecorder()
		req2 := httptest.NewRequest("GET", path, nil)
		req2.Header.Set("If-None-Match", et)
		p.ServeHTTP(rec2, req2)
		if rec2.Code != http.StatusNotModified {
			t.Fatalf("%s 条件请求 code=%d want 304", path, rec2.Code)
		}
		if rec2.Body.Len() != 0 {
			t.Errorf("%s 304 不许带 body（%d 字节）", path, rec2.Body.Len())
		}
	}
	if etags["/panel/"] == etags["/panel/app.js"] {
		t.Error("index 与 app.js 的 ETag 撞了：内容不同，指纹必须不同")
	}
}
