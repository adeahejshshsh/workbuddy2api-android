// tasks.go growth 域「任务」接口：列表查询 / 接受 / 领取奖励。
//
// 来源：上游 scripts/task_common.py 实测口径（list_tasks/accept_tasks/claim_reward），
// 集成进主程序后不再需要外部脚本。
//
// 端点（chatBase，BillingHeaders）：
//   - GET  /v2/activity/growth/tasks                全量任务列表（含 progress/accept_status）
//   - POST /v2/activity/growth/tasks/accept         {"task_codes":[...]} not_accepted → accepted
//   - POST /v2/activity/growth/tasks/reward/claim   {"task_code":"..."} 完成态领奖
//
// 语义要点：
//   - accept 是"报名"，不产生进度；进度由服务端行为事件点亮（如 chat_request_send 上报、
//     真实对话），故 accept 后可幂等重放。
//   - claim 仅在 progress 达标后可领；重复领返回业务错误（安全，不需要前置状态判断）。
package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// growth 域任务路径（与 scripts/task_common.py 对齐）。
const (
	tasksListPath   = "/v2/activity/growth/tasks"
	tasksAcceptPath = "/v2/activity/growth/tasks/accept"
)

// mpPlatform 小程序口径头值：小程序限定任务（Sequential_Tasks_1 / school_season）
// 的列表下发、accept、claim 全链路要求 X-Client-Platform: miniprogram。
const mpPlatform = "miniprogram"

// Task 单个任务的对外视图（字段名与上游 JSON 对齐，多余字段不透出）。
type Task struct {
	TaskCode     string `json:"task_code"`
	Title        string `json:"title,omitempty"`
	Description  string `json:"description,omitempty"` // 操作指引（含跳转说明）
	TaskDesc     string `json:"task_desc,omitempty"`   // 达成条件简述
	Credit       int64  `json:"credit,omitempty"`      // 奖励积分（上游 reward_credit）
	Energy       int64  `json:"energy,omitempty"`      // 奖励能量（上游 reward_energy）
	HasReward    bool   `json:"has_reward,omitempty"`  // 是否带奖励
	RewardBuddy  bool   `json:"reward_buddy,omitempty"`
	TaskType     string `json:"task_type,omitempty"` // single（一次性）/ 累计型
	Tag          string `json:"tag,omitempty"`       // 端标记（PC 等）
	JumpURL      string `json:"jump_url,omitempty"`  // 客户端跳转协议（workbuddy://...）
	Locked       bool   `json:"locked,omitempty"`    // 上游标记未解锁
	Target       int64  `json:"target"`              // 目标次数（恒输出：0 是有效进度值）
	Current      int64  `json:"current"`             // 当前进度（恒输出：0 是有效进度值）
	AcceptStatus string `json:"accept_status,omitempty"`
	Status       string `json:"status,omitempty"`    // 上游任务状态（complete 等）
	Claimable    bool   `json:"claimable,omitempty"` // 进度达标且未领取（本地推算）
	Claimed      bool   `json:"claimed,omitempty"`   // 已领取（accept_status == claimed）
}

// ListTasks 拉取全量任务列表（默认口径，无端标记头）。
// 响应形如 data.tasks[]，元素字段随任务类型变化（progress 可能是 {current,target} 或平铺），
// 这里做宽松解析：两种形状都尝试。
func (c *Client) ListTasks(a *auth.Auth) ([]Task, error) {
	data, err := c.growthJSON(a, http.MethodGet, tasksListPath, nil)
	if err != nil {
		return nil, err
	}
	return parseGrowthTasks(data)
}

// ListTasksMP 拉取小程序口径的任务列表（X-Client-Platform: miniprogram）。
// 小程序限定任务（Sequential_Tasks_1「小程序首对话」/ school_season「校园日」等）
// 仅在该口径下发——默认列表不出现，accept/claim 同样要求该头（缺头 accept 返回
// task not found，上游 task_runner 实测）。**实测 mp 列表是默认口径的超集**
// （含 RichMeow/Model_chat 等常规任务 + wb_wechat_oa_subscribe_task 等 mp 专属），
// 合并时调用方须按 task_code 去重。
func (c *Client) ListTasksMP(a *auth.Auth) ([]Task, error) {
	data, err := c.growthJSONMP(a, http.MethodGet, tasksListPath, nil)
	if err != nil {
		return nil, err
	}
	return parseGrowthTasks(data)
}

// growthJSONMP 发 growth 域请求（小程序口径：叠加 X-Client-Platform: miniprogram）
// 并解信封。语义同 growthJSON。
func (c *Client) growthJSONMP(a *auth.Auth, method, path string, body any) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.chatBase(a)+path, rdr)
	if err != nil {
		return nil, err
	}
	c.BillingHeaders(req, a)
	req.Header.Set("X-Client-Platform", mpPlatform)
	return c.doJSON(req)
}

