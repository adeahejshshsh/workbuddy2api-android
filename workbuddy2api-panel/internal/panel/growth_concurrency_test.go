package panel

// 排程成长队列的账号间并发（schedule.growth_concurrency）：缺省 1、上限 4、
// 面板保存后热生效（不必重启）。并发是上游风控暴露面的旋钮，配错方向要有界。

import "testing"

func TestSetGrowthConcurrencyClamps(t *testing.T) {
	p := &Panel{}
	cases := map[int]int{
		0: 1, -3: 1, 1: 1, 3: 3, 4: 4, 9: 4,
	}
	for in, want := range cases {
		p.SetGrowthConcurrency(in)
		if got := int(p.growthConc.Load()); got != want {
			t.Fatalf("SetGrowthConcurrency(%d)=%d want %d", in, got, want)
		}
	}
}

func TestNewPanelSeedsGrowthConcurrency(t *testing.T) {
	p := New(Config{GrowthConcurrency: 2})
	if got := int(p.growthConc.Load()); got != 2 {
		t.Fatalf("构造期应注入 2，got %d", got)
	}
	// 未配置（0）→ 1：RunGrowthQueueOnce 直接读 atomic，不会自己兜底。
	p2 := New(Config{})
	if got := int(p2.growthConc.Load()); got != 0 {
		t.Fatalf("未配置应保持 0（由 RunGrowthQueueOnce 回落 1），got %d", got)
	}
}
