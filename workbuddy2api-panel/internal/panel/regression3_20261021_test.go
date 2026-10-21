package panel

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// bug 5：任务码兜底解析必须确定性（此前 range map，同一次任务点两次可能解析成不同 code）。
func TestEffectiveTaskCodeDeterministic(t *testing.T) {
	m := map[string]*upstream.Task{
		"valorant_b": {TaskCode: "valorant_b"},
		"valorant_a": {TaskCode: "valorant_a"},
		"perler_x":   {TaskCode: "perler_x"},
	}
	first := effectiveTaskCodeIn(m, "valorant_something")
	for i := 0; i < 100; i++ {
		if got := effectiveTaskCodeIn(m, "valorant_something"); got != first {
			t.Fatalf("解析不稳定：第 %d 次得到 %s，首次是 %s", i, got, first)
		}
	}
	if first != "valorant_a" {
		t.Fatalf("期望按关键词优先级 + 字典序取 valorant_a，得到 %s", first)
	}
	// 精确命中优先
	if got := effectiveTaskCodeIn(m, "perler_x"); got != "perler_x" {
		t.Fatalf("精确匹配优先失效：%s", got)
	}
}
