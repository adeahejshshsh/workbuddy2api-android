// responses.go Codex Responses 入站适配：POST /v1/responses ↔ 现有 chatCompletions。
// ponytail: 不存 previous_response_id / store；Codex 每轮带全量 input。
// 事件语义对齐 relaykit（new-api 拆出的协议转换模块，AGPL 仅作行为参考，未拷代码）：
// 推理走 reasoning_summary_* 事件族、截断如实报 response.incomplete、每事件带 sequence_number。
package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// responsesWebsocketUnsupported 回应 Codex 的 WS 升级探测（GET /v1/responses）。
// 426 是客户端**立刻且粘性**回落 HTTPS 的信号：Codex 的 websocket_fallback 用例确认
// 见到 426 只握手 1 次就转 POST，否则要空转 max_retries 次（日志里的「Reconnecting… n/5」）。
// 本网关不保存 previous_response_id / 连接内状态，WS 复用无从谈起。
func (h *Handler) responsesWebsocketUnsupported(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Upgrade", "websocket")
	http.Error(w, "websocket transport unsupported; use POST /v1/responses (set supports_websockets = false)", http.StatusUpgradeRequired)
}

func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	// 上限必须在读体之前（见 bodylimit.go）：本路径先读全量再转 chat 体。
	limit := h.limitBody(w, r)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		if bodyTooLarge(err) {
			drainBody(w, r, limit)
			writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", bodyLimitText(limit))
			return
		}
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	chat, meta, srv, err := responsesToChat(body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	// stream 标志在增强前后不变（增强只追加 messages），先建回程写入器：下面的积分闸
	// 要和 chatCompletions 里的拒绝走同一条回程（流式请求的拒绝也折成 response.failed）。
	var peek struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(chat, &peek)
	rw := &responsesWriter{ResponseWriter: w, stream: peek.Stream, meta: meta}
	defer rw.finish()
	// 声明了托管 image_generation 的请求，探测 + 出图（真扣积分）都发生在 chatCompletions
	// 的积分闸之前：闸要提前到这里，否则到顶后仍会白出一张图，对话那一步才被拒、图丢了。
	ctx := r.Context()
	if meta.imageTool {
		if !h.admitBudget(rw, r) {
			return
		}
		// 已经放行：出图会把累计推高，对话一步不能再按新累计把整个请求拒掉（见 budgetAdmittedKey）。
		ctx = context.WithValue(ctx, budgetAdmittedKey{}, true)
	}
	// 声明了托管 web_search → 网关侧执行（探测→检索→结果注入，同 /v1/messages）。
	if len(srv) > 0 {
		if augmented, _ := h.searchAugment(r.Context(), chat, srv[0]); augmented != nil {
			chat = augmented
		}
	}
	// 声明了托管 image_generation → 网关出图，成品作为输出项补进回程（见 imagetool.go）。
	if meta.imageTool {
		if augmented, items := h.imageAugment(r.Context(), chat); augmented != nil {
			chat, meta.preItems = augmented, items
		}
		if r.Context().Err() != nil { // 探测或出图期间客户端走了：不再跑对话，也不写任何回程
			rw.abort(499)
			return
		}
	}
	r2 := r.Clone(ctx)
	r2.Body = io.NopCloser(bytes.NewReader(chat))
	r2.ContentLength = int64(len(chat))
	h.chatCompletions(rw, r2)
}

func responsesToChat(src []byte) ([]byte, *responsesMeta, []serverSearchTool, error) {
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return nil, nil, nil, err
	}
	out := map[string]any{}
	meta := &responsesMeta{}
	for _, k := range []string{
		"model", "stream", "tool_choice", "max_tokens", "max_output_tokens",
		"max_completion_tokens", "temperature", "top_p", "user", "n", "stop",
		"metadata", "stream_options", "parallel_tool_calls",
		// ponytail: Codex 客户端按会话发 prompt_cache_key；丢掉就只能靠内容派生会话键。
		"prompt_cache_key",
	} {
		if v, ok := obj[k]; ok {
			out[k] = v
		}
	}
	if r, ok := obj["reasoning"].(map[string]any); ok {
		if e, ok := r["effort"]; ok {
			out["reasoning_effort"] = e
		}
	}
	if v, ok := obj["reasoning_effort"]; ok {
		out["reasoning_effort"] = v
	}
	var srv []serverSearchTool
	if tools, ok := obj["tools"].([]any); ok {
		chatTools, custom, nsMap, alias := convertTools(tools)
		// 托管 web_search / image_generation 由网关执行，不进上游 tools（见 splitHostedTools）。
		var image bool
		chatTools, srv, image = splitHostedTools(chatTools)
		meta.imageTool = image
		if len(chatTools) > 0 {
			out["tools"] = chatTools
		}
		meta.customTools, meta.nsMap, meta.alias = custom, nsMap, alias
	}
	// tool_choice 也要与出站工具名对齐：重名改名过的工具，客户端仍按原名点名。
	if v, ok := out["tool_choice"]; ok {
		out["tool_choice"] = meta.flatToolChoice(v)
	}
	msgs := []any{}
	if inst, ok := obj["instructions"].(string); ok && inst != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": inst})
	}
	switch in := obj["input"].(type) {
	case string:
		if in != "" {
			msgs = append(msgs, map[string]any{"role": "user", "content": in})
		}
	case []any:
		msgs = append(msgs, convertInputItems(in, meta)...)
	}
	out["messages"] = msgs
	b, err := json.Marshal(out)
	if err != nil {
		return nil, nil, nil, err
	}
	return b, meta, srv, nil
}

// splitHostedTools 分离 Responses 的托管工具（web_search / image_generation），返回
// 留在上游 tools 里的部分。
//
// Codex 开 tools.web_search 时声明的是托管工具 {"type":"web_search"}（或
// web_search_preview 变体），没有 name/parameters。上游收下不报错（实测 200）但也
// 不会执行——留着就是空转；本网关有执行器（searchtool.go），故摘出来自己跑，
// 由 searchAugment 拼一份 function 声明送给模型。其余托管类型（code_interpreter 等）
// 原样保留：上游对未知 type 不报错，改它没有证据支撑。
func splitHostedTools(tools []any) ([]any, []serverSearchTool, bool) {
	kept := make([]any, 0, len(tools))
	var srv []serverSearchTool
	var imageTool bool
	for _, t := range tools {
		m, ok := t.(map[string]any)
		if !ok || asString(m["name"]) != "" {
			kept = append(kept, t)
			continue
		}
		typ := asString(m["type"])
		switch {
		case strings.HasPrefix(typ, webSearchToolName):
			srv = append(srv, serverSearchTool{Name: webSearchToolName, MaxUses: int(asNum(m["max_uses"]))})
		case strings.HasPrefix(typ, imageToolName):
			imageTool = true
		default:
			kept = append(kept, t)
		}
	}
	return kept, srv, imageTool
}

// responsesMeta 请求阶段提取、回程需要的工具语义（PR #109 的两条 Codex 协议细节）：
//   - customTools：客户端声明为 custom(freeform) 的工具名（如 Codex 的 apply_patch）。
//     上游没有 freeform 概念，只能降级成单 input 参数的 function 送上去，回程再还原
//     成 custom_tool_call —— 否则模型把载荷当普通文本吐出来，客户端永远收不到工具调用。
//   - nsMap：扁平工具名 -> namespace（新版 Codex App 用 namespace 声明 MCP/插件工具组）。
//     客户端是按 (name, namespace) 二元组派发执行器的，回程必须补回 namespace。
type responsesMeta struct {
	customTools map[string]bool
	nsMap       map[string]string
	// imageTool 客户端声明了托管 image_generation：回程要补 image_generation_call 输出项。
	imageTool bool
	// preItems 网关侧已执行完的托管工具产出（image_generation_call），按序排在 output 最前。
	preItems []any
	// alias 出站扁平名 -> 客户端原名（只对重名改名过的有值）。
	// namespace 组内（或与顶层）重名时子工具被改成稳定后缀名（read_file_2），回程必须
	// 还原成客户端声明的原名，否则客户端下一轮按自己声明的名字回传工具结果就匹配不上；
	// 出站方向（历史 function_call / tool_choice）则要反着映射成扁平名。
	alias map[string]string
}

