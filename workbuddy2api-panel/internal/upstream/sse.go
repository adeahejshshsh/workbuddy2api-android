// sse.go 处理上游 SSE 流：聚合成单个 OpenAI 响应，或透传给客户端。
package upstream

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

// errEmptyStream 上游返回 200 但没有有效 SSE 数据帧（空流/只有注释/[DONE]）。
// 用哨兵错误替代裸 fmt.Errorf：StreamHint 的调用方（handler 流式路径）需要区分
// 「上游空流」与「客户端断连写失败」——空流是上游缺陷，应记 502 观测；写失败是
// 客户端已走，日志口径不同。Aggregate 与 StreamHint 共用同一哨兵（errors.Is 判定）。
var errEmptyStream = errors.New("upstream stream contained no valid data events")

// IsEmptyStreamError 报告错误是否为「上游空流」（无有效 SSE 帧）——供 handler
// 在流式路径把空流记为失败观测（HTTP 头已发出只能 200，但日志/状态应收敛到
// upstream_parse 同语义），与客户端断连类错误区分。
func IsEmptyStreamError(err error) bool { return errors.Is(err, errEmptyStream) }

// ErrorFrame 上游以 error 帧报错（HTTP 200 已开流，但流里是一帧 {"error":...}）。
// 非流式 Aggregate 遇到它不再合成空成功响应，而是把它交回调用方——调用方用
// FrameKind(Payload) 分类后走与 HTTP 错误完全相同的账号处置与末端文案。
type ErrorFrame struct {
	// Payload error 帧的 data 原文（JSON），保留完整以便分类与透传。
	Payload string
}

func (e *ErrorFrame) Error() string {
	p := e.Payload
	if r := []rune(p); len(r) > 200 {
		p = string(r[:200]) + "…"
	}
	return "upstream error frame: " + p
}

// IsErrorFrame 报告错误是否为上游 error 帧（供调用方区分「流里报错」与「流解析失败」）。
func IsErrorFrame(err error) bool {
	var ef *ErrorFrame
	return errors.As(err, &ef)
}

// errClientWrite 标记「写客户端失败」：SSE 透传时 w.Write 出错只可能是客户端
// 断连（上游帧本身是好的）。handler 必须把它与「读上游失败」分开——前者账号健康、
// 记 Interrupted；后者是上游故障，账号要被喂失败计数。
var errClientWrite = errors.New("client write failed")

// errUpstreamRead 标记「读上游失败」：流读到一半 RST/连接被掐（unexpected EOF、
// connection reset）。客户端已收到 200 和若干帧，只能补一帧 error + [DONE] 收尾，
// 但错误必须能追溯到上游，不能让调用方当成「客户端走了」。
var errUpstreamRead = errors.New("upstream stream read failed")

// IsClientWriteError 报告错误是否为写客户端失败（客户端断连）。
func IsClientWriteError(err error) bool { return errors.Is(err, errClientWrite) }

// IsUpstreamReadError 报告错误是否为读上游失败（上游中途断流）。
func IsUpstreamReadError(err error) bool { return errors.Is(err, errUpstreamRead) }

// clientWriteErr 包装写客户端的错误（保留原错误文本，供日志）。
func clientWriteErr(err error) error { return fmt.Errorf("%w: %v", errClientWrite, err) }

