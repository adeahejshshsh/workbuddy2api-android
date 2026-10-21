// blackcat.go 夜猫子任务（black_cat）+ 新手礼包/补偿 API。
//
// 判据（WorkBuddy-Daily 项目实测口径 + 本网关验证）：black_cat 要求在
// **23:00–08:00（本地时区）窗口内**完成 3 次 glm-5.2 对话并上报 chat 事件链；
// 窗口外行为不计分。真实对话走网关既有 ChatStream（glm-5.2），事件链用
// ReportChatActivityModel（chat_5 同款上报形状）。
package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// InNightWindow 当前是否处于夜猫子计数窗口（23:00–08:00 本地时区）。
func InNightWindow(now time.Time) bool {
	h := now.Hour()
	return h >= 23 || h < 8
}

// BlackcatNeed 查 black_cat 任务剩余差额（需要再完成几次对话）。
// 任务不存在返回 0（无可做）；拉取失败返回错误。
func (c *Client) BlackcatNeed(a *auth.Auth) (int64, error) {
	tasks, err := c.ListTasks(a)
	if err != nil {
		return 0, err
	}
	for _, t := range tasks {
		if t.TaskCode == "black_cat" {
			// target=0 是「未 accept / 未下发 progress」的形态（不是已达标）：
			// 0 >= 0 会让 BlackcatNeed 直接返回 0，runBlackCat 据此回「进度已达标」，
			// 一次对话都不发。与 runMPMiniChatTask 的 `if target <= 0 { target = 1 }` 对齐。
			target := t.Target
			if target <= 0 {
				target = 1
			}
			if t.Claimed || t.Current >= target {
				return 0, nil
			}
			return target - t.Current, nil // 用归一化后的 target（0 已按 1 处理）
		}
	}
	return 0, nil
}

// RunNightChats 夜猫子：发 need 次 glm-5.2 真实对话（读干流）并上报事件链。
// 返回成功次数。对话内容极短（1+1），消耗可忽略。
func (c *Client) RunNightChats(a *auth.Auth, need int) (int64, error) {
	return c.RunNightChatsCtx(context.Background(), a, need)
}

// RunNightChatsCtx 带 ctx 版本（bug-07）：停机/客户端断开后可立刻返回，
// 且**最后一轮不再白等 4s**（need=3 从 ≈12s 降到 ≈8s）。
func (c *Client) RunNightChatsCtx(ctx context.Context, a *auth.Auth, need int) (int64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var ok int64
	for i := 0; i < need; i++ {
		body, _ := json.Marshal(map[string]any{
			"model":    "glm-5.2",
			"messages": []map[string]any{{"role": "user", "content": "1+1等于几？直接回答。"}},
			"stream":   true,
		})
		// ctx 必须传进对话请求（bug 8）：此前用 ChatStream（内部 Background），
		// ctx 只在两轮之间的睡眠里生效 —— 一条正在生成的长回复无法被打断，
		// 停机时要等它跑完，与函数名/注释承诺的"立刻返回"相反。
		rc, status, respBody, err := c.ChatStreamContext(ctx, a, body, "", ChatMeta{})
		if err != nil || status >= 400 {
			if rc != nil {
				rc.Close()
			}
			return ok, fmt.Errorf("第 %d 次对话失败: http=%d err=%v body=%.120s", i+1, status, err, respBody)
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(rc, 1<<20))
		rc.Close()
		if err := c.ReportChatActivityModel(a, fmt.Sprintf("wb2api-night-%d-%d", time.Now().UnixMilli(), i), "", "glm-5.2", "GLM-5.2"); err != nil {
			return ok, fmt.Errorf("第 %d 次上报失败: %w", i+1, err)
		}
		ok++
		if i < need-1 {
			// 轮间节流（可取消）；最后一轮不等 —— 等完也没人用了。
			t := time.NewTimer(4 * time.Second)
			select {
			case <-ctx.Done():
				t.Stop()
				return ok, ctx.Err()
			case <-t.C:
			}
		}
	}
	return ok, nil
}

// ClaimGift 领取新手礼包（每号一次，已领返回业务错误）。
func (c *Client) ClaimGift(a *auth.Auth) (int64, error) {
	data, err := c.billingJSON(a, http.MethodPost, "/billing/meter/claim-gift", map[string]any{})
	if err != nil {
		return 0, err
	}
	var resp struct {
		Credit int64 `json:"credit"`
	}
	_ = json.Unmarshal(data, &resp)
	return resp.Credit, nil
}

// ClaimCompensation 领取活动补偿（有则领，无则业务错误）。
func (c *Client) ClaimCompensation(a *auth.Auth) (int64, error) {
	data, err := c.billingJSON(a, http.MethodPost, "/billing/meter/claim-compensation", map[string]any{})
	if err != nil {
		return 0, err
	}
	var resp struct {
		Credit int64 `json:"credit"`
	}
	_ = json.Unmarshal(data, &resp)
	return resp.Credit, nil
}

// HeatmapYesterdayMissed 检查昨日是否漏签（heatmap cell score==0）。
func (c *Client) HeatmapYesterdayMissed(a *auth.Auth) (bool, error) {
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	data, err := c.growthJSON(a, http.MethodGet, "/activity/growth/heatmap", nil)
	if err != nil {
		return false, err
	}
	var resp struct {
		Cells []struct {
			Date  string `json:"date"`
			Score int    `json:"score"`
		} `json:"cells"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return false, err
	}
	for _, cell := range resp.Cells {
		if len(cell.Date) >= 10 && cell.Date[:10] == yesterday {
			return cell.Score == 0, nil
		}
	}
	return false, nil
}

// UseMakeupCard 对指定日期使用补签卡（保住连登连续天数；无卡返回业务错误）。
func (c *Client) UseMakeupCard(a *auth.Auth, date string) error {
	_, err := c.growthJSON(a, http.MethodPost, "/activity/growth/makeup-cards/use",
		map[string]any{"target_date": date})
	return err
}

// CompensationInfo 官方补偿活动的状态（上游 PR #196）。
type CompensationInfo struct {
	Active  bool   `json:"active"`   // false = 当前无补偿活动（正常 200，不算错误）
	Claimed bool   `json:"claimed"`  // 本账号已领
	Credit  int64  `json:"credit"`   // 可领积分数
	EndTime string `json:"end_time"` // 领取截止（RFC3339 或 "2006-01-02 15:04:05"）
}

// CompensationStatus 查询官方补偿活动状态（上游 PR #196「先查后领」）。
//
// 为什么必须先查：旧实现直接 claim-compensation，把「没有活动 / 已领过 / 领取失败」
// 三态压成同一个业务错误静默跳过 —— 带截止日的补偿会**静默过期**，
// 而「查询失败」与「没有补偿」也分不开，面板上完全看不出来。
func (c *Client) CompensationStatus(a *auth.Auth) (CompensationInfo, error) {
	var info CompensationInfo
	data, err := c.billingJSON(a, http.MethodPost, "/billing/meter/compensation-status", map[string]any{})
	if err != nil {
		return info, err
	}
	var resp struct {
		Active    bool   `json:"active"`
		Claimed   bool   `json:"claimed"`
		CreditNum int64  `json:"credit_num"`
		EndTime   string `json:"end_time"`
		Validity  string `json:"validity_period"`
	}
	_ = json.Unmarshal(data, &resp)
	info.Active = resp.Active
	info.Claimed = resp.Claimed
	info.Credit = resp.CreditNum
	info.EndTime = resp.EndTime
	if info.EndTime == "" {
		info.EndTime = resp.Validity
	}
	return info, nil
}
