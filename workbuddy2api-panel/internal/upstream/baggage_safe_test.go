package upstream

// baggage 头未清洗客户端输入：conversationId 来自**请求体**，客户端把格式化串塞进去
// （"abc\r\nX-Whatever: 1"）会让 net/http 在写头阶段报
// `invalid header field value for "Baggage"` → 传输错误分支 → handler 判「网络抖动」
// → NoteFailures → 单账号池连续 5 次即降权出池 10 分钟，期间全 503。
// 同函数族的另两个会话头早就套了 HeaderSafe，baggage 是第三处漏网。

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInjectBaggageStripsControlChars(t *testing.T) {
	c := New()
	req := httptest.NewRequest("POST", "https://example/v2/chat/completions", nil)
	c.injectBaggage(req, ChatMeta{
		ConversationID:        "abc\r\nX-Whatever: 1",
		ConversationRequestID: "req\x00id\n",
	})

	got := req.Header.Get("baggage")
	if strings.ContainsAny(got, "\r\n\x00") {
		t.Fatalf("baggage 里还有控制字符：%q", got)
	}
	if !strings.Contains(got, "codebuddy.session_id=abcX-Whatever: 1") {
		t.Fatalf("清洗后应保留可读内容：%q", got)
	}
	// 关键：整个请求头集合必须能通过 net/http 的写头校验。
	for k, vs := range req.Header {
		for _, v := range vs {
			if strings.ContainsAny(v, "\r\n") {
				t.Fatalf("头 %s 含换行：%q", k, v)
			}
		}
	}
}
