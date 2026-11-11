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

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"

)

// toolOnlyMaxBytes 首段正文超过这个长度就不算「一句预告/一次提前收尾」。
//
// 取值依据（两台机器实测）：
//   - 预告句收尾那批（"…："/无标点）正文多集中在 40~200B；
//   - 「写完一段就收尾」要长得多：中断投诉对得上的两条是 completion 461 / 316 token
//     （≈1400B / 950B），句号收尾、零工具调用；
//   - 按 25 分钟样本放宽到 N token 的覆盖率（prompt≥8k 那批 46 条）：
//     ≤650(2000B) 41% / ≤1000 76% / ≤2500 87% / 不设限 100%。
// 上限取 2000B。曾试过放到 7500B（覆盖 41%→77%），但上线后出现反向副作用：
// 已经写完的任务被推着再调一次工具（用户反馈「任务该结束了却一直在工具调用」），
// 因为「模型肯再调一次」并不等于「它还没做完」。在没有可靠判据区分「提前收尾」与
// 「已经答完」之前，宁可少救：救援只在正文明显短（一句预告）时才触发。
const toolOnlyMaxBytes = 2000

// toolOnlyMinPrompt 长会话门槛：几百 token 的短问答里，模型给一句就收尾是正常的。
// 实测「被门槛挡掉」的那批都是 4k~10k prompt（AsyncOpenAI / Go-http-client 的小会话），
// 给到 8000 能覆盖它们，又不会把一句话问答也算进来。
const toolOnlyMinPrompt = 20000

// toolOnlyFinalAnswerBytes 正文长度达到这个数、且以句末标点收尾，就当作「最终答复」不救。
//
// 为什么需要这条（2026-11-11 实测）：08:00–09:00 一共 29 次救续，**29 次全部被采纳**，
// 正文长度是 39/66/73/148/150/157/189/198/198/366/…/1917B。一个「只在模型提前收尾时
// 救一次」的机制如果采纳率 100%，说明它事实上在给每个 agent 回合无条件追加一次工具
// 调用——Hermes Agent 这种「每步收尾都写一句简短状态」的客户端因此永远结束不了回合。
//
// 判据取「长度 + 尾部」而不是只取尾部：尾部句号在两种形态里都出现（真收尾、写完一段
// 再停），唯一稳定区分的是长度——预告句（"我先看一下："）实测集中在 40~200B，而真正
// 写完的总结通常在 150B 以上。120B 落在两者之间，偏保守地保住了「短预告仍可救」。
const toolOnlyFinalAnswerBytes = 120

