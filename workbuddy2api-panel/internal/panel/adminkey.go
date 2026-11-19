// adminkey.go 管理员密钥（admin_api_key）的**自举**入口。
//
// 为什么需要这个文件（2026-11-16 用户反馈）：
//
// P0-1 之后，没配 admin_api_key 的部署在敏感接口上一律 403 —— 导出凭据 / 读写配置 /
// 导入 / 登录 / 账号增删停改。而「配 admin_api_key」这件事本身要写配置，正好也在被拒
// 名单里：门锁上了，钥匙锁在屋里。老用户升级后打开面板，除了只读页什么都点不动，
// 而面板里也没有任何地方能填这个值。
//
// 为什么不干脆放开（回到「客户端 key 也能管面板」）：
//
// 下游客户端手里就有 api_key，放开等于把面板交给任何一个拿到客户端 key 的人 ——
// 那正是 P0-1 要修的形态。
//
// **第二因子必须是一次性令牌，不能是「回环」（W3-02）**：Android 上回环是跨 App
// 共享的 —— 任一本地 App 先 bind 上端口，再 POST 一次 /panel/api/admin_key，就能
// 把管理员密钥设成自己的值，随后 GET /panel/api/export/accounts 拿走全池 token，
// 原管理员再设置时收到 409。回环只说明「同一台设备」，不说明「同一进程」。
//
// 现在的判据：
//   - 进程启动时用 crypto/rand 生成 128 位一次性 bootstrap_token，**只存在内存**，
//     同时写入 <config 同目录>/panel_bootstrap_token（0600），并打一行带路径的日志；
//   - 宿主 App 可以直接把令牌注入页面（window.__WB2A_BOOTSTRAP_TOKEN），
//     用户在弹窗里也可手填（从上面那个文件读）；
//   - 常量时间比较；设置成功即从内存抹掉并删除文件 —— 同一个令牌不能重放；
//   - 回环校验保留为额外的廉价闸门（反代/远端请求一律拒），但它**不再是**自举的
//     唯一条件。
//   - 已经配过的不走这条路：轮换要经过设置页（那里已被 admin 门禁保护），否则
//     拿到客户端 key 的人可以随时把管理员的 key 顶掉。
//
// 拒绝时把出路都写进错误文案（读令牌文件 / 直接改 config.json），不要只说
// 「不允许」——那只会让用户卡在原地。
package panel

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

// adminKeyMinLen 管理员密钥的最短长度。16 是「随手敲一串」与「能抗住离线爆破」之间的
// 务实分界：太短的串等于没设。（W3-04 之后它只活在会话内存/sessionStorage 里，
// 不再落 localStorage，但长度门槛仍按「可能被离线爆破」定。）
const adminKeyMinLen = 16

// adminKeyMaxLen 上限：挡住把整个配置文件撑大的粘贴事故，也避免超长串进日志。
const adminKeyMaxLen = 128

// bootstrapTokenFile 自举令牌的落盘文件名（与 config.json 同目录 = Android 的
// files/ 私有目录，其它 App 读不到）。0600 由 writeBootstrapTokenFile 保证。
const bootstrapTokenFile = "panel_bootstrap_token"

// bootstrapTokenBytes 令牌熵：128 位。16 字节 hex 后是 32 个字符，手抄不现实，
// 所以必须由宿主注入或从文件复制 —— 这正是它作为「本进程所有权证明」的意义。
const bootstrapTokenBytes = 16

