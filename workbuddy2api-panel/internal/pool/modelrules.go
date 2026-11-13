// modelrules.go 「模型规则」：按规则批量控制「哪些账号能用哪些模型」。
//
// 需求背景：昂贵模型（deepseek-v4-pro、kimi-k3.1-pro 这类）只对少数账号开放，其余
// 账号只用便宜又常用的模型——否则一个账号把自己的额度打穿，整个池子连便宜模型都
// 跟着没得用。
//
// 与 toggle.go 的 modelOff 的关系：modelOff 是「手动关掉某个账号的某个模型」，
// 点对点、逐条；本文件是它的批量版本——规则是**命名的一组模型 + 黑/白模式**，
// 挂到账号上即刻生效，改规则内容时所有挂了它的账号一起变（不用逐个账号重设）。
// 两者在 healthyForModel 里叠加判定，都遵循「人工意图优先于自动机制」。
//
// 语义：
//
//	deny  黑名单：规则里的模型**不可用**；
//	allow 白名单：**只有**规则里的模型可用，列表外的全部不可用。
//
// 一个账号可以挂多条：
//   - 多条 deny 取并集（任何一条命中即禁用）；
//   - 多条 allow 取并集（被任何一条放行即可用）；
//   - allow 与 deny 同时存在时 **deny 优先**（显式禁用比"允许列表"更明确）。
//
// 存储：规则本身与「账号 → 规则 id」都进 state.json（永久语义，重启不丢），
// 运行期把规则展开成账号上的两个 map（ruleDeny / ruleAllow）供 pick 快查。
package pool

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"time"
)

// 规则模式。
const (
	RuleModeDeny  = "deny"
	RuleModeAllow = "allow"
)

