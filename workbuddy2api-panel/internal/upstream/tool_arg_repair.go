// tool_arg_repair.go 修复 DeferredToolCall 的残缺参数。
//
// 背景（2026-11-11/12 生产实测，cn:deepseek-v4.1-flash）：ZCode 的
// DeferredToolCall 是一个「把目标工具名 + 参数对象再包一层」的工具。模型在长
// 上下文里偶发两种格式错误：
//
//  1. input 被写成 JSON 字符串：{"name":"WebFetch","input":"{\"url\":...}"}
//     —— 外层 JSON 合法，但 input 不是对象，客户端 schema 校验直接拒绝；
//  2. 参数 JSON 被截断：完整 567 字符，失败那次只有 566，最后一个 } 没写出来。
//
// 两种错误都只在 DeferredToolCall 这一层出现（目标工具本身的参数是平的）。
// 本修复器只缓存 function.name == DeferredToolCall 的流式参数，在收尾帧之前
// 一次性重建：
//   - input 是字符串且能解析成对象 → 换成对象；
//   - JSON 截断（只在括号/引号未闭合时）→ 补最少闭合符并重新校验；
//   - 其它错误 → 原样透传，绝不猜测/吞字节。
//
// 为什么必须缓存：修复 input 字符串需要完整参数才能判断；流式分片已经发给
// 客户端后无法撤回。首帧仍立即透传（id/name/index + 空 arguments），客户端
// 先登记调用，完整参数在 finish_reason 之前补一帧发完。
package upstream

import (
	"encoding/json"
	"log"
	"strings"
	"unicode/utf8"
)

const deferredToolName = "DeferredToolCall"

// maxRepairArgs 单个调用参与修复的最大参数长度。超过则放弃修复、原样透传：
// 修复器只应缓存小对象，不能因为上游异常把整条流攒在内存里。
const maxRepairArgs = 1 << 20

// pendingToolArg 一个待收尾的 DeferredToolCall 参数。
type pendingToolArg struct {
	index        int
	args         strings.Builder
	firstEmitted bool
	passthrough  bool // 参数过大：不再缓存，按原样透传
}

// ToolArgRepair 是一路响应流一个实例的状态机（非并发安全）。
type ToolArgRepair struct {
	enabled  bool
	pending  map[int]*pendingToolArg
	order    []int
	template map[string]any // 最近一帧，用于补造帧的 created/model
}

// NewToolArgRepair enabled=false 时 Enabled() 恒 false，Process/Flush 零改写。
func NewToolArgRepair(enabled bool) *ToolArgRepair {
	return &ToolArgRepair{enabled: enabled, pending: map[int]*pendingToolArg{}}
}

// Enabled 报告修复器是否启用。
func (r *ToolArgRepair) Enabled() bool { return r != nil && r.enabled }

