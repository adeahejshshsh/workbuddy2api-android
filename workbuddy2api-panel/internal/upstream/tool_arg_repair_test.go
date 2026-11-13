package upstream

import (
	"bufio"
	"encoding/json"
	"io"
	"math/rand"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func sseData(v any) string {
	b, _ := json.Marshal(v)
	return "data: " + string(b) + "\n\n"
}

func baseChunk(delta map[string]any, finish any) map[string]any {
	c := map[string]any{"index": float64(0), "delta": delta}
	if finish != nil {
		c["finish_reason"] = finish
	} else {
		c["finish_reason"] = nil
	}
	return map[string]any{
		"id": "chatcmpl-test", "object": "chat.completion.chunk", "created": 1, "model": "test",
		"choices": []any{c},
	}
}

func toolFrame(idx int, id, name, args string) map[string]any {
	return baseChunk(map[string]any{"tool_calls": []any{map[string]any{
		"index": float64(idx), "id": id, "type": "function",
		"function": map[string]any{"name": name, "arguments": args},
	}}}, nil)
}

func toolArgsFrame(idx int, args string) map[string]any {
	return baseChunk(map[string]any{"tool_calls": []any{map[string]any{
		"index": float64(idx), "function": map[string]any{"arguments": args},
	}}}, nil)
}

func collectFrames(t *testing.T, input string, enabled bool) []map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	var opts []StreamOption
	if enabled {
		opts = append(opts, WithToolArgRepair(true))
	}
	if err := StreamHint(rec, strings.NewReader(input), nil, opts...); err != nil {
		t.Fatalf("StreamHint: %v", err)
	}
	var out []map[string]any
	br := bufio.NewReader(strings.NewReader(rec.Body.String()))
	for {
		line, err := br.ReadString('\n')
		line = strings.TrimSpace(line)
		if line != "" {
			if strings.HasPrefix(line, "data: ") {
				p := strings.TrimPrefix(line, "data: ")
				if p != "[DONE]" {
					var obj map[string]any
					if json.Unmarshal([]byte(p), &obj) != nil {
						t.Fatalf("bad frame %q", p)
					}
					out = append(out, obj)
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

type gotCall struct {
	idx  int
	args string
}

func callsOf(t *testing.T, frames []map[string]any) []gotCall {
	t.Helper()
	var out []gotCall
	for _, f := range frames {
		chs, _ := f["choices"].([]any)
		for _, ci := range chs {
			c, _ := ci.(map[string]any)
			d, _ := c["delta"].(map[string]any)
			tcs, _ := d["tool_calls"].([]any)
			for _, tci := range tcs {
				tc, _ := tci.(map[string]any)
				fn, _ := tc["function"].(map[string]any)
				a, _ := fn["arguments"].(string)
				if a == "" {
					continue
				}
				idx := -1
				if v, ok := tc["index"].(float64); ok {
					idx = int(v)
				}
				out = append(out, gotCall{idx: idx, args: a})
			}
		}
	}
	return out
}

func finishPositions(frames []map[string]any) []int {
	var out []int
	for i, f := range frames {
		chs, _ := f["choices"].([]any)
		for _, ci := range chs {
			c, _ := ci.(map[string]any)
			if fr, _ := c["finish_reason"].(string); fr != "" {
				out = append(out, i)
			}
		}
	}
	return out
}

func TestRepairDeferredArgsStringInput(t *testing.T) {
	raw := `{"name":"WebFetch","input":"{\"url\":\"https://x\",\"prompt\":\"hi\"}"}`
	got, kind := repairDeferredArgs(raw)
	if kind != "string_input" {
		t.Fatalf("kind=%q want string_input", kind)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(got), &v); err != nil {
		t.Fatalf("repaired not JSON: %v (%s)", err, got)
	}
	in, ok := v["input"].(map[string]any)
	if !ok {
		t.Fatalf("input still %T: %v", v["input"], v["input"])
	}
	if in["url"] != "https://x" || in["prompt"] != "hi" {
		t.Fatalf("input=%v", in)
	}
}

func TestRepairDeferredArgsTruncated(t *testing.T) {
	raw := `{"name":"TodoWrite","input":{"todos":[{"content":"写测试","status":"completed","priority":"high"}]`
	got, kind := repairDeferredArgs(raw)
	if kind != "truncation" {
		t.Fatalf("kind=%q want truncation", kind)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(got), &v); err != nil {
		t.Fatalf("repaired not JSON: %v (%s)", err, got)
	}
	todos := v["input"].(map[string]any)["todos"].([]any)
	if len(todos) != 1 {
		t.Fatalf("todos=%d", len(todos))
	}
}

func TestRepairDeferredArgsNonWrapperUntouched(t *testing.T) {
	for _, raw := range []string{
		`{"foo":"bar"}`,
		`{"input":"{\"url\":\"https://x\"}"}`,
		`{"name":"","input":"{\"url\":\"https://x\"}"}`,
	} {
		got, kind := repairDeferredArgs(raw)
		if kind != "" || got != raw {
			t.Fatalf("non-wrapper touched: raw=%q kind=%q got=%q", raw, kind, got)
		}
	}
}

func TestRepairDeferredArgsInvalidUntouched(t *testing.T) {
	for _, raw := range []string{
		`{"name":"DeferredToolCall","input":`,
		`{"name":"DeferredToolCall","input":{"a":}}`,
		`not json`,
	} {
		got, kind := repairDeferredArgs(raw)
		if kind != "" || got != raw {
			t.Fatalf("invalid touched: raw=%q kind=%q got=%q", raw, kind, got)
		}
	}
}

func TestCloseTruncatedJSON(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{`{"a":1`, ``, false},
		{`{"a":2`, ``, false},
		{`{"a":[1,2`, ``, false},
		{`{"a":"b`, `{"a":"b"}`, true},
		{`{"a":2,`, ``, false},
		{`{"a":`, ``, false},
		{`{"a":1}}`, ``, false},
		{`{"a":"b\`, ``, false},
	}
	for _, tc := range cases {
		got, ok := closeTruncatedJSON(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Fatalf("close(%q)=(%q,%v) want (%q,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestStreamRepairsStringInputBeforeFinish(t *testing.T) {
	raw := `{"name":"WebFetch","input":"{\"url\":\"https://x\",\"prompt\":\"hi\"}"}`
	input := sseData(baseChunk(map[string]any{"role": "assistant"}, nil)) +
		sseData(toolFrame(0, "call_1", deferredToolName, "")) +
		sseData(toolArgsFrame(0, raw)) +
		sseData(baseChunk(map[string]any{}, "tool_calls")) +
		"data: [DONE]\n\n"
	frames := collectFrames(t, input, true)
	calls := callsOf(t, frames)
	if len(calls) != 1 {
		t.Fatalf("calls=%d want 1: %#v", len(calls), calls)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(calls[0].args), &v); err != nil {
		t.Fatalf("args not JSON: %v", err)
	}
	if _, ok := v["input"].(map[string]any); !ok {
		t.Fatalf("input not repaired: %T", v["input"])
	}
	fin := finishPositions(frames)
	if len(fin) != 1 {
		t.Fatalf("finish frames=%v", fin)
	}
	if calls[0].idx != 0 {
		t.Fatalf("index=%d", calls[0].idx)
	}
	// 参数帧必须排在 finish 之前。
	argPos := argsFramePos(frames)
	if argPos < 0 || argPos > fin[0] {
		t.Fatalf("args frame pos=%d finish=%d", argPos, fin[0])
	}
}

func TestStreamRepairsTruncatedBeforeFinish(t *testing.T) {
	raw := `{"name":"TodoWrite","input":{"todos":[{"content":"写测试","status":"completed","priority":"high"}]`
	input := sseData(toolFrame(0, "call_2", deferredToolName, "")) +
		sseData(toolArgsFrame(0, raw[:20])) +
		sseData(toolArgsFrame(0, raw[20:])) +
		sseData(baseChunk(map[string]any{}, "tool_calls")) +
		"data: [DONE]\n\n"
	frames := collectFrames(t, input, true)
	calls := callsOf(t, frames)
	if len(calls) != 1 {
		t.Fatalf("calls=%d", len(calls))
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(calls[0].args), &v); err != nil {
		t.Fatalf("args not JSON: %v (%s)", err, calls[0].args)
	}
}

func TestStreamRepairDisabledKeepsOriginal(t *testing.T) {
	raw := `{"name":"WebFetch","input":"{\"url\":\"https://x\",\"prompt\":\"hi\"}"}`
	input := sseData(toolFrame(0, "call_3", deferredToolName, "")) +
		sseData(toolArgsFrame(0, raw)) +
		sseData(baseChunk(map[string]any{}, "tool_calls")) +
		"data: [DONE]\n\n"
	frames := collectFrames(t, input, false)
	calls := callsOf(t, frames)
	if len(calls) != 1 || calls[0].args != raw {
		t.Fatalf("disabled changed args: %#v", calls)
	}
}

func TestStreamRepairsMultipleCalls(t *testing.T) {
	a := `{"name":"WebFetch","input":"{\"url\":\"https://a\"}"}`
	b := `{"name":"WebFetch","input":"{\"url\":\"https://b\"}"}`
	input := sseData(toolFrame(0, "call_a", deferredToolName, "")) +
		sseData(toolArgsFrame(0, a)) +
		sseData(toolFrame(1, "call_b", deferredToolName, "")) +
		sseData(toolArgsFrame(1, b)) +
		sseData(baseChunk(map[string]any{}, "tool_calls")) +
		"data: [DONE]\n\n"
	frames := collectFrames(t, input, true)
	calls := callsOf(t, frames)
	if len(calls) != 2 {
		t.Fatalf("calls=%d want 2", len(calls))
	}
	for _, c := range calls {
		var v map[string]any
		if err := json.Unmarshal([]byte(c.args), &v); err != nil {
			t.Fatalf("index %d not JSON: %v", c.idx, err)
		}
		if _, ok := v["input"].(map[string]any); !ok {
			t.Fatalf("index %d input not repaired", c.idx)
		}
	}
	if calls[0].idx != 0 || calls[1].idx != 1 {
		t.Fatalf("order=%v", calls)
	}
}

func TestStreamFlushesWithoutFinish(t *testing.T) {
	raw := `{"name":"WebFetch","input":"{\"url\":\"https://x\"}"}`
	input := sseData(toolFrame(0, "call_4", deferredToolName, "")) +
		sseData(toolArgsFrame(0, raw)) +
		"data: [DONE]\n\n"
	frames := collectFrames(t, input, true)
	calls := callsOf(t, frames)
	if len(calls) != 1 {
		t.Fatalf("calls=%d want 1", len(calls))
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(calls[0].args), &v); err != nil {
		t.Fatalf("args not JSON: %v", err)
	}
	if _, ok := v["input"].(map[string]any); !ok {
		t.Fatalf("input not repaired")
	}
}

func argsFramePos(frames []map[string]any) int {
	for i, f := range frames {
		chs, _ := f["choices"].([]any)
		for _, ci := range chs {
			c, _ := ci.(map[string]any)
			d, _ := c["delta"].(map[string]any)
			tcs, _ := d["tool_calls"].([]any)
			for _, tci := range tcs {
				tc, _ := tci.(map[string]any)
				fn, _ := tc["function"].(map[string]any)
				if a, _ := fn["arguments"].(string); a != "" {
					return i
				}
			}
		}
	}
	return -1
}

func TestStreamOversizedDeferredArgumentsPreserveBytes(t *testing.T) {
	prefix := strings.Repeat("x", 32)
	input := sseData(toolFrame(0, "call_big", deferredToolName, prefix)) +
		sseData(toolArgsFrame(0, strings.Repeat("y", maxRepairArgs))) +
		sseData(toolArgsFrame(0, "tail")) +
		sseData(baseChunk(map[string]any{}, "tool_calls")) + "data: [DONE]\n\n"
	var got strings.Builder
	for _, call := range callsOf(t, collectFrames(t, input, true)) {
		got.WriteString(call.args)
	}
	want := prefix + strings.Repeat("y", maxRepairArgs) + "tail"
	if got.String() != want {
		t.Fatalf("oversized args changed: got len=%d want len=%d", got.Len(), len(want))
	}
}

func TestStreamUsageOnlyBeforeArgumentsDoesNotFinalize(t *testing.T) {
	raw := `{"name":"WebFetch","input":"{\"url\":\"https://x\"}"}`
	usage := map[string]any{"choices": []any{}, "usage": map[string]any{"total_tokens": 8}}
	input := sseData(toolFrame(0, "call_usage", deferredToolName, "")) +
		sseData(usage) + sseData(toolArgsFrame(0, raw)) +
		sseData(baseChunk(map[string]any{}, "tool_calls"))
	calls := callsOf(t, collectFrames(t, input, true))
	if len(calls) != 1 || !strings.Contains(calls[0].args, `"input":{`) {
		t.Fatalf("usage-only frame broke repair: %#v", calls)
	}
}

func TestStreamFirstFragmentExceedingLimitPreservesWholeCall(t *testing.T) {
	args := strings.Repeat("z", maxRepairArgs+1)
	input := sseData(toolFrame(0, "call_big_first", deferredToolName, args)) +
		sseData(baseChunk(map[string]any{}, "tool_calls"))
	calls := callsOf(t, collectFrames(t, input, true))
	if len(calls) != 1 || calls[0].args != args {
		t.Fatalf("first fragment duplicated or lost: len=%d", len(calls))
	}
}

func TestStreamDeferredArgumentsEOFWithoutDone(t *testing.T) {
	raw := `{"name":"WebFetch","input":"{\"url\":\"https://x\"}"}`
	input := sseData(toolFrame(0, "call_eof", deferredToolName, "")) + sseData(toolArgsFrame(0, raw))
	calls := callsOf(t, collectFrames(t, input, true))
	if len(calls) != 1 {
		t.Fatalf("EOF calls=%d want 1", len(calls))
	}
	var obj map[string]any
	if json.Unmarshal([]byte(calls[0].args), &obj) != nil {
		t.Fatalf("EOF args not JSON: %q", calls[0].args)
	}
	if _, ok := obj["input"].(map[string]any); !ok {
		t.Fatalf("EOF input not repaired: %#v", obj)
	}
}

func TestCloseTruncatedJSONRandomInputsNeverPanic(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 10000; i++ {
		raw := make([]byte, rng.Intn(100))
		for j := range raw {
			raw[j] = byte(rng.Intn(256))
		}
		got, ok := closeTruncatedJSON(string(raw))
		if ok && !utf8.Valid(raw) {
			t.Fatalf("invalid UTF-8 was rewritten for %q: %q", raw, got)
		}
		if ok && utf8.Valid(raw) {
			var repaired any
			if json.Unmarshal([]byte(got), &repaired) == nil {
				t.Fatalf("ambiguous truncation accepted for %q: %q", raw, got)
			}
		}
	}
}

func TestRepairDeferredArgsTruncatedNumberStaysUnchanged(t *testing.T) {
	raw := `{"name":"WebFetch","input":{"max_results":10`
	got, kind := repairDeferredArgs(raw)
	if kind != "" || got != raw {
		t.Fatalf("truncated number was guessed: kind=%q got=%q", kind, got)
	}
}

func TestStreamRepairIndependentInstances(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			raw := `{"name":"WebFetch","input":"{\"url\":\"https://x\"}"}`
			input := sseData(toolFrame(0, "call_parallel", deferredToolName, "")) +
				sseData(toolArgsFrame(0, raw)) + sseData(baseChunk(map[string]any{}, "tool_calls"))
			calls := callsOf(t, collectFrames(t, input, true))
			if len(calls) != 1 || !strings.Contains(calls[0].args, `"input":{`) {
				t.Errorf("parallel repair=%#v", calls)
			}
		}()
	}
	wg.Wait()
}

func TestStreamLeavesOtherToolUntouched(t *testing.T) {
	raw := `{"name":"WebFetch","input":"{\"url\":\"https://x\"}"}`
	input := sseData(toolFrame(0, "call_5", "WebFetch", raw)) +
		sseData(baseChunk(map[string]any{}, "tool_calls")) +
		"data: [DONE]\n\n"
	frames := collectFrames(t, input, true)
	calls := callsOf(t, frames)
	if len(calls) != 1 || calls[0].args != raw {
		t.Fatalf("other tool touched: %#v", calls)
	}
}
