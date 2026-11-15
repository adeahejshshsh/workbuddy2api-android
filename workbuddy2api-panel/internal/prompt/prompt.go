// Package prompt 提供网关自有系统提示词：内置默认 + 文件覆盖 + 降级中性提示词。
//
// 背景：客户端（Claude Code/Codex 等 CLI）在 system prompt 注入固定模板句，
// 上游内容审核按逐字精确匹配误杀合法流量（issue #36/PR39 的 11128）。
// 方案：网关在出站前用自有系统提示词替换客户端 system/developer 消息，
// 从源头消灭 system 来源的指纹误报（用户/assistant 消息里的指纹串仍由
// internal/upstream/sanitize.go 清洗，两层叠加、互不替代）。
//
// 改写口径（审查 W2-C6-01 统一）：本包所有改写都走「顶层对象保序 + 未触碰字段
// 原文保留」的增量改写（rewriteTopLevelMessages），不再整树 json.Marshal 重编。
// 原因：透传型网关对上游是字节级改写，字段顺序与 <>& 的转义形态一变，上游若做
// 原文/前缀匹配（prompt cache、指纹判定）行为就与客户端实际请求不一致，排障不可复现。
package prompt

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

//go:embed defaultprompt.md
var defaultPrompt string

// 出站改写链路的资源上限（审查 W2-C6-03/05/06）：这些数字都是「宁可放弃抢救，
// 也不要把请求改坏」的闸门 —— 触发即原样返回，由调用方走后面的补救层。
const (
	// maxPromptFileBytes Load 允许读取的提示词文件上限（1 MiB）。
	maxPromptFileBytes = 1 << 20

	// maxMoveSystemBytes MoveSystemToUser 允许搬运的 system 原文字节上限（64 KiB）。
	// 超过即放弃搬运：截断会把客户端人设/项目约定切掉一半（比不搬更坏）。
	maxMoveSystemBytes = 64 << 10

	// maxSplitParts SplitSystemMessages 单条 system 最多拆成多少段。
	// 无上限时一条千句 system 会把 messages 膨胀上千条 → 上游 400/413，
	// 计费与截断策略也会跟着异常。
	maxSplitParts = 64
)

// errPromptTooLarge 提示词文件超限（可 errors.Is 判定）。
var errPromptTooLarge = errors.New("prompt file too large (limit 1 MiB)")

// errPromptNotRegular 提示词路径不是普通文件（目录/设备/FIFO）。
var errPromptNotRegular = errors.New("prompt file is not a regular file")

// Degraded 降级提示词：误报处理用，刻意极简中性。
//
// 触发场景：passthrough 模式下请求被上游内容策略拦截（HTTP 400 + 审核文案），
// 判定为指纹误报后换最小中性提示词重试一次。非对抗框架——只用于绕开
// system 来源的误报，不改变用户指令的合法性语义。
const Degraded = "You are a helpful assistant. Respond in the user's language, follow the user's instructions, and be direct and concise."

// Load 按 mode 与 file 加载系统提示词文本。
//   - file 非空 → 读文件（不存在/读失败返回 error，调用方 fail fast）；
//   - file 空 → 返回内置 defaultPrompt。
//
// mode 在此仅做透传记录（实际 custom/passthrough 路由由调用方决定），
// Load 只负责"拿到一段提示词文本"，不关心路由语义。
//
// 审查 W2-C6-06：路径来自配置，此前直接 os.ReadFile —— 无大小限制（配置被改成
// /dev/zero 或几百 MB 的文件即把整个进程的内存打满），且错误串回带完整绝对路径
// （进日志/前端就泄露部署目录）。现在：只接受普通文件、上限 1 MiB、错误只回 basename。
// 不做「基目录白名单」是刻意的：配置里允许绝对路径（运维把人格文件放在 /etc 或
// 容器 secret 挂载点是常见做法），真正要防的是无界读取与路径泄露，不是路径本身。
func Load(mode, file string) (string, error) {
	if file == "" {
		return defaultPrompt, nil
	}
	base := filepath.Base(file)
	st, err := os.Stat(file)
	if err != nil {
		return "", fmt.Errorf("prompt file %s: %w", base, err)
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("prompt file %s: %w", base, errPromptNotRegular)
	}
	if st.Size() > maxPromptFileBytes {
		return "", fmt.Errorf("prompt file %s: %w (size=%d)", base, errPromptTooLarge, st.Size())
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("prompt file %s: %w", base, err)
	}
	// Stat 与 ReadFile 之间文件可能被追加（TOCTOU）：读后再按真实长度判一次。
	if len(raw) > maxPromptFileBytes {
		return "", fmt.Errorf("prompt file %s: %w (size=%d)", base, errPromptTooLarge, len(raw))
	}
	return string(raw), nil
}

