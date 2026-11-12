// narration.go 「说一句预告就停」的救援续写。
//
// 现象（线上取证，2026-11-09 09:05/09:07 两条 stop-no-toolcall 样本）：
// 客户端声明了 32 个工具、历史里全是工具结果，模型在正文里写完一段结论，然后
// 用一句**预告**收尾就停了——
//
//	"5G 已降到 **10 dBm**（功率降低约 32 倍）。重新连接并测试下行："
//	"验证路由器状态并做对比实验（恢复到 25dBm 看是否必然重启）："
//
// finish_reason=stop、零 tool_calls、零报错，上游认为这轮说完了。网关按契约
// 原样透传，客户端就渲染成「说到一半没了」——这正是截图里那种中断：**没有任何
// 一层报错**，日志里既没有 length 也没有断流，所以续写/救援逻辑一次都不会触发。
//
// 救援动作：把已输出正文作为 assistant 前缀，补一句「接着写完、需要工具就直接
// 调用」重发一次。拿到正文就接在客户端流里（同一条 SSE），拿到工具调用就原样
// 透传；两样都没拿到就按原样收尾，行为与接线前完全一致。
//
// 判据刻意窄，四层同时成立才救，避免碰正常回答：
//  1. finish_reason=stop 且正文非空、零 tool_calls、无 error 帧；
//  2. 请求体声明了工具，**且历史里有工具结果**（角色扮演类客户端只发纯文本、
//     不产生工具轮，它们用冒号收尾是正常文风，不能救）；
//  3. 正文尾部是「未说完」形态（冒号/省略号/未闭合代码块/没有句末标点）；
//  4. 每请求最多一次（不套娃，续写段再停就按原样收尾）。
package server

import (
	"bufio"
	"encoding/json"
	"log"
	"strings"

)

// narrationNudge 救援续写的指令。与 continueNudge 的差别：那条针对「撞长度上限」，
// 强调不要重复；这条针对「自己以为说完了」，必须点明「你停在一句预告上」，
// 否则模型会以为任务已完成而再重复一遍结论。
const narrationNudge = "你上一条回复停在一句预告（例如以冒号结尾的「……：」）上就结束了，剩余内容还没写。请紧接着把剩下的部分写完：需要查看文件、执行命令或调用工具就直接调用，不要重复已经写过的内容，不要重新开头，不要道歉或解释。"

// toolCallMarkupMarkers 模型把工具调用写成**文本标记**时用到的特征串。
//
// 线上实测（DSH / deepseek-harness，2026-11-10 21:30 起 40 分钟）：76 次 narration
// 触发里 25 次（33%）正文尾部是 DSML 标记，典型形态
//
//	"…</｜｜DSML｜｜ invoke> </｜｜DSML｜｜ calls>"
//
// 这类文本**本身就是一次工具调用**，客户端（DSH）会自己解析并执行它。网关只看到
// 「正文」，又因为尾部不是句末标点就判定「话没说完」→ 补发续写指令 → 模型再发一次
// 调用 → 客户端再执行一次。用户看到的就是「任务早该结束了，却一直在工具调用」。
//
// 所以判据必须分两种状态（这也是「DSML 修了又像没修」的来由）：
//
//	已有完整块 → "…</｜｜DSML｜｜ invoke> </｜｜DSML｜｜ calls>"
//	             客户端会解析执行这条调用，**不能**续写（续写会让模型再发一次，
//	             用户看到「任务该结束了却一直在工具调用」）。
//	截断块     → "…2>&1</｜｜DSML｜｜ parameter>"
//	             调用写到一半流就断了，客户端解析不出来，界面留下半截标记垃圾
//	             （用户反馈的截图就是这个）。这种**必须**续写把块补完。
var toolCallMarkupMarkers = []string{
	"｜｜DSML｜｜", "|DSML|", // DeepSeek 的 DSML 标记（全角/半角竖线两种写法都出现过）
	"</function_calls>", "<function_calls>",
	"antml:invoke", "<invoke name=",
	"</tool_calls>", "<tool_calls>", // 少数 prompt 会把工具调用格式教成这种 XML
}

// markupState 正文里「写成文本的工具调用」的完整程度。
type markupState int

const (
	markupNone     markupState = iota // 没有工具调用标记
	markupComplete                    // 标记块已闭合：这是一次完整的调用
	markupPartial                     // 标记中间断了：调用写了一半
)

