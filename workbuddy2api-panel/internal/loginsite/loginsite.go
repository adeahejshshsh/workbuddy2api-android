// Package loginsite 官方登录站点表与登录协议常量（对齐 CodeBuddy CLI 的
// product.json 域表与 auth 模块码表）。
//
// 为什么单独一个包：设备授权链路有**两个**调用方——面板内嵌流程
// （internal/panel/login.go）与命令行工具（cmd/login）。域表、UA 版本、登录码
// 语义一旦在两处各写一份，就会出现「面板能登、CLI 工具不能登」这种最难查的分叉。
// 所有官方口径收敛在这里，两处只做流程编排。
package loginsite

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// ClientVersion 官方 CLI 版本。UA 与授权链接的 version 参数同源。
//
// 对齐 2026-10-26 的 @tencent-ai/codebuddy-code@2.176.0（npm dist-tag latest）。
// 官方 CLI 把它自己的 package version 写进授权链接的 version，同时以
// ?platform=CLI&version= 打 console/auth/risk-context —— 版本串确实会进风控判定，
// 落后两个小版本（2.174.0，10-25 发布）时用户侧登录页按旧客户端渲染并埋点。
// 上游 CLI 日更，这个常量要跟着 dist-tag latest 走。
const ClientVersion = "2.176.0"

// ClientUA 设备授权链路的 User-Agent（CodeBuddy CLI 家族形态）。
//
// 仅作**未复核站点**的兜底：WorkBuddy 桌面端（5.11.3 国内 / 5.9.4 国际）的设备码
// 三端点实测发的是桌面端 RPC 族 UA，见 Site.UserAgent。
const ClientUA = "CLI/" + ClientVersion + " CodeBuddy/" + ClientVersion

// WorkBuddy 桌面端版本与内置 CLI 版本（真机 UA 段）。
//
// 2026-10-30 拆包：cli/product.json genieVersion / package.json version
// + resources/install-manifest.json appVersion 三处一致。
// CLI 段 = 该包 cli/package.json 的 @genie/agent-cli，**随包走**：CN 5.11.3 带
// 2.176.0，国际分发包最新仍停在 5.9.4（带 2.165.0）。
const (
	ClientVersionWorkBuddyCN  = "5.11.3"
	ClientVersionWorkBuddyGL  = "5.9.4"
	ClientVersionBuiltinCLICN = "2.176.0"
	ClientVersionBuiltinCLIGL = "2.165.0"
)

// 站点标识（对外 API 取值）。
const (
	SiteCN        = "cn"        // 国内站：API copilot.tencent.com，Origin www.codebuddy.cn
	SiteIntl      = "intl"      // 官方国际站：www.codebuddy.ai（product.json externalDomain）
	SiteWorkBuddy = "workbuddy" // 旧桌面端国际版：www.workbuddy.ai
)

// Realm 取值（池内分域标识，与 auth.Realm() 输出一致）。
const (
	RealmCN     = "cn"
	RealmGlobal = "global"
)

