// Package auth 解析 WorkBuddy auth 文件（嵌套形/扁平形双形态），
// 提供 refresh 后的原子写回。
package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// Auth 是归一化后的账号凭证（来源可以是插件 OAuth 嵌套形或手写扁平形）。
type Auth struct {
	// mu 串行化 RefreshToken 写与 SaveAtomic 读，防止并发写回半更新 token。
	mu sync.RWMutex // 读多写少：日志路径并发读 Nickname/凭证，写只在 refresh 与 SetNickname

	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // Unix 秒
	Domain       string
	// realm 账号域（"cn" / "global"），落盘于 auth.realm（嵌套形）或顶层 realm（扁平形）。
	// 空 = 缺省：Realm() 按 domain 后缀回落，最终恒非空。
	//
	// 命名注记：Go 不允许字段与方法同名，持久化字段用未导出 realm，计算访问器用
	// 导出的 Realm()（跨包调用全部走方法）。Parse/SaveAtomic/login 在包内读写字段。
	realm        string
	UID          string
	EnterpriseID string
	Nickname     string
	FilePath     string // 来源文件；refresh 后原子写回此处

	// DeviceToken 设备风控 Token（X-Device-Token 头），来源 auth 文件的 device_token 键。
	// 缺省为空 = 不注入该头（容器内无桌面端 Turing SDK 的常见部署）。
	// 手写扁平形 auth 文件可直接写 "device_token": "..."；插件 OAuth 嵌套形
	// 顶层 device_token 也会被解析（与桌面端共用状态文件的部署方式）。
	DeviceToken string
}

// NicknameValue 加锁读取 Nickname。
//
// 为什么需要：SetNickname（面板同步昵称）在 a.mu 内改写该字段，而所有日志与状态
// 快照都在锁外直读 a.Nickname —— 这是真实数据竞争（review P1-9）。AccessToken /
// RefreshToken / Domain 早已提供加锁读取器，Nickname 当初漏了。
func (a *Auth) NicknameValue() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Nickname
}

// Lock 供同进程内其他包（upstream.RefreshToken）在改写 Auth 字段期间加锁。
func (a *Auth) Lock() { a.mu.Lock() }

// Unlock 释放 a.Lock 获取的锁。
func (a *Auth) Unlock() { a.mu.Unlock() }

// 读访问器一律用 RLock（issue ISSUE-09）：这些方法在**每请求出站头构造**与
// **每次选号**（pool.mu 持锁下调 Realm()）里被调用，用写锁会让同账号并发请求
// 在写锁上排队。写方（RefreshToken / BackfillRealm）仍是 Lock，语义不变。
//
// AccessTokenValue 加锁读取 AccessToken（出站请求头一律经此取值，勿直读字段）。
//
// 为什么必须加锁：RefreshToken 在 a.mu 内改写 AccessToken/RefreshToken/Domain/ExpiresAt
// （client.go「第 2 段（锁内）：校验快照一致后写回」），而所有出站请求头构造
// （ChatHeaders / BillingHeaders / fetchEnterpriseModels / fetchV3Models /
// global_models）与调度器的 token 检查都在锁外直读这些字段。生产上两侧真会并发：
// Scheduler.RunKeepaliveNow 定时对**每个**非禁用账号刷新（与是否有在途请求无关），
// 而 handler 正基于**同一个** *auth.Auth 指针构造请求头（Pool.AuthByUID/List 返回的
// 就是池内同一个对象）。无同步直读构成数据竞争（go test -race 实证）。
func (a *Auth) AccessTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.AccessToken
}

// ExpiresAtValue 加锁读取 ExpiresAt（RefreshToken 在锁内改写它）。
func (a *Auth) ExpiresAtValue() int64 {
	if a == nil {
		return 0
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ExpiresAt
}

// DomainValue 加锁读取 Domain（同 AccessTokenValue：RefreshToken 在锁内改写它）。
func (a *Auth) DomainValue() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Domain
}