// toolCallMarkupState 判断正文里工具调用标记的状态。两类样本的处理方式相反，见上。
func toolCallMarkupState(text string) markupState {
	if text == "" {
		return markupNone
	}
	found := false
	for _, m := range toolCallMarkupMarkers {
		if strings.Contains(text, m) {
			found = true
			break
		}
	}
	if !found {
		return markupNone
	}
	t := strings.TrimRight(text, " \t\r\n")
	switch {
	case strings.HasSuffix(t, "</function_calls>"), strings.HasSuffix(t, "</tool_calls>"):
		return markupComplete
	case strings.HasSuffix(t, "calls>") && strings.Contains(t, "DSML"):
		return markupComplete // DSML 的调用块以 "</｜｜DSML｜｜ calls>" 收尾
	}
	return markupPartial
}

// hasToolCallMarkup 正文里是否出现了「写成文本的工具调用」（不论完整与否）。
// 供 toolonly 使用：那条只采纳结构化 tool_calls、正文一律丢弃，两种状态都不能碰
// （截断态需要的是「接着写」，那是 narration 的活）。
func hasToolCallMarkup(text string) bool {
	return toolCallMarkupState(text) != markupNone
}

// userYieldMarkers 正文里「把控制权交回用户」的措辞。
//
// 为什么必须有这条判据（2026-11-11 issue #209）：救援的语义是「模型没说完，替它
// 补上」，但有一类收尾是**故意的**——模型在等用户拍板（"邮件已经写好，请确认后我再
// 发送。"）。这时补一句"继续执行下一步"会得到工具调用并被采纳，客户端于是真的把
// 邮件发出去了。线上已发生两例：用户要求先确认再发，结果 agent 直接群发。
//
// 判据取「措辞 + 问号」，不猜语义：宁可漏救（客户端自己重试一轮的代价是几秒），
// 也不能替用户按下发送键。
var userYieldMarkers = []string{
	"请确认", "确认后", "等你确认", "待你确认", "等你回复", "等你确认后",
	"是否需要", "需不需要", "要不要我", "要不要", "你希望", "请告知",
	"让我知道", "请你确认", "你确认后", "确认无误", "可以吗", "行吗", "好吗",
}

// userYieldMarkersEN 英文形态（模型偶发用英文收尾）。
var userYieldMarkersEN = []string{
	"please confirm", "let me know", "should i ", "do you want", "would you like",
	"awaiting your", "waiting for your", "confirm before", "shall i ",
}