// Process 吸收/改写一帧，返回按顺序写出的帧；nil 表示该帧被完全吸收。
//
// 只在 DeferredToolCall 的首帧保留 id/type/name 并把 arguments 置空；后续
// arguments 分片全部缓存。遇到 finish_reason 时先把完整参数补成一帧，再写
// finish 帧——顺序反了客户端会在读到参数前结束回合。
func (r *ToolArgRepair) Process(obj map[string]any) []map[string]any {
	if !r.Enabled() {
		return []map[string]any{obj}
	}
	r.template = obj

	chs, ok := obj["choices"].([]any)
	if !ok {
		return []map[string]any{obj}
	}

	changed := false
	var prefixFrames []map[string]any // 参数超限时先补发已缓存的前缀
	for _, ci := range chs {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		d, _ := c["delta"].(map[string]any)
		if d == nil {
			continue
		}
		tcs, _ := d["tool_calls"].([]any)
		if len(tcs) == 0 {
			continue
		}

		kept := make([]any, 0, len(tcs))
		for _, tci := range tcs {
			tc, _ := tci.(map[string]any)
			if tc == nil {
				kept = append(kept, tci)
				continue
			}
			idx := -1
			if v, ok := tc["index"].(float64); ok {
				idx = int(v)
			}
			fn, _ := tc["function"].(map[string]any)
			name, _ := fn["name"].(string)
			p := r.pending[idx]
			if name != deferredToolName && p == nil {
				kept = append(kept, tc)
				continue
			}
			if idx < 0 {
				// 没有 index 的流无法安全按调用归并；保持原样。
				kept = append(kept, tc)
				continue
			}
			if p == nil {
				p = &pendingToolArg{index: idx}
				r.pending[idx] = p
				r.order = append(r.order, idx)
			}
			args, _ := fn["arguments"].(string)
			if !p.passthrough {
				if p.args.Len()+len(args) > maxRepairArgs {
					// 超过修复上限：把已缓存的前缀立刻补发，然后当前及后续分片原样
					// 透传。客户端按「首帧空参 + 前缀帧 + 后续原始分片」拼接，字节不丢。
					if prefix := p.args.String(); prefix != "" {
						prefixFrames = append(prefixFrames, r.toolCallFrame(idx, prefix))
					}
					p.passthrough = true
					p.args.Reset()
					log.Printf("WARN: [upstream] %s 参数超过 %d 字节，放弃修复、改为原样透传 (index=%d)",
						deferredToolName, maxRepairArgs, idx)
				} else {
					p.args.WriteString(args)
				}
			}
			changed = true
			if p.passthrough {
				// 未缓存过的首帧/后续分片都原样保留（已缓存的前缀已单独补发）。
				p.firstEmitted = true
				kept = append(kept, tc)
				continue
			}
			if !p.firstEmitted {
				// 首帧只登记调用身份，参数留空；完整参数在 finish 前补发。
				fn["arguments"] = ""
				p.firstEmitted = true
				kept = append(kept, tc)
			}
			// 后续分片已被吸收，不再透传。
		}
		if changed {
			if len(kept) > 0 {
				d["tool_calls"] = kept
			} else {
				delete(d, "tool_calls")
			}
		}
	}

	if len(r.pending) == 0 {
		// 本流没有待修复调用：零改写（含 finish 帧）。
		return []map[string]any{obj}
	}

	fr := takeFinishReason(obj)
	if fr == "" {
		if frameEmpty(obj) {
			return prefixFrames
		}
		return append(prefixFrames, obj)
	}
	// usage-only 帧只是旁路统计，不是终态。它可能位于参数分片之间，
	// 必须原样透传，不能因 takeFinishReason 的共享副作用提前结束调用。
	if choices, ok := obj["choices"].([]any); ok && len(choices) == 0 {
		return append(prefixFrames, obj)
	}

	out := make([]map[string]any, 0, len(prefixFrames)+len(r.pending)+2)
	out = append(out, prefixFrames...)
	if !frameEmpty(obj) {
		out = append(out, obj)
	}
	out = append(out, r.finalizeAll()...)
	out = append(out, finishFrameWithUsage(obj, fr))
	return out
}

// Flush 在流结束（[DONE] 或 EOF）时补发尚未收尾的调用。
// 返回 nil 表示没有待收尾调用。
func (r *ToolArgRepair) Flush() []map[string]any {
	if !r.Enabled() || len(r.pending) == 0 {
		return nil
	}
	return r.finalizeAll()
}

// finalizeAll 把每个待收尾调用补成一帧 delta.tool_calls（只带 index +
// function.arguments；id/name 已在首帧登记过），并清空状态。
func (r *ToolArgRepair) finalizeAll() []map[string]any {
	out := make([]map[string]any, 0, len(r.order))
	for _, idx := range r.order {
		p := r.pending[idx]
		if p == nil || p.passthrough {
			// 超限调用已切到原样透传：前缀和后续分片都已写出，收尾不再补。
			continue
		}
		raw := p.args.String()
		if raw == "" {
			// 空参数无法推断目标工具；首帧已把空调用发给客户端，保持原样。
			continue
		}
		repaired, kind := repairDeferredArgs(raw)
		if kind != "" {
			log.Printf("INFO: [upstream] %s 参数已修复 index=%d len=%d kind=%s",
				deferredToolName, idx, len(raw), kind)
		}
		out = append(out, r.toolCallFrame(idx, repaired))
	}
	r.pending = nil
	r.order = nil
	return out
}

// toolCallFrame 以最近一帧为模板补造一帧「只带参数」的 tool_call。
// 不带 id/name：客户端已从首帧登记，OpenAI 流式规范后续分片也只带 index。
func (r *ToolArgRepair) toolCallFrame(idx int, args string) map[string]any {
	frame := map[string]any{
		"object": "chat.completion.chunk",
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{
				"tool_calls": []any{map[string]any{
					"index": float64(idx),
					"function": map[string]any{
						"arguments": args,
					},
				}},
			},
		}},
	}
	if r.template != nil {
		for _, k := range []string{"created", "model"} {
			if v, ok := r.template[k]; ok && v != nil {
				frame[k] = v
			}
		}
	}
	return frame
}

// finishFrameWithUsage 在标准 finish 帧上保留原帧的 id/usage。
// 上游常把 usage 放在最后一个带 finish_reason 的帧里；拆帧时不能丢。
func finishFrameWithUsage(src map[string]any, finish string) map[string]any {
	frame := finishOnlyFrame(src, finish)
	for _, k := range []string{"id", "usage"} {
		if v, ok := src[k]; ok && v != nil {
			frame[k] = v
		}
	}
	return frame
}

