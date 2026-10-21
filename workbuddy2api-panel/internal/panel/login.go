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
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const (
	upstreamBaseCN      = "https://copilot.tencent.com"
	upstreamBaseGlobal  = "https://www.workbuddy.ai"
	clientUA            = "CLI/2.63.2 CodeBuddy/2.63.2"
	originRefererCN     = "https://www.codebuddy.cn"
	originRefererGlobal = "https://www.workbuddy.ai"
)

// loginEndpoints 按 realm 返回设备授权三端点（auth/state、token、account）+ Origin。
// realm=="global" → 国际版（workbuddy.ai 同域）；cn/非法/缺省 → CN（零回归）。
func loginEndpoints(realm string) (state, token, account, origin string) {
	if realm == "global" {
		base := upstreamBaseGlobal
		return base + "/v2/plugin/auth/state?platform=CLI",
			base + "/v2/plugin/auth/token?state=",
			base + "/v2/plugin/login/account?state=",
			originRefererGlobal
	}
	base := upstreamBaseCN
	return base + "/v2/plugin/auth/state?platform=CLI",
		base + "/v2/plugin/auth/token?state=",
		base + "/v2/plugin/login/account?state=",
		originRefererCN
}

// loginHTTP 设备授权专用 client：短超时、无 cookie（每请求携带 state，无会话态）。
var loginHTTP = &http.Client{Timeout: 30 * time.Second}

func commonHeaders(req *http.Request, origin string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", clientUA)
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

// doJSON 发一次 JSON 请求并解信封。origin 为 Origin/Referer 基础域（随 realm 切）。
func doJSON(method, fullURL, bearer string, body io.Reader, origin string) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	commonHeaders(req, origin)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := loginHTTP.Do(req)
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
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, resp.StatusCode, nil
}

