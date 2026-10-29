// Package headers 构造三类上游请求头（common / chat / billing / refresh）。
// 规则来自 docs/api-reference.md §0/§4/§6。
package upstream

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
)

const (
	// defaultClientVersion 出站 WorkBuddy 客户端版本段（UA 的 `WorkBuddy/<ver>` 与
	// 白名单头组的 X-IDE-Version）。对齐官方 WorkBuddy Desktop 分发包版本（5.5.4）。
	// config upstream.client_version 可覆盖（空 = 内置默认）。
	defaultClientVersion = "5.9.3"
	// defaultCliVersion 出站 UA 中 `CLI/<ver>` 段版本。对齐官方内置 CLI（2.137.1）。
	// config upstream.cli_version 可覆盖（空 = 内置默认）。
	defaultCliVersion = "2.165.0"

	originRefererCN     = "https://www.codebuddy.cn"
	originRefererGlobal = "https://www.workbuddy.ai"
)

// originRefererFor 按账号 realm 返回 Origin/Referer 基础域：
// global → https://www.workbuddy.ai；cn（含全局开关未开）→ https://www.codebuddy.cn。
// HeaderSafe 清洗成 header-safe 值：去掉控制字符与 DEL，保留可打印字符。
// header value 里出现 CR/LF 会让 net/http 拒绝整个请求（bug #16）。
func HeaderSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

func originRefererFor(a *auth.Auth) string {
	if a != nil && a.IsGlobal() {
		return originRefererGlobal
	}
	return originRefererCN
}

// clientVersion 生效的 WorkBuddy 客户端版本：Client.ClientVersion 非空则取之，
// 否则内置默认 defaultClientVersion。
func (c *Client) clientVersion() string {
	if c != nil && c.ClientVersion != "" {
		return c.ClientVersion
	}
	return defaultClientVersion
}

// cliVersion 生效的 CLI 版本：Client.CliVersion 非空则取之，否则内置默认 defaultCliVersion。
func (c *Client) cliVersion() string {
	if c != nil && c.CliVersion != "" {
		return c.CliVersion
	}
	return defaultCliVersion
}

// defaultClientVersionGlobal / defaultCliVersionGlobal 国际版（global realm）版本段。
// CN 与 intl 是两条独立发版线：2026-10 实测 CN WorkBuddy 5.8.3（内嵌 CLI 2.161.0）、
// intl 5.8.4（同属 CLI 2.161.0 系列），故按账号 realm 分档。
const (
	defaultClientVersionGlobal = "5.8.4"
	defaultCliVersionGlobal    = "2.161.0"
)

// clientVersionFor / cliVersionFor 按账号 realm 取生效版本段。
// 优先级：realm 专用显式配置（仅 global）> 通用显式配置 > realm 内置默认。
func (c *Client) clientVersionFor(a *auth.Auth) string {
	if a != nil && a.IsGlobal() && c != nil && c.ClientVersionGlobal != "" {
		return c.ClientVersionGlobal
	}
	if c != nil && c.ClientVersion != "" {
		return c.ClientVersion
	}
	if a != nil && a.IsGlobal() {
		return defaultClientVersionGlobal
	}
	return defaultClientVersion
}

func (c *Client) cliVersionFor(a *auth.Auth) string {
	if a != nil && a.IsGlobal() && c != nil && c.CliVersionGlobal != "" {
		return c.CliVersionGlobal
	}
	if c != nil && c.CliVersion != "" {
		return c.CliVersion
	}
	if a != nil && a.IsGlobal() {
		return defaultCliVersionGlobal
	}
	return defaultCliVersion
}

