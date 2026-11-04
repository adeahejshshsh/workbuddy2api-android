package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// 回归：上游 `: heartbeat` 注释行与下一个 data 帧粘连时， Responses 适配器
// 曾把整帧丢掉——GLM 的工具调用首帧恰好携带 id+name，被吞后整个调用消失、
// completed 的 output 序列化成 null，客户端对 null 做 .map 直接崩
// （实测 "Cannot read properties of null (reading 'map')"）。
// 修两层：continueReader 补帧分隔（本测试喂的是修复后的形态），
// responsesWriter 透传注释帧并把空 output 落成 []。
func TestResponsesStreamHeartbeatKeepsToolCall(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &responsesWriter{ResponseWriter: rec, stream: true}
	rw.Header().Set("Content-Type", "text/event-stream")
	frames := []string{
		": heartbeat\n\n",
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"REPL","arguments":"{\"code\":"}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}}]}` + "\n\n",
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
	// 注释帧必须透传：它是上游/本层的保活，吞掉 = 客户端可见静默 → idle 超时掐线。
	if !strings.Contains(body, ": heartbeat\n\n") {
		t.Fatal("heartbeat 注释帧未透传（客户端会看到静默）")
	}
	types := sseTypes(body)
	for _, n := range []string{"response.output_item.added", "response.function_call_arguments.delta", "response.output_item.done", "response.completed"} {
		if !containsStr(types, n) {
			t.Fatalf("missing %s in %v", n, types)
		}
	}
	if !strings.Contains(body, `"name":"REPL"`) {
		t.Fatalf("工具调用丢失: %s", body)
	}
	if strings.Contains(body, `"output":null`) {
		t.Fatal("completed.output 不能是 null")
	}
}

// 空响应（上游只有 finish=stop、无任何内容增量）也必须 output=[] 而不是 null。
func TestResponsesStreamEmptyOutputNotNull(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &responsesWriter{ResponseWriter: rec, stream: true}
	rw.Header().Set("Content-Type", "text/event-stream")
	frames := []string{
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":"stop"}]}` + "\n\n",
		"data: [DONE]\n\n",
	}
	for _, f := range frames {
		if _, err := rw.Write([]byte(f)); err != nil {
			t.Fatal(err)
		}
	}
	rw.finish()
	body := rec.Body.String()
	if strings.Contains(body, `"output":null`) {
		t.Fatalf("completed.output 是 null: %s", body)
	}
	if !strings.Contains(body, `"output":[]`) {
		t.Fatalf("completed.output 应为 []: %s", body)
	}
}