// loginStart 发起设备授权：POST auth/state 拿授权 URL。
// body 可带 {"realm":"global"}（缺省 cn）；state 会话记 realm，poll 同 realm 落盘。
func (p *Panel) loginStart(w http.ResponseWriter, r *http.Request) {
	realm := "cn"
	if r.Body != nil {
		var reqBody struct {
			Realm string `json:"realm"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&reqBody); err == nil {
			if reqBody.Realm == "global" {
				realm = "global"
			}
		}
	}
	epState, _, _, origin := loginEndpoints(realm)
	data, status, err := doJSON(http.MethodPost, epState, "", bytes.NewReader([]byte("{}")), origin)
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
	p.loginMu.Lock()
	// 顺手回收过期会话，防"开弹窗走开"的 state 滞留。
	for s, sess := range p.logins {
		if time.Since(sess.created) > loginTTL {
			delete(p.logins, s)
		}
	}
	p.logins[st.State] = &loginSession{created: time.Now(), realm: realm}
	p.loginMu.Unlock()
	log.Printf("panel: 发起 OAuth 添加账号 realm=%s（state=%s...）", realm, st.State[:min(8, len(st.State))])
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "url": st.AuthURL, "state": st.State, "realm": realm})
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
	if known && time.Since(sess.created) > loginTTL {
		// 超过 TTL 的 state 不该还能完成（BUG-19）：此前只有 loginStart 清理过期会话，
		// 轮询路径不校验 —— 15 分钟前的 state 仍能被 poll 完成。
		delete(p.logins, state)
		p.loginMu.Unlock()
		writeErr(w, http.StatusGone, "登录会话已过期，请重新发起添加账号")
		return
	}
	if known && sess.consumed {
		// 另一路 poll 正在处理同一个 state（前端 3s 轮询 + 完成路径 4 次上游调用
		// 各 30s 超时，慢一次就会叠第二发）。让后来的这一路先回去，别重复执行
		// Pool.Add / SaveAtomic / 签到 / 余额刷新。
		p.loginMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "上一次轮询结果仍在处理，请稍候"})
		return
	}
	if known {
		sess.consumed = true // 立即认领（锁内），离开临界区后不再有第二个 poll 进来
	}
	p.loginMu.Unlock()
	if !known {
		writeErr(w, http.StatusNotFound, "unknown or expired state（请重新发起添加账号）")
		return
	}
	// 未走完成路径就返回（pending / 上游抖动 / 写盘失败）时复位认领，否则这个
	// state 会被永久锁死到 TTL 过期。
	completed := false
	defer func() {
		if completed {
			return
		}
		p.loginMu.Lock()
		if s2, ok := p.logins[state]; ok {
			s2.consumed = false
		}
		p.loginMu.Unlock()
	}()
	_, epToken, epAcct, origin := loginEndpoints(sess.realm)

	// auth/token 是权威登录状态端点：pending 时业务 code 非 0（"login ing"）。
	tokRaw, _, err := doJSON(http.MethodGet, epToken+state, "", nil, origin)
	if err != nil {
		// pending / 未完成：面板前端继续轮询。
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

	// 完成：取 uid/nickname（失败不阻塞，仅缺展示名）。
	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	if acctRaw, _, err := doJSON(http.MethodGet, epAcct+state, tok.AccessToken, nil, origin); err == nil {
		_ = json.Unmarshal(acctRaw, &acct)
	}
	if acct.UID == "" {
		// 视作**终态**（bug #8）：走到这里说明 auth/token 已经成功 —— 上游已经把
		// 这个 state 消费掉了。若按"可重试"处理（completed 保持 false → 复位
		// consumed），下一次轮询会再打 token 端点，上游只会返回 pending/报错，
		// 前端 {done:false} 一直循环到 15 分钟 TTL，用户拿不到"重新发起登录"的提示。
		completed = true
		p.loginMu.Lock()
		delete(p.logins, state)
		p.loginMu.Unlock()
		writeErr(w, http.StatusBadGateway, "登录凭证已签发但账号信息获取失败（state 已消费），请重新发起添加账号")
		return
	}
	// UID 来自上游响应，未经校验就用于拼文件名会被路径穿越利用
	// （filepath.Join("./auths", "workbuddy-../../evil.json") → auths/evil.json）。
	// UID 是腾讯侧账号标识，实测为 UUID（十六进制与连字符），故只放行 [A-Za-z0-9_-]。
	if !validUID(acct.UID) {
		// 与上面 UID=="" 分支同构（bug #3）：auth/token 已成功 = 上游已消费该 state，
		// 若按可重试处理（复位 consumed），前端会 3 秒一次空转到 15 分钟 TTL。
		completed = true
		p.loginMu.Lock()
		delete(p.logins, state)
		p.loginMu.Unlock()
		writeErr(w, http.StatusBadGateway, "上游返回的 uid 含非法字符（state 已消费），请重新发起添加账号")
		return
	}

	// 凭证落盘（嵌套形，与 auths/ 目录既有格式一致）→ 热加载进池。
	if err := os.MkdirAll(p.cfg.AuthDir, 0o755); err != nil {
		// 与上面两个分支同构（bug #2）：auth/token 已成功 = 上游已消费该 state，
		// 这里若是「可重试」语义（completed 保持 false → defer 复位 consumed），
		// 前端 3 秒一次 poll 会继续空转到 15 分钟 TTL，且用户拿不到明确提示。
		completed = true
		p.loginMu.Lock()
		delete(p.logins, state)
		p.loginMu.Unlock()
		writeErr(w, http.StatusInternalServerError, "凭证已签发但创建 auths 目录失败，请重新发起添加账号："+err.Error())
		return
	}
	// 上游没下发 expiresIn（或为 0）时不要写「现在」（bug #10）：那会让
	// NeedsRefresh(10m) 恒真 → **每个** chat 请求都先打一次 refresh（多一次 RTT、
	// 放大 refresh 竞态窗口，失败还会换号）。upstream.RefreshToken 对同一情况有
	// 「缺 expiresIn 保留旧值」的防御，登录路径没对齐：这里留 0（未知），交给首次
	// 刷新补齐。
	var expiresAt int64
	// 先按秒比较再乘（bug #5）：原写法没有任何上限，脏值（ExpiresIn 巨大）乘法溢出成
	// 负数 → ExpiresAt 落在过去 → NeedsRefresh 恒真 → 每个请求先刷一次 token。
	// 10 年上限与 upstream.RefreshToken 同口径。
	// 与 upstream.RefreshTokenExpiresInMax 同源（BUG-21）：此前两处各写一份字面量，
	// 改一处忘另一处会让登录与刷新对「脏 expiresIn」的处理分叉。
	const maxExpiresInSec = int64(upstream.RefreshTokenExpiresInMax / time.Second)
	if tok.ExpiresIn > 0 && tok.ExpiresIn <= maxExpiresInSec {
		expiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	a := &auth.Auth{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    expiresAt,
		Domain:       tok.Domain,
		UID:          acct.UID,
		EnterpriseID: acct.EnterpriseID,
		Nickname:     acct.Nickname,
		FilePath:     filepath.Join(p.cfg.AuthDir, fmt.Sprintf("workbuddy-%s.json", acct.UID)),
	}
	// global 登录：落盘 auth.realm=global（Realm() 按此判域；不写则依赖 domain 后缀回落）。
	if sess.realm == "global" {
		if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
			// 与同文件另外四个终态分支同构（BUG-02）：auth/token 已成功 = 上游已消费
			// 该 state，若按"可重试"处理（completed 保持 false → defer 复位 consumed），
			// 前端 3 秒一次 poll 会空转到 15 分钟 TTL，用户拿不到"请重新发起"的提示。
			completed = true
			p.loginMu.Lock()
			delete(p.logins, state)
			p.loginMu.Unlock()
			writeErr(w, http.StatusInternalServerError, "凭证已签发但写入 realm 失败，请重新发起添加账号："+err.Error())
			return
		}
	} else {
		// CN 也显式补 realm 键（幂等），让 auth 文件形态统一（与 LoadDir 存量迁移对齐）。
		_, _ = a.BackfillRealm()
	}
	if err := a.SaveAtomic(); err != nil {
		// 同上（bug #2）：state 已被上游消费，按终态处理，别让前端空转到 TTL。
		completed = true
		p.loginMu.Lock()
		delete(p.logins, state)
		p.loginMu.Unlock()
		writeErr(w, http.StatusInternalServerError, "凭证已签发但落盘失败，请重新发起添加账号："+err.Error())
		return
	}
	p.cfg.Pool.Add(a)
	p.cfg.Pool.Revive(acct.UID) // 全新登录 = 人工恢复口径：清掉旧号遗留的禁用/冷却/熔断

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
	} else if !a.IsEnterprise() {
		// 企业版跳过签到：上游 POST /v2/billing/meter/daily-checkin 对企业号
		// 400 code 10001「企业账号不支持该操作」，发了只会把该错误写进登录返回。
		// 下方 UserResource 余额查询照常（企业额度走 get-enterprise-user-usage 口径）。
		if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
			// 「今天已签到」也算已签（BUG-8）：不记的话账号行一直显示「签到」，
			// 用户点一次才提示「今天已签到」。与 panel.go / scheduler.go 同口径。
			if upstream.IsAlreadyCheckin(err) {
				p.cfg.Pool.NoteCheckinDone(acct.UID)
			}
			checkinMsg = err.Error()
		}
	}
	if rm, tt, exp, at, rem, uerr := p.cfg.Upstream.UserResourceDetailedWithExpiry(a, p.expiringSoonWindow()); uerr == nil {
		remain, total = rm, tt
		p.cfg.Pool.ReenableIfCredits(acct.UID, rm, tt)
		// 重建到期快照（BUG-12）：ReenableIfCredits 内部会把 creditsExpiring /
		// creditsEarliestExpiry / creditsEarliestRemaining 全部清零，不重建的话新号
		// 在首次签到或余额刷新之前，prefer_expiring 加权与到期展示全部失效。
		p.cfg.Pool.SetCreditsDetailed(acct.UID, rm, tt, exp, at, rem)
	}

	completed = true // 完成路径：后面的 delete 会移除 state，defer 无需复位
	p.loginMu.Lock()
	delete(p.logins, state)
	p.loginMu.Unlock()
	log.Printf("panel: 新账号已热加载 uid=%s nickname=%q realm=%s（免重启生效）", acct.UID, acct.Nickname, sess.realm)
	resp := map[string]any{
		"done":            true,
		"uid":             acct.UID,
		"nickname":        acct.Nickname,
		"realm":           sess.realm,
		"credits_total":   total,
		"checkin_message": checkinMsg,
	}
	// 余额未知时**不下发** credits（bug #13）：以前下发 -1 当哨兵，契约只写在
	// 前端那句 `r.credits >= 0` 里 —— 任何新调用方（脚本/别的前端）都会把 -1
	// 当成真实余额。-1 = 查询失败（余额接口报错/超时），"缺字段"才是通用语义。
	if remain >= 0 {
		resp["credits"] = remain
	} else {
		resp["credits_ok"] = false
	}
	writeJSON(w, http.StatusOK, resp)
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