// defaultWorkBuddyUAFor 组装默认客户端出站 UA（官方桌面端 RestOperations 层形状）：
// `WorkBuddy/<clientVersion> <platform>/<clientVersion> CLI/<cliVersion>`。
// 平台段（第二段）品牌按 realm 切换——CN 用 applicationName 同值 `WorkBuddy`，
// global 用官方国际版 productName `WorkBuddy AI`（intl 项目逆向证据：
// `WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/5.5.2`）。
// global 账号送错平台段（`WorkBuddy` 非 `WorkBuddy AI`）可能触发上游 403 code 11140
// "request illegal" 风控。官方无任何 UA 随机化，故默认确定性。
func (c *Client) defaultWorkBuddyUAFor(a *auth.Auth) string {
	// 平台段**恒为字面量 `WorkBuddy`**（2026-10 解包官方 NSIS 安装包的 app.asar 实测）：
	// 国际版 productName 虽为 "WorkBuddy AI"、applicationName="workbuddy-ai"，出站 UA
	// 第二段仍是 `WorkBuddy`。此前我们按 realm 切成 `WorkBuddy AI` 的结论是错的
	// （依据是第三方逆向笔记，不是安装包本体）—— 送错平台段正是 11140 风控的诱因之一。
	// 版本段按 realm 分档（CN 5.8.3 / intl 5.8.4）。
	ver := c.clientVersionFor(a)
	return "WorkBuddy/" + ver + " WorkBuddy/" + ver + " CLI/" + c.cliVersionFor(a)
}

// defaultWorkBuddyUA 返回 CN 形态的默认 UA（默认账号形态即 CN，零回归兼容既有调用/测试）。
func (c *Client) defaultWorkBuddyUA() string {
	return c.defaultWorkBuddyUAFor(nil)
}

// userAgent 返回当前出站 UA（客户端出站路径：chat/refresh/FetchModels）。
// 优先级：Client.UserAgent（config user_agent）显式覆盖 > 按账号 realm 的默认 WorkBuddy 三段式。
// 显式覆盖兼容既有覆盖逻辑：用户配了即以用户值为准（自定义品牌/版本），
// 未配则走官方桌面端默认形态（global 换 `WorkBuddy AI` 平台段）。
func (c *Client) userAgent(a *auth.Auth) string {
	if c != nil && c.UserAgent != "" {
		return c.UserAgent
	}
	return c.defaultWorkBuddyUAFor(a)
}

// billingUA 白名单类（billing/checkin/banner）出站 UA：单段 `WorkBuddy/<clientVersion>`
// （官方 banner 显式覆写形态，不带 CLI 段）。默认生效（伪造官方桌面端指纹）；
// 显式 client_name="SaaS" 才不设 UA（还原旧行为，Go 默认 UA）。
func (c *Client) billingUA(a *auth.Auth) string {
	if c == nil || c.attributionClientName() == "SaaS" {
		return ""
	}
	// 按账号 realm 取版本段（报告 #3）：global 号的 billing UA 此前恒为 CN 版本。
	return "WorkBuddy/" + c.clientVersionFor(a)
}

// resolveDeviceToken 解析本次请求的 X-Device-Token 取值。
// 优先级：auth.Auth.DeviceToken（每号）> Client.DeviceToken（config 全局）> 文件兜底。
// 三者皆空/读失败则返回空串（调用方不注入该头，优雅降级）。
func (c *Client) resolveDeviceToken(a *auth.Auth) string {
	if a != nil && a.DeviceToken != "" {
		return a.DeviceToken
	}
	if c != nil && c.DeviceToken != "" {
		return c.DeviceToken
	}
	if c != nil && c.DeviceTokenFile != "" {
		return readDeviceTokenFile(c.DeviceTokenFile)
	}
	return ""
}

// injectDeviceToken 在 req 注入 X-Device-Token 头（仅当取到非空 token）。
func (c *Client) injectDeviceToken(req *http.Request, a *auth.Auth) {
	if tok := c.resolveDeviceToken(a); tok != "" {
		req.Header.Set("X-Device-Token", tok)
	}
}

