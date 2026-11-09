package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// segNarrateLeadIn 首段：正文写完一段结论，用一句预告收尾就 finish=stop、零工具调用。
// 线上取证原样（09:05:56 / 09:07:25 两条 stop-no-toolcall 样本）。
const segNarrateLeadIn = `data: {"id":"n1","choices":[{"index":0,"delta":{"content":"5G 已降到 10 dBm。重新连接并测试下行："}}]}` + "\n\n" +
	`data: {"id":"n1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":221445,"completion_tokens":26,"total_tokens":221471}}` + "\n\n" +
	"data: [DONE]\n\n"

// segNarrateContinue 救援段：接着写下去（正常收尾）。
const segNarrateContinue = `data: {"id":"n2","choices":[{"index":0,"delta":{"content":"下行 400-600 Mbps 稳定。"}}]}` + "\n\n" +
	`data: {"id":"n2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":221500,"completion_tokens":12,"total_tokens":221512}}` + "\n\n" +
	"data: [DONE]\n\n"

// agentBody 工具类请求：声明了工具，历史里有工具轮。
const agentBody = `{"model":"cn:deepseek-v4.1-flash","max_tokens":8192,` +
	`"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}],` +
	`"messages":[{"role":"user","content":"查 user-store 的批量赋值"},` +
	`{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{}"}}]},` +
	`{"role":"tool","tool_call_id":"c1","content":"ok"}]}`

func newNarrateReader(t *testing.T, up *upstream.Client, body, seg string) *continueReader {
	t.Helper()
	return newContinueReaderWithFloor(context.Background(), up,
		&auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999},
		[]byte(body), "1.2.3.4", upstream.ChatMeta{}, io.NopCloser(strings.NewReader(seg)), 8192)
}

// TestNarrationTailSuspends 收尾符判据：只认结构性的「没说完」。
func TestNarrationTailSuspends(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"验证路由器状态并做对比实验（恢复到 25dBm 看是否必然重启）：", true},
		{"Let me check the config:", true},
		{"继续检查", true},
		{"Reading failfirst.sh mutation script", true}, // 英文旁白，无标点
		{"```go\nfunc main() {", true},                 // 未闭合代码块
		{"好的，已经改完。", false},                            // 句末句号
		{"Done.", false},                               // 英文句号
		{"这样可以吗？", false},                              // 问号
		{"```go\nfmt.Println(1)\n```", false},          // 已闭合代码块
		{"  \n", false},                                // 空白
		{"", false},                                    //
		{"结论已确认（见上表）", false},                          // 右括号收尾
	}
	for _, c := range cases {
		if got := narrationTailSuspends(c.text); got != c.want {
			t.Errorf("narrationTailSuspends(%q)=%v want %v", c.text, got, c.want)
		}
	}
}

// reqBody 取出测试里记录的出站请求体并解码：上游对 ≥1KB 的 body 做 gzip
// （见 upstream.gzipMinChatBody），测试不能假设拿到的是明文。
func reqBody(t *testing.T, raw string) map[string]any {
	t.Helper()
	b := []byte(raw)
	if len(b) > 2 && b[0] == 0x1f && b[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("gunzip: %v", err)
		}
		b, err = io.ReadAll(zr)
		if err != nil {
			t.Fatalf("gunzip read: %v", err)
		}
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("续写请求体非法: %v（前 40 字节 %q）", err, string(b[:min(40, len(b))]))
	}
	return got
}

