// narration_nudge_test.go 救援指令的选型与「已写完」出口。
package server

import (
	"encoding/json"
	"io"
	"testing"
)

// 截断的调用标记、未闭合代码块也没有标点，但不是承诺句：仍用「停在预告上、把剩下的写完」
// 那条指令（说成「你承诺的动作没执行」是答非所问）。
func TestNarrationStructurallyOpenTailKeepsLeadInNudge(t *testing.T) {
	for _, text := range []string{
		"我先跑一下：\n<｜｜DSML｜｜ invoke name=\"bash\">\n<｜｜DSML｜｜ parameter name=\"cmd\">go test ./... 2>&1</｜｜DSML｜｜ parameter>",
		"先写一个最小复现：\n```go\nfunc main() {",
	} {
		raw, _ := json.Marshal(text)
		seg := `data: {"id":"o1","choices":[{"index":0,"delta":{"content":` + string(raw) + `}}]}` + "\n\n" +
			`data: {"id":"o1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":268538,"completion_tokens":80,"total_tokens":268618}}` + "\n\n" +
			"data: [DONE]\n\n"
		up, reqs := fakeContinueUpstream(t, func(int) (int, string) { return 200, segToolOnlyText })
		if _, err := io.ReadAll(newNarrateReader(t, up, agentBody, seg)); err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(*reqs) != 1 {
			t.Fatalf("结构未闭合应救一次（%q），实际 %d 次", text, len(*reqs))
		}
		msgs, _ := reqBody(t, (*reqs)[0])["messages"].([]any)
		last, _ := msgs[len(msgs)-1].(map[string]any)
		if got, _ := last["content"].(string); got != narrationNudge {
			t.Errorf("结构未闭合（%q）应带 narrationNudge，实际 %q", text, got)
		}
	}
}

// 模型按旧文案的出口回「已完成」（或 toolOnly 的 DONE）：那是显式声明已写完，不再救。
// 否则短正文（<400B，长度闸门不管）会再发一次整段上下文——请求体几乎一字不差。
func TestNarrationAckStopsRescueChain(t *testing.T) {
	text := "我将开始修改 internal/verify/check.go"
	if len(text) > narrationMaxBytes {
		t.Fatalf("前提失败：样本应是短正文，实际 %d 字节", len(text))
	}
	raw, _ := json.Marshal(text)
	seg := `data: {"id":"n9","choices":[{"index":0,"delta":{"content":` + string(raw) + `}}]}` + "\n\n" +
		`data: {"id":"n9","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":20000,"completion_tokens":20,"total_tokens":20020}}` + "\n\n" +
		"data: [DONE]\n\n"
	// 救援段只回「已完成」：模型按出口声明写完了。
	ack := `data: {"id":"n10","choices":[{"index":0,"delta":{"content":"已完成"}}]}` + "\n\n" +
		`data: {"id":"n10","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":20000,"completion_tokens":3,"total_tokens":20003}}` + "\n\n" +
		"data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(int) (int, string) { return 200, ack })
	if _, err := io.ReadAll(newNarrateReader(t, up, agentBody, seg)); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("识别到「已完成」后不该再救，实际发了 %d 次上游请求", len(*reqs))
	}
}
