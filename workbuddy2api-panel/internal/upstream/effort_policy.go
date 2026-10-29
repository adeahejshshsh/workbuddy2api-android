// effort_policy.go 网关侧思考档位策略（issue #18 / #21 的网关优先级一层）。
//
// 语义（config thinking 节；经 Client.SetEffortPolicy 热更新）：
//   - Enabled=false：网关完全不碰档位，行为退回只有 injectThinking + 降级的旧管线；
//   - ClientPriority=true：客户端发了档位就按客户端（含 off/关闭），只在客户端
//     **没发**档位时补网关默认档；
//   - ClientPriority=false：网关优先。客户端显式档位一律改写为网关目标档；
//     OverrideScope=level 时保留客户端「关闭思考」（off/none/disabled 或
//     thinking.type=disabled）的表达，=all 时连关闭也一并改写为开启 + 目标档。
//
// 目标档解析（ResolveGatewayEffort）：default_effort=max 取模型支持的最高档；
// 具体档位支持则用、不支持则取 ≤ 该档的最高支持档（没有则用最低支持档），
// 与 /v1/models 声明的 supportedEfforts 同源于 EffortListing。
//
// 目标档也可以是 off（面板「网关档位」下拉里与 supportedEfforts 一起原样列出，
// 仅 canDisableThinking 的模型才有）：表示该模型在网关侧固定关闭思考。客户端
// 显式发档位且非 all 作用域时仍保留客户端档位（off 改开关，不越权覆盖 level 的约定）。
package upstream

import (
	"log"
	"strings"
)

const (
	// OverrideScopeLevel 网关优先只覆盖「档位」，尊重客户端的关闭意图。
	OverrideScopeLevel = "level"
	// OverrideScopeAll 网关优先覆盖档位与开关：客户端 off/disabled 也会被改写成开启。
	OverrideScopeAll = "all"
	// EffortOff 目标档为「关闭思考」：与 supportedEfforts 同列的下拉选项之一，
	// 由网关在允许改写的时机写成 thinking.type=disabled（并删掉档位字段）。
	EffortOff = "off"
)

// isOffEffort 档位字段里表达「关闭思考」的写法（与 effortValueOff 同口径）。
func isOffEffort(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "off", "none", "disabled":
		return true
	}
	return false
}

// EffortPolicy 网关思考档位策略（config thinking 节的运行时形态）。
//
// 字段均带 json tag：面板保存后按 config 原样落盘/回显，热更新走同一个结构。
type EffortPolicy struct {
	Enabled        bool   `json:"enabled"`
	DefaultEffort  string `json:"default_effort"`
	ClientPriority bool   `json:"client_priority"`
	OverrideScope  string `json:"override_scope"`
	// ModelDefaults 按模型覆盖默认档：键 "realm:model"（精确到域）或裸 "model"
	// （两域共用），值 "max"/"high"/…；空或 "inherit" 表示不覆盖（解析时已剔除）。
	ModelDefaults map[string]string `json:"model_defaults,omitempty"`
}

// Normalized 返回规范化副本（默认值补全 + 键值小写去空白）。
// Client.SetEffortPolicy 存的是规范化结果，热路径不再做字符串处理。
func (p EffortPolicy) Normalized() EffortPolicy {
	p.DefaultEffort = strings.ToLower(strings.TrimSpace(p.DefaultEffort))
	if p.DefaultEffort == "" {
		p.DefaultEffort = "max"
	}
	p.OverrideScope = strings.ToLower(strings.TrimSpace(p.OverrideScope))
	if p.OverrideScope != OverrideScopeAll {
		p.OverrideScope = OverrideScopeLevel
	}
	out := make(map[string]string, len(p.ModelDefaults))
	for k, v := range p.ModelDefaults {
		key := strings.ToLower(strings.TrimSpace(k))
		val := strings.ToLower(strings.TrimSpace(v))
		if key == "" || val == "" || val == "inherit" {
			continue
		}
		out[key] = val
	}
	p.ModelDefaults = out
	return p
}

// DefaultEffortFor 返回该模型应使用的网关目标档：先查 "realm:model"，再查裸
// "model"，都没有则用全局 default_effort。返回值为未解析的愿望档（如 "max"），
// 由 ResolveGatewayEffort 落到具体支持档。
func (p EffortPolicy) DefaultEffortFor(realm, model string) string {
	if model == "" {
		return p.DefaultEffort
	}
	model = strings.ToLower(strings.TrimSpace(model))
	if v, ok := p.ModelDefaults[realmKey(realm)+":"+model]; ok {
		return v
	}
	if v, ok := p.ModelDefaults[model]; ok {
		return v
	}
	return p.DefaultEffort
}

// IsEffortLevel 报告 s 是否是可声明的思考档位（off 也算：它是「关闭」表达，
// config 侧单独拒绝，其它场景用于区分档位与任意字符串）。
func IsEffortLevel(s string) bool {
	_, ok := effortRank[strings.ToLower(strings.TrimSpace(s))]
	return ok
}

