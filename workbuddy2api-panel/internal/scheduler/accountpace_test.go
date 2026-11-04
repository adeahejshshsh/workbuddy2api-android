package scheduler

// 四类任务的「账号间节流」曾经是死代码：checkin/keepalive/travel/activity 四个
// xxxAccountDelay 变量只有 streak 一个调用点，其余全靠 spreadPlan —— 而
// account_spread_seconds 默认没配 → spreadPlan 恒 nil → wait 恒立即返回。
// 默认部署于是变成「全池同秒连发」，正是上游频控 → WAF 403 → 软冷却的自激链起点。

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// 没配摊开窗口（plan==nil）时必须回落到任务族的固定间隔。
func TestAccountPaceFallsBackToFamilyDelay(t *testing.T) {
	s := New(Config{Pool: nil})
	start := time.Now()
	if !s.accountPace(context.Background(), nil, "u1", 40*time.Millisecond) {
		t.Fatal("应正常返回 true")
	}
	if el := time.Since(start); el < 25*time.Millisecond {
		t.Fatalf("计划为空时没有回落节流：只用了 %v（应 ≥40ms）", el)
	}
}

// fallback=0（测试/显式关闭）时不该真睡。
func TestAccountPaceZeroDelayNoSleep(t *testing.T) {
	s := New(Config{Pool: nil})
	start := time.Now()
	s.accountPace(context.Background(), nil, "u1", 0)
	if el := time.Since(start); el > 20*time.Millisecond {
		t.Fatalf("fallback=0 不该等待，实际 %v", el)
	}
}

// 停机/取消要能打断等待（否则 SIGTERM 时还得把 46 个号的间隔睡完）。
func TestAccountPaceHonoursCancel(t *testing.T) {
	s := New(Config{Pool: nil})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if s.accountPace(ctx, nil, "u1", time.Hour) {
		t.Fatal("已取消的 ctx 应立即返回 false")
	}
}

// 接线守卫：四类任务必须走 accountPace，不能再直接 plan.wait ——
// 直连 plan.wait 就会退回「未配窗口 = 不节流」的老问题。
//
// 允许的唯一一处 plan.wait 在 accountPace 自己体内（有计划时委托给它）。
func TestTaskFamiliesUseAccountPace(t *testing.T) {
	src, err := os.ReadFile("scheduler.go")
	if err != nil {
		t.Fatal(err)
	}
	// scheduler.go 里 plan.wait 只应出现在 accountPace 的实现中（一处）。
	if n := strings.Count(string(src), "plan.wait(ctx"); n != 1 {
		t.Errorf("scheduler.go 里 plan.wait 出现 %d 次，应恰好 1 次（只在 accountPace 内）——"+
			"任务族直连 plan.wait 会让默认配置下不节流", n)
	}
	if n := strings.Count(string(src), "s.accountPace(ctx, plan, st.UID"); n != 3 {
		t.Errorf("scheduler.go 里 accountPace 调用 %d 次，应为 3（签到/活跃/保活）", n)
	}

	tr, err := os.ReadFile("travel.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(tr), "plan.wait(ctx") {
		t.Error("travel.go 仍直接调用 plan.wait —— 应改走 s.accountPace")
	}
	if !strings.Contains(string(tr), "s.accountPace(ctx, plan, st.UID, travelAccountDelay)") {
		t.Error("travel.go 未走 accountPace(travelAccountDelay)")
	}
}
