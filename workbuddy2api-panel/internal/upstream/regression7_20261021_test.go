package upstream

import (
	"net/http"
	"testing"
)

// PR #159：content 数组里的 reasoning_text / reasoning 片段必须剔除
// （Codex 经 responses→chat 转换会把思考片段透传进来，上游 content 白名单不含它 → 400 11101）。
func TestDropReasoningContentParts(t *testing.T) {
	obj := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "reasoning_text", "text": "内部推理"},
				map[string]any{"type": "text", "text": "回答"},
			}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "reasoning", "text": "只剩思考"},
			}},
		},
	}
	dropReasoningContentParts(obj)
	msgs := obj["messages"].([]any)
	// 只剩思考片段的消息**保留**（报告 #6）：整条删掉会破坏 user/assistant 交替，
	// 上游可能直接 400，而日志里毫无痕迹。改为 content 置空串。
	if len(msgs) != 3 {
		t.Fatalf("消息条数应保持 3（不整条删）：得到 %d 条", len(msgs))
	}
	last := msgs[2].(map[string]any)
	if c, _ := last["content"].(string); c != "" {
		t.Fatalf("只剩思考片段的消息 content 应置空串: %v", last["content"])
	}
	parts := msgs[1].(map[string]any)["content"].([]any)
	if len(parts) != 1 {
		t.Fatalf("reasoning_text 没被剔除：%d 段", len(parts))
	}
	if ty := parts[0].(map[string]any)["type"]; ty != "text" {
		t.Fatalf("留下的不是 text 段：%v", ty)
	}
}

// PR #185：429 + queue.full 是渠道级容量满载，判 ErrUpstreamBusy（不罚号不轮转）。
func TestClassifyQueueFull(t *testing.T) {
	body := `{"code":6021,"msg":"queue.full.title: inference queue is full"}`
	if got := Classify(http.StatusTooManyRequests, body); got != ErrUpstreamBusy {
		t.Fatalf("429+queue.full 判成 %v，期望 ErrUpstreamBusy", got)
	}
	// 普通 429 仍按限流
	if got := Classify(http.StatusTooManyRequests, `{"msg":"rate limit exceeded"}`); got != ErrSoftRate {
		t.Fatalf("普通 429 判成 %v，期望 ErrSoftRate", got)
	}
}