// toolOnlyLooksFinished 报告正文是否已经像「最终答复」：够长 + 以句末标点收尾。
// 短句（"好的。"）不算——那正是这条救援原本要覆盖的形态。
func toolOnlyLooksFinished(text string) bool {
	s := strings.TrimSpace(text)
	if s == "" || len(s) < toolOnlyFinalAnswerBytes {
		return false
	}
	// narrationTailSuspends 对句末标点返回 false；冒号/逗号/未闭合代码块返回 true。
	return !narrationTailSuspends(s)
}

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
	// 正文里含工具调用标记（DSML / XML 形态）：客户端会自己解析执行，这不是「提前收尾」。
	if hasToolCallMarkup(r.text.String()) {
		return false
	}
	// 正文已经像最终答复（够长 + 以句末标点收尾）：不救。见 toolOnlyFinalAnswerBytes
	// 的实测依据——不加这条时 agent 客户端的每个回合都会被推着多调一次工具。
	if toolOnlyLooksFinished(r.text.String()) {
		return false
	}
	// 尾部落在预告符上：交给 narration（那条会把正文接上去，比这里更合适）。
	if narrationTailSuspends(r.text.String()) {
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
//
// 采纳前还要过一道闸门（2026-11-11）：续写段的工具调用若与「上一次已执行的调用」同名
// 同参，说明模型只是把刚跑完的那次原样重发——采纳它等于让客户端把同一个写操作再执行
// 一遍，回合也永远结束不了。为此 toolOnly 段的工具调用帧不再边到边发，而是先攒在
// toolCallBuf 里，这里判完再决定补发还是整段丢弃（判重复时客户端一个字节都收不到，
// 与「没有工具调用」的回滚路径完全一致）。
func (r *continueReader) settleToolOnly() bool {
	if !r.toolOnly {
		return false
	}
	r.toolOnly = false
	if r.toolSeen {
		if name, dup := r.duplicatesLastExecutedCall(); dup {
			r.toolCallBuf = nil
			r.toolSeen = false
			r.finish, r.tail = r.origFinish, r.origTail
			r.origFinish, r.origTail = "", nil
			log.Printf("[toolonly] acct=%s model=%s seg=%d 续写段要重发上一次的调用（%s）→ 丢弃，按原样收尾",
				logfmt.Label(r.acct.UID, r.acct.NicknameValue()), bareModelOf(r.body), r.seg, name)
			return true
		}
		// 采纳：把缓存的工具调用帧按原顺序补发，收尾照常（回放本段 tail）。
		for _, p := range r.toolCallBuf {
			r.emit(p)
		}
		r.toolCallBuf = nil
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

// captureToolCallDeltas 累积「只采纳工具调用」段里的工具调用分片：name 与 arguments
// 都是分多个 chunk 到达的，必须按 index 拼起来才能与历史做过比较。
func (r *continueReader) captureToolCallDeltas(tcs []any) {
	if r.toolCallNames == nil {
		r.toolCallNames = map[int]string{}
		r.toolCallArgs = map[int]string{}
	}
	for _, raw := range tcs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		idx := 0
		if v, ok := m["index"].(float64); ok {
			idx = int(v)
		}
		fn, ok := m["function"].(map[string]any)
		if !ok {
			continue
		}
		if s, ok := fn["name"].(string); ok {
			r.toolCallNames[idx] += s
		}
		if s, ok := fn["arguments"].(string); ok {
			r.toolCallArgs[idx] += s
		}
	}
}

// recordedToolCall 请求体历史里的一次工具调用。
type recordedToolCall struct{ Name, Args string }

// lastExecutedToolCalls 取请求体里「最后一个带 tool_calls 的 assistant 消息」的调用列表。
//
// 这是「客户端刚刚执行过什么」的唯一权威来源：连续两轮都用同一条消息，模型重发时
// 名字与参数都会一模一样，可以逐字比对。
func lastExecutedToolCalls(body []byte) []recordedToolCall {
	var obj struct {
		Msgs []struct {
			Role      string `json:"role"`
			ToolCalls []struct {
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
			// 旧版单数形态：{"function_call":{"name","arguments"}}
			FunctionCall *struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function_call"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &obj) != nil {
		return nil
	}
	var out []recordedToolCall
	for _, m := range obj.Msgs {
		if m.Role != "assistant" {
			continue
		}
		var calls []recordedToolCall
		for _, tc := range m.ToolCalls {
			calls = append(calls, recordedToolCall{Name: tc.Function.Name, Args: tc.Function.Arguments})
		}
		if m.FunctionCall != nil {
			calls = append(calls, recordedToolCall{Name: m.FunctionCall.Name, Args: m.FunctionCall.Arguments})
		}
		if len(calls) > 0 {
			out = calls // 保留最后一个带调用的 assistant 消息
		}
	}
	return out
}

// duplicatesLastExecutedCall 报告续写段要的第一个工具调用是否与上一次执行过的完全相同。
func (r *continueReader) duplicatesLastExecutedCall() (string, bool) {
	if len(r.toolCallNames) == 0 {
		return "", false
	}
	name := strings.TrimSpace(r.toolCallNames[0])
	if name == "" {
		return "", false
	}
	args := normalizeToolArgs(r.toolCallArgs[0])
	for _, c := range lastExecutedToolCalls(r.body) {
		if strings.TrimSpace(c.Name) == name && normalizeToolArgs(c.Args) == args {
			return name, true
		}
	}
	return "", false
}

// normalizeToolArgs 归一化工具参数用于比较：空串与 "{}" 等价，键序与空白不敏感。
//
// 参数是 JSON 文本，比较的是「同一次调用」而不是字节：模型重发时常把 {} 写成 { }，
// 直接字符串比较会漏判。解析失败时退回「去掉所有空白」，至少挡住逐字重发。
func normalizeToolArgs(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || s == "{}" {
		return "{}"
	}
	var v any
	if json.Unmarshal([]byte(s), &v) == nil {
		if raw, err := json.Marshal(v); err == nil {
			return string(raw)
		}
	}
	return strings.Join(strings.Fields(s), "")
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
