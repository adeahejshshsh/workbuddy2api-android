package upstream

// 上游断流的截断终态（上游 84a48d9）。
//
// 现场：上游既没给 finish_reason、也没发 data: [DONE] 就 EOF（连接说到一半断了）。
// 此前 StreamHint 只补一个 [DONE] 就当成功结束：chat 客户端看到「内容 + 正常结束」但
// finish_reason 为空，responses 适配器更直接回 complete —— Agent 侧表现就是
// 「说到一半停了、也不报错」，只能人工接着催。现在补一帧 finish_reason=length，
// 由客户端自己决定续写/重试。

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// countFinish 数 body 里出现了几次非空 finish_reason。
func countFinish(body string) (n int, last string) {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		if i := strings.Index(payload, `"finish_reason":"`); i >= 0 {
			rest := payload[i+len(`"finish_reason":"`):]
			if j := strings.Index(rest, `"`); j >= 0 {
				v := rest[:j]
				if v != "" && v != "null" {
					n++
					last = v
				}
			}
		}
	}
	return n, last
}

// TestStreamInterruptedEmitsTruncationTerminal 断流必须补一帧 length，且恰好一帧。
func TestStreamInterruptedEmitsTruncationTerminal(t *testing.T) {
	// 只有内容帧，没有 [DONE]、没有 finish_reason，然后 EOF
	raw := `data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"说到一半"}}]}` + "\n\n"
	rec := httptest.NewRecorder()
	if err := StreamHint(rec, strings.NewReader(raw), nil); err != nil {
		t.Fatalf("StreamHint: %v", err)
	}
	n, last := countFinish(rec.Body.String())
	if n != 1 {
		t.Fatalf("应恰好补一帧终态，实际 %d 帧（body=%s）", n, rec.Body.String())
	}
	if last != TruncationFinishReason {
		t.Fatalf("终态取值=%q want %q（length 才能让客户端知道被截断）", last, TruncationFinishReason)
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatal("仍然必须有一个 [DONE] 收尾")
	}
}

// TestStreamNormalDoneNoExtraTerminal 正常收尾（上游给了 finish_reason + [DONE]）不得多补。
func TestStreamNormalDoneNoExtraTerminal(t *testing.T) {
	raw := `data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"完整"}}]}` + "\n\n" +
		`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	if err := StreamHint(rec, strings.NewReader(raw), nil); err != nil {
		t.Fatalf("StreamHint: %v", err)
	}
	n, last := countFinish(rec.Body.String())
	if n != 1 || last != "stop" {
		t.Fatalf("正常流不该多补终态：n=%d last=%q body=%s", n, last, rec.Body.String())
	}
}