// RefreshTokenValue 加锁读取 RefreshToken（同 AccessTokenValue：RefreshToken 在锁内
// 改写它）。调度器的「有无凭证」前置守卫（checkin/keepalive/travel 的
// `a.RefreshToken == ""`）必须经此取值，勿直读字段。
func (a *Auth) RefreshTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.RefreshToken
}

// globalEnabled 全局开关：global realm 是否路由（D5 双保险）。
// 默认开启（与 config global.enabled 缺省 true 一致）：Realm() 正常按显式 realm/
// domain 判定 global/cn。显式 SetGlobalEnabled(false)（config "enabled": false）关闭
// → 逃生门：纯 CN 部署，即便 auth 文件写了 realm=global 或 domain 为 .workbuddy.ai
// 也恒判 cn——「关了才锁死」的单一闸口集中收敛在 Realm()/IsGlobal() 里。
var globalEnabled atomic.Bool

func init() { globalEnabled.Store(true) }

// SetGlobalEnabled 注入 global realm 路由开关（false = 锁死纯 CN，逃生门）。
func SetGlobalEnabled(enabled bool) { globalEnabled.Store(enabled) }

// GlobalEnabled 报告 global realm 路由开关当前状态（测试/运维观测）。
func GlobalEnabled() bool { return globalEnabled.Load() }

// Realm 返回账号的归一化域：显式 Realm=="global" 或 domain 后缀 .workbuddy.ai → "global"，
// 否则 "cn"。显式 global 优先于 domain 回落（D1）。
// 全局开关 SetGlobalEnabled(false) 时恒 "cn"（逃生门：纯 CN 锁定，不影响默认行为）。
// 空 realm + 空 domain → "cn"（老 CN 凭证零回归）。
func (a *Auth) Realm() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.realmLocked()
}

// realmLocked Realm 的无锁内部实现：仅限**已持 a.mu** 的调用方使用（sync.Mutex 不可重入，
// 锁内再调 Realm() 会自锁）。realm 由 BackfillRealm 改写、Domain 由 RefreshToken 在锁内
// 改写，故读取必须与写方同锁（理由见 AccessTokenValue 注释）。
func (a *Auth) realmLocked() string {
	if !globalEnabled.Load() {
		return "cn"
	}
	if strings.TrimSpace(a.realm) == "global" || isGlobalDomain(a.Domain) {
		return "global"
	}
	return "cn"
}

// ResolveRealm 归一化 realm（cn/global）：显式非空优先，否则按原始 domain 推断
// （isGlobalDomain）。不受逃生门影响（逃生门是路由锁，不应影响标识判定）；
// domain 也为空 → "cn"（老 CN 凭证零回归）。
func ResolveRealm(explicit, domain string) string {
	if r := strings.TrimSpace(explicit); r != "" {
		return r
	}
	if isGlobalDomain(domain) {
		return "global"
	}
	return "cn"
}

// BackfillRealm 为缺省 realm 标识的账号持久化补标识：a.realm 为空时按「原始 domain 推断」
// 写回（cn/global），返回 (是否有变更, 归一化后的 realm)。已有标识不动（幂等）。
//
// 注意用 isGlobalDomain(a.Domain) 直接推断，而非 Realm()——Realm() 在逃生门
// （SetGlobalEnabled(false)）下恒降级 cn，把 global 账号写死成 cn 会永久污染凭证
// （逃生门是纯 CN 部署的临时锁，不应改写落盘数据）。domain 也为空时写 "cn"（老 CN 凭证）。
func (a *Auth) BackfillRealm() (bool, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.realm) != "" {
		return false, a.realm
	}
	r := ResolveRealm("", a.Domain)
	a.realm = r
	return true, r
}

// RealmStored 直读持久化的 realm 标识（可能为空 = 未 backfill 的旧文件，Realm() 会 fallback）。
func (a *Auth) RealmStored() string {
	a.mu.RLock() // 只读（bug B4）
	defer a.mu.RUnlock()
	return a.realm
}

