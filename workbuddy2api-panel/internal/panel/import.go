package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// cockpitAccount 映射 cockpit tools 导出格式的单个账号。
type cockpitAccount struct {
	ID             string `json:"id"`
	Email          string `json:"email"`
	UID            string `json:"uid"`
	Nickname       string `json:"nickname"`
	AccessToken    string `json:"access_token"`
	RefreshToken   string `json:"refresh_token"`
	TokenType      string `json:"token_type"`
	ExpiresAt      int64  `json:"expires_at"`
	Domain         string `json:"domain"`
	DosageNotify   string `json:"dosage_notify_code"`
	PaymentType    string `json:"payment_type"`
	Status         string `json:"status"`
	UsageUpdatedAt int64  `json:"usage_updated_at"`
	LastCheckin    int64  `json:"last_checkin_time"`
	CheckinStreak  int    `json:"checkin_streak"`
	CreatedAt      int64  `json:"created_at"`
	LastUsed       int64  `json:"last_used"`
}

// postImportActivate 导入后的后置动作（异步执行，bug #23）：注册激活 / 领 trial /
// 签到 / 回读余额。失败只记日志，不影响导入结果。
func (p *Panel) postImportActivate(a *auth.Auth, realm string) {
	if a == nil || p.cfg.Upstream == nil {
		return
	}
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("panel: import 后置动作 panic uid=%s: %v", a.UID, rec)
		}
	}()
	if realm == "global" {
		if activated, err := p.cfg.Upstream.GlobalCompleteRegistration(a); err != nil {
			log.Printf("panel: import global 注册激活 uid=%s: %v", a.UID, err)
		} else if activated {
			log.Printf("panel: import global 注册激活 uid=%s 完成", a.UID)
		}
		if claimed, err := p.cfg.Upstream.ClaimTrial(a); err != nil {
			log.Printf("panel: import global trial uid=%s: %v", a.UID, err)
		} else if claimed {
			log.Printf("panel: import global trial uid=%s 已领", a.UID)
		}
	} else {
		if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
			log.Printf("panel: import checkin uid=%s: %v", a.UID, err)
		}
	}
	if rm, tt, err := p.cfg.Upstream.UserResource(a); err == nil {
		p.cfg.Pool.ReenableIfCredits(a.UID, rm, tt)
	}
}

// importCockpit 接收 cockpit tools 导出的 JSON 文件，批量导入账号到池中。
//
//	POST /panel/api/import/cockpit
//	Content-Type: multipart/form-data
//	Body: file=<json>
//
// 返回 {ok, total, imported, skipped, errors}。
func (p *Panel) importCockpit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "parse form: "+err.Error())
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "missing file field: "+err.Error())
		return
	}
	defer file.Close()

	// 32MB 只是 ParseMultipartForm 的内存阈值（超出会落到临时文件），文件本身
	// 仍会被整份读进内存 —— 这里再套一层硬上限，别让一个超大上传把进程吃穿。
	raw, err := io.ReadAll(io.LimitReader(file, panelMaxBodyBytes+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read file: "+err.Error())
		return
	}
	if len(raw) > panelMaxBodyBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "import file too large (limit 32MB)")
		return
	}

	var accounts []cockpitAccount
	if err := json.Unmarshal(raw, &accounts); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if len(accounts) == 0 {
		writeErr(w, http.StatusBadRequest, "empty accounts array")
		return
	}

	var total, imported, skipped int
	var errs []string

	for _, acc := range accounts {
		uid := strings.TrimSpace(acc.UID)
		at := strings.TrimSpace(acc.AccessToken)
		rt := strings.TrimSpace(acc.RefreshToken)
		if uid == "" || at == "" || rt == "" {
			skipped++
			errs = append(errs, fmt.Sprintf("missing required fields (id=%s)", acc.ID))
			continue
		}
		if !validImportUID(uid) {
			skipped++
			errs = append(errs, fmt.Sprintf("invalid uid (id=%s)", acc.ID))
			continue
		}

		// 按 domain 推断 realm：workbuddy.ai 家族 → global，否则 cn。
		realm := auth.ResolveRealm("", acc.Domain)

		// cockpit tools 的 expires_at 为毫秒时间戳，转为秒。
		//
		// 缺失时**不得**伪造远期（issue ISSUE-08）：NeedsRefresh 依据 ExpiresAt 判定，
		// 写一年后会让 token 过期后永不主动刷新，只能等 chat 401/12153 三振禁用。
		// 这里写 now+1h —— 留一次刷新窗口，且首次请求后就会进入正常刷新节奏。
		// 按**量级**判单位（bug #6）：只假设毫秒的话，导出工具给秒（1760000000）
		// 时 /1000 得到 1760000（1970-01），既非 0 也进不了兜底，NeedsRefresh 恒真
		// —— 每个 chat 请求前都先打一次 refresh。
		raw := acc.ExpiresAt
		if raw > 1e11 {
			raw /= 1000 // 毫秒（13 位）→ 秒
		}
		if raw <= 0 || raw < time.Now().Add(-24*time.Hour).Unix() {
			raw = time.Now().Add(time.Hour).Unix()
			log.Printf("panel: import uid=%s 的 expires_at 缺失或明显过期，按 now+1h 处理（首次请求即刷新）", acc.UID)
		}
		expiresAt := raw

		nickname := acc.Nickname
		if strings.TrimSpace(nickname) == "" {
			nickname = acc.Email
		}

		a := &auth.Auth{
			AccessToken:  at,
			RefreshToken: rt,
			ExpiresAt:    expiresAt,
			Domain:       acc.Domain,
			UID:          uid,
			Nickname:     nickname,
			FilePath:     filepath.Join(p.cfg.AuthDir, fmt.Sprintf("workbuddy-%s.json", uid)),
		}

		if realm == "global" {
			if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
				skipped++
				errs = append(errs, fmt.Sprintf("uid=%s: set realm failed: %v", uid, err))
				continue
			}
		} else {
			_, _ = a.BackfillRealm()
		}

		if err := a.SaveAtomic(); err != nil {
			skipped++
			errs = append(errs, fmt.Sprintf("uid=%s: save auth failed: %v", uid, err))
			continue
		}

		p.cfg.Pool.Add(a)
		p.cfg.Pool.Revive(uid)

		imported++
		// 后置动作（签到/注册激活/领 trial/查余额）**丢到后台**（bug #23）：
		// 每号 2 次上游调用，几十个号串行 = 几十秒到几分钟，全部跑完才返回 →
		// 浏览器/反代先超时，前端报失败但账号其实已导入一半，用户重试又重复导入。
		go p.postImportActivate(a, realm)
		continue
	}

	// 后置动作已异步化（见上面的 go p.postImportActivate）。
	total = len(accounts)
	log.Printf("panel: cockpit import finished total=%d imported=%d skipped=%d", total, imported, skipped)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"total":    total,
		"imported": imported,
		"skipped":  skipped,
		"errors":   errs,
	})
}

// validImportUID 校验导入 uid 是否可用于拼文件名（同 login.go validUID 口径）。
func validImportUID(uid string) bool {
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
