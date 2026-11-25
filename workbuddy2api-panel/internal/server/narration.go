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
	"io"
	"log"
	"strings"
	"unicode/utf8"
)

// narrationNudge 救援续写的指令。与 continueNudge 的差别：那条针对「撞长度上限」，
// 强调不要重复；这条针对「自己以为说完了」，必须点明「你停在一句预告上」，
// 否则模型会以为任务已完成而再重复一遍结论。
//
// 末尾的出口是防假阳性：判据误触发时，模型否则会写一整段自辩（真机原话「上一条回复
// 并未中断，也没有以冒号结尾…」），那段自辩直接进客户端正文。
//
// 出口要求的是**零输出**而不是「只回一句『已完成』」：正文是边收边转发的，发出去就
// 收不回来（frame() 拿到 content delta 立刻 emit），那三个字会原样拼在正文末尾
// （issue #273 现象 3：52B + 9B = 61B，与日志逐字节对得上）。
const narrationNudge = "你上一条回复停在一句预告（例如以冒号结尾的「……：」）上就结束了，剩余内容还没写。请紧接着把剩下的部分写完：需要查看文件、执行命令或调用工具就直接调用，不要重复已经写过的内容，不要重新开头，不要道歉或解释。如果你确实已经写完了，什么都不要输出，直接结束这一轮。"

// 两条指令都以【】前缀自报身份（issue #273 第二条评论）：这条消息是以 role=user 追加的，
// agent 类客户端分辨不出它与真实用户输入的区别，实测被当成「用户批准」去改了工作区配置
// （客户端历史里还查不到）。前缀不能阻止模型执行，至少让它不再伪装成用户授权。
//
// narrationPromiseNudge 尾部是「我将…/我会…」这类承诺句时用这条。
//
// 不能复用上一句：那条说「停在冒号上」，而承诺句既没有冒号也不是预告排版，模型会
// 反过来跟你争「上一条并未中断」（真机实测原话），争论也会一并写进客户端正文。
// 出口同上：零输出，不写字——写出来的每个字都会进客户端正文。
const narrationPromiseNudge = "你上一条回复最后停在「我将…」「我会…」这类承诺句上就结束了，承诺的动作没有执行。请直接执行它：需要查看文件、执行命令或调用工具就直接调用，不要解释为什么停下，不要重复已经写过的内容，不要重新开头。如果你确实已经写完了，什么都不要输出，直接结束这一轮。"

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
//
// 这一组是明确对用户说的话，出现在正文任何位置都算：确认请求后面常跟着草稿本身
// （"请确认后我再发送。\n\n收件人：…\n主题：…"），只看最后一句就会把草稿当成末句、
// 漏掉它——那正是 #209 的事故形态（#260 回归）。英文项都是小写（与 ToLower 后的正文比）。
var userYieldMarkers = []string{
	"请确认", "请你确认", "你确认后", "等你确认", "待你确认", "等你回复", "让我知道",
	"please confirm", "let me know",
}

// userYieldDeliberation 也能用来问用户、但同样常见于技术叙述的措辞（上游真机样本：4083 字
// 正文停在「我将开始修改 internal/verify/check.go」上，命中的是三千字之前的「为“是否需要
// 更换”提供量化支撑」；同类还有「staging 确认后再上线」「日志格式确认无误」「如有问题请
// 告知维护者」）。
//
// 预告句救援对它们只认三种位置（见 yieldsToUser）：最后一句里；正文最后一个问句里、且问句
// 之后只剩没有句读的内容（草稿行）；后面紧跟「我 / 你」（"确认后我再发送"，「我们 / 你们」不算）。
// toolonly 仍按整段认——那条会采纳工具调用，正文又短，宁可少救。
var userYieldDeliberation = []string{
	"是否需要", "需不需要", "要不要", "确认后", "确认无误", "请告知", "你希望",
	"可以吗", "行吗", "好吗",
	"should i ", "shall i ", "do you want", "would you like", "awaiting your", "waiting for your", "confirm before",
}

// textYieldsToUser 报告这段正文是否在「等用户回话」：以问号收尾，或含请确认类措辞（整段）。
//
// 注意问号收尾在 narrationTailSuspends 里被判为「说完了」（句末标点），所以这条判据
// 必须独立成立——它挡的是「模型主动提问后停下」这个正确行为，而不是「话没说完」。
func textYieldsToUser(text string) bool { return yieldsToUser(text, true) }