// Site 一个登录站点的全部端点要素。
type Site struct {
	ID     string
	Realm  string // 落盘 realm：cn / global
	API    string // 上游 API base（三端点同域）
	Origin string // Origin/Referer（仅授权页跳转用；设备码三端点**不发**，见 Headers）
	// Platform 授权页 platform 口径。官方只对 platform=workbuddy 追加
	// ww_device_nonce（桌面端设备绑定），其余站点只追加 version。
	Platform string
	// UserAgent 设备码三端点（auth/state、auth/token、login/account）的 UA。
	//
	// 2026-10-30 真机实测：这三条走的是桌面端 **RPC 族** UA，与业务链路同源，
	// 不是 `CLI/<ver> CodeBuddy/<ver>`：
	//
	//	CN 5.11.3  WorkBuddy/5.11.3 WorkBuddy/5.11.3 CLI/2.176.0
	//	GL 5.9.4   workbuddy-ai/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0
	//
	// 空 = 保留 ClientUA（CodeBuddy CLI 家族），仅供未复核的站点兜底。
	UserAgent string
	// Domain 设备码端点的 X-Domain 取值（**前端主机名**，不是 API base）。
	//
	// 真机 CN 5.9.3：请求打到 copilot.tencent.com，但 `x-domain: www.workbuddy.cn`
	// —— 该头声明「哪个前端在调用」，与落到哪台机器无关。
	// 空 = 回落 Origin 的主机名。
	Domain string
	// Version 授权链接的 version 参数（官方客户端写自己的包版本）。
	//
	// 2026-11-14 真机实测（CN 5.9.3，Chrome 地址栏）：官方链接带 `version=5.9.3`，
	// 授权页把这个值原样转给 `console/auth/risk-context?platform=workbuddy&version=5.9.3`
	// 并编进回调地址——它确实被读取，不是装饰。空 = 不加（未复核站点）。
	Version string
}

// 官方域表。两个国际站实测是同一后端（2026-10-24：同一 token 打两域，
// /billing/meter/get-user-resource-summary 返回完全相同的 PackageCode 与容量），
// 差别只在主机名与 WAF 口径。
//
// Platform 是设备授权 `?platform=` 的取值，来自各分发包 `product.json` 的
// authentication.attributes.platform（**不是** 顶层 platform 字段，那个是 "CLI"）。
// 2026-10-30 真机实测：
//
//	CN 5.11.3  POST /v2/plugin/auth/state?platform=workbuddy
//	GL 5.9.4   POST /v2/plugin/auth/state?platform=workbuddy-ai
//
// 旧实现一律发 platform=CLI —— 取了顶层字段，两个真机形态都不符。
var (
	siteCN = Site{
		ID: SiteCN, Realm: RealmCN,
		API: "https://copilot.tencent.com", Origin: "https://www.codebuddy.cn",
		Platform:  "workbuddy",
		UserAgent: "WorkBuddy/" + ClientVersionWorkBuddyCN + " WorkBuddy/" + ClientVersionWorkBuddyCN + " CLI/" + ClientVersionBuiltinCLICN,
		Domain:    "www.workbuddy.cn",
		Version:   ClientVersionWorkBuddyCN,
	}
	siteIntl = Site{
		ID: SiteIntl, Realm: RealmGlobal,
		API: "https://www.codebuddy.ai", Origin: "https://www.codebuddy.ai",
		// codebuddy.ai 分发包本轮未抓包，但它是 **同一个** WorkBuddy AI 国际包
		// （product.json externalDomain 首位，platform=workbuddy-ai）。这里沿用
		// workbuddy.ai 站点的真机口径，而不是退回 CodeBuddy CLI 家族：
		// 该包 externalDomain 里 codebuddy.ai 与 workbuddy.ai 并列，同一份 auth 属性。
		Platform:  "workbuddy-ai",
		UserAgent: "workbuddy-ai/" + ClientVersionWorkBuddyGL + " workbuddy-ai/" + ClientVersionWorkBuddyGL + " CLI/" + ClientVersionBuiltinCLIGL,
		Domain:    "www.codebuddy.ai",
		Version:   ClientVersionWorkBuddyGL,
	}
	siteWorkBuddy = Site{
		ID: SiteWorkBuddy, Realm: RealmGlobal,
		API: "https://www.workbuddy.ai", Origin: "https://www.workbuddy.ai",
		Platform:  "workbuddy-ai",
		UserAgent: "workbuddy-ai/" + ClientVersionWorkBuddyGL + " workbuddy-ai/" + ClientVersionWorkBuddyGL + " CLI/" + ClientVersionBuiltinCLIGL,
		Domain:    "www.workbuddy.ai",
		Version:   ClientVersionWorkBuddyGL,
	}
)

