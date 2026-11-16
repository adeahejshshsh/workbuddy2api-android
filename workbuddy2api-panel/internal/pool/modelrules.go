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
	"strconv"
	"strings"
	"sync/atomic"
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
		return fallbackRuleID()
	}
	return hex.EncodeToString(b[:])
}

// fallbackRuleID 随机源不可用时的退化 id（审查 C5-61）。
//
// 退回时间戳，宁可碰撞也不 panic（规则 id 只用于引用）。但纯时间戳在同微秒内连续
// 创建两条规则时会生成同一个 id，后者在 p.rules[r.ID] = r 里**静默覆盖**前者 ——
// 所以再拼一个进程内自增序号。抽成独立函数是为了能被用例直接钉住（rand 失败
// 无法在测试里构造）。
func fallbackRuleID() string {
	return "r" + time.Now().Format("150405.000000") + "-" + strconv.FormatUint(ruleIDSeq.Add(1), 36)
}

// ruleIDSeq 随机源不可用时的进程内自增序号（审查 C5-61），只用于退化路径的唯一性。
var ruleIDSeq atomic.Uint64

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
	} else {
		// 更新既有规则（审查 C5-60）：此前这里还有一个 `else if old, ok := ...` 分支，
		// 两支做的是同一件事（只 TrimSpace 名字），`_ = old` 是遗留占位 —— 删掉避免
		// 后来者以为「更新」与「新建」有语义差别。名字归一化在 normalizeRule 里已做。
		r.Name = strings.TrimSpace(r.Name)
	}
	p.rules[r.ID] = r
	// 规则内容变了 → 所有挂了它的账号重新展开。
	p.recomputeRulesLocked()
	p.dirty.Store(true)
	return r, nil
}

// DeleteRule 删除规则，并从所有账号上摘掉引用。
//
// 摘引用用「新建切片」而不是 `e.ruleIDs[:0]` 原地压缩（审查 C5-58）：原地压缩会把
// 同一底层数组改写，任何在别处持有旧切片视图的代码（快照、面板读取、将来的增量
// 下发）都会看到被"凭空改掉"的内容 —— 目前恰好没人持有，但这是隐藏地雷。
// 规则数量是个位数，重新分配的开销可以忽略。
//
// 暂存号（parked）也要摘（审查 C5-59）：它们不在 byUID 里，但规则 id 会随
// parked_accounts 落盘，文件回来时又带着一个已被删除的 id 复活。
func (p *Pool) DeleteRule(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.rules[id]; !ok {
		return false
	}
	delete(p.rules, id)
	drop := func(e *entry) {
		if len(e.ruleIDs) == 0 {
			return
		}
		kept := make([]string, 0, len(e.ruleIDs))
		for _, rid := range e.ruleIDs {
			if rid != id {
				kept = append(kept, rid)
			}
		}
		if len(kept) == 0 {
			e.ruleIDs = nil
			return
		}
		e.ruleIDs = kept
	}
	for _, e := range p.byUID {
		drop(e)
	}
	for _, e := range p.parked {
		drop(e)
	}
	p.recomputeRulesLocked()
	p.dirty.Store(true)
	return true
}

// entryByUIDLocked 按 uid 找条目：池内优先，其次暂存区（审查 C5-59）。
// 调用方须持 p.mu。
//
// 为什么暂存号也要能操作：热重载把「文件暂时不见了」的号移进 parked（最多 4096 个），
// 这段时间里面板下的批量规则操作如果只查 byUID，就会静默不生效（AssignRules 还会
// 报「账号不存在」）；文件回来后账号带着**旧规则**复活，运维却以为已经改过。
func (p *Pool) entryByUIDLocked(uid string) (*entry, bool) {
	if e, ok := p.byUID[uid]; ok {
		return e, true
	}
	if e, ok := p.parked[uid]; ok {
		return e, true
	}
	return nil, false
}

// AssignRules 设置某账号挂的规则集合（整体替换）。返回落库后的 id 列表。
// 暂存号（parked）同样可操作（审查 C5-59），否则热重载窗口期的配置会静默丢失。
func (p *Pool) AssignRules(uid string, ids []string) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entryByUIDLocked(uid)
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
// 暂存号（parked）也计入（审查 C5-59）：否则热重载窗口期的批量操作对它们静默失效。
func (p *Pool) ApplyRule(id string, uids []string, assigned bool) (int, error) {
	id = strings.TrimSpace(id)
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.rules[id]; !ok {
		return 0, errors.New("规则不存在: " + id)
	}
	changed := 0
	for _, uid := range uids {
		e, ok := p.entryByUIDLocked(uid)
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
// 暂存号也查（审查 C5-59），与 AssignRules/ApplyRule 的操作面保持一致。
func (p *Pool) RuleIDsOf(uid string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.entryByUIDLocked(uid)
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
				// 先按域收一遍：一条 allow 规则若在本账号所属域**一个模型都没命中**
				// （典型是 global 号挂了纯 cn: 的白名单），视作这条规则没挂——否则
				// ruleHasAllow 会把这个号对所有模型封死，而面板上它只是「挂了一条
				// 白名单」，看起来毫无异常。跨域写错规则的意图几乎必然是「这个号
				// 只用这几个模型」，不是「这个号什么都不能用」。
				matched := make(map[string]struct{}, len(r.Models))
				for _, m := range r.Models {
					rm, bare := splitRealmModel(m)
					if rm != "" && realm != "" && rm != realm {
						continue // 别的域的规则与本账号无关（cn:hy4std ≠ global:hy4std）
					}
					matched[bare] = struct{}{}
				}
				if len(matched) == 0 {
					continue
				}
				e.ruleHasAllow = true
				if e.ruleAllow == nil {
					e.ruleAllow = map[string]struct{}{}
				}
				for m := range matched {
					e.ruleAllow[m] = struct{}{}
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

// RulesBlockedForRealm 统计本域内「本来可选、却被模型规则拦住」的账号数，
// 以及本域参与选号的账号总数（禁用 / 暂停 / 占位号不计，与 ModelBlockedForRealm 同口径）。
//
// 用途：选号失败时区分「号都挂着但还是不行」。冷却会自愈、规则不会——两者都落
// no_healthy_account（503）时客户端只会当上游抖动重试，所以调用方拿到
// blocked == eligible 时应当回一个可辨识的 4xx（model_not_allowed）。
//
// 纯只读聚合，用**读锁**：它在每次选号失败（no_healthy_account 503）的出口被调用，
// 拿写锁会和选号互斥，失败越多越慢（与 ModelBlockedForRealm 的 bug 6 同一个理由）。
// 函数体内无写、无回调、不重入 pool 的其它加锁方法。
func (p *Pool) RulesBlockedForRealm(model, realm string) (blocked, eligible int) {
	if model == "" {
		return 0, 0
	}
	// 规则集合里存的是裸名（recomputeRulesLocked 已按账号域过滤并剥前缀），这里
	// 同样剥一遍：面板/排查链路可能带 cn:/global: 前缀传进来。
	_, bare := splitRealmModel(normalizeRuleModel(model))
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, e := range p.byUID {
		// 禁用/暂停号不参与（它们的不可用与模型无关）；needAuth 占位号同理：
		// 它永远选不到，却会拉大分母，让「本域全被规则挡住」判不出来。
		if e.disabled || e.paused || e.needAuth {
			continue
		}
		if realm != "" && e.a != nil && e.a.Realm() != realm {
			continue // 别的域的号不能给"本域可选"作证
		}
		eligible++
		if e.blockedByRules(bare) {
			blocked++
		}
	}
	return blocked, eligible
}