// yieldsToUser 同 textYieldsToUser；deliberationAnywhere=false 时 userYieldDeliberation 只按
// 它注释里的三种位置认（预告句救援用，issue #245 / #260）。
func yieldsToUser(text string, deliberationAnywhere bool) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	if t == "" {
		return false
	}
	if strings.HasSuffix(t, "？") || strings.HasSuffix(t, "?") {
		return true
	}
	if containsAnyOf(t, userYieldMarkers) {
		return true
	}
	if deliberationAnywhere {
		return containsAnyOf(t, userYieldDeliberation)
	}
	return containsAnyOf(toolOnlyLastSentence(t), userYieldDeliberation) ||
		containsAnyOf(trailingQuestion(t), userYieldDeliberation) ||
		deliberationAddressesUser(t)
}

// trailingQuestion 正文最后一个问句——前提是它之后只剩没有句读的内容（"要不要现在发送？\n
// 收件人：…\n主题：…"，问句后面是草稿行）。问句之后还有完整的句子，说明模型接着往下说了，
// 那个问句只是叙述的一部分，返回空串。
func trailingQuestion(t string) string {
	i := strings.LastIndexAny(t, "？?")
	if i < 0 {
		return ""
	}
	_, n := utf8.DecodeRuneInString(t[i:])
	if strings.ContainsAny(t[i+n:], "。！？!?") {
		return ""
	}
	return toolOnlyLastSentence(t[:i+n])
}

// deliberationAddressesUser 正文里的 userYieldDeliberation 后面是否紧跟「我」「你」
// （"确认后我再发送"、"是否需要我先发给你过目"）。「我们 / 你们」是叙述主语，不算。
func deliberationAddressesUser(t string) bool {
	for _, m := range userYieldDeliberation {
		for rest := t; ; {
			i := strings.Index(rest, m)
			if i < 0 {
				break
			}
			rest = rest[i+len(m):]
			if (strings.HasPrefix(rest, "我") || strings.HasPrefix(rest, "你")) &&
				!strings.HasPrefix(rest, "我们") && !strings.HasPrefix(rest, "你们") {
				return true
			}
		}
	}
	return false
}

