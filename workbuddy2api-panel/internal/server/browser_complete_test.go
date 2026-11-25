package server

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const browserCompleteAgentBody = `{"model":"cn:deepseek-v4.1-flash",` +
	`"tools":[{"type":"function","function":{"name":"view_image","parameters":{"type":"object"}}}],` +
	`"messages":[{"role":"user","content":"继续处理浏览器任务"},` +
	`{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"view_image","arguments":"{}"}}]},` +
	`{"role":"tool","tool_call_id":"c1","content":"ok"}]}`

const browserCompleteMarker = `::browser-complete{title="Adjusted three disputed answers"}`

const segBrowserComplete = `data: {"id":"b1","choices":[{"index":0,"delta":{"content":"::browser-complete{title=\"Adjusted three disputed answers\"}"}}]}` + "\n\n" +
	`data: {"id":"b1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":24000,"completion_tokens":20,"total_tokens":24020}}` + "\n\n" +
	"data: [DONE]\n\n"

const segBrowserCompleteContinue = `data: {"id":"b2","choices":[{"index":0,"delta":{"content":"任务还没完成，正在继续处理剩余内容。"}}]}` + "\n\n" +
	`data: {"id":"b2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":24020,"completion_tokens":12,"total_tokens":24032}}` + "\n\n" +
	"data: [DONE]\n\n"

func newBrowserCompleteReader(t *testing.T, up *upstream.Client, seg string) *continueReader {
	t.Helper()
	return newContinueReaderWithFloor(context.Background(), up,
		&auth.Auth{UID: "u-browser", AccessToken: "at", ExpiresAt: 9999999999},
		[]byte(browserCompleteAgentBody), "127.0.0.1", upstream.ChatMeta{},
		io.NopCloser(strings.NewReader(seg)), 8192)
}

func TestBrowserCompleteDirectiveOnly(t *testing.T) {
	if !browserCompleteDirectiveOnly(" \n" + browserCompleteMarker + "\r\n") {
		t.Fatal("standalone browser-complete marker should match")
	}
	for _, text := range []string{
		"说明一下 " + browserCompleteMarker,
		browserCompleteMarker + " 后面还有正文",
		"::browser-completed{title=\"x\"}",
		"::browser-complete{title=\"x\"",
	} {
		if browserCompleteDirectiveOnly(text) {
			t.Fatalf("non-standalone marker matched: %q", text)
		}
	}
}

func TestBrowserCompleteMarkerTriggersContinuation(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(int) (int, string) {
		return 200, segBrowserCompleteContinue
	})
	r := newBrowserCompleteReader(t, up, segBrowserComplete)
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("continuation requests=%d want 1", len(*reqs))
	}
	s := string(out)
	if strings.Contains(s, browserCompleteMarker) {
		t.Fatalf("browser-complete marker leaked to client:\n%s", s)
	}
	if !strings.Contains(s, "任务还没完成") {
		t.Fatalf("continuation text missing:\n%s", s)
	}
	body := reqBody(t, (*reqs)[0])
	msgs, _ := body["messages"].([]any)
	if len(msgs) < 2 {
		t.Fatalf("continuation messages=%d", len(msgs))
	}
	last, _ := msgs[len(msgs)-1].(map[string]any)
	if text, _ := last["content"].(string); !strings.Contains(text, "browser-complete") {
		t.Fatalf("continuation nudge does not identify browser marker: %q", text)
	}
}

func TestBrowserCompleteMarkerSplitAcrossDeltas(t *testing.T) {
	seg := `data: {"id":"b1","choices":[{"index":0,"delta":{"content":"::browser-"}}]}` + "\n\n" +
		`data: {"id":"b1","choices":[{"index":0,"delta":{"content":"complete{title=\"split\"}"}}]}` + "\n\n" +
		`data: {"id":"b1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(int) (int, string) {
		return 200, segBrowserCompleteContinue
	})
	r := newBrowserCompleteReader(t, up, seg)
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("split marker continuation requests=%d want 1", len(*reqs))
	}
	if strings.Contains(string(out), "::browser-") {
		t.Fatalf("split marker leaked to client:\n%s", string(out))
	}
}

func TestBrowserCompleteAggregateOnly(t *testing.T) {
	resp := map[string]any{
		"choices": []any{map[string]any{
			"finish_reason": "stop",
			"message":       map[string]any{"content": browserCompleteMarker},
		}},
	}
	if got := browserCompleteAggregateText(resp); got != browserCompleteMarker {
		t.Fatalf("aggregate marker=%q want %q", got, browserCompleteMarker)
	}
	resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"] = "已完成。" + browserCompleteMarker
	if got := browserCompleteAggregateText(resp); got != "" {
		t.Fatalf("non-standalone aggregate text must stay untouched: %q", got)
	}
}

func TestBrowserCompleteRetryBody(t *testing.T) {
	resp := map[string]any{
		"choices": []any{map[string]any{
			"finish_reason": "stop",
			"message":       map[string]any{"content": browserCompleteMarker, "reasoning_content": "check"},
		}},
	}
	body, ok := browserCompleteRetryBody([]byte(browserCompleteAgentBody), resp, 8192)
	if !ok {
		t.Fatal("retry body was not built")
	}
	got := reqBody(t, string(body))
	msgs, _ := got["messages"].([]any)
	if len(msgs) < 2 {
		t.Fatalf("retry messages=%d", len(msgs))
	}
	asst, _ := msgs[len(msgs)-2].(map[string]any)
	if text, _ := asst["content"].(string); text != browserCompleteMarker {
		t.Fatalf("assistant marker prefix=%q", text)
	}
	if asst["reasoning_content"] != "check" {
		t.Fatalf("reasoning lost: %#v", asst)
	}
	last, _ := msgs[len(msgs)-1].(map[string]any)
	if text, _ := last["content"].(string); !strings.Contains(text, "browser-complete") {
		t.Fatalf("retry nudge does not identify marker: %q", text)
	}
}