// Rewrite 解析 OpenAI 请求体并替换系统提示词：
//   - 删除**开头连续**的 system/developer 块（与 Append 同口径，审查 W2-C6-07：
//     此前连中途出现的 system 一并删除，客户端项目规范/工具约定/多段约束全丢，
//     部分上游对「tool 消息前必须有 system」敏感 → 400/工具调用异常）；
//   - 在 messages 头部插入一条 {"role":"system","content":systemPrompt}；
//   - 其余字段与 user/assistant/tool 消息逐字不动。
//
// 解析失败 → 原样返回（绝不失败）：Rewrite 是出站改写的关键路径，
// 任何解析错误都不应阻塞请求转发，让上游按其原始语义处理。
func Rewrite(body []byte, systemPrompt string) []byte {
	if len(body) == 0 || systemPrompt == "" {
		return body
	}
	out, ok := rewriteTopLevelMessages(body, func(msgs []json.RawMessage, present bool) ([]json.RawMessage, bool) {
		gw := systemMessageRaw("system", systemPrompt)
		if !present || len(msgs) == 0 {
			// 无 messages 字段（或类型不符）→ 只补一条 system，其余字段原文保留。
			return []json.RawMessage{gw}, true
		}
		// 只替换开头连续块：遇第一条非 system/developer（含非对象、无 role）即停。
		lead := 0
		for lead < len(msgs) {
			role, ok := messageRole(msgs[lead])
			if !ok || (role != "system" && role != "developer") {
				break
			}
			lead++
		}
		rebuilt := make([]json.RawMessage, 0, len(msgs)-lead+1)
		rebuilt = append(rebuilt, gw)
		rebuilt = append(rebuilt, msgs[lead:]...)
		return rebuilt, true
	})
	if !ok {
		return body
	}
	return out
}

// Append 解析 OpenAI 请求体并追加网关自有 system 提示词（issue #129 append 模式）：
//   - 开头连续块 = 从 messages[0] 起向后 role 为 system/developer（精确字符串
//     匹配，与 Rewrite 删除口径一致）的消息；遇第一条非 system/developer
//     消息（含非 map 消息、无 role 消息）即停；
//   - 块长 0（没有任何 system）→ 在最前插入一条网关 system；
//   - 块长 ≥1（客户端已带 system）→ **合并进首条**（审查 W2-C6-08）：此前无论
//     有没有既有 system 都再插一条网关 system，请求因此携带两条 system，仅接受
//     单条 system 的上游（部分 Anthropic 兼容层）直接 400。合并方向按函数语义
//     「追加」：既有文本在前、网关提示词在后，一个字符都不丢；
//   - 其余消息（中途 system、user/assistant/tool）逐字不动 —— 客户端项目规范/
//     工具约定与网关提示词并用。
//
// 守卫与 Rewrite 逐条一致：空 body / 空 systemPrompt / 坏 JSON → 原样返回
// （绝不失败）；无 messages 字段 → messages=[网关 system]，其余字段原样。
//
// 边界判定须显式同时匹配 system 与 developer：归一（developer→system）在
// 下游 prepareBody 的 normalizeRoles，Append 执行时开头块里的 developer
// 还是 developer。网关消息角色用 system 而非 developer——上游 role 白名单
// 不含 developer，插 developer 等于制造一次必然归一与多余的 11128 风险窗口。
//
// 未合并的两种情况（保持「宁可不加，也不改坏」）：首条 system 的 content 既不是
// 字符串也不是 parts 数组（例如被写成数字/对象），或合并结果无法序列化。
func Append(body []byte, systemPrompt string) []byte {
	if len(body) == 0 || systemPrompt == "" {
		return body
	}
	out, ok := rewriteTopLevelMessages(body, func(msgs []json.RawMessage, present bool) ([]json.RawMessage, bool) {
		gw := systemMessageRaw("system", systemPrompt)
		if !present || len(msgs) == 0 {
			return []json.RawMessage{gw}, true
		}
		lead := 0
		for lead < len(msgs) {
			role, ok := messageRole(msgs[lead])
			if !ok || (role != "system" && role != "developer") {
				break
			}
			lead++
		}
		if lead == 0 {
			// 没有既有 system：插入是最省事且语义正确的做法（仍只有一条 system）。
			rebuilt := make([]json.RawMessage, 0, len(msgs)+1)
			rebuilt = append(rebuilt, gw)
			rebuilt = append(rebuilt, msgs...)
			return rebuilt, true
		}
		merged, ok := mergeSystemContent(msgs[0], systemPrompt)
		if !ok {
			return nil, false
		}
		rebuilt := append([]json.RawMessage(nil), msgs...)
		rebuilt[0] = merged
		return rebuilt, true
	})
	if !ok {
		return body
	}
	return out
}

