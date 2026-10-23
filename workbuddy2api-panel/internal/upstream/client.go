// Package upstream 封装对 CodeBuddy 上游（chat / billing / auth）的全部 HTTP 调用，
// 以及错误分类（驱动 pool 冷却状态机）。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/cst"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// ErrKind 错误分类，pool 据此决定冷却时长。
type ErrKind int

const (
	ErrNone           ErrKind = iota // 成功
	ErrHardCredit                    // 余额不足（402 或 body 关键词）→ 长冷却
	ErrSoftRate                      // 429 软限流 → 短冷却
	ErrSessionDead                   // 401 + 12153 offline session 失效 → 禁用
	ErrNotFound                      // 404 上游偶发 → 短冷却，不累计错误计数（防雪崩）
	ErrServer                        // 5xx 上游故障
	ErrContentBlocked                // 内容策略拦截（400 + 审核文案）→ 不罚账号，走降级重试
	ErrBadParams                     // 请求体解析失败（400 + Unmarshal chat params failed / 11101）→ 请求级错误：不罚号、不轮转，末端 400 透传原文
	ErrAccountFault                  // 账号级授权/配额故障（11140 request illegal / 14017 trial not activated）→ 冷却轮换，不无限重试
	ErrModelBlocked                  // 11102「该后端无此模型」→ (账号,模型) 负缓存避让，切模型/切账号
	ErrWafBlock                      // 403 + 非业务信封体（APISIX WAF 拦截页/空体）→ 账号软冷却 + 抖动退避
	ErrPromptTooLong                 // 11115「prompt is too long」→ 请求级错误（上下文超限是请求的问题非账号的问题）：不罚号、不轮转，末端透传原文
	ErrImageInvalid                  // 图片请求格式/数据无效 → 请求级错误：不罚号、不轮转，末端透传原文
	ErrUpstreamBusy                  // 6021 queue.full 渠道级容量满载 → 不罚号、不轮转，末端 503 upstream_busy
	ErrChannelRefused                // 11‑-128 request illegal 通道校验失败（上游认为非官方客户端）→ 不罚账号
	ErrModelBusy                     // 14003「模型繁忙」（间歇）→ (账号,模型) 10s 起短冷却，封顶 1m
	ErrClient                        // 其他 4xx / 业务错误
	ErrToolMismatch                  // 11148「tool calls and tool results do not match」→ 请求内容级配对断裂：不罚号、不轮转，末端 400 透传原文
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardCredit:
		return "hard_credit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrContentBlocked:
		return "content_blocked"
	case ErrBadParams:
		return "bad_params"
	case ErrModelBlocked:
		return "model_blocked"
	case ErrWafBlock:
		return "waf_block"
	case ErrPromptTooLong:
		return "prompt_too_long"
	case ErrUpstreamBusy:
		return "upstream_busy"
	case ErrToolMismatch:
		return "tool_mismatch"
	case ErrChannelRefused:
		return "channel_refused"
	case ErrModelBusy:
		return "model_busy"
	case ErrImageInvalid:
		return "image_invalid"
	case ErrAccountFault:
		return "account_fault"
	case ErrClient:
		return "client"
	default:
		return "none"
	}
}

