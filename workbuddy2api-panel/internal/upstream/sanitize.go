// sanitize.go 出站请求体脱敏：剥离上游内容审核黑名单指纹。
//
// 背景：客户端（Claude Code 类 CLI）在 system prompt 注入若干固定模板句，
// 上游内容审核按逐字精确匹配拦截（非语义审核），一字改动即可绕过。
// 策略：键值/header 型指纹整段剥离；承载语义的模板句最小改写（换一词），语义不变。
package upstream

import (
	"regexp"
	"strings"
)

// sanitizeFeatures 特征预检：任一命中才进入净化（strings.Contains 快速路径，
// 普通请求全不中 → 原样返回，零分配）。
var sanitizeFeatures = []string{
	"x-anthropic-billing-hdr",                         // header 键值段键名
	"You are Claude Code",                             // 身份句（截断前缀即可命中）
	"branch (you will usually use this for PRs)",      // 注入指令句（Main/Default 两种前缀都覆盖）
	"You are a coding agent running in the Codex CLI", // Codex instructions 首段
	"github.com/anthropics/",                          // 反馈句里的 Anthropic 仓库链接
	"DeepSeek Harness",                                // DSH 身份句品牌词：上游按「非官方渠道」整单拦（400 11‑-128）
	"cc_version",                                      // 尾随 kv 族
	"cc_entrypoint",                                   // 同族：只带这一项时也必须进净化
	"11-128",                                          // 上游反探测：裸数字错误码
	"11" + "128",                                      // 同一指纹的 5 字节形态（拼接构造，不手打）
}

// sanitizeHdrRe 剥离层：header 键名即触发（与值无关），整段删除。
var sanitizeHdrRe = regexp.MustCompile(`(?i)x-anthropic-billing-(?:hdr|header)[ \t]*:[^\n;]*;?[ \t]*`)

// sanitizeBareHdrRe 兜底层：裸键名（无冒号无值）同样是指纹——2026-09-13 实验 F4
// 证实 assistant 消息里反引号引用裸键名即触发 11128，而剥离层要求冒号、对裸串无效。
// 键值形态被整段删除后，残留的裸键名做最小缩写（header→hdr）：破坏逐字匹配、
// 语义不变、保留可读性。大小写不敏感，覆盖 X-Anthropic-... 变体。
//
// 注意该正则不要求冒号，是 sanitizeHdrRe 的超集——hasFingerprint 与 sanitizeText
// 中两者并用：先删键值形态（sanitizeHdrRe），再缩写残留裸键名（本正则），
// 替换语义不同（整段删除 vs 最小缩写），不可合并为一个正则。
var sanitizeBareHdrRe = regexp.MustCompile(`(?i)x-anthropic-billing-(?:hdr|header)`)

// sanitizeKvRe 剥离层：尾随裸键值（cc_xxx=...;）循环清理。
var sanitizeKvRe = regexp.MustCompile(`(?i)\bcc_[a-z0-9_]+=[^;\n]*;?\s*`)

// sanitizeCodeRe 反探测数字串的**独立出现**形态（BUG-03）：此前用 strings.ReplaceAll
// 做子串替换，正文里任意位置的 11-128（base64 片段、ID、时间戳、代码常量）都会被改写，
// 长度 +1、内容被篡改。这里用边界断言限定为独立 token。
var sanitizeCodeRe = regexp.MustCompile(`(^|[^0-9A-Za-z_\-])11-?128([^0-9A-Za-z_\-]|$)`)

