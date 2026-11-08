// export.go 导出已登录账号的凭据（GET /panel/api/export/accounts）。
//
// 为什么必须有：换包名/换签名之后旧包的私有目录搬不过来（/data/data 非 root 读不到、
// allowBackup=false、旧包非 debuggable），用户唯一能走通的迁移路径就是「旧面板导出 →
// 新面板导入」。导入（import.go）早就有了，导出一直缺 —— 只能靠 root 或者重新逐个 OAuth。
//
// 输出格式与导入接口**同构**：cockpit 风格的 snake_case 数组，字段名与 import.go 的
// cockpitAccount 对齐，所以导出的文件可以原样粘回「导入 JSON」页签。
//
// 安全：这条路由和其它 /panel/api/* 一样走 Bearer 鉴权（api_key）；响应带
// Cache-Control: no-store，避免凭据被浏览器/反代缓存。日志只记数量，不记 token。
package panel

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

)

// exportAccount 单个账号的导出形状（= 导入侧的 cockpitAccount 子集 + 观测字段）。
type exportAccount struct {
	ID           string `json:"id"`
	UID          string `json:"uid"`
	Nickname     string `json:"nickname"`
	Email        string `json:"email"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"` // 秒（导入侧兼容毫秒：>1e11 自动 /1000）
	Domain       string `json:"domain"`
	Realm        string `json:"realm"`
	EnterpriseID string `json:"enterprise_id"`
	Type         string `json:"type"` // 上游 account.type（personal/ultimate/exclusive）
	DeviceToken  string `json:"device_token"`
	// 观测字段：导入侧忽略（encoding/json 会丢掉未知键），但导出的文件里能看出
	// 「当时哪些号是被禁用的」，迁移后好对照。
	Disabled       bool   `json:"disabled,omitempty"`
	DisabledReason string `json:"disabled_reason,omitempty"`
	Paused         bool   `json:"paused,omitempty"`
}

// exportAccounts 把池里所有带 refresh token 的账号导成 JSON 数组。
//
// 只导「有 refresh token」的：没有它，导出去在新机器上第一次请求就会 401，等于废文件。
// 快照恢复出来的占位账号（只有 UID）也因此自然被排除。
func (p *Panel) exportAccounts(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Pool == nil {
		writeErr(w, http.StatusServiceUnavailable, "pool not available")
		return
	}
	statuses := p.cfg.Pool.List()
	out := make([]exportAccount, 0, len(statuses))
	for _, st := range statuses {
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		// 用 *Value() 访问器而不是直读字段：刷新 token 时上游会改写这些字段，
		// 直读是数据竞争（auth 包为此专门提供了带锁的取值器）。
		rt := strings.TrimSpace(a.RefreshTokenValue())
		if rt == "" {
			continue
		}
		nickname := strings.TrimSpace(st.Nickname)
		if nickname == "" {
			nickname = strings.TrimSpace(a.NicknameValue())
		}
		out = append(out, exportAccount{
			ID:             st.UID,
			UID:            st.UID,
			Nickname:       nickname,
			AccessToken:    strings.TrimSpace(a.AccessTokenValue()),
			RefreshToken:   rt,
			ExpiresAt:      a.ExpiresAtValue(),
			Domain:         strings.TrimSpace(a.DomainValue()),
			Realm:          a.Realm(),
			EnterpriseID:   strings.TrimSpace(a.EnterpriseID),
			Type:           strings.TrimSpace(a.AccountType),
			DeviceToken:    strings.TrimSpace(a.DeviceToken),
			Disabled:       st.Disabled,
			DisabledReason: st.DisabledReason,
			Paused:         st.Paused,
		})
	}

	// 文件名带时间戳：多次导出不互相覆盖，也便于和 Release 的构建时间对照。
	name := "workbuddy-accounts-" + time.Now().Format("20060102-1504") + ".json"
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	// 响应体是 refresh token：绝不能被缓存（浏览器、反代、Service Worker 都算）。
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		log.Printf("panel: 导出凭据写响应失败: %v", err)
		return
	}
	log.Printf("panel: 已导出 %d 个账号凭据（未记录任何 token）", len(out))
}
