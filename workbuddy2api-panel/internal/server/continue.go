// continue.go 截断自动续写：同一账号同一模型，把多段上游流拼成一条客户端可见的连续 SSE。
//
// 背景：上游对单次响应有硬上限（实测 cn:deepseek-v4.1-flash = 32000 输出 token，
// 请求显式 50000 也被夹回 32000）。Codex 这类客户端不带输出限额，写大文件/长分析
// 在 32000 处被截：旧行为是报 incomplete 让客户端自己重试（"ERROR: Reconnecting"），
// 客户端重发几乎不带前段输出（≈重新生成）。
//
// 这里在帧级把截断点接上：某段以 finish_reason=length 结束、且本段是纯文本/推理输出
// （无工具调用分片）、客户端未带显式输出限额时，用同一账号同一模型补发
// 「已输出内容作 assistant + 续写指令」的请求（线上实测：模型从中断处无缝接续），
// 其帧直接接进同一条流；终端帧被吞、usage 跨段合并、客户端只看到一次连续输出。
//
// ponytail: 只续文本/推理段。工具参数被截断（半个 JSON）不续——拼接残缺 JSON 比
// 不续更糟，且上游管线本就丢弃残缺 tool_calls；命中该形态只记日志（等真实分布
// 决定要不要做 V2 的「剩余参数改写」）。每请求段数上限见 maxContinueSegments，
// 真正的闸门是累计输出 token（见 continueTotalCapRatio）。
//
// 续写段接缝的客户端可见性：思考在正文尚未开始时照常外泄（续着同一个 reasoning
// item 喂，见 frame()），消掉「续写段纯思考期零事件」的静默——Codex 的 provider
// 配置带 stream_idle_timeout_ms，静默超时判 responseStreamDisconnected 掐线
// （线上实测续写段静默约 122s 后客户端断开、请求记 interrupted）。正文已开始后
// 的思考仍剥离，残余空帧按 keepaliveEvery 补 SSE 注释帧（纯喂活、无语义）。
package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// maxContinueSegments 单请求最多上游段数（1 段原生 + 最多 7 次续写）。
// 段数不是主要闸门——真正管住开销的是 continueTotalCap（累计输出 token）；
// 这里只是防「续写自己也在截断」时无限套娃的硬上限。
const maxContinueSegments = 8

// defaultContinueTotalCap 没有档位下限时（客户端未指定档位、也就没算下限）
// 累计输出 token 的兜底上限。
const defaultContinueTotalCap = 32768

// continueTotalCapRatio 累计输出上限 = 该轮输出预算下限 × 这个系数。
//
// 为什么按 token 而不是按段数管：固定 3 段时，低档位（下限 4096）每段只有 ~4096
// 正文，写到第 3 段就撞上限收工——线上实测 effort=low + max_tokens=1024 写
// 「3000 字以上」的文档，3 段合计正文 9414 token 处还是半句（finish=length，尾
// 巴停在「### 5.3 …- 正常请求」）。同样的段数在高档位（下限 8192）够用，说明
// 该管的是「一共准写多少」，不是「允许问上游几次」。
const continueTotalCapRatio = 4

// continueThinkingShare 思考占「本段输出」的比例达到这个值，就认为这次截断是
// 思考吃光同一份额度造成的，而不是客户端要的长度。
//
// 背景（2026-11-07 本机实测，cn:deepseek-v4.1-flash）：上游把思考与正文算在同一个
// max_tokens 里，没有独立思考上限。客户端给 8192、网关补 effort=max 时：
//
//	effort=max  → completion=8192 think=7612 正文≈580 token → finish=length（截断）
//	effort=high → completion=5794 think=3060 正文≈2734 token → finish=stop（完整）
//
// 即「思考吃掉 93% 预算，回答只剩半句」。此时客户端的限额语义（只要这么多）已经被
// 思考消费掉了，它真正想要的那份回答并没有得到——续写才是尊重意图，不续则是把
// 「预算被思考偷走」伪装成「客户端让它停」。
//
// 阈值 0.5 的依据：正常推理档下思考占比中位数 ~0.4（同日 43256 条样本），而触到
// length 的样本占比普遍 >0.85。取 0.5 既不会把「客户端真的要短回答」误判成需要续写，
// 又能覆盖思考失控的那一批。
const continueThinkingShare = 0.5

// continueNudge 续写指令：线上实测该措辞能让模型从断点无缝接续（不重头、不道歉）。
const continueNudge = "你上一条回复因达到输出长度上限被截断。请从中断处继续输出剩余内容；不要重复已输出过的部分，不要道歉或解释，直接继续。"

// keepaliveEvery 客户端可见静默的最大间隔：超时就向流里补一条 SSE 注释帧
// （": keepalive"），只喂活连接不带语义。≤0 = 每条被吞帧都补（测试确定性用）。
var keepaliveEvery = 10 * time.Second