// ResolveGatewayEffort 把愿望档（"max" / "high" / …）解析成模型实际支持的档位：
//   - off/none/disabled → "off"（关闭思考，调用方转到 thinking 开关处理）；
//   - ""/"max" → 支持列表中的最高档（列表里没有可识别档位时取最后一项）；
//   - 具体档位 → 支持则原样；不支持取 ≤ 该档的最高支持档；没有更低的则取最低档；
//   - 无法识别的愿望档 / 空支持列表 → ""（调用方不改写）。
func ResolveGatewayEffort(want string, supported []string) string {
	want = strings.ToLower(strings.TrimSpace(want))
	if isOffEffort(want) {
		return EffortOff
	}
	if len(supported) == 0 {
		return ""
	}
	if want == "" || want == "max" {
		best, bestRank := "", -1
		for _, s := range supported {
			if idx, ok := effortRank[strings.ToLower(strings.TrimSpace(s))]; ok && idx > bestRank {
				best, bestRank = s, idx
			}
		}
		if best != "" {
			return best
		}
		return supported[len(supported)-1]
	}
	wantRank, known := effortRank[want]
	if !known {
		return ""
	}
	best, bestRank, lowest, lowestRank := "", -1, "", 1<<30
	for _, s := range supported {
		idx, ok := effortRank[strings.ToLower(strings.TrimSpace(s))]
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(s), want) {
			return s
		}
		if idx < lowestRank {
			lowest, lowestRank = s, idx
		}
		if idx <= wantRank && idx > bestRank {
			best, bestRank = s, idx
		}
	}
	if best != "" {
		return best
	}
	return lowest
}

// applyEffortPolicy 在 injectThinking 之前执行档位策略，返回本次由网关写入的
// 目标档（"" = 未改写，后续仍走既有 injectThinking/normalize 管线）。
//
// 必须在 injectThinking 之前：作用域 all 要把「关闭」改写成开启 + 带档位，
// 晚于 injectThinking 会被写成 thinking.type=disabled 且删掉档位。
func applyEffortPolicy(obj map[string]any, model string, supported []string, policy *EffortPolicy, realm string) string {
	if policy == nil || !policy.Enabled || model == "" {
		return ""
	}
	target := ResolveGatewayEffort(policy.DefaultEffortFor(realm, model), supported)
	if target == "" {
		return ""
	}
	// 「关闭思考」在两种写法里都算客户端显式意图：档位 off/none/disabled，
	// 或 thinking.type=disabled。作用域 level / 客户端优先时一律尊重。
	if effortValueOff(obj) || thinkingDisabled(obj) {
		if policy.ClientPriority || policy.OverrideScope != OverrideScopeAll || target == EffortOff {
			return ""
		}
		stripEffort(obj)
		if th, ok := obj["thinking"].(map[string]any); ok {
			th["type"] = "enabled"
		}
		obj["reasoning_effort"] = target
		log.Printf("thinking policy: 网关优先（scope=all）改写关闭意图 model=%s -> %s", model, target)
		return target
	}
	key, value := effortField(obj)
	if key == "" {
		// 客户端没发档位：补网关默认档（客户端优先模式下同样补——补的是缺失值，
		// 不是覆盖客户端意图）。目标档为 off 时补「关闭思考」。
		if target == EffortOff {
			disableThinking(obj)
			log.Printf("thinking policy: 补网关默认档 model=%s -> off（关闭思考）", model)
			return EffortOff
		}
		obj["reasoning_effort"] = target
		return target
	}
	if policy.ClientPriority {
		return ""
	}
	if target == EffortOff {
		// 目标 off 要改 thinking 开关，scope=level 的约定是「只改档位」，所以 level
		// 下保留客户端显式档位；scope=all 才连开关一起固定成关闭。
		if policy.OverrideScope != OverrideScopeAll {
			return ""
		}
		disableThinking(obj)
		log.Printf("thinking policy: 网关优先（scope=all）目标 off model=%s 改写关闭思考（client=%s）", model, value)
		return EffortOff
	}
	if !strings.EqualFold(strings.TrimSpace(value), target) {
		log.Printf("thinking policy: 网关优先改写 model=%s %s -> %s（client_priority=false）", model, value, target)
	}
	// 两个字段同时出现时只保留客户端实际生效的那个，避免旧值残留在另一字段
	// 被 injectThinking 的 off 判定命中、把请求改成关闭思考。
	stripEffort(obj)
	obj[key] = target
	return target
}

// disableThinking 把请求体写成「关闭思考」形态：删掉档位字段（snake/camel 双删），
// thinking.type=disabled。deepseek 的 injectThinking 认这个形态；其它上游对
// thinking.type 的接受度由上游决定（面板只给 canDisableThinking 的模型提供 off 选项）。
func disableThinking(obj map[string]any) {
	stripEffort(obj)
	if th, ok := obj["thinking"].(map[string]any); ok {
		th["type"] = "disabled"
		return
	}
	obj["thinking"] = map[string]any{"type": "disabled"}
}

// effortField 返回请求体里生效的档位字段与值（snake 优先，camel 兜底）。
func effortField(obj map[string]any) (string, string) {
	for _, k := range []string{"reasoning_effort", "reasoningEffort"} {
		if v, ok := obj[k].(string); ok {
			return k, v
		}
	}
	return "", ""
}

// thinkingDisabled 报告客户端是否用 thinking.type=disabled 显式关闭思考。
func thinkingDisabled(obj map[string]any) bool {
	th, ok := obj["thinking"].(map[string]any)
	if !ok {
		return false
	}
	typ, _ := th["type"].(string)
	return strings.EqualFold(strings.TrimSpace(typ), "disabled")
}

// stripEffort 删除 snake/camel 两个档位字段。
func stripEffort(obj map[string]any) {
	delete(obj, "reasoning_effort")
	delete(obj, "reasoningEffort")
}

// SetEffortPolicy 热更新网关思考档位策略（config thinking 节保存后调用）。
// nil 表示关闭：管线退回 injectThinking + 降级，行为与未接入策略时完全一致。
func (c *Client) SetEffortPolicy(p *EffortPolicy) { c.effortPolicy.Store(p) }

// EffortPolicy 返回当前生效的策略快照（nil = 未接入/已关闭）。
func (c *Client) EffortPolicy() *EffortPolicy { return c.effortPolicy.Load() }
