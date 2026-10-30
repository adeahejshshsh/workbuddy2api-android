package server

// issue #117：全池该模型没号，但原因是**在途占满**而不是冷却时，等一个名额再试一次
// （上游 2026-10-30 `9f70e00`）。
//
// 现场：两个 CN 号，一号撞 6004 把 deepseek-v4.1-flash 锁到上游重置时刻 ——
//   一号锁模型 + 另一号空闲   → 200（本来就对）
//   一号锁模型 + 另一号在途满 → 503 no_healthy_account
// 第二条就是 #117：模型限额不锁全池，它只是把名额从「2 号 × 3」压成「1 号 × 3」，
// 第 4 个并发必然没号 —— 而它只差几百毫秒就有名额。

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func issue117Auths() (*auth.Auth, *auth.Auth) {
	return &auth.Auth{UID: "a1", AccessToken: "at1", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a2", AccessToken: "at2", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999}
}

func doChatReq(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestIssue117OtherBusyWaitsForSlot 主现场：另一号在途占满 → 等一个名额（而不是立刻 503）。
func TestIssue117OtherBusyWaitsForSlot(t *testing.T) {
	oldCap := busyWaitCap
	busyWaitCap = 3 * time.Second
	t.Cleanup(func() { busyWaitCap = oldCap })

	a1, a2 := issue117Auths()
	p := testPoolWith(a1, a2)
	p.SetMaxInFlight(3)
	p.CooldownSoftForModel("a1", time.Minute, time.Now().Add(4*time.Hour), "deepseek-v4.1-flash", "6004 ...")
	for i := 0; i < 3; i++ {
		if !p.Acquire("a2") {
			t.Fatalf("acquire %d 失败", i)
		}
	}
	if !p.SlotWaitForRealm("deepseek-v4.1-flash", "cn") {
		t.Fatal("应判定为「都在途占满」")
	}
	// 200ms 后有一个在途请求收尾（释放名额）—— 请求应等到它并成功。
	go func() {
		time.Sleep(200 * time.Millisecond)
		p.Release("a2")
	}()
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{Pool: p, Upstream: up})
	start := time.Now()
	rec := doChatReq(t, h, `{"model":"cn:deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	t.Logf("code=%d waited=%s", rec.Code, time.Since(start).Round(time.Millisecond))
	if rec.Code != 200 {
		t.Errorf("名额释放后应 200，得到 %d：%s", rec.Code, rec.Body.String())
	}
}

// 名额一直不释放时必须收敛回 503（等待有上限，不能把请求挂死）。
func TestIssue117SlotWaitIsBounded(t *testing.T) {
	oldCap := busyWaitCap
	busyWaitCap = 150 * time.Millisecond
	t.Cleanup(func() { busyWaitCap = oldCap })

	a1, a2 := issue117Auths()
	p := testPoolWith(a1, a2)
	p.SetMaxInFlight(3)
	p.CooldownSoftForModel("a1", time.Minute, time.Now().Add(4*time.Hour), "deepseek-v4.1-flash", "6004 ...")
	for i := 0; i < 3; i++ {
		p.Acquire("a2")
	}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{Pool: p, Upstream: up})
	start := time.Now()
	rec := doChatReq(t, h, `{"model":"cn:deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("等待应被 busyWaitCap 收住，实际 %v", d)
	}
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "no_healthy_account") {
		t.Errorf("名额始终不释放应 503 no_healthy_account，得到 %d：%s", rec.Code, rec.Body.String())
	}
}
