// empty_text_part.go 出站前修掉「空 text 段」（上游 400 / code 11151）。
//
// 上游对每个 text 段做长度校验：`text` 为空串或缺失，整条消息判 `a message has
// empty content`，本次请求 400。麻烦在于这条**不可自愈**：客户端每次重试都在
// 重放同一段历史，换账号、换模型之外没有出路，多号池只会把同一个坏请求在上游
// 撞满一轮。
//
// 触发点不止「客户端真的发了 parts 数组」这一种（2026-11-08 上游实测，kimi-k3.1-pro
// + cn 号池，逐形态对照）：
//
//	[{"type":"text","text":""}]       -> 400 / 11151
//	[{"type":"text","text":null}]      -> 400 / 11151
//	[{"type":"text"}]                  -> 400 / 11151
//	[{"type":"text","text":"   "}]     -> 200（只判长度是否为零，不 trim）
//	[{"type":"textarea","text":""}]    -> 200（未识别的 type 被忽略）
//	{"type":"text","text":""}（非数组）-> 200（不嗅探对象）
//
// 更需要兜住的是最后一行背后那条规律：**字符串 content 只要去空白后以 `[` 开头，
// 上游就会把它当 parts 数组解析**。工具结果里常见的表单/按钮抓取恰好是这个形状
// （`[{"type":"text","text":""},…]`），于是一段本该是「数据」的字符串被嗅探成一个
// 空文本段。这里按同一口径嗅探、补值、重新序列化。
//
// 修法是**只补值、不删段**：空 text 段填一个空格。删段会改段落数量与下标（客户端
// 侧的多模态顺序、工具结果的段序都可能被别处引用），而补一个空格对模型上下文没有
// 语义影响——它本来就看不到内容。注意不能用「加前缀空格」来规避嗅探：上游会先跳过
// 空白再解析，那种写法挡不住。
package upstream

import (
	"encoding/json"
	"strings"
)

// emptyTextPartFill 空 text 段的占位值。单个空格：上游只看长度是否为零（"   " 实测
// 放行），且该段本来就没有内容可给模型看。
const emptyTextPartFill = " "

// guardEmptyTextParts 把 messages 里所有空 text 段就地补位。
//
// 只在真有改动时才替换 content 字段：数组形态换成新 slice（只克隆要改的 text 段，
// 图片等其余段保持原引用），字符串形态重新序列化。无改动时整条消息一字不动——
// 这条是「普通请求零影响」的判据。改动都落在 map 上，调用方持有的 slice 无需回写。
func guardEmptyTextParts(messages []any) {
	if len(messages) == 0 {
		return
	}
	for _, rawMsg := range messages {
		msg, ok := rawMsg.(map[string]any)
		if !ok {
			continue
		}
		content, has := msg["content"]
		if !has {
			continue
		}
		switch v := content.(type) {
		case []any:
			parts := make([]any, len(v))
			copied := false
			for i, rawPart := range v {
				part, ok := rawPart.(map[string]any)
				if !ok || partType(part) != "text" {
					parts[i] = rawPart
					continue
				}
				if !isEmptyText(part) {
					parts[i] = rawPart
					continue
				}
				clone := make(map[string]any, len(part)+1)
				for k, val := range part {
					clone[k] = val
				}
				clone["text"] = emptyTextPartFill
				parts[i] = clone
				copied = true
			}
			if copied {
				msg["content"] = parts
			}
		case string:
			// 字符串信封被上游当 parts 嗅探：只有「解析出非空对象数组」才算，否则原样
			// 保留（普通文本、数字、以 [ 开头但解析失败的串都不动）。
			parts := sniffParts(v)
			if parts == nil {
				continue
			}
			if !fillEmptyTextParts(parts) {
				continue
			}
			if nb, err := json.Marshal(parts); err == nil {
				msg["content"] = string(nb)
			}
		}
	}
}

// sniffParts 复刻上游的嗅探口径：字符串去前导空白后以 `[` 开头、且能解析成
// 「非空的对象数组」时返回该数组，否则 nil（表示上游不会把它当 parts）。
func sniffParts(content string) []any {
	stripped := strings.TrimLeft(content, " \t\r\n")
	if !strings.HasPrefix(stripped, "[") {
		return nil
	}
	var parts []any
	if err := json.Unmarshal([]byte(stripped), &parts); err != nil {
		return nil
	}
	if len(parts) == 0 {
		return nil
	}
	for _, p := range parts {
		if _, ok := p.(map[string]any); !ok {
			return nil
		}
	}
	return parts
}

// fillEmptyTextParts 就地补位，返回是否有改动。
func fillEmptyTextParts(parts []any) bool {
	changed := false
	for _, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok || partType(part) != "text" {
			continue
		}
		if !isEmptyText(part) {
			continue
		}
		part["text"] = emptyTextPartFill
		changed = true
	}
	return changed
}

// partType 取段类型。上游只对精确的 "text" 做校验，"textarea" 这类未知 type 被忽略，
// 所以这里**不做**前缀/大小写归一——归一反而会把"不受校验"的段拉进校验。
func partType(part map[string]any) string {
	t, _ := part["type"].(string)
	return t
}

// isEmptyText 该 text 段是否会被上游判为空：text 缺失、null、或空串。
// 只判零长度、不 trim——"   " 实测放行，把它当空来补等于改动了上游本来接受的报文。
func isEmptyText(part map[string]any) bool {
	v, has := part["text"]
	if !has || v == nil {
		return true
	}
	s, ok := v.(string)
	if !ok {
		// 非字符串（数字/对象等）不在此列：上游这句话只对「空串/缺失」下结论，
		// 其余形态没有实测依据，改动它属于猜测。
		return false
	}
	return s == ""
}