// Error 带分类的上游错误。
type Error struct {
	Kind ErrKind
	// Status 语义状态码：HTTP 错误取真实状态；HTTP 200 + 业务 code 时取
	// businessCodeStatus 合成值（BUG-4）—— 下游按 Status 分支的逻辑
	// （ClaimRewardMP 的 400 降级、IsBuddyTaskIncomplete）依赖它。
	Status int
	// HTTPStatus 上游真实 HTTP 状态（日志/排障用，不参与分支判断）。
	HTTPStatus int
	Msg        string
	// RetryAfter 上游明示的等待时长（Retry-After 秒 / retry-after-ms /
	// x-ratelimit-reset 头解析，见 ParseRetryAfter）。零值 = 上游未明示，
	// 冷却时长回落调用方计算值。挂载点选在 Error 信封：Kind 决定「罚不罚」，
	// RetryAfter 决定「罚多久」，同为上游响应的一等公民。
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// hardMarkers 余额不足关键词（小写比较 + 中文原文比较双通道）。
var hardMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "credits exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}

// softRateMarkers 限流/节流关键词（小写比较 + 中文原文比较双通道）。
// 上游在状态码非 429 时也会返回限流语义（如 200 + code 11140
// "The model provider is rate-limiting requests."、400 + "rate limit"），
// 此类响应若不识别，账号既不被冷却也不喂熔断，下次请求仍会被选中（issue #28）。
//
// 词表按子串匹配，宁缺毋滥：只收录明确指向「请求速率/模型用量被节流」的措辞。
// 连字符形式（rate-limiting / rate-limited）需单列——Contains 不跨 '-'。
// "too many" 会命中 "too many tokens" 这类客户端参数错误，代价是该号被软冷却
// 一个 SoftCooldown（默认 60s）后自愈，远小于漏判限流导致反复选中同一号的代价。
var softRateMarkers = []string{
	"rate limit", // rate limit / rate limits / rate limiting
	"rate-limiting",
	"rate-limited",
	"too many requests",
	"too many",
	"usage limit", // usage limit reached / model usage limit exceeded（用量节流，非计费余额）
	"请求过于频繁", "限流",
}

var sessionDeadMarkers = []string{"Offline user session not found", "12153"}

// accountFaultMarkers 账号级授权/配额故障关键词（大小写不敏感子串匹配）。
//
// 定位：这类错误是**账号本身状态**决定的本机故障，不是请求格式、不是临时限流、
// 也不是内容误报——继续重试只会反复刷上游风控/配额检查，必须把该账号冷却轮换。
//   - "request illegal"（code 11140）→ 上游 auth/auth_forbidden，账号级授权风控，
//     需重新 OAuth 登录才能恢复，短冷却只能阻止继续送死。
//   - code 14017（"trial not activated" / "The trial version is not yet activated"）→
//     上游 quota/quota_not_activated，register 未完成的试用未激活账号，同样账号级。
//
// 注意 11140 **不能**按 code 判定：该 code 也承载模型级限流文案（"The model provider
// is rate-limiting requests."），那种场景必须保持 ErrSoftRate（下方 softRateMarkers
// 后判定）。故此处只收 msg 关键词 "request illegal"（auth_forbidden 的真实文案）。
// 14017 文案唯一（无软限流歧义），可安全收录。
var accountFaultMarkers = []string{
	"request illegal",
	"trial not activated",
	"trial version is not yet activated",
}

// contentBlockedMarkers 内容策略拦截关键词（大小写不敏感子串匹配）。
//
// 定位：上游按逐字精确指纹审核，system 来源的模板句（如 Claude Code/Codex
// 注入指令）触发 HTTP 400 + 以下文案。这是「误报」（合法流量被审核误杀），
// 非账号问题——该账号余额健康、未限流、session 未死，故 ErrContentBlocked
// 在 applyErrorPolicy 中不罚账号（无冷却/熔断/NoteError），改由网关降级重试。
var contentBlockedMarkers = []string{
	"blocked by security policy",
	"unapproved channel",
	"illegal api invocation",
}

// badParamsMarkers 请求体解析失败关键词（issue #41 连带）：HTTP 400 + 上游
// "Unmarshal chat params failed..."（code 11101）。这是"发给上游的 body 有问题"，
// 与账号健康无关——不罚号，但仍轮转（commit B）。
var badParamsMarkerMsg = "Unmarshal chat params failed"

// invalidImageMarkers 图片请求格式/数据无效（HTTP 400）的**文案**形态。这类错误由
// 请求内容决定，不是账号问题：换账号不会改变同一 body 的解析结果。上游常见形态包括
// `Parse message failed: invalid image_url content`、invalid_image_data、
// `replace the image`。
//
// 业务码 11135 不放在这里：code 判定必须容忍 JSON 空白（`"code": 11135`），
// 字面量 marker 只能覆盖紧凑形态，故统一走 codeMarker（见 Classify 的 400 分支，
// 与 hint.go 的 isInvalidImageData 同口径；上游 5d5223d 的 Copilot review 修复）。
var invalidImageMarkers = []string{
	"invalid image_url content",
	"invalid_image_data",
	"replace the image",
}

// 定位：上下文超限是**请求的问题不是账号的问题**——同一个 body 换任何账号发都会
// 超限，与 WAF fail-fast 同哲学（确定与账号无关的错误不罚号不轮转，白白浪费健康号
// 的请求配额）。marker 双通道：
//   - `"code":11115`：业务信封 code 字段（JSON 空格容差；`"code":"11115"` 字符串
//     形态也命中）；
//   - "prompt is too long"：msg 文案（大小写不敏感）。
//
// 只在 400/404/413 请求级状态码上判（429+11115 概率极低且属限流语义优先，
// 5xx 属服务端故障优先）。误判代价（好 body 被归 prompt_too_long）：不罚号 +
// 不轮转 + 透传原文，客户端看到上游原文可自行排查，代价可控。
var promptTooLongMarkers = []string{
	`"code":11115`,
	`"code": 11115`,
	`"code":"11115"`,
	"prompt is too long",
}

// isPromptTooLongStatus 11115 只在请求级 4xx 上判（见 promptTooLongMarkers 注释）。
func isPromptTooLongStatus(status int) bool {
	return status == http.StatusBadRequest || status == http.StatusNotFound ||
		status == http.StatusRequestEntityTooLarge
}

// alreadyCheckinMarkers "今天已签到"关键词（上游对重复签到返回 code!=0，
// 实测 code=10001/14001 "今天已签到"/"今日已签到"）。只对 *Error.Msg 做包含匹配，
// 网络层/解析层错误不在此识别（见 IsAlreadyCheckin）。
// 只认明确的幂等文案（bug B4）：此前含裸 "already"，而它是极宽的子串 ——
// "already used" / "already exists" / "already claimed" / "quota already exhausted"
// 全部命中。IsAlreadyCheckin 只对 *Error.Msg 做包含匹配，于是任何带 already 的业务
// 错误都被当成「今天已签到」：面板显示 checkin_done=true、调度器日志写「今天已签到
// （幂等）」，而账号当天实际没签上 —— 用户以为签了，当天漏签。
var alreadyCheckinMarkers = []string{
	"已签到", "今日已签到", "今天已签到",
	"already checked in", "already check-in", "already signed in",
	"already checkin", "already checked-in",
}

// alreadyNegativeMarkers 命中这些就**不是**幂等成功（防御性：万一上游文案组合出
// 「already used/exists/claimed/exhausted」这类语义完全相反的错误）。
var alreadyNegativeMarkers = []string{
	"used", "exist", "claim", "exhaust", "expired", "invalid", "quota",
}
var badParamsMarkerCode = `"code":11101`

// softRateResetLoc 上游 429 6004 文案中的重置时间固定按 UTC+8 解释（上游文案如此，
// 与容器时区无关）。
var softRateResetLoc = cst.Loc

// SoftRateResetLoc 暴露重置时间的固定时区（供测试构造/断言同一时区口径）。
func SoftRateResetLoc() *time.Location { return softRateResetLoc }

// modelRateLimitCode 明确指向「模型级 429 限流」的业务 code。
// 上游用它表达"该模型的使用量超限"（code 6004，msg 带「将在 … 重置」），
// 而不是账号整体被限流——账号健康，只是这个模型此刻被限（issue #31）。
const modelRateLimitCode = "6004"

// softRateResetPatternCN/EN 匹配重置文案（CN「将在 … 重置」/ global 域英文
// "reset at <固定格式时间>"），捕获中间的时间串。
const softRateResetPatternCN = `将在 (.+?) 重置`
const softRateResetPatternEN = `(?i)reset at (\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})`

// 限流判定正则预编译为包级 var：IsModelRateLimit / ParseRateReset 在每次错误
// 分类、每个限流 body 上调用，函数体内 MustCompile 是纯浪费；错误风暴（429
// 轰炸）时尤甚。模式串均为纯常量。regexp 并发安全（匹配只读），无需额外锁。
var (
	reModelRateLimit  = regexp.MustCompile(`"code"\s*:\s*"?` + modelRateLimitCode + `"?`)
	reSoftRateResetCN = regexp.MustCompile(softRateResetPatternCN)
	reSoftRateResetEN = regexp.MustCompile(softRateResetPatternEN)
)

// softRateTimeLayout 上游重置时间的格式（无时区后缀；时区固定 UTC+8）。
const softRateTimeLayout = "2006-01-02 15:04:05"

// IsModelRateLimit 报告 429 body 是否明确指向模型级限流（业务 code 6004）。
// 用于区分"账号级软限流"（按账号冷却）与"模型级用量限流"（切模型即可用）。
func IsModelRateLimit(body string) bool {
	// `"code":6004` / `"code": 6004` / `"code":"6004"` 均可命中（JSON 空格容差）。
	return reModelRateLimit.MatchString(body)
}

// modelBlockCode 明确指向「该后端无此模型」的业务 code。
const modelBlockCode = "11102"

// modelBlockMsgMarker 11102 答复的确定性文案（官方 error message 固定短语）。
// 只收这个窄短语，不收 "model ... not found" 宽正则——后者会误伤其他业务的
// not found 措辞。
const modelBlockMsgMarker = "service info not found"

// ModelBlockReason 11102 负缓存条目在 pool.modelCooldowns 里的 reason 前缀。
// handler 写 BlockModelBackoff；pool.BlockModelClear 按 "11102" 前缀识别条目
// （与 6004 条目的 "6004 model rate limit" reason 互不干扰）。
const ModelBlockReason = "11102 model not available"

// ModelNotAllowedReason 11158「该模型不允许当前账号类型」负缓存条目的 reason 前缀。
// 与 11102 分开记：11102 是"模型不存在"，11158 是"账号等级不够"——后者换号可能有用、
// 换模型一定有用，给客户端的文案与处置都不同（见 handler 末端 model_unavailable 分支）。
const ModelNotAllowedReason = "11158 model not allowed for account type"

// IsModelBlocked 报告 body 是否是「该后端无此模型」(11102) 的确定性答复。
//
// 只比对 code/msg 等独立字段，绝不做整段文本子串匹配：错误体还带 requestId 等字段，
// 拿整段文本匹配会把 "11102" 撞在 ID 上、误避让一个本来能用的模型。判定 =
// code 字段精确等于 "11102"，或 msg/message 字段命中窄短语 "service info not
// found"（两者任一命中即真）。只看 400/404：429 带 11102 属限流语义。
// 字段遍历覆盖顶层与 error 子对象两层。
func IsModelBlocked(status int, body string) bool {
	if (status != http.StatusBadRequest && status != http.StatusNotFound) || body == "" {
		return false
	}
	// 轻量预检：body 既无 "11102" 又无 marker 时直接短路（大多数 4xx 零分配返回）。
	if !strings.Contains(body, modelBlockCode) && !strings.Contains(strings.ToLower(body), modelBlockMsgMarker) {
		return false
	}
	var root map[string]any
	if err := json.Unmarshal([]byte(body), &root); err != nil {
		return false
	}
	nodes := []map[string]any{root}
	if inner, ok := root["error"].(map[string]any); ok {
		nodes = append(nodes, inner)
	}
	code, msg := "", ""
	for _, node := range nodes {
		for _, key := range []string{"code", "errCode", "error_code"} {
			if v, ok := node[key]; ok && v != nil && code == "" {
				code = strings.TrimSpace(fmt.Sprint(v))
			}
		}
		for _, key := range []string{"msg", "message"} {
			if v, ok := node[key].(string); ok && v != "" && msg == "" {
				msg = strings.TrimSpace(v)
			}
		}
	}
	if code == modelBlockCode {
		return true
	}
	return strings.Contains(strings.ToLower(msg), modelBlockMsgMarker)
}

// hasBusinessCode reports whether a JSON error envelope contains an exact
// business code in a field named "code". Upstream envelopes vary between
// top-level and nested error/data objects, so walk the decoded structure.
// normalizeBusinessCode 只保留数字。
//
// 上游这条错误路径实际发的是 `{"code":11‑-128,...}` —— **数字里带连字符，
// 不是合法 JSON**；日志/终端又常把它渲染成 `11‑-128` 或 `"11‑-128"`。
// 三者必须归一到同一个键，否则按字面比较永远不命中（我们 v1.31.0 的
// 通道校验检测就是这样全程失效的）。
func normalizeBusinessCode(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// reBusinessCode 从原文抓 code 字段（允许带引号、允许数字里含连字符）。
// 只锚定 `"code":` 前缀，requestId 等十六进制串不会误命中。
// 字符类含 ASCII 连字符与 U+2011 非断连字符（上游真实报文用的是后者）。
var reBusinessCode = regexp.MustCompile(`(?i)"code"\s*:\s*"?([0-9][0-9\x{2011}-]*)"?`)

// hasBusinessCode 报告报文里是否有指定的业务码。
// 先按 JSON 结构走（兼容嵌套 error/data 对象），解析失败再按原文正则兜底 ——
// 真实报文（及被截断的日志行）常常不是合法 JSON。
func hasBusinessCode(body, want string) bool {
	want = normalizeBusinessCode(want)
	if want == "" {
		return false
	}
	var root any
	if err := json.Unmarshal([]byte(body), &root); err == nil {
		var walk func(any) bool
		walk = func(value any) bool {
			switch node := value.(type) {
			case map[string]any:
				if code, ok := node["code"]; ok && normalizeBusinessCode(fmt.Sprint(code)) == want {
					return true
				}
				for _, child := range node {
					if walk(child) {
						return true
					}
				}
			case []any:
				for _, child := range node {
					if walk(child) {
						return true
					}
				}
			}
			return false
		}
		if walk(root) {
			return true
		}
	}
	for _, m := range reBusinessCode.FindAllStringSubmatch(body, -1) {
		if normalizeBusinessCode(m[1]) == want {
			return true
		}
	}
	return false
}

// hasBusinessEnvelope 报告错误 body 是否携带上游业务信封形态（JSON 且含
// `"code":` 或 `"msg":` 字段）。WAF 403 判定（IsWafBlocked）用「无业务信封」
// 区分 APISIX WAF 拦截页（HTML/空体/纯文本）与上游业务层 403（带 code/msg
// 信封，正常走既有分类）。JSON 解析不做：信封存在性只需字段名命中——
// 畸形 JSON 但含 `"msg":` 字样仍按业务响应保守处理（宁漏判 WAF 也不误罚
// 业务 403，后者有各自的权威分类）。
func hasBusinessEnvelope(body string) bool {
	// 只认字段名、不认紧邻冒号（BUG-17）：`"code" :` 这种带空格的写法会被
	// 判成「无业务信封」→ 403 走 WAF 的 IP 级 fail-fast，而不是业务分类。
	return strings.Contains(body, `"code"`) || strings.Contains(body, `"msg"`)
}

// IsGatewayUnauthorized 报告 401 响应是否为**网关层授权拒绝页**（无业务信封）。
//
// 形态（2026-10-17 实测 CN 站 copilot.tencent.com /v2/*，token 被上游判无效时）：
//
//	HTTP/1.1 401
//	WWW-Authenticate: Bearer realm="copilot", error="invalid_token", ...
//	Server: APISIX/3.9.1
//	<body><html><head><title>401 Authorization Required</title>…openresty…</body>
//
// 与 403 WAF 页同族：都是边缘（APISIX）拦下来的 HTML 页，没有 `{"code":…}` 信封。
// 业务层 401 一定带信封（如 code 12153 offline session），走既有 sessionDeadMarkers。
// 判为 ErrSessionDead 的依据：同一 token 在 /v3/config 仍 200、在 /v2/* 全 401，
// refresh 端点回 invalid_grant ⇒ session 已被上游作废，只能重登。
func IsGatewayUnauthorized(status int, body string) bool {
	return status == http.StatusUnauthorized && !hasBusinessEnvelope(body)
}

// IsWafBlocked 报告 403 响应是否为 WAF 拦截形态：HTTP 403 且 body 无业务信封
// （无 `"code":`/`"msg":` JSON 字段——HTML 拦截页、空体、纯文本均命中）。
// 带业务信封的 403（11140 request illegal / 11-128 等）仍走既有分类链。
// 403 含 accountFault 文案的维持现状（ErrAccountFault），由 Classify 的规则序保证。
func IsWafBlocked(status int, body string) bool {
	return status == http.StatusForbidden && !hasBusinessEnvelope(body)
}

// retryAfterHeaderCandidates 冷却时长优先解析的响应头候选序列：
// retry-after（秒，RFC 7231）/ retry-after-ms（毫秒）/ x-ratelimit-reset
// （epoch 秒或毫秒，取 now+ 剩余量）。大小写不敏感（http.Header.Get 已归一）。
var retryAfterHeaderCandidates = []string{"Retry-After", "Retry-After-Ms", "X-Ratelimit-Reset"}

// retryAfterSanity 解析结果的上限（超过视为上游异常值丢弃，回落本地计算），
// 与 pool 的 softRateMax 默认 2h 同量级。
const retryAfterSanity = 2 * time.Hour

// ParseRetryAfter 从限流/拦截响应头解析上游明示的等待时长：
// 依次尝试 Retry-After（整数秒）→ retry-after-ms（整数毫秒）→
// x-ratelimit-reset（纯数字按 epoch 秒/毫秒推断；HTTP-Date 形态不支持——
// 上游族实践发的是数字）。任一头缺失/非法/非正/超上限则尝试下一头；
// 全部不可用返回 false（调用方回落既有计算值，绝不臆造等待时长）。
func ParseRetryAfter(h http.Header) (time.Duration, bool) {
	for _, name := range retryAfterHeaderCandidates {
		v := strings.TrimSpace(h.Get(name))
		if v == "" {
			continue
		}
		if !isAllDigits(v) {
			continue // 非纯数字（如 HTTP-Date）不解析，宁缺毋滥
		}
		n, ok := parseRetryNumber(v, name)
		if !ok {
			continue
		}
		if n <= 0 || n > retryAfterSanity {
			continue // 非正/异常大：丢弃（回落本地计算）
		}
		return n, true
	}
	return 0, false
}

// isAllDigits 报告 s 是否为纯数字（前置快筛，免 strconv 之后再判语义）。
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// parseRetryNumber 按头名口径把纯数字串折算成时长。x-ratelimit-reset 是
// epoch 时刻而非时长：秒口径（10 位）与毫秒口径（13 位）都按「now+ 该时刻
// 的剩余量」折算，已在过去则不可用。位数不足（8 位以下）无法判定 epoch
// 语义的丢弃（宁缺毋滥：短串多半是序号之类的误用头）。
func parseRetryNumber(v, headerName string) (time.Duration, bool) {
	// 上限 16 位防 int64 溢出（超过 epoch 毫秒的现实量级必非法）。
	if len(v) > 16 {
		return 0, false
	}
	var n int64
	for _, r := range v {
		n = n*10 + int64(r-'0')
	}
	switch headerName {
	case "Retry-After":
		// 先做上限校验再乘 time.Second：16 位数字乘 1e9 会溢出 int64 回绕成
		// 小正数，进而通过调用方的 retryAfterSanity 校验被当作合法等待时长。
		if n > int64(retryAfterSanity/time.Second) {
			return 0, false
		}
		return time.Duration(n) * time.Second, true
	case "Retry-After-Ms":
		if n > int64(retryAfterSanity/time.Millisecond) {
			return 0, false
		}
		return time.Duration(n) * time.Millisecond, true
	default: // X-Ratelimit-Reset：epoch → 剩余量
		sec := n
		if len(v) >= 12 { // 毫秒口径（13 位）；11 位边界按秒（误判代价是多算 1000 倍）
			sec = n / 1000
		}
		remain := time.Until(time.Unix(sec, 0))
		return remain, true
	}
}

// ParseRateReset 从任何限流响应 body 里统一解析「将在 … 重置」时间（上游 UTC+8 文案）。
// 成功返回解析出的**墙钟时刻**（按 UTC+8 解释），失败返回零值 + false。
//
// 是否走模型级豁免、时日对齐到 until 还是 modelCooldowns，由冷却决策侧（pool）按
// IsModelRateLimit 判定，本函数只负责「把上游明说的恢复时刻抽出来」。没有时间文案
// 的限流也照常由调用方退回有界退避（绝不臆造时间）。
func ParseRateReset(body string) (time.Time, bool) {
	// CN 文案优先；global 域 429 body 是英文形态（"will reset at YYYY-MM-DD HH:MM:SS
	// UTC+8"），此前只认中文 → global 限流解析不到恢复时刻，退回有界退避基数反复
	// 翻倍（修「global 域冷却指数翻倍」）。英文正则锚定固定格式时间，自然语言
	// （"reset at the end of the day"）不匹配。
	m := reSoftRateResetCN.FindStringSubmatch(body)
	if len(m) < 2 {
		m = reSoftRateResetEN.FindStringSubmatch(body)
	}
	if len(m) < 2 {
		return time.Time{}, false
	}
	ts := strings.TrimSpace(m[1])
	ts = strings.TrimSuffix(ts, " UTC+8") // 去掉后缀，固定按 softRateResetLoc 解释
	t, err := time.ParseInLocation(softRateTimeLayout, ts, softRateResetLoc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// Classify 按 HTTP 状态码 + body 判定错误类别。
//
// 判定顺序自「严」到「宽」，每层的先后都有语义依据：
//  0. 11102（IsModelBlocked）——「该后端无此模型」确定性答复，语义最具体，最先判
//     （只认 400/404，429+11102 属限流语义走第 4 层）。
//  1. 402 —— 真正的计费余额耗尽状态码，最严、最不可自愈，最先判。
//  2. sessionDeadMarkers —— 需要人工重登的终态。若 401 body 同时含 "12153" 与
//     "rate limit"（如网关错误页混排），归 session_dead：短冷却救不活失效 session，
//     误判为限流会让该死号留在池中反复被选中；且此层 marker 是精确词（12153 等），
//     比限流层的大范围子串更具体，具体优先于宽泛。
//  3. accountFaultMarkers —— 账号级授权/配额故障（11140 request illegal auth 风控、
//     14017 trial not activated register 未完成）。必须先于 status==429 判定：
//     14017 常带 429 状态码，若落到 status==429 会误归 soft_rate（"限流"语义不符：
//     限流可指数退避等自愈，账号级故障等不来）。11140 的 model 级限流变体
//     （rate-limiting 文案）因 marker 不含该文案而天然落到 softRateMarkers 层，
//     不受影响。
//  4. 429 + code 14018 —— 明确的账号积分耗尽，归 ErrHardCredit（issue #175）。
//     只认结构化业务码，不靠可能跨计费/限流两界的文案猜测。
//  5. status==429 —— 限流状态码兜底（先于 hardMarkers）：429 body 高频携带
//     "quota exceeded"/"额度不足" 等跨计费/限流两界的措辞，hardMarkers 先判会把
//     限流误归 ErrHardCredit 硬冷却到次日 04:00，白扔号约 12h。状态码是比关键词
//     更权威的信号；真正的余额耗尽由 402（第 1 层）或 14018（第 4 层）捕获，
//     非 429 状态码的 quota 措辞仍走下方 hardMarkers（第 6 层）。
//  6. hardMarkers —— 非 429 响应携带计费关键词（200 业务信封 / 403 信封等）。
//  7. softRateMarkers —— 非 429 状态码携带限流文案（issue #28 修复点）。
//     位于此处可覆盖 200/400/403/5xx 各状态码。
//  8. 11115 —— 「prompt is too long」请求级语义：判在 404/5xx 与通用 4xx 兜底
//     之前（404 上打 11115 若落 ErrNotFound 会误冷却账号——上下文超限与账号无关）。
//  9. 404 / 5xx —— 与限流无关的常规分类。
//  10. IsWafBlocked —— 403 且无业务信封（HTML 拦截页/空体/纯文本）：APISIX WAF
//     拦截形态。判在通用 4xx 兜底**之前**：此前该形态落 ErrClient → 只换号不罚 →
//     连环 403。带业务信封的 403 已被上方各层捕获，走不到本层。
//     10.5 IsGatewayUnauthorized —— 401 且无业务信封（APISIX HTML 授权拒绝页）：
//     token 被上游判无效，refresh 亦回 invalid_grant ⇒ session 已作废。同上，落
//     ErrClient 会让死号永久留在池里反复被选中，故归 ErrSessionDead。
//  11. 11133 model_param_invalid / 11135 invalid_image_data —— 模型供应商拒绝参数、
//     图片数据无效：确定性请求级 400（同 body 换号结果不变），判在内容策略/参数
//     错误/通用 4xx 之前，避免落 ErrClient 把健康号轮转一遍。
//  12. 内容策略/参数错误/其他 4xx —— 通用兜底。
//  11. 内容策略/参数错误/其他 4xx —— 通用兜底。
func Classify(status int, body string) ErrKind {
	// 11102「该后端无此模型」须最先判：它是「模型在后端不存在」的确定性答复，语义比
	// 计费/限流都更具体——若不先判，msg 里的 "service info not found" 会被更宽的
	// 4xx 兜底归为 ErrClient（只换号不避让），该坏号会留在池内反复被选中。
	// 只认 400/404（见 IsModelBlocked），429+11102 落下方 status==429 层走限流语义。
	if IsModelBlocked(status, body) {
		return ErrModelBlocked
	}
	// 11158「该模型不允许当前账号类型」：与 11102 同属"这个号对这个模型没用"，
	// 同样走 (账号,模型) 负缓存避让、继续轮转别的号；但终端文案必须说清是"
	// 账号等级不支持"——客户端据此换模型/换号，而不是把它当可重试的 503 无限重试。
	// 只认结构化 code（不拿整段文本匹配，避免撞在 requestId 上误判）。
	if IsModelNotAllowedForUserType(body) {
		return ErrModelBlocked
	}
	// 402：真正的计费余额耗尽状态码，最严、最不可自愈，最先判。
	if status == http.StatusPaymentRequired {
		return ErrHardCredit
	}
	lower := strings.ToLower(body)
	// sessionDead / accountFault 先于 status==429：账号级终态等不来自愈，限流状态码
	// 不得掩盖它们（429+14017 必须 accountFault，401+12153 混排 "rate limit" 必须
	// sessionDead——此层 marker 是精确词，比限流层的大范围子串更具体，具体优先于宽泛）。
	for _, m := range sessionDeadMarkers {
		if !strings.Contains(body, m) {
			continue
		}
		// 裸数字标记必须带状态码门槛（BUG-3）："12153" 是上游的会话失效码，
		// 但它也可能出现在 token 数、requestId 片段里 —— 对**任意**状态码做子串
		// 匹配，会把 "prompt is too long ... 12153 tokens" 判成会话失效，
		// 连续 3 次即永久禁用账号。文本标记（Offline user session not found）不设门槛。
		if m == "12153" && status != http.StatusUnauthorized && status != http.StatusForbidden {
			continue
		}
		return ErrSessionDead
	}
	// 通道校验失败必须先于 accountFault 判（上游 PR #233）：11‑-128 的 msg 与
	// 11140 同为 "request illegal"，而 accountFaultMarkers 里就有这句 —— 先判会把
	// 健康账号判成账号故障并硬禁用。双条件（code 11‑-128 + 文案）才分流到通道。
	if isChannelRefused(body) {
		return ErrChannelRefused
	}
	for _, m := range accountFaultMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrAccountFault
		}
	}
	// 14018 是明确的账号积分耗尽业务码。它必须先于通用 429 兜底，否则会被误判为
	// 可自愈的软限流并在全池冷却时反复兜底选中（issue #175）。仅按结构化 code
	// 判定；无该 code 的 "credits exhausted" 文案仍保持普通 429 的软限流语义。
	if status == http.StatusTooManyRequests && hasBusinessCode(body, "14018") {
		return ErrHardCredit
	}
	// status==429 先于 hardMarkers：限流响应 body 高频携带 "quota exceeded"/
	// "额度不足" 等跨计费/限流两界的措辞，hardMarkers 先判会把限流误归
	// ErrHardCredit 硬冷却到次日 04:00，白扔号约 12h。状态码是比关键词更权威的
	// 信号：上游既然给了 429，就按限流语义处理（宁可短冷却自愈，不可长冷却弃号）；
	// 真正的余额耗尽由 402（上层）或 14018（上层）捕获，非 429 状态码的 quota
	// 措辞仍走下方 hardMarkers（历史语义不变）。
	// 6021 queue.full 渠道级容量满载（上游 PR #185）：判在 429 兜底**之前** ——
	// 它是比「限流」更具体的渠道级信号。上游集群排队打满时换任何号都撞同一堵墙，
	// 按限流处置会逐号软冷却（实测 2 秒绞杀整池），换号白撞还把无效请求放大 MaxRotate 倍。
	if isQueueFullBody(lower) {
		return ErrUpstreamBusy
	}
	if status == http.StatusTooManyRequests {
		return ErrSoftRate
	}
	for _, m := range hardMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrHardCredit
		}
	}
	for _, m := range softRateMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrSoftRate
		}
	}
	// 11115「prompt is too long」：判在 404/5xx/WAF/内容策略/参数错误/通用 4xx
	// 之前——请求级语义最具体（上下文超限），须先于宽泛的状态码兜底（404 兜底会
	// 误归 ErrNotFound 只冷却不透传；ErrClient 只换号，浪费健康号配额）。
	if isPromptTooLongStatus(status) {
		for _, m := range promptTooLongMarkers {
			// 两个方向都比对（bug #6）：原条件是
			// `body 含 m || (m 不是全小写 && lower 含 m 的小写)`，而 markers 列表里
			// 全是小写 —— `m != ToLower(m)` 恒假，小写兜底成了死代码，上游回
			// "Prompt is too long"/"PROMPT IS TOO LONG" 时判不出来，落成
			// ErrNotFound（白冷却账号 60s）或 ErrClient（白换一次号）。
			if strings.Contains(body, m) || strings.Contains(lower, strings.ToLower(m)) {
				return ErrPromptTooLong
			}
		}
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	// 11148「tool calls and tool results do not match」：**请求内容**级配对断裂（上游
	// 明示「直接重试无效，请新建任务」）。不在此分流会落 ErrClient → 全池轮转一遍
	// （每个健康号白记一笔连败）+ 末端 503，而同一 body 换任何账号都是同样的 400。
	// 判在状态码兜底之前——上游可能套 5xx 外壳；codeMarker 走空白容差，文案留中英兜底。
	if codeMarker(lower, "11148") || strings.Contains(lower, "tool results do not match") ||
		strings.Contains(body, "工具调用与工具结果不匹配") {
		return ErrToolMismatch
	}
	if status >= 500 {
		return ErrServer
	}
	// WAF 403（无业务信封的拦截形态）：判在内容策略/参数错误/通用 4xx 之前——
	// 这些层只认带文案的 body，WAF 空体/HTML 永远不会命中它们的 marker，
	// 但落 ErrClient 兜底的代价是「只换号不罚」（连环 403 根因），必须在兜底前分流。
	// 带信封的 403 在上方各层已有权威分类，不受影响。
	if IsWafBlocked(status, body) {
		return ErrWafBlock
	}
	// 网关 401（APISIX/openresty HTML 页，无业务信封）：token 被上游判无效。
	// 实测同 token 在 /v3/config 仍 200、在 /v2/* 全 401，refresh 回 invalid_grant
	// ⇒ session 已被作废，只能重登。落 ErrClient（旧行为）的代价是「只换号不罚」：
	// 死号永远留在池里，每个请求都再打一遍、日志里刷出一串 401（2026-10-17 现场：
	// 18 个号里 16 个已死，一次请求打空一整排）。
	if IsGatewayUnauthorized(status, body) {
		return ErrSessionDead
	}
	// 图片格式/数据错误是确定性的请求级错误：同 body 换账号结果不变，直接
	// fail-fast，避免把健康账号轮转一遍后仍把最终 503 返回给客户端。
	// 11135 业务码走 codeMarker（JSON 空白容差），文案走 invalidImageMarkers。
	if status == http.StatusBadRequest && codeMarker(lower, "11135") {
		return ErrImageInvalid
	}
	if status == http.StatusBadRequest {
		for _, m := range invalidImageMarkers {
			if strings.Contains(lower, m) {
				return ErrImageInvalid
			}
		}
	}
	// 14003「模型繁忙」是**模型级、间歇**信号（同一分钟既有 14003 也有成功）：
	// 号是好的，别罚号，只给这个模型停 10s。判在通用 4xx 之前。
	if hasBusinessCode(body, "14003") || strings.Contains(lower, "model busy") {
		return ErrModelBusy
	}
	// （此处原有一处重复的 isChannelRefused 判定，已被上方提前 return 覆盖，
	//   恒不可达 —— 报告 #5 删除）
	// 内容策略拦截（HTTP 400 + 审核文案）：判在通用 ErrClient 之前。
	// 这是误报信号，不罚账号，由网关降级重试处理（见 handler.applyErrorPolicy）。
	if status >= 400 {
		for _, m := range contentBlockedMarkers {
			if strings.Contains(lower, m) {
				return ErrContentBlocked
			}
		}
		// 请求体解析失败（HTTP 400 + Unmarshal chat params failed / code 11101）：
		// 这是"发给上游的 body 有问题"。网关侧截断已由 413 消灭（issue #41 commit A），
		// 剩余来源是客户端 JSON 本身畸形——换了账号照样 400，不该罚号（白白冷却好号）。
		// 归 ErrBadParams：不冷却/不熔断/不计错，且**不轮转**——11101 发生在上游解析
		// 请求体阶段，还没走到模型路由，所以"不同账号可能有不同模型权限"其实是
		// 11102（ErrModelBlocked）的理由，那里已有 (账号,模型) 负缓存避让。
		if strings.Contains(body, badParamsMarkerMsg) || strings.Contains(body, badParamsMarkerCode) {
			return ErrBadParams
		}
		return ErrClient
	}
	// HTTP 200 但业务 code 非 0 且含余额关键词的情况已被上面 hardMarkers 捕获。
	return ErrNone
}