func (m *responsesMeta) isCustom(name string) bool { return m != nil && m.customTools[name] }

// flatName 反向映射：客户端原名 -> 出站扁平名（没改名过就原样返回）。
// 重名改名过的工具，原名不唯一（顶层 read_file 与 codex_app.read_file 都叫 read_file），
// 用 namespace 消歧；没有 ns 时按名字排序取第一个，保证同输入同结果。
func (m *responsesMeta) flatName(orig, ns string) string {
	if m == nil || len(m.alias) == 0 {
		return orig
	}
	best := ""
	for flat, o := range m.alias {
		if o != orig {
			continue
		}
		if ns != "" && m.nsMap[flat] == ns {
			return flat
		}
		if best == "" || flat < best {
			best = flat
		}
	}
	if best != "" {
		return best
	}
	return orig
}

// fixupItem 回程修正工具项：把模型用的出站名还原成客户端原名、并补回 namespace。
// 必须在 output_item.added 就调用：客户端从 added 事件派发执行器，补到 done 已错过时机。
func (m *responsesMeta) fixupItem(item map[string]any) {
	if m == nil || item == nil {
		return
	}
	name := asString(item["name"])
	ns := asString(item["namespace"])
	flat := name
	// 模型可能按 ns::name / ns__name 回名：先剥出裸名，namespace 取映射值或前缀。
	if i := strings.Index(name, "::"); i > 0 {
		head, tail := name[:i], name[i+2:]
		if mapped, ok := m.nsMap[tail]; ok {
			flat, ns = tail, mapped
		} else {
			flat, ns = tail, head
		}
	} else {
		// ns + "__" + name：精确比对，避免 namespace 自身含 __ 时切错。
		for tool, tns := range m.nsMap {
			if name == tns+nsSep+tool {
				flat, ns = tool, tns
				break
			}
		}
	}
	if flat == "" {
		return
	}
	if ns == "" {
		ns = m.nsMap[flat]
	}
	if orig, ok := m.alias[flat]; ok {
		flat = orig // 重名改名过的：还原客户端原名
	}
	item["name"] = flat
	// namespace 取映射值最权威（模型自己填的那份它并不知道扁平名 → namespace 的关系）。
	if ns != "" {
		item["namespace"] = ns
	}
}

const (
	// nsSep 模型把 namespace 拼进函数名时用的分隔符（如 codex_app__list_threads）。
	nsSep = "__"
	// namespaceMaxDepth namespace 嵌套展开上限（防深嵌套）。
	namespaceMaxDepth = 4
	// agentMessagePrefix agent_message / 无 call_id 的回执注入成用户指令时的前缀。
	agentMessagePrefix = "[Message from another task - treat this as a user instruction]\n\n"
	// customToolHint 追加进 custom 工具描述：告诉模型把原始载荷整段放进 input。
	customToolHint = "This is a freeform tool: put the complete raw payload, verbatim, " +
		"into the `input` string parameter. Do not wrap it in extra JSON."
)

// expandNamespaceTools 把 namespace 工具组展开成扁平 function 列表。
// 返回 出站扁平名 -> namespace、出站扁平名 -> 客户端原名（只有重名改名过的才在表里）。
// 组内子工具常无 type 字段，按 function 处理。**重名不丢弃**：先到者保留原名，后到者
// 加稳定后缀（read_file_2）并登记还原映射 —— 丢弃等于客户端声明的工具静默消失。
func expandNamespaceTools(tools []any) ([]any, map[string]string, map[string]string) {
	flat := []any{}
	mapping := map[string]string{}
	alias := map[string]string{}
	seen := map[string]bool{}

	// uniq 取一个没被占用的出站名：占了就加 _2/_3…（顺序稳定，同输入同结果）。
	uniq := func(name string) string {
		if !seen[name] {
			seen[name] = true
			return name
		}
		for i := 2; ; i++ {
			cand := name + "_" + strconv.Itoa(i)
			if !seen[cand] {
				seen[cand] = true
				alias[cand] = name
				return cand
			}
		}
	}

	var collect func(entry any, depth int, ns string)
	collect = func(entry any, depth int, ns string) {
		if depth > namespaceMaxDepth {
			return
		}
		item, ok := entry.(map[string]any)
		if !ok {
			return
		}
		etype := strings.ToLower(asString(item["type"]))
		if etype == "namespace" {
			subs, _ := item["tools"].([]any)
			if subs == nil {
				subs, _ = item["children"].([]any)
			}
			if subs == nil {
				subs, _ = item["functions"].([]any)
			}
			childNS := asString(item["name"])
			if childNS == "" {
				childNS = ns
			}
			for _, sub := range subs {
				collect(sub, depth+1, childNS)
			}
			return
		}
		if ns != "" && (etype == "" || etype == "function") {
			fn, _ := item["function"].(map[string]any)
			if fn == nil {
				fn = map[string]any{
					"name":        item["name"],
					"description": item["description"],
					"parameters":  firstNonNil(item["parameters"], item["input_schema"]),
				}
			}
			name := strings.TrimSpace(asString(fn["name"]))
			if name == "" {
				return
			}
			name = uniq(name)
			mapping[name] = ns
			params := fn["parameters"]
			if params == nil {
				params = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			flatFn := map[string]any{
				"type":        "function",
				"name":        name,
				"description": asString(fn["description"]),
				"parameters":  params,
			}
			if s, ok := fn["strict"]; ok {
				flatFn["strict"] = s
			}
			flat = append(flat, flatFn)
			return
		}
		name := asString(item["name"])
		if name == "" {
			if fn, ok := item["function"].(map[string]any); ok {
				name = asString(fn["name"])
			}
		}
		if name != "" {
			out := uniq(name)
			if ns != "" {
				mapping[out] = ns
			}
			if out != name {
				setToolName(item, out) // 被重名挤出原名：改写出站名，回程按 alias 还原
			}
		}
		flat = append(flat, item)
	}

	for _, entry := range tools {
		collect(entry, 0, "")
	}
	return flat, mapping, alias
}

// setToolName 改写出站工具名：兼容 {"type":"function","function":{...}} 与裸 function 两种形态。
func setToolName(item map[string]any, name string) {
	if fn, ok := item["function"].(map[string]any); ok {
		fn["name"] = name
		return
	}
	item["name"] = name
}

// downgradeCustomTool 把 custom(freeform) 工具降级成单 input 参数的 Chat function。
// 只返回 function 体（name/description/parameters），由调用方按本仓约定包一层
// {"type":"function","function":...}。
func downgradeCustomTool(tool map[string]any) map[string]any {
	extra := ""
	if format, ok := tool["format"].(map[string]any); ok {
		if def := asString(format["definition"]); def != "" {
			extra = "\n\nGrammar:\n" + def
		}
	}
	return map[string]any{
		"name":        asString(tool["name"]),
		"description": strings.TrimSpace(asString(tool["description"]) + "\n\n" + customToolHint + extra),
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"input": map[string]any{
					"type":        "string",
					"description": "Complete raw payload for this tool, verbatim.",
				},
			},
			"required": []any{"input"},
		},
	}
}

// unwrapCustomInput 严格还原 freeform 载荷，返回 (载荷, 是否可信)。
//
// 只接受两种形态：`{"input": "<非空字符串>"}`（单键）与裸 JSON 字符串。其余
// （非法 JSON、多键、input 非字符串、空串）一律 ok=false —— custom 的载荷会被
// 客户端直接执行（apply_patch 就是打补丁），宁可让它看到失败，也不能把形态不明或
// 半截的内容当成可执行调用发出去。
func unwrapCustomInput(args string) (string, bool) {
	s := strings.TrimSpace(args)
	if s == "" {
		return "", false
	}
	var parsed any
	if json.Unmarshal([]byte(s), &parsed) != nil {
		return "", false
	}
	switch v := parsed.(type) {
	case string:
		if v == "" {
			return "", false
		}
		return v, true
	case map[string]any:
		if len(v) != 1 {
			return "", false
		}
		in, ok := v["input"].(string)
		if !ok || in == "" {
			return "", false
		}
		return in, true
	}
	return "", false
}