// Aggregate 读取完整 SSE 流，聚合 delta.content 为单个 OpenAI chat.completion 响应。
// 分片/半行由 bufio.Reader.ReadString 处理；遇到 "data: [DONE]" 结束。
// tool_calls 以流式 delta 到达（按 index 合并：首片带 id/type/name，后续只带 arguments 片段）。
func Aggregate(r io.Reader) (map[string]any, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var (
		id, model     string
		created       float64
		content       strings.Builder
		reasoning     strings.Builder
		role          = "assistant"
		finishReason  = "stop"
		usage         map[string]any
		gotAnyContent bool
		validEvents   int
		sawDone       bool // 上游显式发过 data: [DONE]（正常收尾）
		sawFinish     bool // 上游显式给过非空 finish_reason（它认为这一轮结束了）
		toolCalls     = map[int]map[string]any{}
		toolOrder     []int
		// toolSeq 缺 index 的 tool_call 的分配序号源：跨帧延续「最近分配」槽位，
		// 同帧内递增（见 mergeToolCallsChunk 注释）。
		toolSeq int
		// idIndex id → 已分配的 index：跨帧持续（延续碎片的 id 常出现在后续帧），
		// 供缺 index 时按 id 归位既有调用（见 mergeToolCallsChunk 注释）。
		idIndex = map[string]int{}
	)
	// appendContent 是「已取到正文」（gotAnyContent latch）的唯一写入点：delta 与
	// message 两路 content 都必须经此并入，规约只有一份（issue #142）——
	//   S1 空串不算「已取到正文」、不占 latch 名额：OpenAI 风格 role-only 首帧
	//      （delta.content=""）和整条空 message 帧是常态帧，若空串置位 latch，
	//      后续真正文会被 message 回退分支的 !gotAnyContent 守卫静默拒绝；
	//   S2 空串本身也无可追加字节，跳过 WriteString 与追加语义自洽。
	appendContent := func(txt string) {
		if txt == "" {
			return
		}
		content.WriteString(txt)
		gotAnyContent = true
	}
	// mergeToolCallsChunk 把一段 tool_calls 数组按 index 合并进累计表。
	// delta（流式分片，按 index 累积）与 message（非 delta 整条）共用同一合并逻辑，
	// 保证「上游给的身份/函数名不丢、arguments 拼接语义一致」。
	//
	// index 缺失兼容：OpenAI 规范要求 delta 帧的 tool_call 带 index（标记分片归属），
	// 但部分上游省略它。此前缺 index 一律归 0——多调用场景下不同 call 被合并进同一
	// index 槽，arguments 串联、name 互相覆盖（数据污染）。修法按「id 优先、lastIdx 兜底」：
	//   - 带 index → 按 index 累积（合规形态，零改动）；
	//   - 缺 index 带 id 且 id 已见过 → 延续该 id 所在 index；
	//   - 缺 index 带 id 且 id 是新的 → 开新序号（多调用不合并）；
	//   - 缺 index 无 id → 追加到最近收到碎片的 index（单个调用的延续分片无 id
	//     是标准形态），无既往则开新号。
	// 带 index 的碎片照常按 index 累积，不受影响。
	// nextToolIndex 分配下一个不冲突的缺 index 序号：从 toolSeq 起递增跳过既有
	// index（合规流的 index 是 0..N-1，缺 index 的补位不能覆盖它们）。
	nextToolIndex := func() int {
		for {
			idx := toolSeq
			toolSeq++
			if _, used := toolCalls[idx]; !used {
				return idx
			}
		}
	}
	mergeToolCallsChunk := func(tcs []any) {
		for _, tc := range tcs {
			call, ok := tc.(map[string]any)
			if !ok {
				continue
			}
			idx := -1
			if v, ok := call["index"].(float64); ok {
				idx = int(v)
			} else if cid, _ := call["id"].(string); cid != "" {
				if mid, seen := idIndex[cid]; seen {
					idx = mid // 该 id 已归位过：延续既有调用（跨帧有效）
				} else {
					idx = nextToolIndex()
				}
			} else if len(toolOrder) > 0 {
				idx = toolOrder[len(toolOrder)-1] // 无 id 碎片：延续最近调用
			} else {
				idx = nextToolIndex()
			}
			merged, seen := toolCalls[idx]
			if !seen {
				merged = map[string]any{"index": idx}
				toolCalls[idx] = merged
				toolOrder = append(toolOrder, idx)
			}
			if cid, _ := call["id"].(string); cid != "" {
				idIndex[cid] = idx
			}
			if cid, _ := merged["id"].(string); cid != "" {
				idIndex[cid] = idx
			}
			mergeToolCallDelta(merged, call)
		}
	}
	// mergeMessageFields 把非 delta 的完整 message 内容并入聚合（message 是整条下发，
	// 非流式拼接，content 只取一次）。role/reasoning_content/tool_calls 与 delta 分支
	// 同构透出；content 同样置 gotAnyContent，与 delta 路径的 latch 语义一致
	// （一帧整条 message 之后，后续 delta 帧不重复追加）。
	mergeMessageFields := func(msg map[string]any) {
		if r2, ok := msg["role"].(string); ok && r2 != "" {
			role = r2
		}
		if txt, ok := msg["content"].(string); ok {
			appendContent(txt)
		}
		if rc, ok := msg["reasoning_content"].(string); ok {
			reasoning.WriteString(rc)
		}
		if tcs, ok := msg["tool_calls"].([]any); ok {
			mergeToolCallsChunk(tcs)
		}
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			// 包装成 errUpstreamRead（bug #3）：非流式路径靠 IsUpstreamReadError 判
			// 「上游中途断流」回 502 stream_error，与流式路径同码；此前 Aggregate
			// 原样返回裸错误 → 该分支永远不可达，错误码与流式不一致。
			return nil, fmt.Errorf("%w: %v", errUpstreamRead, err)
		}
		line = strings.TrimRight(line, "\r\n")
		// SSE 规范里 "data:" 后的空格是可选的（"data:{...}" 同样合法）。只认
		// "data: " 会让这种上游变成「零有效事件」→ 误报 502 upstream_parse。
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
			if payload == "[DONE]" {
				// 上游显式结束：停止读取，DONE 之后的任何数据一律忽略。
				sawDone = true
				break
			} else {
				var chunk map[string]any
				if json.Unmarshal([]byte(payload), &chunk) == nil {
					// 上游「200 + error 帧」是真实形态（6004 限流、内容审核、11158 等级
					// 拒绝）。流式路径一直专门处理它（StreamHint 的 error-passthrough），
					// 非流式此前把它当普通事件吃掉：error 帧没有 choices → 不并入内容，
					// 但 validEvents 已自增、不触发 errEmptyStream → 合成
					// content="" + finish_reason:"stop" 的假 200 成功，调用方还会
					// NoteSuccess 把正在限流的号洗成健康号、粘性把会话钉死在它身上。
					// 这里一律升级成 ErrorFrame 交回调用方走既有的错误策略。
					// 必须「键存在且值非 nil」（bug B3）：OpenAI 兼容实现常在正常
					// chunk 里带 "error":null，只判键存在会把它当错误帧 → FrameKind
					// 判 ErrNone → handler default 只换号 → 每个号失败一次 → 503。
					if e, hasErr := chunk["error"]; hasErr && e != nil {
						return nil, &ErrorFrame{Payload: payload}
					}
					// 有效事件计数：仅 JSON 解析成功的数据帧计入（解析失败沿用静默 continue）。
					validEvents++
					if v, ok := chunk["id"].(string); ok && id == "" {
						id = v
					}
					if v, ok := chunk["model"].(string); ok && model == "" {
						model = v
					}
					if v, ok := chunk["created"].(float64); ok && created == 0 {
						created = v
					}
					if u, ok := chunk["usage"].(map[string]any); ok {
						usage = u
					}
					if ch, ok := chunk["choices"].([]any); ok {
						for _, ci := range ch {
							c, _ := ci.(map[string]any)
							if c == nil {
								continue
							}
							if fr, ok := c["finish_reason"].(string); ok && fr != "" {
								finishReason = fr
								sawFinish = true
							}
							if delta, ok := c["delta"].(map[string]any); ok {
								if r2, ok := delta["role"].(string); ok && r2 != "" {
									role = r2
								}
								if txt, ok := delta["content"].(string); ok {
									appendContent(txt)
								}
								if rc, ok := delta["reasoning_content"].(string); ok {
									reasoning.WriteString(rc)
								}
								if tcs, ok := delta["tool_calls"].([]any); ok {
									mergeToolCallsChunk(tcs)
								}
							}
							// 有的上游把完整消息放在 message 里（非 delta）：
							// 整条并入（role/content/reasoning_content/tool_calls），
							// 与 delta 分支同构。delta 已取过正文（gotAnyContent）则跳过
							// （避免与 delta 路径重复拼接——PR #134 的 latch 语义）。
							if msg, ok := c["message"].(map[string]any); ok && !gotAnyContent {
								mergeMessageFields(msg)
							}
						}
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
	}
	if validEvents == 0 {
		// 上游返回 200 但没有任何有效数据事件（空流/只有 [DONE]/只有注释行）：
		// 不再合成空 content 的假成功响应，直接报错，由 handler 映射为 502 upstream_parse。
		return nil, errEmptyStream
	}
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = float64(time.Now().Unix())
	}
	message := map[string]any{
		"role":    role,
		"content": content.String(),
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolOrder) > 0 {
		sort.Ints(toolOrder)
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			calls = append(calls, toolCalls[idx])
		}
		// P1b：流被截断时 tool_call 的 arguments 是残缺 JSON（解析失败），不把脏参数
		// 交给客户端——残留分片会被客户端解析成非法 JSON 卡死会话。截断的两个来源：
		//   - finish_reason=="length"（模型因 max_tokens 提前中止）；
		//   - 上游连接中断（EOF 收尾但未发 data: [DONE]，sawDone=false）。
		// 完整参数原样保留（正例零改动）；空参数（无参工具）不是截断，同样保留。
		// 此前只认 finish_reason=="length"，EOF 截断的 tool_calls 残缺参数被原样下发。
		if finishReason == "length" || !sawDone {
			var dropped int
			calls, dropped = dropTruncatedToolCalls(calls)
			if dropped > 0 {
				// 被丢弃的调用必须有痕迹（issue B14）：上游 EOF 未发 [DONE] 是常态，
				// 静默消失会让"工具没被调用"变成一个无从排查的悬案。
				log.Printf("WARN: [upstream] non-stream id=%s model=%s: dropped %d truncated tool_call(s) (finish_reason=%q saw_done=%v)",
					id, model, dropped, finishReason, sawDone)
			}
		}
		if len(calls) > 0 {
			message["tool_calls"] = calls
		}
	}
	// 上游既没发 [DONE]、也没给过任何非空 finish_reason：这才是真截断（连接被掐 /
	// 上游 EOF），必须把 finish_reason 从初始值 "stop" 改掉，否则客户端会认定
	// 「模型说完了」，把半截答案当完整结果落库（bug 5）。
	// 判据刻意收紧到 sawFinish==false：WorkBuddy 上游「完整结束但不发 [DONE]」是
	// 常态（工具调用分支早有同款注释），只看 !sawDone 会把大量正常回复误标成截断。
	if !sawDone && !sawFinish && finishReason == "stop" {
		finishReason = "length"
		log.Printf("WARN: [upstream] non-stream id=%s model=%s: stream ended without [DONE] or finish_reason, marking finish_reason=length",
			id, model)
	}
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": int64(created),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
	}
	if usage != nil {
		// OpenAI 非流式 usage 必含 total_tokens。上游若只发 prompt_tokens +
		// completion_tokens（部分上游末帧缺 total），网关合成补齐——否则严格按
		// schema 校验的客户端收不到 total_tokens。已有 total 或二者缺一不补
		// （不臆造：单边有值无法合成可信的 total）。
		resp["usage"] = normalizeUsageCacheAliases(ensureUsageTotal(usage))
	}
	return resp, nil
}