// For 按站点标识返回站点定义；空/未知 → 国内站（零回归）。
func For(id string) Site {
	switch strings.ToLower(strings.TrimSpace(id)) {
	case SiteIntl:
		return siteIntl
	case SiteWorkBuddy:
		return siteWorkBuddy
	}
	return siteCN
}

// Default 缺省站点（国内站）。
func Default() Site { return siteCN }

// ForRealm 按 realm 映射站点：global → 旧桌面端 workbuddy.ai（历史行为，保证
// 只传 realm 的老调用方语义不变），cn → 国内站。
//
// 要官方国际站请显式用 SiteIntl（codebuddy.ai）。
func ForRealm(realm string) Site {
	if strings.TrimSpace(realm) == RealmGlobal {
		return siteWorkBuddy
	}
	return siteCN
}

// ValidID 报告 id 是否是已知站点标识。
func ValidID(id string) bool {
	switch strings.ToLower(strings.TrimSpace(id)) {
	case SiteCN, SiteIntl, SiteWorkBuddy:
		return true
	}
	return false
}

// Endpoints 返回设备授权三端点（auth/state、token、account）与 Origin。
//
// `platform` 取站点自己的 Platform（真机 CN=workbuddy / workbuddy.ai=workbuddy-ai），
// 不再硬编码 CLI——见 var 块上方注释。
func (s Site) Endpoints() (state, token, account, origin string) {
	platform := s.Platform
	if platform == "" {
		platform = "workbuddy"
	}
	return s.API + "/v2/plugin/auth/state?platform=" + platform,
		s.API + "/v2/plugin/auth/token?state=",
		s.API + "/v2/plugin/login/account?state=",
		s.Origin
}

// EndpointHeaders 设备码三端点的公共出站头族（真机形态）。
//
// 2026-10-30 双版本逐头核对，三端点（auth/state、auth/token、login/account）共同拥有：
//
//	accept: application/json, text/plain, */*
//	user-agent: <Site.UserAgent>（RPC 族；空则回落 ClientUA）
//	x-product: SaaS
//	x-request-id: 32 hex 纯随机（无时间前缀）
//
// **不含 Content-Type**：只有 POST（auth/state）带 `application/json`；两条 GET
// （auth/token、login/account）真机没有该头——GET 无请求体，带上是多余形态。
//
// 且**不发** Origin / Referer / X-Requested-With —— 旧实现在这里发了三者，等于给
// 「桌面端 node fetch」盖上一层浏览器痕迹（浏览器同源语义只属于「打开授权页」那一步）。
//
// 三条端点的**差异**（content-type、x-domain、x-no-* 哨兵、x-device-token）见
// HeadersForState / HeadersForToken / HeadersForAccount，不要在这里抹平。
func (s Site) EndpointHeaders() map[string]string {
	return map[string]string{
		"Accept":     "application/json, text/plain, */*",
		"User-Agent": s.userAgent(),
		"X-Product":  "SaaS",
		// X-Request-ID：真机三端点恒带（每条一个，纯随机 32 hex，无毫秒前缀）。
		"X-Request-ID": newTraceID(),
	}
}

// newTraceID 生成 32 hex 纯随机请求 ID（真机 X-Request-ID 形态：无毫秒前缀）。
//
// 不复用 internal/session.NewTraceID 是刻意的：那会把 redisstore 整条依赖链拉进
// loginsite（登录站表只需要一个随机串，不该背着会话存储）。熵源故障时回落时间派生，
// 仍恒 32 hex，不 panic、不空串。
func newTraceID() string {
	var b [16]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		ms := time.Now().UnixMilli()
		return fmt.Sprintf("%012x%020x", ms, uint64(ms))
	}
	return hex.EncodeToString(b[:])
}

// userAgent 站点 UA：真机口径优先，未复核站点回落 ClientUA。
func (s Site) userAgent() string {
	if s.UserAgent != "" {
		return s.UserAgent
	}
	return ClientUA
}

