// payload.go 改写发往上游的 chat 请求体：
//  1. 强制 stream:true（上游拒绝非流式）
//  2. tool_choice 归一化（上游该字段是 string，对象形式会 400 code=11101）
//  3. image_url 归一化（上游只认 OpenAI 对象形态，字符串会 400 code=11101）
package upstream

import (
	"bytes"
	"encoding/json"
	"log"
	"math"
	"strconv"
	"strings"
)

// PrepareBodyOpt 单 pass 改写；sanitize=false 时行为完全还原（仅强制 stream + 归一化 tool_choice）。
func PrepareBodyOpt(src []byte, sanitize bool) []byte {
	return PrepareBodyOptWithEffortsAndDefault(src, sanitize, nil, nil)
}

// PrepareBodyOptWithEfforts 在 PrepareBodyOpt 基础上按模型 supportedEfforts 降级 reasoning_effort：
// 仅当请求显式携带且模型不支持该档位时，改为 ≤请求档位的最高支持档；支持档全部高于请求档时取最低档；
// 未知模型/未知档位/未携带该字段一律透传。efforts 为 nil 表示未知（不降级）。
//
// 向后兼容封装：不传 defaultEfforts（无模型声明默认档），thinking.go 回退硬编码 high。
func PrepareBodyOptWithEfforts(src []byte, sanitize bool, efforts map[string][]string) []byte {
	return PrepareBodyOptWithEffortsAndDefault(src, sanitize, efforts, nil)
}

// PrepareBodyOptWithEffortsAndDefault 完整管线：efforts 降级 + thinking.go 按
// defaultEfforts（模型声明默认档）补档。defaultEfforts 为 nil 时与旧行为一致
// （deepseek 缺档回退硬编码 high）。
func PrepareBodyOptWithEffortsAndDefault(src []byte, sanitize bool, efforts map[string][]string, defaultEfforts map[string]string) []byte {
	// 旧入口没有作用域概念：按 "all"（所有角色）处理，行为逐字节不变。
	return prepareBodyOptFull(src, sanitize, efforts, defaultEfforts, "", nil, true, nil)
}

// PrepareBodyOptWithEffortPolicy 在 …AndDefault 之上接入网关思考档位策略
// （config thinking 节）：客户端缺档时补网关默认档；client_priority=false 时按网关
// 档位改写客户端显式档（override_scope 控制是否连「关闭思考」一起改写）。
// realm 用于 thinking.model_defaults 的 "realm:model" 精确匹配；policy 为 nil
// 或未启用时与 …AndDefault 完全一致。
func PrepareBodyOptWithEffortPolicy(src []byte, sanitize bool, efforts map[string][]string, defaultEfforts map[string]string, realm string, policy *EffortPolicy) []byte {
	return prepareBodyOptFull(src, sanitize, efforts, defaultEfforts, realm, policy, true, nil)
}