// marshalCustomInput 把回传的 freeform 输入打包成 Chat 工具参数（与 downgrade 对称）。
func marshalCustomInput(v any) string {
	s, ok := v.(string)
	if !ok {
		if v == nil {
			s = ""
		} else if b, err := json.Marshal(v); err == nil {
			s = string(b)
		}
	}
	b, err := json.Marshal(map[string]any{"input": s})
	if err != nil {
		return "{}"
	}
	return string(b)
}

func firstNonNil(vals ...any) any {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}

// convertTools 把 Responses 工具定义转成上游 Chat 形态，并带出回程需要的语义
// （custom 工具名集合、出站名 -> namespace、出站名 -> 客户端原名）。
func convertTools(tools []any) ([]any, map[string]bool, map[string]string, map[string]string) {
	flat, nsMap, alias := expandNamespaceTools(tools)
	out := make([]any, 0, len(flat))
	custom := map[string]bool{}
	for _, t := range flat {
		m, ok := t.(map[string]any)
		if !ok {
			out = append(out, t)
			continue
		}
		if strings.EqualFold(asString(m["type"]), "custom") {
			if n := asString(m["name"]); n != "" {
				custom[n] = true
			}
			// 与下方普通 function 同款嵌套形状（上游只认 {"type","function"} 包裹）。
			out = append(out, map[string]any{"type": "function", "function": downgradeCustomTool(m)})
			continue
		}
		if _, ok := m["function"].(map[string]any); ok {
			out = append(out, m)
			continue
		}
		typ, _ := m["type"].(string)
		if typ == "" || typ == "function" {
			fn := map[string]any{}
			for _, k := range []string{"name", "description", "parameters", "strict"} {
				if v, ok := m[k]; ok {
					fn[k] = v
				}
			}
			out = append(out, map[string]any{"type": "function", "function": fn})
			continue
		}
		out = append(out, m)
	}
	return out, custom, nsMap, alias
}

// flatToolChoice tool_choice 点名了重名改名过的工具时，映射成出站扁平名。
func (m *responsesMeta) flatToolChoice(v any) any {
	if m == nil || len(m.alias) == 0 {
		return v
	}
	switch t := v.(type) {
	case string:
		return m.flatName(t, "")
	case map[string]any:
		ns := asString(t["namespace"])
		if n := asString(t["name"]); n != "" {
			t["name"] = m.flatName(n, ns)
		}
		if fn, ok := t["function"].(map[string]any); ok {
			if n := asString(fn["name"]); n != "" {
				fn["name"] = m.flatName(n, ns)
			}
		}
		return t
	}
	return v
}

// appendVisibleReasoning 把两段思考文本按序拼成回放用的 reasoning_content：任一为空返回
// 另一段；两段完全相同只留一份（客户端可能把同一段思考既放进 reasoning 项又内联在
// assistant 消息上，重复拼接会让上游读到两遍）；否则用 "\n" 连接。不做 trim：思考里的
// 缩进与换行按原样送达上游，模型才能按原始格式续读。
func appendVisibleReasoning(current, addition string) string {
	if addition == "" {
		return current
	}
	if current == "" {
		return addition
	}
	if current == addition {
		return current
	}
	return current + "\n" + addition
}

// reasoningItemText 提取 reasoning 项里的思考文本。Responses 协议标准形态是 summary 数组
// （各段 {"type":"summary_text","text":...}），取各段 text 用 "\n" 连接；summary 是字符串
// 时直取；两者皆空再退到 content（字符串直取，数组取各 text part）。全都取不到返回空串，
// 调用方按「没有思考」跳过，不产出字段（Codex 会发 {"type":"reasoning","summary":[]}）。
func reasoningItemText(m map[string]any) string {
	if s, ok := m["summary"].(string); ok && s != "" {
		return s
	}
	if parts := textParts(m["summary"]); len(parts) > 0 {
		return strings.Join(parts, "\n")
	}
	if c, ok := m["content"].(string); ok && c != "" {
		return c
	}
	if parts := textParts(m["content"]); len(parts) > 0 {
		return strings.Join(parts, "\n")
	}
	return ""
}

// textParts 从数组形态的 summary/content 里按序取各 map 项的 text 字段（空 text 跳过）。
func textParts(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var parts []string
	for _, p := range arr {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if t := asString(pm["text"]); t != "" {
			parts = append(parts, t)
		}
	}
	return parts
}

// replayMetaTypes 元数据/控制项：不生成消息。
var replayMetaTypes = map[string]bool{
	"additional_tools": true, "compaction_trigger": true,
	"local_shell_call": true, "local_shell_call_output": true,
}

// replayHandledTypes convertInputItems 里有专门分支的类型（与那里的 switch 一一对应），
// 其余类型都落默认分支按消息处理。
var replayHandledTypes = map[string]bool{
	"message": true, "reasoning": true, "item_reference": true,
	"compaction": true, "compaction_summary": true, "context_compaction": true,
	"function_call": true, "function_call_output": true,
	"custom_tool_call": true, "custom_tool_call_output": true,
	"agent_message": true,
}

// replayInert 判断回放项是否「不产生消息、也不动任何状态」：元数据项，以及不认识、没有
// role、也没有可用内容的类型项——托管工具的回执（web_search_call、image_generation_call、
// mcp_call、computer_call、file_search_call、code_interpreter_call 等）都是这种形状，
// 上游没有对应物。它们落进默认分支会变成一条 user 空消息：拆开同一批工具调用、把工具图
// 插进并行 tool 结果之间（11148）、清空调用前暂存的思考。前瞻归属与主循环共用这一个判据，
// 两边对同一项的看法才不会分叉。带内容的未知项照旧按消息送达，不因此丢内容。
//
// 注意：网关自己代执行的出图产出（responsesMeta.preItems）是**回程**项，不走这里；
// 本函数只看客户端回放的入站历史，跳过的也只是回执本身，成品由 preItems 照旧交付。
func replayInert(m map[string]any) bool {
	typ, _ := m["type"].(string)
	if replayMetaTypes[typ] {
		return true
	}
	if typ == "" || replayHandledTypes[typ] || m["role"] != nil {
		return false
	}
	c, isText := convertContent(m["content"]).(string)
	return isText && strings.TrimSpace(c) == ""
}

// reasoningGroupTarget 从 start+1 起找 reasoning 项之后第一个「实质项」：跳过后续的
// reasoning（连续段同属一组）、item_reference（主循环里不产消息、不清思考）和 replayInert
// 项。找不到返回 nil（对话到此结束）。
func reasoningGroupTarget(items []any, start int) map[string]any {
	for i := start + 1; i < len(items); i++ {
		m, ok := items[i].(map[string]any)
		if !ok {
			continue
		}
		if typ, _ := m["type"].(string); typ == "reasoning" || typ == "item_reference" || replayInert(m) {
			continue
		}
		return m
	}
	return nil
}

// reasoningStartsAssistant 判断前瞻目标是否会开启一段新的 assistant 内容（assistant 消息
// 或工具调用批次）：会，则思考暂存等它落地；不会，则思考归前一条 assistant。target 为 nil
// （其后再无实质项）时返回 false。
func reasoningStartsAssistant(target map[string]any) bool {
	if target == nil {
		return false
	}
	typ, _ := target["type"].(string)
	switch typ {
	case "function_call", "custom_tool_call":
		return true
	case "message", "user", "":
		role, _ := target["role"].(string)
		return role == "assistant"
	}
	return false
}

// compactionSummaryMax 交接摘要送上游的字节上限。摘要每轮都随历史重发，来源又是客户端，
// 不封顶时一份异常大的摘要会让此后每一轮都更早撞上 11115。
const compactionSummaryMax = 32 << 10