// host 设备码端点的 X-Domain 取值：站点显式 Domain 优先，否则回落 Origin 主机名。
//
// **不是 API base 的主机名**：真机 CN auth/state 的 `x-domain: www.workbuddy.cn`，
// 而该请求实际打到 copilot.tencent.com —— 该头声明「哪个前端在调用」，与落到哪台
// 机器无关。2026-10-30 实测：CN 5.9.3 → www.workbuddy.cn；GL 5.9.4 → www.workbuddy.ai。
func (s Site) host() string {
	if s.Domain != "" {
		return s.Domain
	}
	h := strings.TrimPrefix(strings.TrimPrefix(s.Origin, "https://"), "http://")
	return strings.TrimSuffix(h, "/")
}

// sentinelNo 未鉴权/未落库端点的 x-no-* 哨兵头。
//
// 真机值是字符串 "true"（不是 "1"）；auth/state 与 auth/token 四个全发，
// login/account 只发后三个（它已带 Bearer，唯一缺的是「账号信息还没落库」）。
func sentinelNo(noAuth bool) map[string]string {
	out := map[string]string{
		"X-No-User-Id":         "true",
		"X-No-Enterprise-Id":   "true",
		"X-No-Department-Info": "true",
	}
	if noAuth {
		out["X-No-Authorization"] = "true"
	}
	return out
}

// HeadersForState POST /v2/plugin/auth/state 的出站头。
//
// 真机（CN 5.9.3 / GL 5.9.4 同形）：
//
//	公共头 + x-domain + 四个 x-no-* + x-custom-channel-id: unknown
//	+ x-device-token（取到才发；空则不发，不伪造）
func (s Site) HeadersForState(deviceToken string) map[string]string {
	h := s.EndpointHeaders()
	// 只有 POST 带 Content-Type（真机两条 GET 都没有）。
	h["Content-Type"] = "application/json"
	h["X-Domain"] = s.host()
	// x-custom-channel-id 真机恒为字面量 "unknown"（渠道号未知的分发包）。
	h["X-Custom-Channel-Id"] = "unknown"
	for k, v := range sentinelNo(true) {
		h[k] = v
	}
	if deviceToken != "" {
		h["X-Device-Token"] = deviceToken
	}
	return h
}

// HeadersForToken GET /v2/plugin/auth/token 的出站头。
//
// 真机**没有 x-domain**（与 state / account 不同，这是唯一缺它的端点）：
// 公共头 + 四个 x-no-*，无 x-device-token、无 x-custom-channel-id。
func (s Site) HeadersForToken() map[string]string {
	h := s.EndpointHeaders()
	for k, v := range sentinelNo(true) {
		h[k] = v
	}
	return h
}

// HeadersForAccount GET /v2/plugin/login/account 的出站头。
//
// 真机：公共头 + x-domain + Authorization + 三个 x-no-*（**无** x-no-authorization，
// 但仍有 x-no-user-id —— token 已发、uid 还没落库，这正是该端点的语义）。
func (s Site) HeadersForAccount(accessToken string) map[string]string {
	h := s.EndpointHeaders()
	h["X-Domain"] = s.host()
	for k, v := range sentinelNo(false) {
		h[k] = v
	}
	if accessToken != "" {
		h["Authorization"] = "Bearer " + accessToken
	}
	return h
}

// 官方 global 域主机（按 auth 文件 domain 选用，不按 realm 猜）。
const (
	BaseOfficial  = "https://www.codebuddy.ai" // 官方国际站
	BaseWorkBuddy = "https://www.workbuddy.ai" // 旧桌面端国际版
)

// BaseForDomain 按 auth 文件的 domain 返回 global 域上游 base：
//
//	*.codebuddy.ai → BaseOfficial
//	*.workbuddy.ai → BaseWorkBuddy
//	其它/空        → ""（调用方回落配置或默认值，不猜域）
//
// 国际域两套主机名并存（老账号 workbuddy.ai、官方 CLI 新登录 codebuddy.ai），
// 用 realm 决定 base 必然把一半账号打到非其签发域。
func BaseForDomain(domain string) string {
	d := strings.ToLower(strings.TrimSpace(domain))
	switch {
	case d == "codebuddy.ai" || strings.HasSuffix(d, ".codebuddy.ai"):
		return BaseOfficial
	case d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai"):
		return BaseWorkBuddy
	}
	return ""
}

