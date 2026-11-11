package server

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// DSML 是 DeepSeek 把工具调用写成文本时的标记。线上实测（DSH 客户端，2026-11-10
// 21:30 起 40 分钟）：76 次 narration 触发里 25 次尾部是这种标记，典型
//
//	"…</｜｜DSML｜｜ invoke> </｜｜DSML｜｜ calls>"
//
// 这段文本本身就是一次工具调用，客户端会解析并执行；网关若当成「话没说完」补发
// 续写指令，模型会再发一次调用，用户看到「任务该结束了却一直在工具调用」。
func TestHasToolCallMarkup(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"…</｜｜DSML｜｜ invoke> </｜｜DSML｜｜ calls>", true},
		{"执行中</｜｜DSML｜｜ parameter>", true},
		{"<|DSML|>invoke", true},
		{"<function_calls><invoke name=\"read_file\"></function_calls>", true},
		{"antml:invoke name=\"x\"", true},
		{"普通的回答，以冒号结尾：", false},
		{"含盐量 < 5g 的饮食建议。", false}, // 有 '<' 但不是标记
		{"a | b 位运算说明。", false},    // 有 '|' 但不是标记
		{"", false},
	}
	for _, c := range cases {
		if got := hasToolCallMarkup(c.text); got != c.want {
			t.Errorf("hasToolCallMarkup(%q)=%v want %v", c.text, got, c.want)
		}
	}
}

// 尾部是闭合的 DSML 调用块：narration 必须放弃（否则会造出第二次调用）。
func TestNarrationTailSkipsToolCallMarkup(t *testing.T) {
	for _, s := range []string{
		"正在执行 </｜｜DSML｜｜ invoke> </｜｜DSML｜｜ calls>",
		"结果如下：<function_calls><invoke name=\"read_file\">",
	} {
		if narrationTailSuspends(s) {
			t.Errorf("含工具调用标记的正文不该判为「话没说完」：%q", s)
		}
	}
	// 对照：同样以 '>' 结尾但没有任何标记 → 仍然按「未说完」处理（英文旁白形态）。
	if !narrationTailSuspends("Reading failfirst.sh mutation script") {
		t.Error("普通无标点收尾仍应判为未说完")
	}
}

// 端到端：DSML 正文（工具轮 + 声明工具）不得触发 narration 续写，客户端只收到一次内容。
func TestNarrationRescueSkipsDSMLFromUpstream(t *testing.T) {
	seg := `data: {"id":"d1","choices":[{"index":0,"delta":{"content":"正在执行 </｜｜DSML｜｜ invoke> </｜｜DSML｜｜ calls>"}}]}` + "\n\n" +
		`data: {"id":"d1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":106507,"completion_tokens":30,"total_tokens":106537}}` + "\n\n" +
		"data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, segNarrateContinue })
	r := newContinueReaderWithFloor(context.Background(), up,
		&auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999},
		[]byte(toolLoopBody), "1.2.3.4", upstream.ChatMeta{}, io.NopCloser(strings.NewReader(seg)), 8192)
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 0 {
		t.Fatalf("DSML 正文不该触发续写，实际 %d 次", len(*reqs))
	}
	if !strings.Contains(string(out), "DSML") {
		t.Fatalf("客户端应原样收到 DSML 文本:\n%s", out)
	}
}