// brandAll 对应 features.sanitize_brand_scope=all：true 时身份类规则（品牌词 / 身份句式）
// 作用于所有消息角色（缺省、与旧行为一致）；false 时只作用于 system/developer。
func prepareBodyOptFull(src []byte, sanitize bool, efforts map[string][]string, defaultEfforts map[string]string, realm string, policy *EffortPolicy, brandAll bool, maxOut map[string]int64) []byte {
	if len(src) == 0 {
		return src
	}
	// UseNumber 保住数字原文（BUG-2）：这是**每个** chat 请求都会走的主改写管线，
	// 默认解码把整数落成 float64，>2^53 的 seed/微秒时间戳会被静默改写。
	// 同仓 rewriteModel / clampNToOne / prompt.Rewrite / InjectPromptCacheKey 都用
	// UseNumber，只有这条主路径没有 —— 口径自相矛盾。
	dec := json.NewDecoder(bytes.NewReader(src))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return src
	}
	// 先剔思考片段（上游 PR #159）：Codex 经 responses→chat 转换后会把
	// reasoning_text 透传进来，上游 content 白名单不含它 → 400 code=11101。
	dropReasoningContentParts(obj)
	obj["stream"] = true
	// max_completion_tokens → max_tokens 翻译（吸收上游 PR #116，Closes #117）：
	// OpenAI 规范里 max_tokens 已 deprecated、max_completion_tokens 是新字段；
	// DeepSeek Harness 等新客户端只发别名。WorkBuddy 上游（CN /v2 与 global
	// /console 同源）只认 max_tokens——别名透传会被上游忽略后回落默认输出上限
	// （实测 32000），长流任务被截。
	translateMaxCompletionTokens(obj)
	clampGPTMinMaxTokens(obj)
	// stream_options 仅当 body 未显式带时补 {include_usage: true}（D7）：
	// 官方 CLI 流式必发该字段，上游据此在末帧返回 usage 用量；显式带则不覆盖。
	// 不能只判「键是否存在」（BUG-04）：部分 SDK 默认发 `"stream_options": {}`，
	// 此时不补 include_usage → 上游末帧不回 usage → token/credit/速率全缺，
	// 用量账本记成「无观测」。已显式给 true 的照旧不动。
	if so, has := obj["stream_options"].(map[string]any); !has {
		obj["stream_options"] = map[string]any{"include_usage": true}
	} else if _, ok := so["include_usage"]; !ok {
		so["include_usage"] = true
	}
	normalizeToolChoice(obj)
	normalizeToolPatterns(obj)
	normalizeRoles(obj)
	normalizeImageURL(obj)
	normalizeStop(obj)
	// tool 配对四步（见 tool_pairing.go）：先重排早到结果，再合并，再重排插入物，最后清孤儿。
	// 所有模型一律执行（独立于 deepseek-only 的 sanitize 开关）。这是「让请求通过」的
	// 安全网——不完整配对的 tool_calls/tool 结果会让上游对之后每条消息都返 400，
	// 必须先行剔除；插在结果中间的非 tool 消息（Codex image_resize_notice）同样判
	// 配对断裂，先 repack 挪后，再 cleanup 删孤儿，两侧同口径。
	//
	// 四步的顺序是有约束的，不是随便排：
	//
	//   ① mergeAdjacentToolCalls 必须**最前**。它把「背靠背的两条 assistant.tool_calls」
	//      合成一条，这是 deepseek 系模型 11148 的正面修复（实测：拆成两条 assistant +
	//      deepseek-v4.1-flash → 503/11148，合成一条 → 200；glm 宽容所以看不出来）。
	//      若让 reorderEarly 先跑，它会把早到的 tool 结果**插到两条相邻 assistant 之间**，
	//      于是 merge 判「不满足相邻」直接跳过 —— 正好把这条修复废掉，deepseek 继续 11148。
	//      （形态举例：[tool(c0), assistant(c0), assistant(c1), tool(c1)]。）
	//   ② reorderEarlyToolResults 必须早于 repack/cleanup。它只处理「结果早于调用」这一种
	//      倒置：倒置没修时 repack 会把那条早到的 tool 当普通消息直通（role != assistant），
	//      cleanup 又因两侧 id 都在判「配对完整」，倒置就一路带到上游撞 11148。
	//   ③ repack 早于 cleanup：先把自己插进结果区的非 tool 消息挪后，再删孤儿，两侧同口径。
	if msgs, ok := obj["messages"].([]any); ok {
		// ⓪ 先合并相邻身份消息，**必须早于 sanitize**（sanitize 在本函数更后面执行）：
		// DSH 0.2.2 起把系统提示拆成一句一条 system 消息，段级脱敏正则跨不了消息边界，
		// 整段 DSH 指纹会逐字出站 → 400 11128。合并后同一段重新落在一个字符串里，
		// 既有 sanitizeDshPathsRe 才匹配得到（详见 identity_merge.go）。
		msgs, _ = mergeAdjacentIdentityMessages(msgs)
		msgs, _ = mergeAdjacentToolCalls(msgs)
		msgs, _ = reorderEarlyToolResults(msgs)
		msgs, _ = repackToolResultBlocks(msgs)
		msgs, _ = cleanupOrphanToolCalls(msgs)
		// 无改动时各步都返回原 slice，这里回写等于零操作；任一步重排/删除
		// （哪怕后续步骤零改动）也必须落到 obj——不能只在「最后一步改动」时回写，
		// 否则 repack 单独生效的结果会被原 slice 覆盖丢失。
		obj["messages"] = msgs
	}
	// DeepSeek 思维链开关（见 thinking.go）：注入 thinking.type=enabled + 缺档补默认档。
	// 先于 normalizeReasoningEffort 执行：补入的默认档也要走既有降级管线，
	// 模型不支持默认档时自动落到 ≤ 默认档的最高支持档（不出站不合规档位）。
	modelName, _ := obj["model"].(string)
	// 网关思考档位策略（见 effort_policy.go）**先于** injectThinking：override_scope=all
	// 要把客户端的 off/disabled 改写成「开启 + 目标档」，晚一步就会被 injectThinking
	// 写成 thinking.type=disabled 并删掉档位。client_priority=true 时这里只在客户端
	// 没发档位时补默认档。
	applyEffortPolicy(obj, modelName, efforts[modelName], policy, realm)
	injectThinking(obj, lookupDefaultEffort(defaultEfforts, modelName))
	normalizeReasoningEffort(obj, efforts)
	// 档位定下来之后再兜输出预算：高档位思考会把同一份额度吃掉，正文断在半句（2b9b773）。
	ensureMinOutputBudget(obj, maxOut)
	// DeepSeek 多轮一致性：assistant 消息带 reasoning 痕迹时回填 reasoning_content
	// （requiresReasoningContentOnAssistantMessages，见 thinking.go）。
	backfillReasoningContent(obj)
	// 出站前最后一道形状检查，两条都是「不可自愈的 400」：
	//   - 空 text 段（11151）：客户端重试只会重放同一段历史，换号也救不回来；
	//   - reasoning 镜像：Kimi/GLM/MiniMax 等按 `reasoning` 读历史，只看
	//     reasoning_content 的话它们的多轮一致性校验会挂。
	// 位置必须在 repack/cleanup 之后（那两步会把字符串 content 挪到别的消息上，先补位
	// 再挪动等于漏掉被挪过去的那一段），也必须在 sanitize 之后（只含指纹的 text 段会被
	// 净化洗成空串，先补位的话出站仍带 ""，照样 11151）。
	mirrorUpstreamReasoning(obj)
	if sanitize {
		if msgs, ok := obj["messages"].([]any); ok {
			sanitizeMessages(msgs, brandAll)
		}
		// 工具描述同样要洗：客户端作者把身份句写进 description 时，只洗 messages
		// 等于整段原样出站（见 sanitizeTools 注释）。
		sanitizeTools(obj["tools"], brandAll)
	}
	if msgs, ok := obj["messages"].([]any); ok {
		guardEmptyTextParts(msgs)
	}
	// 用 marshalNoEscape 而不是 json.Marshal：后者会把 < > & 转成 \u003c 等，
	// 而 Agent 流量的常态就是 XML 工具标签（<tool_use>）、HTML、&amp;、shell 重定向
	// （2>&1）—— 出站 body 与客户端原文不再逐字节一致，用 dump_dir 对比时会看到成片
	// 转义、排障时分不清是客户端发的还是网关改的。语义上 JSON 等价，但本仓把
	// 「body 字节与真实客户端不一致」列为指纹面之一。
	out, err := marshalNoEscape(obj)
	if err != nil {
		return src
	}
	return out
}

