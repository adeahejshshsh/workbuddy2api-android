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

// TestNarrationRescueOnceOnly 续写段又停在同一形态：最多救 narrationMaxRescues 次，不套娃。
func TestNarrationRescueOnceOnly(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, segNarrateLeadIn })
	r := newNarrateReader(t, up, agentBody, segNarrateLeadIn)
	_, _ = io.ReadAll(r)
	if len(*reqs) != narrationMaxRescues {
		t.Fatalf("续写请求数=%d want %d（每请求最多救 %d 次）", len(*reqs), narrationMaxRescues, narrationMaxRescues)
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

// issue #209 建议 2：narration 原先没有长度闸门，只要停在冒号/逗号上就续写，
// agent 客户端的「说明 + 列表」几乎总以冒号收尾，于是每个工具轮都被多续一次。
// issue #245 之后闸门只对「标点型悬停」生效，所以这里同时钉住两种尾部的分类。
func TestNarrationLengthGate(t *testing.T) {
	short := strings.Repeat("我先看一下这几个文件的改动：", 3)       // ~200B，停在冒号
	long := strings.Repeat("这一轮我把改动逐条过了一遍，下面是要点：", 20) // >400B，停在冒号
	if !narrationTailSuspends(short) || !narrationTailSuspends(long) {
		t.Fatal("前提失败：两条都该被判为尾部悬空")
	}
	if !narrationTailPunctuated(short) || !narrationTailPunctuated(long) {
		t.Fatal("前提失败：两条都是标点型悬停，该受长度闸门约束")
	}
	if len(short) > narrationMaxBytes {
		t.Fatalf("样本 1 应小于闸门：%d", len(short))
	}
	if len(long) <= narrationMaxBytes {
		t.Fatalf("样本 2 应大于闸门：%d", len(long))
	}
	// 无标点悬停与标点型悬停必须被分开：前者不受长度闸门约束（issue #245）。
	bare := long + "现在我确认完了，我将开始修改 continue.go 里的续写闸门"
	if narrationTailPunctuated(bare) || !narrationTailSuspends(bare) {
		t.Fatalf("无标点悬停应被判为「非标点型」且悬空（punctuated=%v suspends=%v）",
			narrationTailPunctuated(bare), narrationTailSuspends(bare))
	}
	if !narrationTailStructurallyOpen("```go\nfunc x() {") {
		t.Fatal("未闭合代码块应判为结构上没写完（不受长度闸门限制）")
	}
}

// 等用户回话的正文，narration 也不能续（否则「请确认」之后会自己往下写并动手）。
func TestNarrationSkipsWhenWaitingForUser(t *testing.T) {
	seg := `data: {"id":"n1","choices":[{"index":0,"delta":{"content":"草稿在下面。发之前请你确认收件人，"}}]}` + "\n\n" +
		`data: {"id":"n1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":106507,"completion_tokens":30,"total_tokens":106537}}` + "\n\n" +
		"data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(int) (int, string) { return 200, segToolOnlyText })
	r := newToolOnlyReader(t, up, toolLoopBody, seg)
	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 0 {
		t.Fatalf("含「请你确认」的正文不该被续写，实际 %d 次", len(*reqs))
	}
}

// 长度闸门必须端到端生效：>400B 且停在冒号上 → 一个续写请求都不该发。
// （上面 TestNarrationLengthGate 只测辅助函数，变异跑不出来，所以另立这条。）
func TestNarrationLongLeadInNotRescued(t *testing.T) {
	long := strings.Repeat("这一轮我把三处改动逐条过了一遍，下面是按文件整理的要点清单：", 12) // ~1100B
	if len(long) <= narrationMaxBytes {
		t.Fatalf("样本不够长：%d", len(long))
	}
	seg := `data: {"id":"n2","choices":[{"index":0,"delta":{"content":"` + long + `"}}]}` + "\n\n" +
		`data: {"id":"n2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":106507,"completion_tokens":300,"total_tokens":106807}}` + "\n\n" +
		"data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(int) (int, string) { return 200, segToolOnlyText })
	r := newToolOnlyReader(t, up, toolLoopBody, seg)
	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 0 {
		t.Fatalf("1.1KB 正文停在冒号上不该续写，实际 %d 次", len(*reqs))
	}
}

// 短预告（<400B）仍要救：这是这条救援存在的理由。
func TestNarrationShortLeadInStillRescued(t *testing.T) {
	short := "我先看一下这几个文件的实际实现："
	if len(short) > narrationMaxBytes {
		t.Fatal("样本超标")
	}
	seg := `data: {"id":"n3","choices":[{"index":0,"delta":{"content":"` + short + `"}}]}` + "\n\n" +
		`data: {"id":"n3","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":106507,"completion_tokens":20,"total_tokens":106527}}` + "\n\n" +
		"data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(int) (int, string) { return 200, segToolOnlyText })
	r := newToolOnlyReader(t, up, toolLoopBody, seg)
	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("短预告应救一次，实际 %d 次", len(*reqs))
	}
}

