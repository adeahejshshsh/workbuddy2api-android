package server

import (
	"encoding/json"
	"testing"
)

func TestResponsesMergesAssistantTextWithToolCalls(t *testing.T) {
	body, _, _, err := responsesToChat([]byte(`{"model":"kimi-k3.1-pro","input":[
		{"role":"user","content":"fix tests"},
		{"type":"reasoning","summary":"inspect"},
		{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"running tests"}]},
		{"type":"reasoning","summary":"run both checks"},
		{"type":"function_call","call_id":"c1","name":"check","arguments":"{}"},
		{"type":"function_call","call_id":"c2","name":"check","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"failed"},
		{"type":"function_call_output","call_id":"c2","output":"ok"},
		{"type":"function_call","call_id":"c3","name":"check","arguments":"{}"},
		{"type":"function_call_output","call_id":"c3","output":"fixed"},
		{"type":"message","role":"assistant","content":"done"}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	var chat struct{ Messages []map[string]any }
	if err := json.Unmarshal(body, &chat); err != nil {
		t.Fatal(err)
	}
	msgs := chat.Messages
	if len(msgs) != 7 {
		t.Fatalf("expected 7 messages with text and calls on the same assistant turn, got %v", msgs)
	}
	m := msgs[1]
	if m["role"] != "assistant" || m["content"] != "running tests" || m["reasoning_content"] != "inspect\nrun both checks" {
		t.Fatalf("assistant history changed: %v", m)
	}
	calls, _ := m["tool_calls"].([]any)
	if len(calls) != 2 {
		t.Fatalf("expected two parallel calls: %v", m)
	}
	for i, id := range []string{"c1", "c2"} {
		if calls[i].(map[string]any)["id"] != id || msgs[i+2]["tool_call_id"] != id {
			t.Fatalf("call/result pair %s changed: %v", id, msgs)
		}
	}
	if role, _, call := reasoningTestCall(t, msgs[4]); role != "assistant" || call != "c3" || msgs[5]["tool_call_id"] != "c3" || msgs[6]["content"] != "done" {
		t.Fatalf("later assistant turns changed: %v", msgs[4:])
	}
}

func TestResponsesToolHistoryDoesNotMergeAcrossTurnBoundary(t *testing.T) {
	for _, tc := range []struct{ name, prefix string }{
		{"final answer", `{"type":"message","role":"assistant","phase":"final_answer","content":"done"}`},
		{"opaque compaction", `{"role":"assistant","content":"done"},{"type":"compaction","encrypted_content":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var items []any
			if err := json.Unmarshal([]byte(`[`+tc.prefix+`,{"type":"function_call","call_id":"c1","name":"check","arguments":"{}"}]`), &items); err != nil {
				t.Fatal(err)
			}
			out := convertInputItems(items, nil)
			if len(out) != 2 || out[0].(map[string]any)["tool_calls"] != nil {
				t.Fatalf("merged calls across %s: %v", tc.name, out)
			}
		})
	}
}