// flexInt 兼容上游把业务码写成数字或字符串（BUG-15）。
//
// `"code":"11102"` 时 int 字段会让整个 json.Unmarshal 失败 → doJSON 直接报
// "parse failed" → 按 code 分类的策略（14018/11102/11101/11115）在这条链路上全失效。
// 非数字码按 0 处理：宁可当成功信封，也别把整条响应判成解析失败。
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	t := strings.TrimSpace(string(b))
	if t == "" || t == "null" {
		*f = 0
		return nil
	}
	if len(t) >= 2 && t[0] == '"' && t[len(t)-1] == '"' {
		t = strings.TrimSpace(t[1 : len(t)-1])
	}
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil {
		*f = 0
		return nil
	}
	*f = flexInt(n)
	return nil
}

// apiEnvelope 上游统一信封。
type apiEnvelope struct {
	Code flexInt         `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// Client 上游 HTTP 客户端。Base 字段可覆盖便于测试。
// refreshFlight 一次在途 refresh 的完成信号（上游 PR #217 单飞）。
type refreshFlight struct {
	done chan struct{}
	err  error
}

type Client struct {
	HTTP *http.Client

	// refreshMu/refreshInFlight 同账号 refresh 单飞（按 UID 分桶）：同一时刻只允许
	// 一个 refresh 在途，其余调用等待并复用首个结果。上游对 refreshToken 反重放，
	// 并发第二发会带着已被消费的 RT 撞 12153 并整链作废（账号从此恒 401，只能人工重登）。
	// 单飞在这里是**保号手段**，不只是省流量。
	refreshMu       sync.Mutex
	refreshInFlight map[string]*refreshFlight

	// ChatHTTP 聊天 SSE 专用 client：无总时长上限（Timeout=0），首字节由
	// Transport.ResponseHeaderTimeout 约束，流中空闲由 IdleTimeout 约束。
	// 与 HTTP 共享同一个 *http.Transport 实例，连接池不重复。
	ChatHTTP *http.Client

	// HeaderTimeout 聊天 SSE 首字节前（响应头）超时；<=0 表示未设置（回落 HTTP.Timeout）。
	HeaderTimeout time.Duration
	// IdleTimeout 聊天 SSE 流中空闲超时；<=0 表示禁用空闲监控。
	IdleTimeout time.Duration

	// DumpRequestMax 落盘文件里出站请求体片段的上限（字节）；0 = 默认 256 KB。
	// 见 dump.go。
	DumpRequestMax int
	// DumpDir 非空时把每次 chat 请求的出站报文与上游原始响应落盘（诊断用，
	// 见 dump.go）。启动后只读，空 = 不落盘。
	DumpDir string

	// effortsMu/efforts 缓存各模型 supportedEfforts（FetchModels 刷新），供请求体 effort 降级。
	// 按 realm 分层桶（cn/global）：同模型名跨域探测的 effort 集合可能不同，
	// 混桶会互相污染（C-2）。
	effortsMu sync.RWMutex
	efforts   map[string]map[string][]string
	// defaultEfforts 缓存各模型 reasoning.defaultEffort（FetchModels 刷新），供
	// thinking.go 补档：缺显式 effort 时优先用模型声明默认档，空串回退硬编码 high。
	// 与 efforts 同 realm 分层桶（同 C-2 隔离原则），共用 effortsMu。
	defaultEfforts map[string]map[string]string
	// modelRates 缓存各模型当前生效积分倍率（规范化数值，如 "0.5"）。
	// 与 efforts 共用 realm 分层和锁；每次成功刷新模型目录时整体替换对应域。
	modelRates map[string]map[string]string

	// globalModels 缓存 global 模型名目录探测结果（成功 ∩ 静态 overlay；
	// 1h TTL + 5min 负缓存），见 global_models.go。按实例持有，测试新建 Client 即隔离。
	globalModels fetchGlobalModelsCache

	// SanitizeFingerprints 出站请求体黑名单指纹脱敏开关（默认 true；false 完全还原）。
	// 面板保存配置热改 + chat 热路径并发读写，用 atomic.Bool 消除数据竞争。
	SanitizeFingerprints atomic.Bool

	// UserAgent 出站 User-Agent 显式覆盖（非空时全路径生效，优先于默认三段式）。
	// 空 = 默认官方形态：chat/refresh/FetchModels 走
	// `WorkBuddy/<ver> WorkBuddy/<ver> CLI/<cliVer>`；billing 走 `WorkBuddy/<ver>`
	// （仅当 client_name 非空）。
	UserAgent string

	// ClientVersion WorkBuddy 客户端版本段（出站 UA 的 `WorkBuddy/<ver>` + X-IDE-Version）。
	// 空 = 内置默认（对齐官方 5.5.4 分发包）。
	ClientVersion string

	// CliVersion 出站 UA 中 `CLI/<ver>` 段版本。空 = 内置默认（官方内置 CLI 2.137.1）。
	CliVersion string
	// ClientVersionGlobal / CliVersionGlobal global realm 专用版本段（空 = 内置默认
	// 5.8.4 / 2.161.0）。CN 与 intl 是两条独立发版线，单值会让一侧 UA 与安装包不符。
	ClientVersionGlobal string
	CliVersionGlobal    string

	// ClientName 用量归属头取值（X-Product / X-IDE-Name / X-IDE-Type / X-IDE-Version）。
	// 空 = 旧行为：X-Product="SaaS"，不设 X-IDE-*（向后兼容，不突变归因）。
	ClientName string

	// PassthroughIP 是否透传客户端 IP 给上游（X-Forwarded-For/X-Real-IP 首段）。
	// 缺省 false（反代安全边界）；handler 在 chat 路径按请求把 clientIP 传入 ChatStream。
	PassthroughIP bool

	// DeviceToken 设备风控 Token（X-Device-Token 头）全局兜底来源：config upstream.device_token。
	// 解析优先级：auth.Auth.DeviceToken > DeviceToken（config）> DeviceTokenFile（文件）。
	DeviceToken string

	// DeviceTokenFile 设备 token 文件路径兜底（宿主落盘的桌面端 token，5 分钟读取缓存）。
	DeviceTokenFile string

	ChatBaseCN    string
	BillingBaseCN string
	// WebBaseCN 官网（workbuddy.cn）域：部分「任务领奖」类接口只在此域提供
	// （Web 成长中心用；CLI 域 copilot.tencent.com 的同名路径返回 400）。
	WebBaseCN string

	// ChatBaseGlobal / BillingBaseGlobal 国际版（global realm）上游 base。
	// 空 = 缺省默认 https://www.workbuddy.ai（D5）。
	ChatBaseGlobal    string
	BillingBaseGlobal string

	// GlobalEnabled 是否启用 global realm 路由（config global.enabled，缺省 true）。
	// false 时即便用户 auth 写了 realm=global 也**不**路由到 global base——
	// chatBase/billingBase 返回 CN base，路径也走 CN（双保险，与 auth.Realm() 的开关闸呼应）。
	GlobalEnabled bool

	// creditObserver 余额查询观察者（见 CreditObserver / SetCreditObserver）。
	// atomic：启动期挂载、运行期并发读，不参与任何出站决策。（qcgzxw `826aaded`）
	creditObserver atomic.Pointer[CreditObserver]
}

// New 生产默认值。Transport 由 newTransport() 集中构造（连接层加固：真正禁 h2 /
// TLS 握手超时 / 短 keepalive 探测 / 失败清池，参数见 transport.go——吸收上游
// kongjianguan 4 连击实测经验）。
func New() *Client {
	tr := newTransport()
	c := &Client{
		HTTP:          &http.Client{Timeout: 120 * time.Second, Transport: tr},
		ChatHTTP:      &http.Client{Timeout: 0, Transport: tr}, // 无总时长；首字节由 ResponseHeaderTimeout 管
		ChatBaseCN:    "https://copilot.tencent.com",
		BillingBaseCN: "https://www.codebuddy.cn",
		WebBaseCN:     "https://www.workbuddy.cn",
		// GlobalEnabled 缺省 true（与 config global.enabled 缺省 true 一致；纯 CN 部署行为不变：
		// CN 账号恒判 cn，global base 只在 realm=global 的账号上被使用）。
		GlobalEnabled: true,
	}
	c.SanitizeFingerprints.Store(true)
	return c
}

// chatHTTP 返回聊天专用 client；未设置（如测试只注入 HTTP）时回落 HTTP。
func (c *Client) chatHTTP() *http.Client {
	if c.ChatHTTP != nil {
		return c.ChatHTTP
	}
	return c.HTTP
}

// defaultGlobalBase 缺省 global base（D5：config 未覆盖时默认 workbuddy.ai）。
const defaultGlobalBase = "https://www.workbuddy.ai"

// globalChatBase 生效的 global chat base：Client.ChatBaseGlobal 非空取之，否则默认。
func (c *Client) globalChatBase() string {
	if c.ChatBaseGlobal != "" {
		return c.ChatBaseGlobal
	}
	return defaultGlobalBase
}

// globalBillingBase 生效的 global billing base：Client.BillingBaseGlobal 非空取之，否则默认。
func (c *Client) globalBillingBase() string {
	if c.BillingBaseGlobal != "" {
		return c.BillingBaseGlobal
	}
	return defaultGlobalBase
}

// globalOn 报告账号是否路由到 global 上游：GlobalEnabled 开且账号 Realm()==global。
// 双保险：config 开关是第一道闸（上游侧），auth.Realm() 的开关闸是第二道（账号侧）。
func (c *Client) globalOn(a *auth.Auth) bool {
	return c.GlobalEnabled && a != nil && a.Realm() == "global"
}

// 路径常量：CN 与 global 共用的 chat 出站路径（/v2 单路径）。
const chatCompletionsPath = "/v2/chat/completions"

// chatPaths 返回按 realm 的 chat 路径候选序列：
// global → [/v2]（#119 固定单路径：/console 挂腾讯云 WAF body 内容规则，反引号
// printf/whoami 等命令执行特征确定性 403；/v2 同 base 不挂该规则，实测等价端点。
// 已知取舍：若上游未来关闭 /v2，global chat 整体不可用——届时应重新启用 /console
// 路径，此注释即"坏了再说"的锚点）；cn → [/v2]（单元素，现状）。
func (c *Client) chatPaths(a *auth.Auth) []string {
	return []string{chatCompletionsPath}
}

// billing 域端点路径（billingBase + path）。balance/checkin 与 report（report.go）同域，
// 统一走 billingJSON 发请求。
const (
	billingMeterPath   = "/billing/meter/get-user-resource"    // global 首选（国际版无 /v2 前缀）
	dailyCheckinPath   = "/billing/meter/daily-checkin"        // global 首选
	billingMeterPathV2 = "/v2/billing/meter/get-user-resource" // CN 现状 / global fallback
	dailyCheckinPathV2 = "/v2/billing/meter/daily-checkin"
)

// billingMeterPaths 按 realm 返回 billing/meter 域路径候选序列：
// global → [无 /v2, 有 /v2]（404 时 fallback）；cn → [有 /v2]（现状逐字，零回归）。
// 仅作用于 get-user-resource / daily-checkin（/billing/meter/* 族）；report /v2/report 不参与，
// 其他 billing 端点（growth 等）路径不含 /billing/meter 前缀，走原常量不受影响。
func (c *Client) billingMeterPaths(a *auth.Auth) []string {
	if c.globalOn(a) {
		return []string{billingMeterPath, billingMeterPathV2}
	}
	return []string{billingMeterPathV2}
}

// checkinMeterPaths 同上，针对 daily-checkin。
func (c *Client) checkinMeterPaths(a *auth.Auth) []string {
	if c.globalOn(a) {
		return []string{dailyCheckinPath, dailyCheckinPathV2}
	}
	return []string{dailyCheckinPathV2}
}

func (c *Client) chatBase(a *auth.Auth) string {
	if c.globalOn(a) {
		return c.globalChatBase()
	}
	return c.ChatBaseCN
}

// prepareBody 组装出站请求体（脱敏开关由 Client.SanitizeFingerprints 控制）。
// realm 为账号 Realm()（cn/global），供 efforts 缓存分桶（跨域 effort 集合不互相污染）。
func (c *Client) prepareBody(body []byte, realm, uid, conversationID, cacheKeyID string) []byte {
	efforts, defs := c.effortsSnapshot(realm), c.defaultEffortsSnapshot(realm)
	if realmKey(realm) == "global" {
		// global 域降级源 = 远端探测桶（权威）∪ 产品静态兜底表（全局 21 名内档位如
		// deepseek-v4.1-flash ['high']）。当前探测桶为空时也按静态表降级，不全程透传
		//（issue #84：往 WorkBuddy 上游发 low/max 非法，须降级到 high）。
		efforts, defs = globalEffortMap(efforts, defs)
	} else {
		// CN 侧此前没有静态兜底（bug P1-4）：探测桶为空（预热失败/无可用 CN 号）
		// 时档位原样出站 → 上游 400 + 11101 → 网关当 ErrBadParams 终结请求。
		efforts, defs = cnEffortMap(efforts, defs)
	}
	body = PrepareBodyOptWithEffortsAndDefault(body, c.SanitizeFingerprints.Load(), efforts, defs)
	// prompt_cache_key 注入（P0 费用优化，费用降 ~17×）：按账号隔离的稳定缓存键，
	// 让同一客户端对同一账号的连续请求命中上游前缀缓存。
	// 缓存槽会话段：优先网关派生的 CacheKeyID（sessKey 或内容派生键），
	// 让「只发 user_id」的客户端也能各占一个前缀缓存槽（issue #5）。
	cacheKeySrc := cacheKeyID
	if cacheKeySrc == "" {
		cacheKeySrc = conversationID
	}
	body = InjectPromptCacheKey(body, uid, cacheKeySrc)
	return body
}

// effortsSnapshot 返回 effort 能力缓存副本；nil 表示未知（透传不降级）。
func (c *Client) effortsSnapshot(realm string) map[string][]string {
	c.effortsMu.RLock()
	defer c.effortsMu.RUnlock()
	bucket, ok := c.efforts[realmKey(realm)]
	if !ok || len(bucket) == 0 {
		return nil
	}
	cp := make(map[string][]string, len(bucket))
	for k, v := range bucket {
		cp[k] = v
	}
	return cp
}

// defaultEffortsSnapshot 返回指定 realm 的模型 defaultEffort 缓存副本；
// 该域无探测或无声明默认档 → nil（thinking.go 回退硬编码 high）。
func (c *Client) defaultEffortsSnapshot(realm string) map[string]string {
	c.effortsMu.RLock()
	defer c.effortsMu.RUnlock()
	bucket, ok := c.defaultEfforts[realmKey(realm)]
	if !ok || len(bucket) == 0 {
		return nil
	}
	cp := make(map[string]string, len(bucket))
	for k, v := range bucket {
		cp[k] = v
	}
	return cp
}

// realmKey 归一化 efforts 缓存键：cn/global。空 realm 视为 cn（老调用/无前缀模型名）。
func realmKey(realm string) string {
	if realm == "" {
		return "cn"
	}
	return realm
}

func (c *Client) billingBase(a *auth.Auth) string {
	if c.globalOn(a) {
		return c.globalBillingBase()
	}
	return c.BillingBaseCN
}

// webBase 返回官网域（任务领奖类接口；未注入时回落默认）。
// realm 感知：global 账号切国际站 workbuddy.ai，CN 用 workbuddy.cn。
func (c *Client) webBase(a *auth.Auth) string {
	if c.globalOn(a) {
		return defaultGlobalBase
	}
	if c.WebBaseCN != "" {
		return c.WebBaseCN
	}
	return "https://www.workbuddy.cn"
}

// desktopDomain 返回桌面端 X-Domain 头应填的**纯 host**（bug #15）。
//
// 真实抓包是 `X-Domain: copilot.tencent.com`（见 desktop.go 文件头），而同仓库
// v3ConfigDomain 也明确剥掉 scheme；此前 desktop.go 三处直接塞 chatBase（完整 URL
// `https://copilot.tencent.com`），与真实客户端不一致，上游可能按非法域处理或归因错误。
func (c *Client) desktopDomain(a *auth.Auth) string {
	base := c.desktopBase(a)
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		return u.Host
	}
	return strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
}

