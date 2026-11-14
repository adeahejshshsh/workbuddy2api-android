// campaign.go 运营活动面板接口：批量读活动进度、批量领已达标的奖励。
//
// 为什么只做「读 + 领」不做「做任务」：活动任务（AI 对话 N 次 / 召唤专家 /
// App 端体验）的判据在服务端，只认客户端真实会话；2026-11-12 实测六条服务端侧
// 伪造路径全部不计数（见 docs/campaign-probe.md）。能做的是把已达标未领的奖励
// 批量领回来——那一步目前得逐个账号进小程序点「领取」。
package panel

import (
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// campaignDefaultModule / campaignDefaultCode 当前在跑的活动（星巴克 11/12–11/16）。
// 调用方可用 ?module= / ?code= 覆盖，免得每有活动就改代码。
//
// module 留空是这期的真实形状（`/portal/activity/tasks`）；早期咖啡节那期要
// 传 module=coffee（`/portal/activity/coffee/tasks`）。
const (
	campaignDefaultModule = ""
	campaignDefaultCode   = "campaign_starbucks_2026"
)

// campaignAccount 单账号的活动状态行。
type campaignAccount struct {
	UID      string                     `json:"uid"`
	Nickname string                     `json:"nickname"`
	Realm    string                     `json:"realm"`
	InPeriod bool                       `json:"in_period"`
	Tasks    []upstream.CampaignTask    `json:"tasks,omitempty"`
	Invite   *upstream.CampaignInvite   `json:"invite,omitempty"`
	Vouchers []upstream.CampaignVoucher `json:"vouchers,omitempty"`
	// Ready  已达标但未领的任务 code
	Ready []string `json:"ready,omitempty"`
	Err   string   `json:"error,omitempty"`
}

func campaignQuery(r *http.Request) (string, string) {
	module := strings.TrimSpace(r.URL.Query().Get("module"))
	if module == "" {
		module = campaignDefaultModule
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" {
		code = campaignDefaultCode
	}
	return module, code
}

// campaignStatus GET /panel/api/campaign?module=&code=
//
// 逐 CN 账号读活动任务表（3 并发，与券码/积分构成同款限流）。global 账号没有这套
// 活动，直接标 skipped，不发上游调用——省一次必然失败的 4xx。
func (p *Panel) campaignStatus(w http.ResponseWriter, r *http.Request) {
	module, code := campaignQuery(r)
	accts := p.cfg.Pool.List()
	out := make([]campaignAccount, len(accts))
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, st := range accts {
		if st.Disabled {
			continue
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		wg.Add(1)
		go func(i int, a *auth.Auth) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			it := campaignAccount{UID: a.UID, Nickname: a.Nickname, Realm: a.Realm()}
			if a.IsGlobal() || a.IsAPIKey() || a.IsEnterprise() {
				it.Err = "该领域/账号类型不参与本期活动"
				out[i] = it
				return
			}
			snap, err := p.cfg.Upstream.CampaignTasks(a, module, code)
			if err != nil {
				it.Err = err.Error()
				out[i] = it
				return
			}
			it.InPeriod = snap.InPeriod
			it.Tasks = snap.Tasks
			it.Vouchers = snap.Vouchers
			inv := snap.Invite
			it.Invite = &inv
			for _, t := range snap.Tasks {
				if upstream.CampaignTaskReady(t) {
					it.Ready = append(it.Ready, t.Code)
				}
			}
			sort.Strings(it.Ready)
			out[i] = it
		}(i, a)
	}
	wg.Wait()
	res := make([]campaignAccount, 0, len(out))
	ready := 0
	for _, it := range out {
		if it.UID == "" {
			continue
		}
		if len(it.Ready) > 0 {
			ready++
		}
		res = append(res, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"module":         module,
		"code":           code,
		"accounts":       res,
		"accounts_ready": ready,
	})
}

// campaignClaim POST /panel/api/campaign/claim
//
// 请求体：{"module":"","code":"","uids":[],"dry_run":true}
//
// dryRun 默认 **true**：先看会给哪些账号领哪些任务，确认后再用 dry_run=false 真领。
// 逐账号串行（活动接口有风控节奏要求），每个任务先 viewed 再 claim。
func (p *Panel) campaignClaim(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Module string   `json:"module"`
		Code   string   `json:"code"`
		UIDs   []string `json:"uids"`
		DryRun *bool    `json:"dry_run"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	module := strings.TrimSpace(req.Module)
	if module == "" {
		module = campaignDefaultModule
	}
	code := strings.TrimSpace(req.Code)
	if code == "" {
		code = campaignDefaultCode
	}
	dry := true
	if req.DryRun != nil {
		dry = *req.DryRun
	}

	type result struct {
		UID      string   `json:"uid"`
		Nickname string   `json:"nickname"`
		Claimed  []string `json:"claimed,omitempty"`
		// Pending 上游已接受领奖请求、但回读不到 claimed_at（未确认入账）——
		// 不算 Claimed，下一轮会再试。
		Pending []string `json:"pending,omitempty"`
		Would    []string `json:"would_claim,omitempty"`
		Skipped  []string `json:"skipped,omitempty"`
		Errors   []string `json:"errors,omitempty"`
	}

	want := map[string]bool{}
	for _, u := range req.UIDs {
		if u = strings.TrimSpace(u); u != "" {
			want[u] = true
		}
	}

	var rows []result
	for _, st := range p.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		if len(want) > 0 && !want[st.UID] {
			continue
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.IsGlobal() || a.IsAPIKey() || a.IsEnterprise() {
			continue
		}
		snap, err := p.cfg.Upstream.CampaignTasks(a, module, code)
		if err != nil {
			rows = append(rows, result{UID: a.UID, Nickname: a.Nickname, Errors: []string{err.Error()}})
			continue
		}
		if !snap.InPeriod {
			rows = append(rows, result{UID: a.UID, Nickname: a.Nickname, Skipped: []string{"活动不在进行中"}})
			continue
		}
		row := result{UID: a.UID, Nickname: a.Nickname}
		for _, t := range snap.Tasks {
			if !upstream.CampaignTaskReady(t) {
				continue
			}
			if dry {
				row.Would = append(row.Would, t.Code)
				continue
			}
			if err := p.cfg.Upstream.CampaignViewTask(a, module, code, t.Code); err != nil {
				row.Errors = append(row.Errors, t.Code+" viewed: "+err.Error())
				continue
			}
			claimOK, cerr := p.cfg.Upstream.CampaignClaimVerified(a, module, code, t.Code)
			if cerr != nil {
				if upstream.IsCampaignNotMet(cerr) {
					row.Skipped = append(row.Skipped, t.Code+"（未达标）")
					continue
				}
				row.Errors = append(row.Errors, t.Code+" claim: "+cerr.Error())
				continue
			}
			if !claimOK {
				// 请求被接受但没回读到 claimed_at：不报成已领，下一轮再试。
				row.Pending = append(row.Pending, t.Code)
				log.Printf("campaign claim: uid=%s task=%s 已提交但未回读 claimed_at → 下轮重试",
					a.UID[:min8(len(a.UID))], t.Code)
				continue
			}
			row.Claimed = append(row.Claimed, t.Code)
			log.Printf("campaign claim: uid=%s task=%s 已领取", a.UID[:min8(len(a.UID))], t.Code)
		}
		rows = append(rows, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"module":  module,
		"code":    code,
		"dry_run": dry,
		"rows":    rows,
	})
}

func min8(n int) int {
	if n < 8 {
		return n
	}
	return 8
}
