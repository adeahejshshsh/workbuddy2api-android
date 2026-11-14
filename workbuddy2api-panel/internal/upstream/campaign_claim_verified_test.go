package upstream

// campaign_claim_verified_test.go 钉住「领奖要回读 claimed_at」这条：
// POST 返回 200 只说明请求被接受，积分是否入账要看 progress[].claimed_at。
// 反例正是竞品仓库（niyouqianma/workbuddy2api-hub）报喜不报忧的那种写法。

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// claimedTasksBody 每次读都回同一个任务；claimed 由 claimedAt 决定。
func claimedTasksBody(claimedAt string) string {
	claimed := ""
	if claimedAt != "" {
		claimed = `,"claimed_at":"` + claimedAt + `"`
	}
	return `{"code":0,"msg":"OK","data":{
 "invite":{"count":0,"need":3,"open":false},
 "progress":[{"task_code":"ai_chat","progress":3,"completed_at":"2026-11-12T09:00:00+08:00"` + claimed + `}],
 "tasks":[{"task_code":"ai_chat","title":"简单任务","target_count":3,"reward_credit":100,"need_view":true,"enabled":1}]}}`
}

func TestCampaignClaimVerified(t *testing.T) {
	oldGap, oldPoll := campaignClaimGap, campaignClaimPoll
	campaignClaimGap, campaignClaimPoll = time.Millisecond, 2
	t.Cleanup(func() { campaignClaimGap, campaignClaimPoll = oldGap, oldPoll })

	// 1. claim 之后 progress 里出现 claimed_at → 已入账。
	var claims int32
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/claim"):
			atomic.AddInt32(&claims, 1)
			return jsonResp(200, `{"code":0,"msg":"OK","data":{}}`), nil
		case strings.HasSuffix(r.URL.Path, "/tasks"):
			return jsonResp(200, claimedTasksBody("2026-11-12T09:05:00+08:00")), nil
		}
		return jsonResp(200, `{"code":0,"msg":"OK","data":{}}`), nil
	})
	ok, err := c.CampaignClaimVerified(&auth.Auth{UID: "u1", Domain: "www.workbuddy.cn"}, "", "c", "ai_chat")
	if err != nil || !ok {
		t.Fatalf("verified=%v err=%v want true,nil", ok, err)
	}
	if atomic.LoadInt32(&claims) != 1 {
		t.Fatalf("claim 请求次数=%d want 1", claims)
	}

	// 2. claim 成功但 progress 始终没有 claimed_at → 未入账，必须报 false。
	c2 := testClient(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/tasks") {
			return jsonResp(200, claimedTasksBody("")), nil
		}
		return jsonResp(200, `{"code":0,"msg":"OK","data":{}}`), nil
	})
	ok, err = c2.CampaignClaimVerified(&auth.Auth{UID: "u1"}, "", "c", "ai_chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if ok {
		t.Fatal("没有 claimed_at 却报已入账：这正是「报喜不报忧」")
	}

	// 3. claim 本身失败（409/12304）→ 原样返回错误，调用方据此判「未达标」。
	c3 := testClient(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/claim") {
			return jsonResp(409, `{"code":12304,"msg":"task not completed"}`), nil
		}
		return jsonResp(200, claimedTasksBody("")), nil
	})
	if ok, err = c3.CampaignClaimVerified(&auth.Auth{UID: "u1"}, "", "c", "ai_chat"); ok || !IsCampaignNotMet(err) {
		t.Fatalf("verified=%v err=%v want false + IsCampaignNotMet", ok, err)
	}
}

// claimed_at 已存在时 CampaignTaskReady 必须为假：否则每点一次「领取」都对已领任务重发
// viewed + claim（幂等但白打上游）。
func TestCampaignTaskReadySkipsClaimedAt(t *testing.T) {
	base := CampaignTask{Code: "ai_chat", Target: 3, Progress: 3}
	if !CampaignTaskReady(base) {
		t.Fatal("达标未领 → 应为 true")
	}
	base.ClaimedAt = "2026-11-12T09:05:00+08:00"
	if CampaignTaskReady(base) {
		t.Fatal("claimed_at 有值 → 应为 false")
	}
}