// AcceptTasksMP 接受小程序限定任务（mp 头；缺头实测 task not found）。
// 幂等语义同 AcceptTasks。
func (c *Client) AcceptTasksMP(a *auth.Auth, taskCodes []string) error {
	_, err := c.growthJSONMP(a, http.MethodPost, tasksAcceptPath, map[string]any{"task_codes": taskCodes})
	return err
}

// ClaimRewardMP 领取小程序限定任务奖励：chat 域 /activity/growth/tasks/{code}/claim
// + mp 头（上游 task_runner claim_one(mp=True) 同款）；chat 域 400 时降级 Web 域
// 领奖端点（ClaimReward，x-client-platform: web 形态）。
//
// 返回 (credit, energy, already, err)：already 来自上游的 already_claimed 字段
// （bug #6）。不能再用 credit==0 && energy==0 判「已领取」—— 道具类奖励
// （Buddy 盲盒 / 补签卡 / 抽奖次数）本来就是 0 分 0 能，那是一次**成功**领取。
func (c *Client) ClaimRewardMP(a *auth.Auth, taskCode string) (credit, energy int64, already bool, err error) {
	req, err := http.NewRequest(http.MethodPost,
		c.chatBase(a)+"/activity/growth/tasks/"+url.PathEscape(taskCode)+"/claim", nil)
	if err != nil {
		return 0, 0, false, err
	}
	c.BillingHeaders(req, a)
	req.Header.Set("X-Client-Platform", mpPlatform)
	data, err := c.doJSON(req)
	if err != nil {
		// chat 域对该路径 400（部分任务/租户形态）→ Web 域降级（已实测可领）。
		// 必须用 errors.As（bug #9）：类型断言只看最外层，任何一层 fmt.Errorf("%w")
		// 包装都会让它失败 → 400 时不再降级到 Web 领奖端点，mp 领奖直接报错。
		var ue *Error
		if errors.As(err, &ue) && ue.Status == http.StatusBadRequest {
			return c.ClaimReward(a, taskCode)
		}
		return 0, 0, false, err
	}
	return parseClaimReward(data)
}

// parseClaimReward 解析领奖响应 data：{"already_claimed":bool,"credit":n,"energy":n}。
// already_claimed 必须透出（bug #6）：此前解析了却丢掉，调用方只能拿 0/0 猜，
// 于是"领到道具但 0 分 0 能"被误判成"此前已领取"。
func parseClaimReward(data json.RawMessage) (credit, energy int64, already bool, err error) {
	var resp struct {
		AlreadyClaimed bool  `json:"already_claimed"`
		Credit         int64 `json:"credit"`
		Energy         int64 `json:"energy"`
	}
	// 200 + 空 body 时 doJSON 返回 (nil, nil)：直接 Unmarshal(nil) 会报
	// "unexpected end of JSON input"，把**成功**当成失败（报告 #9）——
	// 自动领奖随之中断。空响应按「成功、无字段」处理。
	if len(data) > 0 {
		if err := json.Unmarshal(data, &resp); err != nil {
			return 0, 0, false, err
		}
	}
	return resp.Credit, resp.Energy, resp.AlreadyClaimed, nil
}