// deriveAccountStableID 按 uid + 用途盐稳定派生 36 hex 设备/会话标识。
// 跨重启稳定（固定盐 "wb2a:"，不随进程换——这是与 session 包派生盐的本质差异：
// 那是会话键维度的进程级随机盐，重启换新；本函数是账号维度，必须跨重启恒定）、
// 账号间互异（uid 不同则不同）、同 uid 同用途恒同值（幂等）。用 sha256 与项目
// 既有派生（session/ids.go、cache_key.go）保持一致；截 36 hex 提供更长熵。
//
// 两个用途：
//   - purpose="machine" → X-Machine-ID（设备级，跨会话稳定）
//   - purpose="session" → X-Session-ID（账号固定会话，跨重启稳定）
//
// 与 injectDeviceToken 的 X-Device-Token 并存不冲突：那是登录时上游签发的
// 真实设备令牌（有则发，权威）；本对头是「每账号一台固定虚拟设备」的稳定指纹，
// 防多号被上游按设备指纹缺失/漂移关联风控。两者是不同头族，官方桌面端都发。
func deriveAccountStableID(uid, purpose string) string {
	sum := sha256.Sum256([]byte("wb2a:" + purpose + ":" + uid))
	return hex.EncodeToString(sum[:18]) // 36 hex chars
}

// injectAccountStableHeaders 在 req 注入 X-Machine-ID / X-Session-ID：按 uid 稳定
// 派生，跨重启固定、账号间互异。uid 为空时不注入（匿名请求无设备标识，上游不要求）。
func (c *Client) injectAccountStableHeaders(req *http.Request, a *auth.Auth) {
	if a == nil || a.UID == "" {
		return
	}
	req.Header.Set("X-Machine-ID", deriveAccountStableID(a.UID, "machine"))
	req.Header.Set("X-Session-ID", deriveAccountStableID(a.UID, "session"))
}

// CommonHeaders 设置所有 API 共享的请求头。
// agentRuntimeLocal 出站 X-Agent-Runtime 取值（桌面端本地 agent）。
const agentRuntimeLocal = "local-agent"

func (c *Client) CommonHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-Runtime", agentRuntimeLocal)
	// Accept 非流式默认 application/json（D6：去掉宽松的 text/plain, */*）。
	// chat 流式路径在 ChatHeaders 覆盖为 event-stream。
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	origin := originRefererFor(a)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", c.userAgent(a))
	// X-CodeBuddy-Request: 1（官方客户端风控闸门头，所有 API 请求必带，D1）。
	req.Header.Set("X-CodeBuddy-Request", "1")
	// Accept-Language 按 realm 切（D5）：CN zh-CN，global en-US。官方客户端按账号域
	// 发对应语言标识，对齐避免上游风控按语言缺失误判。
	req.Header.Set("Accept-Language", acceptLanguageFor(a))
	// X-Machine-ID / X-Session-ID：按 uid 稳定派生的账号级设备头（见
	// injectAccountStableHeaders）。注入在 CommonHeaders——chat 经 ChatHeaders
	// 叠加 CommonHeaders 天然继承；billing 域另行注入，全出站覆盖。
	// X-Machine-ID / X-Session-ID 已移除（2026-10-28 上游 5.9.3 真机 + 静态反编译
	// 双重确认）：客户端**从不发这两个头**——app.asar 与内置 cli 全量字符串里
	// X-Machine-ID 命中 0 次，X-Session-ID 只作为常量表条目存在；真机 40 个出站头
	// 逐一比对也没有它们。设备标识在 /v2/report 的 **body** 里，不在头上。
	// 旧实现按 uid 派生两个 36 hex 塞进头里，是纯粹的网关自造指纹。
}

// acceptLanguageFor 按账号 realm 返回 Accept-Language：global → en-US，cn → zh-CN。
func acceptLanguageFor(a *auth.Auth) string {
	if a != nil && a.IsGlobal() {
		return "en-US"
	}
	return "zh-CN"
}

// injectCodeBuddyRequest 在 req 注入 X-CodeBuddy-Request: 1。
// billing 域未走 CommonHeaders，单独注入保证全出站覆盖（D1）。
func (c *Client) injectCodeBuddyRequest(req *http.Request) {
	req.Header.Set("X-CodeBuddy-Request", "1")
}

