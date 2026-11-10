// sse_frame_split.go 单个 data 帧里塞了多个 JSON 对象时的拆分（issue #197）。
//
// 现象：客户端（zcode 等）报
//
//	JSON parsing failed: Text: {"id":"..."} {"id":"..."}
//	Unexpected non-whitespace character after JSON at position 348 (line 2 column 1)
//
// 按 SSE 规范，同一事件的多行 data 会用 "\n" 拼成一份文本交给客户端；而一个
// `data: {a} {b}` 行本身就是「一份文本里有两条 JSON」，客户端无论怎么读都只能
// 解析第一个——整轮对话直接作废，而且报错在读侧，看不出是上游粘帧。
//
// 本文件只做一件事：把 `{a} {b}` / `{a}{b}` 这种**首尾相接的多个 JSON 值**切成
// 独立字符串，交给调用方逐个当普通帧处理。切不动（不是 JSON、括号不配平）就返回
// 单元素，调用方保持原来的「原样透传」行为，不改变任何既有形态。
package upstream

import (
	"encoding/json"
	"strings"
)

// maxSplitParts 单帧最多拆出多少份。上游粘帧是「一两个」，这里给到 64 只为兜住
// 极端畸形输入——不设上限的话，一个几百 KB 的坏帧会被切成上千次写操作。
const maxSplitParts = 64

// looksLikeJSON 判断一份 payload 是否**打算**是 JSON（首字符是 { 或 [）。
//
// 只用来决定要不要打 WARN：上游有些形态确实是非 JSON 的行（例如纯文本心跳），
// 那些静默透传是对的；以 { 开头却解析失败才是需要人看的故障。
func looksLikeJSON(payload string) bool {
	t := strings.TrimLeft(payload, " \t\r\n")
	if t == "" {
		return false
	}
	return t[0] == '{' || t[0] == '['
}

// splitConcatenatedJSON 把首尾相接的多个 JSON 值切成独立字符串。
//
// 切法：按字节扫描，跟踪字符串状态（含 \ 转义）与 {}、[] 嵌套深度；深度从 0 回到 0
// 且下一个非空白字节与当前值首尾相接，就切一刀。每个值单独再验证一次 json 合法性
// ——宁可少切（返回单元素走原样透传），也不产出半截对象把客户端带偏。
//
// 返回值：len>1 表示确实拆开了；len==1 表示「本来就是一个值」或「切不动」。
func splitConcatenatedJSON(payload string) []string {
	trimmed := strings.TrimSpace(payload)
	if trimmed == "" {
		return []string{payload}
	}
	var (
		parts   []string
		depth   int
		inStr   bool
		escaped bool
		start   = -1
	)
	for i := 0; i < len(trimmed); i++ {
		c := trimmed[i]
		if start < 0 {
			// 值之间允许空白；真遇到非空白就开一个新值。
			if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
				continue
			}
			if c != '{' && c != '[' && c != '"' && c != '-' && (c < '0' || c > '9') &&
				c != 't' && c != 'f' && c != 'n' {
				return []string{payload} // 不是 JSON 值序列，放弃
			}
			start = i
		}
		if inStr {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inStr = false
				if depth == 0 { // 裸字符串值，切在引号后
					if part, ok := trimJSONValue(trimmed[start : i+1]); ok {
						parts = append(parts, part)
						if len(parts) >= maxSplitParts {
							return []string{payload}
						}
						start = -1
					} else {
						return []string{payload}
					}
				}
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth < 0 {
				return []string{payload}
			}
			if depth == 0 {
				if part, ok := trimJSONValue(trimmed[start : i+1]); ok {
					parts = append(parts, part)
					if len(parts) >= maxSplitParts {
						return []string{payload}
					}
					start = -1
				} else {
					return []string{payload}
				}
			}
		}
	}
	if start >= 0 || inStr || depth != 0 {
		return []string{payload} // 末尾有半截值：整份不认，交给原样透传
	}
	if len(parts) == 0 {
		return []string{payload}
	}
	return parts
}

// trimJSONValue 校验候选片段是单个合法 JSON 值，返回去空白后的原文。
func trimJSONValue(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if v == "" || !json.Valid([]byte(v)) {
		return "", false
	}
	return v, true
}