// webOrigin 返回该账号应使用的 Web 来源域（Origin/Referer 的基）。
//
// 为什么必须分域（bug #8）：profile / ClaimReward 的 URL 走 webBase(a)（global 号
// 打 workbuddy.ai），但 Origin/Referer 曾写死 workbuddy.cn —— 国际版号会因来源域
// 与目标域不一致被拒（403/风控），表现为 global 号昵称永远同步不上、领奖走 Web
// 降级路径失败。
func (c *Client) webOrigin(a *auth.Auth) string {
	if c.globalOn(a) {
		return defaultGlobalBase
	}
	return "https://www.workbuddy.cn"
}

// doJSON 发请求并解信封；HTTP 非 2xx 或业务 code != 0 时返回带 body 片段的 *Error。
// body 读失败（连接中断/空闲掐流/截断）返回普通错误（非 *Error）——半截 body 不进
// Classify，不参与账号惩罚（传输层故障不该喂熔断误罚号）。
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		// Retry-After 也要解析（review #11）：ChatStreamContext 一直解析它，
		// 而 doJSON（refresh/checkin/balance/travel/report）此前不解析 —— 走这条路的
		// 429 丢了上游明示的等待时长，冷却只能靠默认基数猜。
		ra, _ := ParseRetryAfter(resp.Header)
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200), RetryAfter: ra}
	}
	// 200 + 空 body 是**成功**（报告 #6）：/v2/report、appearance/set、
	// claim-gift、claim-compensation 这些 RPC 成功时可能回空体，
	// 此前被 json.Unmarshal 判成 parse failed，成功记成失败。
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 120))
	}
	if env.Code != 0 {
		// 把 code 一起交给 Classify（bug B8）：此前只传 env.Msg，而
		// hasBusinessCode / badParamsMarkerCode(`"code":11101`) / IsModelBlocked
		// 都依赖原始 JSON 结构 —— 于是 14018（积分耗尽）、11102（模型不存在）、
		// 11101（参数错）在这些走 doJSON 的路径上一律退化成 ErrClient，
		// 按 code 判定的策略全部失效。
		//
		// 还要给 Classify 一个**语义状态码**（bug #10）：IsModelBlocked /
		// promptTooLongMarkers / invalidImageMarkers 都带 400/404/413 状态门槛，
		// 而这里拿到的常是 HTTP 200 + 业务 code —— 不合成的话只有 14018/11101
		// 两类真正受益，11102/11115/11135 仍然退化。
		// 注意（BUG-17）：Error.Status 报的是**语义状态码**（clsStatus），真实 HTTP
		// 状态放在 Error.HTTPStatus —— 下游 ClaimRewardMP 的 400 降级正是按语义码工作。
		// 这里早先的注释写的是「Status 仍报真实 HTTP 状态」，与实现相反，已更正。
		clsStatus := resp.StatusCode
		if clsStatus < 400 {
			if s := businessCodeStatus(int(env.Code)); s != 0 {
				clsStatus = s
			}
		}
		// 直接把**原始 body** 交给 Classify（报告 #8）：此前用
		// `{"code":%d,"msg":%s}` 重建，原 body 的 error/data 子对象、extError、
		// displayMsg、requestId 全部丢失 —— 依赖结构或 displayMsg 的判定
		// （IsModelBlocked / IsAccountRestricted）在这条路径上只能靠 msg 短语猜。
		// code 与 msg 本来就在 raw 里，重建没有任何收益。
		kind := Classify(clsStatus, string(raw))
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{
			Kind:       kind,
			Status:     clsStatus, // 语义状态（BUG-4）
			HTTPStatus: resp.StatusCode,
			Msg:        fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 160)),
		}
	}
	return env.Data, nil
}

// RefreshToken 刷新 access token；成功时更新 a 的字段（缺省值保留旧值），
// 调用方负责 SaveAtomic。全程持 a 锁，防止并发 SaveAtomic 读半更新 token。
// refreshIOTimeout 刷新端点网络 I/O 上限（两段式锁外执行，防上游 hang 长占锁）。
const refreshIOTimeout = 30 * time.Second

// refreshTokenExpiresInMax refresh 响应 expiresIn 的量级上限（10 年，纯防御值：
// 实测 R-D 响应恒 5184000=60d）。超限视为上游脏数据，不写 ExpiresAt（保留旧值），
// 防止 NeedsRefresh 永假导致 token 永不刷新反而真过期失效。
const RefreshTokenExpiresInMax = 10 * 365 * 24 * time.Hour

// refreshTokenExpiresInMax 兼容旧引用。
const refreshTokenExpiresInMax = RefreshTokenExpiresInMax

// RefreshToken 刷新 access token；成功时更新 a 的字段（缺省值保留旧值），
// 调用方负责 SaveAtomic。
//
// 并发安全模型（两段式，缩小持锁窗口）：
//   - 锁内仅做「读 refreshToken 快照」与「校验未变后写回新 token」两小段内存操作；
//   - 网络 I/O（doJSON）在**锁外**执行，带 30s ctx 超时——避免上游 hang 时长时间
//     独占 a.mu，阻塞同账号的 SaveAtomic / 其他刷新（issue:持锁 120s I/O）。
//   - 写回前重新校验快照一致性：若锁外期间另一 goroutine 已完成刷新（refreshToken
//     已变），本次结果直接采用（新 token 已生效），不再重复写回。
//
// RefreshToken 同账号单飞（上游 PR #217）：并发调用共享首个结果。
func (c *Client) RefreshToken(a *auth.Auth) (err error) {
	if a == nil {
		// nil 防御（报告 P0-1）：此前 key := a.UID 直接解引用，nil 会 panic 在
		// defer 之外 —— 表项永久残留。
		return fmt.Errorf("refresh: nil auth")
	}
	key := a.UID
	if key == "" {
		// 无 UID 的临时 auth：按对象指针去重，仍保证同一对象串行
		key = fmt.Sprintf("%p", a)
	}
	c.refreshMu.Lock()
	if c.refreshInFlight == nil {
		c.refreshInFlight = make(map[string]*refreshFlight)
	}
	if f, ok := c.refreshInFlight[key]; ok {
		c.refreshMu.Unlock()
		<-f.done
		return f.err
	}
	f := &refreshFlight{done: make(chan struct{})}
	c.refreshInFlight[key] = f
	c.refreshMu.Unlock()

	// 异常出口必须有保证（报告 P0-1）：refreshTokenOnce 内部会解 token、JSON、
	// 上游响应，任一 panic 都会跳过下面的收尾 → `done` 永不关闭、表项永不清除，
	// 该 UID 之后的每一次 RefreshToken 都停在 `<-f.done` 上**永久阻塞**（等待端
	// 也没有超时可脱身，handler 的在途租约一并钉死）。defer + recover 兜住。
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("refresh panic: %v", r)
			log.Printf("WARN: [upstream] refresh panic 已恢复（uid=%s）: %v", key, r)
		}
		// 顺序很重要（报告 #10）：先写结果、关信号，再删表。此前先 delete 再 close，
		// 两者之间到达的调用会新建一次刷新 —— 旧 refreshToken 已被上游 rotate，
		// 第二发必然带废 token 撞 12153，整链作废（正是单飞要防的事）。
		f.err = err
		close(f.done)
		// 只能锁一次（sync.Mutex 不可重入）：曾因补丁残留多写一个 Lock()，
		// 第二次永远拿不到 → 所有 refresh 卡死。
		c.refreshMu.Lock()
		delete(c.refreshInFlight, key)
		c.refreshMu.Unlock()
	}()

	return c.refreshTokenOnce(a)
}

// refreshTokenOnce 实际执行一次刷新（原 RefreshToken 的实现体）。
func (c *Client) refreshTokenOnce(a *auth.Auth) error {
	// 第 1 段（锁内）：读快照。
	a.Lock()
	rtSnapshot := a.RefreshToken
	atBefore := a.AccessToken
	a.Unlock()
	if strings.TrimSpace(rtSnapshot) == "" {
		return fmt.Errorf("no refreshToken")
	}

	endpoint := c.chatBase(a) + "/v2/plugin/auth/token/refresh"
	ctx, cancel := context.WithTimeout(context.Background(), refreshIOTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return err
	}
	// RefreshHeaders 读取 a 的字段（domain/uid 等）注入请求头——需在锁内取快照值，
	// 用一个显式逐字段拷贝的临时 auth 构造头（不拷贝 sync.Mutex，避免 vet copies-lock）。
	a.Lock()
	hdrSnapshot := auth.Auth{
		AccessToken:  a.AccessToken,
		RefreshToken: rtSnapshot,
		ExpiresAt:    a.ExpiresAt,
		Domain:       a.Domain,
		UID:          a.UID,
		EnterpriseID: a.EnterpriseID,
		Nickname:     a.Nickname,
		DeviceToken:  a.DeviceToken,
	}
	a.Unlock()
	c.RefreshHeaders(req, &hdrSnapshot)

	// 网络 I/O（锁外，30s 上限）。
	data, err := c.doJSON(req)
	if err != nil {
		return err
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil {
		return fmt.Errorf("refresh_failed: %w", err)
	}
	if tok.AccessToken == "" {
		// 解析成功但 accessToken 为空：**先把已 rotate 的 refreshToken 落回**（bug B5）。
		// 上游 refresh 会 rotate 旧 refreshToken 使其作废，若这里直接 return，新的就丢了
		// → 该账号之后每次刷新都失败，只能重新登录。
		if tok.RefreshToken != "" {
			a.Lock()
			a.RefreshToken = tok.RefreshToken
			if tok.Domain != "" {
				a.Domain = tok.Domain
			}
			a.Unlock()
			// 必须**自己落盘**（bug #2）：三个调用点（handler / scheduler / signin）
			// 在 error 分支都直接放弃、不 SaveAtomic。上游已经把旧 refreshToken
			// rotate 作废了，不写回磁盘的话，进程重启后读回的是作废的旧值 ——
			// 该账号从此再也刷新不了，只能重新登录。
			if a.FilePath != "" {
				if serr := a.SaveAtomic(); serr != nil {
					log.Printf("WARN: [upstream] refresh rotate 落盘失败 uid=%s: %v", a.UID, serr)
				}
			}
		}
		return fmt.Errorf("refresh_failed: no accessToken in response — re-login required")
	}

	// 第 2 段（锁内）：校验快照一致后写回。
	a.Lock()
	defer a.Unlock()
	// 写回守卫是 AND 语义：锁外期间另一刷新已完成 → 两 token 必同时变化（实测 R-D：
	// refresh 响应 accessToken/refreshToken 总是一起 rotate，写回也同时写两个），AND
	// 即「并发刷新已完成」判据；AND 与 OR 在真实形态下等价。唯 OR 会额外放弃的
	// 「只有单 token 变化」（如手工只改 auth 文件一个字段）不构成放弃条件——本次
	// 结果覆盖手工编辑。
	if a.AccessToken != atBefore && a.RefreshToken != rtSnapshot {
		// 锁外期间另一 goroutine 已完成刷新：新 token 已生效，本次结果不必再写
		// （实测 R-E：服务端无 rotation 撤销，并发双刷新拿到的两个新 token 都有效，
		// 后写覆盖先写二者等价可用；提前返回避免无意义覆盖与 ExpiresAt 抖动）。
		return nil
	}
	a.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		a.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		a.Domain = tok.Domain
	}
	// preserveExpiry：响应缺 expiresIn 时保留旧过期时间，避免刷新风暴。
	// 实测 R-D 响应恒带 expiresIn=5184000（60d）——缺省分支仅为防御，保留旧值
	// 避免过期判定漂移。同理，超过 10 年的 expiresIn 按脏值处理保留旧值：
	// 实测 JWT exp-iat 与 expiresIn 严格自洽（R-F），超量级值只会是上游脏数据，
	// 照写会把 ExpiresAt 推到荒谬未来 → NeedsRefresh 永假 → token 永不刷新
	// 反而真过期失效。
	// 先按**秒**比较再乘（bug #5）：原写法是"乘完之后再判是否小于 10 年"，而
	// ExpiresIn > 9.22e9（约 292 年）时乘法本身 int64 溢出成负数 —— 负数恒小于上限
	// → 校验通过 → ExpiresAt 落在过去 → NeedsRefresh 恒真 → **每个 chat 请求前都先打
	// 一次 refresh**，把 refresh 端点打爆，还可能触发 12153 三振禁用。
	maxExpiresInSec := int64(refreshTokenExpiresInMax / time.Second)
	if tok.ExpiresIn > 0 && tok.ExpiresIn <= maxExpiresInSec {
		a.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	return nil
}

// ChatStream 发 chat 请求并返回原始 SSE body 流（调用方负责 Close）。
// 等价于 ChatStreamContext(context.Background(), ...)：不带调用方取消语义。
// 需要客户端断开联动的调用方用 ChatStreamContext 传入请求 ctx。
//
// global realm：先打 /console/chat/completions，404/405 时同一 base 二次换 /v2/chat/completions
// （上游新旧路径分叉，PLAN R9 fallback 顺序）。cn：/v2/chat/completions 现状不变。
func (c *Client) ChatStream(a *auth.Auth, body []byte, clientIP string, meta ChatMeta) (rc io.ReadCloser, status int, respBody []byte, err error) {
	return c.ChatStreamContext(context.Background(), a, body, clientIP, meta)
}

// ChatStreamContext 同 ChatStream，但从 ctx 派生请求 context：调用方（handler）传入
// r.Context() 后，客户端断连/请求取消会立即中断在途上游调用、释放连接与账号在途名额，
// 不再空转到 IdleTimeout。ctx 为 nil 时回落 Background。成功流的 cancel 仍由
// monitorBody 的 Close 接管（reqCtx 取消与显式 Close 任一触发即断）。
//
// 错误路径（≥400 且非 fallback 状态码）除 (status, respBody) 外还返回**已分类的**
// *Error（Kind 信封 + Retry-After 头解析）：客户端错误分类在此一次完成，handler
// 不再对 body 二次 Classify（消除「上游分类一次、网关再分类一次」的双路径漂移面），
// Retry-After 也随信封流动。respBody 仍原样返回（错误透传语义：message 透传上游
// 原文）。判定为 ErrNone 的响应（理论上不存在，防御）err 为 nil，handler 按
// respBody 自行兜底。
//
// global chat 自 #119 实测后固定走 /v2（chat 层无 fallback 链；billing 层的 404
// fallback 独立存在，语义不受影响）。ensureConsoleSystem 在 prepareBody 后统一套用
// 全局脚本：首条消息非 system 时前置兜底 system（防 console 域上游 code 11-128；
// #119 后 global 出站固定 /v2，该兜底保留——上游对 /v2 是否需要 system 无实测
// 反证，删了无回滚路径）。
func (c *Client) ChatStreamContext(ctx context.Context, a *auth.Auth, body []byte, clientIP string, meta ChatMeta) (rc io.ReadCloser, status int, respBody []byte, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	prepared := c.prepareBody(body, a.Realm(), a.UID, meta.ConversationID, meta.CacheKeyID)
	// body 侧会话字段（issue #51）：把本轮 ID 同时写进消息体，与出站头保持头体一致
	// （官方桌面端头与体都带 conversationRequestId）。见 conv_fields.go。
	prepared = injectConversationFields(prepared, meta.ConversationRequestID)
	if c.globalOn(a) {
		prepared = ensureConsoleSystem(prepared)
	}
	// reqCtx 的 cancel 在每个出口显式调用（Do 失败 / ≥400 / 成功分支移交 monitorBody），
	// 循环本身各分支必 return——无循环尾兜底代码（此前外层 var cancel 从未赋值 +
	// 尾部不可达 cancel() 是潜伏 nil-panic，已删；chatPaths 恒非空由构造保证）。
	for _, path := range c.chatPaths(a) {
		endpoint := c.chatBase(a) + path
		req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(prepared))
		if err != nil {
			return nil, 0, nil, err
		}
		c.ChatHeaders(req, a, clientIP, meta)
		// 从调用方 ctx 派生：保留取消传播（父 ctx 取消 → 本 ctx 取消），
		// 同时 monitorBody.Close 仍能独立 cancel 本分支（空闲掐流）。
		reqCtx, cancel := context.WithCancel(ctx)
		req = req.WithContext(reqCtx)
		resp, err := c.chatHTTP().Do(req)
		if err != nil {
			cancel()
			log.Printf("ERR: [upstream] chat_stream acct=%s: transport error: %v", logfmt.Label(a.UID, a.NicknameValue()), err)
			// 传输层失败 → 清空共享连接池的空闲连接（连接层加固）：失败连接可能仍
			// 留在空闲池里，下一个请求会继续捡到它——仅靠 IdleConnTimeout 等过期
			// 不够，主动清池才断根。
			roundTripCloseIdle(c.chatHTTP().Transport)
			return nil, 0, nil, err
		}
		if resp.StatusCode >= 400 {
			raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			cancel()
			// body 读失败（掐流/截断）→ 传输层错误：半截 raw 不交回调用方进 Classify，
			// 否则 handler 侧 applyErrorPolicy 会按误判分类罚号。
			if rerr != nil {
				log.Printf("ERR: [upstream] chat_stream acct=%s: read body: %v", logfmt.Label(a.UID, a.NicknameValue()), rerr)
				return nil, 0, nil, fmt.Errorf("read body: %w", rerr)
			}
			kind := Classify(resp.StatusCode, string(raw))
			log.Printf("WARN: [upstream] chat_stream acct=%s: upstream %d %s body=%s",
				logfmt.Label(a.UID, a.NicknameValue()), resp.StatusCode, kind, truncate(string(raw), 200))
			// ≥400 直接返回（#119 后 global 单路径 /v2，chat 层无 fallback 链）。
			// 分类一次、随 Kind 信封返回（含 Retry-After 头解析）：
			// ErrNone 是防御分支（≥400 不应产生 None），返回原文让 handler 兜底。
			if kind == ErrNone {
				return nil, resp.StatusCode, raw, nil
			}
			ue := &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
			if d, ok := ParseRetryAfter(resp.Header); ok {
				ue.RetryAfter = d
			}
			return nil, resp.StatusCode, raw, ue
		}
		// 成功分支：cancel 所有权交给 monitorBody（其 Close 会 cancel）；
		// IdleTimeout<=0 时 monitorBody 原样返回底流、无人调 cancel——可接受：
		// 取消传播由 http.Transport 在 body Close / 父 ctx 取消时处理，连接正常清理。
		body := c.dumpUpstream(monitorBody(resp.Body, c.IdleTimeout, cancel), prepared, a)
		return body, resp.StatusCode, nil, nil
	}
	// 不用 panic 做兜底（bug #11）：HTTP 路径有 net/http recover，但 RunNightChats /
	// runModelChat 这些后台 goroutine 没有 —— chatPaths 一旦被改成可返回空切片
	// （例如按 realm 配置化），一次 panic 打死的是整个网关进程。
	return nil, 0, nil, fmt.Errorf("no chat path for realm %q", a.Realm())
}

