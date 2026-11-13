package pool

import "testing"

// 免费号调不通的模型（accessMode=exclusive）：已知免费的号不接单，未知的照旧参与。
func TestPickSkipsKnownFreeForExclusiveModel(t *testing.T) {
	p := strategyPool(t, "u0", "u1")
	paid := map[string]struct {
		paid  bool
		known bool
	}{
		"u0": {paid: false, known: true}, // 已知免费
		"u1": {paid: true, known: true},  // 已知付费
	}
	p.SetExclusiveModels(
		func(_, model string) bool { return model == "pro-model" },
		func(uid string) (bool, bool) { s := paid[uid]; return s.paid, s.known },
	)
	for i := 0; i < 20; i++ {
		if a := p.PickExcludingForModel(nil, "pro-model"); a == nil || a.UID != "u1" {
			t.Fatalf("exclusive 模型应只派给已知付费号，第 %d 次拿到 %v", i, a)
		}
	}
	// 非 exclusive 模型：闸门不生效，两个号都还能用。
	seen := map[string]bool{}
	for i := 0; i < 40; i++ {
		if a := p.PickExcludingForModel(nil, "free-model"); a != nil {
			seen[a.UID] = true
		}
	}
	if !seen["u0"] || !seen["u1"] {
		t.Fatalf("非 exclusive 模型不该被闸门影响，实际只用到 %v", seen)
	}
}

// 池里只剩已知免费的号：放行，不能变成「无号可用」。
func TestPickExclusiveFallsBackWhenAllFree(t *testing.T) {
	p := strategyPool(t, "u0", "u1")
	p.SetExclusiveModels(
		func(_, model string) bool { return model == "pro-model" },
		func(string) (bool, bool) { return false, true }, // 全是已知免费
	)
	if a := p.PickExcludingForModel(nil, "pro-model"); a == nil {
		t.Fatal("全是免费号时也要给一个号，宁可撞 11158 也不能说无号可用")
	}
}

// 付费状态没观测过（known=false）时放行：缺数据不能把能用的号排掉。
func TestPickExclusiveAllowsUnknownPaid(t *testing.T) {
	p := strategyPool(t, "u0", "u1")
	p.SetExclusiveModels(
		func(_, model string) bool { return model == "pro-model" },
		func(string) (bool, bool) { return false, false }, // 一律未知
	)
	seen := map[string]bool{}
	for i := 0; i < 40; i++ {
		if a := p.PickExcludingForModel(nil, "pro-model"); a != nil {
			seen[a.UID] = true
		}
	}
	if !seen["u0"] || !seen["u1"] {
		t.Fatalf("未知付费状态应照旧参与选号，实际只用到 %v", seen)
	}
}
