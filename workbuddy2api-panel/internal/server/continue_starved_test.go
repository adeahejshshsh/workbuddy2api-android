package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// seg1StarvedThinking 段1：思考吃掉绝大部分预算后以 length 截断（2026-11-07 真机
// 形态，cn:deepseek-v4.1-flash / effort=max / max_tokens=8192：completion=8192、
// think=7612，正文只写出半句）。
const seg1StarvedThinking = `data: {"id":"c1","choices":[{"index":0,"delta":{"reasoning_content":"先分析目标结构……"}}]}` + "\n\n" +
	`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"TCP 握手的第一个漏洞是半句"}}]}` + "\n\n" +
	`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":42,"completion_tokens":8192,"completion_thinking_tokens":7612,"total_tokens":8234}}` + "\n\n" +
	"data: [DONE]\n\n"

// seg1CheapThinking 段1：思考占比很低（思考 100 / 输出 1024），正文写到 length
// —— 这是客户端要的长度，不该续写。
const seg1CheapThinking = `data: {"id":"c1","choices":[{"index":0,"delta":{"reasoning_content":"简短推理"}}]}` + "\n\n" +
	`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"这是一段被客户端额度截断的正文"}}]}` + "\n\n" +
	`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":42,"completion_tokens":1024,"completion_thinking_tokens":100,"total_tokens":1066}}` + "\n\n" +
	"data: [DONE]\n\n"

// TestContinueStarvedThinkingWithClientLimitContinues 客户端显式带 max_tokens，但
// 额度被思考吃光（think 占 93%）时也必须续写——否则用户看到的就是「回答写到一半
// 停住、只能手动发『继续』」。这条是 issue「用着用着中断」的直接回归。
func TestContinueStarvedThinkingWithClientLimitContinues(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, testSeg2Text })
	r := newTestContinueReader(t, up,
		`{"model":"cn:deepseek-v4.1-flash","max_tokens":8192,"reasoning_effort":"max","messages":[{"role":"user","content":"hi"}]}`,
		io.NopCloser(strings.NewReader(seg1StarvedThinking)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("续写请求数=%d want 1（思考吃光预算应续写）", len(*reqs))
	}
	s := string(out)
	for _, want := range []string{"半句", "继续", `"finish_reason":"stop"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("输出缺少 %q:\n%s", want, s)
		}
	}
	// 续写段必须关思考：否则同一份额度再被思考烧一遍，续三段还是半句。
	var body map[string]any
	if err := json.Unmarshal([]byte((*reqs)[0]), &body); err != nil {
		t.Fatalf("续写请求体非法 JSON: %v", err)
	}
	th, _ := body["thinking"].(map[string]any)
	if th == nil || th["type"] != "disabled" {
		t.Fatalf("续写段应关思考（thinking.type=disabled），实际 thinking=%v", body["thinking"])
	}
	if _, has := body["reasoning_effort"]; has {
		t.Fatalf("关思考时不得再带 reasoning_effort（upstream 对 disabled+effort 语义未取证）")
	}
	// 客户端额度原样保留：不放大它要的预算（超发是语义违约）。
	if v, ok := body["max_tokens"].(float64); !ok || v != 8192 {
		t.Fatalf("续写段 max_tokens=%v，应原样保留 8192", body["max_tokens"])
	}
	// 已输出内容作为 assistant 前缀，模型才能接着写而不是重头。
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("续写请求 messages=%d want 3（user+assistant前缀+nudge）", len(msgs))
	}
}

// TestContinueClientLimitCheapThinkingNoContinue 客户端额度正常用完（思考占比
// 仅 10%）时不续写：这是客户端自己要的长度，多写就是语义违约。
func TestContinueClientLimitCheapThinkingNoContinue(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, testSeg2Text })
	r := newTestContinueReader(t, up,
		`{"model":"cn:deepseek-v4.1-flash","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`,
		io.NopCloser(strings.NewReader(seg1CheapThinking)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 0 {
		t.Fatalf("续写请求数=%d want 0（单纯客户端限额不续）", len(*reqs))
	}
	if !strings.Contains(string(out), `"finish_reason":"length"`) {
		t.Fatalf("length 终态应原样下发给客户端:\n%s", out)
	}
}

// TestStarvedByThinkingThreshold 比率闸门本身的边界：0.5 是分界，且必须有正文
// （纯思考零正文交给 starved 救援，不走续写）。
func TestStarvedByThinkingThreshold(t *testing.T) {
	cases := []struct {
		completion, think float64
		want              bool
	}{
		{8192, 7612, true},
		{8192, 4096, true},  // 恰好 50%
		{8192, 4095, false}, // 略低于 50%
		{1024, 100, false},
		{1024, 0, false},
		{0, 100, false},
	}
	for _, c := range cases {
		r := &continueReader{segCompletion: c.completion, segThink: c.think}
		r.text.WriteString("半句正文") // 判据要求「已写出正文」，纯思考走 starved 救援
		if got := r.starvedByThinking(); got != c.want {
			t.Errorf("completion=%v think=%v: starvedByThinking=%v want %v", c.completion, c.think, got, c.want)
		}
	}
	// 纯思考、零正文：即使占比 100% 也不算——那是 starved 救援的地盘。
	r := &continueReader{segCompletion: 200, segThink: 200}
	if r.starvedByThinking() {
		t.Error("零正文（纯思考）不应判为可续写的思考吃光")
	}
}

// TestNoteSegmentUsageAliases 上游两个字段名都给过思考 token，二者都要认
// （completion_thinking_tokens 优先，缺时退 completion_tokens_details.reasoning_tokens）。
func TestNoteSegmentUsageAliases(t *testing.T) {
	r := &continueReader{}
	r.noteSegmentUsage(map[string]any{"completion_tokens": float64(800),
		"completion_tokens_details": map[string]any{"reasoning_tokens": float64(700)}})
	if r.segCompletion != 800 || r.segThink != 700 {
		t.Fatalf("details 别名未认：completion=%v think=%v", r.segCompletion, r.segThink)
	}
	r2 := &continueReader{}
	r2.noteSegmentUsage(map[string]any{"completion_tokens": float64(800), "completion_thinking_tokens": float64(650)})
	if r2.segThink != 650 {
		t.Fatalf("顶层字段优先口径错：think=%v", r2.segThink)
	}
}

// TestStarvedContinueDisabledForNonSwitchableModel 不能关思考的模型（非
// DeepSeek 系）不做 disabled 改写：不知道拿什么关就不猜，续写段原样带档位。
func TestStarvedContinueDisabledForNonSwitchableModel(t *testing.T) {
	acct := &auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999}
	r := newContinueReader(context.Background(), &upstream.Client{}, acct,
		[]byte(`{"model":"glm-5.2","max_tokens":8192,"reasoning_effort":"max","messages":[]}`),
		"1.2.3.4", upstream.ChatMeta{}, io.NopCloser(strings.NewReader("")))
	r.segCompletion, r.segThink = 8192, 7612
	raw, err := r.continuationBody()
	if err != nil {
		t.Fatalf("continuationBody: %v", err)
	}
	var body map[string]any
	json.Unmarshal(raw, &body)
	if _, has := body["thinking"]; has {
		t.Fatalf("不可关思考的模型不该被注入 thinking: %v", body["thinking"])
	}
	if _, has := body["reasoning_effort"]; !has {
		t.Fatalf("不可关思考的模型应保留原档位")
	}
}

var _ = http.StatusOK
