// accounttype.go 人工定性「是否企业版」（覆盖上游字段的自动判定）。
//
// 为什么需要这个入口：自动判据看 account.type 与 enterpriseId，而国服个人号常带一个
// 默认租户的 enterpriseId（曾经把整批个人号判成企业版：签到 400「企业账号不支持该
// 操作」、成长任务 403，用户在面板上看到的是「所有号都成了企业版」）。上游字段不可信
// 时只能由人定性，所以给一个显式开关，并把它落进 auth 文件。
//
// 三种取值：
//   ""          自动判定（看 type / enterpriseId，缺省行为）
//   "enterprise" 强制企业版（不签到、不跑成长任务、额度走企业口径）
//   "personal"   强制个人版（即使带了 enterpriseId 也按个人号走）
package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// accountEnterpriseOverride POST /panel/api/accounts/{uid}/enterprise
//
// 请求体：{"override": "enterprise" | "personal" | ""}（省略/空 = 恢复自动判定）。
// 返回：{ok, uid, override, enterprise, changed}。
func (p *Panel) accountEnterpriseOverride(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	var body struct {
		Override string `json:"override"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	want := strings.TrimSpace(body.Override)
	// 归一前后不一致 = 用户写了个不认识的值（"企业版账号" 之类）：明确 400，
	// 别静默当成「自动判定」——那会让人以为覆盖生效了。
	if want != "" && auth.NormalizeEnterpriseOverride(want) == "" {
		writeErr(w, http.StatusBadRequest, "override 只接受 enterprise / personal / 空（自动判定）")
		return
	}
	before := a.IsEnterprise()

	// 与目录热重载串行化：写文件 + 入池要和重载的「扫描 → upsert」互斥，否则刚写的
	// 文件可能被同一轮扫描的旧快照覆盖回去。
	p.reloadMu.Lock()
	err := a.SetEnterpriseOverride(want)
	if err == nil {
		p.cfg.Pool.Add(a) // upsert：同一指针，保留 credits/cooling 状态
	}
	p.reloadMu.Unlock()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "save auth: "+err.Error())
		return
	}

	after := a.IsEnterprise()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"uid":        uid,
		"override":   auth.NormalizeEnterpriseOverride(want),
		"enterprise": after,
		"changed":    before != after,
	})
}