// BackfillRealmFor 显式写入 realm 标识（包外登录路径使用：panel login 已知用户选了
// global，直接落盘 realm=global，不依赖 domain 后缀推断）。realm 需为 cn/global，
// 非法值报错（防写脏）。返回是否发生变更。
func BackfillRealmFor(a *Auth, realm string) (bool, error) {
	if a == nil {
		return false, fmt.Errorf("nil auth")
	}
	switch strings.TrimSpace(realm) {
	case "cn", "global":
	default:
		return false, fmt.Errorf("realm must be cn/global, got %q", realm)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.realm == realm {
		return false, nil
	}
	a.realm = realm
	return true, nil
}

// IsGlobal 报告账号是否属于 global realm（= Realm() == "global"）。
func (a *Auth) IsGlobal() bool { return a.Realm() == "global" }

// IsEnterprise 报告账号是否为企业版（auth 文件带非空 enterpriseId）。
//
// 企业版**没有个人成长体系**：签到 / 成长任务 / 连登管家 / 猫猫旅行 / 夜猫子
// 在上游一律被拒——实测（2026-10-07，同一时刻 A/B 对照）：
//   - POST /v2/billing/meter/daily-checkin                → 400 code 10001「企业账号不支持该操作」
//   - POST /billing/meter/claim-gift / claim-compensation → 400 code 10001 同上
//   - GET  /activity/growth/{streak,buddy/info,heatmap,lottery/summary}
//     与 GET /v2/activity/growth/tasks                    → 403「growth system is only available for personal users」
//
// 因此引用处（scheduler 五类任务 + panel 成长任务/签到入口）必须与 IsGlobal 门控
// 并列跳过，避免每日对注定失败的端点空发请求、面板按钮必然报错。
//
// 不受影响的能力：选号派发、保活（token 刷新）、额度查询（企业额度改走
// /v2/billing/meter/get-enterprise-user-usage，见 upstream.enterpriseResource）。
//
// 直读字段（EnterpriseID 自 Parse 后不再改写，与 upstream 读取方式一致），
// 无需加锁——Realm() 加锁是因为 realm/Domain 会被 RefreshToken 在锁内改写。
func (a *Auth) IsEnterprise() bool { return strings.TrimSpace(a.EnterpriseID) != "" }

// isGlobalDomain 判定 domain 是否指向 www.workbuddy.ai 家族。
// 同时接受裸域 workbuddy.ai 与任意子域（HasSuffix("www.workbuddy.ai") 或裸域本身）。
func isGlobalDomain(d string) bool {
	d = strings.ToLower(strings.TrimSpace(d))
	if d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai") {
		return true
	}
	// 官方 CLI 国际站域（product.json externalDomain: www.codebuddy.ai）。与
	// workbuddy.ai 同后端不同主机名，但 domain 落盘值不同——不认它会把国际站账号
	// 误判成 cn（显式 realm 键可兜住面板登录的账号，手写 auth 文件则直接错域路由，
	// 而 token 只在签发域可用）。
	return d == "codebuddy.ai" || strings.HasSuffix(d, ".codebuddy.ai")
}

