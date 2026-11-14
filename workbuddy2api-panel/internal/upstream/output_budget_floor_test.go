package upstream

// 输出预算下限的两个对外出入口（续写/救援段复用）：
//   - MinOutputBudget：档位 → 下限，未知档位返回 0（调用方据此「不猜」）；
//   - EnsureMinBudget：把 max_tokens 抬到不低于 floor，探活级额度与已达标不动；
//   - Client.BudgetFloorFor：按**出站同一条档位管线**（策略 → 注入 → 降级）算下限，
//     保证「首段按 high 抬额、续写段按 low 算下限」这种漂移不会出现。

import (
	"encoding/json"
	"testing"
)

func TestMinOutputBudgetByEffort(t *testing.T) {
	cases := map[string]int64{
		"minimal": 4096, "low": 4096, "medium": 6144,
		"high": 8192, "xhigh": 12288, "max": 16384,
		" HIGH ": 8192, // 大小写/空白归一
		"":       0, "off": 0, "unknown": 0,
	}
	for effort, want := range cases {
		if got := MinOutputBudget(effort); got != want {
			t.Fatalf("MinOutputBudget(%q)=%d want %d", effort, got, want)
		}
	}
}

func TestEnsureMinBudget(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		floor int64
		want  float64 // 0 = 不改
	}{
		{"抬到下限", `{"model":"m","max_tokens":1024}`, 8192, 8192},
		{"已达标不动", `{"model":"m","max_tokens":9000}`, 8192, 9000},
		{"没有显式额度不动", `{"model":"m"}`, 8192, 0},
		{"探活额度不动", `{"model":"m","max_tokens":1}`, 8192, 1},
		{"floor=0 不动", `{"model":"m","max_tokens":1024}`, 0, 1024},
	}
	for _, c := range cases {
		var obj map[string]any
		if err := json.Unmarshal([]byte(c.body), &obj); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		EnsureMinBudget(obj, c.floor)
		// EnsureMinBudget 写回 json.Number（保住数字原文，见 payload.go 的 UseNumber 口径），
		// 断言侧统一按数值比。
		got := 0.0
		switch v := obj["max_tokens"].(type) {
		case json.Number:
			f, _ := v.Float64()
			got = f
		case float64:
			got = v
		}
		if c.want == 0 {
			if _, has := obj["max_tokens"]; has && got != 0 {
				t.Fatalf("%s: max_tokens 不该被写=%v", c.name, obj["max_tokens"])
			}
			continue
		}
		if got != c.want {
			t.Fatalf("%s: max_tokens=%v want %v", c.name, obj["max_tokens"], c.want)
		}
	}
}

func TestBudgetFloorForFollowsEffortPipeline(t *testing.T) {
	c := &Client{}
	// 下限只对目录里有输出上限的模型生效（见 outputFloor）；这里给足上限，走的仍是档位表。
	c.StoreModelMaxOutForTest("cn", map[string]int64{"deepseek-v4.1-flash": 131072})
	// 客户端显式 high → 8192。
	if got := c.BudgetFloorFor([]byte(`{"model":"deepseek-v4.1-flash","reasoning_effort":"high"}`), "cn"); got != 8192 {
		t.Fatalf("effort=high floor=%d want 8192", got)
	}
	// 客户端没给档位时，deepseek 系按硬编码默认 high 补档（thinking.go），下限同源。
	if got := c.BudgetFloorFor([]byte(`{"model":"deepseek-v4.1-flash"}`), "cn"); got != 8192 {
		t.Fatalf("默认档 floor=%d want 8192（缺档回退 high）", got)
	}
	// 客户端要关闭思考 → 没有「思考吃光预算」这回事，下限 0（不抬）。
	if got := c.BudgetFloorFor([]byte(`{"model":"deepseek-v4.1-flash","reasoning_effort":"off"}`), "cn"); got != 0 {
		t.Fatalf("effort=off floor=%d want 0", got)
	}
	// 不可解析 → 0（不猜）。
	if got := c.BudgetFloorFor([]byte(`not json`), "cn"); got != 0 {
		t.Fatalf("非法 body floor=%d want 0", got)
	}
	// 目录里没有这个模型 → 0：不知道它最多能写多少，不能按档位抬到 16384。
	if got := c.BudgetFloorFor([]byte(`{"model":"unknown-model","reasoning_effort":"max"}`), "cn"); got != 0 {
		t.Fatalf("目录外模型 floor=%d want 0", got)
	}
	// 目录上限比档位下限小 → 以目录为准。
	c.StoreModelMaxOutForTest("cn", map[string]int64{"small-model": 6000})
	if got := c.BudgetFloorFor([]byte(`{"model":"small-model","reasoning_effort":"max"}`), "cn"); got != 6000 {
		t.Fatalf("目录封顶 floor=%d want 6000", got)
	}
}