// issue #245 / #260：两条判据的口径不同，别再混用。
//
//   - textYieldsToUser（toolonly）按**整段**认：那条救援会采纳工具调用，正文又短，宁可少救；
//   - yieldsToUser(text,false)（narration）只认「末句 / 末尾问句 / 措辞后紧跟我-你」三种位置：
//     长正文里的技术叙述（"是否需要更换"）不该把尾部的承诺句一起否掉。
func TestTextYieldsToUserWholeText(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"中段含是否需要（toolonly 整段认）", "把读数归档，为“是否需要更换”提供量化支撑。\n\n我将开始修改 check.go", true},
		{"末句真的在等确认", "改动已过一遍。\n\n请确认后我再动手。", true},
		{"末句问号", "三处都改完了。要现在发布吗？", true},
		{"末句英文", "All done. Let me know if you want the diff.", true},
		{"只有一句（末句即全句）", "请确认后我再发送。", true},
		{"空正文", "", false},
	}
	for _, c := range cases {
		if got := textYieldsToUser(c.text); got != c.want {
			t.Fatalf("%s: textYieldsToUser(%q)=%v want %v", c.name, c.text, got, c.want)
		}
	}
	// 同一条正文在 narration 口径下是「技术叙述」：尾部承诺句该救。
	if yieldsToUser("把读数归档，为“是否需要更换”提供量化支撑。\n\n我将开始修改 check.go", false) {
		t.Fatal("narration 口径不该被中段的技术叙述否掉")
	}
}

// issue #245：长正文 + 无标点悬停必须救。长度闸门只该约束标点型悬停（"说明+列表："），
// 无标点是句子被掐断（"我将开始修改…"/"我会…"），与正文长度无关——长会话里恰恰最常见，
// 原先按整段长度挡掉是一条都不救。
func TestNarrationLongBareSuspenseStillRescued(t *testing.T) {
	long := strings.Repeat("我把三处改动逐行过了一遍，其中两点需要单独说明。", 40) // ≈1.6KB
	text := long + "现在我确认完了，我将开始修改 internal/server/continue.go 里的续写闸门"
	if len(text) <= narrationMaxBytes {
		t.Fatalf("样本不够长：%d", len(text))
	}
	if narrationTailPunctuated(text) || !narrationTailSuspends(text) {
		t.Fatalf("前提失败：该样本应判为无标点悬停（punctuated=%v suspends=%v）",
			narrationTailPunctuated(text), narrationTailSuspends(text))
	}
	raw, _ := json.Marshal(text)
	seg := `data: {"id":"n4","choices":[{"index":0,"delta":{"content":` + string(raw) + `}}]}` + "\n\n" +
		`data: {"id":"n4","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":301000,"completion_tokens":500,"total_tokens":301500}}` + "\n\n" +
		"data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(int) (int, string) { return 200, segToolOnlyText })
	r := newToolOnlyReader(t, up, toolLoopBody, seg)
	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("长正文停在无标点断句上应救一次，实际 %d 次", len(*reqs))
	}
}