// opaqueCompactionBlob 只由 base64 / base64url / token 字符组成、且够长的串：真实的
// encrypted_content 是密文（URL-safe base64 不带 padding 时标准解码会失败），解不出摘要
// 就是对上游无意义的乱码。纯文本摘要总会有空白或非 ASCII 字符；短于 32 的放行，免得
// 「Done」这类单词被当成密文。
var opaqueCompactionBlob = regexp.MustCompile(`^[A-Za-z0-9+/=_.\-]{32,}$`)

// decodeCompactionSummary 尽力解码 compaction 项的 encrypted_content：正常形态是
// base64(JSON {"v":1,"summary":...})，先补齐 base64 padding 再解，解出的 summary 为空就是
// 没有摘要（不把 JSON 原文当摘要）；解不开时，像密文的丢弃，其余按纯文本摘要保留——外部
// 提供者可能塞的是纯文本摘要，丢了它就丢了唯一一份上下文交接。结果封顶 compactionSummaryMax。
//
// 本仓没有网关侧生成 compaction 的路径（既不出也不改上下文压缩结果），这里只处理客户端
// 回放进来的项：不解码就等于把一段看不懂的密文每轮重发给上游。
func decodeCompactionSummary(blob string) string {
	text := strings.TrimSpace(blob)
	if text == "" {
		return ""
	}
	padded := text + strings.Repeat("=", (4-len(text)%4)%4)
	if raw, err := base64.StdEncoding.DecodeString(padded); err == nil {
		var obj struct {
			Summary string `json:"summary"`
		}
		if json.Unmarshal(raw, &obj) == nil {
			return logfmt.Truncate(obj.Summary, compactionSummaryMax)
		}
	}
	if opaqueCompactionBlob.MatchString(text) {
		return ""
	}
	return logfmt.Truncate(text, compactionSummaryMax)
}

// convertInputItems 把 /v1/responses 的 input 项转成 Chat messages。
//
// 思考（reasoning）项不再整项丢弃，而是按原样补回发给上游的消息里（消息级
// reasoning_content）：多轮长任务里模型要能看到自己上一轮的思考，读 reasoning 的模型家族
// （kimi/glm/minimax 等）才不会在一致性校验上退化——此前客户端回放的历史思考到不了上游，
// 表现就是「越干越笨」。
func convertInputItems(items []any, meta *responsesMeta) []any {
	var out []any
	var pending []any
	// pendingReasoning 暂存 reasoning 项提取出的思考文本，等它所属的 assistant 内容
	// （正文消息或工具调用批次）落地时挂上去。
	var pendingReasoning string
	// 工具结果里的图片要等本轮工具结果全部落地才发：插在 assistant.tool_calls 与它的
	// tool 消息之间会打断配对（并行工具调用时判 11148）。
	var toolImgs []any
	flushImgs := func() {
		if len(toolImgs) == 0 {
			return
		}
		out = append(out, map[string]any{
			"role":    "user",
			"content": append([]any{map[string]any{"type": "text", "text": toolImagePlaceholder}}, toolImgs...),
		})
		toolImgs = nil
	}
	flush := func() {
		if len(pending) == 0 {
			return
		}
		msg := map[string]any{"role": "assistant", "content": "", "tool_calls": pending}
		if pendingReasoning != "" {
			// 工具调用前的思考段挂在同一批 assistant 上：思考与调用本是同一轮决策，
			// 拆开会让该轮在多轮校验里裸奔。
			msg["reasoning_content"] = pendingReasoning
			pendingReasoning = ""
		}
		out = append(out, msg)
		pending = nil
	}
	for idx, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			// 非 dict 项（纯字符串等）按 user 侧边界处理：没等到归属的过期思考就地
			// 丢弃，绝不跨轮泄漏到不相关的对话里。
			pendingReasoning = ""
			continue
		}
		typ, _ := m["type"].(string)
		if typ == "" && m["role"] == "tool" && asString(m["tool_call_id"]) != "" {
			// Chat 形状的工具结果（客户端把 chat messages 直接塞进 input）按
			// function_call_output 处理：落进默认分支只剩 role/content，tool_call_id 丢了，
			// 出站的 cleanupOrphanToolCalls 会把这一对调用与结果一起删掉。走同一分支也就
			// 同样拆出图片、同样不在并行结果之间落图。
			m = map[string]any{"type": "function_call_output", "call_id": m["tool_call_id"],
				"name": m["name"], "output": m["content"]}
			typ = "function_call_output"
		}
		if replayInert(m) {
			// 元数据项与托管工具回执：不生成消息、不清空 pendingReasoning、不落图——
			// 被它们隔开的连续思考段、同一批工具调用与并行工具结果都保持原样。
			continue
		}
		// 图片的落点是「这一轮工具到此为止」的那个项，**不能挂在 flush() 里**：
		// flush() 在每条 function_call_output 开头都会跑，于是并行工具调用
		// （assistant 一条带多个 tool_calls）处理第二条结果时，会先把第一条结果的图
		// 落下去，正好插进 c1 与 c2 两条 tool 消息之间 —— 后面那条 tool_call_id 就此
		// 失去应答。Anthropic 侧的 hasToolResult(blocks) 是同一个判据（message 级）。
		if typ != "function_call_output" && typ != "custom_tool_call_output" {
			flushImgs()
		}
		switch typ {
		case "reasoning":
			flush()
			if text := reasoningItemText(m); text != "" {
				merged := false
				// 前瞻判断归属：后面紧跟新的 assistant 内容就暂存等它落地；否则
				// （对话末尾、后面是 user/tool 边界）归前一条 assistant。
				if target := reasoningGroupTarget(items, idx); !reasoningStartsAssistant(target) && len(out) > 0 {
					if prev, ok := out[len(out)-1].(map[string]any); ok && prev["role"] == "assistant" {
						prev["reasoning_content"] = appendVisibleReasoning(asString(prev["reasoning_content"]), text)
						merged = true
					}
				}
				if !merged {
					pendingReasoning = appendVisibleReasoning(pendingReasoning, text)
				}
			}
			continue
		case "item_reference":
			// 引用型项指向不存在的存档，无从解引用，维持跳过。
			flush()
			continue
		case "compaction", "compaction_summary", "context_compaction":
			// 上一轮的上下文压缩结果：对上游它是后续对话赖以延续的交接摘要，以
			// system 消息保留在回放历史里；解码不出内容则跳过，不造空消息。
			flush()
			pendingReasoning = ""
			if summary := decodeCompactionSummary(asString(m["encrypted_content"])); strings.TrimSpace(summary) != "" {
				out = append(out, map[string]any{
					"role":    "system",
					"content": "Summary of the earlier conversation (context checkpoint):\n\n" + strings.TrimSpace(summary),
				})
			}
			continue
		case "function_call", "custom_tool_call":
			pending = append(pending, toToolCall(m, meta))
			continue
		case "function_call_output", "custom_tool_call_output":
			flush()
			// 工具结果属于 user 侧回应：其前未归属的思考已被 flush 消费或就此过期。
			pendingReasoning = ""
			if id := asString(m["call_id"]); id != "" {
				txt, imgs := splitToolImages(m["output"])
				msg := map[string]any{
					"role":         "tool",
					"tool_call_id": id,
					"content":      txt,
				}
				if name := asString(m["name"]); name != "" {
					msg["name"] = name // 只有 Chat 形状的结果带 name，原样保留
				}
				out = append(out, msg)
				toolImgs = append(toolImgs, imgs...)
				continue
			}
			// 无 call_id 的孤儿回执（来自其他任务）：同 agent_message 注入成用户指令，
			// 不伪装成工具结果——空 tool_call_id 到上游只会撞 11148 配对失败。
			// 带图时 convertContent 返回结构化 parts，必须整段保留：只取 text part 会把图
			// 悄悄丢掉（移植上游 #145，那里该分支按字符串处理，客户端每次重试都成裸 502）。
			c := convertContent(m["output"])
			if parts, ok := c.([]any); ok {
				out = append(out, map[string]any{
					"role": "user",
					"content": append([]any{map[string]any{
						"type": "text", "text": agentMessagePrefix,
					}}, parts...),
				})
				continue
			}
			if txt, _ := c.(string); strings.TrimSpace(txt) != "" {
				out = append(out, map[string]any{"role": "user", "content": agentMessagePrefix + txt})
			}
			continue
		case "agent_message":
			// 来自其他任务（子代理）的消息：按用户指令注入，别伪装成工具结果。
			flush()
			pendingReasoning = ""
			if txt := asContentString(m["content"]); strings.TrimSpace(txt) != "" {
				out = append(out, map[string]any{"role": "user", "content": agentMessagePrefix + txt})
			}
			continue
		}
		flush()
		role, _ := m["role"].(string)
		if role == "" {
			role = "user"
		}
		msg := map[string]any{"role": role, "content": convertContent(m["content"])}
		if role == "assistant" {
			// 思考与正文/工具声明同轮保真：pendingReasoning 是这轮正前的思考项；
			// Chat 形状回放的 assistant 还可能内联 reasoning_content 与 tool_calls——
			// 丢 tool_calls 会造出孤儿 tool 消息（上游 11148），丢思考则该轮裸奔。
			if pendingReasoning != "" {
				msg["reasoning_content"] = pendingReasoning
				pendingReasoning = ""
			}
			if rc := asString(m["reasoning_content"]); rc != "" {
				msg["reasoning_content"] = appendVisibleReasoning(asString(msg["reasoning_content"]), rc)
			}
			if tcs, ok := m["tool_calls"].([]any); ok && len(tcs) > 0 {
				msg["tool_calls"] = tcs
			}
		} else {
			// user/system 边界：没等到归属的过期思考就地丢弃，绝不跨轮泄漏。
			pendingReasoning = ""
		}
		out = append(out, msg)
	}
	flush()
	flushImgs()
	return out
}

