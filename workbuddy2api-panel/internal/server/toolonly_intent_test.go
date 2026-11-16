// toolonly_intent_test.go 「够长 + 句号收尾」里的意图分流（上游 89cdfe7 的回归）。
//
// 背景：toolOnlyFinalAnswerBytes=120 这条闸门把「正文 ≥120B 且以句末标点收尾」当成
// 最终答复，于是
//
//	"我继续检查其他被……错误替换的内容，例如 manual 关键字、UnityManual.html 链接。"
//
// 被当成总结放过，客户端回合就此结束——用户看到「说到一半没了」。
//
// 修法：只看**最后一句**，有收尾词（总结/已完成/全部完成/完毕…）→ 仍算说完；
// 没有收尾词但有意图词（我继续/接下来/下一步/我来/现在开始/现在修复/再检查）→ 判为没写完，
// 交给 toolonly 续写（只采纳工具调用，正文丢弃，误判时用户看到的内容不变）。
//
// 本仓差异：features.tool_only_rescue **缺省关**，这条判据只在开关打开时才有机会生效，
// 所以末尾另立一条用例钉住「关着就不救」。
package server

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// 线上原样样本（第 108/109 轮）。两条本身只有百来字节，够不到 120B 闸门，
// 所以用例统一加 longPrefix 把它们推过闸门——否则测的是长度短路，不是意图分流。
const (
	line108 = "我继续检查其他被……错误替换的内容，例如 manual 关键字、UnityManual.html 链接。"
	line109 = "……统一替换成正确的 manual 路径，例如 UnityManual.html 链接。"
	// longPrefix 足够长的叙述前缀，用来把用例推过 120B 闸门（短句本来就不走这条判据）。
	longPrefix = "前面三处引用都已经逐个核对过，参数名与返回类型都对得上，没有发现其它调用点。"
	// longPrefixEN 英文形态的前缀（英文样本同样要先过 120B）。
	longPrefixEN = "All five call sites are listed above, none of them are dynamic, and the signatures match the header. "
)

func TestToolOnlyLooksFinishedIntent(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool // true = 当作最终答复（不救）
	}{
		{"108 意图词我继续", longPrefix + line108, false},
		// 109 那条样本本身没有意图词（只有"统一替换成…链接"），末句无判据 → 仍按说完处理。
		// 上游把它标成"意图词再检查"是因为样本不足 120B、被长度闸门短路，从未真正跑过这条判据。
		{"109 样本无意图词（仍算说完）", longPrefix + "前面三条都已经替换过。" + line109, true},
		{"下一步", longPrefix + "计划已经明确，下一步我直接改 router 配置。", false},
		{"接下来", longPrefix + "三处引用都已确认，接下来开始批量替换。", false},
		{"现在开始", longPrefix + "替换范围已经确定，现在开始改文件。", false},
		{"英文 let me", longPrefixEN + "Let me patch them now.", false},
		{"英文 next", longPrefixEN + "Next, I'll update them.", false},
		{"收尾词压过意图词", longPrefix + "任务已全部完成，我来总结一下。", true},
		{"已完成", longPrefix + "五个文件的替换都已经改完并核对，全部完成。", true},
		{"普通总结", longPrefix + "本轮共改动 4 个文件，均通过编译。", true},
		{"意图词在中间不在末句", longPrefix + "我继续检查了三个模块。全部改完。", true},
		{"短句仍是可救形态", "已改完。", false},
		{"无句末标点走 narration", longPrefix + "现在开始改 route 配置：", false},
	}
	for _, c := range cases {
		if got := toolOnlyLooksFinished(c.text); got != c.want {
			t.Fatalf("%s: toolOnlyLooksFinished(%q)=%v want %v", c.name, c.text, got, c.want)
		}
	}
}

func TestToolOnlyLastSentence(t *testing.T) {
	cases := []struct{ in, want string }{
		{"已改完。接下来修改 router。", "接下来修改 router"},
		{"单句没有分隔。", "单句没有分隔"},
		{"带英文句号. Let me check it.", " Let me check it"},
		{"结尾多个标点……ok。", "结尾多个标点……ok"},
		// 文件名里的 "." 不是句末：切成 "html 链接" 会把意图词整段切走（108 样本的形态）。
		{"改完了。我继续检查 UnityManual.html 链接。", "我继续检查 UnityManual.html 链接"},
		{"", ""},
		{"。。。", ""},
	}
	for _, c := range cases {
		if got := toolOnlyLastSentence(c.in); got != c.want {
			t.Fatalf("toolOnlyLastSentence(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

// 端到端：意图句（超过 120B、句号收尾）现在会触发 toolonly 续写并采纳工具调用；
// 而「任务已全部完成，我来总结一下。」仍然不触发。
func TestToolOnlyRescuesIntentSentence(t *testing.T) {
	intent := longPrefix + line108
	raw, _ := json.Marshal(intent)
	seg := `data: {"id":"t1","choices":[{"index":0,"delta":{"content":` + string(raw) + `}}]}` + "\n\n" +
		`data: {"id":"t1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":106507,"completion_tokens":48,"total_tokens":106555}}` + "\n\n" +
		"data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, segToolOnlyToolCall })
	r := newToolOnlyReader(t, up, toolLoopBody, seg)
	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("意图句应触发一次续写, 续写请求数=%d", len(*reqs))
	}
	// 续写指令必须点名「直接调用工具」。
	msgs, _ := reqBody(t, (*reqs)[0])["messages"].([]any)
	lastMsg, _ := msgs[len(msgs)-1].(map[string]any)
	if txt, _ := lastMsg["content"].(string); !strings.Contains(txt, "直接调用工具") {
		t.Fatalf("续写指令不含「直接调用工具」: %q", txt)
	}

	// 对照组：同样超过 120B、但最后一句是收尾 → 不触发。
	done := longPrefix + "任务已全部完成，我来总结一下。"
	rawDone, _ := json.Marshal(done)
	segDone := `data: {"id":"t1","choices":[{"index":0,"delta":{"content":` + string(rawDone) + `}}]}` + "\n\n" +
		`data: {"id":"t1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":106507,"completion_tokens":16,"total_tokens":106523}}` + "\n\n" +
		"data: [DONE]\n\n"
	up2, reqs2 := fakeContinueUpstream(t, func(call int) (int, string) { return 200, segToolOnlyToolCall })
	r2 := newToolOnlyReader(t, up2, toolLoopBody, segDone)
	if _, err := io.ReadAll(r2); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs2) != 0 {
		t.Fatalf("收尾句不该续写, 续写请求数=%d", len(*reqs2))
	}
}

// 本仓语义：features.tool_only_rescue 缺省关。意图分流只在开关打开时才有机会生效——
// 关着时同样的意图句一个续写请求都不该发（否则这条改动会顺手把救援变成默认开）。
func TestToolOnlyIntentRescueStillGatedBySwitch(t *testing.T) {
	intent := longPrefix + line108
	raw, _ := json.Marshal(intent)
	seg := `data: {"id":"t1","choices":[{"index":0,"delta":{"content":` + string(raw) + `}}]}` + "\n\n" +
		`data: {"id":"t1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":106507,"completion_tokens":48,"total_tokens":106555}}` + "\n\n" +
		"data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, segToolOnlyToolCall })
	r := newToolOnlyReader(t, up, toolLoopBody, seg)
	r.toolOnlyRescue = false // 缺省值：开关没打开
	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 0 {
		t.Fatalf("开关关闭时不该续写，实际 %d 次", len(*reqs))
	}
}