// MoveSystemToUser 把 system/developer 的**原文**搬进首条 user 消息前部，system 换成
// 中性提示词，用于 11‑-128 渠道校验的请求级抢救（返回 false = 这条请求结构不安全，别动）。
//
// 与 Rewrite 的区别是**丢不丢东西**：Rewrite 用中性提示词替换 system，客户端辛苦写的
// 人设/项目约定就此消失；这里只换承载通道（system → user），一个字符都不丢。上游判据是
// 「system 消息内容命中特征」，同一段文本放进 user 就能过（同一 body 换号结果一致），
// 所以这不是绕过审核，是换通道。
//
// 保守放弃五种情况：没有 system/developer 文本、system 内容不是纯文本、找不到 user
// 消息、首条 user 不是纯文本（多模态 parts 里插文本会打乱顺序）、system 原文超过
// maxMoveSystemBytes（审查 W2-C6-05：无长度上限时，把超长 system 整段塞进 user 会让
// 首条 user 膨胀到 MB 级 —— 上游按 user 计费/截断，且该文本从此失去 system 的指令优先级）。
func MoveSystemToUser(body []byte, neutral string) ([]byte, bool) {
	if len(body) == 0 || neutral == "" {
		return body, false
	}
	out, ok := rewriteTopLevelMessages(body, func(msgs []json.RawMessage, present bool) ([]json.RawMessage, bool) {
		if !present || len(msgs) == 0 {
			return nil, false
		}
		first, ok := decodeMessageObject(msgs[0])
		if !ok {
			return nil, false
		}
		switch role, _ := first["role"].(string); role {
		case "system", "developer":
		default:
			return nil, false
		}
		sysText, ok := first["content"].(string)
		if !ok || strings.TrimSpace(sysText) == "" {
			return nil, false
		}
		if len(sysText) > maxMoveSystemBytes {
			return nil, false
		}
		for i := 1; i < len(msgs); i++ {
			mm, ok := decodeMessageObject(msgs[i])
			if !ok {
				continue
			}
			if role, _ := mm["role"].(string); role != "user" {
				continue
			}
			txt, ok := mm["content"].(string)
			if !ok {
				return nil, false // 多模态 parts：插文本会打乱 part 顺序
			}
			mm["content"] = sysText + "\n\n" + txt
			first["role"] = "system"
			first["content"] = neutral
			movedRaw, ok1 := encodeValue(mm)
			firstRaw, ok2 := encodeValue(first)
			if !ok1 || !ok2 {
				return nil, false
			}
			rebuilt := append([]json.RawMessage(nil), msgs...)
			rebuilt[0] = firstRaw
			rebuilt[i] = movedRaw
			return rebuilt, true
		}
		return nil, false
	})
	if !ok {
		return body, false
	}
	return out, true
}

// DefaultHarnessIdentity 身份首句替换的默认值（吸收 qcgzxw `a7f10ad1` / 上游 PR #2）。
// 开关缺省关闭，不改变既有行为。
const DefaultHarnessIdentity = "You are CodeBuddy Code."