// NeedsRefresh 报告 token 是否将在 within 内过期（或已过期/无 expiry）。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	// 只读判定用 RLock（bug B4）：这是 chat 热路径每请求都要调的（handler 里
	// acct.NeedsRefresh(...)），用写锁会让同账号并发请求在这里串行化。
	a.mu.RLock()
	defer a.mu.RUnlock()
	// 静态 API Key 永不刷新（refresh 端点只认 OAuth 设备流）：恒 false，否则请求
	// 路径会先 refresh、失败后把该号当坏号冷却换号 —— Key 本身是好的。
	// 判定只读 AccessToken，RLock 足够。
	if a.isAPIKeyLocked() {
		return false
	}
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// Parse 兼容两种磁盘形态：
//
//	嵌套形 {"auth":{...},"account":{...}}  （插件 OAuth 输出）
//	扁平形 {"accessToken":...,"uid":...}   （手写/旧版）
func Parse(raw []byte) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	var a Auth
	if _, nested := probe["auth"]; nested {
		var n struct {
			Auth struct {
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
				ExpiresAt    int64  `json:"expiresAt"`
				Domain       string `json:"domain"`
				Realm        string `json:"realm"`
			} `json:"auth"`
			Account struct {
				UID          string `json:"uid"`
				EnterpriseID string `json:"enterpriseId"`
				Nickname     string `json:"nickname"`
			} `json:"account"`
			// DeviceToken 顶层 device_token（嵌套形与扁平形共用；手写时无需嵌进 auth 对象）。
			DeviceToken string `json:"device_token"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:  n.Auth.AccessToken,
			RefreshToken: n.Auth.RefreshToken,
			ExpiresAt:    n.Auth.ExpiresAt,
			Domain:       n.Auth.Domain,
			realm:        n.Auth.Realm,
			UID:          n.Account.UID,
			EnterpriseID: n.Account.EnterpriseID,
			Nickname:     n.Account.Nickname,
			DeviceToken:  n.DeviceToken,
		}
	} else {
		var f struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
			Domain       string `json:"domain"`
			Realm        string `json:"realm"`
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
			DeviceToken  string `json:"device_token"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:  f.AccessToken,
			RefreshToken: f.RefreshToken,
			ExpiresAt:    f.ExpiresAt,
			Domain:       f.Domain,
			realm:        f.Realm,
			UID:          f.UID,
			EnterpriseID: f.EnterpriseID,
			Nickname:     f.Nickname,
			DeviceToken:  f.DeviceToken,
		}
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	// 归一在两条解析分支**汇合之后**（嵌套形/扁平形都要覆盖）：只归一其中一条，
	// 另一种形态的凭据照样会把到期时间推到公元 58800 年。
	a.ExpiresAt = normalizeExpiresAt(a.ExpiresAt)
	return &a, nil
}

// expiresAtMillisThreshold 区分「秒」与「毫秒」时间戳的分界（1e11）。
//
// 单位到 1e11 秒已是公元 5138 年，真实凭证不可能；而 1e11 毫秒 = 1973 年，
// 毫秒制当前值（~1.8e12）远在其上。阈值取在两者之间，不会误判任一侧。
const expiresAtMillisThreshold = 100_000_000_000

// normalizeExpiresAt 把上游凭据的 expiresAt 归一到 Unix 秒。
//
// 为什么需要：官方 CodeBuddy CLI 的凭据文件写的是**毫秒**（实测 1795254516410），
// 而本仓与旧插件 OAuth 落盘的是**秒**。直接按秒解释毫秒会把到期时间推到公元
// 58800 年 → NeedsRefresh 恒 false → token 永不刷新 → 到期后账号静默失效
// （表现为「用了一个月突然全部 401」，极难排查）。导入官方凭据是本仓支持的用法，
// 所以解析入口必须归一，而不是靠使用者手改文件。
func normalizeExpiresAt(v int64) int64 {
	if v > expiresAtMillisThreshold {
		return v / 1000
	}
	return v
}