// textYieldsToUser 报告这段正文是否在「等用户回话」：以问号收尾，或含请确认类措辞。
//
// 注意问号收尾在 narrationTailSuspends 里被判为「说完了」（句末标点），所以这条判据
// 必须独立成立——它挡的是「模型主动提问后停下」这个正确行为，而不是「话没说完」。
func textYieldsToUser(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return false
	}
	if strings.HasSuffix(t, "？") || strings.HasSuffix(t, "?") {
		return true
	}
	for _, m := range userYieldMarkers {
		if strings.Contains(t, m) {
			return true
		}
	}
	low := strings.ToLower(t)
	for _, m := range userYieldMarkersEN {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// narrationMaxBytes narration 的长度闸门（只对「标点型尾部」生效）。
//
// 这条救援原本没有长度上限：正文多长、只要停在冒号/逗号上就续写一次，而 agent 客户端
// 的「说明 + 列表」几乎总是以冒号或逗号收尾（issue #209 的复现条件之一）。预告句实测
// 集中在 40~200B，400B 已覆盖全部真实预告样本，再长的「停在冒号上」更可能是正常排版。
//
// 未闭合代码块与截断的工具调用标记不受这条限制：那是结构上确实没写完，与长度无关。
const narrationMaxBytes = 400

// narrationTailStructurallyOpen 尾部是否「结构上没写完」（未闭合代码块 / 截断的调用标记）：
// 这类形态长度不设限，见 narrationMaxBytes。
func narrationTailStructurallyOpen(text string) bool {
	if toolCallMarkupState(text) == markupPartial {
		return true
	}
	return strings.Count(strings.TrimRight(text, " \t\r\n"), "```")%2 == 1
}

// narrationTailSuspends 正文尾部是否「话没说完」。
//
// 只认结构性的收尾符，不猜测语义：
//   - 全角/半角冒号、逗号、顿号、破折号、省略号、左括号 → 后面必然还有东西；
//   - 未闭合的 ``` 代码块 → 代码块写了一半；
//   - 末尾既不是句末标点也不是收尾符号 → 视为未写完（英文旁白
//     "Reading failfirst.sh mutation script" 这种就是这一形态：7 个 token
//     的现在分词短语，没有任何标点）。
//
// 例外：工具调用标记按**完整性**分流（见 toolCallMarkupState）——
// 完整块返回 false（客户端会执行它，续写会造出第二次调用）；截断块返回 true
// （调用写到一半断了，必须续写把块补完，否则客户端只看到半截标记）。
func narrationTailSuspends(text string) bool {
	switch toolCallMarkupState(text) {
	case markupComplete:
		return false
	case markupPartial:
		return true
	}
	t := strings.TrimRight(text, " \t\r\n")
	if t == "" {
		return false
	}
	// 结构性收尾：该块已经完整。
	if strings.HasSuffix(t, "</summary>") || strings.HasSuffix(t, "</details>") ||
		strings.HasSuffix(t, "</analysis>") {
		return false
	}
	if strings.Count(t, "```")%2 == 1 {
		return true
	}
	rs := []rune(t)
	last := rs[len(rs)-1]
	if strings.ContainsRune("：:，,、—–…（(【[", last) {
		return true
	}
	// 句末标点与收尾符：正常说完的形态。
	if strings.ContainsRune("。.!！?？\"'”’）)》】]}`*~&", last) {
		return false
	}
	return true
}

// bodyToolShape 请求体是否「工具类请求」：声明了工具、且历史里有工具轮。
//
// 两个条件缺一不可：
//   - 只声明工具不够——角色扮演/闲聊客户端常常把一堆工具挂上去却只用纯文本回答
//     （线上取证 40 条样本里 20 条是这种），它们的「以冒号收尾」是正常文风；
//   - 有工具轮说明这是一条 agent 会话：模型本来就该在这一步调工具或继续写。
//
// 解析失败按「不是工具请求」处理（不救），宁可少救不可救错。
func bodyToolShape(body []byte) (declared, toolTurn bool) {
	var obj struct {
		Tools []any `json:"tools"`
		Msgs  []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []any  `json:"tool_calls"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &obj) != nil {
		return false, false
	}
	declared = len(obj.Tools) > 0
	for _, m := range obj.Msgs {
		if m.Role == "tool" || m.ToolCallID != "" || len(m.ToolCalls) > 0 {
			return declared, true
		}
	}
	return declared, false
}

// tryNarrationRetry 预告式收尾的救援续写（每请求一次）。
//
// 与另外两条救援的分工：
//   - tryStarvedRetry：本轮正文 0B（思考吃光/思考完直接收尾）；
//   - canContinue：finish_reason=length（撞长度上限）或无 finish 的断流；
//   - 这里：finish_reason=stop，正文有内容，但停在预告句上。
//
// 返回 true 表示已换到新段继续读，调用方直接 return。
func (r *continueReader) tryNarrationRetry() bool {
	switch {
	case r.narrationTried, r.errSeen, r.toolSeen, r.clientGone(), r.outputCapped():
		return false
	case r.finish != "stop":
		return false
	case r.text.Len() == 0:
		return false
	case r.seg >= maxContinueSegments-1:
		return false
	}
	txt := r.text.String()
	// 模型在等用户拍板（问句结尾 / 「请确认后我再…」）：这是**故意的**收尾。
	// 补一句「继续执行下一步」会拿到工具调用并被采纳，等于替用户按下发送键
	// （issue #209 两例：要确认的邮件被直接群发）。
	if textYieldsToUser(txt) {
		return false
	}
	// 长度闸门：预告句实测 40~200B，400B 已覆盖；再长的「停在冒号上」多半是正常排版。
	// 结构上确实没写完的（未闭合代码块 / 截断的调用标记）不受此限。
	if !narrationTailStructurallyOpen(txt) && len(txt) > narrationMaxBytes {
		return false
	}
	if !narrationTailSuspends(txt) {
		return false
	}
	declared, toolTurn := bodyToolShape(r.body)
	if !declared || !toolTurn {
		return false
	}
	body, err := r.continuationBodyWithNudge(narrationNudge)
	if err != nil {
		return false
	}
	r.narrationTried = true
	rc, status, _, terr := r.up.ChatStreamContext(r.ctx, r.acct, body, r.clientIP, r.meta)
	if terr != nil || status >= 400 || rc == nil {
		if rc != nil {
			rc.Close()
		}
		log.Printf("WARN: [narration] acct=%s seg=%d 续写请求失败（status=%d err=%v）→ 按原样收尾",
			r.label(), r.seg+1, status, terr)
		return false
	}
	r.seg++
	r.cur, r.br = rc, bufio.NewReaderSize(rc, 64*1024)
	r.finish, r.tail = "", nil
	// 本段 usage 快照归零：下一段的思考/正文构成单独计量。
	r.segCompletion, r.segThink = 0, 0
	log.Printf("[narration] acct=%s model=%s seg=%d 正文停在预告句（%dB）→ 续写剩余内容  尾部=%q",
		r.label(), bareModelOf(r.body), r.seg, r.text.Len(),
		trimForLog(tailOf(r.text.String(), 60), 60))
	return true
}