// splitToolImages 拆开工具输出的正文与图片：Chat 的 tool 消息不许带图（OpenAI 直接
// 400 "Image URLs are only allowed for messages with role 'user'"），图片交给调用方
// 以下一条 user 消息送上游。少了这一步图片会被静默丢掉（Codex 的 view_image 把图
// 放在 function_call_output.output 的数组里）：模型看不到图却照上下文编答案。
func splitToolImages(output any) (string, []any) {
	parts, ok := convertContent(output).([]any)
	if !ok {
		return asContentString(output), nil
	}
	var texts []string
	var imgs []any
	for _, p := range parts {
		mm, _ := p.(map[string]any)
		if asString(mm["type"]) == "image_url" {
			imgs = append(imgs, mm)
			continue
		}
		texts = append(texts, asString(mm["text"]))
	}
	return strings.Join(texts, ""), imgs
}

func toToolCall(m map[string]any, meta *responsesMeta) map[string]any {
	callID := asString(m["call_id"])
	if callID == "" {
		callID = asString(m["id"])
	}
	args := asString(m["arguments"])
	if asString(m["type"]) == "custom_tool_call" {
		// freeform 输入是整段字符串：打包成与降级工具对称的 {"input": ...}。
		args = marshalCustomInput(m["input"])
	}
	if args == "" {
		args = "{}"
	}
	// 历史里客户端用自己声明的原名；重名改名过的要映射成我们声明的出站名，否则对不上。
	// 客户端回传的 item 若带 namespace，用它消歧同名工具。
	return map[string]any{
		"id":   callID,
		"type": "function",
		"function": map[string]any{
			"name":      meta.flatName(asString(m["name"]), asString(m["namespace"])),
			"arguments": args,
		},
	}
}

func convertContent(c any) any {
	switch v := c.(type) {
	case string:
		return v
	case []any:
		var texts []string
		var parts []any
		hasNonText := false
		for _, p := range v {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			switch typ {
			case "input_text", "output_text", "text", "":
				t := asString(m["text"])
				texts = append(texts, t)
				parts = append(parts, map[string]any{"type": "text", "text": t})
			case "input_image":
				hasNonText = true
				parts = append(parts, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": imageURLFrom(m)},
				})
			}
		}
		if !hasNonText {
			return strings.Join(texts, "")
		}
		return parts
	default:
		return asContentString(c)
	}
}

func imageURLFrom(m map[string]any) string {
	switch u := m["image_url"].(type) {
	case string:
		return u
	case map[string]any:
		return asString(u["url"])
	}
	return asString(m["url"])
}

func chatCompletionToResponse(obj map[string]any, meta *responsesMeta) map[string]any {
	id, _ := obj["id"].(string)
	model, _ := obj["model"].(string)
	var created int64
	switch v := obj["created"].(type) {
	case float64:
		created = int64(v)
	case int64:
		created = v
	case int:
		created = int64(v)
	}
	var msg map[string]any
	fr := ""
	if chs, ok := obj["choices"].([]any); ok && len(chs) > 0 {
		if c, ok := chs[0].(map[string]any); ok {
			msg, _ = c["message"].(map[string]any)
			fr, _ = c["finish_reason"].(string)
		}
	}
	status, details := responsesStatusFromFinish(fr)
	output := []any{}
	if msg != nil {
		if rc, ok := msg["reasoning_content"].(string); ok && rc != "" {
			output = append(output, map[string]any{
				"id":      "rs_" + id,
				"type":    "reasoning",
				"status":  status,
				"summary": []any{map[string]any{"type": "summary_text", "text": rc}},
			})
		}
		content, _ := msg["content"].(string)
		tcs, _ := msg["tool_calls"].([]any)
		if content != "" || len(tcs) == 0 {
			output = append(output, map[string]any{
				"id":      "msg_" + id,
				"type":    "message",
				"status":  status,
				"role":    "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": content}},
			})
		}
		for _, tci := range tcs {
			tc, _ := tci.(map[string]any)
			if tc == nil {
				continue
			}
			fn, _ := tc["function"].(map[string]any)
			name, args := "", ""
			if fn != nil {
				name = asString(fn["name"])
				args = asString(fn["arguments"])
			}
			callID := asString(tc["id"])
			var item map[string]any
			if meta.isCustom(name) {
				// custom(freeform) 回程还原：载荷是整段字符串，不是 JSON 参数。
				// 形态非法（截断的半截 JSON / 多键 / 非字符串）时按 incomplete 下发：
				// 客户端不会执行未完成的调用，也不能拿半截补丁去打补丁。
				input, ok := unwrapCustomInput(args)
				itemStatus := status
				if !ok {
					log.Printf("WARN: [responses] custom 载荷形态非法，按不可执行下发 name=%s", name)
					input, itemStatus = "", "incomplete"
				}
				item = map[string]any{
					"id":      callID,
					"type":    "custom_tool_call",
					"call_id": callID,
					"name":    name,
					"input":   input,
					"status":  itemStatus,
				}
			} else {
				item = map[string]any{
					"id":        callID,
					"type":      "function_call",
					"call_id":   callID,
					"name":      name,
					"arguments": args,
					"status":    status,
				}
			}
			meta.fixupItem(item)
			output = append(output, item)
		}
	}
	// 网关侧已执行完的托管工具产出（image_generation_call）排在最前：它们在对话开始
	// 之前就完成了，客户端按序渲染。
	if meta != nil && len(meta.preItems) > 0 {
		output = append(append([]any{}, meta.preItems...), output...)
	}
	// 非流式同样不允许 output:null（nil 切片）：空响应是合法形态。
	if output == nil {
		output = []any{}
	}
	out := map[string]any{
		"id":         respID(id),
		"object":     "response",
		"created_at": created,
		"status":     status,
		"model":      model,
		"output":     output,
	}
	if details != nil {
		out["incomplete_details"] = details
	}
	if u, ok := obj["usage"].(map[string]any); ok && u != nil {
		out["usage"] = convertUsage(u)
	}
	return out
}

func convertUsage(u map[string]any) map[string]any {
	out := map[string]any{}
	if v, ok := u["prompt_tokens"]; ok {
		out["input_tokens"] = v
	}
	if v, ok := u["completion_tokens"]; ok {
		out["output_tokens"] = v
	}
	if v, ok := u["total_tokens"]; ok {
		out["total_tokens"] = v
	}
	// ponytail: Responses 客户端的缓存读数在 input_tokens_details.cached_tokens
	// （Codex 据此显示 cached），上游已在 prompt_tokens_details.cached_tokens 给出。
	if d, ok := u["prompt_tokens_details"].(map[string]any); ok {
		if c, ok := d["cached_tokens"]; ok {
			out["input_tokens_details"] = map[string]any{"cached_tokens": c}
		}
	}
	return out
}