// continueReader 读上游 SSE 并做帧级续写拼接。它在 stats/StreamHint 之下、
// 上游 body 之上：上层看到的永远是「一条完整且恰好一个终态」的流。
// 非流式（Aggregate）路径不接线——截断语义由 starved.go 的救援与
// handler 的既有聚合逻辑负责。
type continueReader struct {
	ctx      context.Context
	up       *upstream.Client
	acct     *auth.Auth
	body     []byte // 已发往上游的原始请求体（续写时在其 messages 后追加）
	clientIP string
	meta     upstream.ChatMeta

	cur io.ReadCloser
	br  *bufio.Reader
	out []byte // 已生成待输出字节

	seg      int   // 当前段序号（0 起）
	limitSet bool  // 客户端显式带了输出限额：尊重之，不续写
	limit    int64 // 该限额的值（0 = 没带或不可解析）

	finish   string   // 本段 finish_reason（续写时每段重置）
	tail     []string // finish 之后的帧：续写时丢弃，降级时原样回放
	text     strings.Builder
	reason   strings.Builder
	toolSeen bool
	errSeen  bool
	id       string

	blindRetried bool // 断流盲重试已用（每请求一次；见 canContinue）

	starvedTried bool // 空截断救援已用（每请求一次；见 tryStarvedRetry）

	// narrationTried 预告式收尾的救援续写已用（每请求一次；见 tryNarrationRetry）。
	narrationTried bool

	// toolOnlyTried 安全续写（只采纳工具调用）已用（每请求一次；见 tryToolOnlyRescue）。
	toolOnlyTried bool
	// toolOnlyRescue 该功能的总开关（features.tool_only_rescue，缺省关闭）。
	// 由 handler 在构造后写入：默认零值 = 关，避免「正常收尾」被判成截断。
	toolOnlyRescue bool
	// toolOnly 当前段处于「只采纳工具调用」模式：帧全部改道 toolOnlyFrame，裁决前客户端什么都收不到。
	toolOnly bool
	// toolOnlyFailed 当前 toolOnly 段出现过 error 帧 / 流内失败帧 / 非法帧（整段回滚）。
	toolOnlyFailed bool
	// toolCallBuf toolOnly 段暂存的工具调用帧与终态帧（已剥掉正文/思考/usage），采纳时按序补发。
	// 否则 settleToolOnly 里按原顺序补发（见那里的说明）。
	toolCallBuf []string
	// toolCallNames/toolCallArgs 按 index 累积续写段的工具调用（name 与 arguments 都
	// 分片到达），用于与请求体历史里上一次执行的调用做同名同参比较。
	toolCallNames map[int]string
	toolCallArgs  map[int]string
	// origFinish/origTail 首段的终态帧：续写段没给工具调用时原样回放。
	origFinish string
	origTail   []string

	lastClientAt time.Time // 上次客户端可见输出/keepalive 的时刻（静默节流）

	// readErr 本段结束时的读错误（nil = 上游 [DONE] 正常收尾）。用于区分「上游
	// 断流」与「客户端先断开」——两者的 err 都是非 nil，但归因完全不同：后者是
	// 人走了，不是上游故障，报成「疑似上游断流」会把排障引向错误方向。
	readErr error

	usage map[string]any // 跨段累计 usage（数字叶子求和）

	// 本段 usage 快照（每段重置）：判断「预算被思考吃光」只能看当前段——
	// 跨段累计值在第二段会把第一段的思考也算进来，比率就失真了。
	segCompletion float64
	segThink      float64

	// budgetFloor 本轮生效档位对应的输出预算下限（upstream.MinOutputBudget）。
	// 续写/救援段的 max_tokens 至少是它：那些段从原始 body 重建、又常关掉思考，
	// 拿不到首段被抬过的额度，否则会继续用客户端那个装不下正文的小额度。
	budgetFloor int64

	// continueCapped 本请求因累计输出到顶而停止续写（用于日志与终态归因）。
	continueCapped bool

	// wrote 本请求是否已经向客户端写出过任何字节（含 keepalive 注释帧）。
	// 只有「一个字节都没写」时，读错误才继续向上冒（见 endSegment 末尾）：
	// 那个形态对客户端完全不可见，handler 还能换号重试；一旦写过字节就改不了
	// 状态码，错误只能就地降级成截断终态。
	wrote bool

	// pendingErr 待冒给上层的读错误（每请求至多一次，见 endSegment）。
	pendingErr error

	// billed 已结束各段 usage 的纯求和（记账口径）。续写、盲重试、各类救援每段都是一次单独计费的
	// 上游调用，prompt 也是每段各算一次；客户端看到的 usage（见 mergeUsage）为了不虚报上下文
	// 占用只取末段 prompt，记到账号 / 用量 / 预算上的必须是这一份。segUsage 当前段的 usage。
	billed   map[string]any
	segUsage map[string]any

	done bool
}

func newContinueReader(ctx context.Context, up *upstream.Client, acct *auth.Auth, body []byte, clientIP string, meta upstream.ChatMeta, rc io.ReadCloser) *continueReader {
	return newContinueReaderWithFloor(ctx, up, acct, body, clientIP, meta, rc, 0)
}

// newContinueReaderWithFloor 同上，另外带上「本轮生效档位对应的输出预算下限」。
// 续写/救援段用它补 max_tokens：那些段是从原始 body 重建的，不会继承首段被抬过的额度
// （见 upstream.MinOutputBudget 的说明，线上实测救援段只拿到客户端原本的小额度）。
func newContinueReaderWithFloor(ctx context.Context, up *upstream.Client, acct *auth.Auth, body []byte, clientIP string, meta upstream.ChatMeta, rc io.ReadCloser, budgetFloor int64) *continueReader {
	r := &continueReader{ctx: ctx, up: up, acct: acct, body: body, clientIP: clientIP, meta: meta, cur: rc,
		lastClientAt: time.Now(), budgetFloor: budgetFloor}
	r.br = bufio.NewReaderSize(rc, 64*1024)
	r.limitSet, r.limit = bodyOutputLimit(body)
	return r
}

