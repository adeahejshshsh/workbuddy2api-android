package pool

import (
	"testing"
	"time"
)

// 游标是运行态、不落盘：选号不能把池置脏，否则每次选号都让 flusher 重写一遍
// state.json（负载下每 5 秒一次 fsync，却没有任何新内容）。
//
// 变异判据：pickRoundRobinLocked / pickSequentialLocked 里加回 p.dirty.Store(true)，本用例红。
func TestStrategyPickDoesNotDirtyState(t *testing.T) {
	for _, strategy := range []string{StrategyRoundRobin, StrategySequentialExhaust} {
		p := strategyPool(t, "u1", "u2", "u3")
		p.SetStrategy(strategy)
		p.Flush() // 清掉准备阶段留下的 dirty
		for i := 0; i < 5; i++ {
			if p.Pick() == nil {
				t.Fatalf("%s 第 %d 次选号返回 nil", strategy, i+1)
			}
			time.Sleep(minPickGap + 2*time.Millisecond)
		}
		if p.dirty.Load() {
			t.Errorf("%s: 选号把池置脏了（游标不落盘，不该触发 state.json 重写）", strategy)
		}
	}
}
