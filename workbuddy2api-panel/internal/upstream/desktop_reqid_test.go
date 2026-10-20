package upstream

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// bug P0-2 回归：同一 chunk 里先出现空 id（`"id":""`，错误帧/心跳形态）、后面才是
// 合法 requestId 时，必须能取到合法那个。此前命中空 id 只推进偏移就 continue，
// 下一轮开头先阻塞 Read —— 缓冲区里后面的合法 id 永远不看：流恰好结束就误报
// "SSE 中未找到服务端 requestId"，流没结束就卡到 HTTP 超时。
func TestDesktopChatWithExpertFindsIDAfterEmptyIDInSameChunk(t *testing.T) {
	const good = "0123456789abcdef0123456789abcdef" // 32 hex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// 一个 chunk 里塞：空 id 帧 → 不匹配的 id → 合法 id，然后立即结束
		_, _ = w.Write([]byte(`data: {"id":""}` + "\n\n" +
			`data: {"id":"msg-123"}` + "\n\n" +
			`data: {"id":"` + good + `"}` + "\n\n"))
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	conv, reqID, err := c.DesktopChatWithExpert(a, "ex_test")
	if err != nil {
		t.Fatalf("应当能在同一缓冲区里找到合法 requestId，实际报错: %v", err)
	}
	if reqID != good {
		t.Fatalf("requestId=%q want %q", reqID, good)
	}
	if conv == "" {
		t.Error("conversationID 不应为空")
	}
}

// 对照：缓冲里只有空 id / 不匹配的 id 时，仍应如实报"未找到"（不能假成功）。
func TestDesktopChatWithExpertNoValidID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`data: {"id":""}` + "\n\n" + `data: {"id":"msg-abc"}` + "\n\n"))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	_, _, err := c.DesktopChatWithExpert(&auth.Auth{UID: "u1", AccessToken: "at"}, "ex_test")
	if err == nil || !strings.Contains(err.Error(), "未找到") {
		t.Fatalf("want 未找到 requestId 错误，got %v", err)
	}
}