// label 日志标签（昵称走加锁读取器：面板同步昵称会并发改写该字段）。
func (r *continueReader) label() string {
	return logfmt.Label(r.acct.UID, r.acct.NicknameValue())
}

// clientLimitLongAnswer 客户端额度达到这个数时按「要长文」处理，而不是「只要这么短」。
//
// 实测（global:deepseek-v4.1-flash-sg，同一句「数到 20000」）：max_tokens=32000 在
// completion=32000 整处 finish=length、正文停在半句；同一提示词不带额度能写 60660
// token 自然收尾。即上游是「给多少切多少」，不是有 32000 硬上限——这一档额度不代表
// 客户端只要这么短，只是它随手给的上限。
const clientLimitLongAnswer int64 = 32000

// bodyOutputLimit 请求体显式带的输出限额（任一别名，取第一个有效值）与「是否带了」。
// 带限额 = 客户端有意只要这么多（如 Claude 客户端必带 max_tokens），截断是它的预期
// 语义，不续写；但额度 ≥ clientLimitLongAnswer 时按「要长文」处理（见 canContinue）。
// 解析失败按「有限额」处理（不续），宁可少续不可续错。
func bodyOutputLimit(body []byte) (bool, int64) {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return true, 0
	}
	for _, k := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		v, ok := obj[k]
		if !ok || v == nil {
			continue
		}
		if f, ok := v.(float64); ok && f > 0 {
			return true, int64(f)
		}
		return true, 0
	}
	return false, 0
}

// bodyHasOutputLimit 请求体是否显式带输出限额（见 bodyOutputLimit）。
func bodyHasOutputLimit(body []byte) bool {
	has, _ := bodyOutputLimit(body)
	return has
}

func (r *continueReader) Read(p []byte) (int, error) {
	for len(r.out) == 0 && !r.done {
		r.step()
	}
	if len(r.out) > 0 {
		n := copy(p, r.out)
		r.out = r.out[n:]
		return n, nil
	}
	if r.pendingErr != nil {
		err := r.pendingErr
		r.pendingErr = nil
		return 0, err
	}
	return 0, io.EOF
}

// Close 关闭当前段 body（续写切换时已关旧段；abandon 场景由 handler defer 兜底）。
func (r *continueReader) Close() error {
	if r.cur != nil {
		return r.cur.Close()
	}
	return nil
}

// step 处理当前段的一行。与 upstream.StreamHint 的行约定保持一致：
// `data: [DONE]` 段结束；`data: <payload>` 帧；其他非空行原样透传；空行吞掉
// （帧分隔由本层自产 "\n\n"）。
func (r *continueReader) step() {
	line, err := r.br.ReadString('\n')
	trimmed := strings.TrimRight(line, "\r\n")
	switch {
	case strings.HasPrefix(trimmed, "data: [DONE]"):
		r.endSegment(true)
		return
	case strings.HasPrefix(trimmed, "data: "):
		r.frame(strings.TrimPrefix(trimmed, "data: "))
	case trimmed != "":
		// 注释/心跳等非 data 行原样透传。必须**补一个空行**恢复帧边界：
		// 上游的 ": heartbeat\n" 后面跟着的是下一帧的 "data: ..."（本层吞掉了
		// 上游的分隔空行），不补的话 Responses/Messages 适配器按 "\n\n" 切帧
		// 会拿到 ": heartbeat\ndata: {...}" 粘连帧——没有 "data: " 前缀整帧
		// 被丢，恰好吞掉紧随其后的那个 content/tool_calls 增量（实测 GLM 的
		// 工具调用首帧带 id+name，被吞后整个调用消失、output 变 null）。
		r.out = append(r.out, line...)
		r.out = append(r.out, '\n')
		r.wrote = true
	}
	if err != nil {
		r.readErr = err // 含 io.EOF 与读错误：交给 endSegment 归因并决定续/降级/结束
		r.endSegment(false)
	}
}

