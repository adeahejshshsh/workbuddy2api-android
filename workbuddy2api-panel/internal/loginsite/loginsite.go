// Package loginsite 官方登录协议常量：站点表、UA 版本、登录业务码语义。
//
// 为什么单独一个包：设备授权链路有**两个**调用方 —— 面板内嵌流程
// （internal/panel/login.go）与命令行工具（cmd/login）。UA 版本、登录码语义一旦在
// 两处各写一份，就会出现「面板能登、CLI 工具不能登」这种最难查的分叉。
//
// 与官方 CLI 2.174.0 的差异（本仓**未**接入的部分，别按官方文档猜）：
// 官方站点表有第四个成员 intl（www.codebuddy.ai，product.json externalDomain）。
// 本仓现有账号都是 cn / workbuddy 两站签发的，intl 未接入 —— 加进来要真实登录实测，
// 没有可登录账号时贸然接会做出「能发起登录、poll 必然失败」的半成品。
// 路由侧（auth.isGlobalDomain）已认 *.codebuddy.ai：手写 auth 文件导入的 intl 账号
// 能正常走 global，只是面板没有发起登录的入口。
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
// 对齐 2026-10-24 的 @tencent-ai/codebuddy-code@2.174.0（npm latest）。上游目前不
// 校验 UA 版本，但登录页会按 version 做灰度与埋点，写旧版本可能拿到旧登录页。
const ClientVersion = "2.176.0"

// ClientUA 兜底 UA（未抓包的站点用）。
//
// 2026-10-30 双版本真机抓包：登录三端点发的是**桌面端 RPC 族**，不是 CodeBuddy 家族串：
//
//	CN 5.9.3     → WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0
//	GL 5.9.4     → workbuddy-ai/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0
//
// 本仓别处注释早就写过「CodeBuddy 家族串在 WorkBuddy 分发包里根本不存在，是最显眼的
// 非官方指纹」，结果登录链路一直在发它。
const ClientUA = ClientVersionWorkBuddyCN + " " + ClientVersionWorkBuddyCN + " CLI/" + ClientVersionBuiltinCLI

// 出站客户端版本段（与 upstream 的 chat/RPC 族同源，三处抓包一致）。
const (
	ClientVersionWorkBuddyCN = "WorkBuddy/5.9.3"
	ClientVersionWorkBuddyGL = "workbuddy-ai/5.9.4"
	ClientVersionBuiltinCLI  = "2.165.0"
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
	Origin string // Origin/Referer
	// Platform 授权页 `?platform=` 口径，取自 product.json →
	// authentication.attributes.platform（**不是**顶层 platform 字段，那是 "CLI"，
	// 两个同名字段取错层是旧实现的 bug，两版都不符）：
	//
	//	CN 5.9.3 → workbuddy     GL 5.9.4 → workbuddy-ai
	//
	// 官方只对 platform=workbuddy 追加 ww_device_nonce（桌面端设备绑定）。
	Platform string
	// UserAgent 该站点登录三端点出站的桌面端 RPC 族 UA（空 = 回落 ClientUA）。
	UserAgent string
	// Domain 设备码端点的 X-Domain 取值（**不是 API base 主机名**）：真机 CN
	// auth/state 打 copilot.tencent.com，但 `x-domain: www.workbuddy.cn` ——
	// 该头声明「哪个前端在调用」。空则回落 Origin 主机名。
	Domain string
}

var (
	// 国内站：API 与授权页不同域（Origin 是 www.codebuddy.cn）。
	siteCN = Site{
		ID: "cn", Realm: RealmCN,
		API: "https://copilot.tencent.com", Origin: "https://www.codebuddy.cn",
		Platform:  "workbuddy",
		UserAgent: ClientVersionWorkBuddyCN + " " + ClientVersionWorkBuddyCN + " CLI/" + ClientVersionBuiltinCLI,
	}
	// 旧桌面端国际版：base 与 Origin 同域。
	siteWorkBuddy = Site{
		ID: "workbuddy", Realm: RealmGlobal,
		API: "https://www.workbuddy.ai", Origin: "https://www.workbuddy.ai",
		Platform:  "workbuddy-ai",
		UserAgent: ClientVersionWorkBuddyGL + " " + ClientVersionWorkBuddyGL + " CLI/" + ClientVersionBuiltinCLI,
	}
)

// ForRealm 按 realm 映射站点：global → workbuddy 旧桌面端（历史行为，保证只传
// realm 的老调用方语义不变），cn/空/非法 → 国内站（零回归）。
func ForRealm(realm string) Site {
	if strings.TrimSpace(realm) == RealmGlobal {
		return siteWorkBuddy
	}
	return siteCN
}

// Endpoints 返回设备授权三端点（auth/state、token、account）与 Origin。
func (s Site) Endpoints() (state, token, account, origin string) {
	return s.API + "/v2/plugin/auth/state?platform=" + s.Platform,
		s.API + "/v2/plugin/auth/token?state=",
		s.API + "/v2/plugin/login/account?state=",
		s.Origin
}

