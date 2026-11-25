// modeltier.go 「账号等级 × 模型」否定事实闸门。
//
// 为什么需要它：上游模型目录（/console 与 /v2 两域同构）只给模型能力字段，**不提供
// 任何权限/等级字段**，所以网关此前把平台全量目录直接透出到 /v1/models 与面板模型页
// ——用户的号明明用不了（上游回 11158：model X is not allowed for user type Y），
// 列表里却照样显示，客户端挑中它必然失败（还顶着一条 503 让人以为服务端过载）。
//
// 做法：只在"事实确凿"时隐藏——从 11158 报文里学到 (等级 → 不能用该模型) 与
// (账号 → 它的等级)，然后当**池内每个账号**都能被判定为"用不了"时，才把该模型
// 从列表里摘掉。任何账号判不出来（等级未知且无该号自己的否定记录）→ 保持可见，
// 宁可多显示一个，也不把一个其实能用的模型藏起来。
package pool

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// tierFacts 落盘形态（与 state.json 同目录的 model_tier.json，独立文件避免动
// state schema）。
type tierFacts struct {
	// Deny: 等级 → 模型 → true（该等级明确不能用该模型）。
	Deny map[string]map[string]bool `json:"deny"`
	// UIDType: 账号 → 等级（从该号自己的 11158 报文里学到）。
	UIDType map[string]string `json:"uid_type"`
	Updated time.Time         `json:"updated"`
}

// tierFilePath 由 state 文件路径推导同级 model_tier.json；stateFp 空（纯内存池，
// 测试/一次性进程）→ 空串 = 不落盘。
func tierFilePath(stateFp string) string {
	if stateFp == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(stateFp), "model_tier.json")
}

// loadTierLocked 惰性加载（调用方持 p.mu）。
func (p *Pool) loadTierLocked() {
	if p.tier != nil {
		return
	}
	p.tier = &tierFacts{Deny: map[string]map[string]bool{}, UIDType: map[string]string{}}
	if p.tierFile == "" {
		return
	}
	raw, err := os.ReadFile(p.tierFile)
	if err != nil {
		return
	}
	var t tierFacts
	if json.Unmarshal(raw, &t) != nil {
		// 损坏文件必须留档 + 告警（审查 C5-69）：此前静默当空处理，而紧接着的
		// NoteModelTierDenied 会用新事实**覆盖**该文件 —— 学到的等级/否定事实
		// 无声清零，且没有任何痕迹可比对（与 state.json 的 .corrupt 处理口径不一致）。
		bak := fmt.Sprintf("%s.corrupt-%d", p.tierFile, time.Now().Unix())
		if rerr := os.Rename(p.tierFile, bak); rerr != nil {
			log.Printf("WARN: [pool] model_tier.json 解析失败且另存失败（%v），本次按空事实启动（原文件保留）", rerr)
		} else {
			log.Printf("WARN: [pool] model_tier.json 解析失败，已另存 %s（本次按空事实启动）", bak)
		}
		return
	}
	if t.Deny == nil {
		t.Deny = map[string]map[string]bool{}
	}
	if t.UIDType == nil {
		t.UIDType = map[string]string{}
	}
	p.tier = &t
}

// marshalTierLocked 把等级事实序列化（纯内存，不碰磁盘）。调用方持 p.mu。
//
// 拆出这一步是为了把 fsync 挪出锁（审查 C5-68）：saveTierLocked 走
// auth.ReplaceAtomic（写 + fsync + rename），在慢速（FUSE 等）卷上是十毫秒量级，而
// NoteModelTierDenied 是**持池写锁**执行的 —— 首轮扫模型/账号期间每个新事实都会让
// pick / /status / /healthz 一起排队。现在锁内只 marshal，写盘在锁外做。
func (p *Pool) marshalTierLocked() []byte {
	if p.tierFile == "" || p.tier == nil {
		return nil
	}
	p.tier.Updated = time.Now()
	raw, err := json.Marshal(p.tier)
	if err != nil {
		return nil
	}
	return raw
}

// writeTier 把 marshalTierLocked 的结果落盘（**不加 p.mu**：纯 IO）。
func (p *Pool) writeTier(raw []byte) {
	if p.tierFile == "" || raw == nil {
		return
	}
	// 唯一临时名 + 原子替换（bug D4）：固定 `p.tierFile+".tmp"` 时，同目录两个写入方
	// （双实例、并行测试）会共用同一个临时文件 —— A 写完、B 覆盖、A 再 rename，
	// 落盘的就成了 B 的内容（或者 rename 直接失败）。auth.ReplaceAtomic 已经是
	// 「唯一 tmp 名 + 原子替换 + EBUSY 原地覆写」的标准写法，这里对齐复用它。
	if err := auth.ReplaceAtomic(p.tierFile, raw, 0o600); err != nil {
		log.Printf("WARN: [pool] model_tier 落盘失败: %v", err)
	}
}

// saveTierLocked 原子落盘（tmp + rename）；失败只记日志，不影响主流程。
// 保留给「调用方已持锁且不介意锁内 IO」的少数路径；新代码请走
// marshalTierLocked + writeTier（审查 C5-68）。
func (p *Pool) saveTierLocked() {
	p.writeTier(p.marshalTierLocked())
}