// ChatMeta 一次 chat 出站的会话头族元数据（issue #35：后台按 X-Conversation-Request-ID
// 聚合请求，官方客户端一次 user send 内所有 tool call/重试/换号复用同一个 ID）。
// handler 在轮转循环外生成 conversationID / conversationRequestID，循环内每次出站
// 复用同值；TraceID 透传入站值（空 = 回落 conversationRequestID）。
// messageID（消息级，每条独立）由 ChatHeaders 内部生成，无需外部可见。
type ChatMeta struct {
	ConversationID        string // X-Conversation-ID：body 提取的入站值，空则不发（透传优先，不伪造）
	ConversationRequestID string // X-Conversation-Request-ID / X-Root-Request-ID：聚合主键，必发
	TraceID               string // X-Trace-ID：入站透传值，空则回落 conversationRequestID
	// CacheKeyID prompt_cache_key 的会话段来源（网关派生的粘性会话键 sessKey；
	// sessKey 为空时由调用方用内容派生键填入）。只影响缓存槽，不影响出站头。
	CacheKeyID string
}

// ChatHeaders 在 common 之上加 chat 专属的账号头。
// 缺省字段用 X-No-* 约定（与 CodeBuddy 官方 CLI 一致）。
// clientIP 为本次请求的客户端 IP（按参数传递，不读共享字段——避免并发串扰）；
// PassthroughIP=false 或 clientIP 为空时不注入 IP 头。
// meta 为会话头族元数据（纯新增，不改既有头），见 injectConversationHeaders。
func (c *Client) ChatHeaders(req *http.Request, a *auth.Auth, clientIP string, meta ChatMeta) {
	c.CommonHeaders(req, a)
	// chat 流式 Accept 覆盖 CommonHeaders 的非流式默认（D6）。
	req.Header.Set("Accept", "application/json, text/event-stream")
	// CLI 客户端没有浏览器语义：chat 路径从不发 Origin / Referer / Accept-Language
	// （两版客户端真机逐头对照）。CommonHeaders 为其它路径设了它们，这里剥掉。
	req.Header.Del("Origin")
	req.Header.Del("Referer")
	req.Header.Del("Accept-Language")
	// 官方桌面端 chat 头组（2026-10 解包 app.asar 实测）：X-Product 恒为 SaaS
	// （deploymentType || "SaaS"），IDE 名单独走 X-IDE-Name=WorkBuddy，并带
	// X-Private-Data: true。此前主 chat 路径只发了 UA，缺这四项 —— 与官方客户端
	// 的头组不一致，是 11140 风控的诱因之一。
	// 显式 client_name="SaaS" 保持旧行为（只 X-Product，不设 X-IDE-*）。
	if c.attributionClientName() != "SaaS" {
		// OpenAI Node SDK 指纹头族（misakano7545 `8f7366f2`，两版官方真机对齐）：
		// 官方桌面端是 Electron + `openai` SDK，这批头是 SDK 自己加的，**缺失本身就是
		// 破绽**（真机有、我们没有）。取值照抄实测：js / 6.25.0 / Windows / x64 /
		// node / v22.21.1 / Retry-Count 0（首跳恒 0）。
		req.Header.Set("X-Stainless-Lang", "js")
		req.Header.Set("X-Stainless-Package-Version", "6.25.0")
		req.Header.Set("X-Stainless-OS", "Windows")
		req.Header.Set("X-Stainless-Arch", "x64")
		req.Header.Set("X-Stainless-Runtime", "node")
		req.Header.Set("X-Stainless-Runtime-Version", "v22.21.1")
		req.Header.Set("X-Stainless-Retry-Count", "0")
		// 官方 chat 头组里的 agent 归因（另两处在 desktop 专家路径已设）。
		req.Header.Set("X-Agent-Type", "main")
		req.Header.Set("X-Agent-Intent", "craft")
		req.Header.Set("X-Product", "SaaS")
		req.Header.Set("X-IDE-Name", "WorkBuddy")
		req.Header.Set("X-IDE-Type", "WorkBuddy")
		req.Header.Set("X-IDE-Version", c.clientVersionFor(a))
		req.Header.Set("X-Private-Data", "true")
	}
	// AccessToken 加锁快照：keepalive 定时刷新会在 a.mu 内改写它，锁外直读构成数据竞争
	// （见 auth.AccessTokenValue 注释）。
	if at := a.AccessTokenValue(); at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	// 安全红线：绝不在 chat 请求里携带 X-Refresh-Token。
	// 企业与域头按 realm 分发：CN 走既有分支（EnterpriseID/Domain 原样透传，缺省 X-No-*）；
	// global 账号由 injectGlobalChatHeaders 统一覆写为国际客户端形态
	// （X-No-Enterprise-Id=1 声明无企业 + X-Domain=www.workbuddy.ai 声明国际版域），
	// 且不回退 X-Domain 到登录会话原值——对齐 intl 项目出站头。
	if a != nil && !a.IsGlobal() {
		if a.EnterpriseID != "" {
			req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		} else {
			req.Header.Set("X-No-Enterprise-Id", "1")
		}
		if d := a.DomainValue(); d != "" {
			req.Header.Set("X-Domain", d)
		} else {
			req.Header.Set("X-No-Department-Info", "1")
		}
	} else {
		c.injectGlobalChatHeaders(req, a)
	}
	// 用量归属头：默认伪造 WorkBuddy 桌面端指纹（client_name="SaaS" 还原旧行为）。
	c.injectAttribution(req, a)
	// 客户端 IP 透传（仅 PassthroughIP=true 且本次请求带 IP）。
	c.injectClientIP(req, clientIP)
	// 设备风控头：auth 每号 > config 全局 > 文件兜底；空则不注入。
	c.injectDeviceToken(req, a)
	// 会话头族（对话/请求/消息/B3 链路），见 injectConversationHeaders。
	c.injectConversationHeaders(req, meta)
	c.injectBaggage(req, meta)
}