// SaveAtomic 以嵌套形原子写回 FilePath（tmp + rename），保持嵌套形（插件可读）格式。
// SaveAtomic 把当前凭证原子写回磁盘。
//
// 锁内**只做快照**，磁盘 I/O 全在锁外（bug #13）：原先全程持 a.mu 做
// ReadFile + MarshalIndent + 写文件 + rename，而 pool.pick 持有池写锁并调用
// e.a.Realm()（需要 a.mu.RLock）—— 一次 token 落盘就会把全池选号堵住；
// 22:00 全量保活 46 个号依次落盘时，chat 请求会出现周期性卡顿。
//
// 并发写同一文件由调用方串行化（RefreshToken 与 SetNickname 各自有写前校验），
// 且每次写的都是完整快照 + 原子替换，最坏结果是后写者覆盖先写者，不会写坏文件。
// 防御：accessToken 为空时拒绝写回，避免误用空凭证覆盖有效文件。
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	if strings.TrimSpace(a.AccessToken) == "" {
		uid := a.UID
		a.mu.Unlock()
		return fmt.Errorf("save refused: empty accessToken (uid=%s)", uid)
	}
	if a.FilePath == "" {
		a.mu.Unlock()
		return fmt.Errorf("no FilePath set")
	}
	snap := authFileSnapshot{
		path:         a.FilePath,
		uid:          a.UID,
		accessToken:  a.AccessToken,
		refreshToken: a.RefreshToken,
		expiresAt:    a.ExpiresAt,
		domain:       a.Domain,
		realm:        a.realm,
		enterpriseID: a.EnterpriseID,
		nickname:     a.Nickname,
		deviceToken:  a.DeviceToken,
	}
	a.mu.Unlock()
	return writeAuthSnapshot(snap)
}

// authFileSnapshot SaveAtomic 锁内取出的落盘快照（锁外 I/O 只读它）。
type authFileSnapshot struct {
	path                                          string
	uid, accessToken, refreshToken, domain, realm string
	enterpriseID, nickname, deviceToken           string
	expiresAt                                     int64
}