// ensureUsageTotal 在 usage 缺 total_tokens 但 prompt_tokens/completion_tokens 都在时
// 补齐 total = prompt + completion（通过新 map 合并，不修改原上游 map）。
// 任一缺失或已有 total 时原样返回。
func ensureUsageTotal(u map[string]any) map[string]any {
	if _, ok := u["total_tokens"]; ok {
		return u
	}
	pt, pok := num64(u["prompt_tokens"])
	ct, cok := num64(u["completion_tokens"])
	if !pok || !cok {
		return u
	}
	out := make(map[string]any, len(u)+1)
	for k, v := range u {
		out[k] = v
	}
	out["total_tokens"] = pt + ct
	return out
}

// num64 把 JSON number（float64/int64 均可）归一为 float64；非数字返回 ok=false。
//
// 注意与 payload.go 的类型 switch 语义不同：那边是翻译请求别名字段（非数字拒绝
// 整个请求），这边是聚合响应求和（非数字只跳过合成）。防后人合并两处。
func num64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	default:
		return 0, false
	}
}

// mergeToolCallDelta 把流式 tool_call 片段合并到累计对象：
// id/type/function.name 直覆盖（后续分片通常缺省），function.arguments 拼接。
func mergeToolCallDelta(merged, delta map[string]any) {
	if v, ok := delta["id"].(string); ok && v != "" {
		merged["id"] = v
	}
	if v, ok := delta["type"].(string); ok && v != "" {
		merged["type"] = v
	}
	df, _ := delta["function"].(map[string]any)
	if df == nil {
		return
	}
	mf, _ := merged["function"].(map[string]any)
	if mf == nil {
		mf = map[string]any{}
		merged["function"] = mf
	}
	if v, ok := df["name"].(string); ok && v != "" {
		mf["name"] = v
	}
	if v, ok := df["arguments"].(string); ok && v != "" {
		if prev, _ := mf["arguments"].(string); prev != "" {
			mf["arguments"] = prev + v
		} else {
			mf["arguments"] = v
		}
	}
}

