package server

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 工具调用标记要按**完整性**分流，这是「DSML 修了又像没修」的修复点。
//
// 线上两类真实样本：
//
//	完整块  "…</｜｜DSML｜｜ invoke> </｜｜DSML｜｜ calls>"  → 客户端会执行，
//	        不能续写（续写会造出第二次调用：用户看到「任务该结束了却一直在调工具」）
//	截断块  "…2>&1</｜｜DSML｜｜ parameter>"               → 调用写了一半，
//	        客户端解析不出来，界面留下半截标记（用户截图那种）→ 必须续写补完
func TestToolCallMarkupState(t *testing.T) {
	cases := []struct {
		name string
		text string
		want markupState
	}{
		{"完整 DSML 块", "正在执行 </｜｜DSML｜｜ invoke> </｜｜DSML｜｜ calls>", markupComplete},
		{"完整 XML 块", "调用：<function_calls><invoke name=\"x\"></invoke></function_calls>", markupComplete},
		{"截断在 parameter", "…2>&1</｜｜DSML｜｜ parameter>", markupPartial},
		{"截断在 invoke", "…<｜｜DSML｜｜ invoke name=\"read_file\">", markupPartial},
		{"无标记", "普通回答，以冒号结尾：", markupNone},
		{"含 < 但不是标记", "温度 < 5℃ 时注意。", markupNone},
	}
	for _, c := range cases {
		if got := toolCallMarkupState(c.text); got != c.want {
			t.Errorf("%s: toolCallMarkupState=%v want %v", c.name, got, c.want)
		}
	}
}

// 截断的标记必须触发续写，完整的必须不触发。
func TestNarrationTailOnMarkupStates(t *testing.T) {
	if narrationTailSuspends("执行中 </｜｜DSML｜｜ invoke> </｜｜DSML｜｜ calls>") {
		t.Error("完整工具调用块不该被判为「没说完」")
	}
	if !narrationTailSuspends("…2>&1</｜｜DSML｜｜ parameter>") {
		t.Error("截断的工具调用块必须被判为「没说完」，否则客户端只看到半截标记")
	}
}

// RP 客户端的情绪标记 &&happy&& 与 </summary> 都是「说完了」的形态。
func TestNarrationTailRPAndSummary(t *testing.T) {
	for _, s := range []string{
		"没生气就最好啦  &&happy&&",
		"报告/）。在此之前不发任何新请求。 </summary>",
		"结论如上。</details>",
	} {
		if narrationTailSuspends(s) {
			t.Errorf("这是完整收尾，不该续写：%q", s)
		}
	}
	// 对照：真正的英文旁白仍然要救。
	if !narrationTailSuspends("Reading Dashboard.tsx render block") {
		t.Error("英文旁白仍应判为未说完")
	}
}

// 端到端：截断的 DSML 文本必须被续写补完（客户端不该只收到半截标记）。
func TestNarrationRescuesTruncatedMarkup(t *testing.T) {
	seg := `data: {"id":"d1","choices":[{"index":0,"delta":{"content":"执行 pwsh -c 即可。</｜｜DSML｜｜ parameter>"}}]}` + "\n\n" +
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
	if len(*reqs) != 1 {
		t.Fatalf("截断的标记应触发一次续写，实际 %d 次", len(*reqs))
	}
	if !strings.Contains(string(out), "下行 400-600 Mbps 稳定。") {
		t.Fatalf("客户端应收到续写内容:\n%s", out)
	}
}

// 端到端：完整的 DSML 块不续写（否则会造出第二次工具调用）。
func TestNarrationSkipsCompleteMarkup(t *testing.T) {
	seg := `data: {"id":"d2","choices":[{"index":0,"delta":{"content":"正在执行 </｜｜DSML｜｜ invoke> </｜｜DSML｜｜ calls>"}}]}` + "\n\n" +
		`data: {"id":"d2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":106507,"completion_tokens":30,"total_tokens":106537}}` + "\n\n" +
		"data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, segNarrateContinue })
	r := newContinueReaderWithFloor(context.Background(), up,
		&auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999},
		[]byte(toolLoopBody), "1.2.3.4", upstream.ChatMeta{}, io.NopCloser(strings.NewReader(seg)), 8192)
	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 0 {
		t.Fatalf("完整标记块不该续写，实际 %d 次", len(*reqs))
	}
}