func respID(chatID string) string {
	if chatID == "" {
		chatID = "wb2api"
	}
	if strings.HasPrefix(chatID, "resp_") {
		return chatID
	}
	return "resp_" + chatID
}

type responsesWriter struct {
	http.ResponseWriter
	stream  bool
	status  int
	hdrSent bool
	rest    []byte
	body    []byte
	x       streamState
	meta    *responsesMeta
	aborted bool // 客户端已走：finish 什么都不写（否则流式会补一条 200 + response.failed，被记成成功）
}

// abort 客户端已断开时收尾：把状态码直接交给底层写入器（请求统计据此记 499），之后不再写任何内容。
func (w *responsesWriter) abort(code int) {
	w.aborted, w.status = true, code
	w.ResponseWriter.WriteHeader(code)
}

type streamState struct {
	created, failed, completed bool
	// sawFinish/sawDone 上游正常收尾的两种标志：finish_reason 与 [DONE]。
	// 两者都没有就结束 = 异常 EOF / 断流，此时工具载荷不可信（见 closeOpenItems）。
	sawFinish, sawDone    bool
	id, model             string
	createdAt             int64
	outN                  int
	seq                   int // SSE 事件序号（sequence_number，当前 Responses wire 契约要求）
	status                string
	incompleteDetails     map[string]any
	reasoningOpen         bool
	reasoningIdx          int
	reasoningID           string
	reasoningText         strings.Builder
	messageOpen, textOpen bool
	messageIdx            int
	msgID                 string
	messageText           strings.Builder
	tools                 map[int]*toolAcc
	toolOrder             []int
	usage                 map[string]any
	output                []any
}

type toolAcc struct {
	idx, outIdx    int
	id, name       string
	args           strings.Builder
	opened, closed bool
	custom         bool // 客户端声明为 custom(freeform)：回程发 custom_tool_call 类 item
}

// toolDeltaEvent 按工具类型选流式增量事件名（custom 是 input 语义而非 arguments）。
func (w *responsesWriter) toolDeltaEvent(acc *toolAcc) string {
	if acc.custom {
		return "response.custom_tool_call_input.delta"
	}
	return "response.function_call_arguments.delta"
}

func (w *responsesWriter) Header() http.Header { return w.ResponseWriter.Header() }

func (w *responsesWriter) WriteHeader(code int) { w.status = code }

func (w *responsesWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *responsesWriter) Write(p []byte) (int, error) {
	if w.stream && strings.Contains(w.Header().Get("Content-Type"), "event-stream") {
		w.rest = append(w.rest, p...)
		for {
			i := bytes.Index(w.rest, []byte("\n\n"))
			if i < 0 {
				break
			}
			frame := string(bytes.TrimSpace(w.rest[:i]))
			w.rest = w.rest[i+2:]
			if err := w.handleFrame(frame); err != nil {
				return len(p), err
			}
		}
		return len(p), nil
	}
	w.body = append(w.body, p...)
	return len(p), nil
}

func (w *responsesWriter) finish() {
	if w.aborted {
		return
	}
	if w.stream {
		if strings.Contains(w.Header().Get("Content-Type"), "event-stream") || w.hdrSent {
			if w.x.created && !w.x.completed && !w.x.failed {
				_ = w.closeOpenItems()
				_ = w.emitCompleted()
			}
			return
		}
		// W2-08：零字节时的真实 HTTP 错误（首帧否决/上游 4xx/5xx）按状态码 +
		// JSON 错误体返回。此前 emitJSONErrorAsFailed 会写成 200 + response.failed
		// SSE，Codex 按状态码做的退避重试全部失效。
		if w.status >= 400 {
			w.Header().Set("Content-Type", "application/json")
			w.ResponseWriter.WriteHeader(w.status)
			if len(w.body) > 0 {
				_, _ = w.ResponseWriter.Write(w.body)
			}
			return
		}
		_ = w.emitJSONErrorAsFailed()
		return
	}
	w.flushJSON()
}

func (w *responsesWriter) flushJSON() {
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	if len(w.body) == 0 {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	var obj map[string]any
	if json.Unmarshal(w.body, &obj) != nil {
		w.ResponseWriter.WriteHeader(status)
		_, _ = w.ResponseWriter.Write(w.body)
		return
	}
	if _, hasErr := obj["error"]; hasErr {
		w.ResponseWriter.WriteHeader(status)
		_, _ = w.ResponseWriter.Write(w.body)
		return
	}
	raw, err := json.Marshal(chatCompletionToResponse(obj, w.meta))
	if err != nil {
		w.ResponseWriter.WriteHeader(status)
		_, _ = w.ResponseWriter.Write(w.body)
		return
	}
	w.ResponseWriter.WriteHeader(status)
	_, _ = w.ResponseWriter.Write(raw)
}

func (w *responsesWriter) emitJSONErrorAsFailed() error {
	var obj map[string]any
	_ = json.Unmarshal(w.body, &obj)
	return w.emitFailed(obj["error"])
}

func (w *responsesWriter) handleFrame(frame string) error {
	if w.x.failed || w.x.completed {
		return nil
	}
	// SSE 注释帧（上游 ": heartbeat"、本层 ": keepalive"）：协议规定的 no-op，
	// 但**必须透传给客户端**——它的唯一作用就是让连接「有字节流动」，吞掉等于
	// 把上游的保活变成客户端可见的静默，CodeBuddy/Codex 的 stream idle 超时
	// 会据此掐线。注释不占 sequence_number（对事件序无感知）。
	if strings.HasPrefix(frame, ":") {
		return w.emitRaw(frame + "\n\n")
	}
	if strings.HasPrefix(frame, "data: [DONE]") {
		w.x.sawDone = true
		_ = w.closeOpenItems()
		return w.emitCompleted()
	}
	payload, ok := cutDataPayload(frame)
	if !ok {
		return nil
	}
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		return nil
	}
	if errObj, ok := obj["error"]; ok {
		return w.emitFailed(errObj)
	}
	return w.handleChunk(obj)
}

func (w *responsesWriter) handleChunk(obj map[string]any) error {
	if !w.x.created {
		id, _ := obj["id"].(string)
		model, _ := obj["model"].(string)
		w.x.id = id
		w.x.model = model
		w.x.created = true
		if v, ok := obj["created"].(float64); ok && v > 0 {
			w.x.createdAt = int64(v)
		} else {
			w.x.createdAt = time.Now().Unix()
		}
		if err := w.emit("response.created", map[string]any{
			"response": map[string]any{
				"id":         respID(id),
				"object":     "response",
				"status":     "in_progress",
				"created_at": w.x.createdAt,
				"model":      model,
				"output":     []any{},
			},
		}); err != nil {
			return err
		}
		if err := w.emitPreItems(); err != nil {
			return err
		}
	}
	if u, ok := obj["usage"].(map[string]any); ok && u != nil {
		w.x.usage = u
	}
	chs, _ := obj["choices"].([]any)
	if len(chs) == 0 {
		return nil
	}
	c, _ := chs[0].(map[string]any)
	if c == nil {
		return nil
	}
	if delta, _ := c["delta"].(map[string]any); delta != nil {
		if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
			if err := w.ensureReasoning(); err != nil {
				return err
			}
			w.x.reasoningText.WriteString(rc)
			if err := w.emit("response.reasoning_summary_text.delta", map[string]any{
				"item_id":       w.x.reasoningID,
				"output_index":  w.x.reasoningIdx,
				"summary_index": 0,
				"delta":         rc,
			}); err != nil {
				return err
			}
		}
		if txt, ok := delta["content"].(string); ok && txt != "" {
			if err := w.ensureMessageText(); err != nil {
				return err
			}
			w.x.messageText.WriteString(txt)
			if err := w.emit("response.output_text.delta", map[string]any{
				"item_id":       w.x.msgID,
				"output_index":  w.x.messageIdx,
				"content_index": 0,
				"delta":         txt,
			}); err != nil {
				return err
			}
		}
		if tcs, ok := delta["tool_calls"].([]any); ok {
			if err := w.handleToolDeltas(tcs); err != nil {
				return err
			}
		}
	}
	if fr, ok := c["finish_reason"].(string); ok && fr != "" {
		w.x.sawFinish = true
		w.applyFinishReason(fr)
		return w.closeOpenItems()
	}
	return nil
}