// parseGrowthTasks 解析 growth 任务列表 data.tasks[]（默认与 mp 口径共用）。
func parseGrowthTasks(data json.RawMessage) ([]Task, error) {
	var resp struct {
		Tasks []struct {
			TaskCode     string          `json:"task_code"`
			Title        string          `json:"title"`
			Description  string          `json:"description"`
			TaskDesc     string          `json:"task_desc"`
			RewardCredit int64           `json:"reward_credit"` // 上游实际字段名（reward_ 前缀）
			RewardEnergy int64           `json:"reward_energy"`
			HasReward    bool            `json:"has_reward"`
			RewardBuddy  bool            `json:"reward_buddy"`
			TaskType     string          `json:"task_type"`
			Tag          string          `json:"tag"`
			JumpURL      string          `json:"jump_url"`
			Locked       bool            `json:"locked"`
			AcceptStatus string          `json:"accept_status"`
			Status       string          `json:"status"`
			Target       int64           `json:"target"`
			Current      int64           `json:"current"`
			Progress     json.RawMessage `json:"progress"`
		} `json:"tasks"`
	}
	// 200 + 空 body（doJSON 返回 nil）：按「本次没有任务」处理，不报解析错误（报告 #9）。
	if len(data) > 0 {
		if err := json.Unmarshal(data, &resp); err != nil {
			return nil, err
		}
	}
	out := make([]Task, 0, len(resp.Tasks))
	for _, t := range resp.Tasks {
		cur, tgt := t.Current, t.Target
		// progress 可能是 {current,target} 对象（实测口径），覆盖平铺字段。
		if len(t.Progress) > 0 && string(t.Progress) != "null" {
			var pr struct {
				Current int64 `json:"current"`
				Target  int64 `json:"target"`
			}
			if json.Unmarshal(t.Progress, &pr) == nil {
				// 逐字段覆盖（BUG-8）：上游下发 progress:{current:3,target:0}（target
				// 只在平铺字段里）时，整对覆盖会把 tgt 写成 0 → Claimable 判据
				// `tgt > 0 && cur >= tgt` 恒 false → 永远不可领、队列反复重跑并累计跳过。
				if pr.Current > 0 {
					cur = pr.Current
				}
				if pr.Target > 0 {
					tgt = pr.Target
				}
			}
		}
		claimed := t.AcceptStatus == "claimed"
		// done 上游已完成（一次性任务的主要完成标志，bug P1-1）
		done := t.AcceptStatus == "completed" || t.Status == "complete"
		out = append(out, Task{
			TaskCode:     t.TaskCode,
			Title:        t.Title,
			Description:  t.Description,
			TaskDesc:     t.TaskDesc,
			Credit:       t.RewardCredit,
			Energy:       t.RewardEnergy,
			HasReward:    t.HasReward,
			RewardBuddy:  t.RewardBuddy,
			TaskType:     t.TaskType,
			Tag:          t.Tag,
			JumpURL:      t.JumpURL,
			Locked:       t.Locked,
			Target:       tgt,
			Current:      cur,
			AcceptStatus: t.AcceptStatus,
			Status:       t.Status,
			// 一次性任务常 target=0/progress=null，完成状态只在 accept_status 上
			// （bug P1-1）：只判 tgt>0 && cur>=tgt 会让它们永远 claimable=false，
			// 面板不渲染「领取」按钮、排序也归到"进行中"，而 autotask 各处却按
			// AcceptStatus=="completed" 判完成 —— 两侧口径不一致。
			Claimable: !claimed && ((tgt > 0 && cur >= tgt) || done),
			Claimed:   claimed,
		})
	}
	return out, nil
}

// AcceptTasks 接受任务（幂等：已 accepted 时上游返回成功或业务提示，均不视为致命错误）。
func (c *Client) AcceptTasks(a *auth.Auth, taskCodes []string) error {
	_, err := c.growthJSON(a, http.MethodPost, tasksAcceptPath, map[string]any{"task_codes": taskCodes})
	return err
}

// ClaimReward 领取单个任务奖励。
//
// 端点来源（实测）：Web 成长中心的领奖请求 ——
//
//	POST https://www.workbuddy.cn/activity/growth/tasks/<task_code>/claim
//	（任务码在**路径**里，无 body；带 x-client-platform: web 头，Bearer 鉴权）
//
// 关键区别：此前误用 CLI 域 copilot.tencent.com 的
// /v2/activity/growth/tasks/reward/claim（task_code 放 body），该路径**不存在**，
// 一直返回 400 "task not completed"，是此前领奖失败的真实原因。
// 返回 (credit, energy, already, err)：credit/energy 为本次到账奖励，
// already 为上游明示的「此前已领取」（不要用金额推断，见 ClaimRewardMP 注释）。
func (c *Client) ClaimReward(a *auth.Auth, taskCode string) (credit, energy int64, already bool, err error) {
	req, err := http.NewRequest(http.MethodPost,
		c.webBase(a)+"/activity/growth/tasks/"+url.PathEscape(taskCode)+"/claim", nil)
	if err != nil {
		return 0, 0, false, err
	}
	// Web 端请求头形状（对照浏览器实际请求）：Origin/Referer 指向 workbuddy.cn 成长中心，
	// 带 x-client-platform: web 标记来源端。
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", c.webOrigin(a))
	req.Header.Set("Referer", c.webOrigin(a)+"/profile/growth-center")
	req.Header.Set("x-client-platform", "web")
	if ua := c.userAgent(a); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if d := a.DomainValue(); d != "" {
		req.Header.Set("X-Domain", d)
	}

	data, err := c.doJSON(req)
	if err != nil {
		return 0, 0, false, err
	}
	// 响应 data：{"already_claimed":bool,"credit":100,"energy":5,...}
	var resp struct {
		AlreadyClaimed bool  `json:"already_claimed"`
		Credit         int64 `json:"credit"`
		Energy         int64 `json:"energy"`
	}
	// 空 body = 成功但无字段（报告 #9），不要 Unmarshal(nil) 判失败。
	if len(data) > 0 {
		if err := json.Unmarshal(data, &resp); err != nil {
			return 0, 0, false, err
		}
	}
	// 幂等：重复领取不算错误，但无新增奖励。用上游明示的 already_claimed 判断，
	// 不再靠"金额为 0"推断（bug #6：道具类奖励本来就是 0 分 0 能）。
	return resp.Credit, resp.Energy, resp.AlreadyClaimed, nil
}
