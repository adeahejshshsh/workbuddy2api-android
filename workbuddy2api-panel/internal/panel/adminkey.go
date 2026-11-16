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
// 那正是 P0-1 要修的形态。所以这里用**来源地址**当第二因子：
//
//   - 只接受回环（127.0.0.1 / ::1）。能打到回环的人本来就有这台机器的执行权
//     （Android App 的 WebView 正是走 http://127.0.0.1:<port>/panel/），
//     而远端的客户端 key 持有者打不到；
//   - 已经配过的不走这条路：轮换要经过设置页（那里已被 admin 门禁保护），否则
//     拿到客户端 key 的人可以随时把管理员的 key 顶掉。
//
// 拒绝时把两条出路都写进错误文案（本机打开面板 / 直接改 config.json），不要只说
// 「不允许」——那只会让用户卡在原地。
package panel

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"unicode"
)

// adminKeyMinLen 管理员密钥的最短长度。16 是「随手敲一串」与「能抗住离线爆破」之间的
// 务实分界：面板会把它存进浏览器 localStorage，太短的串等于没设。
const adminKeyMinLen = 16

// adminKeyMaxLen 上限：挡住把整个配置文件撑大的粘贴事故，也避免超长串进日志。
const adminKeyMaxLen = 128

// adminKeyBootstrap POST /panel/api/admin_key
//
// 请求体 {"admin_api_key": "..."}。仅在「还没有独立 admin_api_key」且「请求来自回环」
// 时可用；成功后立刻热生效（走 SaveConfig 同一条深合并 + livecfg 路径），不需要重启。
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
	var req struct {
		AdminAPIKey string `json:"admin_api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
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
	// 日志只记来源与长度，不记密钥本身（面板日志会落盘、还能导出）。
	log.Printf("panel: admin_api_key 已通过本机引导写入（来源 %s，长度 %d）", r.RemoteAddr, len(key))
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
		return "admin_api_key 至少 " + strconv.Itoa(adminKeyMinLen) + " 位（面板会把它存在浏览器里，太短等于没设）"
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