// OverrideHarnessIdentity 仅在开头**连续**的 system/developer 消息中，寻找以
// "You are " 开头的第一句身份声明并替换；其余提示词内容、消息顺序与其他字段保留。
//
// 用途：passthrough/append 模式下客户端会带上自己的 harness 身份句（例如
// "You are DeepSeekRuntime, ..."），上游据此识别「非官方客户端」并拒绝通道。改成
// 官方文案后语义不变、识别消失。无命中时原样返回（**不重新序列化**，避免把整段
// body 重排、丢大整数精度）。
func OverrideHarnessIdentity(body []byte, replacement string) []byte {
	if len(body) == 0 {
		return body
	}
	if strings.TrimSpace(replacement) == "" {
		replacement = DefaultHarnessIdentity
	}

	// UseNumber 保住数字原文（报告 #2）：默认解码把整数落成 float64，超过 2^53
	// 的 seed/微秒时间戳会被静默改写成近似值 —— 本函数在 passthrough/append +
	// 开关打开时**每个请求**都跑，属高频静默改写。与 Rewrite/Append 同口径。
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return body
	}
	for _, raw := range msgs {
		msg, ok := raw.(map[string]any)
		if !ok {
			break
		}
		role, _ := msg["role"].(string)
		if role != "system" && role != "developer" {
			break // 不触碰对话中段及 user/assistant/tool 的内容
		}

		switch content := msg["content"].(type) {
		case string:
			updated, changed := replaceHarnessIdentitySentence(content, replacement)
			if !changed {
				continue
			}
			msg["content"] = updated
		case []any:
			// OpenAI 多模态形式（审查 W2-C6-10）：逐个 text 块尝试，命中即停。
			// 此前是「只看首个 part 且非 text 就 break」——首块是 image 时，后面
			// 的文本块里的身份句永远改不到（漏改，不是安全问题但会让开关形同虚设）。
			changed := false
			for _, rawPart := range content {
				part, ok := rawPart.(map[string]any)
				if !ok {
					continue
				}
				if kind, ok := part["type"].(string); !ok || kind != "text" {
					continue
				}
				text, ok := part["text"].(string)
				if !ok {
					continue
				}
				updated, didChange := replaceHarnessIdentitySentence(text, replacement)
				if !didChange {
					continue
				}
				part["text"] = updated
				changed = true
				break // 只替换找到的第一处声明
			}
			if !changed {
				continue
			}
		default:
			continue
		}

		out, err := jsonMarshalNoEscape(obj)
		if err != nil {
			return body
		}
		return out // 只替换找到的第一处声明
	}
	return body
}

// replaceHarnessIdentitySentence 识别英文 harness 常见的 "You are ..." 首句。
// 以句末标点（后接空白/结尾）或换行作为边界；**不把 v4.1 这类词内句点当句尾**
// （否则 "You are DeepSeek v4.1 ..." 会被切成 "You are DeepSeek v4."）。
// 保留前导空白与句尾之后的全部文本及原有换行。非身份首句不动。
func replaceHarnessIdentitySentence(text, replacement string) (string, bool) {
	start := len(text) - len(strings.TrimLeft(text, " \t\r\n\ufeff"))
	const prefix = "You are "
	if len(text)-start < len(prefix) || !strings.EqualFold(text[start:start+len(prefix)], prefix) {
		return text, false
	}
	end := len(text)
scan:
	for i := start; i < len(text); i++ {
		switch text[i] {
		case '\r', '\n':
			end = i
			break scan
		case '.', '!', '?':
			if i+1 == len(text) || strings.ContainsRune(" \t\r\n", rune(text[i+1])) {
				end = i + 1
				break scan
			}
		}
	}
	if text[start:end] == replacement {
		return text, false
	}
	return text[:start] + replacement + text[end:], true
}

// jsonMarshalNoEscape 序列化但不做 HTML 转义。
//
// 默认 Marshal 会把 < > & 转成 \u003c 等：对「透传型」网关来说那是改写了客户端
// 原文（与 UseNumber 同一个理由）。审查 W2-C6-01：本包所有序列化都必须走这里。
func jsonMarshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// ---------------------------------------------------------------- 保序改写 ----
//
// 下面这组 helper 是审查 W2-C6-01/02 的落地点：不再「整棵树 decode 成 map 再
// encode 回去」（那会重排 map 键、把 <>& 转义、把 >2^53 的整数落成 float64），
// 而是只把**确实要改的**那一两条消息解出来重编，其余字段与消息一律保留原始字节。

// topLevelField 顶层对象的一个字段（保序 + 原文保留）。
type topLevelField struct {
	key string
	raw json.RawMessage
}