// stripToolCallNames 收敛流式 tool_calls 的 name 语义为「每个 index 只出现一次」：
// 首片保留 function.name，同一 index 后续分片里的 name 键一律删除（无论上游是
// 空串还是重复非空串）。这是 OpenAI 官方流的真实形态——首帧带 name，后续帧只带
// arguments 片段、不再出现 name 键——因此是累加型与覆盖型客户端的共同祖先行为。
//
// 两类消费模型在该形态下同时正确：
//   - 累加型（官方 WorkBuddy/CodeBuddy `name += tc_function?.name || ""`）：
//     后续分片 name 键缺失 → 追加空串，累积 name 保持唯一，不再拼成 Bash×帧数（issue #82）。
//   - 覆盖型（hawklithm#2 / Grok Build `name ?? state.name` 或 `if (name) state.name = name`）：
//     后续分片 name 键缺失 → 保留已建好的首帧 name，不被空串意外清空。
//     键缺失是比空串更安全的形态：`??` 与 truthy 守卫对缺失键必然保留旧值，
//     而对空串，`??` 会误判为重设并清空工具名。
//
// seen 记录每个 index 是否已发过首片（与 name 是否非空无关）；删除是幂等的。
// 只动 function.name 键，id/type/arguments 原样透传。
func stripToolCallNames(obj map[string]any, seen map[int]bool) {
	choices, _ := obj["choices"].([]any)
	for _, ci := range choices {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		delta, _ := c["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		tcs, _ := delta["tool_calls"].([]any)
		for _, tci := range tcs {
			tc, _ := tci.(map[string]any)
			if tc == nil {
				continue
			}
			// 缺 index 的分片不能归到 0 号槽（bug B5）：上游常省略 index，
			// 一旦落 0 号槽，第一个调用把 seen[0] 置真后，后面每个调用首片的
			// function.name 都被当成「同 index 的后续分片」删掉 —— 客户端拿到
			// 无名工具调用，直接失败（或按 index=0 合并导致参数串味）。
			// 宁可不收敛，也不能删名字。
			idx := -1
			if v, ok := tc["index"].(float64); ok {
				idx = int(v)
			}
			if idx < 0 {
				continue // 无 index：原样透传
			}
			if seen[idx] {
				// 已发过首片：删除本分片的 name 键（存在即删，幂等）。
				if fn, _ := tc["function"].(map[string]any); fn != nil {
					delete(fn, "name")
				}
				continue
			}
			// 首现：保留 name 键原样（上游首片通常带非空 name；空 name 也照发，
			// 与 OpenAI 对「首帧无 name」的容忍一致）。
			//
			// 但「空 name」不能占住首片名额：上游先发 function.name:""、真名在
			// 第二片才到的形态是实测存在的，占位会让第二片的真名被 delete 抹掉，
			// 客户端最终拿到空工具名。只有真发过非空 name 才算「首片已出」。
			if fn, _ := tc["function"].(map[string]any); fn != nil {
				if nm, _ := fn["name"].(string); nm != "" {
					seen[idx] = true
				}
			}
		}
	}
}

// maxToolIdxIn 返回 obj 里出现过的最大 tool_call index（没有则返回 cur）。
// 供 repairFrame 在分配合成 index 之前把「同一帧里的真实调用」算进去。
func maxToolIdxIn(obj map[string]any, cur int) int {
	chs, _ := obj["choices"].([]any)
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
		for _, tci := range tcs {
			tc, _ := tci.(map[string]any)
			if tc == nil {
				continue
			}
			if v, ok := tc["index"].(float64); ok && int(v) > cur {
				cur = int(v)
			}
		}
	}
	return cur
}

