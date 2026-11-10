package server

import (
	"bytes"
	"log"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 复刻真实上游帧序（抓自线上）：正文 → 带 name 的首个 tool_call 帧 → 若干只带
// arguments 的参数分片 → finish_reason=tool_calls → [DONE]。
//
// 这里钉的是**告警不能误报**：收尾会被调用两次（finish_reason 与 [DONE]），已经
// 发出去的工具调用第二次扫描必然命中「closed」分支。曾经那条分支连带把 closed 也
// 报成「上游给了工具调用但缺函数名」，于是每条请求都打一行假警告——排障时会让人
// 以为工具调用天天被丢。同理，参数分片本身不带 name 是正常协议形态。
const realToolFrames = `data: {"id":"c1","model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"content":"我将读取 /etc/hostname。"}}]}` + "\n\n" +
	`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_00_x","type":"function","function":{"name":"read_file","arguments":""}}]}}]}` + "\n\n" +
	`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"arguments":"{\"path\""}}]}}]}` + "\n\n" +
	`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"arguments":": \"/etc"}}]}}]}` + "\n\n" +
	`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"arguments":"/hostname\"}"}}]}}]}` + "\n\n" +
	`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":100,"completion_tokens":40,"total_tokens":140}}` + "\n\n" +
	"data: [DONE]\n\n"

func TestResponsesRealToolFramesNoPhantomWarn(t *testing.T) {
	up, _ := fakeContinueUpstream(t, func(call int) (int, string) { return 200, realToolFrames })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	body := `{"model":"deepseek-v4.1-flash","stream":true,"input":"读 /etc/hostname",` +
		`"tools":[{"type":"function","name":"read_file","parameters":{"type":"object"}}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))

	s := rec.Body.String()
	if !strings.Contains(s, `"type":"function_call"`) {
		t.Fatalf("应下发 function_call:\n%s", s)
	}
	if got := buf.String(); strings.Contains(got, "缺函数名") {
		t.Fatalf("真实帧序不该触发误报，日志:\n%s", got)
	}
}
