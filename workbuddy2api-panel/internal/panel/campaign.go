// campaign.go 运营活动面板接口：批量读活动进度、批量领已达标的奖励。
//
// 为什么只做「读 + 领」不做「做任务」：活动任务（AI 对话 N 次 / 召唤专家 /
// App 端体验）的判据在服务端，只认客户端真实会话；2026-11-12 实测六条服务端侧
// 伪造路径全部不计数（见 docs/campaign-probe.md）。能做的是把已达标未领的奖励
// 批量领回来——那一步目前得逐个账号进小程序点「领取」。
package panel

import (
	"context"
	"encoding/json"
	"log"
	"math/rand/v2"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
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

// campaignIdentRe module 与 activity_code 的合法形状。module 直接拼进上游 URL 的路径，
// 不校验就能用 ../ 或 ? 把请求带到同一主机的别的接口上（带着账号的 Bearer）。
var campaignIdentRe = regexp.MustCompile(`^[A-Za-z0-9_-]{0,64}$`)

// campaignClaimGap 真领时账号之间的间隔下限（实际在 [gap, 2×gap) 里随机）：活动接口有
// 风控节奏，一口气把几十个号连着领是最好认的形状。变量：测试压到零。
var campaignClaimGap = 3 * time.Second

// campaignIdent 取 module / code（空则用当前活动的缺省值）并校验形状。
func campaignIdent(module, code string) (string, string, bool) {
	module, code = strings.TrimSpace(module), strings.TrimSpace(code)
	if module == "" {
		module = campaignDefaultModule
	}
	if code == "" {
		code = campaignDefaultCode
	}
	return module, code, campaignIdentRe.MatchString(module) && campaignIdentRe.MatchString(code)
}

// campaignEligible 参与 CN 运营活动的账号：国际版、API Key、企业号都没有这套活动。
func campaignEligible(a *auth.Auth) bool {
	return a != nil && !a.IsGlobal() && !a.IsAPIKey() && !a.IsEnterprise()
}

// campaignCacheTTL 活动进度查询结果的缓存时长。变量：测试可改。
var campaignCacheTTL = time.Minute

type campaignCacheEntry struct {
	key  string
	at   time.Time
	body map[string]any
}

func (p *Panel) campaignCached(key string) (map[string]any, bool) {
	p.campaignCacheMu.Lock()
	defer p.campaignCacheMu.Unlock()
	c := p.campaignCache
	if c.key != key || c.body == nil || time.Since(c.at) >= campaignCacheTTL {
		return nil, false
	}
	return c.body, true
}

// sleepCampaignGap 号与号之间的随机间隔；ctx 取消返回 false。
func sleepCampaignGap(ctx context.Context) bool {
	if campaignClaimGap <= 0 {
		return true
	}
	t := time.NewTimer(campaignClaimGap + rand.N(campaignClaimGap))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// campaignStatus GET /panel/api/campaign?module=&code=
//
// 逐 CN 账号读活动任务表（3 并发，与券码/积分构成同款限流）。global 账号没有这套
// 活动，直接标 skipped，不发上游调用——省一次必然失败的 4xx。
func (p *Panel) campaignStatus(w http.ResponseWriter, r *http.Request) {
	module, code, ok := campaignIdent(r.URL.Query().Get("module"), r.URL.Query().Get("code"))
	if !ok {
		writeErr(w, http.StatusBadRequest, "module / code 只允许字母、数字、下划线和连字符")
		return
	}
	// 一次查询对每个国内号打 3 个上游请求：与领奖共用一把锁（同一时刻只有一轮在打活动接口），
	// 结果短时缓存，刷新页面、脚本轮询不会每次都把全池扫一遍。
	if body, ok := p.campaignCached(module + "|" + code); ok {
		writeJSON(w, http.StatusOK, body)
		return
	}
	if !p.campaignMu.TryLock() {
		writeErr(w, http.StatusConflict, "已有一轮活动查询或领奖在执行，请稍后再试")
		return
	}
	defer p.campaignMu.Unlock()
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
			if !campaignEligible(a) {
				it.Err = "该领域/账号类型不参与本期活动"
				out[i] = it
				return
			}
			if r.Context().Err() != nil {
				return // 客户端已断开：剩下的号不再查
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
	body := map[string]any{
		"ok":             true,
		"module":         module,
		"code":           code,
		"accounts":       res,
		"accounts_ready": ready,
	}
	if r.Context().Err() == nil { // 客户端中途走了，结果不完整，不缓存
		p.campaignCacheMu.Lock()
		p.campaignCache = campaignCacheEntry{key: module + "|" + code, at: time.Now(), body: body}
		p.campaignCacheMu.Unlock()
	}
	writeJSON(w, http.StatusOK, body)
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
	module, code, ok := campaignIdent(req.Module, req.Code)
	if !ok {
		writeErr(w, http.StatusBadRequest, "module / code 只允许字母、数字、下划线和连字符")
		return
	}
	dry := req.DryRun == nil || *req.DryRun
	want := map[string]bool{}
	for _, u := range req.UIDs {
		if u = strings.TrimSpace(u); u != "" {
			want[u] = true
		}
	}
	if !dry && len(want) == 0 {
		writeErr(w, http.StatusBadRequest, "真领（dry_run=false）必须列出 uids；先用 dry_run 看要领哪些")
		return
	}
	if !p.campaignMu.TryLock() {
		writeErr(w, http.StatusConflict, "已有一轮活动领奖在执行，请等它结束")
		return
	}
	defer p.campaignMu.Unlock()
	// 真领会改变进度：作废查询缓存。
	if !dry {
		p.campaignCacheMu.Lock()
		p.campaignCache = campaignCacheEntry{}
		p.campaignCacheMu.Unlock()
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

	ctx := r.Context()
	var rows []result
	wrote := false // 本轮已对上游发过写调用：下一个号要先等一段间隔
	for _, st := range p.cfg.Pool.List() {
		if st.Disabled || (len(want) > 0 && !want[st.UID]) {
			continue
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if !campaignEligible(a) {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		if wrote && !dry && !sleepCampaignGap(ctx) {
			break
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
			wrote = true
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
				log.Printf("campaign claim: %s task=%s 已提交但未回读 claimed_at → 下轮重试",
					logfmt.Label(a.UID, a.Nickname), t.Code)
				continue
			}
			row.Claimed = append(row.Claimed, t.Code)
			log.Printf("campaign claim: %s task=%s 已领取", logfmt.Label(a.UID, a.Nickname), t.Code)
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

