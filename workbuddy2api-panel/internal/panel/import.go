package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// cockpitAccount 映射 cockpit tools 导出格式的单个账号。
type cockpitAccount struct {
	ID           string `json:"id"`
	Email        string `json:"email"`
	UID          string `json:"uid"`
	Nickname     string `json:"nickname"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresAt    int64  `json:"expires_at"`
	Domain       string `json:"domain"`
	// 企业标识：cockpit 导出里两种命名都出现过，任一非空即按企业号处理。
	// 丢了它，企业号导入后会退化成个人号 —— 签到/成长任务全线白打，还会被上游
	// 400 code 10001 刷一屏假错误。
	EnterpriseID      string `json:"enterprise_id"`
	EnterpriseIDCamel string `json:"enterpriseId"`
	DosageNotify      string `json:"dosage_notify_code"`
	PaymentType       string `json:"payment_type"`
	Status            string `json:"status"`
	UsageUpdatedAt    int64  `json:"usage_updated_at"`
	LastCheckin       int64  `json:"last_checkin_time"`
	CheckinStreak     int    `json:"checkin_streak"`
	CreatedAt         int64  `json:"created_at"`
	LastUsed          int64  `json:"last_used"`
	// DeviceToken 设备风控 token（X-Device-Token）。cockpit 导出与本仓 auths/*.json
	// 都可能带；丢了它，导入的号在风控维度上等于「换了台设备」，指纹会漂。
	DeviceToken string `json:"device_token"`
	// Realm 显式 realm（本仓 auths/*.json 的 auth.realm）。空 = 按 domain 推断。
	Realm string `json:"realm"`
	// Type 上游 account.type（personal / ultimate / exclusive）。企业版判定的权威判据：
	// 只看 enterpriseId 会把「带企业 ID 的个人号」判成企业版（签到/成长任务被门控）。
	Type string `json:"type"`
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
	} else if a.SupportsPersonalGrowth() {
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

	accounts, perr := parseImportPayload(raw)
	if perr != nil {
		writeErr(w, http.StatusBadRequest, perr.Error())
		return
	}
	if len(accounts) == 0 {
		writeErr(w, http.StatusBadRequest, "empty accounts array")
		return
	}

	// 凭证目录必须在第一次落盘前就位。login.go / apikey.go 各自在写盘前建了目录，
	// 唯独本文件漏了：全新部署（auths/ 还没生成、也没登录过任何账号）时，写盘用的
	// 临时文件创建直接 ENOENT，用户看到的是「导入失败」，而服务本身完全正常
	// —— LoadDir 用 filepath.Glob，目录不存在时返回空列表且不报错。
	if err := os.MkdirAll(p.cfg.AuthDir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "mkdir auth dir: "+err.Error())
		return
	}

	var total, imported, skipped int
	var errs, warnings []string
	// 覆盖已有凭据的号先收集，循环后统一有界并发求证（见 verifyImportOverwrites）：
	// 串行时几百个号每个都要等一次 refresh，请求会先被反代/浏览器掐断。
	var toVerify []importAcct
	var vmu sync.Mutex // 保护 imported/skipped/errs/warnings：求证回调并发进来

	// 后置动作并发上限（bug 18）：与 packages / syncNicknames 同款 sem=3
	importSem := make(chan struct{}, 3)
	// postImport 后置动作（签到/注册激活/领 trial/查余额）丢到后台并限并发（bug #23 / bug 18）：
	// 每号 2 次上游调用、几十个号串行就是几十秒到几分钟，全部跑完才返回 → 浏览器/反代先超时，
	// 前端报失败但账号其实已导入一半，用户重试又重复导入。抢槽必须在 goroutine 内部，
	// 否则后台化被同步信号量抵消。
	postImport := func(ia importAcct) {
		go func(a *auth.Auth, realm string) {
			importSem <- struct{}{}
			defer func() { <-importSem }()
			defer func() {
				if r := recover(); r != nil {
					log.Printf("ERR: [panel] postImportActivate panic uid=%s: %v", a.UID, r)
				}
			}()
			p.postImportActivate(a, realm)
		}(ia.a, ia.realm)
	}

	// commit 落盘并入池（调用方持 vmu）。落盘在前、入池在后：入池后选号就会用新凭据。
	// verified 表示新凭据刚被 refresh 求证过：上游已把文件里的 refresh token 轮换，池里原有
	// 那份多半随之作废（旧导出与池里是同一条登录链时），此时落盘失败也必须把新凭据放进内存，
	// 否则这个号就真的只能重新登录了。
	commit := func(ia importAcct, verified bool) {
		if err := ia.a.SaveAtomic(); err != nil {
			if !verified {
				skipped++
				errs = append(errs, fmt.Sprintf("uid=%s: save auth failed: %v", ia.uid, err))
				return
			}
			log.Printf("WARN: panel: import uid=%s 新凭据已求证但落盘失败，仅放入内存: %v", ia.uid, err)
			warnings = append(warnings, fmt.Sprintf("uid=%s 凭据落盘失败（%v），新令牌只在内存里生效，请检查凭证目录后重新导入一次", ia.uid, err))
		}
		p.reloadMu.Lock()
		p.cfg.Pool.Add(ia.a)
		p.reloadMu.Unlock()
		p.cfg.Pool.Revive(ia.uid)
		imported++
		postImport(ia)
	}
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

		// realm：本仓 auths/*.json 里带 auth.realm 的按它走（国际版号否则会被判回 cn）；
		// 没带就按 domain 推断 —— workbuddy.ai 家族 → global，否则 cn。
		realm := auth.ResolveRealm(acc.Realm, acc.Domain)

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
		// 企业标识原样带过去（两种命名取先非空者）：没有它，后续签到/成长任务
		// 判定会把企业号当个人号。
		// 归一：占位值（"0"/"null"/"personal"…）当没有企业身份，否则个人号会被判企业版。
		if ent := auth.NormalizeEnterpriseID(acc.EnterpriseID); ent != "" {
			a.EnterpriseID = ent
		} else if ent := auth.NormalizeEnterpriseID(acc.EnterpriseIDCamel); ent != "" {
			a.EnterpriseID = ent
		}
		a.AccountType = strings.TrimSpace(acc.Type)
		// 设备风控 token 同样带过去：本仓 auths/*.json 里存着它，丢了下游会按「新设备」
		// 重新算指纹（等于自己把风控线索断了）。
		if dt := strings.TrimSpace(acc.DeviceToken); dt != "" {
			a.DeviceToken = dt
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

		// 覆盖已有凭据是**不可逆**动作：先用 refresh 求证这份新凭据还活着，只接受「明确活着」。
		// 起因（上游 e36ad07）：把一份旧导出重新导入时，文件里的 RT 早已被轮换，直接覆盖
		// 会让本来能用的账号变成要重新登录，而 .bak 不会被自动回退。三态判据复用
		// upstream.VerifyVerdict：alive 才覆盖；dead（上游明确拒绝）与 inconclusive
		// （网络/5xx/429 没判词）都保留原凭据 —— 主动销毁时没有判词就不能动手。
		// 顺带：alive 分支里 a 已被 refresh 轮换过 RT，下面落盘存的就是新值。
		ia := importAcct{a: a, uid: uid, realm: realm}
		if existing := p.cfg.Pool.AuthByUID(uid); existing != nil && p.cfg.Upstream != nil {
			// 覆盖要先求证，统一在循环后有界并发地做（见 verifyImportOverwrites）：
			// 串行时几百个已有号每个都要等一次最长 30s 的 refresh。
			toVerify = append(toVerify, ia)
			continue
		}
		vmu.Lock()
		commit(ia, false)
		vmu.Unlock()
	}

	// 覆盖前求证：有界并发 + 总时限。每个号求证完**立刻**落盘入池，不等整批——求证已把
	// 文件里的 refresh token 轮换了，拖着不提交会让新令牌只活在内存里。
	if len(toVerify) > 0 {
		ctx, cancel := context.WithTimeout(r.Context(), importVerifyBudget)
		p.verifyImportOverwrites(ctx, toVerify, func(k int, verdict upstream.VerifyVerdict, ran bool) {
			ia := toVerify[k]
			vmu.Lock()
			defer vmu.Unlock()
			switch {
			case ran && verdict == upstream.VerifyAlive:
				commit(ia, true)
			case !ran:
				skipped++
				errs = append(errs, fmt.Sprintf("uid=%s: 批量求证超时（开工总限 %s）或请求已断开，保留原有凭据，请分批重试", ia.uid, importVerifyBudget))
			default:
				skipped++
				errs = append(errs, fmt.Sprintf("uid=%s: 新凭据校验未通过（%s），保留原有凭据", ia.uid, verdict))
			}
		})
		cancel()
	}

	total = len(accounts)
	log.Printf("panel: cockpit import finished total=%d imported=%d skipped=%d", total, imported, skipped)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"total":    total,
		"imported": imported,
		"skipped":  skipped,
		"errors":   errs,
		"warnings": warnings, // 已入池但凭据没能落盘的号（新令牌只在内存里）
	})
}

// parseImportPayload 解析导入文件，支持三种形状（用户手上有什么就能导什么）：
//
//  1. cockpit tools 导出：[{id,uid,access_token,refresh_token,expires_at,domain,…}, …]（snake_case）
//  2. 本仓 auths/*.json 单个文件：{"auth":{accessToken,refreshToken,expiresAt,domain,realm},
//     "account":{uid,enterpriseId,nickname},"device_token":…}（camelCase，嵌套形）
//  3. 上面两种的数组，或 {"accounts":[…] } 包装
//
// 为什么加 2：换包名后旧包的私有目录搬不过来（/data/data 非 root 读不到），用户能拿到
// 的就是 auths/*.json 这些文件本身；只认 cockpit 形状等于逼人手工改键名。
// importEnvelopeKeys 导出工具可能使用的信封键（值可能是数组或单个对象）。
var importEnvelopeKeys = []string{"accounts", "data", "list", "items", "results", "auths"}

// isImportEnvelopeKey 报告键名是否是信封名（不是 uid）。大小写不敏感：
// 手写/不同工具导出的键名大小写并不统一。
func isImportEnvelopeKey(k string) bool {
	for _, e := range importEnvelopeKeys {
		if strings.EqualFold(strings.TrimSpace(k), e) {
			return true
		}
	}
	return false
}

func parseImportPayload(raw []byte) ([]cockpitAccount, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("empty file")
	}
	var items []json.RawMessage
	switch trimmed[0] {
	case '[':
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, fmt.Errorf("invalid json: %w", err)
		}
	case '{':
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &probe); err != nil {
			return nil, fmt.Errorf("invalid json: %w", err)
		}
		// 信封键（accounts / data / list / items / results / auths）：值是数组时逐条解，
		// 值是**单个账号对象**时按一条解（用户把单账号详情又包了一层）。多种导出工具
		// 各用各的键名，用户不会区分。
		var enveloped bool
		for _, k := range importEnvelopeKeys {
			sub, ok := probe[k]
			if !ok {
				continue
			}
			if err := json.Unmarshal(sub, &items); err == nil {
				enveloped = true
				break
			}
			if acc, derr := decodeImportItem(sub); derr == nil {
				return []cockpitAccount{acc}, nil
			}
		}
		if enveloped {
			break
		}
		// 嵌套单账号（cockpit 详情导出 {account,auth} / 本仓 auths/*.json）
		if _, ok := probe["account"]; ok {
			items = []json.RawMessage{trimmed}
			break
		}
		if _, ok := probe["auth"]; ok {
			items = []json.RawMessage{trimmed}
			break
		}
		if _, ok := probe["accessToken"]; ok {
			items = []json.RawMessage{trimmed}
			break
		}
		if _, ok := probe["access_token"]; ok {
			items = []json.RawMessage{trimmed}
			break
		}
		// 字典形态 {"<uid>": {...}}：键当 uid 兜底。
		//
		// 信封名**不能**当 uid：`{"accounts":{"refresh_token":"r"}}` 是「信封的值是
		// 单个对象」形态，走到这里时 accounts 这个键会命中内层，兜底就会把 uid 写成
		// 字面量 "accounts" —— 池里从此多一个格式合法、内容垃圾的号。真实 uid 是
		// 手机号/UUID，信封名本身不是 uid，直接跳过。
		keys := make([]string, 0, len(probe))
		for k := range probe {
			if isImportEnvelopeKey(k) {
				continue
			}
			keys = append(keys, k)
		}
		sort.Strings(keys) // 稳定顺序，便于前端展示与测试比对
		out := make([]cockpitAccount, 0, len(keys))
		for _, k := range keys {
			acc, derr := decodeImportItem(probe[k])
			if derr != nil {
				continue
			}
			if strings.TrimSpace(acc.UID) == "" {
				acc.UID = k
			}
			out = append(out, acc)
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("无法识别的 JSON 结构：既不是账号数组，也不是 {account,auth} 或 {\"<uid>\":{…}}")
		}
		return out, nil
	default:
		return nil, fmt.Errorf("不是 JSON 对象或数组")
	}
	out := make([]cockpitAccount, 0, len(items))
	for i, it := range items {
		acc, err := decodeImportItem(it)
		if err != nil {
			return nil, fmt.Errorf("第 %d 条: %w", i+1, err)
		}
		out = append(out, acc)
	}
	return out, nil
}

// decodeImportItem 单条：靠键名区分 cockpit（snake_case）与本仓 auths（camelCase）。
func decodeImportItem(raw json.RawMessage) (cockpitAccount, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return cockpitAccount{}, fmt.Errorf("不是对象: %w", err)
	}
	if _, ok := probe["auth"]; ok {
		return decodeNativeAuth(raw)
	}
	if _, ok := probe["accessToken"]; ok {
		return decodeNativeAuth(raw)
	}

	var c cockpitAccount
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, err
	}
	return c, nil
}

// decodeNativeAuth 本仓 auths/*.json：嵌套形与扁平形都认（与 auth.Parse 同口径）。
func decodeNativeAuth(raw json.RawMessage) (cockpitAccount, error) {
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
			Type         string `json:"type"`
			Nickname     string `json:"nickname"`
		} `json:"account"`
		// 扁平形（手写/旧版）
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    int64  `json:"expiresAt"`
		Domain       string `json:"domain"`
		Realm        string `json:"realm"`
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Type         string `json:"type"`
		Nickname     string `json:"nickname"`
		DeviceToken  string `json:"device_token"`
	}
	if err := json.Unmarshal(raw, &n); err != nil {
		return cockpitAccount{}, err
	}
	c := cockpitAccount{
		UID:          firstNonEmpty(n.Account.UID, n.UID),
		Nickname:     firstNonEmpty(n.Account.Nickname, n.Nickname),
		EnterpriseID: firstNonEmpty(n.Account.EnterpriseID, n.EnterpriseID),
		Type:         firstNonEmpty(n.Account.Type, n.Type),
		AccessToken:  firstNonEmpty(n.Auth.AccessToken, n.AccessToken),
		RefreshToken: firstNonEmpty(n.Auth.RefreshToken, n.RefreshToken),
		ExpiresAt:    n.Auth.ExpiresAt,
		Domain:       firstNonEmpty(n.Auth.Domain, n.Domain),
		Realm:        firstNonEmpty(n.Auth.Realm, n.Realm),
		DeviceToken:  n.DeviceToken,
	}
	if c.ExpiresAt == 0 {
		c.ExpiresAt = n.ExpiresAt
	}
	return c, nil
}

// firstNonEmpty 取第一个非空（trim 后）值。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
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

// importAcct 一个通过本地校验、待落盘的导入账号。
type importAcct struct {
	a     *auth.Auth
	uid   string
	realm string
}

// importVerifyConcurrency 覆盖前求证（每号一次 refresh）的并发上限：串行时几百个已有号要等几十分钟，
// 请求先被反向代理 / 浏览器掐断；并发太高又像批量刷 refresh 端点。
// importVerifyBudget 是求证「开工」的总时限：到点后不再为没轮到的号发 refresh，它们一律保留原凭据、
// 由人分批重试。已发出的 refresh 不打断（打断会丢掉已轮换的令牌），所以请求最坏要等
// importVerifyBudget + 一次 refresh 的超时。两者是变量只为让测试不必真等 60 秒。
var (
	importVerifyConcurrency = 8
	importVerifyBudget      = 60 * time.Second
)

// verifyImportOverwrites 用 refresh 求证导入的新凭据还活着，有界并发，开工受 ctx 约束。每号求证完
// 立刻调 done（并发调用，调用方自己加锁）；没轮到的号也调一次（ran=false），保证每个号恰好一次回调。
func (p *Panel) verifyImportOverwrites(ctx context.Context, list []importAcct, done func(k int, verdict upstream.VerifyVerdict, ran bool)) {
	sem := make(chan struct{}, importVerifyConcurrency)
	var wg sync.WaitGroup
	k := 0
loop:
	for ; k < len(list); k++ {
		// select 在两路都就绪时随机选一路，先判一次 ctx，已结束就一个也不再发。
		if ctx.Err() != nil {
			break
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break loop
		}
		wg.Add(1)
		go func(k int) {
			defer func() { <-sem; wg.Done() }()
			done(k, p.cfg.Upstream.VerifySessionByRefresh(list[k].a), true)
		}(k)
	}
	for ; k < len(list); k++ {
		done(k, upstream.VerifyInconclusive, false)
	}
	wg.Wait()
}
