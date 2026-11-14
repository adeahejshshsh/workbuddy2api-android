package server

// 11140 风控连坐保护（restrict.go）：命中即停手 + 多号被限制时全池暂停。
//
// 为什么这条值得单独钉：上游把「账号存在异常行为」判在**设备/出口**上，本仓原来的
// 处置是「禁用这个号、换下一个继续同一请求」——一个客户端请求能顺着轮转把池子挨个
// 撞一遍（hub #422 实测一次任务 7 个号全封）。所以判据不是「有没有禁用这个号」，
// 而是「有没有停手」。

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const restrictedCNBody = `{"code":11140,"msg":"账号存在异常行为，已被限制使用"}`

func restrictedAuths() (*auth.Auth, *auth.Auth) {
	return &auth.Auth{UID: "r1", AccessToken: "at1", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999},
		&auth.Auth{UID: "r2", AccessToken: "at2", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999}
}

// TestRestrictionGate 状态机本身：同号不计数、异号触发、hold=0 关闭、窗口外剪枝。
func TestRestrictionGate(t *testing.T) {
	oldW, oldM := restrictionWaveWindow, restrictionWaveMin
	t.Cleanup(func() { restrictionWaveWindow, restrictionWaveMin = oldW, oldM })

	var g restrictionGate
	hold := 30 * time.Minute

	// 同一个号反复命中只更新时间，不算「多个账号」——否则一个号被重试几次就把全池停了
	if g.note("cn", "u1", hold, 0) {
		t.Fatal("单号首次命中不应暂停全池")
	}
	if g.note("cn", "u1", hold, 0) {
		t.Fatal("同一个号重复命中不应算连坐")
	}
	if d := g.remaining("cn"); d != 0 {
		t.Fatalf("不应处于暂停态，remaining=%s", d)
	}

	// 窗口内第二个**不同**账号命中 → 全池暂停
	if !g.note("cn", "u2", hold, 0) {
		t.Fatal("窗口内第二个账号被限制应触发全池暂停")
	}
	if d := g.remaining("cn"); d <= 0 || d > hold {
		t.Fatalf("remaining=%s，应在 (0, %s]", d, hold)
	}

	// 判定成立后窗口清空：解除后需要全新一轮才再触发
	g.reset()
	if d := g.remaining("cn"); d != 0 {
		t.Fatalf("reset 后不应仍暂停: %s", d)
	}
	if g.note("cn", "u3", hold, 0) {
		t.Fatal("reset 后单号命中不应立即暂停")
	}

	// hold=0 = 只保留「命中即停手」，不做全池暂停
	g.reset()
	if g.note("cn", "u4", 0, 0) || g.note("cn", "u5", 0, 0) {
		t.Fatal("hold=0 时不应暂停全池")
	}
	if d := g.remaining("cn"); d != 0 {
		t.Fatalf("hold=0 仍暂停了: %s", d)
	}

	// 窗口外的旧命中要被剪掉
	g.reset()
	restrictionWaveWindow = 10 * time.Millisecond
	g.note("cn", "u6", hold, 0)
	time.Sleep(30 * time.Millisecond)
	if g.note("cn", "u7", hold, 0) {
		t.Fatal("窗口外的命中不应计入连坐")
	}
}

// TestChatStopsRotatingOnRestrictedAccount 命中 11140 就地停手：不换号、不喂熔断，
// 且单号命中还不到全池暂停的门槛。
func TestChatStopsRotatingOnRestrictedAccount(t *testing.T) {
	calls := 0
	hitAuth := ""
	fake := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		hitAuth = authz
		return http.StatusForbidden, restrictedCNBody, false
	})
	a1, a2 := restrictedAuths()
	h := NewHandler(Config{Pool: testPoolWith(a1, a2), Upstream: fake, RestrictionHold: 30 * time.Minute})

	rec := doChatReq(t, h, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("上游 403 不应回 200: %s", rec.Body.String())
	}
	if calls != 1 {
		t.Fatalf("11140 应就地停手（只打 1 个账号），实际打了 %d 个 —— 换号会把下一个号也送进风控", calls)
	}
	if d := h.restrict.remaining("cn"); d != 0 {
		t.Fatalf("只有 1 个账号被限制时不应全池暂停，remaining=%s", d)
	}
	// 被上游判异常的那个号必须硬禁用（不是软冷却 10 分钟后又回到轮转里）。
	// 选号是加权随机，不能假定先选 r1：按实际命中的凭证反查 uid。
	uid := map[string]string{"Bearer at1": "r1", "at1": "r1", "Bearer at2": "r2", "at2": "r2"}[hitAuth]
	if uid == "" {
		t.Fatalf("未能从 Authorization=%q 反查命中账号", hitAuth)
	}
	st, ok := h.cfg.Pool.Status(uid)
	if !ok || !st.Disabled {
		t.Fatalf("被判异常行为的账号 %s 应硬禁用（disabled=true），实际 ok=%v disabled=%v reason=%q",
			uid, ok, st.Disabled, st.Reason)
	}
}

// TestChatFailsFastDuringRestrictionHold 全池暂停期间：直接 503 + Retry-After，
// 一个上游请求都不发（拿账号去试就是扩大损失）。
func TestChatFailsFastDuringRestrictionHold(t *testing.T) {
	calls := 0
	fake := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	a1, a2 := restrictedAuths()
	h := NewHandler(Config{Pool: testPoolWith(a1, a2), Upstream: fake, RestrictionHold: 30 * time.Minute})

	// 制造一轮连坐扫荡（两个不同账号被限制）
	if h.restrict.note("cn", "x1", 30*time.Minute, 0) {
		t.Fatal("首个账号不应触发暂停")
	}
	if !h.restrict.note("cn", "x2", 30*time.Minute, 0) {
		t.Fatal("第二个账号应触发暂停")
	}

	rec := doChatReq(t, h, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("暂停期应回 503，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Fatal("暂停期必须带 Retry-After，否则客户端只会盲目重试")
	}
	if !strings.Contains(rec.Body.String(), "account_restricted_hold") {
		t.Fatalf("错误码应为 account_restricted_hold: %s", rec.Body.String())
	}
	if calls != 0 {
		t.Fatalf("暂停期不应再打上游，实际 %d 次", calls)
	}
}

// TestChatKeepsRotatingWhenHoldDisabled hold=0 时只保留「命中即停手」：
// 单号仍停手，但不会因为多号被限制而全池暂停。
func TestChatKeepsRotatingWhenHoldDisabled(t *testing.T) {
	calls := 0
	fake := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return http.StatusForbidden, restrictedCNBody, false
	})
	a1, a2 := restrictedAuths()
	h := NewHandler(Config{Pool: testPoolWith(a1, a2), Upstream: fake, RestrictionHold: 0})

	doChatReq(t, h, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	if calls != 1 {
		t.Fatalf("hold=0 时仍应「命中即停手」（只打 1 个账号），实际 %d", calls)
	}
	if d := h.restrict.remaining("cn"); d != 0 {
		t.Fatalf("hold=0 不应进入全池暂停: %s", d)
	}
}
