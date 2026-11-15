// 模型规则（黑/白名单）的面板接口。
//
// 需求：昂贵模型只给少数账号用，其余账号只用便宜常用的——一个账号把额度打穿，
// 整个池子连便宜模型都跟着不可用。逐条点 model_toggle 太慢（16 个账号 × 几十个
// 模型），所以做成「规则」：命名的一组模型 + deny/allow 模式，挂到账号上即刻生效，
// 改规则内容所有挂了它的账号一起变。语义与存储见 internal/pool/modelrules.go。
package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// modelRuleMaxBatch 单次批量操作的 uid / 模型条数上限（W2-MD-04）。
// 请求体已由 limitJSONBody 收到 1MB，这里再挡一层「数组长度」——1MB 的
// 短字符串数组解码后能放大成几十万条，落盘时整份规则写进 config.json。
const modelRuleMaxBatch = 512

// modelRulesEntry 规则 + 引用计数（面板左栏列表用）。
type modelRulesEntry struct {
	pool.ModelRule
	Accounts int `json:"accounts"`
}

// modelRules GET /panel/api/model_rules
//
// 返回规则表 + 「账号 → 规则 id」映射：前端右栏要按账号显示已挂的规则，
// 分两次请求会在两次轮询之间出现短暂不一致，一次拿全更省事。
func (p *Panel) modelRules(w http.ResponseWriter, r *http.Request) {
	counts := p.cfg.Pool.RuleAccountCounts()
	rules := p.cfg.Pool.Rules()
	out := make([]modelRulesEntry, 0, len(rules))
	for _, rr := range rules {
		out = append(out, modelRulesEntry{ModelRule: rr, Accounts: counts[rr.ID]})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"rules":       out,
		"assignments": p.cfg.Pool.RuleAssignments(),
	})
}

// modelRuleSave POST /panel/api/model_rules
//
// 新建（不带 id）或整条覆盖（带 id）。models 可带 cn:/global: 前缀（区分域），
// 也可以只写裸名（不分域）；匹配细节见 pool.blockedByRules。
func (p *Panel) modelRuleSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     string   `json:"id"`
		Name   string   `json:"name"`
		Mode   string   `json:"mode"`
		Models []string `json:"models"`
		Note   string   `json:"note"`
	}
	// 请求体收紧 + 数组上限（W2-MD-04）。
	limitJSONBody(w, r)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	if len(req.Models) > modelRuleMaxBatch {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("models 最多 %d 条", modelRuleMaxBatch))
		return
	}
	saved, err := p.cfg.Pool.UpsertRule(pool.ModelRule{
		ID: strings.TrimSpace(req.ID), Name: req.Name, Mode: req.Mode,
		Models: req.Models, Note: req.Note,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "rule": saved})
}

// modelRuleApply POST /panel/api/model_rules/apply
//
// 把一条规则批量挂到一批账号上（assigned=true）或摘掉（assigned=false）。
// 规则卡片上的「应用」用它：账号多的池子里不用逐个账号点「配置」。
// 只动这条规则的引用，账号原有的其它规则不变。
//
// 增量形态（W4F1-077）：{"id":"r1","add":["u1"],"del":["u2"]} —— 一次请求同时
// 挂上 add、摘掉 del。前端此前是「先发 assigned:true 再发 assigned:false」两条
// 独立请求，第一条成功、第二条失败时挂载已部分生效而界面报「保存失败」并 return
// （不关弹窗、不刷新），用户再点一次会把已生效的部分重发。改成单请求后，
// 失败只会是「一条都没动」（规则不存在/参数非法在动之前就返回）。
func (p *Panel) modelRuleApply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID       string   `json:"id"`
		UIDs     []string `json:"uids"`
		Assigned bool     `json:"assigned"`
		// 增量形态（W4F1-077）。
		Add []string `json:"add"`
		Del []string `json:"del"`
	}
	limitJSONBody(w, r)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	id := strings.TrimSpace(req.ID)
	if id == "" {
		writeErr(w, http.StatusBadRequest, "缺少规则 id")
		return
	}
	if len(req.Add)+len(req.Del) > 0 {
		if len(req.Add) > modelRuleMaxBatch || len(req.Del) > modelRuleMaxBatch {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("add / del 各最多 %d 条", modelRuleMaxBatch))
			return
		}
		added, removed, err := p.applyRuleDelta(id, req.Add, req.Del)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "id": id, "added": added, "removed": removed, "changed": added + removed,
		})
		return
	}
	if len(req.UIDs) > modelRuleMaxBatch {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("uids 最多 %d 条", modelRuleMaxBatch))
		return
	}
	n, err := p.cfg.Pool.ApplyRule(id, req.UIDs, req.Assigned)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "changed": n})
}

// applyRuleDelta 在一次请求内同时挂上 add、摘掉 del（W4F1-077）。
//
// 语义：重叠的 uid 以 del 为准（先 add 再 del，摘除是更保守的结果）。
// 原子性：规则存在性在**任何写入之前**校验，因此失败时一条都没动；两次
// ApplyRule 由 modelRuleMu 串起来，避免并发请求交错看到「只挂了一半」的中间态。
func (p *Panel) applyRuleDelta(id string, add, del []string) (added, removed int, err error) {
	p.modelRuleMu.Lock()
	defer p.modelRuleMu.Unlock()
	known := false
	for _, rr := range p.cfg.Pool.Rules() {
		if rr.ID == id {
			known = true
			break
		}
	}
	if !known {
		return 0, 0, fmt.Errorf("规则不存在: %s", id)
	}
	if len(add) > 0 {
		n, e := p.cfg.Pool.ApplyRule(id, add, true)
		if e != nil {
			return 0, 0, e
		}
		added = n
	}
	if len(del) > 0 {
		n, e := p.cfg.Pool.ApplyRule(id, del, false)
		if e != nil {
			return added, 0, e
		}
		removed = n
	}
	return added, removed, nil
}

// modelRuleDelete POST /panel/api/model_rules/delete
//
// 删规则并从所有账号上摘掉引用（账号本身的其它规则不受影响）。
func (p *Panel) modelRuleDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	limitJSONBody(w, r)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	id := strings.TrimSpace(req.ID)
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id is required")
		return
	}
	if !p.cfg.Pool.DeleteRule(id) {
		writeErr(w, http.StatusNotFound, "规则不存在")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// accountModelRules POST /panel/api/accounts/{uid}/model_rules
//
// 整体替换某账号挂的规则集合（前端「应用/取消应用」一次提交全部 id）。
func (p *Panel) accountModelRules(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	var req struct {
		IDs []string `json:"ids"`
	}
	limitJSONBody(w, r)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	if len(req.IDs) > modelRuleMaxBatch {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("ids 最多 %d 条", modelRuleMaxBatch))
		return
	}
	ids, err := p.cfg.Pool.AssignRules(uid, req.IDs)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "ids": ids})
}
