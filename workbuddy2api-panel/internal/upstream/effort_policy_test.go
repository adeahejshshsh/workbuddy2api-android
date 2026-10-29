package upstream

import (
	"encoding/json"
	"testing"
)

func policyObj(t *testing.T, raw string) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return obj
}

func TestResolveGatewayEffort(t *testing.T) {
	supported := []string{"low", "high", "xhigh"}
	cases := []struct {
		name string
		want string
		sup  []string
		exp  string
	}{
		{"max 取最高档", "max", supported, "xhigh"},
		{"空愿望档等同于 max", "", supported, "xhigh"},
		{"具体档支持则原样", "high", supported, "high"},
		{"不支持时落到不高于它的最高档", "medium", supported, "low"},
		{"低于最低支持档时取最低", "minimal", []string{"high", "max"}, "high"},
		{"列表不含可识别档位时取最后一项", "max", []string{"weird-a", "weird-b"}, "weird-b"},
		{"空支持列表", "max", nil, ""},
		{"无法识别的愿望档", "turbo", supported, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ResolveGatewayEffort(c.want, c.sup); got != c.exp {
				t.Fatalf("ResolveGatewayEffort(%q,%v)=%q want %q", c.want, c.sup, got, c.exp)
			}
		})
	}
}

func TestDefaultEffortForModelDefaults(t *testing.T) {
	p := EffortPolicy{DefaultEffort: "max", ModelDefaults: map[string]string{
		"cn:m1":  "low",
		"m2":     "high",
		"global": "xhigh",
	}}
	if got := p.DefaultEffortFor("cn", "m1"); got != "low" {
		t.Errorf("realm 精确覆盖=%q want low", got)
	}
	if got := p.DefaultEffortFor("global", "m1"); got != "max" {
		t.Errorf("realm 不匹配时应回落全局=%q want max", got)
	}
	if got := p.DefaultEffortFor("cn", "m2"); got != "high" {
		t.Errorf("裸名两域共用=%q want high", got)
	}
	if got := p.DefaultEffortFor("cn", "absent"); got != "max" {
		t.Errorf("未配置=%q want max", got)
	}
}

func TestApplyEffortPolicyFillsMissing(t *testing.T) {
	// 客户端没发档位：即使客户端优先也要补默认档（补的是缺失值，不是覆盖意图）。
	obj := policyObj(t, `{"model":"m"}`)
	got := applyEffortPolicy(obj, "m", []string{"low", "high"}, &EffortPolicy{
		Enabled: true, ClientPriority: true, DefaultEffort: "max",
	}, "cn")
	if got != "high" || obj["reasoning_effort"] != "high" {
		t.Fatalf("missing effort: got=%q body=%v", got, obj)
	}
}

func TestApplyEffortPolicyClientPriorityKeepsClient(t *testing.T) {
	obj := policyObj(t, `{"reasoning_effort":"low"}`)
	got := applyEffortPolicy(obj, "m", []string{"low", "high"}, &EffortPolicy{
		Enabled: true, ClientPriority: true, DefaultEffort: "max",
	}, "cn")
	if got != "" || obj["reasoning_effort"] != "low" {
		t.Fatalf("client priority should keep client effort: got=%q body=%v", got, obj)
	}
}

func TestApplyEffortPolicyGatewayRewrites(t *testing.T) {
	obj := policyObj(t, `{"reasoning_effort":"low"}`)
	got := applyEffortPolicy(obj, "m", []string{"low", "high"}, &EffortPolicy{
		Enabled: true, ClientPriority: false, DefaultEffort: "max", OverrideScope: OverrideScopeLevel,
	}, "cn")
	if got != "high" || obj["reasoning_effort"] != "high" {
		t.Fatalf("gateway priority should rewrite: got=%q body=%v", got, obj)
	}
}

func TestApplyEffortPolicyRespectsDisableByDefault(t *testing.T) {
	// scope=level（默认）下，客户端显式关闭思考属于意图，网关不改写。
	for _, raw := range []string{
		`{"reasoning_effort":"off"}`,
		`{"reasoningEffort":"none"}`,
		`{"thinking":{"type":"disabled"}}`,
	} {
		obj := policyObj(t, raw)
		got := applyEffortPolicy(obj, "m", []string{"low", "high"}, &EffortPolicy{
			Enabled: true, ClientPriority: false, DefaultEffort: "max", OverrideScope: OverrideScopeLevel,
		}, "cn")
		if got != "" {
			t.Fatalf("%s: disable intent must be respected, got=%q body=%v", raw, got, obj)
		}
	}
}

func TestApplyEffortPolicyScopeAllForcesThinking(t *testing.T) {
	obj := policyObj(t, `{"reasoning_effort":"off","thinking":{"type":"disabled"}}`)
	got := applyEffortPolicy(obj, "m", []string{"low", "xhigh"}, &EffortPolicy{
		Enabled: true, ClientPriority: false, DefaultEffort: "max", OverrideScope: OverrideScopeAll,
	}, "cn")
	th, _ := obj["thinking"].(map[string]any)
	if got != "xhigh" || obj["reasoning_effort"] != "xhigh" || th["type"] != "enabled" {
		t.Fatalf("scope=all should force thinking: got=%q body=%v", got, obj)
	}
}

