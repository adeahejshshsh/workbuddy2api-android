package server

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// chatBody 角色扮演客户端的请求形态：声明了工具、历史里有过工具轮（所以 narration 的
// 「这是 agent 会话」判据成立），但最后一跳是用户发言。
//
// 这个夹具不是随便挑的：toolOnly 那条救援要求「最后一条消息是工具结果」，
// narration 只要求「历史里任意一处有工具轮」，两者在真实会话里不是同一批请求。
// 用 toolLoopBody 测会把 narration 的假阳性记成 toolOnly 的账（实测确实如此）。
const chatBody = `{"model":"cn:deepseek-v4.1-flash","max_tokens":8192,` +
	`"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}],` +
	`"messages":[{"role":"user","content":"查 user-store 的批量赋值"},` +
	`{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{}"}}]},` +
	`{"role":"tool","tool_call_id":"c1","content":"ok"},` +
	`{"role":"user","content":"在吗"}]}`

// issue #273 现象 1：QQ 角色扮演机器人「一两句短话 + ||| 分条 + <face:…> 收尾、
// 基本不写句号」，四条判据（声明工具 / 历史有工具轮 / 尾部无句末标点 / finish=stop）
// 条条成立，于是每条正常回复都被当成「停在预告句」续写一次——实测 87 次/天，
// 单条计费从 0.2 涨到 8.58 credits（救援要重发整段十万级上下文）。
func TestNarrationChatStyleNotRescued(t *testing.T) {
	for _, text := range []string{
		"怎么啦妈 ||| 是干活累着了吗 <face:抱抱>", // issue 原样本（52B）
		"好呀 ||| 那你早点睡",
		"嗯嗯我知道啦",
	} {
		seg := `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"` + text + `"}}]}` + "\n\n" +
			`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":416436,"completion_tokens":20,"total_tokens":416456}}` + "\n\n" +
			"data: [DONE]\n\n"
		up, reqs := fakeContinueUpstream(t, func(int) (int, string) { return 200, segToolOnlyText })
		r := newToolOnlyReader(t, up, chatBody, seg)
		if _, err := io.ReadAll(r); err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(*reqs) != 0 {
			t.Errorf("聊天文风 %q 不该续写，实际 %d 次（每次都要重付一遍整段 prompt）", text, len(*reqs))
		}
	}
}

// 救援指令的出口不能再让模型往正文里写字：上一版让模型回「已完成」，
// 那三个字原样拼进了客户端正文（issue #273 现象 3，52B + 9B = 61B 与日志对得上）。
// 正文是边收边转发的，发出去就收不回来——只能在指令层不产出它。
func TestNarrationNudgeAsksForSilence(t *testing.T) {
	for name, nudge := range map[string]string{
		"narrationNudge":        narrationNudge,
		"narrationPromiseNudge": narrationPromiseNudge,
	} {
		if strings.Contains(nudge, "已完成") {
			t.Errorf("%s 仍要求模型回「已完成」：这三个字会进客户端正文", name)
		}
		if !strings.Contains(nudge, "不要输出") {
			t.Errorf("%s 缺「已写完就什么都别输出」的出口：模型会写一段自辩进正文", name)
		}
	}
}

// 真实预告句仍要救：收窄判据不能把 issue #245 的那条路一起关掉
// （长正文 + 尾部「我将开始修改 internal/verify/check.go」）。
func TestNarrationPromiseTailStillRescued(t *testing.T) {
	text := strings.Repeat("我把三处改动逐行过了一遍，其中两点需要单独说明。", 8) +
		"现在我确认完了，我将开始修改 internal/server/continue.go 里的续写闸门"
	raw, _ := json.Marshal(text)
	seg := `data: {"id":"p1","choices":[{"index":0,"delta":{"content":` + string(raw) + `}}]}` + "\n\n" +
		`data: {"id":"p1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":301000,"completion_tokens":500,"total_tokens":301500}}` + "\n\n" +
		"data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(int) (int, string) { return 200, segToolOnlyText })
	r := newToolOnlyReader(t, up, chatBody, seg)
	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("承诺句收尾应救一次，实际 %d 次", len(*reqs))
	}
}