// applyFinishReason 把上游截断如实映射成 incomplete（relaykit 同款语义）：
// max_output_tokens / content_filter 都不允许伪装成 completed，否则客户端
// （Codex）会把被截断的输出当作完整结果。
func (w *responsesWriter) applyFinishReason(fr string) {
	status, details := responsesStatusFromFinish(fr)
	if status != "" {
		w.x.status = status
		w.x.incompleteDetails = details
	}
}

func responsesStatusFromFinish(fr string) (string, map[string]any) {
	switch strings.TrimSpace(fr) {
	case "length", "max_tokens":
		return "incomplete", map[string]any{"reason": "max_output_tokens"}
	case "content_filter":
		return "incomplete", map[string]any{"reason": "content_filter"}
	default:
		return "completed", nil
	}
}

// itemStatus 关闭 item 时用的状态：截断时 item 也要标 incomplete。
func (w *responsesWriter) itemStatus() string {
	if w.x.status == "incomplete" {
		return "incomplete"
	}
	return "completed"
}

func (w *responsesWriter) nextIdx() int {
	i := w.x.outN
	w.x.outN++
	return i
}

func (w *responsesWriter) ensureReasoning() error {
	if w.x.reasoningOpen {
		return nil
	}
	w.x.reasoningIdx = w.nextIdx()
	w.x.reasoningID = "rs_" + w.x.id
	w.x.reasoningOpen = true
	if err := w.emit("response.output_item.added", map[string]any{
		"output_index": w.x.reasoningIdx,
		"item": map[string]any{
			"id":      w.x.reasoningID,
			"type":    "reasoning",
			"summary": []any{},
		},
	}); err != nil {
		return err
	}
	return w.emit("response.reasoning_summary_part.added", map[string]any{
		"item_id":       w.x.reasoningID,
		"output_index":  w.x.reasoningIdx,
		"summary_index": 0,
		"part":          map[string]any{"type": "summary_text"},
	})
}

func (w *responsesWriter) ensureMessageText() error {
	// 这里**不**收尾 reasoning：上游会把 reasoning_content 与 content 交错下发
	// （思考→正文→再思考），首段正文就 done 掉会让后续思考重开一个 item，
	// 客户端表现为「思考→显示→又思考」反复循环。reasoning 只在换工具调用或
	// 收尾时关闭（新段落由 ensureReasoning 记住新的 item id）。
	if w.x.textOpen {
		return nil
	}
	if !w.x.messageOpen {
		w.x.messageIdx = w.nextIdx()
		w.x.msgID = "msg_" + w.x.id
		w.x.messageOpen = true
		if err := w.emit("response.output_item.added", map[string]any{
			"output_index": w.x.messageIdx,
			"item": map[string]any{
				"id":      w.x.msgID,
				"type":    "message",
				"status":  "in_progress",
				"role":    "assistant",
				"content": []any{},
			},
		}); err != nil {
			return err
		}
	}
	w.x.textOpen = true
	return w.emit("response.content_part.added", map[string]any{
		"item_id":       w.x.msgID,
		"output_index":  w.x.messageIdx,
		"content_index": 0,
		"part":          map[string]any{"type": "output_text", "text": ""},
	})
}

