// campaign.go 运营活动（campaign）接口：任务进度、任务领奖、奖品/券查询。
//
// 与 growth 域的任务体系（tasks.go）**完全两套**：
//   - growth 任务：/v2/activity/growth/tasks，判据是行为事件（chat_request_send 等），
//     我们能用 ReportMPEvent 自己点亮；
//   - campaign 活动：/portal/activity/<module>/tasks，判据在服务端，只认客户端真实
//     会话，服务端侧伪造的任何事件/会话都不计数（2026-11-12 实测六条路径全部无效，
//     详见 docs/campaign-probe.md）。
//
// 所以本文件只做**读进度 + 领奖**：进度由服务端算，我们负责把已达标未领的奖励
// 领回来，省掉逐个账号进小程序点「领取」。
//
// 端点（chatBase，Bearer + X-User-Id，带 activity_code 查询参数）：
//
//	GET  /portal/activity/<module>/config?activity_code=…        活动窗口/in_period
//	GET  /portal/activity/<module>/tasks?activity_code=…         任务表 + 本号进度
//	POST /portal/activity/<module>/tasks/<code>/viewed?…         标记已看（need_view 必做）
//	POST /portal/activity/<module>/tasks/<code>/claim?…          领奖（未达标 409/12304）
//	GET  /portal/activity/<module>/vouchers?activity_code=…      券库存/领取状态
package upstream

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// CampaignTask 活动任务（含本号进度）。
type CampaignTask struct {
	Code     string          `json:"task_code"`
	Title    string          `json:"title"`
	Desc     string          `json:"description,omitempty"`
	Target   int             `json:"target_count"`
	Credit   int             `json:"reward_credit"`
	Chance   int             `json:"reward_chance"`
	NeedView bool            `json:"need_view"`
	Sort     int             `json:"sort"`
	Enabled  int             `json:"enabled"`
	Stages   []CampaignStage `json:"stages,omitempty"`
	// 以下来自 tasks[].progress 合并（服务端按账号算）
	Progress  int    `json:"progress"`
	PeriodKey string `json:"period_key,omitempty"`
	ViewedAt  string `json:"viewed_at,omitempty"`
	Status    string `json:"status,omitempty"` // pending / done / claimed
}

// CampaignStage 分阶段任务（邀请类：clicked → logged_in → joined）。
type CampaignStage struct {
	Code     string `json:"code"`
	Progress int    `json:"progress"`
	Trigger  string `json:"trigger"`
	Qualify  bool   `json:"qualify"`
	Seq      int    `json:"seq"`
}

// CampaignSnapshot 一次活动读取的完整快照。
type CampaignSnapshot struct {
	Code     string            `json:"activity_code"`
	Title    string            `json:"title"`
	StartAt  string            `json:"start_at"`
	EndAt    string            `json:"end_at"`
	InPeriod bool              `json:"in_period"`
	ServerAt string            `json:"server_time,omitempty"`
	Invite   CampaignInvite    `json:"invite"`
	Tasks    []CampaignTask    `json:"tasks"`
	Vouchers []CampaignVoucher `json:"vouchers,omitempty"`
}

// CampaignInvite 邀请类任务的汇总（单独一层，前端要按「3 人」显示）。
type CampaignInvite struct {
	Count int  `json:"count"`
	Need  int  `json:"need"`
	Open  bool `json:"open"`
}

// CampaignVoucher 券的可领状态。
type CampaignVoucher struct {
	Prize       string `json:"prize_code"`
	Title       string `json:"title"`
	Status      string `json:"status"`
	Claimable   bool   `json:"claimable"`
	TodayRemain int    `json:"today_remaining"`
	Unused      int    `json:"unused"`
	StockReset  int64  `json:"stock_reset_at"`
}

// campaignBase 活动接口前缀。
//
// module 为空 → `/portal/activity`（星巴克这期的形状：`campaign_starbucks_2026`
// 直接挂在 /portal/activity 下）；module 非空 → `/portal/activity/<module>`
// （早期咖啡节那期是 `/portal/activity/coffee`）。
//
// 这个差别踩过一次：按「一定有模块段」拼成 `/portal/activity/campaign/tasks`，
// 上游照样回 200 但任务表是空的，看起来像「活动没任务」而不是「路径错了」。
func campaignBase(module string) string {
	if module == "" {
		return "/portal/activity"
	}
	return "/portal/activity/" + module
}

// campaignJSON 发一次活动接口请求并解信封 data。
//
// 活动接口同时挂在 workbuddy.cn 与 codebuddy.cn（实测两边同源响应），走 chatBase。
// 头族用小程序口径：活动在小程序内打开，服务端可能按平台分流。
func (c *Client) campaignJSON(a *auth.Auth, method, path string, out any) error {
	req, err := http.NewRequest(method, c.chatBase(a)+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	req.Header.Set("X-Client-Platform", "miniprogram")
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("campaign read: %w", err)
	}
	var env struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("campaign parse: %w (body: %s)", err, truncateBody(string(raw), 160))
	}
	if env.Code != 0 {
		// code 12304「条件不满足」是 claim 的常规拒绝（未达标），单独识别给上层描述；
		// 其余错误保留错误码原文，便于对照文档新增口径。
		return &CampaignError{Status: resp.StatusCode, Code: env.Code, Msg: env.Msg}
	}
	if out == nil || len(env.Data) == 0 {
		return nil
	}
	return json.Unmarshal(env.Data, out)
}

// CampaignError 活动接口的业务错误（保留 code，供调用方区分「未达标」与真错误）。
type CampaignError struct {
	Status int
	Code   int
	Msg    string
}