// initBootstrapToken 生成一次性令牌（New 里调用，早于任何请求）。
//
// 落盘失败不致命：内存里那份仍然能用（宿主 App 同进程/同 uid 读不到文件时，
// 还可以注入）。但 crypto/rand 失败必须 fail closed —— 拿不到真随机就宁可不给
// 自举入口，也不要发一个可预测的令牌（那等于把 W3-02 原样换个写法）。
func (p *Panel) initBootstrapToken() {
	var b [bootstrapTokenBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		log.Printf("WARN: panel: 无法生成管理员密钥自举令牌（crypto/rand: %v），自举入口关闭；"+
			"请直接改 config.json 的 admin_api_key", err)
		return
	}
	tok := hex.EncodeToString(b[:])
	p.bootstrapMu.Lock()
	p.bootstrapToken = tok
	p.bootstrapMu.Unlock()

	path := p.bootstrapTokenPath()
	if path == "" {
		log.Printf("panel: 未配置 ConfigPath，自举令牌只在内存中（由宿主注入页面）")
		return
	}
	if err := writeBootstrapTokenFile(path, tok); err != nil {
		log.Printf("WARN: panel: 自举令牌落盘失败（%v），仅内存有效（由宿主注入页面）", err)
		return
	}
	log.Printf("panel: 管理员密钥自举令牌已写入 %s（0600），设置管理员密钥时需填入", path)
}

// bootstrapTokenPath 令牌文件路径；未配置 ConfigPath 时返回空（测试与嵌入用法）。
func (p *Panel) bootstrapTokenPath() string {
	if strings.TrimSpace(p.cfg.ConfigPath) == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(p.cfg.ConfigPath), bootstrapTokenFile)
}