// frame 处理一个 data 帧：解析状态（正文/推理/工具/usage），决定透传、吞掉或入 tail。
func (r *continueReader) frame(payload string) {
	// 「只采纳工具调用」段在裁决前一个字节都不给客户端：整段改道，不走下面的常规路径
	// （常规路径会透传 error 帧、非法 JSON，以及工具分片之后的正文）。
	if r.toolOnly {
		r.toolOnlyFrame(payload)
		return
	}
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		// 非法 JSON：原样透传（不解析状态，防抖）
		r.emit(payload)
		return
	}
	if _, hasErr := obj["error"]; hasErr {
		r.errSeen = true
		r.emit(payload)
		return
	}
	// 同型的「流内失败帧」：上游把安全策略拦截这类失败塞在顶层 {code,msg,displayMsg}、
	// 无 choices、无 error。若不认它，这帧会走完常规路径后什么都不命中（errSeen 不置位）
	// —— 于是 (a) 日志把它记成「段无 finish 结束（疑似上游断流）」，(b) 续写段还会白跑
	// 一次盲重试去撞同一个已被拦的账号。按显式失败处理：如实透传 + 终止续写。
	if code, msg, ok := upstream.StreamErrorFrame(obj); ok {
		log.Printf("WARN: [continue] acct=%s 流内失败帧 code=%d msg=%q → 按显式失败终止（不续写）",
			r.label(), code, msg)
		r.errSeen = true
		r.emit(payload)
		return
	}
	if id, ok := obj["id"].(string); ok && id != "" && r.id == "" {
		r.id = id
	}
	// usage 帧：消费入账（跨段合计），原始帧剥掉 usage 后再走常规路径——
	// 避免 usage 双发（本层末尾会合成一条总计帧），同时兼容与 finish/delta
	// 同帧的「bundled usage」形态（剥掉后内容照常透传）。
	hasUsage := false
	if u, ok := obj["usage"].(map[string]any); ok {
		r.mergeUsage(u)
		r.noteSegmentUsage(u)
		hasUsage = true
	}
	deltaNonEmpty := false
	rebuilt := false
	if chs, ok := obj["choices"].([]any); ok && len(chs) > 0 {
		if c, ok := chs[0].(map[string]any); ok {
			if d, ok := c["delta"].(map[string]any); ok {
				// 「只采纳工具调用」阶段的正文/思考增量：既不给客户端，也不进本地累积
				// （否则后续续写前缀里会混进被丢弃的内容）。
				if s, ok := d["reasoning_content"].(string); ok && s != "" {
					r.reason.WriteString(s)
					// 续写段思考的接缝策略（两个子情形）：
					//   - 正文尚未开始（r.text==0）：照常外泄。此时 responses 侧的
					//     reasoning item 还开着，续写思考只是往同一个 item 里续 delta，
					//     零协议风险；同时消掉「续写段纯思考期客户端零事件」的静默——
					//     codex 的 provider 带 stream_idle_timeout_ms，实测续写段静默
					//     约 122s 后客户端断开、请求记 interrupted（观测到的截断全是
					//     text=0B 的纯思考段，正是这条路径）。
					//   - 正文已开始（message item 开着）：剥离（照旧）。此时再喂思考
					//     responses 侧会开第二个 reasoning item，codex 后续文本 delta
					//     全报 "OutputTextDelta without active item"（实测 1.2 万条）。
					if r.seg > 0 && r.text.Len() > 0 {
						delete(d, "reasoning_content")
						rebuilt = true
					}
					deltaNonEmpty = true
				}
				if s, ok := d["content"].(string); ok && s != "" {
					r.text.WriteString(s)
					deltaNonEmpty = true
				}
				if tcs, ok := d["tool_calls"].([]any); ok && len(tcs) > 0 {
					r.toolSeen = true
					deltaNonEmpty = true
				}
			}
			if fr, ok := c["finish_reason"].(string); ok && fr != "" {
				r.finish = fr
			}
		}
	}
	if hasUsage {
		delete(obj, "usage")
		rebuilt = true
	}
	if rebuilt {
		if raw, err := json.Marshal(obj); err == nil {
			payload = string(raw)
		}
	}
	// 空帧（各字段皆空的 no-op 帧、且非终态）→ 吞掉；这是残余的静默源，
	// 按需补 keepalive（chat 路径注释帧直达；codex 路径的续写思考帧已改为
	// 直通，见上面 reasoning 分支，不再走这里）。
	if r.seg > 0 && !deltaNonEmpty && r.finish == "" {
		r.maybeKeepalive()
		return
	}
	if r.finish != "" {
		// finish 已见：后续帧全部入 tail（续写成功则丢弃；否则原样回放）。
		r.tail = append(r.tail, payload)
		return
	}
	if hasUsage && !deltaNonEmpty {
		return // 纯 usage 帧（已入账）：不再下发
	}
	r.emit(payload)
}

// noteSegmentUsage 记下当前段的 usage 快照（每段重置），供 starvedByThinking 判
// 「这一段的额度是不是被思考吃掉了」。上游两个字段都给过同一数值：
// 顶层 completion_thinking_tokens 与 completion_tokens_details.reasoning_tokens。
func (r *continueReader) noteSegmentUsage(u map[string]any) {
	if v, ok := u["completion_tokens"].(float64); ok {
		r.segCompletion = v
	}
	if v, ok := u["completion_thinking_tokens"].(float64); ok {
		r.segThink = v
		return
	}
	if d, ok := u["completion_tokens_details"].(map[string]any); ok {
		if v, ok := d["reasoning_tokens"].(float64); ok {
			r.segThink = v
		}
	}
}

// starvedByThinking 本段是否「思考吃掉预算」：思考 token 占本段输出 ≥
// continueThinkingShare，**且已经写出正文**。判据只看本段（见 segCompletion 注释）。
//
// 为什么要正文非空：纯思考、零正文的形态由 tryStarvedRetry 的「关思考重发」处理，
// 那条路径把预算从思考手里拿回来重写一遍；而这里若也放行，续写请求的 assistant
// 前缀是「空 content + 一堆 reasoning_content」，模型只会接着想，既写不出答案
// 又白烧一次额度（线上 starved 救援后仍零正文的那批就是这种）。
func (r *continueReader) starvedByThinking() bool {
	if r.text.Len() == 0 {
		return false
	}
	if r.segCompletion <= 0 || r.segThink <= 0 {
		return false
	}
	return r.segThink >= continueThinkingShare*r.segCompletion
}