func TestApplyEffortPolicyModelDefault(t *testing.T) {
	obj := policyObj(t, `{"model":"m"}`)
	got := applyEffortPolicy(obj, "m", []string{"low", "high"}, &EffortPolicy{
		Enabled: true, ClientPriority: true, DefaultEffort: "max",
		ModelDefaults: map[string]string{"cn:m": "low"},
	}, "cn")
	if got != "low" || obj["reasoning_effort"] != "low" {
		t.Fatalf("model default: got=%q body=%v", got, obj)
	}
}

func TestApplyEffortPolicyNoop(t *testing.T) {
	cases := []struct {
		name   string
		policy *EffortPolicy
		sup    []string
	}{
		{"nil 策略", nil, []string{"low"}},
		{"策略关闭", &EffortPolicy{Enabled: false, DefaultEffort: "max"}, []string{"low"}},
		{"模型无档位", &EffortPolicy{Enabled: true, DefaultEffort: "max"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obj := policyObj(t, `{"model":"m"}`)
			if got := applyEffortPolicy(obj, "m", c.sup, c.policy, "cn"); got != "" {
				t.Fatalf("expected no rewrite, got=%q body=%v", got, obj)
			}
		})
	}
}

func TestResolveGatewayEffortOff(t *testing.T) {
	// off 是「关闭思考」的目标档（面板下拉里与 supportedEfforts 同列），
	// 必须原样解析成 off，不能被就近降级成最弱正档。
	for _, want := range []string{"off", "none", "disabled", " OFF "} {
		if got := ResolveGatewayEffort(want, []string{"low", "high"}); got != EffortOff {
			t.Errorf("ResolveGatewayEffort(%q)=%q want off", want, got)
		}
	}
	if got := ResolveGatewayEffort("off", nil); got != EffortOff {
		t.Errorf("off should resolve without supported list, got=%q", got)
	}
}

func TestApplyEffortPolicyOffTargetFillsDisable(t *testing.T) {
	// 目标 off：客户端没发档位时补「关闭思考」形态（thinking.type=disabled，删档位）。
	obj := policyObj(t, `{"model":"m"}`)
	got := applyEffortPolicy(obj, "m", []string{"low", "high"}, &EffortPolicy{
		Enabled: true, ClientPriority: true, DefaultEffort: "max",
		ModelDefaults: map[string]string{"cn:m": "off"},
	}, "cn")
	th, _ := obj["thinking"].(map[string]any)
	if got != EffortOff || th["type"] != "disabled" {
		t.Fatalf("off target should disable thinking: got=%q body=%v", got, obj)
	}
	if _, ok := obj["reasoning_effort"]; ok {
		t.Fatalf("off target must drop effort field: %v", obj)
	}
}

func TestApplyEffortPolicyOffTargetRespectsScope(t *testing.T) {
	// scope=level：off 要改开关，约定是「只改档位」——客户端显式档位保留。
	obj := policyObj(t, `{"reasoning_effort":"high"}`)
	p := &EffortPolicy{Enabled: true, DefaultEffort: "max", OverrideScope: OverrideScopeLevel,
		ModelDefaults: map[string]string{"m": "off"}}
	if got := applyEffortPolicy(obj, "m", []string{"low", "high"}, p, "cn"); got != "" || obj["reasoning_effort"] != "high" {
		t.Fatalf("scope=level should keep client effort: got=%q body=%v", got, obj)
	}
	// scope=all：连开关一起固定成关闭。
	p.OverrideScope = OverrideScopeAll
	obj = policyObj(t, `{"reasoning_effort":"high"}`)
	got := applyEffortPolicy(obj, "m", []string{"low", "high"}, p, "cn")
	th, _ := obj["thinking"].(map[string]any)
	if got != EffortOff || th["type"] != "disabled" {
		t.Fatalf("scope=all off target: got=%q body=%v", got, obj)
	}
	if _, ok := obj["reasoning_effort"]; ok {
		t.Fatalf("scope=all off target must drop effort: %v", obj)
	}
}

func TestEffortPolicyNormalized(t *testing.T) {
	p := EffortPolicy{
		DefaultEffort: " MAX ", OverrideScope: "BOGUS",
		ModelDefaults: map[string]string{" GLM-5.2 ": " High ", "x": "inherit", "": "max", "y": ""},
	}.Normalized()
	if p.DefaultEffort != "max" || p.OverrideScope != OverrideScopeLevel {
		t.Fatalf("normalized=%+v", p)
	}
	if len(p.ModelDefaults) != 1 || p.ModelDefaults["glm-5.2"] != "high" {
		t.Fatalf("model defaults=%v", p.ModelDefaults)
	}
}
