package server

import (
	"net/http"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// 进程重启后台账是空的：同一个 task_id 再来查，只能按账号去试、命中后收养。收养时它已经是终态，
// 说明重启前那个进程很可能已经记过账（客户端一路在查）；再记一次就是重复计入用量。所以收养时已
// 完成的任务视为已记账——宁可漏记重启窗口里恰好没人查过的那一单，也不重复记每一个被复查的旧任务。
func TestVideoAdoptedCompletedTaskNotChargedAgain(t *testing.T) {
	f := &videoFake{on: func(c videoCall) (int, string) {
		if c.Path == "/v2/videos/tasks" {
			return 200, videoTaskDone
		}
		return 404, `{"code":404,"msg":"unexpected"}`
	}}
	rec := usage.New("")
	h := NewHandler(Config{Pool: imagePool(), Upstream: f.client(), Usage: rec}) // 刚重启：台账空
	if r := getVideo(t, h, videoTestTaskID); r.Code != 200 {
		t.Fatalf("code=%d body=%s", r.Code, r.Body)
	}
	if got := rec.Snapshot(24, nil).Totals.Credits; got != 0 {
		t.Fatalf("重启后复查已完成的任务又记了 %.2f 积分", got)
	}
}

// 收养时任务还在跑：重启前不可能记过账，出片时照常记一次，之后复查不再记。
func TestVideoAdoptedRunningTaskChargedOnce(t *testing.T) {
	m, f := newPollModeFake("running")
	rec := usage.New("")
	h := NewHandler(Config{Pool: imagePool(), Upstream: f.client(), Usage: rec})
	if r := getVideo(t, h, videoTestTaskID); r.Code != 200 {
		t.Fatalf("收养 running 任务 code=%d body=%s", r.Code, r.Body)
	}
	m.set("done")
	ageQuery(h, videoTestTaskID, 2*videoTaskMinPoll)
	for i := 0; i < 2; i++ {
		if r := getVideo(t, h, videoTestTaskID); r.Code != 200 {
			t.Fatalf("第 %d 次查询 code=%d body=%s", i+1, r.Code, r.Body)
		}
	}
	if got := rec.Snapshot(24, nil).Totals.Credits; got != 57.14 {
		t.Fatalf("收养时还在跑的任务出片后应恰好记一次 57.14，实际 %.2f", got)
	}
}

// 重启后台账里没有 realm：国际版任务只有 global 账号查得到，不能只按 cn 域去试然后回 404。
func TestVideoUnknownTaskTriesBothRealms(t *testing.T) {
	f := &videoFake{on: func(c videoCall) (int, string) {
		if c.Path == "/v2/videos/tasks" && c.Host == "global.example" {
			return 200, videoTaskRunning
		}
		return 404, `{"code":404,"msg":"task not found"}`
	}}
	h := NewHandler(Config{Pool: imagePool(), Upstream: f.client()})
	if r := getVideo(t, h, videoTestTaskID); r.Code != 200 {
		t.Fatalf("国际版任务重启后应能查到，实际 code=%d body=%s", r.Code, r.Body)
	}
	if o, ok := h.videoLedger.get(videoTestTaskID); !ok || o.UID != "g1" || o.Realm != "global" {
		t.Fatalf("应按 global 账号收养：%+v ok=%v", o, ok)
	}
}

// 查任务是轮询，不是用账号干活：查询撞上 429 / 5xx 不能把账号打进冷却，否则一个视频任务被
// 查得勤一点，就把这个号从对话的选号里踢出去。
func TestVideoPollFailureDoesNotCoolAccount(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{http.StatusTooManyRequests, `{"code":429,"msg":"rate limited"}`},
		{http.StatusInternalServerError, `{"code":500,"msg":"internal error"}`},
		{http.StatusBadGateway, `{"code":502,"msg":"bad gateway"}`},
	} {
		f := &videoFake{on: func(c videoCall) (int, string) { return tc.status, tc.body }}
		p := imagePool()
		h := NewHandler(Config{Pool: p, Upstream: f.client()})
		h.videoLedger.put(videoTestTaskID, "c1", "cn", "minimax-video-h3")
		getVideo(t, h, videoTestTaskID)
		if n := len(p.AvailableUIDsForRealm("cn")); n != 1 {
			t.Errorf("查任务撞上 %d 后 cn 账号被冷却（可用 %d/1）", tc.status, n)
		}
	}
}