// writeBootstrapTokenFile 以 0600 写令牌。
//
// 不用 os.WriteFile：它只在**创建**时应用权限，文件已存在且权限更宽时会原样保留
// ——令牌文件被上一次运行留下 0644 的话，同一台机器上的其它用户就能读到。
func writeBootstrapTokenFile(path, tok string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	// 已存在的文件也别信旧权限：显式再 chmod 一次（OpenFile 的 perm 同样只在创建时生效）。
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.WriteString(tok + "\n"); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// BootstrapToken 当前一次性令牌（空 = 已用掉或生成失败）。
//
// 导出给宿主集成与测试用：正常路径是「宿主读文件后注入页面」，不经过 HTTP ——
// 任何能拿到令牌的 HTTP 接口本身就会把 W3-02 的洞重新打开。
func (p *Panel) BootstrapToken() string {
	p.bootstrapMu.Lock()
	defer p.bootstrapMu.Unlock()
	return p.bootstrapToken
}

// verifyBootstrapToken 常量时间比较（长度不同直接返回 0，不泄漏前缀信息）。
func (p *Panel) verifyBootstrapToken(got string) bool {
	got = strings.TrimSpace(got)
	p.bootstrapMu.Lock()
	want := p.bootstrapToken
	p.bootstrapMu.Unlock()
	if want == "" || got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// burnBootstrapToken 用后置空：内存抹掉 + 删文件。设置成功才调用 ——
// 新密钥校验失败（太短/带空白）不该把用户锁在门外，那只是手误。
func (p *Panel) burnBootstrapToken() {
	p.bootstrapMu.Lock()
	p.bootstrapToken = ""
	p.bootstrapMu.Unlock()
	if path := p.bootstrapTokenPath(); path != "" {
		_ = os.Remove(path)
	}
}

// adminKeyBootstrap POST /panel/api/admin_key
//
// 请求体 {"admin_api_key": "...", "bootstrap_token": "..."}。仅在「还没有独立
// admin_api_key」「请求来自回环」「bootstrap_token 匹配」时可用；成功后立刻热生效
// （走 SaveConfig 同一条深合并 + livecfg 路径），不需要重启，令牌随即作废。
func (p *Panel) adminKeyBootstrap(w http.ResponseWriter, r *http.Request) {
	if p.cfg.SaveConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config api not available")
		return
	}
	// 已经配过：这条引导路径关闭。轮换走设置页（那里要求当前 admin key）。
	if _, dedicated := p.apiKey(); dedicated {
		writeErr(w, http.StatusConflict,
			"admin_key_already_set：已配置 admin_api_key，轮换请用「设置 → 管理员密钥」（需要当前 admin key）")
		return
	}
	if !isLoopbackRequest(r) {
		writeErr(w, http.StatusForbidden,
			"admin_key_local_only：设置管理员密钥只接受本机访问（127.0.0.1）。"+
				"请在运行网关的这台机器上打开面板；或直接在 config.json 里写 "+
				`"admin_api_key": "<16 位以上随机串>"`+" 后重启")
		return
	}
	limitJSONBody(w, r)
	var req struct {
		AdminAPIKey    string `json:"admin_api_key"`
		BootstrapToken string `json:"bootstrap_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	// 令牌校验放在密钥校验之前：没有令牌的人连「密钥要几 位」都不该试出来。
	if !p.verifyBootstrapToken(req.BootstrapToken) {
		// 不在这里回显绝对路径（W3-20 同口径）：只说文件名，完整路径在启动日志里
		// （运维看得到，网络响应里不该出现部署结构）。
		writeErr(w, http.StatusForbidden,
			"bootstrap_token_required：缺少或有误的一次性令牌。请从数据目录下的 "+
				bootstrapTokenFile+"（0600，进程启动时生成，启动日志里有完整路径）复制，"+
				"或在宿主 App 里让它注入页面；也可直接改 config.json 的 admin_api_key 后重启")
		return
	}
	key := strings.TrimSpace(req.AdminAPIKey)
	if msg := validateAdminKey(key); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	// 与当前生效的客户端 key 相同 → 等于没分开，直接拒。这正是 P0-1 的原始形态：
	// 下游客户端拿到的那把 key 同时能管面板。
	if cur, _ := p.apiKey(); cur != "" && key == cur {
		writeErr(w, http.StatusBadRequest,
			"admin_api_key 不能与下游 api_key 相同：拿到客户端 key 的人就能管面板，正是这条要修的")
		return
	}
	payload, err := json.Marshal(map[string]string{"admin_api_key": key})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "marshal: "+err.Error())
		return
	}
	if _, err := p.cfg.SaveConfig(payload); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// 成功后立即作废令牌（W3-02）：一次性，重放拿不到第二次机会。
	p.burnBootstrapToken()
	// 日志只记来源与长度，不记密钥本身（面板日志会落盘、还能导出）。
	log.Printf("panel: admin_api_key 已通过一次性令牌引导写入（来源 %s，长度 %d），令牌已作废", r.RemoteAddr, len(key))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"note": "已生效，无需重启。面板此后用这把密钥鉴权 —— 请在设置页把它保存好，" +
			"丢了只能改 config.json 或删掉该键后重启",
	})
}

// validateAdminKey 校验管理员密钥，返回空串表示通过（否则是给用户看的中文原因）。
//
// 只做「明显不可用」的拦截：太短、太长、带空白/控制字符。不强制复杂度——随机串与
// 长口令都该放行，强扭的复杂度规则只会让人写 Pa55word! 这种。
func validateAdminKey(key string) string {
	if key == "" {
		return "admin_api_key 不能为空"
	}
	if len(key) < adminKeyMinLen {
		return "admin_api_key 至少 " + strconv.Itoa(adminKeyMinLen) + " 位（太短的串等于没设）"
	}
	if len(key) > adminKeyMaxLen {
		return "admin_api_key 最长 " + strconv.Itoa(adminKeyMaxLen) + " 位"
	}
	for _, r := range key {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "admin_api_key 不能包含空白或控制字符"
		}
	}
	return ""
}

// isLoopbackRequest 报告请求是否来自回环地址。
//
// 只看 RemoteAddr，**不看 X-Forwarded-For**：那个头是客户端可写的，拿它当判据等于
// 任何人加一行头就能自举管理员密钥。反代场景下 RemoteAddr 是反代自己（不是回环），
// 会落到「本机打开或改 config.json」那条提示上——宁可让反代用户改一次文件，
// 也不给伪造头留门。
func isLoopbackRequest(r *http.Request) bool {
	addr := strings.TrimSpace(r.RemoteAddr)
	if addr == "" {
		return false
	}
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