// translateMaxCompletionTokens 把 OpenAI 别名 max_completion_tokens 翻译为上游
// 认的 max_tokens（吸收上游 PR #116）。规则：显式 max_tokens 优先（别名只删）；
// 别名非正数值（0/null/负数）不翻译（0/null 语义是「未设置」，负数是非法值，
// 翻译等于把垃圾搬进 max_tokens）；非数值别名（字符串等畸形）不翻译（原样
// 透传由上游报 11101 参数错）。两域同口径：CN /v2 与 global /console 是同一套
// API，翻译不分 realm。
func translateMaxCompletionTokens(obj map[string]any) {
	alias, has := obj["max_completion_tokens"]
	if !has {
		return
	}
	// 只在**真正翻译成功**后才删别名（bug #1）：此前第一行就 delete，于是
	//   - 小数（1000.5）→ 别名没了、max_tokens 没写 → 上游回落自己的默认上限，长任务被截；
	//   - 字符串（"1000"）→ 同样被删，而函数注释承诺"非数值别名不翻译、原样透传
	//     由上游报 11101" —— 删掉之后连 11101 都报不出来。
	// 现在：能翻成正整数就翻（小数四舍五入），翻不了就保留别名交给上游判定。
	// 审查 W4-U1-13（P2）：守卫必须是「**值有效**」而不是「键存在」。
	//
	// 缺陷：`if _, explicit := obj["max_tokens"]; !explicit` 只看键在不在。
	// 客户端发 `{"max_tokens":null,"max_completion_tokens":1000}` 时键存在 → 不翻译；
	// 而收尾的删除守卫用的是 hasPositiveInt（值有效）→ null 不是有效值 → 别名也不删。
	// 结果是 body 里同时带 null 的 max_tokens 和 1000 的别名，上游只认 max_tokens →
	// 按默认上限截断或直接 11101，而客户端明明给了额度。
	// 改成 !hasPositiveInt：null / 0 / 非数值一律视为「没给」，照常从别名翻译。
	if !hasPositiveInt(obj["max_tokens"]) {
		// json.Unmarshal 数字 → float64（整数去整后回写，避免 1.28e5 科学计数法/小数
		// 尾巴进上游 body）；其他数值类型防御性兼容（int 家族——手构造 map 的调用方）。
		switch v := alias.(type) {
		case json.Number:
			// UseNumber 之后数字是 json.Number（BUG-2 的连带）：不处理就会落 default，
			// 翻译静默失效。整数值直接用；小数值四舍五入。
			if i, ierr := v.Int64(); ierr == nil {
				if i > 0 {
					obj["max_tokens"] = i
				}
			} else if f, ferr := v.Float64(); ferr == nil {
				if n := int64(f + 0.5); n > 0 {
					obj["max_tokens"] = n
				}
			}
		case float64:
			if n := int64(v + 0.5); n > 0 { // 小数四舍五入（1000.5 → 1001）
				obj["max_tokens"] = n
			}
		case int64:
			if v > 0 {
				obj["max_tokens"] = v
			}
		case int:
			if v > 0 {
				obj["max_tokens"] = int64(v)
			}
		}
	}
	// 翻译完成（或**有效的** max_tokens 已存在）才删别名；翻译失败保留原样透传。
	//
	// 判据必须是「值有效」而不是「键存在」（报告 #4）：客户端发
	// {"max_tokens": null, "max_completion_tokens": 1000} 时，只看键存在会把别名删掉、
	// max_tokens 仍是 null → 上游按默认上限截断或直接 11101，而客户端明明给了额度。
	if hasPositiveInt(obj["max_tokens"]) {
		delete(obj, "max_completion_tokens")
	}
}

// gptMinMaxTokens GPT 系上游接受的 max_tokens 下限。
const gptMinMaxTokens = 16

// clampGPTMinMaxTokens 把 GPT 系模型过小的 max_tokens 抬到下限。
//
// 背景：上游 GPT 系（实测 gpt-6-sol / gpt-6-luna / gpt-5.6-sol）对 max_tokens < 16
// 一律 400 code=11133 model_param_invalid（15 拒、16 过，同号同 body 对照）；hy4 等
// 非 GPT 模型无此限制。Claude Code 切模型时发 max_tokens 极小的探针，全号轮转同样
// 被拒 → 客户端 503，模型永远切不过去。账号与 body 其余部分无关，换号无用，只能
// 在发送前修。抬到下限只放宽输出上限、不改语义；未携带字段 / 非数值 / 已达下限一律不动。
func clampGPTMinMaxTokens(obj map[string]any) {
	model, _ := obj["model"].(string)
	if !strings.Contains(strings.ToLower(model), "gpt-") {
		return
	}
	var v int64
	switch n := obj["max_tokens"].(type) {
	case json.Number:
		// UseNumber 的连带：不认 json.Number 会让「抬到下限」静默失效，
		// Claude Code 切模型时的极小 max_tokens 探针又会被上游 11133 拒掉。
		if i, ierr := n.Int64(); ierr == nil {
			v = i
		} else if f, ferr := n.Float64(); ferr == nil {
			v = int64(f)
		} else {
			return
		}
	case float64:
		v = int64(n)
	case int64:
		v = n
	case int:
		v = int64(n)
	default:
		return
	}
	// 只钳「正但过小」的值（bug #19）：0 与负数在客户端语义里常表示"用模型默认上限"，
	// 抬到 16 会把响应截成 16 token（静默截断比让上游报 11133 更糟 —— 后者可诊断）。
	if v > 0 && v < gptMinMaxTokens {
		obj["max_tokens"] = int64(gptMinMaxTokens)
		log.Printf("max_tokens clamped model=%s %d -> %d", model, v, gptMinMaxTokens)
	}
}

// effortRank 档位从低到高。
var effortRank = map[string]int{"off": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6}

// normalizeReasoningEffort 按模型 supportedEfforts 降级 reasoning_effort（snake/camel 双字段兼容）。
//   - 请求档位模型支持 → 原样透传
//   - 请求档位不支持 → 改为 ≤请求档位的最高支持档（降级）
//   - 支持档全部高于请求档 → 取最低支持档（偏离最小）
//   - 未知模型/未知档位/未携带字段/模型未缓存 → 一律透传
func normalizeReasoningEffort(obj map[string]any, efforts map[string][]string) {
	if len(efforts) == 0 {
		return
	}
	model, _ := obj["model"].(string)
	if model == "" {
		return
	}
	// off/none 是「关闭思考」的表达，不是可降级的档位。没有模型声明 off 档位
	// （effortRank 里 rank 0 的 off 只是排序占位），不截住就会被就近降级成最弱
	// 正档——客户端要关闭，网关却给它开了最弱的思考（实测 glm 变 low）。
	// deepseek 的关闭形态由 injectThinking 写成 thinking.type=disabled。
	if effortValueOff(obj) {
		delete(obj, "reasoning_effort")
		delete(obj, "reasoningEffort")
		return
	}
	supported, ok := efforts[model]
	if !ok || len(supported) == 0 {
		return
	}
	key := ""
	if _, present := obj["reasoning_effort"]; present {
		key = "reasoning_effort"
	} else if _, present := obj["reasoningEffort"]; present {
		key = "reasoningEffort"
	} else {
		return
	}
	reqStr, ok := obj[key].(string)
	if !ok {
		return
	}
	reqStr = strings.TrimSpace(strings.ToLower(reqStr))
	reqIdx, known := effortRank[reqStr]
	if !known {
		return
	}
	// 在 ≤请求档位的支持档里选最高档；命中且与请求不同才改写。
	best, bestIdx := "", -1
	for _, s := range supported {
		idx, k := effortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx <= reqIdx && idx > bestIdx {
			best, bestIdx = s, idx
		}
	}
	if best != "" {
		if !strings.EqualFold(best, reqStr) {
			obj[key] = best
			log.Printf("reasoning_effort downgraded model=%s %s -> %s", model, reqStr, best)
		}
		return
	}
	// 支持档全部高于请求档：取最低支持档。
	lowest, lowestIdx := "", 1<<30
	for _, s := range supported {
		idx, k := effortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx < lowestIdx {
			lowest, lowestIdx = s, idx
		}
	}
	if lowest != "" {
		obj[key] = lowest
		log.Printf("reasoning_effort floored model=%s %s -> %s", model, reqStr, lowest)
	}
}