// parseTopLevel 把顶层 JSON 对象拆成保序字段表。
// 只接受「单个对象」：数组/标量/带尾随垃圾的 body 一律判失败（原样返回给调用方）。
func parseTopLevel(body []byte) ([]topLevelField, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		return nil, false
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, false
	}
	var fields []topLevelField
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, ok := kt.(string)
		if !ok {
			return nil, false
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, false
		}
		fields = append(fields, topLevelField{key: key, raw: raw})
	}
	if _, err := dec.Token(); err != nil { // 收尾的 '}'
		return nil, false
	}
	// 尾随只允许空白（"{} {}" 这种 body 交给上游原样处理，本包不动）。
	if rest := bytes.TrimSpace(body[dec.InputOffset():]); len(rest) != 0 {
		return nil, false
	}
	return fields, true
}

// encodeTopLevel 把保序字段表写回 JSON 对象（字段顺序与原文一致）。
func encodeTopLevel(fields []topLevelField) ([]byte, bool) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, f := range fields {
		if i > 0 {
			buf.WriteByte(',')
		}
		k, err := jsonMarshalNoEscape(f.key)
		if err != nil {
			return nil, false
		}
		buf.Write(k)
		buf.WriteByte(':')
		buf.Write(f.raw)
	}
	buf.WriteByte('}')
	return buf.Bytes(), true
}

// encodeMessageList 把消息列表写回 JSON 数组（每个元素都是原文/新编的合法 JSON）。
func encodeMessageList(msgs []json.RawMessage) []byte {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, m := range msgs {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(m)
	}
	buf.WriteByte(']')
	return buf.Bytes()
}

// rewriteTopLevelMessages 以「保序 + 未触碰字段原文保留」的方式改写 messages。
//
// fn 收到 messages 的原始字节列表；present=false 表示请求里没有可用的 messages
// 数组（缺字段 / null / 类型不符），此时 fn 仍可返回新列表（用于「补一条 system」）。
// 返回 (新列表, true) = 已改写；(nil, false) = 放弃改写，调用方原样返回 body。
func rewriteTopLevelMessages(body []byte, fn func(msgs []json.RawMessage, present bool) ([]json.RawMessage, bool)) ([]byte, bool) {
	fields, ok := parseTopLevel(body)
	if !ok {
		return body, false
	}
	idx := -1
	for i := range fields {
		if fields[i].key == "messages" {
			idx = i
			break
		}
	}
	var msgs []json.RawMessage
	present := false
	if idx >= 0 {
		// 只接受数组；null 解出来是 nil 切片（present 仍为 true，长度 0）。
		if err := json.Unmarshal(fields[idx].raw, &msgs); err == nil {
			present = true
		}
	}
	out, changed := fn(msgs, present)
	if !changed {
		return body, false
	}
	raw := encodeMessageList(out)
	if idx >= 0 {
		fields[idx].raw = raw
	} else {
		fields = append(fields, topLevelField{key: "messages", raw: raw})
	}
	encoded, ok := encodeTopLevel(fields)
	if !ok {
		return body, false
	}
	return encoded, true
}

// messageRole 只解 role 字段（不碰 content，避免为大 messages 付解码成本）。
func messageRole(raw json.RawMessage) (string, bool) {
	var head struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return "", false
	}
	return head.Role, true
}

// decodeMessageObject 把单条消息解成 map（UseNumber：审查 W2-C6-02，>2^53 的
// seed/微秒时间戳按字面量保留，不回写成近似值或科学计数法）。
func decodeMessageObject(raw json.RawMessage) (map[string]any, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, false
	}
	return m, true
}

// encodeValue 序列化单个值（不转义 HTML）。
func encodeValue(v any) (json.RawMessage, bool) {
	b, err := jsonMarshalNoEscape(v)
	if err != nil {
		return nil, false
	}
	return json.RawMessage(b), true
}

// systemMessageRaw 构造一条 {role, content} 消息（字段顺序固定为 role→content，
// 与客户端/官方 SDK 的写法一致，不经过 map 的字典序重排）。
func systemMessageRaw(role, content string) json.RawMessage {
	r, err := jsonMarshalNoEscape(role)
	if err != nil {
		r = []byte(`"system"`)
	}
	c, err := jsonMarshalNoEscape(content)
	if err != nil {
		c = []byte(`""`)
	}
	return json.RawMessage(`{"role":` + string(r) + `,"content":` + string(c) + `}`)
}