// budgetWasRaised 客户端原本给的额度是否低于本轮下限（即网关抬过额度）。
//
// 用途：抬过额度时，真正生效的上限是网关选的数，不是客户端意图——被它切断就该续写，
// 否则只是把断点从客户端的 1024 挪到 8192，长回答照样停在半句。
func (r *continueReader) budgetWasRaised() bool {
	if r.budgetFloor <= 0 {
		return false
	}
	v, ok := clientOutputBudget(r.body)
	return ok && v > 0 && v < r.budgetFloor
}

// clientOutputBudget 读客户端原始请求体里的输出额度（三个别名）。
func clientOutputBudget(body []byte) (int64, bool) {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return 0, false
	}
	for _, k := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		switch v := obj[k].(type) {
		case float64:
			return int64(v), true
		case int:
			return int64(v), true
		case int64:
			return v, true
		}
	}
	return 0, false
}

// billedUsage 本请求各段 usage 的纯求和（记账口径，见 billed 字段），含还没结束的当前段；
// 没有任何 usage 时为 nil。
func (r *continueReader) billedUsage() map[string]any {
	if r.segUsage == nil {
		return r.billed
	}
	return sumUsage(sumUsage(nil, r.billed), r.segUsage)
}

// sumUsage 把 u 的数字叶子逐项加进 acc 并返回（acc 为 nil 时新建，不改 u）。
func sumUsage(acc, u map[string]any) map[string]any {
	if acc == nil {
		acc = map[string]any{}
	}
	if u != nil {
		sumInto(acc, u)
	}
	return acc
}

// flushSegUsage 段结束：把本段 usage 计入记账合计 billed 并清空。必须在任何换段之前调用。
func (r *continueReader) flushSegUsage() {
	if r.segUsage != nil {
		r.billed = sumUsage(r.billed, r.segUsage)
		r.segUsage = nil
	}
}

func (r *continueReader) emit(payload string) {
	r.lastClientAt = time.Now()
	r.wrote = true
	r.out = append(r.out, "data: "...)
	r.out = append(r.out, payload...)
	r.out = append(r.out, '\n', '\n')
}

// maybeKeepalive 被吞帧到达时调用：距上次客户端可见输出超过 keepaliveEvery 就
// 补一条 SSE 注释帧。注释行（":" 开头）是 SSE 规范的 no-op——StreamHint 原样
// 透传并 flush，responsesWriter 与 chat 类客户端的解析器都忽略它；唯一作用是
// 让连接保持「有数据流动」，防流静默超时掐线。
func (r *continueReader) maybeKeepalive() {
	if keepaliveEvery > 0 && time.Since(r.lastClientAt) < keepaliveEvery {
		return
	}
	r.out = append(r.out, ": keepalive\n\n"...)
	r.lastClientAt = time.Now()
	r.wrote = true
}

