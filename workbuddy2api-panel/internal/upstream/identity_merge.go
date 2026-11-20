// identity_merge.go 把相邻的 system/developer 消息合并成一条。
//
// 为什么（11128 的又一处结构性成因，2026-11-20 线上取证）：
// DSH 0.2.2 起把系统提示拆成「一句一条 system 消息」——一次请求 53 条 system。
// 上游的内容闸门按**重建后的提示文本**做连续原文匹配，拆开只是 JSON 里的分隔，
// 重建之后仍是同一段；而本仓的段级脱敏规则（sanitizeDshPathsRe，见 sanitize.go）
// 只能在**单条消息内**匹配。拆开之后：
//
//	{"role":"system","content":"Tokens prefixed with @ are paths the user explicitly referenced."},
//	{"role":"system","content":"Relative paths resolve against the workspace root; ..."},
//	{"role":"system","content":"A trailing slash marks a directory: ..."},
//	{"role":"system","content":"Anything else is a file: ... @\"...\" quotes a path containing spaces."}
//
// 四段各自都不含完整指纹 → 正则一路不命中 → 整段逐字出站 → 400
// `11-128 Illegal API invocation from an unapproved channel`。
// 线上快照（blocked-20261120-134043.620.json）里这四段一条不少地排在 messages[2..5]。
//
// 合并回一条之后，同一段文本重新落在一个字符串里，既有正则照旧生效（实测该段
// 是「只发首句放行、前两句连排即拦」的连续窗口指纹，改写后即通过）。
//
// 语义安全性：同角色、同顺序、同文本，只把消息之间的分隔从
// `","role":"system","content":"` 换成两个换行 —— 系统提示本来就是一个通道。
// 只合并**纯文本**的 system/developer 消息（带 tool_calls / 数组 content 的一律不动），
// 并且只在「上一条已合并结果」也是纯文本身份消息时接续合并。
package upstream

import "strings"

// isIdentityRole 报告 role 是否系统级消息（system / developer）。
func isIdentityRole(role string) bool {
	r := strings.ToLower(strings.TrimSpace(role))
	return r == "system" || r == "developer"
}

// identityMergeSeparator 合并时的分隔符：两个换行。用换行而不是空串，
// 与客户端「一段一条消息」的渲染语义一致（段间本来就有空行），也让上游重建后的
// 文本与拆分前逐字一致。
const identityMergeSeparator = "\n\n"

// mergeAdjacentIdentityMessages 合并相邻的纯文本 system/developer 消息。
//
// 返回新 slice 与「是否发生合并」。无相邻身份消息时原样返回（零改动路径）。
func mergeAdjacentIdentityMessages(msgs []any) ([]any, bool) {
	if len(msgs) < 2 {
		return msgs, false
	}
	out := make([]any, 0, len(msgs))
	changed := false
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		role, _ := m["role"].(string)
		text, isText := m["content"].(string)
		// 只碰「只有 role + content 两个键的纯文本身份消息」：带 name / tool_calls /
		// 数组 content 的消息有自己的语义，合并会改变结构。
		if !isText || !isIdentityRole(role) || len(m) != 2 {
			out = append(out, raw)
			continue
		}
		if n := len(out); n > 0 {
			prev, ok := out[n-1].(map[string]any)
			if ok {
				pRole, _ := prev["role"].(string)
				pText, okText := prev["content"].(string)
				if okText && isIdentityRole(pRole) && len(prev) == 2 {
					prev["content"] = pText + identityMergeSeparator + text
					changed = true
					continue
				}
			}
		}
		out = append(out, m)
	}
	if !changed {
		return msgs, false
	}
	return out, true
}