// ModelInfo 动态模型信息（含 maxInputTokens/maxOutputTokens + 上游模型对象全字段）。
// CN /console 与 global /v2 的模型对象同构，故共用此结构；上游省略的字段保持零值，
// /v1/models 侧按「空值省略」透出（不编造）。
type ModelInfo struct {
	ID            string
	Name          string
	ContextWindow int64    // = maxInputTokens
	MaxTokens     int64    // = maxOutputTokens（思考与最终回答共享此预算，上游无独立思考上限字段）
	Efforts       []string // reasoning.supportedEfforts（空=未知/固定档）
	DefaultEffort string   // reasoning.defaultEffort（新模型键）或 reasoning.effort（老模型键）；空=未返回

	// 模型目录全字段（models-full-fields）：
	Description string   // descriptionZh 中文描述
	Credits     string   // credits 积分倍率原文（如 "x0.05"），仅展示不参与选号
	Tags        []string // tags 模型标签（含 badge:限时免费 等）
	// Tier /v3/config data.modelTiers 的官方标识文案（global「Pro Only」/ CN
	// 「订阅优先」），空=不受限。只透文案不给布尔：海外是 Pro 专属（免费号调不通），
	// 国内只是「优先调度」（能用，排队靠后），同一个 requiredUserType 两种语义，
	// 一刀切成「不可用」会把国内这三个模型错标成禁选（issue #235）。
	Tier               string
	Vendor             string // vendor 厂商标识
	IsDefault          bool   // isDefault 是否默认模型
	SupportsReasoning  bool   // supportsReasoning 是否支持推理
	SupportsToolCall   bool   // supportsToolCall 是否支持工具调用
	OnlyReasoning      bool   // onlyReasoning 是否纯推理模型
	SupportsImages     bool   // 顶层 supportsImages（多模态能力，透出到 /v1/models）
	MaxAllowedSize     int64  // maxAllowedSize 最大允许上下文（与 maxInputTokens 口径并列，上游各自下发）
	CanDisableThinking bool   // reasoning.canDisableThinking：思考可关（off 档可用）
	ReasoningEffort    string // reasoning.effort 推理模式（与 supportedEfforts 数组不同源）
	ReasoningSummary   string // reasoning.summary 推理摘要模式（如 "auto"）

	// 优惠（modelPromotions，/v3/config data.modelPromotions）：Credits 是**牌价**
	//（转正后基准倍率），Promo* 是当前生效的限时优惠——面板据此显示「生效价 +
	// 标签 + 牌价」。PromoFactor 为 nil 表示无 machine-readable 折扣（如「错峰
	// 使用」只有时段文案无 factor），仅挂标签/提示。
	PromoFactor  *float64 // 折扣系数（0=限时免费，0.5=五折）；nil=无
	PromoCredits string   // 折扣后倍率原文（如 "0x" / "0.50x"），仅展示
	PromoLabel   string   // 徽章文案（限时免费 / 夜间折扣 / 错峰使用）
	PromoNote    string   // hover 说明原文（含时段/日期描述）
}

// dynModelEntry 上游模型目录（CN /console 与 global /v2 同构）的单条模型解析形态，
// FetchModels 与 global_models.go 的探测共用。iconUrl/descriptionEn/生成参数等
// 按「不透出」原则不解析。modelInfo() 是 dynEntry→ModelInfo 映射的单一事实来源，
// 杜绝两域映射漂移。
type dynModelEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// ModelID / Model id 的宽松回退键（仅 global 目录的多信封兜底用，CN 目录
	// 不下发这两个键；字段加在这里只是让 typed 解析能"看见"它们）。
	ModelID         string   `json:"modelId"`
	Model           string   `json:"model"`
	Description     string   `json:"descriptionZh"`
	Credits         string   `json:"credits"`
	Tags            []string `json:"tags"`
	Vendor          string   `json:"vendor"`
	IsDefault       bool     `json:"isDefault"`
	MaxInputTokens  int64    `json:"maxInputTokens"`
	MaxOutputTokens int64    `json:"maxOutputTokens"`
	MaxAllowedSize  int64    `json:"maxAllowedSize"`
	Disabled        bool     `json:"disabled"`
	SupportsImages  bool     `json:"supportsImages"`
	SupportsReason  bool     `json:"supportsReasoning"`
	SupportsTool    bool     `json:"supportsToolCall"`
	OnlyReasoning   bool     `json:"onlyReasoning"`
	Reasoning       struct {
		Effort             string   `json:"effort"`
		Summary            string   `json:"summary"`
		DefaultEffort      string   `json:"defaultEffort"`
		CanDisableThinking bool     `json:"canDisableThinking"`
		SupportedEfforts   []string `json:"supportedEfforts"`
	} `json:"reasoning"`
}

// modelInfo 按解析条目构造 ModelInfo（dynEntry→ModelInfo 映射的单一事实来源）。
// defaultEffort 新老双键兼容：defaultEffort 优先，缺省回落 effort。
func (m dynModelEntry) modelInfo() ModelInfo {
	def := m.Reasoning.DefaultEffort
	if def == "" {
		def = m.Reasoning.Effort
	}
	return ModelInfo{
		ID:                 m.ID,
		Name:               m.Name,
		ContextWindow:      m.MaxInputTokens,
		MaxTokens:          m.MaxOutputTokens,
		Efforts:            m.Reasoning.SupportedEfforts,
		DefaultEffort:      def,
		SupportsImages:     m.SupportsImages,
		Description:        m.Description,
		Credits:            m.Credits,
		Tags:               m.Tags,
		Vendor:             m.Vendor,
		IsDefault:          m.IsDefault,
		SupportsReasoning:  m.SupportsReason,
		SupportsToolCall:   m.SupportsTool,
		OnlyReasoning:      m.OnlyReasoning,
		MaxAllowedSize:     m.MaxAllowedSize,
		CanDisableThinking: m.Reasoning.CanDisableThinking,
		ReasoningEffort:    m.Reasoning.Effort,
		ReasoningSummary:   m.Reasoning.Summary,
	}
}

// nonChatModel 判定是否非对话模型（应从模型列表过滤掉）。
// 来源：harness buddy.ts:547-555。三类规则：
//   - id 前缀 nes-/completion-/codewise-：嵌入/补全/代码专用模型，选了报 code=11102。
//   - maxOutputTokens ≤ 256：tiny 输出非对话模型。
//   - tags 含生成类标签（图片/视频）：生成模型走各自专用端点，作为对话模型
//     选上去只会报 11102，非本网关用途。
//
// 生成类标签随上游扩充：早期只有 text-to-image，桌面端目录（2026-10-02 实测）
// 另有 text-to-video / image-to-video（seedance 系列）与 image-to-image
// （gpt-image 系列）——后者已由 text-to-image 覆盖，此处补齐视频两类。
// 注意本函数 CN 与 global 共用，新增标签对两域同时生效。
func nonChatModel(id string, maxOutputTokens int64, tags []string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, p := range [...]string{"nes-", "completion-", "codewise-"} {
		if strings.HasPrefix(id, p) {
			return true
		}
	}
	if maxOutputTokens > 0 && maxOutputTokens <= 256 {
		return true
	}
	for _, t := range tags {
		switch t {
		case "text-to-image", "image-to-image", "text-to-video", "image-to-video":
			return true
		}
	}
	return false
}

// codeBuddyIDEUA /v3/config 要求能解析出 CodeBuddy 版本号的 UA。
// CLI 三段式 WorkBuddy UA 会拿到精简目录（flash 输出 128K、无 supportedEfforts）；
// 官方 IDE 头 `CodeBuddyIDE/4.12.0 CodeBuddy/4.12.0` 才返回完整能力
// （flash：393216 + low/high/max）。
// 版本号需随上游 IDE 发版跟进：UAn 版本过旧时该端点可能同样返回精简目录。
const codeBuddyIDEUA = "CodeBuddyIDE/4.12.0 CodeBuddy/4.12.0"

// codeBuddyCLIUA CLI 三段式 UA。**实测（2026-09-22）该端点对不同 UA 下发的模型集合不同**：
//   - IDE UA  → 14 条（10 个 chat：含 o4-mini / enhance-1.0 / auto-chat，**无 deepseek 系列**）
//   - CLI UA  → 22 条（22 个 chat：**含 deepseek-v4.1-flash / deepseek-v4.1-flash-sg /
//     gpt-6-astra / kimi-k2.8-preview**，但无 o4-mini / enhance-1.0 / auto-chat）
//
// 注意两点，都与旧注释相反，勿再按旧注释推断：
//  1. 旧注释称「CLI UA 拿到精简目录、IDE UA 才返回完整能力」——实测模型数量恰好相反，
//     但 **IDE 响应体积更大**（26003B vs 21111B），故「完整能力」应理解为**单条字段更全**，
//     而非模型更多。两路各有独有模型，缺一不可。
//  2. 该常量仅用于 global 侧第二路探测；CN 侧仍走 codeBuddyIDEUA 单路。
const codeBuddyCLIUA = "CLI/2.63.2 CodeBuddy/2.63.2"

// FetchModels 调上游动态模型接口（CN 侧；global 账号见 global_models.go 家族）。
//
// v3-config-merge：动态目录 = /v3/config（主，IDE UA 完整能力版）+ 企业端点
// （/console，cli 面过滤，补缺）的并集，两路**并发**探测。合并去重 key = 模型 id，
// v3 条目优先（credits 等字段以 v3 为准），企业端点只补 v3 缺失的模型。
// /v3 失败（400/网络错/解析失败）不拖累企业端点结果——降级为仅企业端点，warn 日志；
// 反之亦然（两路独立容错）。
func (c *Client) FetchModels(a *auth.Auth) ([]ModelInfo, error) {
	type probeResult struct {
		infos []ModelInfo
		err   error
	}
	enterpriseCh := make(chan probeResult, 1)
	v3Ch := make(chan probeResult, 1)
	go func() {
		infos, err := c.fetchEnterpriseModels(a)
		enterpriseCh <- probeResult{infos, err}
	}()
	go func() {
		infos, err := c.fetchV3Models(a)
		v3Ch <- probeResult{infos, err}
	}()
	enterprise := <-enterpriseCh
	v3 := <-v3Ch
	if enterprise.err != nil && v3.err != nil {
		return nil, enterprise.err // 两路全失败：返回企业端点错误（既有调用方语义零漂移）
	}
	if v3.err != nil {
		// /v3 失败降级：不拖累企业端点结果（降级仅企业端点 + warn）。
		log.Printf("WARN: [upstream] fetch models: v3/config probe failed (degraded to enterprise endpoint): %v", v3.err)
	}
	if enterprise.err != nil {
		log.Printf("WARN: [upstream] fetch models: enterprise endpoint failed (v3/config only): %v", enterprise.err)
	}
	out := mergeModelInfos(v3.infos, enterprise.infos)
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	c.storeModelRates(a.Realm(), out)
	// 刷新 effort 能力缓存（供请求体降级；无 supportedEfforts 的模型不入桶）。
	// 空桶时跳过写：避免「某探测无档位数据」清掉既有桶。
	cache := make(map[string][]string, len(out))
	defCache := make(map[string]string, len(out))
	for _, mi := range out {
		if len(mi.Efforts) > 0 {
			cache[mi.ID] = mi.Efforts
		}
		if mi.DefaultEffort != "" {
			defCache[mi.ID] = mi.DefaultEffort
		}
	}
	if len(cache) == 0 && len(defCache) == 0 {
		return out, nil
	}
	// 按探测账号的 realm 写入对应桶：CN 探测只进 cn 桶，global 同模型名不被污染（C-2）。
	c.storeEfforts(a.Realm(), cache, defCache)
	return out, nil
}