// mergeSystemContent 把网关提示词追加进既有 system/developer 消息（审查 W2-C6-08）。
//
//   - 字符串 content：`原文 + "\n\n" + 网关提示词`（原文一个字符都不丢）；
//   - parts 数组：末尾追加一个 {"type":"text","text":网关提示词} 的文本块
//     （保持「追加」语义，也不打乱客户端已有的块顺序）；
//   - 其余形态（数字/对象/null 等）：放弃合并，由调用方原样返回 body。
func mergeSystemContent(raw json.RawMessage, systemPrompt string) (json.RawMessage, bool) {
	msg, ok := decodeMessageObject(raw)
	if !ok {
		return nil, false
	}
	switch content := msg["content"].(type) {
	case string:
		if strings.TrimSpace(content) == "" {
			msg["content"] = systemPrompt
		} else {
			msg["content"] = content + "\n\n" + systemPrompt
		}
	case []any:
		part := map[string]any{"type": "text", "text": systemPrompt}
		msg["content"] = append(content, part)
	case nil:
		// 没有 content 字段：直接补上网关提示词（不存在丢东西的问题）。
		msg["content"] = systemPrompt
	default:
		return nil, false
	}
	return encodeValue(msg)
}

// SplitSystemMessages 把最长的那条 system/developer 消息按句子边界拆成多条消息，
// 用于 11-128 的**首选**抢救：上游按「连续原文」匹配指纹，句界一断就失配，而文本一个字都
// 不改、承载通道仍是 system（模型的指令优先级不变）。
//
// 实测（2026-10-28，CN 账号 deepseek-v4.1-flash，internal/probe/probe_channel.go）：
//   - 原始 DSH @ 路径段整段在 system        → 400 11-128
//   - 同一段同义改写后仍在 system           → 200（sanitize 层已覆盖这种情况）
//   - 同一段按句界拆成两条 system           → 200
//   - 每句一条 system                       → 200
//
// 返回 false 的三种情况：没有任何一条 system 能切出 ≥2 段（单句 system 没法拆，交给
// 调用方走后面的补救层）；切出的段数超过 maxSplitParts（审查 W2-C6-03：此前无上限，
// 千句 system 会把 messages 膨胀上千条 → 上游 400/413）；序列化失败。
//
// 角色保真（审查 W2-C6-03）：拆出来的每条都沿用**原消息的 role**。此前一律重建成
// system，把 developer 静默改写成 system —— 属未声明的语义改写，调用方无法察觉。
func SplitSystemMessages(body []byte) ([]byte, bool) {
	if len(body) == 0 {
		return body, false
	}
	out, ok := rewriteTopLevelMessages(body, func(msgs []json.RawMessage, present bool) ([]json.RawMessage, bool) {
		if !present || len(msgs) == 0 {
			return nil, false
		}
		// 只拆最长的一条：短 system 拆开反而是把一句指令切成两半（实测人格句拆两条就失效）。
		best, bestLen, bestText, bestRole := -1, 0, "", ""
		for i, raw := range msgs {
			role, ok := messageRole(raw)
			if !ok || (role != "system" && role != "developer") {
				continue
			}
			var head struct {
				Content string `json:"content"`
			}
			if err := json.Unmarshal(raw, &head); err != nil {
				continue // content 不是纯文本（多模态 parts）→ 不参与拆分
			}
			if len(head.Content) <= bestLen {
				continue
			}
			best, bestLen, bestText, bestRole = i, len(head.Content), head.Content, role
		}
		if best < 0 {
			return nil, false
		}
		parts := splitSentences(bestText)
		if len(parts) < 2 || len(parts) > maxSplitParts {
			return nil, false
		}
		rebuilt := make([]json.RawMessage, 0, len(msgs)+len(parts)-1)
		rebuilt = append(rebuilt, msgs[:best]...)
		for _, p := range parts {
			rebuilt = append(rebuilt, systemMessageRaw(bestRole, p))
		}
		rebuilt = append(rebuilt, msgs[best+1:]...)
		return rebuilt, true
	})
	if !ok {
		return body, false
	}
	return out, true
}