// endSegment 一段结束（DONE 或 EOF/读错误）时的决策：
// 能续 → 换段继续读；否则回放 tail + 合成 usage 总计 + 结束。
func (r *continueReader) endSegment(sawDone bool) {
	if r.done {
		return
	}
	if r.cur != nil {
		r.cur.Close()
	}
	// 本段 usage 计入记账合计：必须在任何换段（救援、续写、盲重试）之前。
	r.flushSegUsage()
	// 安全续写段的裁决：拿到工具调用就采纳（继续正常收尾），否则回滚到首段终态。
	r.settleToolOnly(sawDone)
	if r.tryStarvedRetry() {
		return
	}
	// 安全续写（只采纳工具调用）：模型刚看完工具结果却只写一句就收尾。
	if r.tryToolOnlyRescue() {
		return
	}
	// 预告式收尾（finish=stop + 正文停在「……：」上 + 工具类请求）：续写剩余内容。
	// 必须排在 canContinue 之前判断，但两者互斥（后者要求 finish=length 或断流）。
	if r.tryNarrationRetry() {
		return
	}
	if r.canContinue(sawDone) {
		// 必须是「断流盲重试」还是「length 截断续写」（r.finish 在续写成功后被重置）。
		blind := !sawDone && r.finish == ""
		body, err := r.continuationBody()
		if err == nil {
			rc, status, _, terr := r.up.ChatStreamContext(r.ctx, r.acct, body, r.clientIP, r.meta)
			if terr == nil && status < 400 && rc != nil {
				r.seg++
				if blind {
					r.blindRetried = true
				}
				r.cur, r.br = rc, bufio.NewReaderSize(rc, 64*1024)
				blindNote := ""
				if blind {
					blindNote = " (断流盲重试)"
				}
				starvedNote := ""
				if r.limitSet && r.starvedByThinking() {
					starvedNote = fmt.Sprintf(" (思考吃光预算 think=%d/%d，关思考续写)", int(r.segThink), int(r.segCompletion))
				} else if r.budgetWasRaised() {
					starvedNote = fmt.Sprintf(" (额度已被网关抬到下限 %d，续接被该上限切断的正文)", r.budgetFloor)
				}
				log.Printf("[continue] seg=%d model=%s text=%dB reason=%dB%s%s -> 续写",
					r.seg, bareModelOf(r.body), r.text.Len(), r.reason.Len(), blindNote, starvedNote)
				r.finish, r.tail = "", nil
				// 本段 usage 快照归零：下一段的思考/正文构成要重新测（见 segCompletion）。
				r.segCompletion, r.segThink = 0, 0
				return
			}
			if rc != nil {
				rc.Close()
			}
			log.Printf("WARN: [continue] acct=%s seg=%d 续写失败（status=%d err=%v）→ 降级为截断终态",
				r.label(), r.seg+1, status, terr)
		} else {
			log.Printf("WARN: [continue] acct=%s 续写请求体构造失败: %v",
				r.label(), err)
		}
	} else if r.continueCapped {
		// 撞的是网关自己的累计输出上限，不是上游故障——写清楚，免得排障时
		// 把它当成「又在半句处断了」。
		log.Printf("INFO: [continue] acct=%s 累计输出 %d token 已达上限（下限 %d × %d）→ 停止续写，按截断收尾",
			r.label(), int(r.totalOutput()), r.budgetFloor, continueTotalCapRatio)
	}
	// 段在没有任何 finish 的情况下以 EOF/读错误结束：疑似上游断流（正常收尾都带
	// finish_reason）。客户端此前会看到「被标 completed 的中途截断」——这里把
	// 本层早就知道的事实落到客户端可见的协议字段里（本文件开头那条契约：上层看到
	// 的永远是「一条完整且恰好一个终态」的流；此前只记日志、没落帧）。
	if r.cutNoFinish(sawDone) {
		// 归因分流：读错误是「客户端先断开」时，上游只是被动跟着断——报成「疑似
		// 上游断流」会把排障引向错误方向（实测就踩过：189s 那条被当成上游故障，
		// 实际是客户端 189s 后放弃、上游随之 cancel）。客户端断开也不补截断终态：
		// 连接已经没人读了，补帧没意义。真正的上游断流才降级为截断终态。
		if r.clientGone() {
			log.Printf("INFO: [continue] acct=%s 客户端先断开，段随之结束（非上游故障，不补截断终态）",
				r.label())
		} else {
			log.Printf("WARN: [continue] acct=%s 段无 finish 结束（疑似上游断流，按截断终态降级）",
				r.label())
			// 有真实输出才补终态：零输出段不能补——否则 StreamHint 的 validEvents 从 0
			// 变 1，空流本该收敛的 502 观测会被伪装成「有内容的 200」。
			if r.text.Len() > 0 || r.reason.Len() > 0 || r.toolSeen {
				r.emitTruncationTerminal()
			}
		}
	}
	// 不续/续写失败：回放 tail（含 finish/末端帧，客户端行为与未接线时一致），
	// 再补一条跨段合计 usage，最后 EOF（[DONE] 由 StreamHint 统一写出，恰好一条）。
	for _, f := range r.tail {
		r.emit(f)
	}
	r.tail = nil
	if r.usage != nil {
		raw, _ := json.Marshal(map[string]any{"id": r.id, "object": "chat.completion.chunk", "choices": []any{}, "usage": r.usage})
		r.emit(string(raw))
	}
	// 末段上游显式发了 [DONE]（正常收尾）就把它交给下游：StreamHint 只在「没见到 [DONE] 也
	// 没见到终态」时补截断终态。中间各段的 [DONE] 照旧吞掉（续写接缝），唯独末段这一条是
	// 「上游说完了」的事实——吞掉它，下游会把每条「没带 finish_reason 的正常流」都当成断流。
	if sawDone {
		r.out = append(r.out, "data: [DONE]\n\n"...)
	}
	// 救援效果的可观测性：本次命中的是「预告句收尾」时，记录续写后到底拿到了什么。
	// 没有这行就分不清「救回来是工具调用（回合继续）」和「只是多打了一段正文」。
	if r.narrationTried {
		log.Printf("[narration] acct=%s 结果：共 %d 段，工具调用=%v，正文合计=%dB",
			r.label(), r.seg+1, r.toolSeen, r.text.Len())
	}
	r.done = true
	// 读错误冒泡（本仓特有，上游没有这条）：一个字节都没写出去时，这次尝试对客户端
	// 完全不可见——上层（StreamHint 的首帧否决 / handler 的换号）还能拿真实错误去
	// 换号重试，比在这里伪装成「正常结束的空流」有用得多。写过字节则只能就地降级。
	if r.pendingErr == nil && r.readErr != nil && !errors.Is(r.readErr, io.EOF) && !r.wrote {
		r.pendingErr = r.readErr
	}
}

// clientGone 报告本段结束是否由「客户端先断开」引起：只看客户端自己的请求 ctx。
//
// 不能看读错误是不是 context.Canceled：上游空闲看门狗（upstream/idle.go）静默超时
// 时 cancel 的是派生的上游请求 ctx，读错误同样是 context canceled，可客户端还在等——
// 那是上游断流，该按截断降级、照常盲重试。客户端真断开时 r.ctx 必然已取消，单看它就够。
func (r *continueReader) clientGone() bool {
	return r.ctx != nil && r.ctx.Err() != nil
}

// cutNoFinish 本段是否「没有任何正常收尾就结束」（上游断流）：既没给 finish_reason、
// 也没发 data: [DONE]，且不是流内 error 帧（那是上游明说失败，客户端已收到 error 帧，
// 不能再补一条截断终态与之矛盾）。三处共用同一判据：续写资格、终态降级、WARN 日志。
func (r *continueReader) cutNoFinish(sawDone bool) bool {
	return !sawDone && r.finish == "" && !r.errSeen
}

