// login.go 面板内嵌的 WorkBuddy CN OAuth 设备授权流程（cmd/login 的进程内移植）。
//
//	POST /panel/api/login/start → 拿 state+authUrl，state 存进程内（不再落 /tmp，
//	  原方案在 Windows 上不可用），返回授权 URL；
//	GET  /panel/api/login/poll   → 面板前端每 3s 轮询本接口；未完成返回 done=false，
//	  完成后取 uid/nickname、凭证落盘 auths/workbuddy-<uid>.json、热加载进池
//	  （pool.Add + Revive），并顺带签到 + 余额刷新 —— 免重启加载新账号。
//
// 无 PKCE（workbuddy 设备流由服务端签发 state），请求头与上游端点与 cmd/login 保持一致。
package panel

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/loginsite"
	"github.com/linguo2625469/workbuddy2api-panel/internal/proxypool"
)

const (
	// clientUA 保留仅为兼容既有测试引用；设备码端点实际 UA 按站点取
	// （loginsite.Site.UserAgent → EndpointHeaders），登录链路已不再发 CodeBuddy CLI 串。
	clientUA = loginsite.ClientUA
)

// loginSite 别名：站点定义来自 internal/loginsite（官方域表的唯一来源）。
type loginSite = loginsite.Site

// 站点标识别名（面板内部沿用短名）。
const (
	siteCN        = loginsite.SiteCN
	siteIntl      = loginsite.SiteIntl
	siteWorkBuddy = loginsite.SiteWorkBuddy
)

// siteResolver 站点表查询入口（测试可替换为指向 httptest 服务器的站点，
// 否则 loginStart/loginPoll 的单测必须打真实上游）。
var siteResolver = loginsite.For

// siteOrDefault 归一化站点标识：空/非法 → cn（老调用零回归）。
func siteOrDefault(id string) loginSite { return siteResolver(id) }

// siteForRealm 按 realm 映射站点：global → 旧桌面端 workbuddy.ai（历史行为，
// 保持 login.sh / 老前端 realm=global 的语义不变），cn → 国内站。
// 新代码应直接用 siteOrDefault（官方国际站要显式 site=intl）。
// 走 siteResolver 而非直接查表，使单测能把两站点都指到 httptest 服务器。
func siteForRealm(realm string) loginSite { return siteResolver(loginsite.ForRealm(realm).ID) }

// loginEndpointsFor 按站点返回设备授权三端点（auth/state、token、account）+ Origin。
func loginEndpointsFor(s loginSite) (state, token, account, origin string) { return s.Endpoints() }

// loginEndpoints 兼容旧签名：按 realm 选站点（global → workbuddy 旧桌面端）。
func loginEndpoints(realm string) (state, token, account, origin string) {
	return loginEndpointsFor(siteForRealm(realm))
}

// 官方登录码语义（码表与判定在 internal/loginsite，两处调用方共用）。
const (
	loginCodeRetryFetchToken   = loginsite.CodeRetryFetchToken
	loginCodeRetryFetchAccount = loginsite.CodeRetryFetchAccount
	loginCodeLicenseSeatLimit  = loginsite.CodeLicenseSeatLimit
	loginCodeLicenseExpired    = loginsite.CodeLicenseExpired
	loginCodeTrialExpired      = loginsite.CodeTrialExpired
)

// loginCodePending 报告该业务码是否表示「登录进行中，应继续轮询」。
func loginCodePending(code int) bool { return loginsite.CodePending(code) }

// loginCodeFatal 报告该业务码是否属于官方定义的终态许可错误（SignLicenseError）。
func loginCodeFatal(code int) bool { return loginsite.CodeFatal(code) }

// loginEnvelopeError 上游业务码非 0 的结构化错误：调用方按官方码表区分「继续
// 轮询」与「终态失败」，不必解析错误字符串。
type loginEnvelopeError struct {
	Code   int
	Msg    string
	Status int
}

func (e *loginEnvelopeError) Error() string {
	return fmt.Sprintf("code=%d msg=%s", e.Code, e.Msg)
}