// injectGlobalChatHeaders global 账号（无企业 ID）的 chat 专属声明头，对齐 intl 项目
// （ANALYSIS-global-chat-solutions.md）：
//   - X-No-Enterprise-Id: 1  个人账号无企业 ID，显式声明（避免上游按缺省/可疑判定）
//   - X-Domain: www.workbuddy.ai  显式声明国际版域（与 Origin/Referer 同域）
//
// 仅 global realm 注入；CN 账号走既有 X-No-Department-Info 等分支，零回归。
func (c *Client) injectGlobalChatHeaders(req *http.Request, a *auth.Auth) {
	if a == nil || !a.IsGlobal() {
		return
	}
	req.Header.Set("X-No-Enterprise-Id", "1")
	req.Header.Set("X-Domain", "www.workbuddy.ai")
}

// injectConversationHeaders 注入官方客户端会话头族（issue #35 后台聚合）。
// 头族分四层，各司其职：
//   - X-Conversation-ID：会话级，多轮稳定（body 的 conversationId）。空则不发——
//     透传客户端原值优先，客户端没给就不伪造，避免误导后台建错会话。
//   - X-Conversation-Request-ID：**对话轮级聚合主键**，必发。一次 user send 内的
//     所有 tool call/重试/换号/降级复用同一个 → 后台按它聚合成一条（不再碎片化）。
//   - X-Conversation-Message-ID = X-Request-ID：消息级，每条独立（32 位 hex）。
//   - X-Root-Request-ID：= conversationRequestID（根请求追踪）。
//   - X-Trace-ID：入站透传或 = conversationRequestID。
//   - X-B3-TraceId / X-B3-SpanId / X-B3-Sampled：链路族。B3 规范只认 16/32 hex
//     TraceId 与 16 hex SpanId；入站 conversationRequestID 非法时 TraceId 回落
//     messageID（恒 32 hex），SpanId 取 messageID[:16]（每消息新）。
//
// injectBaggage 注入 W3C `baggage` 头，对齐客户端 chat 出站形态（5.9.3 真机逐字）：
//
//	baggage: buddyId=,buddyName=,codebuddy.session_id=<X-Conversation-ID 同值>,
//	         codebuddy.conversation_request_id=<X-Conversation-Request-ID 同值>,
//	         x-agent-runtime=<X-Agent-Runtime 同值>,x-agent-intent=<X-Agent-Intent 同值>
//
// 客户端由 OTel W3CBaggagePropagator 序列化，空值保留 `k=` 形态，所以 buddyId /
// buddyName 恒为空串但仍列在表里（不是漏填）。两个 codebuddy.* 键取本次出站真实值，
// 与同名头保持一致 —— 头体不一致本身就是可判别形态。
// x-agent-runtime / x-agent-intent 也承载在 baggage 里：客户端解析场景时从 baggage
// 读这两项，不发等于告诉上游「这个运行时不知道自己是谁」。
func (c *Client) injectBaggage(req *http.Request, meta ChatMeta) {
	conv := meta.ConversationID
	if conv == "" {
		conv = req.Header.Get("X-Conversation-ID")
	}
	reqID := meta.ConversationRequestID
	if reqID == "" {
		reqID = req.Header.Get("X-Conversation-Request-ID")
	}
	req.Header.Set("baggage", "buddyId=,buddyName=,codebuddy.session_id="+conv+
		",codebuddy.conversation_request_id="+reqID+
		",x-agent-runtime="+req.Header.Get("X-Agent-Runtime")+
		",x-agent-intent="+req.Header.Get("X-Agent-Intent"))
}