// emitTruncationTerminal 补一条截断终态帧（finish_reason=length）。
//
// 本层是三条适配器（chat / responses / messages）共用的唯一接缝，所以补在这一处就能
// 让三边同时如实标截断——它们各自早已把 length 映射正确（responses →
// response.incomplete + max_output_tokens，messages → stop_reason=max_tokens），此前
// 缺的只是「这段没正常收尾」这个事实没被说出来。若在三个适配器各写一份，既重复又
// 迟早漂移。取值口径见 upstream.TruncationFinishReason。
//
// 位置：必须在 tail 回放之后、usage 帧之前（OpenAI 惯例：终态帧在前，纯 usage 帧收尾）。
// 断流分支下 tail 必为空（tail 只在见到 finish 后填充），故顺序天然正确。
func (r *continueReader) emitTruncationTerminal() {
	raw, err := json.Marshal(map[string]any{
		"id":     r.id,
		"object": "chat.completion.chunk",
		"model":  bareModelOf(r.body),
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": upstream.TruncationFinishReason,
		}},
	})
	if err == nil {
		r.emit(string(raw))
	}
}

// totalOutput 累计输出 token（跨段求和；mergeUsage 已按段累加 completion_tokens）。
// 段内还没有 usage 时按 0 算——续写资格主要看 finish=length，不靠这个数。
func (r *continueReader) totalOutput() float64 {
	if f, ok := r.usage["completion_tokens"].(float64); ok {
		return f
	}
	return 0
}

// outputCapped 累计输出是否已达本请求上限（下限 × continueTotalCapRatio，兜底
// defaultContinueTotalCap）。没算下限时（客户端没指定档位）用兜底值。
func (r *continueReader) outputCapped() bool {
	capTokens := float64(defaultContinueTotalCap)
	if r.budgetFloor > 0 {
		capTokens = float64(r.budgetFloor * continueTotalCapRatio)
	}
	return r.totalOutput() >= capTokens
}

// canContinue 续写资格判定（sawDone=false 表示 EOF/读错误收尾——只有显式
// finish_reason=length 才续，读错误不续，避免把故障流当截断流续错）。
func (r *continueReader) canContinue(sawDone bool) bool {
	switch {
	case r.seg >= maxContinueSegments-1:
		return false
	case r.outputCapped():
		// 累计输出到顶：再续就是无节制地烧配额了。标出来，让终态归因能区分
		// 「撞网关自己的上限」与「上游断流」。
		r.continueCapped = true
		return false
	case r.clientGone():
		// 客户端已断连：ctx 取消后重试请求发不出去，只会白烧一次上游额度。
		// 实测就踩过（15:31:24 那条 seg=1 续写失败 context canceled）——门禁
		// 在发请求前就拦掉，别让日志里出现一次注定失败的续写。
		return false
	case r.errSeen:
		return false
	case r.limitSet && r.limit < clientLimitLongAnswer && !r.starvedByThinking() && !r.budgetWasRaised():
		// 客户端显式带限额 = 它只要这么多，截断是预期语义（Claude 客户端必带
		// max_tokens，每回合 8192）。两种例外都续写：
		//   1) 额度被思考吃光（见 continueThinkingShare）——客户端要的那份回答没写出来；
		//   2) 网关把这个额度抬到过下限（budgetWasRaised）——此时生效的上限是**网关**
		//      选的数，不是客户端的意图，被它切断就该接着写（否则「抬了额度还是断」
		//      只是把断点从 1024 挪到 8192，问题没真正解决）；
		//   3) 额度 ≥ clientLimitLongAnswer——这一档额度不代表「只要这么短」（见该常量）。
		return false
	case r.cutNoFinish(sawDone):
		// 段无 finish 结束 = 上游偶发断流（线上实测：15 次续写里 1 次）。已产出内容时
		// 允许一次盲重试：重试请求体把已收内容作为 assistant 前缀，幂等不重复。
		// 主段同样给（此前只给 seg>0）——主段断流不是「请求本身失败」，它已经产出过
		// 输出，把它交给截断终态正是「Codex 每次莫名中断、要手动继续」的主因（线上
		// 实测 2 天内 27 次）。零输出段不给：没有可续的东西，且
		// 客户端已断连时 ctx 已取消、重试发不出去，不会白烧额度。
		// 含工具分片同样不给（与下面 length 分支同一条理由）：残缺 tool_calls 不进
		// 重试前缀，模型会重发一遍，客户端在同一块里拼两次参数 → 半截 JSON 变垃圾。
		return (r.seg > 0 || r.text.Len() > 0 || r.reason.Len() > 0) && !r.toolSeen && !r.blindRetried
	case r.finish != "length":
		return false
	case r.toolSeen:
		log.Printf("[continue] 跳过：本段含工具调用分片（参数可能残缺，拼接风险高）")
		return false
	case r.text.Len() == 0 && r.reason.Len() == 0:
		return false
	}
	return true
}

// continuationBody 构造续写请求：原请求 messages + assistant（已输出内容）+ 续写指令。
// 复用原请求其余全部字段（model/tools/采样参数等）；max_tokens 类字段已在
// canContinue 阶段排除。prepareBody/thinking/缓存键等由 ChatStreamContext 内部统一套用。
func (r *continueReader) continuationBody() ([]byte, error) {
	return r.continuationBodyWithNudge(continueNudge)
}