// normalizeFrame 以 OpenAI 流式规范白名单重建帧：仅保留标准字段，
// 剔除上游噪声（finish_reason:"" → null、空 content/refusal、空 tool_calls 列表、
// 空占位 function_call、顶层未知字段），空 delta 键一律省略，
// usage 缺失 → null，保证任意标准客户端按规范解析。
func normalizeFrame(obj map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"id", "object", "created", "model", "system_fingerprint", "service_tier"} {
		if v, ok := obj[k]; ok && v != nil {
			out[k] = v
		}
	}
	if _, ok := out["object"]; !ok {
		out["object"] = "chat.completion.chunk"
	}
	if _, ok := out["id"]; !ok {
		out["id"] = "chatcmpl-wb2api"
	}
	if chs, ok := obj["choices"].([]any); ok {
		nchs := make([]any, 0, len(chs))
		for _, ci := range chs {
			c, ok := ci.(map[string]any)
			if !ok {
				continue
			}
			nc := map[string]any{}
			if idx, ok := c["index"]; ok {
				nc["index"] = idx
			}
			delta := map[string]any{}
			if d, ok := c["delta"].(map[string]any); ok {
				if v, ok := d["role"].(string); ok && v != "" {
					delta["role"] = v
				}
				if v, ok := d["content"].(string); ok && v != "" {
					delta["content"] = v
				}
				if v, ok := d["reasoning_content"].(string); ok && v != "" {
					delta["reasoning_content"] = v
				}
				if v, ok := d["refusal"].(string); ok && v != "" {
					delta["refusal"] = v
				}
				if tcs, ok := d["tool_calls"].([]any); ok && len(tcs) > 0 {
					delta["tool_calls"] = tcs
				}
				if fc, ok := d["function_call"]; ok && fc != nil {
					// 空占位 function_call（name/arguments 全空）视为噪声剔除
					keep := false
					if fcm, ok2 := fc.(map[string]any); ok2 {
						n, _ := fcm["name"].(string)
						a, _ := fcm["arguments"].(string)
						keep = n != "" || a != ""
					} else {
						keep = true
					}
					if keep {
						delta["function_call"] = fc
					}
				}
			}
			nc["delta"] = delta
			if fr, ok := c["finish_reason"].(string); ok && fr != "" {
				nc["finish_reason"] = fr
			} else {
				nc["finish_reason"] = nil
			}
			nchs = append(nchs, nc)
		}
		out["choices"] = nchs
	}
	if rawUsage, ok := obj["usage"]; ok {
		if u, ok := rawUsage.(map[string]any); ok {
			out["usage"] = normalizeUsageCacheAliases(u)
		} else {
			out["usage"] = rawUsage
		}
	} else {
		out["usage"] = nil
	}
	return out
}

// Stream 透传上游 SSE 到 w（逐帧规范化后 flush），保证至少写一个 [DONE]。
// 调用方必须先设置过 status 200；本函数自设 SSE headers。
// 流式策略：逐帧透传（规范化已剥空 content 噪声），恢复与上游一致的平滑流式。
//
// StreamHint 变体（gateway_hint 任务）：上游 error 帧透传时附加
// error.gateway_hint 字段——message 原文不动，hint 并列补充；hintFn 返回空串
// 或 nil 时与 Stream 行为逐字节一致。
func Stream(w http.ResponseWriter, r io.Reader) error {
	return StreamHint(w, r, nil)
}

// StreamOption StreamHint 的可选行为开关。
// WithErrorFrameObserver 只做旁路观测、不改透传字节；WithMarkupRepair 会改写出站
// 帧（把正文里的原生工具调用标记还原成 delta.tool_calls，见 dsml.go）。
type StreamOption func(*streamOptions)

type streamOptions struct {
	onErrorFrame func(payload string)
	markup       *MarkupRepair
}

// WithMarkupRepair 注册「模型原生工具调用标记」修复器（见 dsml.go）。
//
// 上游在没有 tools 的请求里会把工具调用吐成标记文本，Codex 这类只执行结构化
// function_call 的客户端于是把它当普通正文落进会话历史，回合直接结束、工具一次
// 都没跑。挂上修复器后，这类标记在透传前被还原成 delta.tool_calls。
//
// 传 nil、或传入未启用的修复器（客户端没声明过 tools）→ 与 Stream 行为逐字节一致。
func WithMarkupRepair(r *MarkupRepair) StreamOption {
	return func(o *streamOptions) { o.markup = r }
}

// WithErrorFrameObserver 注册"上游以 error 帧报错"的观察者：透传该帧之前先用
// payload（原始 data 内容）回调一次。给调用方一个**账号处置**的挂载点——
// 上游「200 + error 帧」是真实形态（6004 限流、内容拦截、审核），此前网关在读
// 第一帧之前就把账号记成功，限流号被当成健康号；有观察者后可在流尾按帧分类处置。
func WithErrorFrameObserver(fn func(payload string)) StreamOption {
	return func(o *streamOptions) { o.onErrorFrame = fn }
}

