package upstream

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// 上游 workbuddy.cn 的注释行后**不发空行**，直接跟 data 帧：
//
//	: heartbeat\ndata: {"choices":[{"delta":{"tool_calls":[{"id":"call_x",...
//
// StreamHint 会吞掉上游的分隔空行、自产 "\n\n" 只覆盖 data 帧，所以注释行必须自己补回
// 分隔——否则下游按 "\n\n" 切帧会拿到 ": heartbeat\ndata: {...}" 粘连块，没有 "data: "
// 前缀，整帧被丢。被丢的恰是 GLM 工具调用的首帧（带 id 与 function.name），整个调用随之
// 消失（见 PR #158）。PR #158 的回归测试只喂 responsesWriter，这一层没有覆盖，这里补上。
func TestStreamHintSeparatesCommentFromNextFrame(t *testing.T) {
	raw := ": heartbeat\n" +
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n" +
		"data: [DONE]\n\n"

	rec := httptest.NewRecorder()
	if err := StreamHint(rec, strings.NewReader(raw), nil); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()

	if strings.Contains(body, ": heartbeat\ndata: ") {
		t.Fatalf("注释行与 data 帧粘连（下游会整帧丢弃）: %q", body)
	}
	if !strings.Contains(body, ": heartbeat\n\n") {
		t.Fatalf("注释行后缺帧分隔空行: %q", body)
	}
	// 粘连的下游表现就是「工具调用首帧消失」：名字必须仍在透出内容里。
	if !strings.Contains(body, `"name":"get_weather"`) {
		t.Fatalf("工具调用首帧丢失: %q", body)
	}
}