// writeAuthSnapshot 把快照写成 auth 文件（全部 I/O 在此，不持 a.mu）。
func writeAuthSnapshot(s authFileSnapshot) error {
	// 先读回原文件，保留用户/插件手写的未知键（review P2-14）：此前只重写已知
	// 字段，第一次 token 刷新后用户在文件里加的键就静默消失了。读取失败（文件
	// 不存在/损坏）时从空 map 起，行为与旧版一致。
	doc := map[string]any{}
	if prevRaw, rerr := os.ReadFile(s.path); rerr == nil {
		_ = json.Unmarshal(prevRaw, &doc)
		// 文件内容是 JSON `null` 时 Unmarshal 会把 doc 置成 nil map（不是留空 map），
		// 后面 doc[key] = dst 直接 panic（报告 #1）。这里补回空 map。
		if doc == nil {
			doc = map[string]any{}
		}
	}
	// 已知键覆盖、子对象里的**未知键保留**（bug #11）：此前 `doc["auth"] = map{...}`
	// 是整组替换，用户在 auth/account 里手写的扩展键（自定义域字段、附加 token）
	// 一次 refresh 后就消失 —— 而上面的注释承诺的是"保留手写的未知键"。
	putMerged := func(key string, known map[string]any) {
		dst := map[string]any{}
		if old, ok := doc[key].(map[string]any); ok {
			for k, v := range old {
				dst[k] = v
			}
		}
		for k, v := range known {
			dst[k] = v
		}
		doc[key] = dst
	}
	putMerged("auth", map[string]any{
		"accessToken":  s.accessToken,
		"refreshToken": s.refreshToken,
		"expiresAt":    s.expiresAt,
		"domain":       s.domain,
		"realm":        s.realm,
	})
	putMerged("account", map[string]any{
		"uid":          s.uid,
		"enterpriseId": s.enterpriseID,
		"nickname":     s.nickname,
	})
	// 扁平形遗留键必须清掉（bug 16）：把扁平文件升级成嵌套形时只 putMerged 了
	// auth/account，顶层的 accessToken/refreshToken/... 原样留着 —— 功能上暂时正确
	// （Parse 优先读嵌套形），但文件里永久多一份旧 token，任何按扁平键读的外部
	// 工具/插件都会拿到过期凭证。只删已知键，用户手写的未知键继续保留。
	for _, k := range []string{
		"accessToken", "refreshToken", "expiresAt", "domain", "realm",
		"uid", "enterpriseId", "nickname",
	} {
		delete(doc, k)
	}
	// DeviceToken 非空才写回顶层 device_token：避免在无该字段的旧文件里引入空键
	// （保持与插件 OAuth 输出形状一致，插件读取忽略未知键）。
	if s.deviceToken != "" {
		doc["device_token"] = s.deviceToken
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	// 用公共的原子替换（含单文件 bind mount 的 EBUSY 兜底，bug #4）。
	if err := ReplaceAtomic(s.path, raw, 0o600); err != nil {
		// Docker bind-mount 权限问题的典型现场：容器内 app 用户（uid 10001）
		// 对宿主机挂载目录无写权限。给出可操作指引而不是裸 syscall 错误。
		msg := fmt.Sprintf("写入 %s 失败: %v", s.path, err)
		if errors.Is(err, fs.ErrPermission) {
			msg += "\n（Docker 部署：容器内用户对宿主机挂载目录无写权限。解法任选：" +
				"1) 以本机 uid 运行容器：PUID=$(id -u) PGID=$(id -g) docker compose up -d；" +
				"2) sudo chown -R 10001:10001 ./auths ./data ./config.json；" +
				"3) compose 设 user: \"0:0\" 以 root 运行）"
		}
		return errors.New(msg)
	}
	return nil
}

// atomicSeq 原子替换的临时文件名序号（bug-01：唯一临时名，避免并发写交错）。
var atomicSeq atomic.Uint64

// ReplaceAtomic 用 tmp 原子替换 path，并对「单文件 bind mount」做 EBUSY 兜底。
//
// 为什么需要（bug #4）：Docker 用 `-v ./data/state.json:/app/data/state.json` 这种
// 单文件挂载时，rename 到挂载点恒返回 EBUSY，state.json / auths 的落盘会静默失败
// （冷却截止、熔断计数、成本账本只留内存，重启全丢）。config.json 早已单独处理了
// 这种情况，state/auth 一直没同步 —— 抽成公共函数三处共用。
//
// 返回 nil 表示内容已落盘（原子替换成功，或 bind mount 原地覆写成功）。
func ReplaceAtomic(path string, raw []byte, mode os.FileMode) error {
	// 临时名必须唯一（bug-01）：固定 `path+".tmp"` 时，同一账号的两个并发 SaveAtomic
	// 会共用同一个临时文件 —— A 截断并写完后，B 截断、只写了一半，此时 A 的 rename
	// 把**半截内容**换成正式文件（文件长度正常，JSON 已损坏），下次启动 LoadDir
	// Parse 失败后静默跳过 → 面板上少一个号，日志里没有任何提示。
	// v1.27.6 把 SaveAtomic 的磁盘 I/O 挪出 a.mu（性能修复）后，这条竞争才真正可达。
	tmp := fmt.Sprintf("%s.tmp.%d.%d", path, os.Getpid(), atomicSeq.Add(1))
	// 写临时文件后**先 fsync 再 rename**（misakano7545 `4356a346`）：只 rename 不 fsync
	// 时，掉电可能留下「文件名已换、内容还在页缓存」的半截文件 —— 表现为下次启动
	// 解析失败、账号/冷却状态整段丢失。这一处修好，auth 与 pool 两条落盘路径同时受益。
	if err := writeFileSync(tmp, raw, mode); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err == nil {
		return nil
	} else if !errors.Is(err, syscall.EBUSY) {
		_ = os.Remove(tmp)
		return err
	}
	// bind mount：原地覆写（tmp 里是完整新内容，写失败时保留供人工恢复）
	f, openErr := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, mode)
	if openErr != nil {
		return openErr
	}
	_, werr := f.Write(raw)
	serr := f.Sync()
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	if serr != nil {
		return serr
	}
	if cerr != nil {
		return cerr
	}
	_ = os.Remove(tmp)
	return nil
}

