// toolonly.go 「说一句就停，但下一步本该动手」的安全续写。
//
// 与 narration.go 的分工：narration 救的是**尾部落在一句预告上**的形态（冒号、
// 省略号、未闭合代码块、无句末标点）；这里救的是**尾部看着说完了、其实没干完**的
// 形态——线上原样样本（17:29:45，acct=150****5392，27 个工具，prompt 106,507）：
//
//	"这是免注册即可变更他人登记信息的接口：只靠邮箱就能改。这是高价值面，
//	 需要走通验证。先看完整表单。"
//
// finish_reason=stop、零 tool_calls、零报错。模型认为这轮说完了，客户端回合就此结束，
// 用户看到的就是「说到一半没了」。取证 156 条同类样本里，这种句号收尾占 38 条。
//
// 为什么不能像 narration 那样直接把续写正文接上去：句号收尾的样本里既有真正的
// 「马上要动手」，也有大量正常短答（角色扮演闲聊、明确在等用户回话、拒绝回答）。
// 接正文 = 把正常回答续成一段废话。
//
// 所以这里的采用条件是**只认工具调用**：
//   - 续写段拿到 tool_calls → 采纳（客户端立刻就能继续执行下一步）；
//   - 续写段只有正文 → 整段丢弃，客户端看到的内容与接线前**一模一样**。
//
// 判据刻意压窄，且误命中的代价只有「多打一次上游」（前缀缓存命中，费用接近 0）：
//  1. finish=stop、正文非空、零 tool_calls、无 error 帧；
//  2. 请求声明了工具，历史里有工具轮，**且最后一条消息就是工具结果**——模型刚看完
//     工具输出，本来该继续动手；闲聊/角色扮演的最后一跳通常是 user 或 assistant；
//  3. 正文很短（一眼扫过的一句话，< toolOnlyMaxBytes）；
//  4. 尾部不落在预告符上（那种由 narration 救，避免同一请求救两次）；
//  5. prompt 够长（> toolOnlyMinPrompt，长会话里的短输出才是异常；不知道就不救）；
//  6. 每请求一次。
package server

import (
	"bufio"
	"encoding/json"
	"log"
	"strings"

)

// toolOnlyMaxBytes 首段正文超过这个长度就不算「一句预告/一次提前收尾」。
//
// 取值依据（两台机器实测）：
//   - 预告句收尾那批（"…："/无标点）正文多集中在 40~200B；
//   - 但「写完一段就收尾」的形态要长得多——线上中断投诉对得上的两条是
//     completion 461 / 316 token（≈1400B / 950B），end 于句号、零工具调用；
//     该机今日 120 条同类请求的 completion 中位数是 825 token。
// 上限给到 2000B：能覆盖「提前收尾」这一带，又不至于把成篇的最终答复
// （p50≈2600B 以上）全拉进来。误命中的代价只是多一次上游调用——续写段只采纳
// 工具调用，纯正文整段丢弃，客户端看到的内容不变。
const toolOnlyMaxBytes = 2000

// toolOnlyMinPrompt 长会话门槛：短会话里模型给一句就收尾是正常的（问答、寒暄）。
const toolOnlyMinPrompt = 20000

// toolOnlyNudge 安全续写的指令。必须点明「别再用文字解释」——否则模型会再写一段
// 总结，那段正文会被丢弃，等于白发一次请求。
const toolOnlyNudge = "继续执行下一步。需要查看文件、执行命令或使用工具时，直接调用工具，不要再用文字解释或重复已有结论。如果确实已经全部完成，请只回复 DONE。"

// lastMessageIsToolResult 请求体的最后一条消息是否工具结果。
//
// 这是本判据里最强的结构信号：agent 循环里每次工具执行完都会带着「最后一条是
// tool 消息」再请求下一次；模型此时只写一句话就停，基本就是没接着动手。
// 闲聊/角色扮演的最后一跳是 user 或 assistant，不会命中。
func lastMessageIsToolResult(body []byte) bool {
	var obj struct {
		Msgs []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &obj) != nil || len(obj.Msgs) == 0 {
		return false
	}
	last := obj.Msgs[len(obj.Msgs)-1]
	return last.Role == "tool" || last.ToolCallID != ""
}

// toolIntentTail 尾部是否在「预告下一步要动手」。
//
// 这是本判据里唯一的语义闸门，也是默认关闭之外的**第二道**收缩：只靠
// 「末条是工具结果 + 正文短」会把 agent 循环里的正常收尾全部命中（实测 1608B 的
// 完整回答被判成截断）。这里要求最后 60 字节里出现前瞻动作词，且不出现收尾词
// （"已完成/如上/总结"），把「说完了」和「马上要动手」分开。
func toolIntentTail(text string) bool {
	tail := tailOf(strings.TrimSpace(text), 60)
	if tail == "" {
		return false
	}
	for _, done := range []string{"完成", "完毕", "如上", "以上", "总结", "结束", "搞定", "通过", "结论"} {
		if strings.Contains(tail, done) {
			return false
		}
	}
	for _, act := range []string{"先看", "先查", "先确认", "先读", "我先", "接下来", "下一步",
		"然后", "再去", "让我", "去看", "查看", "看一下", "检查", "执行", "调用", "跑一下", "试一下"} {
		if strings.Contains(tail, act) {
			return true
		}
	}
	return false
}