// issue #245 第二例（真机取证）：正文里出现「是否需要」这类技术叙述、尾部停在承诺句上。
// textYieldsToUser 原先对整段做 Contains，一句「是否需要更换钢丝绳」就把整条救援否掉——
// 正文越长越容易撞上，这正是「会话越长越频繁」的成因。
func TestNarrationMidTextYieldMarkerStillRescued(t *testing.T) {
	// 真机原文的收尾（2026-11-16 16:12，4083 字）：措辞就在末句之前，200B 窗口也躲不开。
	text := "## 三、整改建议\n\n把每一次实测读数归档，使判断不再依赖人而异的经验判断，" +
		"也使每次检验的数据自动进入趋势库，为“是否需要更换”提供量化支撑。\n\n---\n\n" +
		"我将开始修改 internal/verify/check.go"
	if yieldsToUser(text, false) {
		t.Fatal("末句是承诺句，不该判为等用户回话")
	}
	if narrationTailPunctuated(text) || !narrationTailSuspends(text) {
		t.Fatalf("前提失败：该样本应判为无标点悬停（punctuated=%v suspends=%v）",
			narrationTailPunctuated(text), narrationTailSuspends(text))
	}
	raw, _ := json.Marshal(text)
	seg := `data: {"id":"n5","choices":[{"index":0,"delta":{"content":` + string(raw) + `}}]}` + "\n\n" +
		`data: {"id":"n5","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":268538,"completion_tokens":900,"total_tokens":269438}}` + "\n\n" +
		"data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(int) (int, string) { return 200, segToolOnlyText })
	r := newNarrateReader(t, up, agentBody, seg)
	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("中段含 yield 措辞、尾部停在承诺句上应救一次，实际 %d 次", len(*reqs))
	}
	// 指令要按形态选：承诺句用 narrationPromiseNudge，否则模型会回怼「并未中断」。
	obj := reqBody(t, (*reqs)[0])
	msgs, _ := obj["messages"].([]any)
	last, _ := msgs[len(msgs)-1].(map[string]any)
	if got, _ := last["content"].(string); got != narrationPromiseNudge {
		t.Fatalf("承诺句形态应带 narrationPromiseNudge，实际：%q", got)
	}
}

// 措辞落在**末句**里（模型真的在等用户拍板）：仍然不救。这条守着 #209 的事故形态。
func TestNarrationFinalSentenceYieldStillSkipped(t *testing.T) {
	text := "我已经把三处改动过了一遍，剩下两处涉及线上配置。\n\n请确认后我再动手："
	if !yieldsToUser(text, false) {
		t.Fatal("末句含「请确认后」，应判为等用户回话")
	}
	raw, _ := json.Marshal(text)
	seg := `data: {"id":"n8","choices":[{"index":0,"delta":{"content":` + string(raw) + `}}]}` + "\n\n" +
		`data: {"id":"n8","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":268538,"completion_tokens":60,"total_tokens":268598}}` + "\n\n" +
		"data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(int) (int, string) { return 200, segToolOnlyText })
	r := newNarrateReader(t, up, agentBody, seg)
	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 0 {
		t.Fatalf("末句在等用户拍板，不该续写，实际 %d 次", len(*reqs))
	}
}

// 续写段自己也停在承诺句上：再救一次就够。原先 narrationTried 是布尔，
// 第二次直接放弃，客户端仍停在「我将…」——长正文模型常常连写两段预告。
func TestNarrationSecondRescueWhenContinuedSegmentAlsoSuspends(t *testing.T) {
	raw, _ := json.Marshal("我先说明结论，然后我将开始修改 internal/verify/check.go")
	segAgain := `data: {"id":"n6","choices":[{"index":0,"delta":{"content":` + string(raw) + `}}]}` + "\n\n" +
		`data: {"id":"n6","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":268600,"completion_tokens":60,"total_tokens":268660}}` + "\n\n" +
		"data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) {
		if call == 1 {
			return 200, segAgain // 第一段续写又停在承诺句上
		}
		return 200, segToolOnlyText // 第二段续写正常收尾
	})
	r := newNarrateReader(t, up, agentBody, segNarrateLeadIn)
	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 2 {
		t.Fatalf("续写段再次停在预告句上应再救一次（共 2 次），实际 %d 次", len(*reqs))
	}
}

// 第三次不救：两次机会用完就按原样收尾，避免模型反复预告把请求拖成无限续写。
func TestNarrationCapsAtTwoRescues(t *testing.T) {
	raw, _ := json.Marshal("我将开始修改 internal/verify/check.go")
	suspend := `data: {"id":"n7","choices":[{"index":0,"delta":{"content":` + string(raw) + `}}]}` + "\n\n" +
		`data: {"id":"n7","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":268600,"completion_tokens":60,"total_tokens":268660}}` + "\n\n" +
		"data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(int) (int, string) { return 200, suspend })
	r := newNarrateReader(t, up, agentBody, suspend)
	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 2 {
		t.Fatalf("每请求最多救两次，实际 %d 次", len(*reqs))
	}
}

// ── 等用户拍板的措辞挡住救援；技术叙述里的同类措辞不挡（#209 / #260 / #245）──────

// narrationRescueCount 首段正文为 text、finish=stop 的工具类请求一共发了几次预告句救援。
func narrationRescueCount(t *testing.T, text string) int {
	t.Helper()
	raw, _ := json.Marshal(text)
	seg := `data: {"id":"y1","choices":[{"index":0,"delta":{"content":` + string(raw) + `}}]}` + "\n\n" +
		`data: {"id":"y1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":268538,"completion_tokens":80,"total_tokens":268618}}` + "\n\n" +
		"data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(int) (int, string) { return 200, segToolOnlyText })
	if _, err := io.ReadAll(newNarrateReader(t, up, agentBody, seg)); err != nil {
		t.Fatalf("read: %v", err)
	}
	return len(*reqs)
}

