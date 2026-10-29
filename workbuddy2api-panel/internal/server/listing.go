package server

import (
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// reasoningOffEffort 是「关闭思考」在档位表里的声明值。上游 supportedEfforts 不含
// 它（它是开关不是档位），但网关与面板都允许 off，客户端要把它当合法选项就必须在
// 列表里看到——否则「支持哪些档位」与「能不能关」两份声明会互相矛盾。
const reasoningOffEffort = "off"

// declareReasoning 修正 /v1/models 里两份「客户端会直接读」的思考档位声明：
//
//  1. reasoning_default_effort —— 客户端据此预填默认档。上游给的值是「上游自己的
//     默认档」（典型 high），而网关实际会在缺档时补 thinking.default_effort
//     （默认 max，或按模型覆盖）。沿用上游值会让客户端预填 high，把网关的补档
//     抵消掉，所以策略开启时改写为网关解析后的**实际生效档**；策略关闭时保持
//     上游原值（网关不再干预）。
//  2. reasoning_supported_efforts 追加 off —— 仅 can_disable_thinking 的模型：
//     网关允许 off 关闭思考，档位表里也要能选到。
//
// 不新增网关专有字段：下游只看到列表里既有的通用字段（id/context_length/
// max_output_tokens/能力旗标/reasoning_*），标准客户端忽略未知键、认得的客户端
// 直接拿到正确值。
//
// supported 为空（模型没有档位表）时不声明任何字段：不编造档位。
func (h *Handler) declareReasoning(entry map[string]any, realm, bare string, supported []string) {
	if len(supported) == 0 {
		return
	}
	if policy := h.effortPolicy(); policy != nil && policy.Enabled {
		target := upstream.ResolveGatewayEffort(policy.DefaultEffortFor(realm, bare), supported)
		if target == reasoningOffEffort {
			// 网关默认关思考：别把 off 写进「默认档」（它不是一个可发送的档位），
			// 客户端不填档位即可，网关会关掉；填了档位则按客户端优先生效。
			delete(entry, "reasoning_default_effort")
		} else if target != "" {
			entry["reasoning_default_effort"] = target
		}
	}
	if disable, _ := entry["can_disable_thinking"].(bool); disable && !hasEffort(supported, reasoningOffEffort) {
		entry["reasoning_supported_efforts"] = append(append([]string(nil), supported...), reasoningOffEffort)
	}
}

// effortPolicy 取当前网关思考档位策略（热更后的快照）；未接入上游客户端时 nil。
func (h *Handler) effortPolicy() *upstream.EffortPolicy {
	if h.cfg.Upstream == nil {
		return nil
	}
	return h.cfg.Upstream.EffortPolicy()
}

// hasEffort 精确匹配档位（大小写不敏感，与 payload 侧归一化同口径）。
func hasEffort(efforts []string, want string) bool {
	for _, e := range efforts {
		if strings.EqualFold(strings.TrimSpace(e), want) {
			return true
		}
	}
	return false
}