// normalizeRoles 把 messages 里的 developer 角色归一为 system。
//
// 背景：上游对 messages 的 role 字段做白名单校验，developer 不在白名单内，
// 命中即 HTTP 400 code=11128。developer 是 OpenAI 新规范里 system 的别名
// （Codex / Cursor 等新客户端用它承载 system 级指令），改写为 system 不丢语义。
//
// 此归一化是「协议兼容」（补上游 role 白名单），不是「内容脱敏」，
// 因此有意与 SanitizeFingerprints / sanitize 参数解耦：即使 sanitize=false 也照常归一。
//
// 只认 developer 这一个值：其余 role（system/user/assistant/tool/任意未知值）一律原样保留，
// 不合并、不重排、不删除任何消息（上游对多 system 的行为尚未实测，合并会引入新变量）。
func normalizeRoles(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for i, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, ok := msg["role"].(string)
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(role), "developer") {
			msg["role"] = "system"
			log.Printf("role normalized developer->system idx=%d", i)
		}
	}
}

// normalizeImageURL 兼容 OpenAI chat 多模态内容的两种 image_url 写法。
//
// OpenAI Chat Completions 规范使用对象形态 {"url":"...","detail":"..."}，
// 部分客户端（以及 Responses -> Chat 转换器）会发送字符串形态 "data:..." 或
// "https://..."。WorkBuddy 上游只接受对象形态，字符串会返回 400 code=11101
// "cannot unmarshal string into ... ImageContent"。
//
// 这里只做形状转换：字符串转 {"url": 原值}；已有对象及其中 url/detail/mime_type
// 原样保留；空字符串、缺失值、对象内非法 url 一律不补默认值，让上游返回真实错误。
func normalizeImageURL(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for _, rawMsg := range msgs {
		msg, ok := rawMsg.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok || part["type"] != "image_url" {
				continue
			}
			imageURL, ok := part["image_url"].(string)
			if !ok || imageURL == "" {
				continue
			}
			part["image_url"] = map[string]any{"url": imageURL}
		}
	}
}

// normalizeStop 兼容 OpenAI chat 请求 stop 字段的两种合法写法。
//
// OpenAI 规范允许 stop 为字符串或字符串数组（≤4 个停止词），部分客户端（SillyTavern
// 等）只发字符串形态；但上游 Go struct 把 stop 定义成 []string，字符串形态直接解码
// 失败：400 code=11101 "cannot unmarshal string into Go struct field ***.stop of type
// []string"。网关把 11101 归 ErrBadParams（不罚号但**仍轮换**），同一 body 换任何号都
// 必然同样 400——全池试完走末端透传，客户端看到「所有账号不可用」，与账号健康无关。
//
// 只做形状转换：字符串 → 单元素数组。**不 TrimSpace**——停止词的前后空白是匹配语义
// 本身（"\n\nHuman:"、"\n\n" 靠前导换行触发），裁剪等于静默改写客户端意图。空串同样
// 转 [""] 保留（不替客户端判定什么是无效值）；数组 / null / 其他类型 / 字段缺失一律不动。
func normalizeStop(obj map[string]any) {
	s, ok := obj["stop"].(string)
	if !ok {
		return
	}
	obj["stop"] = []string{s}
}