// NoteModelTierDenied 记录一条等级事实：账号 uid（等级 userType）明确不能用 model。
//
// 落盘在锁外（审查 C5-68）：锁内只判断「有没有新事实」+ marshal，fsync 交给
// writeTier。语义不变（返回即已落盘），但 pick / /status 不再被磁盘 IO 阻塞。
func (p *Pool) NoteModelTierDenied(uid, userType, model string) {
	if model == "" {
		return
	}
	p.mu.Lock()
	p.loadTierLocked()
	// **只在事实新增时落盘**：saveTierLocked 走 auth.ReplaceAtomic（写 + fsync +
	// rename），而这里是**持池写锁**执行的 —— 客户端反复请求该等级用不了的模型时，
	// 每个请求都会让 pick / /status / /healthz 一起排队。同一个 (userType, model)
	// 第二次起就没有新信息，跳过写盘即可。
	changed := false
	if userType != "" {
		if p.tier.Deny[userType] == nil {
			p.tier.Deny[userType] = map[string]bool{}
		}
		if !p.tier.Deny[userType][model] {
			// 条数上限（审查 C5-70）：Deny 随模型目录增长且只增不减，长期运行 +
			// 模型改名（带日期后缀）会让 model_tier.json 单调变大。到顶后不再记新模型，
			// 只打一条提示 —— 已学到的事实全部保留，判定方向仍是「保守拦截」。
			if len(p.tier.Deny[userType]) < maxTierDenyModels {
				p.tier.Deny[userType][model] = true
				changed = true
			} else {
				log.Printf("WARN: [pool] model_tier 等级 %s 的否定事实已达上限 %d，忽略新模型 %s",
					userType, maxTierDenyModels, model)
			}
		}
		if uid != "" && p.tier.UIDType[uid] != userType {
			p.tier.UIDType[uid] = userType
			changed = true
		}
	}
	var raw []byte
	if changed {
		raw = p.marshalTierLocked()
	}
	p.mu.Unlock()
	if raw != nil {
		p.writeTier(raw)
	}
}

// maxTierDenyModels 单个等级最多记多少个「不能用」的模型（审查 C5-70）。
// 512 远大于真实目录规模（当前 ~44 个模型），只用于挡住模型名高基数增长。
const maxTierDenyModels = 512

// dropTierUID 从等级事实表里摘掉一个已删除账号（审查 C5-70）。
// 只在 tier 已加载时动手（不为了清理而触发一次文件读取）；调用方**不要**持锁。
//
// 此前 UIDType[uid] 永久残留：账号删了又换一批（轮换导入）时 model_tier.json
// 单调变大，且旧 uid 的等级判定永远留在文件里。
func (p *Pool) dropTierUID(uids ...string) {
	if len(uids) == 0 {
		return
	}
	p.mu.Lock()
	if p.tier == nil || len(p.tier.UIDType) == 0 {
		p.mu.Unlock()
		return
	}
	changed := false
	for _, uid := range uids {
		if _, ok := p.tier.UIDType[uid]; ok {
			delete(p.tier.UIDType, uid)
			changed = true
		}
	}
	var raw []byte
	if changed {
		raw = p.marshalTierLocked()
	}
	p.mu.Unlock()
	if raw != nil {
		p.writeTier(raw)
	}
}

// ModelDeniedForAllAccounts 报告池内**每个账号**都能被判定为"用不了该模型"。
// 池空 → false（无信息不下结论）。
func (p *Pool) ModelDeniedForAllAccounts(model string) bool {
	if model == "" {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.tier == nil || len(p.byUID) == 0 {
		return false
	}
	matched := 0
	for uid, e := range p.byUID {
		// 分母只算「可能被选中」的号（报告 #6）：disabled / paused / needAuth 永远选不到，
		// 把它们算进来会让本该整池隐藏的模型继续出现在 /v1/models（客户端挑中必失败）。
		if e == nil || e.disabled || e.paused || e.needAuth {
			continue
		}
		matched++
		if !p.tier.deniedFor(uid, model) {
			return false
		}
	}
	// 整池都不可选时**不能**下结论（报告 #6）：没有可服务的号 ≠ 每个号都用不了该模型。
	// 此处若不守卫，全池暂停/禁用时 modelList 会把缓存里的模型整段过滤掉。
	return matched > 0
}

// deniedFor 供包内只读判定（不加锁：调用方持锁）。
func (t *tierFacts) deniedFor(uid, model string) bool {
	if t == nil {
		return false
	}
	typeName := t.UIDType[uid]
	if typeName == "" {
		return false
	}
	return t.Deny[typeName][model]
}

// ModelDeniedForAllAccountsInRealm 同 ModelDeniedForAllAccounts，但只看该 realm 的账号。
//
// 为什么必须分域：`global:` 前缀的模型只会派给 global 号（选号有 realm 谓词），所以
// 「全池都判得出来用不了」是过宽的判据 —— 一个 CN 号等级未知就会让 global 模型继续显示，
// 而它实际上永远选不到号（用户实测：列表里留着 global:gpt-6-astra，一发就 503）。
// 该 realm 内没有任何账号 → false（无信息不下结论）。
func (p *Pool) ModelDeniedForAllAccountsInRealm(model, realm string) bool {
	if model == "" {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.tier == nil || len(p.byUID) == 0 {
		return false
	}
	matched := 0
	for uid, e := range p.byUID {
		// 与不分域版本同口径（报告 #4）：disabled / paused / needAuth 永远选不到，
		// 把它们算进分母会让「本域实际无号可服务」的模型继续出现在 /v1/models。
		if e == nil || e.disabled || e.paused || e.needAuth {
			continue
		}
		if realm != "" && e.a != nil && e.a.Realm() != realm {
			continue
		}
		matched++
		if !p.tier.deniedFor(uid, model) {
			return false
		}
	}
	return matched > 0
}

// TierDenyStats 观测用：已学到多少个等级、多少条 (等级,模型) 否定、多少个账号等级。
func (p *Pool) TierDenyStats() (types, pairs, uids int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.tier == nil {
		return 0, 0, 0
	}
	for _, set := range p.tier.Deny {
		types++
		pairs += len(set)
	}
	return types, pairs, len(p.tier.UIDType)
}