// StreamHint 同 Stream，但上游 error 帧透出前把 hintFn(payload) 的返回值写入
// error.gateway_hint。hintFn 为 nil 或返回空串 → 原样透传（零改写）。
// 空流兜底 error 帧（"empty upstream stream"）不带 hint（网关本地故障形态
// 未覆盖，不编造）。
// opts：旁路观测开关（见 WithErrorFrameObserver），不改变任何透传字节。
func StreamHint(w http.ResponseWriter, r io.Reader, hintFn func(string) string, opts ...StreamOption) error {
	var o streamOptions
	for _, f := range opts {
		if f != nil {
			f(&o)
		}
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)

	// toolCallSeen 跨帧记录 delta.tool_calls 里已发过首片的 index，
	// 供逐 chunk 透传时收敛 name 为「每 index 一次」（对齐 OpenAI 官方流）。
	toolCallSeen := map[int]bool{}

	// firstID 透传流的消息级 id 基准：缓存首个非空上游 id，后续帧缺失/空串时复用
	// （issue #35：同一条 SSE 消息所有帧共用一个真实 id，后台按 id 归并；此前中间帧
	// 一律补 chatcmpl-wb2api 哨兵，造成同流 id 分裂）。全流无真实 id → 才出现哨兵。
	firstID := ""

	// 标记修复（见 dsml.go）：把上游正文里的原生工具调用标记还原成 delta.tool_calls。
	// 客户端没声明过 tools 时 repair.Enabled() 为 false → 整条流零改写零分配。
	//   - synthIdx：本流已还原的调用数（合成的 index 从上游最大 index 之后接续）；
	//   - maxToolIdx：上游已用过的最大 tool_call index，避免合成的 index 与真实调用撞车；
	//   - toolFinishSent：是否已发出过 finish_reason: tool_calls，供流尾兜底。
	repair := o.markup
	synthIdx := 0
	maxToolIdx := -1
	toolFinishSent := false

	// writeSSE 原样写出一帧并 flush。
	// 写失败一律包成 errClientWrite：上游帧本身是好的，写不出去只可能是客户端
	// 断连。handler 据此区分「人走了」与「上游坏了」——两者对账号的处置相反。
	writeSSE := func(payload string) error {
		if _, werr := io.WriteString(w, "data: "+payload+"\n\n"); werr != nil {
			return clientWriteErr(werr)
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}

	// writeRaw 原样写出一帧（绕过 normalizeFrame）。上游 error 帧（error-passthrough）
	// 与空流错误帧需保留 error 字段，不能被白名单剥掉，故经此写出。
	// gateway_hint：上游 error 帧透出前按 hintFn 附加 error.gateway_hint 字段
	// （error 对象上加一个键，message/code/requestId 等原文不动；hintFn 为
	// nil / 空串 / 非 JSON 帧 → 原样写出，零改写）。
	writeRaw := func(payload string) error {
		if hint := frameGatewayHint(hintFn, payload); hint != "" {
			payload = attachHintToErrorFrame(payload, hint)
		}
		return writeSSE(payload)
	}

	// writeChunk 把一个 chunk 对象按规范白名单重建后写出并 flush，同时维护三件跨帧
	// 状态：tool_calls name 收敛（每 index 仅首片带 name）、消息 id 续传、以及上游
	// 已用过的最大 tool_call index（供标记修复接续编号）。
	// fallback 是规范化失败时的兜底原文（与修复前「marshal 失败即原样透传」一致）。
	writeChunk := func(obj map[string]any, fallback string) error {
		// 先按 index 收敛 tool_calls name（每 index 仅首片保留，后续分片删 name 键），再规范化透传。
		stripToolCallNames(obj, toolCallSeen)
		// id 续传：首帧非空真实 id 缓存；后续帧缺 id / 空 id 一律用缓存值，
		// 有自己 id 的帧保持原样（不同流分裂的帧允许各自 id）。
		if firstID == "" {
			if v, ok := obj["id"].(string); ok && v != "" {
				firstID = v
			}
		} else if v, ok := obj["id"].(string); !ok || v == "" {
			obj["id"] = firstID
		}
		if chs, ok := obj["choices"].([]any); ok {
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
				for _, tci := range tcs {
					tc, _ := tci.(map[string]any)
					if tc == nil {
						continue
					}
					if v, ok := tc["index"].(float64); ok && int(v) > maxToolIdx {
						maxToolIdx = int(v)
					}
				}
			}
		}
		raw, err := json.Marshal(normalizeFrame(obj))
		if err != nil {
			return writeSSE(fallback)
		}
		return writeSSE(string(raw))
	}

	// repairFrame 在透传前跑标记修复，并把一帧展开成「可能多帧」：正文帧（标记被摘掉
	// 后的可见文本）+ N 个 delta.tool_calls 帧 + 收尾 finish_reason 帧。没有命中标记
	// 时原样返回入参帧（单元素），与修复前逐字节一致。
	repairFrame := func(obj map[string]any) []map[string]any {
		if !repair.Enabled() {
			return []map[string]any{obj}
		}
		// 先把本帧自带的真实 tool_call index 并入 maxToolIdx。maxToolIdx 原本只在
		// writeChunk 里更新，而 repairFrame 在 writeChunk **之前**跑：同一帧既有真实
		// delta.tool_calls[index=0] 又有正文标记时（首帧 maxToolIdx=-1），合成调用也
		// 拿到 index 0，被 stripToolCallNames 当成「同 index 的后续分片」删掉 name。
		maxToolIdx = maxToolIdxIn(obj, maxToolIdx)
		chs, ok := obj["choices"].([]any)
		if !ok {
			return []map[string]any{obj}
		}
		var calls []MarkupCall
		for _, ci := range chs {
			c, ok := ci.(map[string]any)
			if !ok {
				continue
			}
			d, ok := c["delta"].(map[string]any)
			if !ok {
				continue
			}
			txt, ok := d["content"].(string)
			if !ok || txt == "" {
				continue
			}
			emit, got := repair.Feed(txt)
			calls = append(calls, got...)
			if emit == "" {
				delete(d, "content")
			} else {
				d["content"] = emit
			}
		}
		if len(calls) == 0 {
			// 本帧没命中标记；但本流此前还原过调用 → 把上游的 stop 收尾改写成
			// tool_calls，客户端据此知道「回合以工具调用结束」而不是「模型只说了话」。
			if synthIdx > 0 && rewriteFinishReason(obj, "tool_calls") {
				toolFinishSent = true
			}
			return []map[string]any{obj}
		}
		// 命中：finish_reason 必须挪到工具调用帧之后，否则客户端在读到调用之前就
		// 认为回合已经结束。注意「本帧本来没有 finish_reason」时**不得**补一帧收尾
		// ——上游的收尾帧随后就到，补了会变成两个 finish_reason（后续那个由
		// rewriteFinishReason 改写）。
		fr := takeFinishReason(obj)
		out := []map[string]any{obj}
		for _, call := range calls {
			idx := maxToolIdx + 1 + synthIdx
			synthIdx++
			out = append(out, markupCallFrame(obj, idx, call))
		}
		if fr != "" {
			if fr == "stop" {
				fr = "tool_calls"
			}
			out = append(out, finishOnlyFrame(obj, fr))
			toolFinishSent = fr == "tool_calls"
		}
		return out
	}

	// writeFrame 把 payload 按规范白名单重建后以 data: 帧写出并 flush。
	// 只要**成功转发了一帧**就计数（bug #12）：非 JSON 的 data 帧（心跳/纯文本兼容
	// 形态）此前不计数，若整条流都是这种帧，客户端明明收到了内容，这里仍判空流 →
	// 补写 "empty upstream stream" 错误帧 + 返回 errEmptyStream，handler 记 502 假失败。
	writeFrame := func(payload string) (int, error) {
		var obj map[string]any
		if json.Unmarshal([]byte(payload), &obj) != nil {
			// 非 JSON 帧：原样降级写出，**计一次有效转发**（已成功交给客户端）。
			if werr := writeSSE(payload); werr != nil {
				return 0, werr
			}
			return 1, nil
		}
		// 上游错误帧透传（error-passthrough）：带 error 键的帧**原样写出**，不走
		// normalizeFrame 白名单——白名单会剥掉 error 字段，客户端就看不到上游
		// code/msg/requestId。error.message 即上游原文（如 6004 限流、审核拦截），
		// 计入有效帧（避免误判空流补写 "empty upstream stream"）。
		// 同样要求「键存在且值非 nil」（bug B3）："error":null 是正常 chunk。
		if e, hasErr := obj["error"]; hasErr && e != nil {
			// 旁路通知（账号处置用）：原始 payload 交给观察者，透传字节不变。
			if o.onErrorFrame != nil {
				o.onErrorFrame(payload)
			}
			if werr := writeRaw(payload); werr != nil {
				return 0, werr
			}
			return 1, nil
		}
		for _, ch := range repairFrame(obj) {
			if werr := writeChunk(ch, payload); werr != nil {
				return 0, werr
			}
		}
		return 1, nil
	}

	br := bufio.NewReaderSize(r, 64*1024)
	validFrames := 0
	// readErr 上游读错误（非 EOF）：流中途被掐。记下来走统一收尾，绝不裸 return。
	var readErr error
readLoop:
	for {
		line, err := br.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(trimmed, "data:"):
			// "data:" 后的空格可选（SSE 规范）。[DONE] 统一在循环结束后写出。
			payload := strings.TrimPrefix(strings.TrimPrefix(trimmed, "data:"), " ")
			if strings.TrimSpace(payload) == "[DONE]" {
				// 上游显式结束：停止读取，DONE 之后的任何数据（含垃圾帧）一律不再透传。
				break readLoop
			}
			n, werr := writeFrame(payload)
			validFrames += n
			if werr != nil {
				return werr
			}
		case trimmed != "":
			// 注释/其他行：原样透传
			if _, werr := io.WriteString(w, line); werr != nil {
				return clientWriteErr(werr)
			}
			if fl != nil {
				fl.Flush()
			}
		}
		// 空行（帧分隔）吞掉：本函数自产 "\n\n"
		if err != nil {
			if err == io.EOF {
				break
			}
			// 上游中途断流（RST / unexpected EOF）：不能直接 return —— 那样客户端
			// 拿到的是没有 error 帧、没有 [DONE] 的悬挂流，而这个函数承诺「恰好一个
			// [DONE]」；handler 还会把错误当「客户端断连」把账号洗白。
			// 这里记下读错误、跳出循环，走下方统一的 error 帧 + [DONE] 收尾。
			readErr = err
			break
		}
	}
	// 标记修复的尾部收尾：流结束时仍未判定的字节一律原文回吐（绝不吞字节，未闭合
	// 的块连起始标记一起交还）；若本流还原过调用、但上游从未给出收尾帧，补一帧
	// finish_reason: tool_calls，保证客户端不会把「工具调用回合」读成「模型只说了话」。
	if repair.Enabled() {
		if tail := repair.Flush(); tail != "" {
			tailObj := map[string]any{
				"object": "chat.completion.chunk",
				"choices": []any{map[string]any{
					"index": 0,
					"delta": map[string]any{"content": tail},
				}},
			}
			if werr := writeChunk(tailObj, ""); werr != nil {
				return werr
			}
			validFrames++
		}
		if synthIdx > 0 && !toolFinishSent {
			if werr := writeChunk(finishOnlyFrame(map[string]any{}, "tool_calls"), ""); werr != nil {
				return werr
			}
		}
	}
	// 上游读错误（流中途断）：补一帧 error 再补 [DONE]。客户端因此能区分
	// 「说完了」与「说到一半上游断了」，不会把半截答案当完整答案。
	if readErr != nil {
		_ = writeRaw(`{"error":{"message":"upstream stream broke before completion","type":"upstream_error","code":"upstream_read_error"}}`)
	}
	// 空流（0 有效帧）：先写一帧 error（绕过 normalizeFrame 原样保留 error 字段），
	// 再补 [DONE] 保证客户端能正常收尾，并返回非 nil error 供调用方记录。
	// 网关本地空流兜底帧走 hintFn=nil 的直写路径：该形态未覆盖（不编造 hint），
	// 且 writeRaw 的 hintFn 闭包在空流路径下可能携带上一帧的上下文造成误配。
	if validFrames == 0 && readErr == nil {
		// 用 writeSSE 而不是 writeRaw（bug B12）：上面注释明确要求"走 hintFn=nil 的
		// 直写路径"，而 writeRaw 内部会调 frameGatewayHint(hintFn, payload) ——
		// 当前 FrameKind 判 ErrClient 恰好不给 hint，属"靠巧合正确"；一旦 hint.go
		// 给 ErrClient 加了兜底文案，本地故障帧就会带上误导性的上游语义提示。
		_ = writeSSE(`{"error":{"message":"empty upstream stream","type":"upstream_error","code":"upstream_parse"}}`)
	}
	// 保证恰好写一个 [DONE]（上游漏发时兜底补上）。
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return clientWriteErr(err)
	}
	if fl != nil {
		fl.Flush()
	}
	// 读错误优先（bug #12）：上游刚开流就 RST（0 有效帧 + 读错误）时，此前先返回
	// errEmptyStream，handler 记成 "empty upstream stream (200+0 frames)"，与真正的
	// "上游 200 但空流"混在一起，排障时分不清是上游没吐数据还是连接被打断。
	if readErr != nil {
		return fmt.Errorf("%w: %v", errUpstreamRead, readErr)
	}
	if validFrames == 0 {
		return errEmptyStream
	}
	return nil
}