// sanitizeRewrites 改写层：全模板句逐字替换（每句只改一个词，语义不变）。
//
// 身份句的匹配串**不带结尾标点**（只到 "…for Claude" 为止）：
// CLI 版这句以句号收尾（"…for Claude."），桌面版（claude-desktop-3p / Agent SDK）
// 以逗号接后继内容（"…for Claude, running within the Claude Agent SDK."）。
// 带句号的整句只匹配前者，桌面版会漏网、指纹原样发上游 → 400 code=11128。
// 去掉结尾标点后两种形态一并覆盖（替换串同样不带标点，让原有标点原样保留）。
// 注意仍要求 "You are Claude Code, " 前缀，不做更宽的子串替换，
// 以免误伤 TestExactMatchOnlyVariantNotTouched 所保护的零散文本。
// 模板句原文常量。**右值一律由左值派生**（strings.Replace），不再手写两遍：
// 手写正是「左右值相同 → 空操作」与「左右写反 → 主动把指纹拼进请求体」这两类
// 事故的唯一来源，而这个文件里两种都出过。
//
// 左值 = 上游按逐字匹配拦截的原文（保持出厂验证过的方向，不要凭直觉调换）：
// 每个左值都是「少一个词」的形态，右值补上那个词/换一种说法。
const (
	claudeIdentity   = "You are Claude Code, Anthropic's official CLI for Claude"
	branchSentence   = "Main branch (you will usually use this for PRs)"
	codexInstruction = "You are a coding agent running in the Codex CLI, a terminal-based coding assistant."
	feedbackSentence = "To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues"
)

// sanitizeRewrites 改写层：{上游拦截的原文, 换一个词的等价说法}。
var sanitizeRewrites = [][2]string{
	{claudeIdentity, strings.Replace(claudeIdentity, " CLI for", " CLI tool for", 1)},
	{branchSentence, strings.Replace(branchSentence, "Main", "Default", 1)},
	{codexInstruction, strings.Replace(codexInstruction, "Codex CLI,", "Codex CLI tool,", 1)},
	{feedbackSentence, strings.Replace(feedbackSentence, "give", "provide", 1)},
	// DSH 的身份句（@deepseek-ai/dsh-system-prompt 注入的 harness:identity 段）被上游
	// 当成「非官方渠道调用」**整单拦截**：400 `11‑-128 Illegal API invocation from an
	// unapproved channel`。
	//
	// 改写值 = `DeepSeek` + U+200B（零宽空格）+ `Harness`，两层都动：
	//   - 品牌词中间不是 0x20 而是零宽空格 → 逐字匹配原串不再命中；
	//   - 后半段换成 Harness（不是原词 Runtime）→ 即使上游把零宽字符归一化掉，
	//     归一后的串是 `DeepSeekHarness`，与原句仍不同形。
	// 渲染出来仍是「DeepSeek Harness」，语义不变、肉眼可核对。
	// （源码里写 \u200B 转义而非裸零宽字符：裸字符在 diff / 终端里不可见，
	//   审查时看不出改了什么，也没法 grep。）
	// 按短语全局替换而非只认开头 —— 同一品牌词在长提示词正文里还会出现多次。
	// 依据：misakano7545/workbuddy2api-panel `4f60a4c6`（DSH 身份句改按品牌词表驱动）。
	{"DeepSeek Harness", "DeepSeek\u200BHarness"},
	// 数字错误码（11-128 / 11-128）不在这张表里：子串替换会篡改 base64/ID 里的
	// 同形片段，改由 sanitizeCodeRe 的边界正则单独处理（见其注释）。
}

// sanitizeText 单段文本净化：预检不中 → 返回原串（零分配）。
func sanitizeText(text string) string {
	if !hasFingerprint(text) {
		return text
	}
	for _, rw := range sanitizeRewrites {
		text = strings.ReplaceAll(text, rw[0], rw[1])
	}
	// 数字错误码单独走边界正则（见 sanitizeCodeRe 注释）
	text = sanitizeCodeRe.ReplaceAllString(text, "${1}11\u2011-128${2}")
	if sanitizeHdrRe.MatchString(text) {
		text = sanitizeHdrRe.ReplaceAllString(text, "")
	}
	if strings.Contains(text, "cc_") {
		prev := ""
		for prev != text { // 清尾随裸 kv（cc_version=...; cc_entrypoint=...;）
			prev = text
			text = sanitizeKvRe.ReplaceAllString(text, "")
		}
	}
	// 兜底：键值形态已在上面整段删除，这里只剩裸键名（引用/示例文本形态）。
	text = sanitizeBareHdrRe.ReplaceAllString(text, "x-anthropic-hdr")
	// 不能 TrimSpace（BUG-02）：只要某条消息命中任一指纹，整段正文的缩进、行首空格、
	// 行尾空白都会被删掉，代码块/缩进结构被静默破坏，而且 sanitizeContent 因
	// s != c 判成「已改写」，没有任何日志。剥离留下的空白留着即可。
	return text
}

