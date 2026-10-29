package server

import (
	"reflect"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// policyHandler 构造只带思考策略的最小 handler（declareReasoning 只读
// cfg.Upstream 的策略快照，不触网、不碰账号）。
func policyHandler(t *testing.T, p *upstream.EffortPolicy) *Handler {
	t.Helper()
	c := &upstream.Client{}
	c.SetEffortPolicy(p)
	return &Handler{cfg: Config{Upstream: c}}
}

func TestDeclareReasoningDefaultEffortFollowsGateway(t *testing.T) {
	h := policyHandler(t, &upstream.EffortPolicy{Enabled: true, DefaultEffort: "max"})
	// 上游默认档是 medium，网关默认补 max → 声明成最高支持档 high。
	entry := map[string]any{"reasoning_default_effort": "medium"}
	h.declareReasoning(entry, "cn", "glm-5.2", []string{"low", "high"})

	if got := entry["reasoning_default_effort"]; got != "high" {
		t.Fatalf("reasoning_default_effort=%v want high（客户端按它预填才会和网关补档一致）", got)
	}
	if _, ok := entry["reasoning_gateway_effort"]; ok {
		t.Fatalf("不得再透出网关专有字段: %v", entry)
	}
}

func TestDeclareReasoningDefaultEffortModelOverride(t *testing.T) {
	h := policyHandler(t, &upstream.EffortPolicy{
		Enabled:       true,
		DefaultEffort: "max",
		ModelDefaults: map[string]string{"cn:glm-5.2": "low"},
	})
	entry := map[string]any{"reasoning_default_effort": "high"}
	h.declareReasoning(entry, "cn", "glm-5.2", []string{"low", "high"})
	if got := entry["reasoning_default_effort"]; got != "low" {
		t.Fatalf("按模型覆盖应生效: got=%v want low", got)
	}

	// 域前缀优先：global 同裸名不命中 cn 覆盖，回到最高支持档。
	entry = map[string]any{"reasoning_default_effort": "high"}
	h.declareReasoning(entry, "global", "glm-5.2", []string{"low", "high"})
	if got := entry["reasoning_default_effort"]; got != "high" {
		t.Fatalf("cn 覆盖不该串到 global: got=%v want high", got)
	}
}

func TestDeclareReasoningOffTargetRemovesDefault(t *testing.T) {
	h := policyHandler(t, &upstream.EffortPolicy{
		Enabled:       true,
		DefaultEffort: "max",
		ModelDefaults: map[string]string{"glm-5.2": "off"},
	})
	entry := map[string]any{"reasoning_default_effort": "high", "can_disable_thinking": true}
	h.declareReasoning(entry, "cn", "glm-5.2", []string{"low", "high"})

	if _, ok := entry["reasoning_default_effort"]; ok {
		t.Fatalf("off 不该写进默认档（它不是可发送的档位）: %v", entry)
	}
	got, _ := entry["reasoning_supported_efforts"].([]string)
	if !reflect.DeepEqual(got, []string{"low", "high", "off"}) {
		t.Fatalf("可关思考仍有 off 选项: %v", got)
	}
}

func TestDeclareReasoningKeepsUpstreamDefaultWhenPolicyOff(t *testing.T) {
	for _, p := range []*upstream.EffortPolicy{nil, {Enabled: false, DefaultEffort: "max"}} {
		h := policyHandler(t, p)
		entry := map[string]any{"reasoning_default_effort": "medium"}
		h.declareReasoning(entry, "cn", "glm-5.2", []string{"low", "high"})
		if got := entry["reasoning_default_effort"]; got != "medium" {
			t.Fatalf("策略未启用时不该改写上游默认档: got=%v", got)
		}
		if _, ok := entry["reasoning_gateway_effort"]; ok {
			t.Fatalf("策略未启用时不得新增字段: %v", entry)
		}
	}
}

func TestDeclareReasoningAppendsOff(t *testing.T) {
	supported := []string{"low", "high"}
	h := policyHandler(t, &upstream.EffortPolicy{Enabled: true, DefaultEffort: "max"})
	entry := map[string]any{"can_disable_thinking": true, "reasoning_supported_efforts": supported}
	h.declareReasoning(entry, "cn", "m", supported)

	got, _ := entry["reasoning_supported_efforts"].([]string)
	want := []string{"low", "high", "off"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("supported_efforts=%v want %v", got, want)
	}
	if !reflect.DeepEqual(supported, []string{"low", "high"}) {
		t.Fatalf("不得原地修改调用方切片: %v", supported)
	}
}

func TestDeclareReasoningDoesNotDuplicateOff(t *testing.T) {
	h := policyHandler(t, &upstream.EffortPolicy{Enabled: true, DefaultEffort: "max"})
	entry := map[string]any{"can_disable_thinking": true, "reasoning_supported_efforts": []string{"low", "off"}}
	h.declareReasoning(entry, "cn", "m", []string{"low", "off"})
	got, _ := entry["reasoning_supported_efforts"].([]string)
	if !reflect.DeepEqual(got, []string{"low", "off"}) {
		t.Fatalf("off 已存在时不该重复追加: %v", got)
	}
}

func TestDeclareReasoningWithoutSupported(t *testing.T) {
	h := policyHandler(t, &upstream.EffortPolicy{Enabled: true, DefaultEffort: "max"})
	entry := map[string]any{"can_disable_thinking": true}
	h.declareReasoning(entry, "cn", "m", nil)
	if len(entry) != 1 { // 只保留调用方原有的 can_disable_thinking
		t.Fatalf("没有档位表时不该编造声明: %v", entry)
	}
}