// ModelRule 一条模型规则。
type ModelRule struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Mode      string    `json:"mode"`   // deny | allow
	Models    []string  `json:"models"` // 模型名，可带 cn:/global: 前缀（区分域）；匹配时按账号域过滤后取裸名
	Note      string    `json:"note,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

func newRuleID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 随机源不可用时退回时间戳，宁可碰撞也不 panic（规则 id 只用于引用）。
		return "r" + time.Now().Format("150405.000000")
	}
	return hex.EncodeToString(b[:])
}

// normalizeRuleModel 归一化模型名：去空白、小写；**保留 realm 前缀**。
//
// 前缀是区分依据：cn:hy4std 与 global:hy4std 走两套上游、两拨账号，剥掉就分不开了。
// 小写是安全的：上游模型 id 本身全小写，而运维手抄时大小写很随意。
// 旧规则里存的是裸名，匹配时按「裸名 = 两个域都命中」兼容（见 ruleSetHas）。
func normalizeRuleModel(m string) string {
	return strings.ToLower(strings.TrimSpace(m))
}

// splitRealmModel 把规则里的模型名拆成 realm 前缀与裸名：cn:hy4std → ("cn","hy4std")；
// 没有前缀时 realm 为空（早期按裸名存的规则，语义是「不分域」）。
func splitRealmModel(m string) (string, string) {
	if i := strings.Index(m, ":"); i >= 0 {
		return strings.ToLower(m[:i]), m[i+1:]
	}
	return "", m
}

// ruleSetHas 判断一组规则模型名里是否命中 model。
// 集合里存的**一律是裸名**（展开时已按账号 realm 过滤并剥前缀）：运行期选号拿到的
// 是裸模型名（见 handler 的 curModel），带前缀存会永远匹配不上。
func ruleSetHas(set map[string]struct{}, model string) bool {
	if _, ok := set[model]; ok {
		return true
	}
	if i := strings.Index(model, ":"); i >= 0 {
		_, ok := set[model[i+1:]]
		return ok
	}
	return false
}

// normalizeRule 校验并归一化一条规则（写入口统一走这里）。
func normalizeRule(in ModelRule) (ModelRule, error) {
	out := in
	out.Name = strings.TrimSpace(out.Name)
	if out.Name == "" {
		return out, errors.New("规则名不能为空")
	}
	if len([]rune(out.Name)) > 40 {
		return out, errors.New("规则名最长 40 字")
	}
	switch strings.ToLower(strings.TrimSpace(out.Mode)) {
	case RuleModeDeny, "blacklist", "black":
		out.Mode = RuleModeDeny
	case RuleModeAllow, "whitelist", "white":
		out.Mode = RuleModeAllow
	default:
		return out, errors.New("模式只能是 deny（禁用）或 allow（只允许）")
	}
	seen := map[string]struct{}{}
	models := make([]string, 0, len(out.Models))
	for _, m := range out.Models {
		n := normalizeRuleModel(m)
		if n == "" {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		models = append(models, n)
	}
	if len(models) == 0 {
		return out, errors.New("至少要勾选一个模型")
	}
	sort.Strings(models)
	out.Models = models
	out.Note = strings.TrimSpace(out.Note)
	out.UpdatedAt = time.Now()
	return out, nil
}

// Rules 返回全部规则（按名字、id 排序，输出稳定）。
func (p *Pool) Rules() []ModelRule {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]ModelRule, 0, len(p.rules))
	for _, r := range p.rules {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Rule 按 id 取一条规则。
func (p *Pool) Rule(id string) (ModelRule, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	r, ok := p.rules[id]
	return r, ok
}

// UpsertRule 新建（ID 为空）或更新一条规则。返回落库后的规则。
func (p *Pool) UpsertRule(in ModelRule) (ModelRule, error) {
	r, err := normalizeRule(in)
	if err != nil {
		return r, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rules == nil {
		p.rules = map[string]ModelRule{}
	}
	if r.ID == "" {
		r.ID = newRuleID()
	} else if old, ok := p.rules[r.ID]; ok {
		r.Name = strings.TrimSpace(r.Name)
		_ = old
	}
	p.rules[r.ID] = r
	// 规则内容变了 → 所有挂了它的账号重新展开。
	p.recomputeRulesLocked()
	p.dirty.Store(true)
	return r, nil
}

// DeleteRule 删除规则，并从所有账号上摘掉引用。
func (p *Pool) DeleteRule(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.rules[id]; !ok {
		return false
	}
	delete(p.rules, id)
	for _, e := range p.byUID {
		if len(e.ruleIDs) == 0 {
			continue
		}
		kept := e.ruleIDs[:0]
		for _, rid := range e.ruleIDs {
			if rid != id {
				kept = append(kept, rid)
			}
		}
		e.ruleIDs = kept
	}
	p.recomputeRulesLocked()
	p.dirty.Store(true)
	return true
}

// AssignRules 设置某账号挂的规则集合（整体替换）。返回落库后的 id 列表。
func (p *Pool) AssignRules(uid string, ids []string) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return nil, errors.New("账号不存在")
	}
	want := make([]string, 0, len(ids))
	seen := map[string]struct{}{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := p.rules[id]; !ok {
			return nil, errors.New("规则不存在: " + id)
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		want = append(want, id)
	}
	sort.Strings(want)
	e.ruleIDs = want
	p.recomputeRulesLocked()
	p.dirty.Store(true)
	return append([]string(nil), want...), nil
}

// ApplyRule 把一条规则批量挂到 uids（assigned=true）或从这些账号上摘掉（false）。
//
// 只增删**这一条**规则的引用，账号原有的其它规则原样保留——规则卡片上的「应用」
// 走这条路：账号多的池子里，逐个账号「配置」太慢（19 个账号就要点 19 次弹窗）。
// 返回真正发生变化的账号数；规则 id 不存在时报错，避免把野 id 写进账号。
func (p *Pool) ApplyRule(id string, uids []string, assigned bool) (int, error) {
	id = strings.TrimSpace(id)
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.rules[id]; !ok {
		return 0, errors.New("规则不存在: " + id)
	}
	changed := 0
	for _, uid := range uids {
		e, ok := p.byUID[uid]
		if !ok {
			continue
		}
		has := false
		for _, x := range e.ruleIDs {
			if x == id {
				has = true
				break
			}
		}
		if has == assigned {
			continue // 已经是想要的状态，不计入变化
		}
		next := make([]string, 0, len(e.ruleIDs)+1)
		for _, x := range e.ruleIDs {
			if x != id {
				next = append(next, x)
			}
		}
		if assigned {
			next = append(next, id)
		}
		sort.Strings(next)
		e.ruleIDs = next
		changed++
	}
	if changed > 0 {
		p.recomputeRulesLocked()
		p.dirty.Store(true)
	}
	return changed, nil
}

// RuleIDsOf 返回某账号挂的规则 id（复数拷贝，调用方可安全持有）。
func (p *Pool) RuleIDsOf(uid string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return nil
	}
	return append([]string(nil), e.ruleIDs...)
}

// RuleAccountCounts 统计每条规则被多少账号挂着（面板列表展示用）。
func (p *Pool) RuleAccountCounts() map[string]int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := map[string]int{}
	for _, e := range p.byUID {
		for _, id := range e.ruleIDs {
			out[id]++
		}
	}
	return out
}

// RuleAssignments 返回「账号 uid → 挂着的规则 id」全量映射（面板右栏一次性拿全）。
func (p *Pool) RuleAssignments() map[string][]string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make(map[string][]string, len(p.byUID))
	for uid, e := range p.byUID {
		if len(e.ruleIDs) == 0 {
			continue
		}
		out[uid] = append([]string(nil), e.ruleIDs...)
	}
	return out
}

// recomputeRulesLocked 把规则展开到每个账号的 ruleDeny / ruleAllow 快查表。
// 调用方必须已持写锁。删除规则后残留的 id 引用也会在这里被摘掉（防御性）。
func (p *Pool) recomputeRulesLocked() {
	for _, e := range p.byUID {
		e.ruleDeny = nil
		e.ruleAllow = nil
		e.ruleHasAllow = false
		if len(e.ruleIDs) == 0 {
			continue
		}
		realm := e.a.Realm()
		// 只按「已装载的规则」展开集合，**不改写 e.ruleIDs**：装载顺序是先账号
		// （带着 id）后规则本体，若在这里按"规则不存在"剔除引用，账号挂的规则会在
		// 每次重启后被清空（state.json 里明明还有）。真删除规则时由 DeleteRule 摘引用。
		for _, id := range e.ruleIDs {
			r, ok := p.rules[id]
			if !ok {
				continue
			}
			if r.Mode == RuleModeAllow {
				e.ruleHasAllow = true
				if e.ruleAllow == nil {
					e.ruleAllow = map[string]struct{}{}
				}
				for _, m := range r.Models {
					rm, bare := splitRealmModel(m)
					if rm != "" && realm != "" && rm != realm {
						continue // 别的域的规则与本账号无关（cn:hy4std ≠ global:hy4std）
					}
					e.ruleAllow[bare] = struct{}{}
				}
				continue
			}
			if e.ruleDeny == nil {
				e.ruleDeny = map[string]struct{}{}
			}
			for _, m := range r.Models {
				rm, bare := splitRealmModel(m)
				if rm != "" && realm != "" && rm != realm {
					continue
				}
				e.ruleDeny[bare] = struct{}{}
			}
		}
	}
}

// blockedByRules 报告该账号对 model 是否被模型规则拦住。
// 空 model → false（同 modelOffed：没有具体模型就不做模型级判断）。
func (e *entry) blockedByRules(model string) bool {
	if model == "" {
		return false
	}
	// 规则集合里存的是裸名（已按 realm 过滤），这里同样把入参归一：选号链路传的是
	// 裸名（handler 的 curModel = bareModel），面板/排查链路可能带前缀。
	_, model = splitRealmModel(normalizeRuleModel(model))
	if e.ruleHasAllow {
		if !ruleSetHas(e.ruleAllow, model) {
			return true // 白名单之外
		}
	}
	if ruleSetHas(e.ruleDeny, model) {
		return true // 显式禁用优先
	}
	return false
}

// RuleBlocked 供面板/日志排查：某账号是否被规则拦了某模型。
func (p *Pool) RuleBlocked(uid, model string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	return e.blockedByRules(normalizeRuleModel(model))
}
