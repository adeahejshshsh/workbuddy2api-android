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
// 空文本段。这里按同一口径嗅探，只在字节层面补值，其余原文不动。
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
			if s, ok := fillSniffedEmptyText(v); ok {
				msg["content"] = s
			}
		}
	}
}

// textSpan 待替换的字节区间 [start,end) 与替换内容；start==end 表示插入。
type textSpan struct {
	start, end int
	repl       string
}

// fillSniffedEmptyText 对「会被上游当 parts 嗅探」的字符串补位，返回新串与是否改动。
//
// 只在字节层面改空段的 text 值（缺失时插入 text 键），其余字节一律不动。这串多半是
// 工具输出：整体 Unmarshal+Marshal 会把大整数转成 float64 丢精度、把 < > & 转义成
// <、按字母序重排对象键，模型看到的工具结果就不再是客户端发来的那份。
func fillSniffedEmptyText(content string) (string, bool) {
	stripped := strings.TrimLeft(content, " \t\r\n")
	lead := len(content) - len(stripped)
	if !strings.HasPrefix(stripped, "[") || !json.Valid([]byte(stripped)) {
		return content, false
	}
	elems, ok := rawElements(stripped, '[')
	// 空数组或含非对象元素：上游不把它当 parts。
	if !ok || len(elems) == 0 {
		return content, false
	}
	var spans []textSpan
	for _, e := range elems {
		raw := stripped[e.start:e.end]
		if raw[0] != '{' {
			return content, false
		}
		if sp, ok := emptyTextSpan(raw); ok {
			sp.start += e.start
			sp.end += e.start
			spans = append(spans, sp)
		}
	}
	if len(spans) == 0 {
		return content, false
	}
	var b strings.Builder
	b.Grow(len(content) + 16*len(spans))
	b.WriteString(content[:lead])
	prev := 0
	for _, sp := range spans {
		b.WriteString(stripped[prev:sp.start])
		b.WriteString(sp.repl)
		prev = sp.end
	}
	b.WriteString(stripped[prev:])
	return b.String(), true
}

// rawElements 列出一个 JSON 数组（open='['）的元素或对象（open='{'）的值在 src 中的
// 字节区间（src 必须已是合法 JSON）。对象时同时给出每个值的键名。
//
// 区间由 Decoder.InputOffset 减去原始值长度推出；再逐一核对区间内容与解码出的原文
// 一致，任何不一致都按「无法定位」放弃——宁可不补，也不写坏客户端的数据。
func rawElements(src string, open byte) ([]rawElem, bool) {
	dec := json.NewDecoder(strings.NewReader(src))
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil || tok != json.Delim(open) {
		return nil, false
	}
	var out []rawElem
	for dec.More() {
		var key string
		if open == '{' {
			tok, err := dec.Token()
			if err != nil {
				return nil, false
			}
			key, _ = tok.(string)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, false
		}
		end := int(dec.InputOffset())
		start := end - len(raw)
		if start < 0 || src[start:end] != string(raw) {
			return nil, false
		}
		out = append(out, rawElem{key: key, start: start, end: end})
	}
	return out, true
}

type rawElem struct {
	key        string
	start, end int
}

// emptyTextSpan 判断一个对象原文是否为「上游会判空的 text 段」，是则给出补位编辑。
// 重复键按最后一次出现为准（与解析成 map 的口径一致）。
func emptyTextSpan(obj string) (textSpan, bool) {
	fields, ok := rawElements(obj, '{')
	if !ok {
		return textSpan{}, false
	}
	typ, text := -1, -1
	for i, f := range fields {
		switch f.key {
		case "type":
			typ = i
		case "text":
			text = i
		}
	}
	if typ < 0 {
		return textSpan{}, false
	}
	var t string
	if json.Unmarshal([]byte(obj[fields[typ].start:fields[typ].end]), &t) != nil || t != "text" {
		return textSpan{}, false
	}
	fill, _ := json.Marshal(emptyTextPartFill)
	if text < 0 {
		// 缺失：紧跟 `{` 插入。对象至少有 type 键，后面补逗号合法。
		return textSpan{start: 1, end: 1, repl: `"text":` + string(fill) + `,`}, true
	}
	v := obj[fields[text].start:fields[text].end]
	if v != "null" && v != `""` {
		return textSpan{}, false
	}
	return textSpan{start: fields[text].start, end: fields[text].end, repl: string(fill)}, true
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