// mergeModelInfos 合并两路模型目录：primary 为主（同 id 以 primary 条目为准——
// credits 等字段以主端点为权威），secondary 只补 primary 缺失的 id。
// 去重 key = 模型 id；输出顺序 = primary 原序在前、secondary 补充项（secondary 原序）
// 在后——稳定输出，不依赖 map 迭代序。
func mergeModelInfos(primary, secondary []ModelInfo) []ModelInfo {
	if len(secondary) == 0 {
		return primary
	}
	seen := make(map[string]bool, len(primary)+len(secondary))
	out := make([]ModelInfo, 0, len(primary)+len(secondary))
	for _, mi := range primary {
		if mi.ID == "" || seen[mi.ID] {
			continue
		}
		seen[mi.ID] = true
		out = append(out, mi)
	}
	for _, mi := range secondary {
		if mi.ID == "" || seen[mi.ID] {
			continue
		}
		seen[mi.ID] = true
		out = append(out, mi)
	}
	return out
}

// fetchEnterpriseModels 单路探测企业模型端点（/console/enterprises/personal/models）。
// 解析口径：agents[cli].models 过滤 + nonChatModel 剔除 + disabled 剔除。
func (c *Client) fetchEnterpriseModels(a *auth.Auth) ([]ModelInfo, error) {
	// 局部变量名避开 url（本包已 import net/url，同名会造成阅读混淆）。
	endpoint := c.chatBase(a) + "/console/enterprises/personal/models"
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	c.CommonHeaders(req, a) // 复用共享请求头（Origin/Referer/UA/Accept/Content-Type）
	// AccessToken 加锁快照（见 auth.AccessTokenValue：keepalive 刷新在 a.mu 内改写）。
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		// 读失败 → 传输层错误（handler 侧该路径不 NoteError）。
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models api status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Models []dynModelEntry `json:"models"`
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("models api code=%d", env.Code)
	}
	var cliIDs []string
	for _, ag := range env.Data.Agents {
		if ag.Name == "cli" {
			cliIDs = ag.Models
			break
		}
	}
	if len(cliIDs) == 0 {
		return nil, fmt.Errorf("no cli agent models found")
	}
	// dynMap 收集模型字段；nonChatModel 过滤在写入 dynMap 前执行，
	// 确保非对话条目（nes-/completion-/codewise- 前缀、maxOutputTokens≤256、
	// tags 含 text-to-image）根本不进返回列表（来源：harness buddy.ts:547-555）。
	dynMap := make(map[string]dynModelEntry, len(env.Data.Models))
	for _, m := range env.Data.Models {
		if nonChatModel(m.ID, m.MaxOutputTokens, m.Tags) {
			continue
		}
		dynMap[m.ID] = m
	}
	out := make([]ModelInfo, 0, len(cliIDs))
	for _, id := range cliIDs {
		if m, ok := dynMap[id]; ok && !m.Disabled {
			out = append(out, m.modelInfo())
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	return out, nil
}

// fetchV3Models 单路探测 /v3/config（IDE UA 完整能力版，见 codeBuddyIDEUA）。
// v3 面取全量 models（不按 agents[cli] 过滤，与 global 探测口径一致），按同一
// nonChatModel 规则剔除非对话条目（selected 会选模型报 code=11102）。
// 失败返回错误（调用方降级为仅企业端点）。
func (c *Client) fetchV3Models(a *auth.Auth) ([]ModelInfo, error) {
	byID, err := c.fetchV3ConfigModelMap(a, codeBuddyIDEUA)
	if err != nil {
		return nil, err
	}
	// 按 id 排序后再输出（bug #8）：byID 是 map，迭代序随机 —— 于是 /v1/models 与
	// 面板「模型与档位」的顺序每 10 分钟缓存过期就重排一次，mergeModelInfos 承诺的
	// 「输出顺序 = primary 原序在前」不成立，前端按顺序做的对比/截图也无法复现。
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]ModelInfo, 0, len(ids))
	for _, id := range ids {
		mi := byID[id]
		if nonChatModel(mi.ID, mi.MaxTokens, mi.Tags) {
			continue
		}
		out = append(out, mi)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("v3/config returned empty models")
	}
	return out, nil
}

// v3ModelPromotion /v3/config data.modelPromotions 单条优惠定义（2026-09-23 实测
// 7 条：deepseek 系错峰五折、glm-5.2 夜间五折、hy3 与 hy4-preview-f 限时免费）。
// discount 只在部分条目上存在：有 factor 的可算生效价；「错峰使用」类只有时段
// 文案（factor 藏在 hover 文本里，无机器可读值），仅透出标签与说明。
type v3ModelPromotion struct {
	Enabled  bool     `json:"enabled"`
	Priority int      `json:"priority"`
	ModelIDs []string `json:"modelIds"`
	Badge    *struct {
		Label string `json:"label"`
	} `json:"badge"`
	Discount *struct {
		DiscountedCredits string  `json:"discountedCredits"`
		Factor            float64 `json:"factor"`
	} `json:"discount"`
	Hover *struct {
		TextZh string `json:"textZh"`
	} `json:"hover"`
	Schedule *struct {
		Daily []struct {
			Start string `json:"start"` // "23:00"
			End   string `json:"end"`   // "7:50"（可跨午夜）
		} `json:"daily"`
		Timezone   string `json:"timezone"`  // 实测恒 Asia/Shanghai
		ValidFrom  string `json:"validFrom"` // RFC3339，可缺省
		ValidUntil string `json:"validUntil"`
	} `json:"schedule"`
}

// promoZone 优惠时区：上游恒 Asia/Shanghai（UTC+8 无夏令时），用 FixedZone 免依赖
// 系统 tzdata（Windows 无 IANA 库时 LoadLocation 会失败）。
var promoZone = cst.Loc

// promoClock 解析 "HH:MM" 为当日分钟数；坏值返回 (-1, false)。
func promoClock(hhmm string) (int, bool) {
	parts := strings.Split(hhmm, ":")
	if len(parts) != 2 {
		return -1, false
	}
	h, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	m, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	// 小时上限必须是 23（bug #7）：允许 "24:00" 会返回 1440，若上游下发 start="24:00"
	// 则 st=1440 > ed → promoActive 走跨午夜分支 cur>=st||cur<ed，对大多数窗口恒真
	// → 优惠被判"全天生效"，面板显示错误的生效价。
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return -1, false
	}
	return h*60 + m, true
}

// promoActive 评估优惠在 now 是否生效：enabled + validFrom/validUntil 内 + 落在
// 任一 daily 窗口（支持跨午夜，如 23:00→7:50）。schedule 为 nil 视为全天生效。
func promoActive(p *v3ModelPromotion, now time.Time) bool {
	if !p.Enabled {
		return false
	}
	if sc := p.Schedule; sc != nil {
		if sc.ValidFrom != "" {
			from, err := time.Parse(time.RFC3339, sc.ValidFrom)
			if err == nil && now.Before(from) {
				return false
			}
		}
		if sc.ValidUntil != "" {
			until, err := time.Parse(time.RFC3339, sc.ValidUntil)
			if err == nil && !now.Before(until) {
				return false
			}
		}
		if len(sc.Daily) > 0 {
			cur := now.Hour()*60 + now.Minute()
			inWindow := false
			for _, w := range sc.Daily {
				st, ok1 := promoClock(w.Start)
				ed, ok2 := promoClock(w.End)
				if !ok1 || !ok2 {
					continue
				}
				if st <= ed {
					if cur >= st && cur < ed {
						inWindow = true
						break
					}
				} else if cur >= st || cur < ed { // 跨午夜（23:00→7:50）
					inWindow = true
					break
				}
			}
			if !inWindow {
				return false
			}
		}
	}
	return true
}

// v3ModelTier /v3/config data.modelTiers 单条付费层级（issue #235 实测两域各一条）：
// 一层级带一组 modelIds + 官方标识文案。只取 label —— requiredUserType 两域同值
// （"standard"）语义却不同，不能拿来判「能不能调」，判了就会误标。
type v3ModelTier struct {
	Enabled  bool     `json:"enabled"`
	Priority int      `json:"priority"`
	ModelIDs []string `json:"modelIds"`
	Badge    *struct {
		Label string `json:"label"`
	} `json:"badge"`
}

// applyModelTiers 给受限模型挂官方标识文案（同模型多条命中取 priority 最高）。
// 只标注不过滤：一次 /v1/models 是全池共享的，而层级是账号级属性，在列表接口上
// 没有「当前账号」，硬过滤会按池里最差的号降级掉所有人的选择。
func applyModelTiers(out map[string]ModelInfo, tiers []v3ModelTier) {
	if len(tiers) == 0 || len(out) == 0 {
		return
	}
	best := map[string]int{}
	for _, t := range tiers {
		if !t.Enabled || t.Badge == nil || strings.TrimSpace(t.Badge.Label) == "" {
			continue
		}
		for _, id := range t.ModelIDs {
			mi, ok := out[id]
			if !ok {
				continue // 目录外模型（如已下架）不挂
			}
			if p, seen := best[id]; seen && p >= t.Priority {
				continue
			}
			best[id] = t.Priority
			mi.Tier = t.Badge.Label
			out[id] = mi
		}
	}
}

// applyModelPromotions 把当前生效的优惠挂到目录条目：同模型多条命中取 priority
// 最高（实测 glm-5.2 白天 badge-only(50) 与夜间五折(100) 靠 priority+daily 双轨
// 切换）。无 discount 对象的条目也挂标签/说明（错峰类），PromoFactor 留 nil。
func applyModelPromotions(out map[string]ModelInfo, promos []v3ModelPromotion) {
	if len(promos) == 0 || len(out) == 0 {
		return
	}
	now := time.Now().In(promoZone)
	type cand struct {
		prio int
		p    *v3ModelPromotion
	}
	best := map[string]cand{}
	for i := range promos {
		p := &promos[i]
		if !promoActive(p, now) {
			continue
		}
		for _, id := range p.ModelIDs {
			if _, ok := out[id]; !ok {
				continue // 目录外模型（如同名 global 变体）不挂
			}
			if b, seen := best[id]; !seen || p.Priority > b.prio {
				best[id] = cand{prio: p.Priority, p: p}
			}
		}
	}
	for id, c := range best {
		mi := out[id]
		if c.p.Badge != nil {
			mi.PromoLabel = c.p.Badge.Label
		}
		if c.p.Hover != nil {
			mi.PromoNote = c.p.Hover.TextZh
		}
		if c.p.Discount != nil {
			f := c.p.Discount.Factor
			mi.PromoFactor = &f
			mi.PromoCredits = c.p.Discount.DiscountedCredits
		}
		out[id] = mi
	}
}

// storeEfforts 按 realm 写入 effort 能力缓存桶（efforts + defaultEfforts），并发安全。
// 供 CN FetchModels 与 global 探测共用：拉取到的模型档位落桶后，出站请求体
// normalizeReasoningEffort 才能按域降级。efforts 与 defs 均空时删除该 realm 桶
// （等价「该域无可降级档位」）。调用方负责在「无新数据」时跳过写。
func (c *Client) storeEfforts(realm string, efforts map[string][]string, defs map[string]string) {
	c.effortsMu.Lock()
	defer c.effortsMu.Unlock()
	if c.efforts == nil {
		c.efforts = make(map[string]map[string][]string)
	}
	if c.defaultEfforts == nil {
		c.defaultEfforts = make(map[string]map[string]string)
	}
	k := realmKey(realm)
	if len(efforts) == 0 && len(defs) == 0 {
		delete(c.efforts, k)
		delete(c.defaultEfforts, k)
		return
	}
	c.efforts[k] = efforts
	c.defaultEfforts[k] = defs
}