// SaveAtomicRetry 在 SaveAtomic 基础上做有界重试（0/100ms/300ms）。
//
// 为什么需要：refresh 成功后上游已经轮换了 RT，新 token 只在内存里；如果这一次
// 落盘失败（磁盘瞬时满、bind-mount 抖动），进程重启会拿旧 RT 去刷新——旧 RT 可能
// 已被消费，账号直接进入「需要重新登录」。重试把瞬时失败挡掉；仍失败由调用方
// 打 ERR（错误汇总可见），至少不让问题静默。
func (a *Auth) SaveAtomicRetry() error {
	var err error
	for i, delay := range []time.Duration{0, 100 * time.Millisecond, 300 * time.Millisecond} {
		if i > 0 {
			time.Sleep(delay)
		}
		if err = a.SaveAtomic(); err == nil {
			return nil
		}
	}
	return err
}

// LoadDir 扫描并解析 dir 下 workbuddy*.json；解析失败的文件静默跳过（启动日志由调用方统计）。
// 顺带做 realm 标识存量迁移：对空 realm 的 auth 自动 backfill（原始 domain 推断）并 SaveAtomic
// 落盘，一次性把旧文件补上 realm 键。单个文件写失败不阻断启动（log WARN 继续），
// 避免历史 auth 目录个别文件不可写时整个服务起不来。
func LoadDir(dir string) ([]*Auth, error) {
	files, err := filepath.Glob(filepath.Join(dir, "workbuddy*.json"))
	if err != nil {
		return nil, err
	}
	// seenUID 重复 UID 检测：同 UID 出现在多个文件时（双 realm 同名 UID 概率近零）
	// 打 WARN 告警含两文件路径，由「后载入者胜出」保持现状行为（不改变加载结果）。
	seenUID := make(map[string]string, len(files))
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			continue
		}
		// 缺 uid 的文件一律不进池（bug #1）：pool 以 UID 作 map key，空 key 会让
		// auths/ 下所有缺 uid 的文件互相顶替（只有第一个能进，其余被 seenUID 当重复
		// 丢弃），而面板路由 /panel/api/accounts/{uid}/... 也匹配不到空路径段 ——
		// 结果是「能发请求，但复活/禁用/签到/删除全点不动」，流水日志里 UID8 只剩 "-"。
		// 与 Parse 的 "missing accessToken" 同口径：宁可跳过并告警，也不留半残的号。
		if strings.TrimSpace(a.UID) == "" {
			log.Printf("WARN: auth 文件缺 uid，已跳过（补 \"uid\" 字段后重启即恢复）：%s", f)
			continue
		}
		a.FilePath = f
		if prev, ok := seenUID[a.UID]; ok {
			// 真的去重（bug-4）：此前只打日志、两条都 append，而 pool.upsertLocked 是
			// "后写覆盖" → 实际保留的是**后出现的**文件，与日志说的"保留 prev"相反。
			// 保留先出现的（files 已排序，结果确定），并如实说明丢弃了谁。
			log.Printf("WARN: uid %s duplicated — 保留 %s，丢弃 %s（不同 realm 同名 UID？）",
				logfmt.Label(a.UID, a.NicknameValue()), prev, f)
			continue
		}
		seenUID[a.UID] = f
		if a.RealmStored() == "" {
			if changed, r := a.BackfillRealm(); changed {
				if err := a.SaveAtomic(); err != nil {
					log.Printf("WARN: auth %s realm backfill save: %v", logfmt.Label(a.UID, a.NicknameValue()), err)
				} else if r == "global" {
					log.Printf("auth %s 存量迁移: 补 realm=global（domain=%s）", logfmt.Label(a.UID, a.NicknameValue()), a.Domain)
				}
			}
		}
		out = append(out, a)
	}
	return out, nil
}