func (c *Client) injectConversationHeaders(req *http.Request, meta ChatMeta) {
	convReqID := meta.ConversationRequestID
	if convReqID == "" {
		// 零值 meta（直接调 ChatHeaders 的调用方/测试）也要保证聚合主键必发：
		// 本级补一个 32 hex，调用方（handler）已生成稳定的不走到这里。
		convReqID = session.NewMessageID()
	}
	messageID := session.NewMessageID()
	if meta.ConversationID != "" {
		// 清洗成 header-safe（bug #16）：conversation_id 来自客户端 JSON 原文，
		// 带换行/其它控制字符时 net/http 会在写请求时报 "invalid header field value"
		// → 传输层错误 → 轮转/503，而客户端本身只是 ID 里带了奇怪字符。
		req.Header.Set("X-Conversation-ID", HeaderSafe(meta.ConversationID))
	}
	// 客户端传来的会话头必须清洗（bug 9）：原值含 \r/\n 时 net/http 在写请求阶段
	// 才报 "invalid header field value" → 传输层错误 → 每次轮转都失败 → 最终 503。
	// X-Conversation-ID 早就做了，这两个漏了。
	req.Header.Set("X-Conversation-Request-ID", HeaderSafe(convReqID))
	req.Header.Set("X-Conversation-Message-ID", messageID)
	req.Header.Set("X-Request-ID", messageID)
	req.Header.Set("X-Root-Request-ID", HeaderSafe(convReqID)) // 与 297/305 同口径（报告 #5）
	traceID := meta.TraceID
	if traceID == "" {
		traceID = convReqID
	}
	req.Header.Set("X-Trace-ID", HeaderSafe(traceID))
	b3Trace := convReqID
	if !validTraceID(b3Trace) {
		b3Trace = messageID // 非法 B3 TraceId → 回落恒 32 hex 的消息级 ID
	}
	req.Header.Set("X-B3-TraceId", b3Trace)
	req.Header.Set("X-B3-SpanId", messageID[:16])
	req.Header.Set("X-B3-Sampled", "1")
}

// validTraceID 判断 B3 TraceId 是否合法：16 或 32 位 hex（大小写均可）。
// 官方客户端生成的 conversationRequestId 是 32 位 hex（UUID 去横线），入站透传值
// 可能是任意形状（含横线/超长/非 hex），直接塞进 B3 头会破坏链路关联（issue #35）。
func validTraceID(s string) bool {
	if len(s) != 16 && len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')) {
			return false
		}
	}
	return true
}

// attributionClientName 生效的用量归属名：ClientName 非空取之；
// 空默认 "WorkBuddy"（伪造官方桌面端指纹；显式配 "SaaS" 可还原旧行为）。
func (c *Client) attributionClientName() string {
	if c != nil && c.ClientName != "" {
		return c.ClientName
	}
	return "WorkBuddy"
}