// frameGatewayHint 取 error 帧的 gateway_hint（hintFn 缺失/异常返回空串 → 不附加）。
func frameGatewayHint(hintFn func(string) string, payload string) string {
	if hintFn == nil {
		return ""
	}
	// panic 隔离：hint 判定是补充功能，任何实现缺陷不得击穿流透传主路径。
	defer func() { _ = recover() }()
	return strings.TrimSpace(hintFn(payload))
}

// attachHintToErrorFrame 在 error 帧的 error 对象上附加 gateway_hint 字段。
// message/code/requestId 等既有键原样保留（只加不改）；非 JSON / 无 error 对象 →
// payload 原样返回（宁可不加 hint 也不破坏原文透传）。
func attachHintToErrorFrame(payload, hint string) string {
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		return payload
	}
	e, ok := obj["error"].(map[string]any)
	if !ok {
		return payload
	}
	e["gateway_hint"] = hint
	out, err := json.Marshal(obj)
	if err != nil {
		return payload
	}
	return string(out)
}

// takeFinishReason 取出并删除所有 choice 的 finish_reason，返回第一个非空值。
// 标记修复把一帧拆成多帧时需要它：finish_reason 必须落在工具调用帧之后，否则
// 客户端在读到调用之前就认为回合结束了。
func takeFinishReason(obj map[string]any) string {
	fr := ""
	chs, _ := obj["choices"].([]any)
	for _, ci := range chs {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		if v, ok := c["finish_reason"].(string); ok && v != "" && fr == "" {
			fr = v
		}
		delete(c, "finish_reason")
	}
	return fr
}