// 官方登录业务码（CLI auth 模块枚举）。语义分两类，混为一谈会让用户在本该立刻
// 报错的场景白等满 TTL（面板 15 分钟 / CLI 一直提示「请确认已登录」）：
//
//	11217 RetryFetchToken / 12151 RetryFetchAccount → 还没登完，继续轮询
//	12005 LicenseSeatLimit / 11212 LicenseExpired / 11216 TrialExpired → 终态
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
//
// 注意：**不在码表里的码不算终态** —— 未知语义按 pending 处理，宁可多转一会儿也
// 不要把「上游新增的中间态码」误报成致命错误（那会让用户以为账号不能用）。
func CodeFatal(code int) bool {
	switch code {
	case CodeLicenseSeatLimit, CodeLicenseExpired, CodeTrialExpired:
		return true
	}
	return false
}

// DecorateAuthURL 按官方 CLI 行为补齐授权链接参数：
//
//	version=<ClientVersion>   官方对每个站点都加
//	ww_device_nonce=<nonce>   官方仅 platform=workbuddy 且拿到设备 nonce 时加
//
// 本仓没有桌面端 Turing 设备 SDK，nonce 只能由部署侧提供（环境变量
// CODEBUDDY_LOGIN_DEVICE_NONCE）；为空则不加 —— 不伪造设备指纹，错值比缺值更容易
// 被上游风控判异常。raw 解析失败时原样返回（宁可少一个参数，不要弄坏链接）。
func DecorateAuthURL(raw string, s Site, nonce string) string {
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	if q.Get("version") == "" {
		q.Set("version", ClientVersion)
	}
	if s.Platform == "workbuddy" && nonce != "" && q.Get("ww_device_nonce") == "" {
		q.Set("ww_device_nonce", nonce)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// EndpointHeaders 设备码三端点的**公共**头。
//
// 2026-10-30 双版本真机抓包：三端点都是 node fetch，**不发** Origin / Referer /
// X-Requested-With（浏览器同源语义只属于「打开授权页」那一步）。
// 三端点的差异（Content-Type / X-Domain / x-no-* 哨兵 / x-device-token）见
// HeadersForState / HeadersForToken / HeadersForAccount，不要在这里抹平。
func (s Site) EndpointHeaders() map[string]string {
	return map[string]string{
		"Accept":       "application/json, text/plain, */*",
		"User-Agent":   s.userAgent(),
		"X-Product":    "SaaS",
		"X-Request-ID": newTraceID(),
	}
}

// userAgent 站点 UA：真机口径优先，未复核站点回落 ClientUA。
func (s Site) userAgent() string {
	if s.UserAgent != "" {
		return s.UserAgent
	}
	return ClientUA
}

// host 设备码端点的 X-Domain 取值：站点显式 Domain 优先，否则回落 Origin 主机名。
func (s Site) host() string {
	if s.Domain != "" {
		return s.Domain
	}
	h := strings.TrimPrefix(strings.TrimPrefix(s.Origin, "https://"), "http://")
	return strings.TrimSuffix(h, "/")
}

// sentinelNo 未鉴权/未落库端点的 x-no-* 哨兵头。
//
// 真机值是字符串 "true"（不是 "1"）。auth/state 与 auth/token 四个全发；
// login/account 只发后三个 —— 它已带 Bearer，唯一缺的是「账号信息还没落库」。
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

// HeadersForState POST /v2/plugin/auth/state 的出站头：
// 公共头 + X-Domain + 四个 x-no-* + X-Custom-Channel-Id: unknown
// + X-Device-Token（取到才发；空则不伪造）。
func (s Site) HeadersForState(deviceToken string) map[string]string {
	h := s.EndpointHeaders()
	h["Content-Type"] = "application/json" // 只有 POST 带，两条 GET 都没有
	h["X-Domain"] = s.host()
	h["X-Custom-Channel-Id"] = "unknown" // 真机恒为字面量（渠道号未知的分发包）
	for k, v := range sentinelNo(true) {
		h[k] = v
	}
	if deviceToken != "" {
		h["X-Device-Token"] = deviceToken
	}
	return h
}

// HeadersForToken GET /v2/plugin/auth/token 的出站头：
// 公共头 + 四个 x-no-*。**唯一没有 X-Domain 的端点**，也没有 x-custom-channel-id
// 与 x-device-token。
func (s Site) HeadersForToken() map[string]string {
	h := s.EndpointHeaders()
	for k, v := range sentinelNo(true) {
		h[k] = v
	}
	return h
}

// HeadersForAccount GET /v2/plugin/login/account 的出站头：
// 公共头 + X-Domain + Authorization + 三个 x-no-*（无 x-no-authorization）。
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

// newTraceID 生成 32 hex 纯随机请求 ID（真机 X-Request-ID 形态：无毫秒前缀）。
// 熵源故障时回落时间派生，仍恒 32 hex，不 panic、不空串。
func newTraceID() string {
	var b [16]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		ms := time.Now().UnixMilli()
		return fmt.Sprintf("%012x%020x", ms, uint64(ms))
	}
	return hex.EncodeToString(b[:])
}