func (w *responsesWriter) handleToolDeltas(tcs []any) error {
	if err := w.closeReasoning(); err != nil {
		return err
	}
	if err := w.closeMessage(); err != nil {
		return err
	}
	if w.x.tools == nil {
		w.x.tools = map[int]*toolAcc{}
	}
	for _, tci := range tcs {
		tc, _ := tci.(map[string]any)
		if tc == nil {
			continue
		}
		idx := 0
		if v, ok := tc["index"].(float64); ok {
			idx = int(v)
		}
		acc, ok := w.x.tools[idx]
		if !ok {
			acc = &toolAcc{idx: idx}
			w.x.tools[idx] = acc
			w.x.toolOrder = append(w.x.toolOrder, idx)
		}
		if id := asString(tc["id"]); id != "" {
			acc.id = id
		}
		fn, _ := tc["function"].(map[string]any)
		args := ""
		if fn != nil {
			if n := asString(fn["name"]); n != "" {
				acc.name = n
				acc.custom = w.meta.isCustom(n)
			}
			args = asString(fn["arguments"])
		}
		if args != "" {
			acc.args.WriteString(args)
		}
		// ponytail: 无名调用不是合法 Responses item——先攒着，等名字到了再宣告，
		// 宣告时把已攒参数作为整段 delta 一次性发出（relaykit 同款）。
		if !acc.opened {
			if acc.name == "" {
				continue
			}
			if err := w.openTool(acc); err != nil {
				return err
			}
			if acc.args.Len() > 0 {
				if err := w.emit(w.toolDeltaEvent(acc), map[string]any{
					"item_id":      acc.id,
					"output_index": acc.outIdx,
					"delta":        acc.args.String(),
				}); err != nil {
					return err
				}
			}
			continue
		}
		if args != "" {
			if err := w.emit(w.toolDeltaEvent(acc), map[string]any{
				"item_id":      acc.id,
				"output_index": acc.outIdx,
				"delta":        args,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *responsesWriter) openTool(acc *toolAcc) error {
	if acc.opened {
		return nil
	}
	acc.outIdx = w.nextIdx()
	if acc.id == "" {
		acc.id = "call_" + asString(acc.idx)
	}
	acc.opened = true
	item := map[string]any{
		"id":      acc.id,
		"status":  "in_progress",
		"call_id": acc.id,
		"name":    acc.name,
	}
	if acc.custom {
		item["type"], item["input"] = "custom_tool_call", ""
	} else {
		item["type"], item["arguments"] = "function_call", ""
	}
	// namespace 必须在 added 就带上：客户端从 added 事件派发执行器，补到 done 已太晚。
	w.meta.fixupItem(item)
	return w.emit("response.output_item.added", map[string]any{
		"output_index": acc.outIdx,
		"item":         item,
	})
}

func (w *responsesWriter) closeOpenItems() error {
	if err := w.closeReasoning(); err != nil {
		return err
	}
	if err := w.closeMessage(); err != nil {
		return err
	}
	for _, idx := range w.x.toolOrder {
		acc := w.x.tools[idx]
		if acc == nil || acc.closed || acc.name == "" {
			// 无名调用不是合法 item，丢弃。但**不能静默**：上游给了工具调用、
			// 客户端一个都没收到，表现就是「模型说完一句就停」，而网关这层
			// 一条日志都没有——排障时完全看不见。
			//
			// 只有 acc.closed 的**不算**：收尾会被调用多次（finish_reason 与
			// [DONE] 各一次），已发出去的工具调用第二次扫描必然命中这里，把它报成
			// 「缺函数名」会让整条排障线跑偏（实测每条请求都打一行，全是误报）。
			if acc != nil && !acc.closed && acc.args.Len() > 0 {
				log.Printf("WARN: [responses] 上游给了工具调用但缺函数名（idx=%d id=%s args=%dB）→ 丢弃，客户端只会看到正文",
					idx, acc.id, acc.args.Len())
			}
			continue
		}
		if !acc.opened {
			if err := w.openTool(acc); err != nil {
				return err
			}
			if acc.args.Len() > 0 {
				if err := w.emit(w.toolDeltaEvent(acc), map[string]any{
					"item_id":      acc.id,
					"output_index": acc.outIdx,
					"delta":        acc.args.String(),
				}); err != nil {
					return err
				}
			}
		}
		var item map[string]any
		if acc.custom {
			input, ok := unwrapCustomInput(acc.args.String())
			status := w.itemStatus()
			// 残缺的 custom 调用不作为可执行调用发出：delta 已经流出去的收不回来，
			// 用 item 的 status=incomplete 表达 —— 客户端不会执行未完成的调用。
			if !ok || !(w.x.sawFinish || w.x.sawDone) {
				log.Printf("WARN: [responses] custom 调用残缺（载荷可信=%v 正常收尾=%v），按不可执行下发 name=%s",
					ok, w.x.sawFinish || w.x.sawDone, acc.name)
				if !ok {
					input = ""
				}
				status = "incomplete"
			}
			if err := w.emit("response.custom_tool_call_input.done", map[string]any{
				"item_id":      acc.id,
				"output_index": acc.outIdx,
				"input":        input,
			}); err != nil {
				return err
			}
			item = map[string]any{
				"id": acc.id, "type": "custom_tool_call", "status": status,
				"call_id": acc.id, "name": acc.name, "input": input,
			}
		} else {
			if err := w.emit("response.function_call_arguments.done", map[string]any{
				"item_id":      acc.id,
				"output_index": acc.outIdx,
				"arguments":    acc.args.String(),
			}); err != nil {
				return err
			}
			item = map[string]any{
				"id":        acc.id,
				"type":      "function_call",
				"status":    w.itemStatus(),
				"call_id":   acc.id,
				"name":      acc.name,
				"arguments": acc.args.String(),
			}
		}
		w.meta.fixupItem(item)
		if err := w.emit("response.output_item.done", map[string]any{
			"output_index": acc.outIdx,
			"item":         item,
		}); err != nil {
			return err
		}
		w.x.output = append(w.x.output, item)
		acc.closed = true
	}
	return nil
}

func (w *responsesWriter) closeReasoning() error {
	if !w.x.reasoningOpen {
		return nil
	}
	text := w.x.reasoningText.String()
	if err := w.emit("response.reasoning_summary_text.done", map[string]any{
		"item_id":       w.x.reasoningID,
		"output_index":  w.x.reasoningIdx,
		"summary_index": 0,
		"text":          text,
	}); err != nil {
		return err
	}
	if err := w.emit("response.reasoning_summary_part.done", map[string]any{
		"item_id":       w.x.reasoningID,
		"output_index":  w.x.reasoningIdx,
		"summary_index": 0,
		"part":          map[string]any{"type": "summary_text", "text": text},
	}); err != nil {
		return err
	}
	// ponytail: Codex 消费 summary 事件族；聊天上游的 reasoning_content 无
	// 「摘要 vs 原文」之分，统一作为 summary 下发（relaykit 同款）。
	item := map[string]any{
		"id":      w.x.reasoningID,
		"type":    "reasoning",
		"status":  w.itemStatus(),
		"summary": []any{map[string]any{"type": "summary_text", "text": text}},
	}
	if err := w.emit("response.output_item.done", map[string]any{
		"output_index": w.x.reasoningIdx,
		"item":         item,
	}); err != nil {
		return err
	}
	w.x.output = append(w.x.output, item)
	w.x.reasoningOpen = false
	return nil
}

func (w *responsesWriter) closeMessage() error {
	if !w.x.messageOpen {
		return nil
	}
	if w.x.textOpen {
		if err := w.emit("response.output_text.done", map[string]any{
			"item_id":       w.x.msgID,
			"output_index":  w.x.messageIdx,
			"content_index": 0,
			"text":          w.x.messageText.String(),
		}); err != nil {
			return err
		}
		if err := w.emit("response.content_part.done", map[string]any{
			"item_id":       w.x.msgID,
			"output_index":  w.x.messageIdx,
			"content_index": 0,
		}); err != nil {
			return err
		}
		w.x.textOpen = false
	}
	item := map[string]any{
		"id":     w.x.msgID,
		"type":   "message",
		"status": w.itemStatus(),
		"role":   "assistant",
		"content": []any{
			map[string]any{"type": "output_text", "text": w.x.messageText.String()},
		},
	}
	if err := w.emit("response.output_item.done", map[string]any{
		"output_index": w.x.messageIdx,
		"item":         item,
	}); err != nil {
		return err
	}
	w.x.output = append(w.x.output, item)
	w.x.messageOpen = false
	return nil
}

// emitPreItems 补发网关侧已执行完的托管工具产出（image_generation_call）：图是在对话
// 开始前就出好的，added 用 in_progress 占位、done 带成品，与 OpenAI 的事件形状一致。
func (w *responsesWriter) emitPreItems() error {
	if w.meta == nil {
		return nil
	}
	for _, it := range w.meta.preItems {
		item, _ := it.(map[string]any)
		if item == nil {
			continue
		}
		idx := w.nextIdx()
		head := map[string]any{"id": item["id"], "type": item["type"], "status": "in_progress"}
		if err := w.emit("response.output_item.added", map[string]any{"output_index": idx, "item": head}); err != nil {
			return err
		}
		if err := w.emit("response.output_item.done", map[string]any{"output_index": idx, "item": item}); err != nil {
			return err
		}
		w.x.output = append(w.x.output, item)
	}
	return nil
}

func (w *responsesWriter) emitCompleted() error {
	if w.x.completed || w.x.failed {
		return nil
	}
	w.x.completed = true
	typ, status := "response.completed", "completed"
	if w.x.status == "incomplete" {
		typ, status = "response.incomplete", "incomplete"
	}
	// output 不能是 nil：Go 的 nil 切片会序列化成 null，而 Responses 客户端
	// （CodeBuddy/Codex）拿到 completed 后会对 output 做 .map —— null 直接崩
	// （实测 "Cannot read properties of null (reading 'map')"）。空输出是合法
	// 响应（模型只思考不输出、或全部增量被上游吞掉），必须落成 []。
	output := w.x.output
	if output == nil {
		output = []any{}
	}
	resp := map[string]any{
		"id":         respID(w.x.id),
		"object":     "response",
		"status":     status,
		"created_at": w.x.createdAt,
		"model":      w.x.model,
		"output":     output,
	}
	if w.x.incompleteDetails != nil {
		resp["incomplete_details"] = w.x.incompleteDetails
	}
	if w.x.usage != nil {
		resp["usage"] = convertUsage(w.x.usage)
	}
	return w.emit(typ, map[string]any{"response": resp})
}

func (w *responsesWriter) emitFailed(errObj any) error {
	if w.x.failed {
		return nil
	}
	w.x.failed = true
	if errObj == nil {
		errObj = map[string]any{"message": "upstream error", "type": "api_error"}
	}
	return w.emit("response.failed", map[string]any{
		"response": map[string]any{
			"id":     respID(w.x.id),
			"object": "response",
			"status": "failed",
			"error":  errObj,
		},
	})
}

// emitRaw 原样写一行 SSE（注释帧等无语义载荷）：不进事件序号、不改写内容，
// 只负责确保响应头已提交并 Flush。
func (w *responsesWriter) emitRaw(s string) error {
	if !w.hdrSent {
		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		w.hdrSent = true
		w.status = http.StatusOK
	}
	if _, err := io.WriteString(w.ResponseWriter, s); err != nil {
		return err
	}
	w.Flush()
	return nil
}

func (w *responsesWriter) emit(typ string, payload map[string]any) error {
	if payload == nil {
		payload = map[string]any{}
	}
	payload["type"] = typ
	// ponytail: 当前 Responses wire 契约要求每个事件带递增 sequence_number（Codex 会读）。
	payload["sequence_number"] = w.x.seq
	w.x.seq++
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if !w.hdrSent {
		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		// ponytail: 不显式 WriteHeader——下面的 io.WriteString 会隐式提交 200。
		// 流式路径的 Flush（StreamHint 逐帧 flush）可能已先提交，再显式调用就是
		// net/http 的 "superfluous response.WriteHeader call" 噪音。
		w.hdrSent = true
		w.status = http.StatusOK
	}
	if _, err := io.WriteString(w.ResponseWriter, "event: "+typ+"\ndata: "+string(raw)+"\n\n"); err != nil {
		return err
	}
	w.Flush()
	return nil
}