// frameEmpty 判断一帧在摘掉 finish_reason / 被吸收的 tool_calls 后是否已无内容。
// choices 缺失（usage-only 帧）一律保留。
func frameEmpty(obj map[string]any) bool {
	chs, ok := obj["choices"].([]any)
	if !ok || len(chs) == 0 {
		return false
	}
	for _, ci := range chs {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		if fr, _ := c["finish_reason"].(string); fr != "" {
			return false
		}
		d, _ := c["delta"].(map[string]any)
		for _, v := range d {
			switch x := v.(type) {
			case nil:
			case string:
				if x != "" {
					return false
				}
			default:
				return false
			}
		}
	}
	return true
}

// repairDeferredArgs 修复一条 DeferredToolCall 参数；返回原文与修复类型
// （空串 = 未修改）。只在能重新解析成合法 JSON 时返回修复结果。
func repairDeferredArgs(raw string) (string, string) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return raw, ""
	}
	var v any
	kind := ""
	if err := json.Unmarshal([]byte(trimmed), &v); err != nil {
		fixed, ok := closeTruncatedJSON(trimmed)
		if !ok {
			return raw, ""
		}
		if err := json.Unmarshal([]byte(fixed), &v); err != nil {
			return raw, ""
		}
		trimmed = fixed
		kind = "truncation"
	}

	obj, ok := v.(map[string]any)
	if !ok {
		return raw, ""
	}
	// 这里拿到的是 DeferredToolCall 的参数，name 是「目标工具」（WebFetch/
	// TodoWrite/...），不会是 DeferredToolCall 本身。只要求它是一个非空的
	// 工具名，避免把恰好带 input 字符串的普通 JSON 也改掉。
	if name, _ := obj["name"].(string); name == "" {
		return raw, ""
	}
	if s, ok := obj["input"].(string); ok {
		var inner any
		if err := json.Unmarshal([]byte(s), &inner); err == nil {
			if _, ok := inner.(map[string]any); ok {
				obj["input"] = inner
				if kind == "" {
					kind = "string_input"
				} else {
					kind += "+string_input"
				}
			}
		}
	}
	if kind == "" {
		return raw, ""
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return raw, ""
	}
	var check any
	if json.Unmarshal(out, &check) != nil {
		return raw, ""
	}
	return string(out), kind
}

// closeTruncatedJSON 只修「结构没写完」：未闭合的 {}、[]、字符串。
// 不做通用 JSON 修复（缺逗号/缺值/乱序等一律放弃），修完由调用方重新解析校验。
func closeTruncatedJSON(raw string) (string, bool) {
	s := strings.TrimRight(raw, " \t\r\n")
	if s == "" || !utf8.ValidString(s) {
		return "", false
	}
	var stack []byte
	inStr, escaped := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			if escaped {
				escaped = false
				continue
			}
			switch c {
			case '\\':
				escaped = true
			case '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{', '[':
			stack = append(stack, c)
		case '}':
			if len(stack) == 0 || stack[len(stack)-1] != '{' {
				return "", false
			}
			stack = stack[:len(stack)-1]
		case ']':
			if len(stack) == 0 || stack[len(stack)-1] != '[' {
				return "", false
			}
			stack = stack[:len(stack)-1]
		}
	}
	if escaped {
		// 结尾是裸反斜杠：补引号会改变转义语义，放弃。
		return "", false
	}
	openedString := inStr
	if inStr {
		s += `"`
	}
	s = strings.TrimRight(s, " \t\r\n")
	if strings.HasSuffix(s, ":") {
		// 缺值（{"a":）无法安全猜测。
		return "", false
	}
	if strings.HasSuffix(s, ",") {
		// 截断在值后的逗号：去掉尾逗号再闭括号。
		s = strings.TrimRight(s[:len(s)-1], " \t\r\n")
	}
	if len(stack) == 0 && !openedString {
		return "", false
	}
	// 裸数字、true/false/null 截断后也能被闭括号“修好”，但补括号会猜出另一个值：
	// 例如 1 → 10、e → 1e10。这里只接受语法上已结束的对象、数组或字符串。
	last := s[len(s)-1]
	if !inStr && last != '}' && last != ']' && last != '"' {
		return "", false
	}
	for i := len(stack) - 1; i >= 0; i-- {
		if stack[i] == '{' {
			s += "}"
		} else {
			s += "]"
		}
	}
	return s, true
}