// dropReasoningContentParts 剔除 chat content 数组里的 reasoning_text / reasoning 片段
// （吸收上游 PR #159）。
//
// 场景：Codex 只走 /v1/responses，第三方中转的 responses→chat 转换器（如 new-api 的
// openai_responses_to_openai_chat_completions）会把助手历史里的思考片段原样透传进
// /v1/chat/completions；上游 content 类型白名单不含它，命中即 HTTP 400 code=11101
// 「unsupported content type at index 0: reasoning_text」。本仓自己的 responses 入站
// 不产这种片段（客户端回传的 reasoning item 在 convertInputItems 里转成消息级的
// reasoning_content 字段，不进 content），这条是给「链路上游还有别的转换器」兜底。
//
// 思考片段只是模型私有推理，下游续写不需要，剔除无副作用：
//   - 删除 type 为 reasoning_text / reasoning 的片段（TrimSpace 后比较）
//   - 删除后 content 为空且该消息无 tool_calls → 整条消息一并删除；有 tool_calls 则保留
//     （留空数组，避免破坏工具调用配对）
func dropReasoningContentParts(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	out := make([]any, 0, len(msgs))
	for _, rawMsg := range msgs {
		msg, ok := rawMsg.(map[string]any)
		if !ok {
			out = append(out, rawMsg)
			continue
		}
		parts, ok := msg["content"].([]any)
		if !ok {
			out = append(out, rawMsg)
			continue
		}
		kept := make([]any, 0, len(parts))
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok {
				kept = append(kept, rawPart)
				continue
			}
			ptype, _ := part["type"].(string)
			if tv := strings.TrimSpace(ptype); tv == "reasoning_text" || tv == "reasoning" {
				continue
			}
			kept = append(kept, rawPart)
		}
		if len(kept) == 0 {
			// 审查 W4-U1-18（P2）：**「置空保留」只对 assistant 轮成立**。
			//
			// 缺陷：原来对所有角色一律 `msg["content"] = ""`。user 消息的 content
			// 全被识别成 reasoning 片段时（responses→chat 转换的畸形历史、或客户端
			// 把思考塞进 user 轮），出站就多一条**空 content 的 user 消息** ——
			// 上游对空内容直接 11151/400，而日志里没有任何痕迹（原注释只讨论
			// assistant 轮的交替关系，把 user 轮一起带进去了）。
			// 空 user 轮本身不携带任何可用信息（片段全是思考），整条删掉比留空更安全：
			// 留空是**确定**的 400，删掉最坏只是少一轮无信息量的历史。
			role, _ := msg["role"].(string)
			_, hasToolCalls := msg["tool_calls"]
			if !hasToolCalls {
				if role != "assistant" {
					log.Printf("WARN: [upstream] %s 消息的 content 全是 reasoning 片段，已整条丢弃（留空 content 会 11151）", role)
					continue
				}
				// 整条删掉会破坏 user/assistant 交替（报告 #6）：Codex 经
				// responses→chat 转换后会产出「content 全是 reasoning 片段」的 assistant
				// 轮，删掉它历史就少一轮，上游可能直接 400，而日志里毫无痕迹。
				// 改为保留该轮、把 content 置空串（最小改动，交替关系不变）。
				msg["content"] = ""
				out = append(out, msg)
				continue
			}
			// 带 tool_calls 但内容全是思考片段：**删掉 content 键**而不是写 []（报告 #7）。
			// `"content": []` 与 `"content": null` 在上游是不同的形态，空数组可能被判非法。
			delete(msg, "content")
			out = append(out, rawMsg)
			continue
		}
		msg["content"] = kept
		out = append(out, rawMsg)
	}
	obj["messages"] = out
}

// consoleSystemFallback 兜底 system 正文：只写行为约定，不写身份（上游 e4e6f5c / issue #187）。
const consoleSystemFallback = "Follow the user's instructions and respond in the user's language."

// ensureConsoleSystem global realm 兜底 system 注入（吸收 PR #45，防 console 域上游 code 11-128）：
// 首条消息非 system 时在 messages 最前补一条 fallback system。仅对 global 请求调用
// （CN 现状不动；即使首条就是 system 也不重复注入）。
//
// 兜底文本**不声明身份**：上游的判据只是「首条必须是 system」，正文不参与校验；写成
// 「You are a helpful assistant.」等于网关自己往对话里插一句身份声明，与客户端 harness
// 自带的人设并列时，模型会把它当成外部注入而拒绝执行（issue #187 的现象）。
// body 不可解析时原样返回（与 prepareBody 语义一致：坏 body 不在这里二次错误化）。
func ensureConsoleSystem(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	// UseNumber（报告 #11）：主管线已用它保住数字原文，这里若用默认 Unmarshal 再解
	// 一次，超过 2^53 的整数（seed、微秒时间戳）会变成浮点近似 —— 整段 body 失真。
	var obj map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&obj); err != nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return body
	}
	first, ok := msgs[0].(map[string]any)
	if ok {
		if role, _ := first["role"].(string); strings.EqualFold(strings.TrimSpace(role), "system") {
			return body // 首条已是 system：不注入
		}
	}
	obj["messages"] = append([]any{map[string]any{"role": "system", "content": consoleSystemFallback}}, msgs...)
	// 不转义 HTML（报告 #11）：Marshal 默认把 < > & 转成 \u003c 等，与客户端原文不一致。
	out, err := marshalNoEscape(obj)
	if err != nil {
		return body
	}
	return out
}

// normalizeToolChoice 按上游 Go struct（string 类型）改写 OpenAI tool_choice。
//   - "none"            → 删 tool_choice + 删 tools/functions
//   - {"type":"none"}   → 同上
//   - {"type":"auto"/"required"} → 字符串 "auto"/"required"
//   - {"type":"function","function":{"name":"x"}} → 字符串 "x"
//   - 其他对象/非标量 → 删 tool_choice
func normalizeToolChoice(obj map[string]any) {
	suppress := func() {
		delete(obj, "tools")
		delete(obj, "functions")
	}
	tc, present := obj["tool_choice"]
	if !present {
		return
	}
	switch v := tc.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			delete(obj, "tool_choice")
			suppress()
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		typ = strings.ToLower(strings.TrimSpace(typ))
		switch typ {
		case "none":
			delete(obj, "tool_choice")
			suppress()
		case "auto", "required":
			obj["tool_choice"] = typ
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = v["name"].(string)
			}
			if name = strings.TrimSpace(name); name != "" {
				obj["tool_choice"] = name
			} else {
				// 缺 name 的 function 约束：**不能**降级成 auto（报告 #5）——
				// 那等于把「只许调这个工具」放开成「随便调」，客户端看不出任何异常，
				// 表现就是模型乱调工具。保留原样交给上游判错。
				// （保留原值即不动 obj["tool_choice"]）
			}
		default:
			// 未知 type（如 allowed_tools）：同样不删（报告 #5）。删掉 = 静默放宽，
			// 保留则上游要么认识、要么明确报错，两种都比「悄悄放开」好。
		}
	default:
		// 非字符串/非对象形态：同样保留，交给上游判。
	}
}

