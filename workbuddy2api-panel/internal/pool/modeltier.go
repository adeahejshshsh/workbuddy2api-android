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
	"log"
	"os"
	"path/filepath"
	"time"
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
		return // 损坏文件：当空处理，不阻断（与 taskgate 同口径）
	}
	if t.Deny == nil {
		t.Deny = map[string]map[string]bool{}
	}
	if t.UIDType == nil {
		t.UIDType = map[string]string{}
	}
	p.tier = &t
}

// saveTierLocked 原子落盘（tmp + rename）；失败只记日志，不影响主流程。
func (p *Pool) saveTierLocked() {
	if p.tierFile == "" || p.tier == nil {
		return
	}
	p.tier.Updated = time.Now()
	raw, err := json.Marshal(p.tier)
	if err != nil {
		return
	}
	tmp := p.tierFile + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("WARN: [pool] model_tier 落盘失败: %v", err)
		return
	}
	if err := os.Rename(tmp, p.tierFile); err != nil {
		log.Printf("WARN: [pool] model_tier 改名失败: %v", err)
	}
}

// NoteModelTierDenied 记录一条等级事实：账号 uid（等级 userType）明确不能用 model。
func (p *Pool) NoteModelTierDenied(uid, userType, model string) {
	if model == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.loadTierLocked()
	if userType != "" {
		if p.tier.Deny[userType] == nil {
			p.tier.Deny[userType] = map[string]bool{}
		}
		p.tier.Deny[userType][model] = true
		if uid != "" {
			p.tier.UIDType[uid] = userType
		}
	}
	p.saveTierLocked()
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
	for uid := range p.byUID {
		if !p.tier.deniedFor(uid, model) {
			return false
		}
	}
	return true
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
		if realm != "" && e.a.Realm() != realm {
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