// loginHTTP 设备授权专用 client：无 cookie（每请求携带 state，无会话态）。
//
// ⚠️ 为什么不能直接用默认 transport（2026-10-24 实案，处置与
// internal/upstream/transport.go 同源）：
// 默认 transport 会经 ALPN 协商出 HTTP/2，而本网络下容器 DNS（127.0.0.11）偶发
// 「server misbehaving」并留下半死连接——半死 h2 流被复用时会一直等响应头，直到
// 客户端**总超时**才报错：
//
//	auth state (upstream 0): Post "https://copilot.tencent.com/v2/plugin/auth/state?platform=CLI":
//	context deadline exceeded (Client.Timeout exceeded while awaiting headers)
//
// 用户可见表现：国内版一直拿不到授权链接（国际版当时恰好没撞上抖动，看起来正常）；
// 登录轮询走的 auth/token 是同一个 client，同样会卡——浏览器侧明明已登录成功，
// 网关却迟迟检测不到，面板像卡死。
//
// 两层防护（与 newTransport 一致）：
//   - TLS 层：空 TLSNextProto 真正禁 h2（必须 make 而非 nil；只设
//     ForceAttemptHTTP2=false 无效，ALPN 仍会协商出 h2，见 transport.go 注释）；
//   - 连接层：DialContext 8s 建连 + 15s keepalive、TLS 握手 8s、响应头 15s，
//     让单次尝试在 ~15s 内失败而不是拖满 30s 总超时，配合下方 doJSON 重试。
var loginHTTP = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   8 * time.Second,
			KeepAlive: 15 * time.Second,
		}).DialContext,
		TLSNextProto:          make(map[string]func(string, *tls.Conn) http.RoundTripper),
		TLSHandshakeTimeout:   8 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       60 * time.Second,
	},
}

// commonHeaders 设备码端点的公共头族（真机形态，见 loginsite.Site.EndpointHeaders）。
//
// 不再发 Origin / Referer / X-Requested-With：2026-10-30 双版本真机抓包显示这三条
// 端点（auth/state、auth/token、login/account）由桌面端 node fetch 发出，恒无这三个头
// ——浏览器同源语义只出现在「打开授权页」那一步，不在 API 调用上。
//
// 三端点的**差异头**（x-domain / x-no-* / x-device-token / Authorization）由
// stateHeaders / tokenHeaders / accountHeaders 三个包装补，不要在这里抹平。
func commonHeaders(req *http.Request, site loginSite) {
	applyHeaders(req, site.EndpointHeaders())
}

// applyHeaders 把 map 头批量写到 req（空值跳过，避免发出空头）。
func applyHeaders(req *http.Request, h map[string]string) {
	for k, v := range h {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
}

// stateHeaders POST /v2/plugin/auth/state 的头族（含 x-device-token 与哨兵）。
func stateHeaders(site loginSite, deviceToken string) map[string]string {
	return site.HeadersForState(deviceToken)
}

// tokenHeaders GET /v2/plugin/auth/token 的头族（真机唯一不带 x-domain 的端点）。
func tokenHeaders(site loginSite) map[string]string {
	return site.HeadersForToken()
}

// accountHeaders GET /v2/plugin/login/account 的头族（带 Bearer，x-no-authorization 缺席）。
func accountHeaders(site loginSite, accessToken string) map[string]string {
	return site.HeadersForAccount(accessToken)
}

// validUID 校验上游返回的 uid 是否可安全用于拼文件名。
// 只放行字母、数字、下划线、连字符（腾讯侧 uid 实测为 UUID 形态），
// 长度上限 64 兜底异常超长串；拒绝 . / \ 等路径字符与空串。
func validUID(uid string) bool {
	if uid == "" || len(uid) > 64 {
		return false
	}
	for _, c := range uid {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// apiEnvelope 与 upstream 同形：{code,msg,data}，code!=0 视为业务错误。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// loginAttempts 单次登录步骤的传输层重试次数（只对「一个 HTTP 响应都没拿到」的
// 失败重试）。理由见 loginHTTP：容器 DNS 偶发 server misbehaving，单发失败不该让
// 用户从头再点一遍；且三步都可安全重发（auth/state 重签一个 state、旧 state 由
// loginTTL 回收；token/account 是只读查询）。已拿到响应（业务错误如 token 的
// 11217 "login ing"、或 HTTP 4xx/5xx）绝不重试——那是确定结果，重试只会拖慢轮询。
const loginAttempts = 3

// doJSON 发一次 JSON 请求并解信封，遇纯传输失败按 loginAttempts 重试。
// site 决定头族与 UA（真机形态，见 commonHeaders）；extra 为端点差异头
// （state 的 x-device-token / 哨兵、account 的 Bearer 等），在公共头之后覆盖写入，
// nil 表示只发公共头。body 用 []byte 以便重放。route 决定出站出口（见 loginRoute）。
func doJSON(route loginRoute, method, fullURL string, extra map[string]string, body []byte, site loginSite) (json.RawMessage, int, error) {
	var lastErr error
	for attempt := 0; attempt < loginAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 400 * time.Millisecond)
		}
		data, status, err := doJSONOnce(route, method, fullURL, extra, body, site)
		if err == nil {
			return data, status, nil
		}
		if status != 0 {
			return nil, status, err // 上游已应答：确定结果，不重试
		}
		lastErr = err
	}
	return nil, 0, lastErr
}