// containsAnyOf s 是否含 subs 里任一子串。
func containsAnyOf(s string, subs []string) bool {
	for _, m := range subs {
		if strings.Contains(s, m) {
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

// narrationPromiseMarkers 承诺下一步动作的措辞（"我将开始修改…"、"我需要先读取…"、"let me check…"）。
// 「接下来 / 下一步」单独出现不收：它们也常用来对用户说话（"接下来你可以运行 npm test"），
// 长正文里认它们会把写完的回答续写一段；带上动作的「接下来开始 / 现在开始」才收。
var narrationPromiseMarkers = []string{
	"我将", "我会", "我先", "我要", "我来", "我继续", "我现在", "让我",
	"我需要", "我得", "我打算", "我开始", "我再", "我接着", "我还要", "我马上", "我这就",
	"我们接着", "接下来开始", "现在开始", "正在",
	"let me", "let's", "i'll", "i will", "i'm ", "i am going to", "i need to", "i should", "now i",
}

// narrationPromiseNoise 含承诺词却不是承诺的固定说法，匹配前先剔掉（"对我来说" 里的「我来」）。
var narrationPromiseNoise = []string{"我来说", "我来看"}

// narrationTailPromises 最后一句是否在预告下一步动作（长正文无标点收尾时救援的前提，issue #245）：
// 含 narrationPromiseMarkers，或以现在分词开头的英文旁白（"Reading failfirst.sh mutation script"，
// 见 narrationTailSuspends 的样本）。
func narrationTailPromises(text string) bool {
	last := strings.ToLower(strings.TrimSpace(toolOnlyLastSentence(text)))
	for _, n := range narrationPromiseNoise {
		last = strings.ReplaceAll(last, n, "")
	}
	if containsAnyOf(last, narrationPromiseMarkers) {
		return true
	}
	first, _, _ := strings.Cut(last, " ")
	if len(first) < 5 || !strings.HasSuffix(first, "ing") {
		return false
	}
	for _, c := range first {
		if c < 'a' || c > 'z' {
			return false
		}
	}
	return true
}

// tailHangPunct 标点型悬停的收尾符（后面必然还有东西）。
const tailHangPunct = "：:，,、—–…（(【["

// narrationTailPunctuated 尾部是否「标点型悬停」：停在冒号/逗号/顿号/破折号/省略号/
// 左括号上。这类形态是排版习惯（「说明 + 列表：」几乎总以冒号收尾，issue #209），
// 只有它们才受 narrationMaxBytes 约束——正文越长越可能是正常排版。
//
// 与之相对的无标点悬停（"我将开始修改…"、"我会…"）是句子被掐断，与正文长度无关，
// 长会话里恰好最常见；按整段长度挡掉等于一条都不救（issue #245）。
func narrationTailPunctuated(text string) bool {
	t := strings.TrimRight(text, " \t\r\n")
	if t == "" {
		return false
	}
	rs := []rune(t)
	return strings.ContainsRune(tailHangPunct, rs[len(rs)-1])
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
	if strings.ContainsRune(tailHangPunct, last) {
		return true
	}
	// 句末标点与收尾符：正常说完的形态。
	// `>` 也归这一类：`<face:…>` / `<sticker:…>` / `<br>` 这类标签闭合即意味着说完了，
	// 而角色扮演客户端的正文几乎都以表情标签收尾、通篇不写句号（issue #273 原样本
	// "怎么啦妈 ||| 是干活累着了吗 <face:抱抱>"，52B 每条都被续写一次）。
	if strings.ContainsRune("。.!！?？\"'”’）)》】]}`*~&>", last) {
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

// narrationMaxRescues 每个请求最多救几次预告式收尾。
//
// 2 而不是 1：长正文的模型常常连写两段预告（"我先说明结论，然后我将开始修改…"），
// 只给一次机会时第二段又原样回到客户端。上限仍存在——不设上限会让「反复预告」的
// 模型把一条请求拖成无限续写，且每次续写都把整个上下文重发一遍（费用翻倍）。
// 总量另由 outputCap / atSegmentCap 兜底（本仓这两条闸门不动）。
const narrationMaxRescues = 2

// tryNarrationRetry 预告式收尾的救援续写（每请求最多 narrationMaxRescues 次）。
//
// 与另外两条救援的分工：
//   - tryStarvedRetry：本轮正文 0B（思考吃光/思考完直接收尾）；
//   - canContinue：finish_reason=length（撞长度上限）或无 finish 的断流；
//   - 这里：finish_reason=stop，正文有内容，但停在预告句上。
//
// 返回 true 表示已换到新段继续读，调用方直接 return。
func (r *continueReader) tryNarrationRetry() bool {
	switch {
	case r.narrationOff:
		return r.skip("narration", "disabled")
	case r.narrationTries >= narrationMaxRescues:
		return r.skip("narration", "tries_cap")
	case r.errSeen:
		return r.skip("narration", "err_frame")
	case r.toolSeen:
		return r.skip("narration", "tool_seen")
	case r.browserIncomplete:
		return r.skip("narration", "browser_incomplete")
	case r.browserCompleteTried:
		return r.skip("narration", "browser_complete_tried")
	case r.clientGone():
		return r.skip("narration", "client_gone")
	case r.outputCapped():
		return r.skip("narration", "output_capped")
	case r.finish != "stop":
		return r.skip("narration", "finish_not_stop")
	case r.text.Len() == 0:
		return r.skip("narration", "no_text")
	case r.text.Len() == r.narrationTextAt:
		// 上一次救援一个字都没写出来：再发的请求体与上一次一字不差，只是再付一次
		// 整段上下文的钱（issue #264）。
		return r.skip("narration", "no_new_text")
	case r.atSegmentCap():
		return r.skip("narration", "segment_cap")
	}
	txt := r.text.String()
	// 模型在等用户拍板（问句结尾 / 「请确认后我再…」）：这是**故意的**收尾。
	// 补一句「继续执行下一步」会拿到工具调用并被采纳，等于替用户按下发送键
	// （issue #209 两例：要确认的邮件被直接群发）。
	// 「是否需要 / 要不要」这类也常见于技术叙述的措辞只认问用户的用法（见 deliberationAddressesUser）。
	if yieldsToUser(txt, false) {
		return r.skip("narration", "yields_to_user")
	}
	// 救援指令早些时候的出口是「只回一句『已完成』」。文案已经换成「什么都不要输出」
	// （issue #273：那三个字会进客户端正文），但模型按旧习惯照回的情况仍会有——认它，
	// 免得白付第二次整段上下文。
	// 按后缀判而不是按「最后一句」判：续写段是直接拼上去的，与上一条之间没有分隔符，
	// toolOnlyLastSentence 会把两句连成一句。
	if tail := strings.TrimRight(txt, " 	\r\n「」\"'*_。"); strings.HasSuffix(tail, "已完成") ||
		strings.HasSuffix(strings.ToLower(tail), "done") {
		return r.skip("narration", "ack_done")
	}
	// 两道形态闸门都不让「聊天文风」过关（issue #273：角色扮演机器人「一两句 + ||| 分条 +
	// <face:…> 收尾、基本不写句号」，既不是标点型悬停也不是承诺句，于是每条正常回复都被
	// 续写一次——实测 87 次/天，单条计费 0.2 → 8.58 credits）：
	//   - 标点型悬停（"说明 + 列表："，issue #209）只救短正文，整段越长越可能是正常排版；
	//   - 无标点悬停必须含承诺词（"我将…"/"我会…"，issue #245）——那是句子被掐断的唯一
	//     可靠信号；长总结的最后一行（列表项、表格行、「全部测试通过 ✅」、文件路径）不含
	//     承诺词，一律放开的话每次都要重付一遍整段上下文。
	// 结构上确实没写完的（未闭合代码块 / 截断的调用标记）不受这两条限制。
	if !narrationTailStructurallyOpen(txt) {
		if narrationTailPunctuated(txt) {
			if len(txt) > narrationMaxBytes {
				return r.skip("narration", "tail_punct_long")
			}
		} else if !narrationTailPromises(txt) {
			return r.skip("narration", "tail_no_promise")
		}
	}
	if !narrationTailSuspends(txt) {
		return r.skip("narration", "tail_ends_sentence")
	}
	// 结构未闭合（截断的调用标记 / 未闭合代码块）只补一次：残缺标记留在前缀里，
	// 追加正文补不完它（续写段写多少都还是半截），再救也只是白花一次上游调用。
	if narrationTailStructurallyOpen(txt) && r.narrationTries >= 1 {
		return r.skip("narration", "structural_retry_cap")
	}
	declared, toolTurn := bodyToolShape(r.body)
	if !declared || !toolTurn {
		return r.skip("narration", "no_tool_history")
	}
	nudge := narrationNudge
	if !narrationTailPunctuated(txt) && !narrationTailStructurallyOpen(txt) {
		// 无标点悬停（承诺句）用另一条指令：说成「停在冒号上」模型会反过来争辩。截断的
		// 调用标记、未闭合代码块同样没有标点，但它们不是承诺句：告诉模型「你承诺的动作
		// 没执行」是答非所问（issue #264）。
		nudge = narrationPromiseNudge
	}
	body, err := r.continuationBodyWithNudge(nudge)
	if err != nil {
		return r.skip("narration", "body_build_failed")
	}
	r.narrationTries++
	r.narrationTextAt = r.text.Len()
	noteRescue("narration")
	// 异步换段：等待期内客户端只会收到 keepalive（见 openSegmentAsync）。
	return r.openSegmentAsync(switchNarration, body, func(rc io.ReadCloser) {
		r.seg++
		r.cur, r.br = rc, bufio.NewReaderSize(rc, 64*1024)
		r.finish, r.tail = "", nil
		// 本段 usage 快照归零：下一段的思考/正文构成单独计量。
		r.segCompletion, r.segThink = 0, 0
		log.Printf("[narration] acct=%s model=%s seg=%d 正文停在预告句（%dB）→ 续写剩余内容  尾部=%q",
			r.label(), bareModelOf(r.body), r.seg, r.text.Len(),
			trimForLog(tailOf(r.text.String(), 60), 60))
	}, func(status int, terr error) {
		log.Printf("WARN: [narration] acct=%s seg=%d 续写请求失败（status=%d err=%v）→ 按原样收尾",
			r.label(), r.seg+1, status, terr)
	})
}