// normalizeToolPatterns 归一化 tools 子树的参数定义（OpenAI 形态
// tools[].function.parameters 与裸 tools[].parameters 两种写法）：
//   - 顶层补齐：type 缺失或非标别名 → object，properties 缺失 → {}；
//   - 顶层 $ref：能按 $defs 展开就就地展开，展不开也必须删（见 normalizeToolParameters）；
//   - 递归规整整棵 Schema 树（见 normalizeSchemaTree）。
//
// 为什么非做不可：上游对 tools 是「整颗定义一次过」的 Go 强类型反序列化，任一处形态
// 不合规就整条请求 400 code=11129「工具定义不合规」，换任何账号都是同一处拒绝。
// 此前网关没把这个码认成确定性请求级错误（见 client.go 的 isToolSchemaRejected），
// 于是逐号轮转、每个健康号白记一笔连败，连败到阈值触发连败降权，整池降权后后续
// 请求全 503（2026-11-18 实案）。所以「出站前把 schema 修成上游能吃的形状」与
// 「11129 判请求级」必须一起落，只落一半等于没修。
//
// 归一无损的边界：只动**形态**（缺的类型、Python 风格别名、非法取值类型、容器位置），
// 不动**数据**——default / const / enum / example(s) 里的取值是客户端声明的数据，
// 里面出现 type / items / required / null 也只是数据（见 schemaDataKeys）。
//
// 独立于 sanitize 开关：这是「让请求通过」，不是脱敏。
func normalizeToolPatterns(obj map[string]any) {
	rawTools, ok := obj["tools"].([]any)
	if !ok {
		return
	}
	for _, raw := range rawTools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		// OpenAI 形态 tools[].function.parameters；裸 tools[].parameters 兼容。
		if fn, ok := tool["function"].(map[string]any); ok {
			fn["parameters"] = normalizeToolParameters(fn["parameters"])
		}
		if _, ok := tool["parameters"]; ok {
			tool["parameters"] = normalizeToolParameters(tool["parameters"])
		}
	}
}

// normalizeToolParameters 归一化单颗工具的**顶层**参数定义，再递归规整整棵 Schema 树。
//
// 顶层必须是对象：上游的 parameters 是强类型结构体，null / 字符串 / 数组一律 400
// 11129。补出来的 {"type":"object","properties":{}} 是「无参工具」的合法形态。
func normalizeToolParameters(node any) any {
	if node == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	params, ok := node.(map[string]any)
	if !ok {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	// 顶层 $ref：整份参数 schema 指向 $defs 时上游一刀拒收（实测 400 11129），而嵌套
	// $ref 实测 200 —— 只有顶层要处理。能就地展开就展开（保住真实参数定义），展不开
	// （非 #/$defs/ 的 ref、目标缺失、循环）也必须删掉：留着等于保证这颗工具每次请求
	// 都 400，删掉至少退化成一个无参工具。
	if ref, ok := params["$ref"].(string); ok {
		delete(params, "$ref")
		if defs, ok := params["$defs"].(map[string]any); ok && strings.HasPrefix(ref, "#/$defs/") {
			if t, ok := inlineKimiSchema(map[string]any{"$ref": ref}, defs, nil, 0).(map[string]any); ok {
				for k, v := range t {
					if _, dup := params[k]; !dup {
						params[k] = v
					}
				}
			}
		}
	}
	if normalizeSchemaTypeAlias(params) == "" {
		params["type"] = "object"
	}
	if params["type"] == "object" {
		if _, hasProps := params["properties"].(map[string]any); !hasProps {
			params["properties"] = map[string]any{}
		}
	}
	normalizeSchemaTree(params)
	return params
}

// schemaTypeAliases 弱类型客户端（Python dict、PHP array、手写 JSON 模板）常见的非标
// 类型别名 → JSON Schema 标准类型。上游按 Go 强类型反序列化，别名一律 400 11129；
// 这些映射在 JSON Schema 语义上无损（str→string 等只是同一个类型的另一种写法）。
var schemaTypeAliases = map[string]string{
	"str":   "string",
	"int":   "integer",
	"float": "number",
	"bool":  "boolean",
	"dict":  "object",
	"list":  "array",
}

// normalizeSchemaTypeAlias 把 type 的非标别名纠成标准类型，返回纠完之后的 type
// （未声明 type / 非字符串形态返回空串）。
//
// 非字符串形态（联合类型 `["string","null"]`）原样保留：那是合法 schema，上游实测
// 200，改写它等于把「可为 null」改成「只能 string」，语义没了。
func normalizeSchemaTypeAlias(n map[string]any) string {
	t, isStr := n["type"].(string)
	if !isStr {
		return ""
	}
	t = strings.TrimSpace(t)
	if t == "" {
		return ""
	}
	if std, ok := schemaTypeAliases[strings.ToLower(t)]; ok {
		n["type"] = std
		return std
	}
	return t
}

// fillObjectTypeIfImplied type **缺省**（键不存在或空串）但带 properties 的节点补
// type=object。联合类型这类非字符串形态不动（见 normalizeSchemaTypeAlias）。
func fillObjectTypeIfImplied(n map[string]any) {
	if v, has := n["type"]; has {
		if s, isStr := v.(string); !isStr || strings.TrimSpace(s) != "" {
			return
		}
	}
	if _, hasProps := n["properties"]; hasProps {
		n["type"] = "object"
	}
}

// schemaContainerKeys 定义了 JSON Schema 规范里的容器级 map。
//
// 容器自身的键是属性名 / 定义名（**用户数据**），绝不能当作 Schema 关键字判定；
// 递归时只遍历它的子 Schema 节点。不这么分的话，属性名叫 type / items / required
// 的客户端会被改成 {"type":"string"} 这种垃圾（`properties.required` 被当成
// required 关键字处理）。
var schemaContainerKeys = map[string]bool{
	"properties":        true,
	"patternProperties": true,
	"$defs":             true,
	"definitions":       true,
	"dependentSchemas":  true,
	// dependentRequired 的值是属性名数组、dependencies（draft-7）的值是属性名数组或
	// 子 schema：键都是属性名。不当容器，属性名叫 items / type 时会被当成关键字改写
	// （["type"] 被换成 {"type":"string"}）。
	"dependentRequired": true,
	"dependencies":      true,
}

// schemaDataKeys 取值是**数据**而不是 schema 的关键字：default / const / enum /
// example(s) 里的对象就是客户端声明的取值本身，里面出现 type、items、required、null
// 也只是数据。规整只作用于 schema，绝不递归进这些值——否则改写的是模型看到的默认值、
// 常量和枚举项（如 default {"type":"str"} 被改成 "string"）。
var schemaDataKeys = map[string]bool{
	"default":  true,
	"const":    true,
	"enum":     true,
	"example":  true,
	"examples": true,
}

// schemaNumberKeys / schemaBoolKeys / schemaStringKeys 三组关键字的**取值类型**同样被
// 上游按 Go 强类型反序列化校验，类型不符即整体拒收（code=11129，displayMsg「工具定义
// 不合规」）。2026-11-18 在 www.workbuddy.cn 上逐形态单独实测，每个样本只带一处缺陷：
//
//	minimum:"1"        → 400 11129      minimum:1        → 200
//	uniqueItems:"true" → 400 11129      uniqueItems:true → 200
//	description:123    → 400 11129      description:"x"  → 200
//	enum:"x"           → 400 11129      enum:["x"]       → 200
//	additionalProperties:"false" → 400 11129
//
// 弱类型客户端（Python dict、PHP array、手写 JSON 模板）序列化出来就是这些形状；同一
// 实测里联合类型 type:["string","null"]、oneOf、嵌套 $ref + $defs、items:{}、
// required 指向不存在的属性全部 200，所以这里只纠这三组与顶层 $ref，不扩面。
var schemaNumberKeys = []string{
	"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf",
	"minLength", "maxLength", "minItems", "maxItems", "minProperties", "maxProperties",
}
var schemaBoolKeys = []string{"uniqueItems", "deprecated", "readOnly", "writeOnly"}
var schemaStringKeys = []string{"description", "title", "format", "pattern"}

// coerceSchemaKeywords 把三组关键字的取值纠成上游能接受的基本类型；纠不动就删键
// （留着一个非法值等于保证整颗工具必然 400，删掉只是丢掉一条约束或注释）。
func coerceSchemaKeywords(n map[string]any) {
	for _, k := range schemaNumberKeys {
		if v, ok := n[k]; ok {
			if num, ok := schemaNumber(v); ok {
				n[k] = num
			} else {
				delete(n, k)
			}
		}
	}
	for _, k := range schemaBoolKeys {
		if v, ok := n[k]; ok {
			if b, ok := schemaBool(v); ok {
				n[k] = b
			} else {
				delete(n, k)
			}
		}
	}
	for _, k := range schemaStringKeys {
		if v, ok := n[k]; ok {
			if _, isStr := v.(string); !isStr {
				delete(n, k)
			}
		}
	}
	if v, ok := n["additionalProperties"]; ok {
		if _, isSchema := v.(map[string]any); !isSchema {
			if b, ok := schemaBool(v); ok {
				n["additionalProperties"] = b
			} else {
				delete(n, "additionalProperties")
			}
		}
	}
	for _, k := range []string{"enum", "examples"} {
		if v, ok := n[k]; ok {
			if _, isList := v.([]any); !isList {
				delete(n, k)
			}
		}
	}
}

// schemaNumber 纠数值关键字：数字原样，纯数字字符串转数值，其余（bool / 对象 /
// NaN / Infinity）判非法。
//
// **必须认 json.Number**（本仓主管线 prepareBodyOptFull 用 dec.UseNumber() 解码，
// 为保住大整数原文）：只认 float64 的话，客户端**本来就合法**的 `minimum: 1` 在
// 生产路径上是 json.Number，会被判非法而删掉——把一条真约束静默抹掉，而上游本来
// 完全接受它。数字形态原样保留 json.Number（重序列化仍是原文数字）。
func schemaNumber(v any) (any, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, false
		}
		return x, true
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil, false
		}
		return x, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, false
		}
		return f, true
	}
	return nil, false
}