// 官方登录业务码（CLI auth 模块枚举）。语义分两类，混为一谈会让用户在本该立刻
// 报错的场景白等满 TTL：
//
//	11217 RetryFetchToken   / 12151 RetryFetchAccount  → 还没登完，继续轮询
//	12005 LicenseSeatLimit  / 11212 LicenseExpired / 11216 TrialExpired → 终态
const (
	CodeRetryFetchToken   = 11217
	CodeRetryFetchAccount = 12151
	CodeLicenseSeatLimit  = 12005
	CodeLicenseExpired    = 11212
	CodeTrialExpired      = 11216
)

// CodePending 报告该业务码是否表示「登录进行中，应继续轮询」。
func CodePending(code int) bool {
	return code == CodeRetryFetchToken || code == CodeRetryFetchAccount
}

// CodeFatal 报告该业务码是否属于官方定义的终态许可错误（SignLicenseError）：
// 官方对这三类直接把上游 msg 抛给用户并结束登录，不再轮询。
func CodeFatal(code int) bool {
	switch code {
	case CodeLicenseSeatLimit, CodeLicenseExpired, CodeTrialExpired:
		return true
	}
	return false
}

// DecorateAuthURL 按真机实测补齐授权链接参数（对应官方的 appendHostLoginJourneyParams /
// appendAuthUrlParams / appendLoginPageHintParams）：
//
//	version=<site.Version>      官方写自己的包版本（CN 5.11.3 / GL 5.9.4，与 UA 同源）；
//	                            CN 授权页会把它转给 console/auth/risk-context 并编进回调
//	loginSessionId=<id>         官方客户端本地生成的登录会话标识；只对 platform=workbuddy
//	                            （CN）追加，授权页读它并原样带进 signin-callback
//	ww_device_nonce=<nonce>     官方仅 platform=workbuddy 且拿到设备 nonce 时加
//
// 2026-11-14 两侧真机链接原文（脱敏）：
//
//	CN : /login?platform=workbuddy&state=…&version=5.11.3&loginSessionId=<uuid>
//	          &traceparent=00-…&login_owner_id=<uuid>&ww_device_nonce=<nonce>
//	GL : /login/?platform=workbuddy-ai&state=…&version=5.9.4
//
// traceparent / login_owner_id 是客户端自己的链路与宿主标识，本仓没有等价物，
// 不伪造（错值比缺值更容易被判异常）。
//
// 本仓没有桌面端 Turing 设备 SDK，nonce 只能由部署侧提供；为空则不加。
// raw 解析失败时原样返回。
func DecorateAuthURL(raw string, s Site, nonce, sessionID string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	if s.Version != "" && q.Get("version") == "" {
		q.Set("version", s.Version)
	}
	// loginSessionId 与 nonce 同一道闸：官方 appendHostLoginJourneyParams 只对
	// platform=workbuddy 生效，真机国际版链接（platform=workbuddy-ai&version=5.9.4）没有它。
	if s.Platform == "workbuddy" && sessionID != "" && q.Get("loginSessionId") == "" {
		q.Set("loginSessionId", sessionID)
	}
	if s.Platform == "workbuddy" && nonce != "" && q.Get("ww_device_nonce") == "" {
		q.Set("ww_device_nonce", nonce)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// NewLoginSessionID 生成 uuid v4 形态的登录会话标识。
//
// 官方由客户端本地生成、只做透传（授权页读它再编进回调），不是服务端签发的凭证，
// 所以本地随机即可；用 v4 形状只是为了与真机同一形态。
func NewLoginSessionID() string {
	var b [16]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		return ""
	}
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