// 等用户拍板的措辞不论在哪一句都不救（#209 的事故形态）：确认请求后面常跟着草稿本身，
// 只看最后一句（或末尾 200B）会把收件人 / 主题行当成末句、把它判成承诺句救掉，模型就
// 真把邮件发出去了。这就是 #260 的回归形态。
func TestNarrationYieldMarkersStillSkipped(t *testing.T) {
	for _, text := range []string{
		"邮件已经写好，请确认后我再发送。\n\n收件人：all@example.com\n主题：Q3 计划",
		"邮件已经写好，确认后我再发送。\n\n收件人：all@example.com\n主题：Q3 计划",
		"请确认以下操作后我再执行\n- 删除 A\n- 删除 B",
		"是否需要我现在把邮件发出去？\n\n收件人：all@example.com\n主题：Q3 计划",
		"要不要现在发送？\n\n收件人：all@example.com\n主题：Q3 计划",
		"是否需要我先发给你过目。草稿如下\n收件人：all@example.com",
		"三处改动都过了一遍。是否需要顺便把文档也更新",
		// 末句里带文件名 / 版本号的半角点：不能把「是否需要」切到前一句去。
		"三处都改完了。是否需要顺便改 config.go 里的超时",
		"要不要顺便把 v1.2 的配置也改掉",
	} {
		if n := narrationRescueCount(t, text); n != 0 {
			t.Errorf("正文在等用户拍板，不该续写（%q），实际 %d 次", text, n)
		}
	}
}

// 反方向：技术叙述里的「确认无误 / 确认后 / 请告知 / 可以吗 / 是否需要我们」不是在等用户，
// 尾部停在承诺句上照样要救（#245）。按整段认这些词会把长回复一条条挡掉。
func TestNarrationNarrativeYieldWordsStillRescued(t *testing.T) {
	long := strings.Repeat("我把三处改动逐行过了一遍，其中两点需要单独说明。", 12)
	for _, mid := range []string{
		"日志格式确认无误。",
		"staging 确认后再上线。",
		"如有问题请告知维护者。",
		"这个函数的行为可以吗？我查过调用方，结论是可以。",
		"是否需要我们更换钢丝绳要看趋势数据。",
	} {
		text := long + mid + "\n\n我将开始修改 internal/verify/check.go"
		if n := narrationRescueCount(t, text); n != 1 {
			t.Errorf("中段是技术叙述（%q）、尾部停在承诺句上，应救一次，实际 %d 次", mid, n)
		}
	}
}

// toolonly 会采纳工具调用：确认请求后面又跟了一句（"草稿放在 draft.md 里。"）时，只看最后
// 一句就会放行救援、把「发送」这一步替用户执行掉。这条救援按整段认等用户措辞。
func TestToolOnlySkipsWaitingForUserBeforeLastSentence(t *testing.T) {
	seg := `data: {"id":"t9","choices":[{"index":0,"delta":{"content":"邮件已写好，请确认后我再发送。草稿放在 draft.md 里。"}}]}` + "\n\n" +
		`data: {"id":"t9","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":106507,"completion_tokens":40,"total_tokens":106547}}` + "\n\n" +
		"data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, segToolOnlyToolCall })
	if _, err := io.ReadAll(newToolOnlyReader(t, up, toolLoopBody, seg)); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 0 {
		t.Fatalf("模型在等用户确认，不该续写，实际 %d 次", len(*reqs))
	}
}

// 救援段一个字都没写出来（只给了 finish=stop）：不发第二次——请求体与第一次一字不差，
// 再发只是再付一次整段上下文的钱。
func TestNarrationNoSecondRescueWithoutProgress(t *testing.T) {
	empty := `data: {"id":"e1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":268600,"completion_tokens":3,"total_tokens":268603}}` + "\n\n" +
		"data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(int) (int, string) { return 200, empty })
	out, err := io.ReadAll(newNarrateReader(t, up, agentBody, segNarrateLeadIn))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("救援段没写出新正文时不该再救，实际 %d 次", len(*reqs))
	}
	if n := strings.Count(string(out), `"finish_reason":`); n != 1 {
		t.Fatalf("终态必须恰好一个，实际 %d 个", n)
	}
}
