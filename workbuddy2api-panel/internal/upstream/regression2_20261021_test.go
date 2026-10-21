package upstream

import (
	"strings"
	"testing"
)

// bug 5：上游既没发 [DONE]、也没给过 finish_reason 时，是**真截断**，
// 聚合结果不能报 finish_reason="stop"（客户端会把半截答案当完整结果落库）。
func TestAggregateMarksTruncationWithoutDoneOrFinish(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"半截答"}}]}`,
		``,
	}, "\n")
	got, err := Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatal(err)
	}
	ch := got["choices"].([]any)[0].(map[string]any)
	if fr, _ := ch["finish_reason"].(string); fr != "length" {
		t.Fatalf("finish_reason = %q，期望 length（无 [DONE] 且无 finish_reason）", fr)
	}
}

// 反向用例：上游**给过** finish_reason="stop"、只是没发 [DONE]（WorkBuddy 常态），
// 不能误标成截断 —— 否则每条正常回复都会被客户端当成不完整。
func TestAggregateKeepsExplicitFinishWithoutDone(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"完整"}}]}`,
		`data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		``,
	}, "\n")
	got, err := Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatal(err)
	}
	ch := got["choices"].([]any)[0].(map[string]any)
	if fr, _ := ch["finish_reason"].(string); fr != "stop" {
		t.Fatalf("finish_reason = %q，期望 stop（上游显式给过，不该被误标截断）", fr)
	}
}
