package upstream

// 静默回合（issue #44）：上游 200 + 完整流，但整条流只有 role / finish_reason，
// 既没有正文也没有工具调用。配合 WithHoldUntilContent 时客户端一个字节都收不到，
// 调用方因此可以换号或改参数重发 —— 对客户端完全透明。

import (
	"net/http/httptest"
	"strings"
	"testing"
)

const silentStopStream = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"x\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":0,\"total_tokens\":1}}\n\n" +
	"data: [DONE]\n\n"

const toolCallStream = "data: {\"id\":\"c2\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"do_it\",\"arguments\":\"{}\"}}]},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"c2\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"x\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
	"data: [DONE]\n\n"

// TestStreamHintSilentTurnWritesNothingWhenHolding ⭐ 核心：扣帧生效时，静默回合
// 一个字节都不写、不 flush，并返回 IsSilentEmptyTurn 供调用方重发。
func TestStreamHintSilentTurnWritesNothingWhenHolding(t *testing.T) {
	rec := httptest.NewRecorder()
	err := StreamHint(rec, strings.NewReader(silentStopStream), nil, WithHoldUntilContent())
	if err == nil {
		t.Fatal("want errSilentEmptyTurn")
	}
	if !IsSilentEmptyTurn(err) {
		t.Fatalf("want IsSilentEmptyTurn, got %v", err)
	}
	// 静默回合不是「上游断流」也不是「空流」：两者处置不同（断流要罚号、空流报 502）。
	if IsUpstreamReadError(err) || IsEmptyStreamError(err) {
		t.Fatalf("静默回合不该混进断流/空流口径: %v", err)
	}
	if got := rec.Body.String(); got != "" {
		t.Fatalf("扣帧生效时一个字节都不该写: %q", got)
	}
	if rec.Flushed {
		t.Fatal("不得 flush：一旦 flush 响应头就发出，重发就不可能了")
	}
}

// 不扣帧时保持既有行为：静默回合照样把 role/finish/[DONE] 透传出去（200 假成功）。
// 这条钉子防的是「扣帧逻辑泄漏到默认路径」。
func TestStreamHintSilentTurnWithoutHoldKeepsLegacyBehavior(t *testing.T) {
	rec := httptest.NewRecorder()
	err := StreamHint(rec, strings.NewReader(silentStopStream), nil)
	if err != nil {
		t.Fatalf("不扣帧时静默回合不是错误: %v", err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"role":"assistant"`) {
		t.Fatalf("role 帧应透传: %q", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("finish 帧应透传: %q", out)
	}
	if !strings.HasSuffix(strings.TrimRight(out, "\n"), "data: [DONE]") {
		t.Fatalf("应以 [DONE] 收尾: %q", out)
	}
}

// 扣住的帧在见到正文时必须整批刷出（含先前扣下的 role 帧），且顺序不变 ——
// 否则正常流的客户端会丢 role，或收到乱序帧。
func TestStreamHintHoldFlushesBufferedFramesOnContent(t *testing.T) {
	raw := "data: {\"id\":\"c3\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c3\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c3\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"x\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	if err := StreamHint(rec, strings.NewReader(raw), nil, WithHoldUntilContent()); err != nil {
		t.Fatalf("正常流不该报错: %v", err)
	}
	out := rec.Body.String()
	roleAt := strings.Index(out, `"role":"assistant"`)
	contentAt := strings.Index(out, `"content":"hi"`)
	if roleAt < 0 || contentAt < 0 {
		t.Fatalf("扣住的 role 帧或正文帧丢了: %q", out)
	}
	if roleAt > contentAt {
		t.Fatalf("帧顺序被打乱（role 应在正文之前）: %q", out)
	}
}

// 只有工具调用的流同样算「有内容」：必须刷出，且不得被判成静默回合。
func TestStreamHintHoldToolCallsAreContent(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := StreamHint(rec, strings.NewReader(toolCallStream), nil, WithHoldUntilContent()); err != nil {
		t.Fatalf("带工具调用的流不该报错: %v", err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"tool_calls"`) {
		t.Fatalf("工具调用帧丢了: %q", out)
	}
	if !strings.HasSuffix(strings.TrimRight(out, "\n"), "data: [DONE]") {
		t.Fatalf("应以 [DONE] 收尾: %q", out)
	}
}

// 思维链帧必须**立刻**写出去，不能被扣住：思考型模型先吐几十 KB 的
// reasoning_content，扣住会让客户端的「思考中」先卡顿再一次性倾倒 —— 这是每条
// 正常请求都会吃到的体验回退。代价是「纯思考后 stop」这种罕见形态救不回来。
func TestStreamHintHoldLetsReasoningThroughImmediately(t *testing.T) {
	raw := "data: {\"id\":\"c4\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c4\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"想一想\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c4\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"x\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	if err := StreamHint(rec, strings.NewReader(raw), nil, WithHoldUntilContent()); err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "reasoning_content") {
		t.Fatalf("思维链帧被吞了: %q", out)
	}
	if strings.Contains(out, "errSilentEmptyTurn") {
		t.Fatal("不该判成静默回合")
	}
}