// TestNarrationRescueOnLeadIn 预告式收尾必须续写一次，且续写段带上 assistant 前缀
// 与原请求的 tools（否则模型没法接着调工具）。
func TestNarrationRescueOnLeadIn(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, segNarrateContinue })
	r := newNarrateReader(t, up, agentBody, segNarrateLeadIn)
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("续写请求数=%d want 1（预告式收尾应续写一次）", len(*reqs))
	}
	got := reqBody(t, (*reqs)[0])
	if _, has := got["tools"]; !has {
		t.Fatalf("续写段丢了 tools：模型没法接着调工具")
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) < len(mustMsgs(t, agentBody))+2 {
		t.Fatalf("续写段应追加 assistant 前缀 + 指令，实际消息数 %d", len(msgs))
	}
	// 倒数第二条是 assistant 前缀，内容必须等于首段正文（不重复、不丢）。
	asst, _ := msgs[len(msgs)-2].(map[string]any)
	if c, _ := asst["content"].(string); !strings.Contains(c, "重新连接并测试下行：") {
		t.Fatalf("assistant 前缀丢了首段正文：%q", c)
	}
	last, _ := msgs[len(msgs)-1].(map[string]any)
	if s, _ := last["content"].(string); !strings.Contains(s, "预告") {
		t.Fatalf("续写指令没说清「停在预告上」：%q", s)
	}
	// 客户端看到两段正文拼在一起。
	s := string(out)
	if !strings.Contains(s, "重新连接并测试下行：") || !strings.Contains(s, "下行 400-600 Mbps 稳定。") {
		t.Fatalf("客户端应看到拼接后的完整正文:\n%s", s)
	}
}

func mustMsgs(t *testing.T, body string) []any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("body: %v", err)
	}
	m, _ := obj["messages"].([]any)
	return m
}

// TestNoNarrationRescueWithoutToolTurn 只声明工具、历史里没有工具轮（闲聊/角色扮演）
// 不救：它们用冒号收尾是正常文风，续写会污染回答。
func TestNoNarrationRescueWithoutToolTurn(t *testing.T) {
	body := `{"model":"cn:deepseek-v4.1-flash","max_tokens":8192,` +
		`"tools":[{"type":"function","function":{"name":"read_file"}}],` +
		`"messages":[{"role":"user","content":"你好"}]}`
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, segNarrateContinue })
	r := newNarrateReader(t, up, body, segNarrateLeadIn)
	_, _ = io.ReadAll(r)
	if len(*reqs) != 0 {
		t.Fatalf("无工具轮的纯文本会话不应续写，实际 %d 次", len(*reqs))
	}
}

// TestNoNarrationRescueWhenTextComplete 正文正常说完（句末句号）不救。
func TestNoNarrationRescueWhenTextComplete(t *testing.T) {
	seg := `data: {"id":"n1","choices":[{"index":0,"delta":{"content":"根因已确认。"}}]}` + "\n\n" +
		`data: {"id":"n1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" + "data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, segNarrateContinue })
	r := newNarrateReader(t, up, agentBody, seg)
	_, _ = io.ReadAll(r)
	if len(*reqs) != 0 {
		t.Fatalf("正常说完的正文不应续写，实际 %d 次", len(*reqs))
	}
}

// TestNarrationRescueOnceOnly 续写段又停在同一形态：只救一次，不套娃。
func TestNarrationRescueOnceOnly(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, segNarrateLeadIn })
	r := newNarrateReader(t, up, agentBody, segNarrateLeadIn)
	_, _ = io.ReadAll(r)
	if len(*reqs) != 1 {
		t.Fatalf("续写请求数=%d want 1（每请求只救一次）", len(*reqs))
	}
}

// TestNoNarrationRescueOnToolCalls 工具轮（有 tool_calls 分片）不能被当成预告收尾。
func TestNoNarrationRescueOnToolCalls(t *testing.T) {
	seg := `data: {"id":"n1","choices":[{"index":0,"delta":{"content":"检查中：","tool_calls":[{"index":0,"id":"c2","function":{"name":"read_file","arguments":"{}"}}]}}]}` + "\n\n" +
		`data: {"id":"n1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" + "data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, segNarrateContinue })
	r := newNarrateReader(t, up, agentBody, seg)
	_, _ = io.ReadAll(r)
	if len(*reqs) != 0 {
		t.Fatalf("含工具调用的轮次不应续写，实际 %d 次", len(*reqs))
	}
}