// schemaBool 纠布尔关键字：bool 原样，"true"/"false" 转义，其余判非法。
func schemaBool(v any) (bool, bool) {
	switch x := v.(type) {
	case bool:
		return x, true
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "true":
			return true, true
		case "false":
			return false, true
		}
	}
	return false, false
}

// normalizeSchemaTree 递归归一化工具参数 schema 树（出站前把上游吃不下、但语义上
// 可无损纠正的形态统一掉，见 normalizeToolPatterns 的「为什么非做不可」）：
//   - 关键字取值类型纠偏（数值/布尔/字符串关键字，见 coerceSchemaKeywords）；
//   - 剔除值为 nil 的关键字（description: null、title: null 等导致上游反序列化失败），
//     数据关键字（default / const / enum / example(s)）的 null 保留；
//   - 类型别名归一化（str/int/bool/dict/list 等 Python 非标类型别名映射至标准类型）；
//   - 区分 Schema 节点与 Schema 容器：容器自身键为用户属性名，只对其子 Schema 做规整；
//   - required 规整（剔除 nil、空字符串及非 string 元素，去重，空列表直接清理）；
//   - array items 规整（解包 tuple 模式，清理 nil items）；
//   - pattern 值与 patternProperties 键里的 `\_` → `_`（patternProperties 的键也是正则；
//     map 键不可原地改，命中时重建该层）；
//   - 只有 enum、没有 type 的属性补上可无损推断的类型（见 inferEnumType）。
//
// `\_` 那一条的实案：上游对 pattern 做严格正则文法校验，`\_`（转义的字面量下划线）
// 会整体拒收（400 11129）。`\_` 不是任何正则文法的合法转义，但所有主流引擎
// （RE2/PCRE/JS Annex B）都宽容地视为 `_` 本身——上游校验器比它们全部更严（对照 V8
// 严格文法 u 标志，唯一同样拒绝的实现）。ZCode 的 exa 插件 agent_run 工具
// runId/previousRunId 带 `^agent\_run\_`，deepseek 系全家确定性 400（2026-09-29/30
// 两次实案）。归一无损：`\_` 与 `_` 在所有引擎匹配语义相同（各引擎实测 + 上游对照
// 探针：归一后 200），工具方功能不变。只动 tools 子树；消息正文里的 `\_`
// （如 Windows 路径 C:\_x）不碰。其余非标转义（`\:` 等）未证实触发，不扩面——有实案
// 再议。
func normalizeSchemaTree(node any) {
	switch n := node.(type) {
	case map[string]any:
		// 0. 关键字取值类型纠偏。放在最前：纠不动的键被删掉，后面的 nil 剔除与类型
		// 推断就只看剩下的合法值，不必每步都防一次类型。
		coerceSchemaKeywords(n)

		// 1. 剔除值为 nil 的关键字（避免上游 Go 强类型反序列化失败报 11129）。
		// 数据关键字除外：const:null（只能是 null）、default:null（缺省为 null）是有
		// 意义的取值，删了就改了语义（见 schemaDataKeys）。
		for k, v := range n {
			if v == nil && !schemaDataKeys[k] {
				delete(n, k)
			}
		}

		// 2. 类型别名归一化。空 type 但带 properties 的当 object——上游对 object
		// 形态有额外要求，type 缺省时它会按「无类型」拒收。联合类型（数组形态）保留。
		normalizeSchemaTypeAlias(n)
		fillObjectTypeIfImplied(n)

		// 3. 规整 required 字段。重复项也要去重：`["a","a"]` 实测同样 400 11129。
		if req, hasReq := n["required"]; hasReq {
			if reqList, ok := req.([]any); ok {
				var cleaned []any
				seen := make(map[string]bool, len(reqList))
				for _, item := range reqList {
					if s, ok := item.(string); ok {
						s = strings.TrimSpace(s)
						if s != "" && !seen[s] {
							seen[s] = true
							cleaned = append(cleaned, s)
						}
					}
				}
				if len(cleaned) == 0 {
					delete(n, "required")
				} else {
					n["required"] = cleaned
				}
			} else {
				delete(n, "required")
			}
		}

		// 4. 规整 array items：tuple 模式（items 是数组）上游不收，取第一个对象子
		// schema（多元素时语义已退化，但比整颗工具 400 强）；全是标量时退化成 string。
		if items, hasItems := n["items"]; hasItems {
			if items == nil {
				delete(n, "items")
			} else if itemList, ok := items.([]any); ok {
				var firstDict map[string]any
				for _, it := range itemList {
					if d, ok := it.(map[string]any); ok {
						firstDict = d
						break
					}
				}
				if firstDict != nil {
					n["items"] = firstDict
				} else {
					n["items"] = map[string]any{"type": "string"}
				}
			}
		}

		// 5. pattern 转义修复（含 patternProperties 的键：键也是正则，map 键不可原地
		// 改，命中时重建该层）。
		if p, ok := n["pattern"].(string); ok && strings.Contains(p, `\_`) {
			n["pattern"] = strings.ReplaceAll(p, `\_`, `_`)
		}
		if props, ok := n["patternProperties"].(map[string]any); ok {
			rebuilt := false
			fixed := make(map[string]any, len(props))
			for k, v := range props {
				if strings.Contains(k, `\_`) {
					k = strings.ReplaceAll(k, `\_`, `_`)
					rebuilt = true
				}
				fixed[k] = v
			}
			if rebuilt {
				n["patternProperties"] = fixed
			}
		}

		// 6. enum 缺 type：Kimi 系对参数 schema 严格校验，只写 enum 不写 type 直接
		// 400 11133「Invalid request parameters」——同一个模型对带这类 schema 的客户端
		// 等于全废（DSH 等客户端的整套工具定义里就有这种写法）。类型能无损推断就补上，
		// 推断不出唯一类型（混合标量/含 null）保持原样让上游按原语义报错，不猜。
		// enum 取值一字不改；response_format 不走这条（那是输出约束，补错类型会改语义）。
		if _, hasType := n["type"]; !hasType {
			if e, ok := n["enum"].([]any); ok && len(e) > 0 {
				if t := inferEnumType(e); t != "" {
					n["type"] = t
				}
			}
		}

		// 7. 递归子树：数据关键字整段跳过；容器键只遍历子 Schema（容器自身的键是属性
		// 名，见 schemaContainerKeys），防止属性名叫 required / type 的被当成关键字改写。
		for k, v := range n {
			if schemaDataKeys[k] {
				continue
			}
			if schemaContainerKeys[k] {
				if c, ok := v.(map[string]any); ok {
					for childKey, child := range c {
						if child == nil {
							delete(c, childKey)
							continue
						}
						normalizeSchemaTree(child)
					}
				}
				continue
			}
			normalizeSchemaTree(v)
		}
	case []any:
		for _, v := range n {
			normalizeSchemaTree(v)
		}
	}
}