// tryToolOnlyRescue 安全续写：只采纳工具调用，正文一律丢弃。
//
// **缺省关闭**（features.tool_only_rescue）：只有「末条是工具结果 + 正文短」这一条
// 结构证据，而 agent 循环里正常收尾同样满足它——线上表现为「正常回答被判成截断，
// 白打一次上游，且续写段只采纳工具调用」。需要这条救援时必须显式打开配置，并且
// 仍要过 toolIntentTail 的语义闸门。
func (r *continueReader) tryToolOnlyRescue() bool {
	switch {
	case !r.toolOnlyRescue:
		return false
	case r.toolOnlyTried, r.narrationTried, r.errSeen, r.toolSeen, r.clientGone(), r.outputCapped():
		return false
	case r.finish != "stop":
		return false
	case r.text.Len() == 0 || r.text.Len() > toolOnlyMaxBytes:
		return false
	case r.seg >= maxContinueSegments-1:
		return false
	}
	// 尾部落在预告符上：交给 narration（那条会把正文接上去，比这里更合适）。
	if narrationTailSuspends(r.text.String()) {
		return false
	}
	// 语义闸门：尾部不像「马上要动手」就是正常收尾，不救（见 toolIntentTail）。
	if !toolIntentTail(r.text.String()) {
		return false
	}
	declared, toolTurn := bodyToolShape(r.body)
	if !declared || !toolTurn || !lastMessageIsToolResult(r.body) {
		return false
	}
	// prompt 未知（上游没给 usage）就不救：无法区分长会话与一句问答。
	if p, ok := r.usage["prompt_tokens"].(float64); !ok || p < toolOnlyMinPrompt {
		return false
	}
	body, err := r.continuationBodyWithNudge(toolOnlyNudge)
	if err != nil {
		return false
	}
	r.toolOnlyTried = true
	// 首段的终态帧先存起来：续写段如果只给正文，要原样回放这一份。
	r.origFinish, r.origTail = r.finish, r.tail
	rc, status, _, terr := r.up.ChatStreamContext(r.ctx, r.acct, body, r.clientIP, r.meta)
	if terr != nil || status >= 400 || rc == nil {
		if rc != nil {
			rc.Close()
		}
		r.origFinish, r.origTail = "", nil
		log.Printf("WARN: [toolonly] acct=%s seg=%d 续写请求失败（status=%d err=%v）→ 按原样收尾",
			r.label(), r.seg+1, status, terr)
		return false
	}
	r.seg++
	r.cur, r.br = rc, bufio.NewReaderSize(rc, 64*1024)
	r.finish, r.tail = "", nil
	r.toolOnly = true
	r.segCompletion, r.segThink = 0, 0
	log.Printf("[toolonly] acct=%s model=%s seg=%d 正文 %dB 且尾部预告动手 → 试续一次，只采纳工具调用",
		r.label(), bareModelOf(r.body), r.seg, r.text.Len())
	return true
}

// settleToolOnly 续写段结束时的裁决：拿到工具调用就采纳，否则整段回滚。
// 返回 true 表示已回滚（调用方继续走正常收尾路径，回放首段的终态帧）。
func (r *continueReader) settleToolOnly() bool {
	if !r.toolOnly {
		return false
	}
	r.toolOnly = false
	if r.toolSeen {
		// 采纳：续写段的 tool_calls 已经实时发给客户端了，收尾照常（回放本段 tail）。
		r.origFinish, r.origTail = "", nil
		log.Printf("[toolonly] acct=%s seg=%d 续写段拿到工具调用 → 采纳",
			r.label(), r.seg)
		return false
	}
	// 回滚：续写段只有正文（或什么都没有）→ 丢掉，客户端看到的内容与接线前一致。
	r.finish, r.tail = r.origFinish, r.origTail
	r.origFinish, r.origTail = "", nil
	log.Printf("[toolonly] acct=%s seg=%d 续写段没有工具调用（正文 %dB）→ 丢弃，按原样收尾",
		r.label(), r.seg, r.text.Len())
	return true
}

// discardToolOnlyDelta 在「只采纳工具调用」阶段剥掉正文/思考增量：这些字节不给客户端，
// 也不进本地累积（否则后续续写前缀里会混进被丢弃的内容）。
// 返回 true 表示该 delta 被改动过，需要重新序列化。
func discardToolOnlyDelta(d map[string]any) bool {
	changed := false
	if s, ok := d["content"].(string); ok && s != "" {
		delete(d, "content")
		changed = true
	}
	if s, ok := d["reasoning_content"].(string); ok && s != "" {
		delete(d, "reasoning_content")
		changed = true
	}
	return changed
}

// trimForLog 截断日志里的正文片段。
func trimForLog(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:] + "…" // 取尾部：收尾形态才是判据关心的
}

// tailOf 取字符串尾部 n 字节（按 rune 边界回退，避免截断半个汉字）。
func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && (s[i]&0xC0) == 0x80 {
		i++
	}
	return s[i:]
}
