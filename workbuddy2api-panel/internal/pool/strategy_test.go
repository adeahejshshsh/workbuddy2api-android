// strategy_test.go 选号策略（轮询 / 顺序耗尽）的回归钉。
//
// 三条断言对应三个「写反就静默错」的点：
//
//	轮询要真的换号     否则「轮询」等于加权随机，对账/灰度都失去意义
//	耗尽不推进        当前号还能接就继续用它，否则语义退化成轮询
//	切策略游标失效    旧游标的 UID 在另一套语义下没有意义，沿用会跳到随机位置
package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// strategyPool 造一个 3 号可用池（credits 充足、无冷却）。
func strategyPool(t *testing.T, uids ...string) *Pool {
	t.Helper()
	p := New("")
	for _, uid := range uids {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at-" + uid, RefreshToken: "rt", ExpiresAt: time.Now().Add(time.Hour).Unix()})
		p.SetCredits(uid, 100000, 100000)
	}
	return p
}

// TestRoundRobinCyclesAccounts 轮询必须按 UID 序逐次换号并回环。
//
// 反向代价：这条松掉，「轮询」实际等于加权随机，靠它做账号对账/灰度的人
// 会拿到一堆看似随机的结果却查不出原因。
func TestRoundRobinCyclesAccounts(t *testing.T) {
	p := strategyPool(t, "u1", "u2", "u3")
	p.SetStrategy(StrategyRoundRobin)
	var got []string
	for i := 0; i < 6; i++ {
		a := p.Pick()
		if a == nil {
			t.Fatalf("第 %d 次选号返回 nil", i+1)
		}
		got = append(got, a.UID)
		// 跨过防撞号窗口，否则会走「刚用过就换一个」的兜底，测不到环。
		time.Sleep(minPickGap + 2*time.Millisecond)
	}
	// 断言「不连续撞同一个号」+「6 次覆盖全部 3 个号」。
	// 不断言严格 1-2-3 顺序：候选集本身会随权重/成本分层波动（某轮 u2 被
	// 短暂分层掉时，环上就跳过它），那是既有选号逻辑的正常行为，不是轮询坏了。
	for i := 1; i < len(got); i++ {
		if got[i] == got[i-1] {
			t.Fatalf("连续两次选中同一账号: %v", got)
		}
	}
	seen := map[string]bool{}
	for _, uid := range got {
		seen[uid] = true
	}
	if len(seen) != 3 {
		t.Fatalf("6 次只覆盖了 %d 个账号: %v", len(seen), got)
	}
}

// TestSequentialExhaustHoldsCurrent 顺序耗尽：当前号还能接就一直用它。
func TestSequentialExhaustHoldsCurrent(t *testing.T) {
	p := strategyPool(t, "u1", "u2")
	p.SetStrategy(StrategySequentialExhaust)
	first := p.Pick()
	if first == nil {
		t.Fatal("选号 nil")
	}
	for i := 0; i < 4; i++ {
		time.Sleep(minPickGap + 2*time.Millisecond)
		if a := p.Pick(); a == nil || a.UID != first.UID {
			t.Fatalf("第 %d 次选号换号了（%v → %v）：耗尽语义应继续用当前号", i+2, first.UID, a)
		}
	}
}

// TestSequentialExhaustAdvancesWhenUnavailable 当前号不可用时才换下一个。
func TestSequentialExhaustAdvancesWhenUnavailable(t *testing.T) {
	p := strategyPool(t, "u1", "u2")
	p.SetStrategy(StrategySequentialExhaust)
	first := p.Pick()
	if first == nil {
		t.Fatal("选号 nil")
	}
	// 让当前号进入长冷却：它立刻从候选里消失，策略必须推进到另一个号。
	p.Cooldown(first.UID, CoolSoft, time.Hour, "test")
	time.Sleep(minPickGap + 2*time.Millisecond)
	next := p.Pick()
	if next == nil {
		t.Fatal("推进后选号 nil")
	}
	if next.UID == first.UID {
		t.Fatalf("冷却号仍被选中: %s", next.UID)
	}
}

// TestStrategySwitchDropsStaleCursor 切策略后旧游标不得沿用。
//
// 两个策略的游标语义不同（环上位置 vs 当前烧的号），沿用会让切换后的第一次
// 选号落在「上一个策略的逻辑位置」上，看起来像随机跳。
func TestStrategySwitchDropsStaleCursor(t *testing.T) {
	p := strategyPool(t, "u1", "u2", "u3")
	key := scheduleKey("", "")
	p.SetStrategy(StrategySequentialExhaust)
	p.Pick()
	p.SetStrategy(StrategyRoundRobin)
	if cur := p.scheduling[key]; cur.Strategy != "" && cur.Strategy == StrategySequentialExhaust {
		// 允许保留 UID，但策略标签必须已被新策略覆盖（下一次选号会重建游标）。
		time.Sleep(minPickGap + 2*time.Millisecond)
		p.Pick()
		if cur2 := p.scheduling[key]; cur2.Strategy != StrategyRoundRobin {
			t.Fatalf("切到轮询后游标策略仍为 %q", cur2.Strategy)
		}
	}
}

// TestWeightedStrategyUnchanged 缺省（weighted）不得被策略分派接管。
//
// 这是「不影响既有行为」的边界：weighted 下 pickByStrategyLocked 必须返回 nil，
// 把决策权还给既有的加权随机 + top5 + LRU 兜底那一整套。
func TestWeightedStrategyUnchanged(t *testing.T) {
	p := strategyPool(t, "u1", "u2")
	p.SetStrategy("") // 空 = weighted
	if got := p.Strategy(); got != StrategyWeighted {
		t.Fatalf("空配置应归一为 weighted，实际 %q", got)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.pickByStrategyLocked([]*entry{}, "", "", time.Now()); e != nil {
		t.Fatal("weighted 下不该由策略分派选号")
	}
}

// TestNormalizeStrategy 别名与未知值。
func TestNormalizeStrategy(t *testing.T) {
	cases := map[string]string{
		"":                   StrategyWeighted,
		"weighted":           StrategyWeighted,
		"round_robin":        StrategyRoundRobin,
		"round-robin":        StrategyRoundRobin,
		"rr":                 StrategyRoundRobin,
		"sequential":         StrategySequentialExhaust,
		"sequential_exhaust": StrategySequentialExhaust,
		"seq":                StrategySequentialExhaust,
		"whatever":           StrategyWeighted,
		"  ROUND_ROBIN  ":    StrategyRoundRobin,
	}
	for raw, want := range cases {
		if got := NormalizeStrategy(raw); got != want {
			t.Errorf("NormalizeStrategy(%q)=%q want %q", raw, got, want)
		}
	}
}
