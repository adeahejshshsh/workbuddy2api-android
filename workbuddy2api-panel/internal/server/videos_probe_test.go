package server

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

const videoNotMine = `{"code":404,"msg":"task not found"}`

// probePool n 个 cn 账号 a01..aNN（UID 排序即探测顺序）；globalUID 非空时把它换成 global 域账号。
func probePool(n int, globalUID string) *pool.Pool {
	var auths []*auth.Auth
	for i := 1; i <= n; i++ {
		a := &auth.Auth{UID: fmt.Sprintf("a%02d", i), AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
		if a.UID == globalUID {
			a.Domain = "www.workbuddy.ai"
		}
		auths = append(auths, a)
	}
	return testPoolWith(auths...)
}

// expireQuietPeriod 让 id 的负缓存静默期立即结束（不 sleep 30 秒）。
func expireQuietPeriod(h *Handler, id string) {
	h.videoProbe.mu.Lock()
	h.videoProbe.miss[id].retryAt = time.Now().Add(-time.Second)
	h.videoProbe.mu.Unlock()
}

// 台账里没有的 id：一次请求最多问 videoProbeMax 个账号；静默期内重复查不再碰上游。
func TestVideoProbeBoundedPerRequest(t *testing.T) {
	f := &videoFake{on: func(videoCall) (int, string) { return 404, videoNotMine }}
	h := NewHandler(Config{Pool: probePool(12, ""), Upstream: f.client()})

	for i := 0; i < 3; i++ {
		rec := getVideo(t, h, "ghost")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("code=%d want 404 body=%s", rec.Code, rec.Body)
		}
		if i > 0 && !strings.Contains(rec.Body.String(), "近期刚探测过") {
			t.Fatalf("静默期内的 404 应说明是探测被跳过：%s", rec.Body)
		}
	}
	if n := f.count("/v2/videos/tasks"); n != videoProbeMax {
		t.Fatalf("12 个账号下 3 次查询共打上游 %d 次，want %d（一次探测的上限，其余命中负缓存）", n, videoProbeMax)
	}
}

// 游标接力：静默期过后的重试从上次停下的地方继续，能到达前 5 个之外的账号并收养；
// 收养后已终态的任务标已记账，之后走台账不再碰上游。
func TestVideoProbeCursorAdvancesAndAdopts(t *testing.T) {
	f := &videoFake{on: func(c videoCall) (int, string) {
		if c.Host == "global.example" { // 只有 a08 在 global 域，任务属于它
			return 200, videoTaskDone
		}
		return 404, videoNotMine
	}}
	p := probePool(12, "a08")
	h := NewHandler(Config{Pool: p, Upstream: f.client()})

	if rec := getVideo(t, h, videoTestTaskID); rec.Code != http.StatusNotFound {
		t.Fatalf("a01..a05 都不是主人，首次应 404，got %d body=%s", rec.Code, rec.Body)
	}
	if n := f.count("/v2/videos/tasks"); n != videoProbeMax {
		t.Fatalf("首次探测 %d 次，want %d", n, videoProbeMax)
	}
	expireQuietPeriod(h, videoTestTaskID)
	rec := getVideo(t, h, videoTestTaskID)
	if rec.Code != http.StatusOK {
		t.Fatalf("重试应从 a06 接着问并命中 a08，got %d body=%s", rec.Code, rec.Body)
	}
	if n := f.count("/v2/videos/tasks"); n != videoProbeMax+3 { // a06 a07 a08
		t.Fatalf("重试问了 %d 个账号，want 3", n-videoProbeMax)
	}
	o, ok := h.videoLedger.get(videoTestTaskID)
	if !ok || o.UID != "a08" || o.Realm != "global" || !o.Charged {
		t.Fatalf("应收养到 a08/global 并标已记账：%+v ok=%v", o, ok)
	}
	if st, _ := p.Status("a08"); len(st.ModelCosts) != 0 {
		t.Errorf("已完成的任务收养时不该记账：%+v", st.ModelCosts)
	}
	before := f.count("/v2/videos/tasks")
	getVideo(t, h, videoTestTaskID)
	if n := f.count("/v2/videos/tasks"); n != before {
		t.Errorf("收养的终态任务再查应走台账，不再碰上游：%d → %d", before, n)
	}
}

// 收养时还在跑的任务保持未记账，出片时才由台账命中路径记一次。
func TestVideoProbeAdoptsRunningTaskUncharged(t *testing.T) {
	f := &videoFake{on: func(videoCall) (int, string) { return 200, videoTaskRunning }}
	h := NewHandler(Config{Pool: probePool(3, ""), Upstream: f.client()})
	if rec := getVideo(t, h, videoTestTaskID); rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if o, ok := h.videoLedger.get(videoTestTaskID); !ok || o.UID != "a01" || o.Realm != "cn" || o.Charged {
		t.Fatalf("应收养到 a01/cn 且未记账：%+v ok=%v", o, ok)
	}
}

// 全局限速：换着花样填 id 也只能在每分钟额度内打上游。
func TestVideoProbeGlobalBudget(t *testing.T) {
	f := &videoFake{on: func(videoCall) (int, string) { return 404, videoNotMine }}
	h := NewHandler(Config{Pool: probePool(12, ""), Upstream: f.client()})
	var skipped int
	for i := 0; i < 50; i++ {
		rec := getVideo(t, h, fmt.Sprintf("ghost-%d", i))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("code=%d want 404", rec.Code)
		}
		if strings.Contains(rec.Body.String(), "额度已用尽") {
			skipped++
		}
	}
	if n := f.count("/v2/videos/tasks"); n != videoProbeBudget {
		t.Fatalf("50 个不同 id 共打上游 %d 次，want 恰为额度 %d", n, videoProbeBudget)
	}
	if skipped == 0 {
		t.Error("额度用尽后的 404 应带「额度已用尽」提示")
	}
}

// 探测撞上 429：如实回 429，立即停手，暂停全局探测，且不让任何账号进冷却。
func TestVideoProbe429PausesWithoutCoolingAccounts(t *testing.T) {
	_, f := newPollModeFake("limit")
	p := probePool(12, "")
	h := NewHandler(Config{Pool: p, Upstream: f.client()})
	avail := len(p.AvailableUIDsForRealm("cn"))

	rec := getVideo(t, h, "ghost-429")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d want 429 body=%s", rec.Code, rec.Body)
	}
	if n := f.count("/v2/videos/tasks"); n != 1 {
		t.Fatalf("遇到限流应立即停手，实际探测 %d 次", n)
	}
	if n := len(p.AvailableUIDsForRealm("cn")); n != avail {
		t.Errorf("探测 429 不该让账号进冷却：可用 %d → %d", avail, n)
	}
	getVideo(t, h, "ghost-other")
	if n := f.count("/v2/videos/tasks"); n != 1 {
		t.Errorf("暂停期内换 id 仍在探测：共 %d 次", n)
	}
}

// 台账命中但所属账号已不在号池：任务只有它看得见，不扇出到别的账号，直接 404。
func TestVideoKnownTaskOwnerGoneDoesNotProbe(t *testing.T) {
	f := &videoFake{on: func(videoCall) (int, string) { return 200, videoTaskRunning }}
	h := NewHandler(Config{Pool: probePool(3, ""), Upstream: f.client()})
	h.videoLedger.put("orphan", "removed-uid", "cn", "minimax-video-h3")
	if rec := getVideo(t, h, "orphan"); rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d want 404", rec.Code)
	}
	if n := f.count("/v2/videos/tasks"); n != 0 {
		t.Fatalf("所属账号不在号池时不该打上游，got %d", n)
	}
}
