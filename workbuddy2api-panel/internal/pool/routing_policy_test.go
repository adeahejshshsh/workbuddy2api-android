package pool

// 账号级路由策略（上游 issue #62）、保留积分（#147 同族）与保底放行倍率（#30）。
//
// 这三条都是「人的显式配置」压过自动启发式，所以断言要干净：要么单账号看
// 「选中/返回 nil」的二分，要么两账号看「是不是只剩该层」——不要用多账号+权重去
// 猜，随机源会把失效的实现测成绿的。

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestReserveCreditsStopsLowBalanceAccount：余额已知且 <= 阈值 → 停止接单。
func TestReserveCreditsStopsLowBalanceAccount(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetReserveCredits(10)

	a := &auth.Auth{UID: "low"}
	p.Add(a)
	p.SetCredits("low", 8, 100) // 余额已知（total>0）且 <= 10
	if got := p.Pick(); got != nil {
		t.Fatalf("保留积分应停单，got %v", got)
	}

	// 充值越过阈值 → 自动恢复（不需要 revive）。
	p.SetCredits("low", 50, 100)
	if got := p.Pick(); got == nil {
		t.Fatal("充值后应自动恢复接单，got nil")
	}
}

// TestReserveCreditsIgnoresUnknownBalance：从未查到余额（total=0）的号不受影响 ——
// 否则新导入、还没刷过余额的账号会被 0 值直接停掉。
func TestReserveCreditsIgnoresUnknownBalance(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetReserveCredits(10)

	p.Add(&auth.Auth{UID: "fresh"})
	// 余额未观测（credits=0 且 total=0）。
	if got := p.Pick(); got == nil {
		t.Fatal("余额未知的账号不该被保留积分停掉")
	}
}

// TestReserveCreditsByRealmOverrides：按域覆盖优先于全局值。
func TestReserveCreditsByRealmOverrides(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetReserveCredits(10)
	p.SetReserveCreditsByRealm(map[string]int64{"cn": 1000})

	a := &auth.Auth{UID: "cn1"}
	p.Add(a)
	p.SetCredits("cn1", 500, 500) // > 全局 10，但 < cn 的 1000
	if got := p.Pick(); got != nil {
		t.Fatalf("cn 域阈值 1000 应停单，got %v", got)
	}
}

// TestReserveCreditsExemptsFreeModel：免费档不受保留积分限制（保底是为了留住余额
// 给免费模型用，反过来说免费模型不该被这条规则挡掉）。
func TestReserveCreditsExemptsFreeModel(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetReserveCredits(10)
	p.SetModelRateOf(rates2(map[string]map[string]string{"cn": {"free-model": "0"}}))

	a := &auth.Auth{UID: "low"}
	p.Add(a)
	p.SetCredits("low", 5, 100)
	if got := p.PickExcludingForRealm(nil, "free-model", "cn"); got == nil {
		t.Fatal("免费模型不该被保留积分挡住")
	}
	if got := p.PickExcludingForRealm(nil, "paid-model", "cn"); got != nil {
		t.Fatalf("收费/未知模型应被保留积分挡住，got %v", got)
	}
}

// TestAccountPriorityBurnsConfiguredLayerFirst：配了优先级的号独占流量。
func TestAccountPriorityBurnsConfiguredLayerFirst(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.Add(&auth.Auth{UID: "first"})
	p.Add(&auth.Auth{UID: "later"})
	p.SetAccountRouting(map[string]int{"first": 1, "later": 2}, nil)

	for i := 0; i < 50; i++ {
		if got := p.Pick(); got == nil || got.UID != "first" {
			t.Fatalf("第 %d 次选中 %v，want 只烧 priority=1 的 first", i, got)
		}
	}

	// 层内号临时出池 → 自动让位给下一层（优先级不是排他锁）。
	// 走连败降权（threshold=1），不必等默认 5 次。
	p.SetDegrade(1, time.Minute, time.Minute)
	p.NoteFailures("first")
	if got := p.Pick(); got == nil || got.UID != "later" {
		t.Fatalf("先烧层冷却后应让位给 later，got %v", got)
	}
}

// TestAccountShareSplitsTraffic：同层内按占比分流量（70/30 → 大致 7:3）。
func TestAccountShareSplitsTraffic(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	p.SetAccountRouting(nil, map[string]float64{"a": 70, "b": 30})

	counts := map[string]int{}
	const n = 2000
	for i := 0; i < n; i++ {
		got := p.Pick()
		if got == nil {
			t.Fatal("不该选不到号")
		}
		counts[got.UID]++
	}
	// 允许 ±8% 的抽样误差（2000 次、p=0.7，标准差约 1%）。
	shareA := float64(counts["a"]) / n
	if shareA < 0.62 || shareA > 0.78 {
		t.Fatalf("a 的占比 %.3f 偏离 70%% 太多（counts=%v）", shareA, counts)
	}
}

// TestCreditFloorMaxRateAllowsCheapModels：保底放行倍率上限（0.2）只放便宜模型。
func TestCreditFloorMaxRateAllowsCheapModels(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetCreditFloor(100)
	p.SetCreditFloorMaxRate(0.2)
	p.SetModelRateOf(rates2(map[string]map[string]string{
		"cn": {"cheap": "0.11", "pricey": "1.62"},
	}))

	p.Add(&auth.Auth{UID: "low"})
	p.SetCredits("low", 30, 30) // 触底

	if a := p.PickExcludingForRealm(nil, "cheap", "cn"); a == nil {
		t.Fatal("x0.11 的便宜模型应在触底号上放行（让新模型跑出第一笔账本）")
	}
	if a := p.PickExcludingForRealm(nil, "pricey", "cn"); a != nil {
		t.Fatalf("x1.62 应仍按收费拦，got %v", a)
	}

	// 0 = 严格模式：连便宜模型也拦。
	p.SetCreditFloorMaxRate(0)
	if a := p.PickExcludingForRealm(nil, "cheap", "cn"); a != nil {
		t.Fatalf("严格模式下便宜模型也该拦，got %v", a)
	}
}

// rates2 构造 SetModelRateOf 的查表函数（realm → 模型 → 倍率）。
func rates2(m map[string]map[string]string) func(realm, model string) string {
	return func(realm, model string) string {
		if b, ok := m[realm]; ok {
			return b[model]
		}
		return ""
	}
}
