package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 同步等待出片期间客户端断开：轮询要跟着停，不能空转到 timeout_seconds 才罢手
// （每 5 秒还会再问一次上游，同一账号被密集查询会触发 429）。任务已提交，留在台账里，
// 之后仍可用 task_id 查询。REST 与 MCP 两条入口都要跟随客户端的请求 ctx。
func TestVideoSyncWaitStopsWhenClientGone(t *testing.T) {
	for _, entry := range []string{"rest", "mcp"} {
		t.Run(entry, func(t *testing.T) {
			fastVideoPoll(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f := &videoFake{on: func(c videoCall) (int, string) {
				switch c.Path {
				case "/v2/videos/generations":
					return 200, videoSubmitOK
				case "/v2/videos/tasks":
					cancel() // 第一次轮询时客户端断开
					return 200, videoTaskRunning
				}
				return 404, `{"code":404,"msg":"unexpected"}`
			}}
			h := NewHandler(Config{Pool: imagePool(), Upstream: f.client()})

			req := httptest.NewRequest("POST", "/v1/videos/generations",
				strings.NewReader(`{"prompt":"一只猫","seconds":4,"wait":true,"timeout_seconds":60}`))
			if entry == "mcp" {
				req = httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call",`+
					`"params":{"name":"generate_video","arguments":{"prompt":"一只猫","seconds":4,"wait":true,"timeout_seconds":60}}}`))
			}
			req = req.WithContext(ctx)

			done := make(chan struct{})
			go func() {
				h.ServeHTTP(httptest.NewRecorder(), req)
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("客户端断开后同步等待没有停下")
			}
			polls := f.count("/v2/videos/tasks")
			time.Sleep(50 * time.Millisecond)
			if got := f.count("/v2/videos/tasks"); got != polls {
				t.Fatalf("handler 返回后仍在轮询上游：%d → %d", polls, got)
			}
			if _, ok := h.videoLedger.get(videoTestTaskID); !ok {
				t.Fatal("任务已提交，应留在台账里供之后查询")
			}
		})
	}
}