// injectAttribution 注入用量归属头（X-Agent-Purpose / X-IDE-* / X-Product）。
// 仅在 chat/completions 路径生效（ChatHeaders 调用）。
//
// 默认（ClientName 空）即对齐官方 WorkBuddy 桌面端指纹：X-Agent-Purpose="conversation"
// + X-IDE-Name/Type/Product="WorkBuddy" + X-IDE-Version=client_version，上游用量归因
// 不再出现 client/agentPurpose 为空的「网关特征」。显式 ClientName="SaaS" 还原旧行为
// （仅 X-Product="SaaS"，不设 X-IDE-*）；配其他值则四头跟随该值。
func (c *Client) injectAttribution(req *http.Request, a *auth.Auth) {
	name := c.attributionClientName()
	if name == "SaaS" {
		req.Header.Set("X-Product", "SaaS")
		return
	}
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-IDE-Name", name)
	req.Header.Set("X-IDE-Type", name)
	// 版本段必须按账号 realm 取（报告 #3）：ChatHeaders 刚按 realm 写好 5.8.4，
	// 这里用不带 realm 的 clientVersion() 会覆盖成 CN 默认 5.8.3 —— global 号的
	// UA 是 5.8.4 而 X-IDE-Version 是 5.8.3，两个头自相矛盾。
	req.Header.Set("X-IDE-Version", c.clientVersionFor(a))
	req.Header.Set("X-Product", name)
}

// injectClientIP 在 PassthroughIP 开启时把 clientIP 参数透传给上游（三等价头）。
func (c *Client) injectClientIP(req *http.Request, clientIP string) {
	if c == nil || !c.PassthroughIP || clientIP == "" {
		return
	}
	req.Header.Set("X-Forwarded-For", clientIP)
	req.Header.Set("X-Real-IP", clientIP)
	req.Header.Set("X-Client-IP", clientIP)
}

// ExtractClientIP 从入站请求提取客户端 IP 首段（X-Forwarded-For 首段，回落 X-Real-IP）。
func ExtractClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' {
				return strings.TrimSpace(xff[:i])
			}
		}
		return strings.TrimSpace(xff)
	}
	if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
		return real
	}
	return ""
}

// BillingHeaders billing 接口请求头。
// UA 语义：默认**不设置**（保持现状，Go 客户端自带默认 UA）；仅当显式配置
// c.UserAgent 非空才覆盖——避免默认路径给 billing 引入新的 UA 指纹。
func (c *Client) BillingHeaders(req *http.Request, a *auth.Auth) {
	// AccessToken 加锁快照（同 ChatHeaders：keepalive 可在 a.mu 内改写）。
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	c.injectCodeBuddyRequest(req)
	// Accept-Language 按 realm 切（D5，billing 域未走 CommonHeaders，单独注入）。
	req.Header.Set("Accept-Language", acceptLanguageFor(a))
	if c != nil && c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	} else if ua := c.billingUA(a); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if d := a.DomainValue(); d != "" {
		req.Header.Set("X-Domain", d)
	}
	// 设备风控头：billing 域（report/travel/balance/checkin）同样注入。
	c.injectDeviceToken(req, a)
}

// RefreshHeaders refresh 端点专属头（X-Refresh-Token 只允许出现在这里）。
func (c *Client) RefreshHeaders(req *http.Request, a *auth.Auth) {
	c.CommonHeaders(req, a)
	// 加锁读（bug #13）：同文件其它头都走 AccessTokenValue/DomainValue/RefreshTokenValue，
	// 这一处是唯一直读 —— 一旦调用方按签名传池内的 *auth.Auth（AuthByUID 返回的
	// 就是同一对象），就会与 RefreshToken 在 a.mu 内改字段形成数据竞争。
	req.Header.Set("X-Refresh-Token", a.RefreshTokenValue())
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	}
	// X-Auth-Refresh-Source 对齐官方客户端 refresh 渠道标识 "plugin"（D3）。
	req.Header.Set("X-Auth-Refresh-Source", "plugin")
}