// unescapePatternLiteralEscapes 旧入口（enum_type_test 等直接对单个节点调用）：
// 等价于 normalizeSchemaTree。名字里的「转义修复」已不足以描述现在的职责（类型别名 /
// nil 关键字 / required / items / 关键字类型纠偏），实现与理由见 normalizeSchemaTree。
func unescapePatternLiteralEscapes(node any) { normalizeSchemaTree(node) }

// inferEnumType 从 enum 取值推断 JSON Schema 类型；推不出唯一类型时返回空（不改写）。
// 按「全为整数值」区分 integer / number。
//
// **必须同时认 float64 与 json.Number**：主改写管线 prepareBodyOptFull 用
// dec.UseNumber() 解码（为保住大整数原文），所以生产路径上 enum 里的数字是
// json.Number；只有默认 Unmarshal（测试夹具）才是 float64。此前只写了 float64 分支，
// 客户端发 `{"enum":[1,2,3]}` 时三个计数器全 0、返回 ""、type 不补 → 上游照旧 11133，
// 而用例因为走默认 Unmarshal 永远是绿的。
func inferEnumType(values []any) string {
	var strs, nums, ints, bools int
	for _, v := range values {
		switch x := v.(type) {
		case string:
			strs++
		case bool:
			bools++
		case float64:
			nums++
			if x == math.Trunc(x) {
				ints++
			}
		case json.Number:
			nums++
			if _, err := x.Int64(); err == nil {
				ints++
			}
		}
	}
	n := len(values)
	switch {
	case strs == n:
		return "string"
	case bools == n:
		return "boolean"
	case nums == n && ints == n:
		return "integer"
	case nums == n:
		return "number"
	}
	return ""
}

// marshalNoEscape 序列化但不做 HTML 转义（保持与客户端原文一致）。// marshalNoEscape 序列化但不做 HTML 转义（保持与客户端原文一致）。
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// hasPositiveInt 判断 JSON 解出来的值是不是「有效的正数」。
//
// 用于「显式给了额度」这类判据：只看键存在会把 null / 0 / "" 也当成给了
// （报告 #4：max_tokens:null 时别名被删、额度丢失）。
func hasPositiveInt(v any) bool {
	switch n := v.(type) {
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i > 0
		}
		if f, err := n.Float64(); err == nil {
			return f > 0
		}
	case float64:
		return n > 0
	case int:
		return n > 0
	case int64:
		return n > 0
	}
	return false
}