// rewriteFinishReason 把 choice 上「已有且为 stop」的 finish_reason 改写成 want。
// 本来就没有 finish_reason 的中间帧一律不动（绝不凭空造收尾），已有其他值
// （length / content_filter 等）时也不动——那些收尾语义更需要被客户端看到。
// 返回是否发生了改写。
func rewriteFinishReason(obj map[string]any, want string) bool {
	changed := false
	chs, _ := obj["choices"].([]any)
	for _, ci := range chs {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		if v, ok := c["finish_reason"].(string); ok && v == "stop" {
			c["finish_reason"] = want
			changed = true
		}
	}
	return changed
}

// markupCallFrame 以 src 帧为模板，构造一帧只带单个 delta.tool_calls 的 chunk。
// id 不在这里写：交给 writeChunk 的 id 续传逻辑补齐（src 帧先写出，firstID 已就位）。
func markupCallFrame(src map[string]any, idx int, call MarkupCall) map[string]any {
	frame := map[string]any{
		"object": "chat.completion.chunk",
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{
				"tool_calls": markupToolCalls([]MarkupCall{call}, idx),
			},
		}},
	}
	for _, k := range []string{"created", "model"} {
		if v, ok := src[k]; ok && v != nil {
			frame[k] = v
		}
	}
	return frame
}

// finishOnlyFrame 构造一帧只带 finish_reason 的收尾 chunk（空 delta）。
func finishOnlyFrame(src map[string]any, finish string) map[string]any {
	frame := map[string]any{
		"object": "chat.completion.chunk",
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": finish,
		}},
	}
	for _, k := range []string{"created", "model"} {
		if v, ok := src[k]; ok && v != nil {
			frame[k] = v
		}
	}
	return frame
}