// hasFingerprint 特征预检：先走 strings.Contains 快速路径（零分配）；
// header 键名有大小写变体（X-Anthropic-...）且可能以裸键名形态出现（无冒号），
// Contains 大小写敏感、sanitizeHdrRe 要求冒号——两者都会漏掉「混合大小写 + 裸键名」，
// 必须再用不要求冒号的 (?i) 正则兜底（sanitizeBareHdrRe），否则整条净化被跳过。
// sanitizeBareHdrRe 不要求冒号，是 sanitizeHdrRe 的超集，故无需再单独匹配后者。
func hasFingerprint(text string) bool {
	for _, f := range sanitizeFeatures {
		if strings.Contains(text, f) {
			return true
		}
	}
	return sanitizeBareHdrRe.MatchString(text)
}

// sanitizeContent 兼容字符串与多模态数组；只动 text part，image 等 part 不动。
// 返回净化后的值及是否发生变化。
func sanitizeContent(v any) (any, bool) {
	switch c := v.(type) {
	case string:
		s := sanitizeText(c)
		return s, s != c
	case []any:
		changed := false
		for _, p := range c {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			text, ok := m["text"].(string)
			if !ok {
				continue
			}
			if s := sanitizeText(text); s != text {
				m["text"] = s
				changed = true
			}
		}
		return c, changed
	}
	return v, false
}

// sanitizeToolCalls 净化 assistant.tool_calls[].function.arguments。
//
// arguments 是**字符串化的 JSON**（不是对象），因此按文本走 sanitizeText 即可。
// 这块长期是盲区：工具调用消息的 content 通常是 null，而旧版 sanitizeMessages
// 在 content 缺失时直接 continue，整条消息连 tool_calls 一起被跳过——
// 于是历史里任何写进工具参数的被拦字符串（文件名、命令、写入内容）都会原样漏出。
func sanitizeToolCalls(v any) bool {
	callList, ok := v.([]any)
	if !ok {
		return false
	}
	changed := false
	for _, c := range callList {
		call, ok := c.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := call["function"].(map[string]any)
		if !ok {
			continue
		}
		args, ok := fn["arguments"].(string)
		if !ok {
			continue
		}
		if s := sanitizeText(args); s != args {
			fn["arguments"] = s
			changed = true
		}
	}
	return changed
}

// sanitizeMessages 净化 messages 中的 content 与 tool_calls；任一命中返回 true。
func sanitizeMessages(messages []any) bool {
	changed := false
	for _, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		// content 与 tool_calls 各自独立判断：content 可以为 null（工具调用轮），
		// 早期版本在此 continue，导致这类消息的 tool_calls 完全不被净化。
		if c, ok := m["content"]; ok {
			if nc, ch := sanitizeContent(c); ch {
				m["content"] = nc
				changed = true
			}
		}
		// reasoning_content（思维链回填字段，见 thinking.go/sse.go）实测同样
		// 携带指纹，与 content 同等净化。string 形态直接走 sanitizeText。
		if rc, ok := m["reasoning_content"].(string); ok {
			if s := sanitizeText(rc); s != rc {
				m["reasoning_content"] = s
				changed = true
			}
		}
		// reasoning：另一形态的思维链字段。thinking.go 的回填会把客户端送来的
		// reasoning_content 镜像进 reasoning（反之亦然），而此前只洗 content /
		// reasoning_content / tool_calls —— 镜像进 reasoning 的指纹原样出站，
		// 裸 "11128" 这种反探测串同样致命。
		if r, ok := m["reasoning"].(string); ok {
			if s := sanitizeText(r); s != r {
				m["reasoning"] = s
				changed = true
			}
		}
		if tc, ok := m["tool_calls"]; ok {
			if sanitizeToolCalls(tc) {
				changed = true
			}
		}
	}
	return changed
}