func (e *CampaignError) Error() string {
	return fmt.Sprintf("campaign: HTTP %d code=%d msg=%s", e.Status, e.Code, e.Msg)
}

// campaignNotMetCode 「条件不满足」：任务未达标，属预期分支而非故障。
const campaignNotMetCode = 12304

// IsCampaignNotMet 报告 err 是否为「任务未达标」（claim 的正常拒绝）。
func IsCampaignNotMet(err error) bool {
	var ce *CampaignError
	return errors.As(err, &ce) && ce.Code == campaignNotMetCode
}

// CampaignTasks 读活动任务表 + 本号进度（含券状态）。
//
// module 为活动模块名（coffee / campaign …），code 为 activity_code。
func (c *Client) CampaignTasks(a *auth.Auth, module, code string) (CampaignSnapshot, error) {
	var snap CampaignSnapshot
	snap.Code = code
	q := url.Values{"activity_code": {code}}

	var cfg struct {
		Config struct {
			Code     string `json:"activity_code"`
			Title    string `json:"title"`
			StartAt  string `json:"start_at"`
			EndAt    string `json:"end_at"`
			InPeriod bool   `json:"in_period"`
			ServerAt string `json:"server_time"`
		} `json:"config"`
	}
	if err := c.campaignJSON(a, http.MethodGet, campaignBase(module)+"/config?"+q.Encode(), &cfg); err == nil {
		snap.Title = cfg.Config.Title
		snap.StartAt = cfg.Config.StartAt
		snap.EndAt = cfg.Config.EndAt
		snap.InPeriod = cfg.Config.InPeriod
		snap.ServerAt = cfg.Config.ServerAt
	}

	var body struct {
		Invite struct {
			Count int  `json:"count"`
			Need  int  `json:"need"`
			Open  bool `json:"open"`
		} `json:"invite"`
		Progress []struct {
			TaskCode  string `json:"task_code"`
			PeriodKey string `json:"period_key"`
			Progress  int    `json:"progress"`
			ViewedAt  string `json:"viewed_at"`
		} `json:"progress"`
		Tasks []CampaignTask `json:"tasks"`
	}
	if err := c.campaignJSON(a, http.MethodGet, campaignBase(module)+"/tasks?"+q.Encode(), &body); err != nil {
		return snap, err
	}
	snap.Invite = CampaignInvite{Count: body.Invite.Count, Need: body.Invite.Need, Open: body.Invite.Open}

	// progress 是按账号的独立数组，按 task_code 合并进任务表（没有进度条目的任务
	// 说明还没开始，progress 保持 0）。
	byCode := map[string]struct {
		Progress  int
		PeriodKey string
		ViewedAt  string
	}{}
	for _, p := range body.Progress {
		byCode[p.TaskCode] = struct {
			Progress  int
			PeriodKey string
			ViewedAt  string
		}{p.Progress, p.PeriodKey, p.ViewedAt}
	}
	for i := range body.Tasks {
		if p, ok := byCode[body.Tasks[i].Code]; ok {
			body.Tasks[i].Progress = p.Progress
			body.Tasks[i].PeriodKey = p.PeriodKey
			body.Tasks[i].ViewedAt = p.ViewedAt
		}
	}
	snap.Tasks = body.Tasks

	// 券状态只在活动期间查得到，失败不影响任务进度。
	var vs struct {
		Items []struct {
			Prize       string `json:"prize_code"`
			Title       string `json:"title"`
			Status      string `json:"status"`
			Claimable   bool   `json:"claimable"`
			TodayRemain int    `json:"today_remaining"`
			Unused      int    `json:"unused"`
			StockReset  int64  `json:"stock_reset_at"`
		} `json:"items"`
	}
	if err := c.campaignJSON(a, http.MethodGet, campaignBase(module)+"/vouchers?"+q.Encode(), &vs); err == nil {
		for _, v := range vs.Items {
			snap.Vouchers = append(snap.Vouchers, CampaignVoucher{
				Prize: v.Prize, Title: v.Title, Status: v.Status, Claimable: v.Claimable,
				TodayRemain: v.TodayRemain, Unused: v.Unused, StockReset: v.StockReset,
			})
		}
	}
	return snap, nil
}

// CampaignTaskReady 报告任务是否已达标（含 viewed 前置）。
func CampaignTaskReady(t CampaignTask) bool {
	if t.Target <= 0 {
		return false
	}
	return t.Progress >= t.Target
}

// CampaignViewTask 标记任务已看。need_view 的任务必须先调，否则 claim 会拒绝。
func (c *Client) CampaignViewTask(a *auth.Auth, module, code, taskCode string) error {
	q := url.Values{"activity_code": {code}}
	path := campaignBase(module) + "/tasks/" + url.PathEscape(taskCode) + "/viewed?" + q.Encode()
	return c.campaignJSON(a, http.MethodPost, path, nil)
}

// CampaignClaimTask 领任务奖励。未达标返回 IsCampaignNotMet 为真的错误。
func (c *Client) CampaignClaimTask(a *auth.Auth, module, code, taskCode string) error {
	q := url.Values{"activity_code": {code}}
	path := campaignBase(module) + "/tasks/" + url.PathEscape(taskCode) + "/claim?" + q.Encode()
	return c.campaignJSON(a, http.MethodPost, path, nil)
}

// truncateBody 截断响应体用于错误信息（保留可读前缀）。
func truncateBody(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// CampaignStockResetAt 券库存重置时刻（stock_reset_at 是秒级时间戳）。
func CampaignStockResetAt(v CampaignVoucher) time.Time {
	if v.StockReset == 0 {
		return time.Time{}
	}
	return time.Unix(v.StockReset, 0)
}
