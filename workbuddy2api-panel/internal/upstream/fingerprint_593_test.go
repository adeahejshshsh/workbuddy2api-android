package upstream

// 5.9.3 客户端契约对齐（上游 2026-10-28 `ee1c80d`/`8ecf115`/`47f1ed6`，真机 182 条请求）。
//
// 这里钉住 6 处可离线判定的偏差：#1/#2 版本、#3 X-Private-Data、#4 X-Agent-Runtime、
// #5 baggage 形态、#6 不发 X-Machine-ID/X-Session-ID、#7 chat 不发浏览器语义头。

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestChatHeadersMatch593(t *testing.T) {
	c := New()
	req := httptest.NewRequest("POST", "https://example/v2/chat/completions", nil)
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	meta := ChatMeta{ConversationID: "conv-1", ConversationRequestID: "req-1"}
	c.ChatHeaders(req, a, "", meta)

	h := req.Header
	// ① 版本段
	if ua := h.Get("User-Agent"); !strings.Contains(ua, "/5.9.3") {
		t.Errorf("客户端版本应为 5.9.3: %q", ua)
	}
	// ② 桌面端本地 agent
	if got := h.Get("X-Agent-Runtime"); got != "local-agent" {
		t.Errorf("X-Agent-Runtime=%q want local-agent", got)
	}
	// ③ 客户端从不发这两个头（旧实现按 uid 派生塞进去 = 自造指纹）
	if v := h.Get("X-Machine-ID"); v != "" {
		t.Errorf("不该发 X-Machine-ID: %q", v)
	}
	if v := h.Get("X-Session-ID"); v != "" {
		t.Errorf("不该发 X-Session-ID: %q", v)
	}
	// ④ chat 无浏览器语义
	for _, k := range []string{"Origin", "Referer", "Accept-Language"} {
		if v := h.Get(k); v != "" {
			t.Errorf("chat 不该发 %s: %q", k, v)
		}
	}
	// ⑤ baggage：空 buddy 键保留 `k=` 形态，codebuddy.* 与同名头一致
	bg := h.Get("baggage")
	for _, want := range []string{
		"buddyId=,buddyName=,",
		"codebuddy.session_id=conv-1",
		"codebuddy.conversation_request_id=req-1",
		"x-agent-runtime=local-agent",
	} {
		if !strings.Contains(bg, want) {
			t.Errorf("baggage 缺 %q: %q", want, bg)
		}
	}
}
