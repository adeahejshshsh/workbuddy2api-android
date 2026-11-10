package server

import (
	"bytes"
	"log"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 上游给了 tool_calls 分片但从不带 function.name：Responses 适配器无法拼出合法
// function_call item（会丢），这种情况下**必须留一条 WARN**——否则客户端看到的是
// 「模型说完一句就停」，而网关日志一片干净，排障时完全看不出工具调用被丢了。
func TestResponsesLogsNamelessToolCallDrop(t *testing.T) {
	up, _ := fakeContinueUpstream(t, func(call int) (int, string) {
		return 200, `data: {"id":"r1","choices":[{"index":0,"delta":{"content":"先看完整表单。"}}]}` + "\n\n" +
			`data: {"id":"r1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"/x\"}"}}]}}]}` + "\n\n" +
			`data: {"id":"r1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
			"data: [DONE]\n\n"
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	body := `{"model":"deepseek-v4.1-flash","stream":true,"input":"看看表单",` +
		`"tools":[{"type":"function","name":"read_file","parameters":{"type":"object"}}]}`

	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))

	got := buf.String()
	if !strings.Contains(got, "上游给了工具调用但缺函数名") {
		t.Fatalf("缺函数名时应有 WARN，实际日志:\n%s", got)
	}
	if !strings.Contains(rec.Body.String(), "先看完整表单。") {
		t.Fatalf("正文应照常下发:\n%s", rec.Body.String())
	}
}
