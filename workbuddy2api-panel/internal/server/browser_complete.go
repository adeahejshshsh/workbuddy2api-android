package server

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// browserCompletePrefix is the client control directive that a Codex browser
// turn can emit as assistant text. It is not a normal final answer: forwarding a
// standalone directive to the client ends the browser turn before the task is
// actually complete.
const browserCompletePrefix = "::browser-complete"

const browserCompleteNudge = "你刚才只输出了 `::browser-complete{...}` 浏览器完成控制标记，但用户任务还没有结束。不要只输出完成标记，也不要假装已经完成；请继续执行用户要求的下一步，完成后给出最终结果。"

// browserCompleteDirectiveOnly reports whether text is exactly one standalone
// browser-complete control directive. The title may contain text, but no other
// assistant prose is accepted.
func browserCompleteDirectiveOnly(text string) bool {
	s := strings.TrimSpace(text)
	if !strings.HasPrefix(s, browserCompletePrefix+"{") {
		return false
	}
	rest := s[len(browserCompletePrefix):]
	end := strings.IndexByte(rest, '}')
	return end >= 0 && strings.TrimSpace(rest[end+1:]) == ""
}

// filterBrowserContent buffers only a possible leading browser-complete
// directive. Once the response has any non-whitespace prose, later directives
// are left untouched. Returned visible text is what the client may receive;
// hidden marks that the input was consumed as a candidate control directive.
func (r *continueReader) filterBrowserContent(delta string) (visible string, hidden bool) {
	if r.browserCompleteSeen ||
		(r.browserHold == "" && strings.TrimSpace(r.text.String()) != "") {
		return delta, false
	}

	s := r.browserHold + delta
	r.browserHold = ""
	core := strings.TrimLeft(s, " \t\r\n")
	if core == "" {
		r.browserHold = s
		return "", true
	}

	if !strings.HasPrefix(browserCompletePrefix, core) &&
		!strings.HasPrefix(core, browserCompletePrefix) {
		return s, false
	}
	if !strings.HasPrefix(core, browserCompletePrefix) {
		r.browserHold = s
		return "", true
	}

	rest := core[len(browserCompletePrefix):]
	if rest == "" {
		r.browserHold = s
		return "", true
	}
	if !strings.HasPrefix(rest, "{") {
		// The prefix was only a false positive, e.g. ::browser-completed.
		return s, false
	}
	end := strings.IndexByte(rest, '}')
	if end < 0 {
		r.browserHold = s
		return "", true
	}

	r.browserCompleteSeen = true
	tail := rest[end+1:]
	if strings.TrimSpace(tail) == "" {
		return "", true
	}
	return tail, true
}

// flushBrowserHold emits an incomplete candidate verbatim. This keeps the
// filtering layer byte-preserving when a stream ends halfway through a possible
// directive.
func (r *continueReader) flushBrowserHold() {
	text := r.browserHold
	r.browserHold = ""
	if text == "" {
		return
	}
	if strings.TrimSpace(text) == "" {
		return
	}
	r.browserIncomplete = true
	payload, err := json.Marshal(map[string]any{
		"id":     r.id,
		"object": "chat.completion.chunk",
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{"content": text},
			"finish_reason": nil,
		}},
	})
	if err == nil {
		r.emit(string(payload))
	}
}

// tryBrowserCompleteRescue turns a standalone browser-complete control message
// into one continuation request. The control marker remains in r.text so the
// model sees exactly what it emitted, but it is never sent to the client.
func (r *continueReader) tryBrowserCompleteRescue() bool {
	switch {
	case r.browserCompleteTried:
		return r.skip("browser_complete", "tried")
	case !r.browserCompleteSeen:
		return r.skip("browser_complete", "not_seen")
	case r.errSeen:
		return r.skip("browser_complete", "err_frame")
	case r.toolSeen:
		return r.skip("browser_complete", "tool_seen")
	case r.clientGone():
		return r.skip("browser_complete", "client_gone")
	case r.outputCapped():
		return r.skip("browser_complete", "output_capped")
	case r.finish != "stop":
		return r.skip("browser_complete", "finish_not_stop")
	case !browserCompleteDirectiveOnly(r.text.String()):
		return r.skip("browser_complete", "not_standalone")
	case r.atSegmentCap():
		return r.skip("browser_complete", "segment_cap")
	}

	declared, toolTurn := bodyToolShape(r.body)
	if !declared && !toolTurn {
		return r.skip("browser_complete", "no_tool_context")
	}
	body, err := r.continuationBodyWithNudge(browserCompleteNudge)
	if err != nil {
		return r.skip("browser_complete", "body_build_failed")
	}

	r.browserCompleteTried = true
	noteRescue("browser_complete")
	return r.openSegmentAsync(switchBrowserComplete, body, func(rc io.ReadCloser) {
		r.seg++
		r.cur, r.br = rc, bufio.NewReaderSize(rc, 64*1024)
		r.finish, r.tail = "", nil
		r.segCompletion, r.segThink = 0, 0
		r.browserIncomplete = false
		log.Printf("[browser-complete] acct=%s model=%s seg=%d 检测到独立浏览器完成控制标记 → 请求模型继续真正任务",
			logfmt.Label(r.acct.UID, r.acct.Nickname), bareModelOf(r.body), r.seg)
	}, func(status int, terr error) {
		log.Printf("WARN: [browser-complete] acct=%s seg=%d 续写请求失败（status=%d err=%v）→ 保留上游原始响应",
			logfmt.Label(r.acct.UID, r.acct.Nickname), r.seg+1, status, terr)
	})
}

// browserCompleteAggregateText returns the standalone browser-complete text
// from a normal (non-streaming) completion. A response with real tool calls or
// any other content is deliberately left untouched.
func browserCompleteAggregateText(resp map[string]any) string {
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		return ""
	}
	c, _ := choices[0].(map[string]any)
	if c == nil || asString(c["finish_reason"]) != "stop" {
		return ""
	}
	msg, _ := c["message"].(map[string]any)
	if msg == nil {
		return ""
	}
	if tcs, _ := msg["tool_calls"].([]any); len(tcs) > 0 {
		return ""
	}
	text, _ := msg["content"].(string)
	if text == "" || !browserCompleteDirectiveOnly(text) {
		return ""
	}
	return text
}

// browserCompleteRetryBody appends the model's control message plus a clear
// continuation instruction. It is used by the non-streaming aggregate path,
// where continueReader is not in the call chain.
func browserCompleteRetryBody(body []byte, resp map[string]any, budgetFloor int64) ([]byte, bool) {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return nil, false
	}
	msgs, _ := obj["messages"].([]any)
	asst := map[string]any{"role": "assistant", "content": browserCompleteDirectiveOnlyText(resp)}
	if rc := browserCompleteAggregateReasoning(resp); rc != "" {
		asst["reasoning_content"] = rc
	}
	msgs = append(msgs, asst, map[string]any{"role": "user", "content": internalNudgeText(browserCompleteNudge)})
	obj["messages"] = msgs
	upstream.EnsureMinBudget(obj, budgetFloor)
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return out, true
}

func browserCompleteDirectiveOnlyText(resp map[string]any) string {
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		return ""
	}
	c, _ := choices[0].(map[string]any)
	msg, _ := c["message"].(map[string]any)
	if msg == nil {
		return ""
	}
	text, _ := msg["content"].(string)
	return text
}

func browserCompleteAggregateReasoning(resp map[string]any) string {
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		return ""
	}
	c, _ := choices[0].(map[string]any)
	msg, _ := c["message"].(map[string]any)
	if msg == nil {
		return ""
	}
	return asString(msg["reasoning_content"])
}