// continuationBodyWithNudge 同上，续写指令可替换（预告式收尾的救援用另一条指令，
// 见 narrationNudge：那条要说明「你停在一句预告上」，否则模型会以为已经说完）。
func (r *continueReader) continuationBodyWithNudge(nudge string) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(r.body, &obj); err != nil {
		return nil, err
	}
	msgs, _ := obj["messages"].([]any)
	asst := map[string]any{"role": "assistant"}
	if t := r.text.String(); t != "" {
		asst["content"] = t
	} else {
		asst["content"] = ""
	}
	if rc := r.reason.String(); rc != "" {
		asst["reasoning_content"] = rc
	}
	msgs = append(msgs, asst, map[string]any{"role": "user", "content": nudge})
	obj["messages"] = msgs
	// 续写段至少带上本轮档位的输出预算下限：这一层是从原始 body 重建的，拿不到首段
	// 被 upstream 抬过的 max_tokens（见 newContinueReaderWithFloor 的说明）。
	upstream.EnsureMinBudget(obj, r.budgetFloor)
	// 思考吃光预算的续写段：把思考关掉再续。否则续写段拿同一份 max_tokens、
	// 以同一思路再烧一遍思考，三个段合计还是只写出半句（实测每段正文 ~580 token）。
	// 关思考后整份额度留给正文，一次续写就能收尾（与 starved.go 的救援同一手法：
	// 不放大客户端额度，只把预算从思考手里拿回来）。
	// 额度被网关抬过（budgetWasRaised）同样关思考：这一档的思考本身就要吃掉大半
	// 额度，继续开着只会让第二段卡在同一个上限上。
	if r.limitSet && (r.starvedByThinking() || r.budgetWasRaised()) {
		model, _ := obj["model"].(string)
		if _, has := obj["thinking"].(map[string]any); has || upstream.ThinkingSwitchSupported(model) {
			obj["thinking"] = map[string]any{"type": "disabled"}
			// disabled 与 effort 不共存（thinking.go 的 disabled 分支要求两者不共存：
			// 上游对「关思考 + 指定档位」的语义未取证）。
			delete(obj, "reasoning_effort")
			delete(obj, "reasoningEffort")
		}
	}
	return json.Marshal(obj)
}

// mergeUsage 跨段合并 usage。输出/计费侧数字叶子求和（completion、credit 等
// 跨段真实累加）；prompt 侧取末段值——续写段的 prompt 天然包含整个上下文，
// 求和会把上下文重复计入 N 段倍，客户端按 usage 判断上下文占用时会被虚高
// 值误触发压缩（与真实上下文长度无关地提前全量压缩）。total_tokens 按
// 「末段 prompt + 累计 completion」重算，保持口径自洽。
func (r *continueReader) mergeUsage(u map[string]any) {
	r.usage = mergeUsageInto(r.usage, u)
	// 记账口径的段内合并与客户端可见的合计同一规则（段内多帧 usage 时与改动前记的一样），
	// 段结束时再按段求和进 billed（见 flushSegUsage）。
	r.segUsage = mergeUsageInto(r.segUsage, u)
}

// mergeUsageInto 把一个 usage 帧并入 dst 并返回（dst 为 nil 时新建）：prompt 侧取最新值，
// 其余数字叶子求和，total_tokens 按 prompt+completion 重算（口径见 mergeUsage）。
func mergeUsageInto(dst, u map[string]any) map[string]any {
	if dst == nil {
		dst = map[string]any{}
	}
	for k, v := range u {
		switch k {
		case "prompt_tokens", "prompt_cache_hit_tokens", "prompt_cache_miss_tokens", "prompt_tokens_details":
			dst[k] = v // 末段值
		case "total_tokens":
			// 先按末段留作兜底（段里没有 prompt_tokens 时不会重算），末尾统一
			// 按 prompt(末段)+completion(累计) 覆盖。
			dst[k] = v
		default:
			if f, ok := v.(float64); ok {
				if d, ok2 := dst[k].(float64); ok2 {
					dst[k] = d + f
					continue
				}
				dst[k] = f
				continue
			}
			if m, ok := v.(map[string]any); ok {
				if dm, ok2 := dst[k].(map[string]any); ok2 {
					sumInto(dm, m)
				} else {
					nm := map[string]any{}
					sumInto(nm, m)
					dst[k] = nm
				}
				continue
			}
			dst[k] = v
		}
	}
	if p, ok := dst["prompt_tokens"].(float64); ok {
		c, _ := dst["completion_tokens"].(float64)
		dst["total_tokens"] = p + c
	}
	return dst
}

func sumInto(dst, src map[string]any) {
	for k, v := range src {
		switch sv := v.(type) {
		case float64:
			if dv, ok := dst[k].(float64); ok {
				dst[k] = dv + sv
			} else {
				dst[k] = sv
			}
		case map[string]any:
			if dm, ok := dst[k].(map[string]any); ok {
				sumInto(dm, sv)
			} else {
				nm := map[string]any{}
				sumInto(nm, sv)
				dst[k] = nm
			}
		default:
			dst[k] = v
		}
	}
}

// bareModelOf 从请求体取裸模型名（续写日志与合成帧的 model 字段）。
// 请求体可能带 cn:/global: 路由前缀（handler 不提前改写，出站由 prepareBody 剥），
// 这里必须同样剥掉，否则客户端会在续写帧里看到前缀。
func bareModelOf(body []byte) string {
	var obj struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(bytes.TrimSpace(body), &obj) != nil {
		return "?"
	}
	_, bare := resolveModel(obj.Model)
	return bare
}