// IsEnterpriseSession 报告当前凭证是否处于「企业上下文」（上游 PR #162）：
// 账号本身是企业版，或 AccessToken 签发自**企业切换会话**（JWT payload 带
// token_source=enterprise_switch —— 浏览器在企业空间状态下完成 OAuth 时签发）。
//
// 为什么单独一站：企业切换会话与真企业版的上游表现不同 —— 成长任务列表/额度查询
// 等**读端点照常**，但 /v2/report 行为事件**返回 200 却被静默丢弃**（任务进度永不
// 跳动、连登/签到无效）。不跳过的话，队列每轮白跑全部行为任务且无任何报错。
//
// 每次现解码 JWT（RefreshToken 会整体换发，企业切换状态可能翻转；解码只是一次
// base64 + 小 JSON，成本可忽略）。
func (a *Auth) IsEnterpriseSession() bool {
	if a == nil {
		return false
	}
	if a.IsEnterprise() {
		return true
	}
	return jwtTokenSource(a.AccessTokenValue()) == "enterprise_switch"
}

// jwtTokenSource 从 JWT 的 payload 段取 token_source（非 JWT / 解不出 → 空串）。
func jwtTokenSource(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var payload struct {
		TokenSource string `json:"token_source"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return ""
	}
	return strings.TrimSpace(payload.TokenSource)
}

// writeFileSync 写文件并 fsync 落盘（rename 前必须先落盘，见 ReplaceAtomic 注释）。
func writeFileSync(path string, raw []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// apiTokenPrefixes 静态 API Key 的前缀集合：ck_（CodeBuddy 控制台签发）与
// pt_（官方 CLI @tencent-ai/codebuddy-code 另有该前缀）。
//
// 官方 CLI 的判据是 tokenType === "ApiKey" 且 token 以这两个前缀之一开头；auth
// 文件不持久化 tokenType（refresh 只认 OAuth 设备流，静态 Key 无法刷新），前缀是
// 落盘形态里唯一稳定、跨进程一致的判据。
var apiTokenPrefixes = []string{"ck_", "pt_"}

// isAPIKeyToken 判 token 是否以静态 API Key 前缀开头。
func isAPIKeyToken(at string) bool {
	for _, p := range apiTokenPrefixes {
		if strings.HasPrefix(at, p) {
			return true
		}
	}
	return false
}

// isAPIKeyLocked IsAPIKey 的无锁内部实现：仅限**已持 a.mu** 的调用方使用
// （sync.Mutex 不可重入，锁内再调 AccessTokenValue 会自锁）。
func (a *Auth) isAPIKeyLocked() bool { return isAPIKeyToken(a.AccessToken) }

// IsAPIKey 报告账号是否为静态 API Key 账号（AccessToken 以 ck_/pt_ 开头）。
//
// 静态 Key 直接可用（Authorization: Bearer <key>），但它**没有**个人账号身份：
// 签到 / 成长任务 / 连登 / 旅行 / 夜猫子一律不适用，额度只能走企业口径端点
// （个人 get-user-resource 对静态 Key 返回空 Accounts），见 UsesEnterpriseBilling。
func (a *Auth) IsAPIKey() bool {
	if a == nil {
		return false
	}
	return isAPIKeyToken(a.AccessTokenValue())
}

// UsesEnterpriseBilling 报告额度查询是否该走企业口径端点
// （/v2/billing/meter/get-enterprise-user-usage）。
//
// 企业版与静态 API Key 同属此列：个人 get-user-resource 对二者均返回空 Accounts，
// 额度必须走企业口径，否则余额恒为 0 → 积分保底把可用号当成耗尽号挡掉。
func (a *Auth) UsesEnterpriseBilling() bool { return a.IsEnterprise() || a.IsAPIKey() }

// SupportsPersonalGrowth 报告账号是否具备个人成长体系（签到 / 成长任务 / 连登管家 /
// 猫猫旅行 / 夜猫子）。企业版与静态 API Key 都没有该体系。
func (a *Auth) SupportsPersonalGrowth() bool { return !a.IsEnterprise() && !a.IsAPIKey() }

// SupportsPersonalEndpoints 报告账号是否可调用「签到 / 任务列表」这类显式报错的个人端点。
// 与 SupportsPersonalGrowth（执行类门控）刻意分开命名：判据相同、语义不同。
func (a *Auth) SupportsPersonalEndpoints() bool { return !a.IsEnterprise() && !a.IsAPIKey() }
