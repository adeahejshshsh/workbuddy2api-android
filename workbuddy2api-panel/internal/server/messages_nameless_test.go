package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMessagesNamelessToolUseNotOpened 无名调用不是合法 tool_use：块一旦开出，
// content_block_start 里的 name:"" 就定死了，客户端拿到也派发不了。与 responses 侧
// closeOpenItems 的同名守卫一致——连块都不开（此前 messages 侧照开，泄漏 name:""）。
func TestMessagesNamelessToolUseNotOpened(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &messagesWriter{ResponseWriter: rec, stream: true}
	rw.Header().Set("Content-Type", "text/event-stream")
	frames := []string{
		// 只有 id/arguments、没有 function.name 的调用帧（上游残缺形态）。
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"arguments":"{\"a\":1}"}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n",
		"data: [DONE]\n\n",
	}
	for _, f := range frames {
		if _, err := rw.Write([]byte(f)); err != nil {
			t.Fatal(err)
		}
	}
	rw.finish()
	body := rec.Body.String()

	if strings.Contains(body, `"type":"tool_use"`) {
		t.Fatalf("无名调用不应开出 tool_use 块: %s", body)
	}
	if strings.Contains(body, `"name":""`) {
		t.Fatalf("不应发出 name 为空的块: %s", body)
	}
	// 正常收尾不能因此消失（回合仍要终止）。
	if !strings.Contains(body, `"type":"message_stop"`) {
		t.Fatalf("缺少 message_stop: %s", body)
	}
}