// splitSentences 按句界切分 system 文本（审查 W2-C6-04）。
//
// 此前只认「标点 + 空白」，于是三类形态被从中间切断：
//   - ``` 围栏代码块（"fmt.Println(\"a. b\")" 里的点）；
//   - 编号列表（"1. foo" 的 "1."）；
//   - URL（"https://host/a.b c" 里 path 内的点）。
//
// 现在：围栏整行不参与切分（进入/退出各一次）；编号列表项标记（段内只有
// 数字+点）不算句界；句界后的首个非空白字符必须是「句首形态」（大写字母、
// 引号/括号/反引号/列表符、非 ASCII 文字），小写字母一律不切 —— 这一条同时
// 兜住 "e.g. foo" / "1. foo" / URL 尾部这类小写续写。
//
// 注意：CJK 句号（。）不在切分集合内（与既有口径一致，改动会改变已实测有效的
// 拆分形态）。切分只丢「句界处的一个空白」，其余字符一个不动（既有用例钉住了
// 「拼回去等于原文」这条不变量）。
func splitSentences(text string) []string {
	var out []string
	start := 0
	inFence := false
	for i := 0; i < len(text); i++ {
		if i == 0 || text[i-1] == '\n' {
			// 行首：``` / ~~~ 围栏行（含 ```go 这类带语言标记的）整行跳过。
			if isFenceLine(text[i:]) {
				inFence = !inFence
				if j := strings.IndexByte(text[i:], '\n'); j >= 0 {
					i += j // 循环 i++ 后正好落在下一行行首
				} else {
					i = len(text)
				}
				continue
			}
		}
		if inFence {
			continue
		}
		switch text[i] {
		case '.', '!', '?':
			// 后面紧跟空白才算句界（"3.5" / "v1.2.3" / "example.com" 不切）。
			if i+1 >= len(text) {
				continue
			}
			if next := text[i+1]; next != ' ' && next != '\n' && next != '\t' {
				continue
			}
			if isListMarkerAt(text, i) {
				continue // "1." / "12." 这类行首列表项标记
			}
			if !startsSentence(text[i+1:]) {
				continue
			}
			if seg := strings.TrimSpace(text[start : i+1]); seg != "" {
				out = append(out, seg)
			}
			start = i + 1
		}
	}
	if seg := strings.TrimSpace(text[start:]); seg != "" {
		out = append(out, seg)
	}
	return out
}

// isFenceLine 报告 s（从行首起）是否为 Markdown 代码围栏行。
func isFenceLine(s string) bool {
	t := strings.TrimLeft(s, " \t")
	return strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~")
}

// isListMarkerAt 报告 text[i]（一个 '.'）是否属于**行首**编号列表标记（"1." / "23."）。
//
// 只看当前行行首到该点为止的内容：列表项标记的判据是「行首只有数字+点」，
// 与段起点无关（段起点可能落在这行的中间）。
func isListMarkerAt(text string, i int) bool {
	ls := strings.LastIndexByte(text[:i+1], '\n') + 1
	return isListMarker(text[ls : i+1])
}

// isListMarker 报告 seg（句界前的当前段）是否只是一个编号列表标记（"1." / "23."）。
func isListMarker(seg string) bool {
	t := strings.TrimSpace(seg)
	if len(t) < 2 || t[len(t)-1] != '.' {
		return false
	}
	for i := 0; i < len(t)-1; i++ {
		if t[i] < '0' || t[i] > '9' {
			return false
		}
	}
	return true
}

// startsSentence 报告句界之后的剩余文本是否以「句首形态」开始。
// 只用于抑制误切：判不准时返回 false（少切一刀只是少拆一条，不会改坏文本）。
func startsSentence(rest string) bool {
	t := strings.TrimLeft(rest, " \t\r\n")
	if t == "" {
		return false
	}
	c := t[0]
	switch {
	case c >= 'A' && c <= 'Z':
		return true
	case c >= 'a' && c <= 'z':
		return false
	case c >= '0' && c <= '9':
		// 纯数字开头：只有「下一条列表项」（"2. xxx"）才算新的一句 —— 句界正好
		// 落在列表项之间时该切；"42 items remain" 这种量词开头则不切。
		i := 0
		for i < len(t) && t[i] >= '0' && t[i] <= '9' {
			i++
		}
		return i < len(t) && t[i] == '.'
	}
	switch c {
	case '"', '\'', '(', '[', '`', '*', '_', '-', '#', '>', '{':
		return true
	}
	// 非 ASCII：CJK/日文/俄文等按「句首」处理（UTF-8 首字节 >= 0x80）。
	return c >= 0x80
}
