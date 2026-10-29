package main

// 思考档位策略配置（上游 2026-10-29 `1792ef1`）。
//
// 缺省必须是「补缺档 + 客户端优先 + 不覆盖关闭意图」——这是对既有行为最小的改动：
// 老配置不写 thinking 节时，只在客户端没发档位的情况下补一个默认档，其它一律不动。

import "testing"

func TestThinkingDefaults(t *testing.T) {
	c := Default()
	if !c.Thinking.Enabled {
		t.Error("思考档位策略默认应开启（缺档补默认档）")
	}
	if c.Thinking.DefaultEffort != "max" {
		t.Errorf("默认档应为 max（取模型最高档），got %q", c.Thinking.DefaultEffort)
	}
	if !c.Thinking.ClientPriority {
		t.Error("默认应客户端优先：客户端显式档位不该被改写")
	}
	if c.Thinking.OverrideScope != "level" {
		t.Errorf("默认作用域应为 level（尊重客户端关闭思考），got %q", c.Thinking.OverrideScope)
	}
}

func TestThinkingEnvOverrides(t *testing.T) {
	t.Setenv("WB2A_THINKING_ENABLED", "false")
	t.Setenv("WB2A_THINKING_DEFAULT_EFFORT", "high")
	t.Setenv("WB2A_THINKING_CLIENT_PRIORITY", "false")
	t.Setenv("WB2A_THINKING_OVERRIDE_SCOPE", "all")
	c := Default()
	applyEnv(c)
	if c.Thinking.Enabled {
		t.Error("env 应能关闭策略")
	}
	if c.Thinking.DefaultEffort != "high" {
		t.Errorf("默认档 env 未生效: %q", c.Thinking.DefaultEffort)
	}
	if c.Thinking.ClientPriority {
		t.Error("client_priority env 未生效")
	}
	if c.Thinking.OverrideScope != "all" {
		t.Errorf("override_scope env 未生效: %q", c.Thinking.OverrideScope)
	}
}

// 策略转换：未启用时 Enabled=false（管线退回旧行为），字段逐个透传。
func TestEffortPolicyOf(t *testing.T) {
	if effortPolicyOf(nil) != nil {
		t.Error("nil 配置应返回 nil")
	}
	c := Default()
	c.Thinking.Enabled = false
	c.Thinking.DefaultEffort = "high"
	c.Thinking.OverrideScope = "all"
	c.Thinking.ModelDefaults = map[string]string{"cn:deepseek-v4.1-flash": "high"}
	p := effortPolicyOf(c)
	if p.Enabled || p.DefaultEffort != "high" || p.OverrideScope != "all" {
		t.Fatalf("转换丢字段: %+v", p)
	}
	if p.ModelDefaults["cn:deepseek-v4.1-flash"] != "high" {
		t.Errorf("ModelDefaults 未透传: %v", p.ModelDefaults)
	}
}
