package pool

// 窗口用量调度（issue #112）的记账与降权语义。
//
// 上游那条用例没搬：它断言「零用量基线按 uid 兜底选 a1」，依赖上游的并列兜底内部
// 实现，本仓实测选 a2 —— 照搬会变成一条假红用例。这里改成断言**本仓自己的语义**：
// 记账口径、跨窗口翻转、关停等价、以及降权因子确实只对热点号 >1。

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestWindowTokensAccumulatesPerModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a1"})
	p.SetBalanceWindow(time.Hour)
	p.NoteModelCost("a1", "m", 0, 1000)
	p.NoteModelCost("a1", "m", 0, 500)

	p.mu.RLock()
	got := p.byUID["a1"].windowTokens("m", p.balanceWindow, time.Now())
	other := p.byUID["a1"].windowTokens("other", p.balanceWindow, time.Now())
	expired := p.byUID["a1"].windowTokens("m", p.balanceWindow, time.Now().Add(2*time.Hour))
	p.mu.RUnlock()

	// 同一窗口内累加（曾因 seen 分支整体重写结构体漏带窗口字段而掉回 500）
	if got != 1500 {
		t.Fatalf("窗口内用量应为 1500，得到 %d", got)
	}
	// 调度是 per (账号, 模型) 的，别的模型独立成账
	if other != 0 {
		t.Fatalf("别的模型不该共享窗口用量，得到 %d", other)
	}
	// 窗口过期整体翻篇（不是逐次衰减）
	if expired != 0 {
		t.Fatalf("窗口过期后应归零，得到 %d", expired)
	}
}

func TestBalanceWindowOffRecordsNothing(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a1"})
	p.SetBalanceWindow(0) // 关停
	p.NoteModelCost("a1", "m", 0, 1000)

	p.mu.RLock()
	got := p.byUID["a1"].windowTokens("m", 0, time.Now())
	p.mu.RUnlock()
	if got != 0 {
		t.Fatalf("关停时不该记账，得到 %d", got)
	}
}

// 降权因子：只有窗口内有用量、且同层有均值可比时 > 1；其余情况恒 1（与旧版等价）。
func TestUsageDivOnlyPenalizesHotAccounts(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "hot"})
	p.Add(&auth.Auth{UID: "cold"})
	p.Add(&auth.Auth{UID: "idle"}) // 本窗口零用量
	p.SetBalanceWindow(time.Hour)
	p.NoteModelCost("hot", "m", 0, 3000)
	p.NoteModelCost("cold", "m", 0, 1000)

	p.mu.RLock()
	hot := p.byUID["hot"]
	cold := p.byUID["cold"]
	idle := p.byUID["idle"]
	uctx := usageCtx{model: "m", n: 3,
		sum: hot.windowTokens("m", p.balanceWindow, time.Now()) +
			cold.windowTokens("m", p.balanceWindow, time.Now()) +
			idle.windowTokens("m", p.balanceWindow, time.Now())}
	hotDiv := p.usageDiv(hot, uctx, time.Now())
	coldDiv := p.usageDiv(cold, uctx, time.Now())
	idleDiv := p.usageDiv(idle, uctx, time.Now())
	p.mu.RUnlock()

	// 公式：权重 /= 1 + used/mean。**每个有用量的号都会被除**，用完最多的除得最狠；
	// 零用量的号除数恒 1（这才是「不打散不动它」的语义）。
	if hotDiv <= coldDiv {
		t.Errorf("用量最多的号降权应最重：hot=%v cold=%v", hotDiv, coldDiv)
	}
	if coldDiv <= 1 {
		t.Errorf("有用量的号降权因子应 >1，得到 %v", coldDiv)
	}
	if idleDiv != 1 {
		t.Errorf("零用量号降权因子应为 1，得到 %v", idleDiv)
	}
	// 关停 / 无模型 / 零均值 三种情况都必须恒 1，否则会改变既有选号行为
	if d := p.usageDiv(hot, usageCtx{}, time.Now()); d != 1 {
		t.Errorf("空 context 应恒 1，得到 %v", d)
	}
	if d := p.usageDiv(hot, usageCtx{model: "m", n: 2, sum: 0}, time.Now()); d != 1 {
		t.Errorf("零均值应恒 1，得到 %v", d)
	}
}