// doJSONOnce 单次尝试（不做重试）。
func doJSONOnce(route loginRoute, method, fullURL string, extra map[string]string, body []byte, site loginSite) (json.RawMessage, int, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, fullURL, rdr)
	if err != nil {
		return nil, 0, err
	}
	commonHeaders(req, site)
	applyHeaders(req, extra)
	resp, err := route.do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		// 结构化错误：调用方按官方码表区分 pending / 终态（见 loginCodePending/Fatal）。
		return nil, resp.StatusCode, &loginEnvelopeError{Code: env.Code, Msg: env.Msg, Status: resp.StatusCode}
	}
	return env.Data, resp.StatusCode, nil
}

// loginDeviceNonce 官方 ww_device_nonce 的来源（桌面端 Turing 设备指纹 SDK）。
// 本仓无 SDK，取部署侧环境变量；空 = 不加该参数（不伪造设备指纹）。
func loginDeviceNonce() string {
	return strings.TrimSpace(os.Getenv("CODEBUDDY_LOGIN_DEVICE_NONCE"))
}

// loginDeviceToken 设备风控 token（X-Device-Token），用于 auth/state 的出站头。
//
// 真机（CN 5.9.3 / GL 5.9.4）该头恒为 `v3:<base64…>` 形态，由客户端 qimei 组件
// 本地生成后加密上报，**不是**能从 machineId 推导出来的东西。网关拿不到就返回空串，
// 调用方据此不发该头——伪造一个假 token 比缺这个头更容易被判定为非客户端。
//
// 取值来源（按优先级）：
//
//	CODEBUDDY_LOGIN_DEVICE_TOKEN 环境变量
//	CODEBUDDY_LOGIN_DEVICE_TOKEN_FILE 指向的文件（宿主从桌面端落盘处导出）
func loginDeviceToken() string {
	if v := strings.TrimSpace(os.Getenv("CODEBUDDY_LOGIN_DEVICE_TOKEN")); v != "" {
		return v
	}
	if p := strings.TrimSpace(os.Getenv("CODEBUDDY_LOGIN_DEVICE_TOKEN_FILE")); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

// loginRoute 一次登录（加号）的出站出口。零值 = 直连（loginHTTP 原样出网）；
// proxyID 非空 = 这次登录的每个端点都钉在该节点上，落盘后写成该号的固定绑定。
type loginRoute struct {
	pool    *proxypool.Manager
	proxyID string
}

// do 发一次登录请求：没钉节点就走 loginHTTP 的直连传输，否则经指定节点。
//
// 经代理时用的是该节点的传输层（与其它出站同一套 TLS 指纹、超时与连接池），
// 不再沿用 loginHTTP 的 Go 原生握手：代理隧道上必须由我们自己的指纹拨号，
// 否则 net/http 会改用它的原生 CONNECT 握手（见 proxypool.transportLocked）。
// 直连分支原样保留 loginHTTP——那套「禁 h2 + 分档超时」是 2026-10-24 的实案结论
// （默认 transport 的半死 h2 流会让登录卡到总超时），一个字节都不能丢。
func (r loginRoute) do(req *http.Request) (*http.Response, error) {
	if r.pool == nil || r.proxyID == "" {
		return loginHTTP.Do(req)
	}
	return r.pool.DoPinned(loginHTTP, req, r.proxyID)
}

// pickLoginRoute 给这次加号选出口：号池里有启用节点就随机钉一条。
//
// 返回零值（直连）只有两种情况：池里一条启用节点都没有，或运维用
// features.login_bind_proxy 显式关掉了这条能力。其余情况一律经代理——
// 登录是整条链路里最不该露机房 IP 的一步，也是唯一「还没有 uid 可查绑定」
// 的一步（selectLocked 的三档查表在这里全落空，所以必须另行选）。
func (p *Panel) pickLoginRoute() loginRoute {
	if p.cfg.ProxyPool == nil || !p.loginBindEnabled() {
		return loginRoute{}
	}
	id := p.cfg.ProxyPool.PickForLogin()
	if id == "" {
		log.Printf("panel: 号池没有启用中的出口，本次登录走直连（先用面板加节点可避免）")
		return loginRoute{}
	}
	return loginRoute{pool: p.cfg.ProxyPool, proxyID: id}
}

// loginBindEnabled 加号是否绑定号池出口（features.login_bind_proxy，缺省 true）。
//
// 快照里存的是取反的 LoginBindOff，这样零值 = 开启：没接 livecfg 的裸用路径与
// 单测不必初始化就保持「绑定」这一默认行为。
func (p *Panel) loginBindEnabled() bool {
	if p.cfg.Live == nil {
		return true
	}
	return !p.cfg.Live.Load().LoginBindOff
}

// loginRouteLabel 出口的可读描述（日志与响应回显用，不含凭据）。
func (p *Panel) loginRouteLabel(r loginRoute) string {
	if r.pool == nil || r.proxyID == "" {
		return "直连"
	}
	if name := r.pool.NodeLabel(r.proxyID); name != "" {
		return "代理 " + name
	}
	return "代理"
}

// bindLoginProxy 把新号钉在本次登录用的节点上（固定绑定，压过 realm 默认与随机池）。
//
// 失败只记日志、不判登录失败：凭据已经落盘，号是可用的；绑定失败最多让它按
// realm 默认出网，用户还能在面板里手动补一次。
func bindLoginProxy(m *proxypool.Manager, uid, proxyID string) error {
	if m == nil || uid == "" || proxyID == "" {
		return nil
	}
	return m.Bind(uid, proxypool.Binding{Mode: "fixed", ProxyID: proxyID})
}

// loginStart 发起设备授权：POST auth/state 拿授权 URL。
// body 可带 {"site":"cn"|"intl"|"workbuddy"}（官方域表，缺省 cn）；
// 兼容老前端 {"realm":"global"} → 旧桌面端 workbuddy.ai（零回归）。
// state 会话记站点，poll 用同一站点取 token/落盘。
func (p *Panel) loginStart(w http.ResponseWriter, r *http.Request) {
	// 会话表硬上限（W2-LG-01）：每条会话都要打一次上游 auth/state，只在下次
	// start / poll 命中时才回收 —— 不设上限就是无界放大。**必须先于上游调用**拒，
	// 否则「拒绝」本身也成了一次上游请求。面板是单管理员使用，64 条已远超真实用量。
	p.loginMu.Lock()
	full := len(p.logins) >= loginMaxSessions
	p.loginMu.Unlock()
	if full {
		writeErr(w, http.StatusTooManyRequests,
			"too many concurrent login sessions; finish the pending ones or wait for them to expire")
		return
	}
	site := siteOrDefault("")
	if r.Body != nil {
		var reqBody struct {
			Realm string `json:"realm"`
			Site  string `json:"site"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&reqBody); err == nil {
			if reqBody.Site != "" {
				// site 显式优先（官方国际站走 site=intl）。
				site = siteOrDefault(strings.ToLower(strings.TrimSpace(reqBody.Site)))
			} else {
				site = siteForRealm(reqBody.Realm)
			}
		}
	}
	epState, _, _, _ := loginEndpointsFor(site)
	// 出口先定下来：auth/state 就是加号的第一个出站，直连与经代理在这里分岔，
	// 后面 token/account 必须与它同一条（见 loginRoute）。
	route := p.pickLoginRoute()
	// auth/state：真机带 x-device-token（取到才发）+ x-custom-channel-id + 四个 x-no-*。
	data, status, err := doJSON(route, http.MethodPost, epState, stateHeaders(site, loginDeviceToken()), []byte("{}"), site)
	if err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("auth state (upstream %d): %v", status, err))
		return
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		writeErr(w, http.StatusBadGateway, "auth state: missing state or authUrl")
		return
	}
	// 对齐官方：授权链接补 version（workbuddy 域再加设备 nonce）。
	st.AuthURL = loginsite.DecorateAuthURL(st.AuthURL, site, loginDeviceNonce(), loginsite.NewLoginSessionID())
	p.loginMu.Lock()
	// 顺手回收过期会话，防"开弹窗走开"的 state 滞留。
	for s, sess := range p.logins {
		if time.Since(sess.created) > loginTTL {
			delete(p.logins, s)
		}
	}
	p.logins[st.State] = loginSession{created: time.Now(), realm: site.Realm, site: site.ID, proxyID: route.proxyID}
	p.loginMu.Unlock()
	log.Printf("panel: 发起 OAuth 添加账号 realm=%s site=%s 出口=%s（state=%s...）",
		site.Realm, site.ID, p.loginRouteLabel(route), st.State[:min(8, len(st.State))])
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "url": st.AuthURL, "state": st.State,
		"realm": site.Realm, "site": site.ID, "proxy": p.loginRouteLabel(route)})
}

// logPollOnce 上游轮询结果变了才落一行日志（state 前缀 + 上游原文）。
//
// 为什么需要：浏览器侧报「登录失败」时，面板此前只有「发起 OAuth」一行记录——
// code/msg 全丢在轮询里，运维无法判断是「浏览器根本没授权成功」还是「授权了但
// 取 token 被拒」。逐轮记不行：前端 3s 一轮、最长 loginTTL，一条 state 能刷出
// 300 行，真正的一次性错误反而被淹掉。state 已不在表里（超时 / 已落盘）则不记。
func (p *Panel) logPollOnce(state, msg string) {
	p.loginMu.Lock()
	s, ok := p.logins[state]
	if !ok || s.lastMsg == msg {
		p.loginMu.Unlock()
		return
	}
	s.lastMsg = msg
	p.logins[state] = s
	p.loginMu.Unlock()
	log.Printf("panel: 登录轮询 state=%s... 上游=%s", state[:min(8, len(state))], msg)
}

// loginPoll 轮询登录态。未完成 → {done:false}；完成 → 建凭证、落盘、热加载、签到。
func (p *Panel) loginPoll(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state == "" {
		writeErr(w, http.StatusBadRequest, "missing state")
		return
	}
	p.loginMu.Lock()
	sess, known := p.logins[state]
	if known && sess.processing {
		p.loginMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "正在写入凭据…"})
		return
	}
	if known {
		sess.processing = true
		p.logins[state] = sess
	}
	p.loginMu.Unlock()
	if !known {
		// 410：state 已作废（进程重启或超过 loginTTL）。前端据此停止轮询；网络抖动
		// （5xx/超时）则继续重试——混成一个状态码时，一次抖动就让用户放弃等待，
		// 而 OAuth 其实已在浏览器侧完成、账号却不会入池。
		writeErr(w, http.StatusGone, "login session 已过期，请关闭后重新添加账号")
		return
	}
	// 失败路径复位闸门，供用户重试（成功路径 delete(state) 已经清掉）。
	defer func() {
		p.loginMu.Lock()
		if s, ok := p.logins[state]; ok {
			s.processing = false
			p.logins[state] = s
		}
		p.loginMu.Unlock()
	}()
	pollSite := siteOrDefault(sess.site)
	_, epToken, epAcct, _ := loginEndpointsFor(pollSite)
	// 与 loginStart 同一条出口：auth/state 在 A 出口签发、token/account 从 B 出口取，
	// 在上游看来是两次无关的访问（同一 state 换了来源 IP）。
	route := loginRoute{pool: p.cfg.ProxyPool, proxyID: sess.proxyID}

	// auth/token 是权威登录状态端点。官方码表语义：
	//   11217（RetryFetchToken）→ 还没登完，继续轮询；
	//   12005/11212/11216（席位满/许可证过期/试用到期）→ 终态，立刻把上游 msg
	//   抛给前端并停轮询（官方 SignLicenseError 行为），不再空转到 TTL。
	tokRaw, _, err := doJSON(route, http.MethodGet, epToken+state, tokenHeaders(pollSite), nil, pollSite)
	if err != nil {
		var envErr *loginEnvelopeError
		if errors.As(err, &envErr) && loginCodeFatal(envErr.Code) {
			p.loginMu.Lock()
			delete(p.logins, state)
			p.loginMu.Unlock()
			log.Printf("panel: 登录被上游拒绝 state=%s... code=%d msg=%s", state[:min(8, len(state))], envErr.Code, envErr.Msg)
			writeJSON(w, http.StatusOK, map[string]any{
				"done":    false,
				"fatal":   true,
				"code":    envErr.Code,
				"message": fmt.Sprintf("登录被上游拒绝（code=%d）：%s", envErr.Code, envErr.Msg),
			})
			return
		}
		// pending（11217/12151）或其它可重试错误：面板前端继续轮询。
		p.logPollOnce(state, err.Error())
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": err.Error()})
		return
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "waiting for login"})
		return
	}

	// 完成：取 uid/nickname。官方 12151（RetryFetchAccount）是「token 已发但账号信息
	// 还在落库」，属于 pending，必须继续轮询而不是报错——否则刚登录成功的号会被
	// 误判成失败（用户重登又撞同一个窗口）。
	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	acctRaw, _, errAcct := doJSON(route, http.MethodGet, epAcct+state, accountHeaders(pollSite, tok.AccessToken), nil, pollSite)
	if errAcct == nil {
		_ = json.Unmarshal(acctRaw, &acct)
	} else {
		var envErr *loginEnvelopeError
		if errors.As(errAcct, &envErr) {
			if loginCodePending(envErr.Code) {
				p.logPollOnce(state, errAcct.Error())
				writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": errAcct.Error()})
				return
			}
			if loginCodeFatal(envErr.Code) {
				p.loginMu.Lock()
				delete(p.logins, state)
				p.loginMu.Unlock()
				log.Printf("panel: 登录账号信息被上游拒绝 state=%s... code=%d msg=%s", state[:min(8, len(state))], envErr.Code, envErr.Msg)
				writeJSON(w, http.StatusOK, map[string]any{
					"done":    false,
					"fatal":   true,
					"code":    envErr.Code,
					"message": fmt.Sprintf("登录被上游拒绝（code=%d）：%s", envErr.Code, envErr.Msg),
				})
				return
			}
		}
	}
	if acct.UID == "" {
		writeErr(w, http.StatusBadGateway, "login done but no uid（token 已发但账号信息获取失败，请重试）")
		return
	}
	// UID 来自上游响应，未经校验就用于拼文件名会被路径穿越利用
	// （filepath.Join("./auths", "workbuddy-../../evil.json") → auths/evil.json）。
	// UID 是腾讯侧账号标识，实测为 UUID（十六进制与连字符），故只放行 [A-Za-z0-9_-]。
	if !validUID(acct.UID) {
		writeErr(w, http.StatusBadGateway, "上游返回的 uid 含非法字符，拒绝落盘（防路径穿越）")
		return
	}

	// 凭证落盘（嵌套形，与 auths/ 目录既有格式一致）→ 热加载进池。
	if err := os.MkdirAll(p.cfg.AuthDir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "mkdir auth dir: "+err.Error())
		return
	}
	a := &auth.Auth{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix(),
		Domain:       tok.Domain,
		UID:          acct.UID,
		EnterpriseID: acct.EnterpriseID,
		Nickname:     acct.Nickname,
		FilePath:     filepath.Join(p.cfg.AuthDir, fmt.Sprintf("workbuddy-%s.json", acct.UID)),
	}
	// global 登录：落盘 auth.realm=global（Realm() 按此判域；不写则依赖 domain 后缀回落）。
	if sess.realm == "global" {
		if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
			writeErr(w, http.StatusInternalServerError, "set realm: "+err.Error())
			return
		}
	} else {
		// CN 也显式补 realm 键（幂等），让 auth 文件形态统一（与 LoadDir 存量迁移对齐）。
		_, _ = a.BackfillRealm()
	}
	// 落盘 + 入池与目录热重载串行化（issue #199 风险 3）：登录刚写出半份文件时
	// 若正好被重载扫到，该账号会被判成「解析失败」进而在本轮拒绝删除——不会丢，
	// 但这一轮的 upsert 顺序可能反过来把旧凭证盖回来。持同一把锁就没有这个窗口。
	isNewAccount, saveErr := p.saveLoginAuth(a)
	if saveErr != nil {
		writeErr(w, http.StatusInternalServerError, "save auth: "+saveErr.Error())
		return
	}
	p.cfg.Pool.Revive(acct.UID) // 全新登录 = 人工恢复口径：清掉旧号遗留的禁用/冷却/熔断

	// 出口随号落定：这次登录用的节点立刻写成该号的固定绑定，下面的注册激活/签到/
	// 余额刷新以及之后的一切任务都跟着它走。顺序不能挪到这些调用之后——它们本身就
	// 是出站请求，晚一步就等于「登录藏了 IP、紧接着用机房 IP 做第一件事」。
	bound := ""
	if route.proxyID != "" {
		if err := bindLoginProxy(p.cfg.ProxyPool, acct.UID, route.proxyID); err != nil {
			log.Printf("panel: 登录出口绑定失败 uid=%s proxy=%s: %v", acct.UID, route.proxyID, err)
		} else {
			bound = p.loginRouteLabel(route)
			log.Printf("panel: 登录出口已绑定 uid=%s → %s（此后任务/聊天共用该出口）", acct.UID, bound)
		}
	}

	// 顺带签到 + 余额刷新（幂等；失败不影响登录结果，只体现在返回字段里）。
	// realm 分支：CN 走 DailyCheckin；global 无 CN 签到体系，改为注册激活 + trial 领取
	// （D4 门控同 scheduler：CN 任务端点对 global 不发起任何调用）。
	checkinMsg := ""
	remain := int64(-1)
	total := int64(0)
	if sess.realm == "global" {
		// 注册激活（幂等）：region required 时自动补地区（白名单首个，HK）后重新激活。
		// 失败不阻断登录结果（auth 已落盘），只在返回字段里体现。
		if activated, err := p.cfg.Upstream.GlobalCompleteRegistration(a); err != nil {
			checkinMsg = "注册激活失败: " + err.Error()
			log.Printf("panel: global 注册激活 uid=%s: %v", acct.UID, err)
		} else if activated {
			log.Printf("panel: global 注册激活 uid=%s 完成", acct.UID)
		}
		// trial 加油包（幂等 14051 = 已领过，非错误）。
		if claimed, err := p.cfg.Upstream.ClaimTrial(a); err != nil {
			checkinMsg = joinMsg(checkinMsg, "trial 领取失败: "+err.Error())
			log.Printf("panel: global trial uid=%s: %v", acct.UID, err)
		} else if claimed {
			log.Printf("panel: global trial uid=%s 已领", acct.UID)
		}
	} else if a.SupportsPersonalGrowth() {
		// 无个人成长体系的账号跳过签到：企业号上游 400 code 10001「企业账号不支持该
		// 操作」，静态 API Key（ck_/pt_）根本没有个人账号身份——发了只会把错误写进登录
		// 返回。判据与 import / 签到端点 / 成长任务同一把闸（SupportsPersonalGrowth），
		// 不再单看 IsEnterprise 而漏掉 API Key。下方 UserResource 余额查询照常。
		if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
			checkinMsg = err.Error()
		}
	}
	balanceErr := ""
	if rm, tt, err := p.cfg.Upstream.UserResource(a); err == nil {
		remain, total = rm, tt
		p.cfg.Pool.ReenableIfCredits(acct.UID, rm, tt)
	} else {
		// 凭据已落盘但上游仍拒绝 billing（典型：旧会话没清干净 / 渠道降级 token）。
		// 不判登录失败（chat 可能可用），但必须如实回传，否则运维只会看到「已添加」
		// 却继续 401，误以为重新登录没用。
		balanceErr = err.Error()
		log.Printf("panel: 登录后余额校验失败 uid=%s: %v", acct.UID, err)
	}

	p.loginMu.Lock()
	delete(p.logins, state)
	p.loginMu.Unlock()
	if isNewAccount {
		// 本仓没有上游的「新账号后台任务」执行体（StartNewAccountTasks 属 taskjobs），
		// 但导入路径上的后置激活（注册激活 / 领 trial / 首次签到准备）是同一件事：
		// 走 postImportActivate 保持「加号」与「导入」两条入口行为一致。
		p.postImportActivate(a, sess.realm)
	}
	log.Printf("panel: 新账号已热加载 uid=%s nickname=%q realm=%s（免重启生效）", acct.UID, acct.Nickname, sess.realm)
	writeJSON(w, http.StatusOK, map[string]any{
		"done":            true,
		"uid":             acct.UID,
		"nickname":        acct.Nickname,
		"realm":           sess.realm,
		"credits":         remain,
		"credits_total":   total,
		"checkin_message": checkinMsg,
		"balance_error":   balanceErr,
		"proxy":           bound,
	})
}

// saveLoginAuth 登录结果落盘并入池，返回登录的是否池里原本没有的号。
// 落盘与入池在 reloadMu 内，与账号目录热重载串行。「是否新号」必须在 Add 之前判，
// 否则恒为 false，新号的后台任务永远不启动。
func (p *Panel) saveLoginAuth(a *auth.Auth) (isNew bool, err error) {
	p.reloadMu.Lock()
	defer p.reloadMu.Unlock()
	isNew = p.cfg.Pool.AuthByUID(a.UID) == nil
	if err := a.SaveAtomic(); err != nil {
		return false, err
	}
	p.cfg.Pool.Add(a)
	return isNew, nil
}

// joinMsg 拼接 login 完成后的提示消息（多段用「；」连接，空段跳过）。
func joinMsg(parts ...string) string {
	out := ""
	for _, s := range parts {
		if s == "" {
			continue
		}
		if out != "" {
			out += "；"
		}
		out += s
	}
	return out
}

// loginRegions 返回 global 注册可选地区（panel 前端选地区弹窗用；CN 不调用）。
// 未持账号时返回白名单静态兜底（前端只读展示，不依赖上游）。
func (p *Panel) loginRegions(w http.ResponseWriter, r *http.Request) {
	// 静态白名单（对齐国际版 web 展示集）：面板前端只读展示，无需账号态。
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"regions": []map[string]string{
			{"code": "HK", "name": "Hong Kong"},
			{"code": "MO", "name": "Macao"},
			{"code": "SG", "name": "Singapore"},
			{"code": "TH", "name": "Thailand"},
			{"code": "PH", "name": "Philippines"},
			{"code": "MY", "name": "Malaysia"},
			{"code": "ID", "name": "Indonesia"},
		},
	})
}