// normalizeModelRate 把上游倍率原文规范化为可比较的数值键。
// 兼容 "x0.05" / "x0.05 credits" / "0.50x" 等形态；无法数值化时保留去除
// credits 后缀与空白后的原文，避免编造倍率。
func normalizeModelRate(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if strings.HasSuffix(strings.ToLower(s), "credits") {
		s = strings.TrimSpace(s[:len(s)-len("credits")])
	}
	if strings.HasPrefix(strings.ToLower(s), "x") {
		s = strings.TrimSpace(s[1:])
	} else if strings.HasSuffix(strings.ToLower(s), "x") {
		s = strings.TrimSpace(s[:len(s)-1])
	}
	if s == "" {
		return ""
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return strings.TrimSpace(raw)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// effectiveModelRate 返回模型当前生效倍率：有机器可读优惠时取折扣价，
// 否则取牌价；两者均缺省时为空。
func effectiveModelRate(mi ModelInfo) string {
	if mi.PromoFactor != nil && strings.TrimSpace(mi.PromoCredits) != "" {
		return normalizeModelRate(mi.PromoCredits)
	}
	return normalizeModelRate(mi.Credits)
}

// storeModelRates 按 realm 整体替换模型倍率快照。目录成功刷新但没有可解析
// 倍率时写入空桶，使旧倍率不会继续冒充当前价。
func (c *Client) storeModelRates(realm string, infos []ModelInfo) {
	rates := make(map[string]string, len(infos))
	for _, mi := range infos {
		if mi.ID == "" {
			continue
		}
		if rate := effectiveModelRate(mi); rate != "" {
			rates[mi.ID] = rate
		}
	}
	c.effortsMu.Lock()
	defer c.effortsMu.Unlock()
	if c.modelRates == nil {
		c.modelRates = make(map[string]map[string]string)
	}
	c.modelRates[realmKey(realm)] = rates
}

// ModelRate 返回最近成功刷新的指定域模型生效倍率；未知返回空串。
func (c *Client) ModelRate(realm, model string) string {
	if c == nil || model == "" {
		return ""
	}
	c.effortsMu.RLock()
	defer c.effortsMu.RUnlock()
	return c.modelRates[realmKey(realm)][model]
}

// GlobalEffortSnapshot 导出 global 域 effort 能力缓存（探测下发 ∪ 静态兜底合并后的桶），
// 供 /v1/models 输出 reasoning_supported_efforts / reasoning_default_effort。
// 返回副本；桶未填充（无 global 账号或从未探测）→ nil（调用方回落静态兜底表）。
func (c *Client) GlobalEffortSnapshot() (efforts map[string][]string, defaults map[string]string) {
	return c.effortsSnapshot("global"), c.defaultEffortsSnapshot("global")
}

// v3ConfigDomain /v3/config 的 X-Domain：优先账号落盘 domain，否则 chatBase host。
func v3ConfigDomain(a *auth.Auth, chatBase string) string {
	if a != nil {
		// Domain 加锁快照（见 auth.DomainValue：keepalive 刷新在 a.mu 内改写）。
		if d := strings.TrimSpace(a.DomainValue()); d != "" {
			d = strings.TrimPrefix(d, "https://")
			d = strings.TrimPrefix(d, "http://")
			return strings.TrimSuffix(d, "/")
		}
	}
	if u, err := url.Parse(chatBase); err == nil && u.Host != "" {
		return u.Host
	}
	return "copilot.tencent.com"
}

// fetchV3ConfigModelMap 拉官方 IDE 配置目录，按模型 id 建能力表。
// 该端点对 UA 敏感：必须带 CodeBuddy/CodeBuddyIDE 版本，否则 400 code=12403。
// ua 为该次请求的 User-Agent；空串等价 codeBuddyIDEUA。该端点对 UA 敏感且**不同 UA 下发
// 不同模型集合**（见 codeBuddyCLIUA 注释），global 探测据此并发两路取并集。
func (c *Client) fetchV3ConfigModelMap(a *auth.Auth, ua string) (map[string]ModelInfo, error) {
	req, err := http.NewRequest(http.MethodGet, c.chatBase(a)+"/v3/config", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	// 先判空再解引用（BUG-05）：上一行直接调 a.AccessTokenValue()，下一行才判 a != nil，
	// 真传 nil 就是 panic —— 防御代码自相矛盾。同函数里的 v3ConfigDomain 是按可空处理的。
	if a == nil {
		return nil, nil
	}
	// AccessToken 加锁快照（同 fetchEnterpriseModels）。
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	req.Header.Set("X-Domain", v3ConfigDomain(a, c.chatBase(a)))
	req.Header.Set("X-Product", "SaaS")
	if ua == "" {
		ua = codeBuddyIDEUA
	}
	req.Header.Set("User-Agent", ua)
	c.injectCodeBuddyRequest(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		// 读失败 → 传输层错误：半截 body 不进解析（不罚号）。
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("v3/config status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Models []dynModelEntry `json:"models"`
			// 试用模型横幅：上游把「N 天免费试用」的模型放在这里，**不在 data.models 里**。
			// 实测 global 侧 hy4-preview-f 只出现在此（modelId=hy4-preview-f、
			// targetModelId=hy4-preview、trialDays=14），纯 data.models 解析会漏掉它。
			ProductFeaturesConfig struct {
				ModelTrialBanner struct {
					Banners []struct {
						ModelID       string `json:"modelId"`
						TargetModelID string `json:"targetModelId"`
					} `json:"banners"`
				} `json:"ModelTrialBanner"`
			} `json:"productFeaturesConfig"`
			ModelPromotions []v3ModelPromotion `json:"modelPromotions"`
			// ModelTiers 付费层级表（issue #235）：海外「Pro Only」= 专业版专属
			// （免费号选了就报错），国内「订阅优先」= 仅排队优先。清单与文案都在这，
			// 模型目录本身不会按账号权限裁剪。
			ModelTiers []v3ModelTier `json:"modelTiers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("v3/config parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("v3/config code=%d", env.Code)
	}
	out := make(map[string]ModelInfo, len(env.Data.Models))
	for _, m := range env.Data.Models {
		if strings.TrimSpace(m.ID) == "" {
			continue
		}
		out[m.ID] = m.modelInfo()
	}
	// 补入试用横幅模型（ModelTrialBanner）：上游把「N 天免费试用」的模型只放在这里，
	// data.models 里没有，故纯目录解析会漏（实测 global 侧 hy4-preview-f 即如此，
	// 但该模型**实际可调用**）。
	//
	// 元数据口径：能力字段（context/maxTokens/efforts/reasoning 等）从 targetModelId
	// 的既有条目继承——试用版与其转正目标是同族模型，能力应当一致；
	// 但 **Credits 与 Tags 显式清空**——它们描述的是"转正后"的计费与营销信息
	// （如 hy4-preview 的 x0.29 与 badge），用在免费试用版上会误导下游展示。
	//
	// firstUseTimeKey / trialDays 属**账号级**试用状态，不透出给下游。
	for _, b := range env.Data.ProductFeaturesConfig.ModelTrialBanner.Banners {
		id := strings.TrimSpace(b.ModelID)
		if id == "" {
			continue
		}
		if _, exists := out[id]; exists {
			continue
		}
		mi := ModelInfo{ID: id}
		if tgt := strings.TrimSpace(b.TargetModelID); tgt != "" {
			if base, ok := out[tgt]; ok {
				mi = base
				mi.ID = id
			}
		}
		mi.Credits = ""
		mi.Tags = nil
		out[id] = mi
	}
	// 挂当前生效的限时优惠（modelPromotions）：Credits 字段是**牌价**（转正后基准
	// 倍率，如 hy4-preview-f 的 x0.29），而 WorkBuddy 客户端显示的是生效价（试用/
	// 折扣窗口内 factor 打折）——面板据此展示「生效价 + 标签 + 牌价」。
	applyModelPromotions(out, env.Data.ModelPromotions)
	applyModelTiers(out, env.Data.ModelTiers)

	if len(out) == 0 {
		return nil, fmt.Errorf("v3/config returned empty models")
	}
	return out, nil
}

// UserResource 查询账号积分余额与总额度（所有套餐聚合）。remain 负值钳 0；
// total 取与 remain 同源的额度字段（CycleCapacitySize 优先，无周期额度退
// CapacitySize），上游缺 size 的套餐按 remain 兜底，保证百分比不超 100%。
// CreditPackage 单个积分包的构成明细（面板「积分构成」用）。
//
// 两个账号即使任务完成度完全一致，余额也可能相差上千——差别藏在包的**面额与
// 来源**里（「国内运营裂变包」「拉新权益包」按次发放，面额 6~1500 不等）。
// 只看聚合值看不出这件事，所以把逐包明细暴露出来。
type CreditPackage struct {
	Name   string `json:"name"`
	Remain int64  `json:"remain"`
	Used   int64  `json:"used"`
	Size   int64  `json:"size"`
	// EndTime 该包的失效时刻：优先 DeductionEndTime（可抵扣窗口结束，真「用不完
	// 就没了」），缺失依次回落 ExpiredTime / PackageEndTime / CycleEndTime（周期
	// 边界，仅兜底）。RFC3339 或上游墙钟字符串，前端取日期部分展示。
	EndTime string `json:"end_time,omitempty"`
	// ExpiresAt 与 EndTime 同源的 Unix 毫秒时间戳，供面板按精确剩余天数聚合。
	ExpiresAt int64 `json:"expires_at,omitempty"`
	// CreatedAt 发放时刻，RFC3339。**这是区分「首登赠送」与「活动奖励」的唯一依据**：
	// 两类包的 PackageName 与 PackageCode 完全相同（例如都是「国内运营裂变包」+
	// TCACA_code_007_*），只看名字无法区分，只有时间能说明它是不是账号首次授权那刻发的。
	CreatedAt string `json:"created_at,omitempty"`
	// PackageCode / SubProductCode 上游的包类型标识。同 Name 不同 Code 的包可能
	// 是不同来源；同 Code 不同面额则是同来源分批发放（首登 1500 与活动 300 即如此）。
	PackageCode    string `json:"package_code,omitempty"`
	SubProductCode string `json:"sub_product_code,omitempty"`
	SubProductName string `json:"sub_product_name,omitempty"`
	// Cycle 为 true 表示按周期发放的包（读 Cycle* 字段），否则读 Capacity*。
	Cycle bool `json:"cycle,omitempty"`
}

// CreditPackages 返回账号当前的逐包构成。remain/size 为各包求和。
//
// 字段选择与 UserResourceDetailed 的聚合口径一致：CycleCapacitySize > 0 时按
// 周期字段算，否则按 Capacity 字段算——两条路径不能混，否则同一个包会被算两次。
func (c *Client) CreditPackages(a *auth.Auth) ([]CreditPackage, int64, int64, error) {
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format(packageEndLayout),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format(packageEndLayout),
	}
	// 企业版分流（报告 #25）：企业额度不在「个人资源包」体系内，
	// get-user-resource* 对 enterpriseId 非空的号恒返回空 Accounts（实测 code 0 + Accounts null），
	// 面板「积分构成」对企业号永远为空。改用 get-enterprise-user-usage 合成一条周期额度。
	// 企业版与静态 API Key 同走企业口径（个人 get-user-resource 对二者均返回空 Accounts）。
	if a != nil && a.UsesEnterpriseBilling() {
		remain, total, _, earliestAt, _, eerr := c.enterpriseResource(a, 30*24*time.Hour)
		if eerr != nil {
			return nil, 0, 0, eerr
		}
		pkg := CreditPackage{
			Name:   "企业版周期额度",
			Remain: remain,
			Size:   total,
		}
		// 不限量时两个字段都是哨兵（total=-1、remain=1<<40），相减会得到 -1e12 量级的
		// 「已用」并原样透出（报告 #19）。此时不报已用，让面板走 isUnlimited 分支。
		if total >= 0 {
			pkg.Used = total - remain
		}
		if !earliestAt.IsZero() {
			pkg.EndTime = earliestAt.Format(time.RFC3339)
			pkg.ExpiresAt = earliestAt.UnixMilli()
		}
		return []CreditPackage{pkg}, remain, total, nil
	}
	data, err := c.billingMeterJSON(a, c.billingMeterPaths(a), http.MethodPost, body)
	if err != nil {
		return nil, 0, 0, err
	}
	// 注意层级：doJSON 已经解过 apiEnvelope 并返回 env.Data，所以这里从
	// Response 开始解析——**不能**再套一层 Code/Data，否则 Accounts 恒为空，
	// 表现为「每个号都 0 个包」（实测踩过）。
	var resp struct {
		Response struct {
			Data struct {
				Accounts []struct {
					PackageName         string `json:"PackageName"`
					CapacityRemain      int64  `json:"CapacityRemain"`
					CapacityUsed        int64  `json:"CapacityUsed"`
					CapacitySize        int64  `json:"CapacitySize"`
					CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
					CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
					CycleCapacitySize   int64  `json:"CycleCapacitySize"`
					// 到期时间字段名在上游存在三种口径：ExpiredTime / PackageEndTime
					// 在 CN/global 实测字段全集里均恒 miss（见 UserResourceDetailed
					// 处注释），真实下发的是 CycleEndTime——三者都读，谁有值用谁。
					ExpiredTime    string `json:"ExpiredTime"`
					PackageEndTime string `json:"PackageEndTime"`
					CycleEndTime   string `json:"CycleEndTime"`
					// DeductionEndTime 可抵扣窗口结束（epoch 毫秒）——「这个包什么时候
					// 不能再花」的真失效时刻。CycleEndTime 是周期边界（额度重置点），
					// 两者语义不同：判「用不完就没了」以本字段为准，CycleEndTime 兜底
					//（OkRoromori 分支实测结论：请求参数叫 PackageEndTimeRange*，但
					// 响应里 ExpiredTime 恒空，真正的失效时刻只有这里下发）。
					DeductionEndTime int64 `json:"DeductionEndTime"`
					// 发放时刻（epoch 毫秒）。
					CreateTime     int64  `json:"CreateTime"`
					PackageCode    string `json:"PackageCode"`
					SubProductCode string `json:"SubProductCode"`
					SubProductName string `json:"SubProductName"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, 0, 0, fmt.Errorf("packages parse: %w", err)
	}
	packs := resp.Response.Data.Accounts
	out := make([]CreditPackage, 0, len(packs))
	var sumRemain, sumSize int64
	for _, p := range packs {
		cp := CreditPackage{
			Name:           p.PackageName,
			PackageCode:    p.PackageCode,
			SubProductCode: p.SubProductCode,
			SubProductName: p.SubProductName,
		}
		switch {
		case p.DeductionEndTime > 0:
			// 真失效时刻（可抵扣窗口结束），语义见上方字段注释：判「用不完就没了」
			// 用它而不是周期边界。epoch 毫秒 → RFC3339，与 CycleEndTime 字符串口径
			// 共存（前端统一 slice(0,10) 取日期）。ExpiresAt 直接用原始毫秒——
			// RFC3339 不是 packageEndLayout 形态，交给下方解析会静默失败得 0。
			cp.EndTime = time.UnixMilli(p.DeductionEndTime).Format(time.RFC3339)
			cp.ExpiresAt = p.DeductionEndTime
		case p.ExpiredTime != "":
			cp.EndTime = p.ExpiredTime
		case p.PackageEndTime != "":
			cp.EndTime = p.PackageEndTime
		default:
			cp.EndTime = p.CycleEndTime
		}
		if cp.ExpiresAt == 0 && cp.EndTime != "" {
			if end, perr := time.ParseInLocation(packageEndLayout, cp.EndTime, softRateResetLoc); perr == nil {
				cp.ExpiresAt = end.UnixMilli()
			}
		}
		// CreateTime 是 epoch 毫秒；0 表示上游没给，留空而不是伪造 1970。
		if p.CreateTime > 0 {
			cp.CreatedAt = time.UnixMilli(p.CreateTime).Format(time.RFC3339)
		}
		// 与 UserResourceDetailed 共用同一份口径（bug #5）：此前这里内联了一份
		// 等价逻辑，但缺「remain > size 钳制」与「CycleUsed 反算 remain」两条。
		// 上游脏数据（CycleCapacityRemain > Size，或 CycleUsed 与 remain 不自洽）时，
		// 面板「积分构成」会显示"剩余 > 面额"，与「账号余额」列互相矛盾。
		remain, used, size := packageRemainUsed(respAccount{
			CapacityRemain:      p.CapacityRemain,
			CapacityUsed:        p.CapacityUsed,
			CapacitySize:        p.CapacitySize,
			CycleCapacityRemain: p.CycleCapacityRemain,
			CycleCapacityUsed:   p.CycleCapacityUsed,
			CycleCapacitySize:   p.CycleCapacitySize,
		})
		cp.Cycle = p.CycleCapacitySize > 0
		cp.Remain, cp.Used, cp.Size = remain, used, size
		sumRemain += cp.Remain
		sumSize += cp.Size
		out = append(out, cp)
	}
	// 面额降序：大包一眼可见，正是差异最可能出现的地方。
	sort.SliceStable(out, func(i, j int) bool { return out[i].Size > out[j].Size })
	return out, sumRemain, sumSize, nil
}

func (c *Client) UserResource(a *auth.Auth) (remain, total int64, err error) {
	remain, total, _, err = c.UserResourceDetailed(a, 0)
	return remain, total, err
}

// packageEndLayout 上游套餐到期时间的墙钟格式（UTC+8，与 softRateResetLoc 同口径）。
const packageEndLayout = "2006-01-02 15:04:05"

// parsePackageEndTime 统一解析上游套餐到期时间。空值、格式异常返回 false，
// 调用方据此保守地不把该包计入最早到期路由。
func parsePackageEndTime(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation(packageEndLayout, raw, softRateResetLoc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// UserResourceDetailed 在 UserResource 基础上额外返回「快过期」积分子集：
// soon > 0 且套餐 CycleEndTime 解析成功且到期时刻 ≤ now+soon 的余额计入 expiring
// （pool 据此优先消耗，避免官方活动赠送的奖励积分到期作废）；soon ≤ 0 时 expiring
// 恒 0（禁用分桶，行为与引入前一致）。expiring 是 remain 的一部分。
//
// 到期时间判据是 CycleEndTime（上游实测：CN/global 两域字段全集均无 PackageEndTime，
// 旧判据恒 miss 致 expiring 恒 0；CycleEndTime 是上游真实下发的到期时刻——
// global Bonus Pack 14 天赠送积分的到期时间即此字段）。解析失败/缺失的套餐保守
// 不计入 expiring（不误标为快过期而插队）。
// 单套餐取数统一调 packageRemainUsed（与 CreditPackages 同一事实来源，含 remain
// 钳 [0,size] 与 used 修正；消除双份逻辑漂移——旧中间 switch 只钳负值，上游脏数据
// CycleRemain>Size 时会高估）。
func (c *Client) UserResourceDetailed(a *auth.Auth, soon time.Duration) (remain, total, expiring int64, err error) {
	remain, total, expiring, _, _, err = c.UserResourceDetailedWithExpiry(a, soon)
	return remain, total, expiring, err
}

// enterpriseUnlimitedRemain / enterpriseUnlimitedTotal 企业版「不限量」在池内的等价表示
// （上游 limitNum == -1；桌面端同判为 unlimited）。
//
// 池内没有"不限"状态位，而 credits 同时参与两处判定——路由权重（credits/maxCredits）与
// credit_floor（余额低于阈值则不接收费模型）——不限量必须两处都"不构成限制"，故剩余量
// 取一个足够大的值；total 记 -1 供面板显示「不限」。total 仅用于展示与快照，
// 不参与任何判定（已核对全仓库引用面）。
const (
	enterpriseUnlimitedRemain int64 = 1 << 40
	enterpriseUnlimitedTotal  int64 = -1
)

// enterpriseResource 查询**企业版账号**的分配额度，并归一到个人口径的
// (remain, total, expiring, earliestAt, earliestRemaining)。
//
// 为什么必须分流：企业额度不在「个人资源包」体系内——/billing/meter/get-user-resource*
// 对 enterpriseId 非空的账号恒返回空 Accounts（实测 code 0 且 Accounts null），
// 面板因此长期显示 0/未知（额度其实是有的）。
//
// 上游口径（实测 2026-10-07，企业号 a6239ec8）：
//
//	POST /v2/billing/meter/get-enterprise-user-usage   （X-Enterprise-Id 由 BillingHeaders 注入）
//	{"credit":812.45,"limitNum":2000,
//	 "cycleStartTime":"2026-09-27 00:00:00","cycleEndTime":"2026-10-26 23:59:59",
//	 "cycleResetTime":"2026-10-27 00:00:00"}
//
// credit = **本周期已用**（与个人口径 CapacityRemain「剩余」语义相反），
// limitNum = **分配给本账号的额度**。故 remain = limitNum - credit、total = limitNum。
// 注意：企业**池**总额度是另一回事，成员账号无权查询（实测所有池端点 403 not_authorized），
// 本函数只反映该账号被分配的额度。
//
// 分桶：企业配额按周期重置、未用完即作废，与个人「奖励积分到期作废」同性质，故把
// cycleEndTime 作为唯一到期批次——周期末企业号会被 prefer_expiring 优先选中（期望行为）。
// limitNum < 0（不限量）无作废语义，不参与分桶。
func (c *Client) enterpriseResource(a *auth.Auth, soon time.Duration) (remain, total, expiring int64, earliestAt time.Time, earliestRemaining int64, err error) {
	return c.enterpriseResourceCtx(context.Background(), a, soon)
}

// enterpriseResourceCtx 带 ctx（bug #3）：SIGTERM 时重试的 2s+4s 不再睡满。
func (c *Client) enterpriseResourceCtx(ctx context.Context, a *auth.Auth, soon time.Duration) (remain, total, expiring int64, earliestAt time.Time, earliestRemaining int64, err error) {
	// 与个人口径一致：余额是签到后的紧邻调用，偶发 500 值得有界重试。
	var data json.RawMessage
	err = c.retryBillingTransientCtx(ctx, func() error {
		var e error
		data, e = c.billingJSONCtx(ctx, a, http.MethodPost, "/v2/billing/meter/get-enterprise-user-usage", map[string]any{})
		return e
	})
	if err != nil {
		return 0, 0, 0, time.Time{}, 0, err
	}
	// 兼容上游两套字段命名（桌面端两条解析路径分别读 camelCase 与 snake_case）。
	var resp struct {
		Credit         float64 `json:"credit"`
		LimitNum       int64   `json:"limitNum"`
		LimitNumSnake  int64   `json:"limit_num"`
		UsedNum        float64 `json:"used_num"`
		CycleEndTime   string  `json:"cycleEndTime"`
		CycleResetTime string  `json:"cycleResetTime"`
	}
	if uerr := json.Unmarshal(data, &resp); uerr != nil {
		return 0, 0, 0, time.Time{}, 0, fmt.Errorf("enterprise resource parse: %w", uerr)
	}
	limit := resp.LimitNum
	if limit == 0 && resp.LimitNumSnake != 0 {
		limit = resp.LimitNumSnake
	}
	used := resp.Credit
	if used == 0 && resp.UsedNum != 0 {
		used = resp.UsedNum
	}
	// 不限量：池内无对应状态位，用大剩余量让路由权重与 credit_floor 都不构成限制。
	if limit < 0 {
		return enterpriseUnlimitedRemain, enterpriseUnlimitedTotal, 0, time.Time{}, 0, nil
	}
	// credit 是浮点（如 812.45），池内 credits 是整数：四舍五入到最近整数
	// （向下取整会低报剩余额度，与"额度还有多少"的展示意图相悖）。
	usedInt := int64(used)
	if used-float64(usedInt) >= 0.5 {
		usedInt++
	}
	remain = limit - usedInt
	if remain < 0 {
		remain = 0
	}
	total = limit
	// 周期到期批次：cycleEndTime 优先，缺失回落 cycleResetTime。
	now := time.Now()
	end, ok := parsePackageEndTime(resp.CycleEndTime)
	if !ok {
		end, ok = parsePackageEndTime(resp.CycleResetTime)
	}
	if ok && remain > 0 && end.After(now) {
		earliestAt, earliestRemaining = end, remain
		if soon > 0 && !end.After(now.Add(soon)) {
			expiring = remain
		}
	}
	return remain, total, expiring, earliestAt, earliestRemaining, nil
}

// UserResourceDetailedWithExpiry 在 UserResourceDetailed 基础上返回最早未来到期批次：
// earliestAt 是最早的可用到期时刻，earliestRemaining 是同一时刻所有正余额包的剩余量之和。
// 已过期、剩余为 0、缺少或无法解析到期时间的包都不会成为最早批次；无有效批次时返回零值。
func (c *Client) UserResourceDetailedWithExpiry(a *auth.Auth, soon time.Duration) (remain, total, expiring int64, earliestAt time.Time, earliestRemaining int64, err error) {
	return c.UserResourceDetailedWithExpiryCtx(context.Background(), a, soon)
}

// UserResourceDetailedWithExpiryCtx 带 ctx（bug #3）：停机时重试的 2s+4s 可被打断。
func (c *Client) UserResourceDetailedWithExpiryCtx(ctx context.Context, a *auth.Auth, soon time.Duration) (remain, total, expiring int64, earliestAt time.Time, earliestRemaining int64, err error) {
	// 企业版额度不在「个人资源包」体系内：get-user-resource* 对 enterpriseId 非空账号
	// 恒返回空 Accounts（实测 code 0 且 Accounts null），故分流到企业口径端点。
	if a.UsesEnterpriseBilling() {
		remain, total, expiring, earliestAt, earliestRemaining, err = c.enterpriseResourceCtx(ctx, a, soon)
		c.notifyCredits(a, remain, err)
		return
	}
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format(packageEndLayout),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format(packageEndLayout),
	}
	// 余额查询同样做瞬时错误有界重试（签到后紧接着的 user-resource 偶发 500 会让
	// 该账号错过本次解冻/到期快照更新，只能等下一个刷新周期）。
	var data json.RawMessage
	err = c.retryBillingTransientCtx(ctx, func() error {
		var e error
		data, e = c.billingMeterJSONCtx(ctx, a, c.billingMeterPaths(a), http.MethodPost, body)
		return e
	})
	if err != nil {
		return 0, 0, 0, time.Time{}, 0, err
	}
	var resp struct {
		Response struct {
			Data struct {
				Accounts []struct {
					PackageName string `json:"PackageName"`
					// DeductionEndTime 可抵扣窗口结束（epoch 毫秒）= **真失效时刻**（bug-02）。
					// 与 CreditPackages 的口径一致：CycleEndTime 只是周期边界（额度重置点），
					// 判「用不完就没了」必须以本字段为准，否则快过期的包拿不到 ×3 加权。
					DeductionEndTime    int64  `json:"DeductionEndTime"`
					ExpiredTime         string `json:"ExpiredTime"`
					PackageEndTime      string `json:"PackageEndTime"`
					CycleEndTime        string `json:"CycleEndTime"` // "2006-01-02 15:04:05"，缺省/空 = 无到期
					CapacitySize        int64  `json:"CapacitySize"`
					CapacityRemain      int64  `json:"CapacityRemain"`
					CapacityUsed        int64  `json:"CapacityUsed"`
					CycleCapacitySize   int64  `json:"CycleCapacitySize"`
					CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
					CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, 0, 0, time.Time{}, 0, fmt.Errorf("resource parse: %w", err)
	}
	// 空 Accounts 与「真实余额为 0」不可区分（报告 #3）：上游偶尔回 200 + Accounts 缺失，
	// 此前照样按 remain=0 往下走 —— notifyCredits 会把它当成一次真实观测，
	// 积分台账里就多出一条「余额归零」的假流水，池里也会把号标成没积分。
	// 这里明确区分：没有 Accounts 条目 = **没有观测**，返回 0 但**不通知**。
	hasAccounts := len(resp.Response.Data.Accounts) > 0
	for _, acct := range resp.Response.Data.Accounts {
		r, _, size := packageRemainUsed(respAccount{
			CapacityRemain:      acct.CapacityRemain,
			CapacityUsed:        acct.CapacityUsed,
			CapacitySize:        acct.CapacitySize,
			CycleCapacityRemain: acct.CycleCapacityRemain,
			CycleCapacityUsed:   acct.CycleCapacityUsed,
			CycleCapacitySize:   acct.CycleCapacitySize,
		})
		if r < 0 {
			r = 0
		}
		if size < r {
			size = r
		}
		remain += r
		total += size
		if r <= 0 {
			continue
		}
		// 真失效时刻优先（bug-02）：DeductionEndTime（ms）> ExpiredTime > PackageEndTime
		// > CycleEndTime 兜底。与 CreditPackages 的解析同序，两条链路不再各说各话。
		var end time.Time
		var ok bool
		switch {
		case acct.DeductionEndTime > 0:
			end, ok = time.UnixMilli(acct.DeductionEndTime), true
		case acct.ExpiredTime != "":
			end, ok = parsePackageEndTime(acct.ExpiredTime)
		case acct.PackageEndTime != "":
			end, ok = parsePackageEndTime(acct.PackageEndTime)
		default:
			end, ok = parsePackageEndTime(acct.CycleEndTime)
		}
		if !ok || !end.After(now) {
			continue
		}
		if earliestAt.IsZero() || end.Before(earliestAt) {
			earliestAt = end
			earliestRemaining = r
		} else if end.Equal(earliestAt) {
			earliestRemaining += r
		}
		// 分桶：仅 soon>0 且确实在窗口内 → expiring。
		if soon > 0 && !end.After(now.Add(soon)) {
			expiring += r
		}
	}
	if hasAccounts {
		c.notifyCredits(a, remain, nil)
	}
	return remain, total, expiring, earliestAt, earliestRemaining, nil
}

// respAccount 供 packageRemainUsed 解析的套餐字段（CreditPackages 的逐包结构同构）。
type respAccount struct {
	CapacityRemain      int64
	CapacityUsed        int64
	CapacitySize        int64
	CycleCapacityRemain int64
	CycleCapacityUsed   int64
	CycleCapacitySize   int64
}

// packageRemainUsed 聚合单套餐的 remain/used/size（与 CreditPackages/cmd/credit 的
// 历史口径一致，收敛至此作为单一事实来源）。Cycle 期套餐优先：用 CycleCapacity
// 三字段，used 取 CycleUsed 与 size-remain 的较大者；否则回退 Capacity 三字段。
func packageRemainUsed(a respAccount) (remain, used, size int64) {
	if a.CycleCapacitySize > 0 {
		remain = a.CycleCapacityRemain
		size = a.CycleCapacitySize
		if remain < 0 {
			remain = 0
		}
		if remain > size {
			remain = size
		}
		used = size - remain
		if a.CycleCapacityUsed > used {
			used = a.CycleCapacityUsed
		}
		// 先把 used 钳到 size、再由 used 反算 remain（bug #4）：此前只在 size>=used 时
		// 回算，上游对账延迟给出 CycleCapacityUsed > CycleCapacitySize 时 remain 保持旧值
		// → remain+used > size，面板出现「剩余 50 / 已用 150 / 面额 100」这种自相矛盾的行，
		// credit_floor 与选号权重也跟着高估余额。
		if used > size {
			used = size
		}
		remain = size - used
		return remain, used, size
	}
	// 非 Cycle 分支同样钳制（bug #2）：上游脏数据（CapacityRemain > Size，常见于
	// 对账延迟 / 包已过期未清）会高估 credits → 选号权重 credits/maxCredits 与
	// credit_floor 判定失真，面板还会显示"剩余 > 面额"。函数注释承诺了钳 [0,size]，
	// 但此前只实现了 Cycle 那一半。
	remain = a.CapacityRemain
	used = a.CapacityUsed
	size = a.CapacitySize
	if remain < 0 {
		remain = 0
	}
	if size > 0 && remain > size {
		remain = size
	}
	if used == 0 && size > remain {
		used = size - remain
	}
	// used 也要钳（BUG-07）：上游对账延迟时 CapacityUsed > CapacitySize，
	// 不钳就会出现「剩余 50 / 已用 150 / 面额 100」这种自相矛盾的行。
	if size > 0 && used > size {
		used = size
	}
	if size > 0 && remain+used > size {
		remain = size - used
	}
	return remain, used, size
}

// DailyCheckin 执行每日签到。已签到（业务 code 非 0）也返回错误，调用方按 msg 区分。
// 偶发上游 5xx（code 10000）做有界重试（见 retryBillingTransient）——单次抖动不再
// 让该账号整天漏签；「已签到」等业务错误不重试。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	return c.DailyCheckinCtx(context.Background(), a)
}

// DailyCheckinCtx 带 ctx（bug #3）：停机时不必把重试的 2s+4s 睡满。
func (c *Client) DailyCheckinCtx(ctx context.Context, a *auth.Auth) error {
	return c.retryBillingTransientCtx(ctx, func() error {
		_, err := c.billingMeterJSONCtx(ctx, a, c.checkinMeterPaths(a), http.MethodPost, map[string]any{})
		return err
	})
}

// IsAlreadyCheckin 报告 err 是否表示"今天已签到"（上游幂等拒绝重复签到）。
// 只认带分类的 *Error（业务 code 或 HTTP 错误）：网络层/解析层错误不得当作幂等成功，
// 否则停机补签遇到抖动会误记为 already，账号当天实际未签到却被判定正常。
func IsAlreadyCheckin(err error) bool {
	var ue *Error
	if !errors.As(err, &ue) {
		return false
	}
	msg := strings.ToLower(ue.Msg)
	for _, m := range alreadyCheckinMarkers {
		if strings.Contains(strings.ToLower(ue.Msg), strings.ToLower(m)) {
			// 命中幂等文案后还要排除反向语义（bug B4）。
			negated := false
			for _, n := range alreadyNegativeMarkers {
				if strings.Contains(msg, n) {
					negated = true
					break
				}
			}
			if !negated {
				return true
			}
		}
	}
	return false
}

// businessCodeStatus 把「HTTP 200 + 业务 code」映射成等价的语义 HTTP 状态（bug #10）。
// 下游的模型阻塞 / prompt 超长 / 图片非法判定都按状态码设门槛，不合成就会退化成
// ErrClient（只换号不罚、错误码也不对）。返回 0 = 未知码，保持原状态。
func businessCodeStatus(code int) int {
	switch code {
	case 11102: // 模型不存在/不可用 → 与 404 同语义
		return http.StatusNotFound
	case 11115: // prompt is too long → 与 413 同语义
		return http.StatusRequestEntityTooLarge
	case 11135: // 图片格式/数据错误 → 与 400 同语义
		return http.StatusBadRequest
	case 11101: // 参数错误
		return http.StatusBadRequest
	case 14018: // 积分耗尽
		return http.StatusPaymentRequired
	case 12153:
		// 会话失效：refresh 端点会回 **HTTP 200 + 信封 code=12153**（报告 #4）。
		// 映射成 401 后，Classify 的 sessionDead marker（"12153"）才会命中 ——
		// 此前 businessCodeStatus 返回 0、状态停在 200，被 `status != 401 && != 403`
		// 门槛跳过，最终落 ErrClient 走熔断三振，NoteSessionDead 永不触发。
		return http.StatusUnauthorized
	}
	return 0
}

func truncate(s string, n int) string {
	return logfmt.Truncate(s, n)
}

// isQueueFullBody 判定渠道级容量满载文案（6021 queue.full 家族，上游 PR #185）。
// 该形态与具体账号无关：换号在任何时刻都撞同一堵墙，正确处置是不罚号不轮转。
func isQueueFullBody(lower string) bool {
	return strings.Contains(lower, "queue.full") || strings.Contains(lower, "queue full")
}

// accountRestrictedMsg 账号级限制与通道校验失败共用的文案（上游 PR #233）。
// 注意它**不能**当通用 marker：11‑-128（通道校验）与 11140（账号限制）都是这句。
const accountRestrictedMsg = "request illegal"

// isChannelRefused 11‑-128 通道校验失败（code + 文案双条件）。
func isChannelRefused(body string) bool {
	if !strings.Contains(strings.ToLower(body), accountRestrictedMsg) {
		return false
	}
	// 业务码归一比较（11‑-128 / 11‑-128 / 11‑-128 视为同一个）。
	return hasBusinessCode(body, "11‑-128")
}

// IsAccountRestricted 11140 账号级限制（code + 文案双条件）：账号被上游风控限制，
// 解除只能走官方渠道申诉，重登无效 —— 与 11‑-128 通道校验失败严格区分。
func IsAccountRestricted(body string) bool {
	lower := strings.ToLower(body)
	if !strings.Contains(lower, accountRestrictedMsg) {
		return false
	}
	// 11140 也承载**模型级限流**文案（displayMsg "The model provider is
	// rate-limiting requests."）：那时账号健康，属软限流。code + 文案双条件仍会命中，
	// 故必须显式排除限流语义 —— 误禁用的代价远高于一次短冷却。
	for _, m := range softRateMarkers {
		if strings.Contains(lower, m) {
			return false
		}
	}
	return hasBusinessCode(body, "11140")
}

// CreditObserver 余额查询观察者：每次**真实查到**余额后回调（uid + 余额绝对值）。
// 旁路通知，不改变任何透传字节；未挂载时零开销。用于积分历史留痕
// （internal/credithist）：余额查询是「积分变动」唯一可靠的观测口 —— 签到 / 活跃
// 上报等渠道上游不落金额日志，只能靠比对余额覆盖。（qcgzxw `826aaded`）
type CreditObserver func(uid string, credits int64)

// SetCreditObserver 挂载/注销（nil）余额观察者。可在启动后任意时刻调用；
// 观察者必须自身并发安全且快速返回 —— 它在余额查询的调用栈上同步执行。
func (c *Client) SetCreditObserver(fn CreditObserver) {
	if c == nil {
		return
	}
	if fn == nil {
		c.creditObserver.Store(nil)
		return
	}
	c.creditObserver.Store(&fn)
}

// notifyCredits 查询成功后旁路通知观察者。**失败不是观测值**：网络抖动返回的 0
// 余额若被留痕，会在下一次成功时造出一条假变动。
func (c *Client) notifyCredits(a *auth.Auth, credits int64, err error) {
	if c == nil || err != nil || a == nil {
		return
	}
	fn := c.creditObserver.Load()
	if fn == nil {
		return
	}
	// 观察者是纯旁路：它 panic 绝不能打断余额刷新/签到这条主流程（调度器 goroutine
	// 里未恢复的 panic 会直接带走整个进程）。
	defer func() {
		if r := recover(); r != nil {
			log.Printf("WARN: [upstream] 余额观察者 panic 已忽略: %v", r)
		}
	}()
	(*fn)(a.UID, credits)
}
