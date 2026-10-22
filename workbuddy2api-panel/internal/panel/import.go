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

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
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
	} else if !a.IsEnterprise() {
		// 企业号没有个人成长体系（上游对 daily-checkin 一律 400 code 10001
		// "企业账号不支持该操作"）：同仓库的 login.go / panel.go / scheduler.go 都做了
		// IsEnterprise 分流，只有导入后置动作漏了（bug B11）——导入几十个企业号
		// 就是几十条假错误刷日志。余额查询照常走下面的 UserResource。
		if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
			// 同上（BUG-8）：已签到的形态也要标记，否则导入的号一直显示「签到」
			if upstream.IsAlreadyCheckin(err) {
				p.cfg.Pool.NoteCheckinDone(a.UID)
			}
			log.Printf("panel: import checkin uid=%s: %v", a.UID, err)
		} else {
			// 同上（报告 #11）：成功分支也要标记，否则导入的号一直显示「签到」。
			p.cfg.Pool.NoteCheckinDone(a.UID)
		}
	}
	if rm, tt, exp, at, rem, uerr := p.cfg.Upstream.UserResourceDetailedWithExpiry(a, p.expiringSoonWindow()); uerr == nil {
		p.cfg.Pool.ReenableIfCredits(a.UID, rm, tt)
		// 与登录路径同口径（BUG-12）：ReenableIfCredits 会清空到期快照，必须重建。
		p.cfg.Pool.SetCreditsDetailed(a.UID, rm, tt, exp, at, rem)
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
	// 内存阈值与文件大小上限分开（bug #12）：外层 MaxBytesReader 是 32MB，而
	// multipart 有 boundary 与头部开销，实际可读字节更少 —— 一个正好 32MB 的导出
	// 文件会在这里被截断，报 "unexpected EOF"，用户看到的是语法错误而不是"文件太大"。
	// 阈值给 1MB 余量（超出的部分会落到临时文件，不影响正确性）。
	if err := r.ParseMultipartForm(33 << 20); err != nil {
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

	// 后置动作并发上限（bug 18）：与 packages / syncNicknames 同款 sem=3
	importSem := make(chan struct{}, 3)
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
		// 变量名用 expRaw（bug B17）：此前叫 raw，与函数开头的 raw []byte（请求体）
		// 同名不同类型，遮蔽后极易在后续改动里误用。
		expRaw := acc.ExpiresAt
		if expRaw > 1e11 {
			expRaw /= 1000 // 毫秒（13 位）→ 秒
		}
		if expRaw <= 0 || expRaw < time.Now().Add(-24*time.Hour).Unix() {
			// 写 0（未知）而不是伪造 now+1h（BUG-2）：auth.NeedsRefresh(10m) 的判据是
			// now+10m >= ExpiresAt，伪造成 now+1h 会让前 50 分钟 NeedsRefresh 恒 false，
			// 于是每次都拿早已过期的 access token 打上游（401/12153），三振还会禁用账号。
			// 登录路径（login.go）在同样场景下就是留 0 让首次请求立即刷新 —— 两条口径统一。
			expRaw = 0
			log.Printf("panel: import uid=%s 的 expires_at 缺失或明显过期，按 0（未知）处理，首次请求即刷新", acc.UID)
		}
		expiresAt := expRaw

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
		//
		// 但要限并发（bug 18）：导入几百个号时裸 go 会同时发起几百组上游请求，
		// 正是仓库其它地方用 sem=3/4 专门防的 WAF 频控形态。
		// 抢槽必须在 goroutine **内部**（BUG-01）：放在外面时，从第 4 个账号起
		// handler 会卡在 channel 上等前 3 个后台动作跑完 —— 后台化被同步信号量抵消，
		// 100 个账号的导入接口要几分钟才回包，浏览器/反代先超时，用户重试又重复导入。
		go func(a *auth.Auth, realm string) {
			importSem <- struct{}{}
			defer func() { <-importSem }()
			defer func() {
				if r := recover(); r != nil {
					log.Printf("ERR: [panel] postImportActivate panic uid=%s: %v", a.UID, r)
				}
			}()
			p.postImportActivate(a, realm)
		}(a, realm)
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
