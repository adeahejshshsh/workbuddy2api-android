// effort_catalog.go 推理档位（reasoning effort）产品级静态兜底表。
//
// 数据来源三级（吸收参考仓库 reconcileWithFallback/buddy-adapter.ts:499-523 语义）：
//   - 远端 FetchModels / global 探测已解析的 supportedEfforts/defaultEffort 桶（权威，优先）；
//   - 本文件按 realm 分开的产品级静态兜底表（远端缺失时补齐）；
//   - 两者皆无 → 不在 /v1/models 输出 effort 字段（omitted，不是空数组）。
//
// 档位值照抄参考仓库 dsh-codearts：CN/CodeBuddy 面取 src/product.ts CODEBUDDY_FALLBACK_MODELS
// ∪ src/buddy-adapter.ts:126-137 REASONING_EFFORTS；global/WorkBuddy 面取 src/product.ts
// WORKBUDDY_FALLBACK_MODELS。两个 realm 对同一模型给出不同档位（如 deepseek-v4.1-flash：
// CN 三档 ['low','high','max']、global 仅 ['high']），故**按 realm 分表**，绝不混用。
//
// 只列**可枚举**档位的模型；仅有固定默认档（glm-5.1/kimi-* 的 medium）同样入表，
// 但其档位是「可枚举单档」而非「无选择器」，照抄参考仓库如实暴露。
package upstream

// effortCap 一个模型的档位能力（对齐产品兜底表条目 reasoningEfforts + defaultReasoningEffort）。
type effortCap struct {
	efforts       []string
	defaultEffort string
}

// cnEffortFallback CN / CodeBuddy 面静态兜底表。
// 三档模型 defaultEffort 均为 high（product.ts CODEBUDDY_FALLBACK_MODELS 逐条 defaultReasoningEffort）。
//
// 2026-10-21 国区 5.8.3 GUI 真机抓包核对（带鉴权 /v3/config，62 个模型）：
// deepseek-v4.1-flash=low,high,max、glm-5.3=low,high,max、glm-5.2=high,xhigh、
// hy4-preview=high、hy3=low,high 与本表一致；但 deepseek-v4-flash 实为 high,xhigh、
// deepseek-v4-pro 实为 high,xhigh、kimi-k3-1 实为 low,high,xhigh —— 已按实测改正
// （此前照 product.ts 抄的 low,high,max / low,high,xhigh / medium 与上游目录不符，
// 远程桶缺失时会按错档位降级）。
var cnEffortFallback = map[string]effortCap{
	"deepseek-v4-flash":   {efforts: []string{"high", "xhigh"}, defaultEffort: "high"},
	"deepseek-v4.1-flash": {efforts: []string{"low", "high", "max"}, defaultEffort: "high"},
	"deepseek-v4-pro":     {efforts: []string{"high", "xhigh"}, defaultEffort: "high"},
	"hy4-preview":         {efforts: []string{"high"}, defaultEffort: "high"},
	"hy4-preview-x":       {efforts: []string{"high"}},
	"hy3":                 {efforts: []string{"low", "high"}, defaultEffort: "high"},
	"hy3-x":               {efforts: []string{"low", "high"}, defaultEffort: "high"},
	"glm-5.3":             {efforts: []string{"low", "high", "max"}, defaultEffort: "high"},
	"glm-5.3-flash":       {efforts: []string{"low", "high", "max"}, defaultEffort: "high"},
	"glm-5.2":             {efforts: []string{"high", "xhigh"}, defaultEffort: "high"},
	"glm-5.1":             {efforts: []string{"medium"}},
	"glm-5v-turbo":        {efforts: []string{"medium"}},
	"kimi-k3-1":           {efforts: []string{"low", "high", "xhigh"}},
	"kimi-k2.7":           {efforts: []string{"medium"}},
	"kimi-k2.6":           {efforts: []string{"medium"}},
	"minimax-m3":          {efforts: []string{"medium"}},
}

// globalEffortFallback global / WorkBuddy 国际版面静态兜底表。
//
// 2026-10-29 WorkBuddy AI 5.9.3 真机核对（带鉴权 GET/POST https://www.workbuddy.ai/v3/config，
// data.models 25 条，逐条抄 reasoning.supportedEfforts / reasoning.defaultEffort /
// reasoning.effort）。上一版按 5.8.4 product.ts 与 IDE 缓存抄写，已与实际目录脱节：
//
//   - **已下架**（目录里没有，留着只会给出不存在的档位）：gpt-5.5、gpt-5.4、gpt-5.3-codex、
//     hy4-preview-f、gemini-3.5-flash、kimi-k2.6、glm-5.2；GL 目录里 hy4-preview 才是那个
//     单档 "high" 的混元模型（-f 是 CN 侧变体命名）。
//   - **新上架/此前缺失**（远端桶取不到时无档位可报）：glm-5.3-flash、kimi-k3.1-pro、
//     kimi-k3.1-pro-highspeed、minimax-m3.1-flash、gpt-6-sol、gpt-6-luna、gpt-6.1-sol、
//     gemini-3.8-flash、grok-4.7、hy4-preview、deepseek-v4.1-flash-sg。
//   - **deepseek-v4.1-flash 国际版是三档**：旧注释写「国际版只有 high，发 low/max 会 400」，
//     依据是 5.8.4 的 IDE 缓存；5.9.3 目录已明确下发 supportedEfforts=[low,high,max]。
//
// 「单档」形态照旧写成唯一元素的可枚举档（上游用老的 reasoning.effort 键而非数组：
// fast-model=medium、balanced-model=medium、primary-model=high、kimi-k3=medium），
// 让 /v1/models 与面板有档位可展示，而不是整段省略。
//
// default-model 与两个媒体模型（hy-image-3.5 出图 / kling-video-v3 出视频）目录里没有
// reasoning 段——不编造，一律不入表。
var globalEffortFallback = map[string]effortCap{
	"fast-model":              {efforts: []string{"medium"}},
	"balanced-model":          {efforts: []string{"medium"}},
	"primary-model":           {efforts: []string{"high"}},
	"hy4-preview":             {efforts: []string{"high"}, defaultEffort: "high"},
	"hy3":                     {efforts: []string{"low", "high"}, defaultEffort: "high"},
	"deepseek-v4.1-flash":     {efforts: []string{"low", "high", "max"}, defaultEffort: "high"},
	"deepseek-v4.1-flash-sg":  {efforts: []string{"low", "high", "max"}, defaultEffort: "high"},
	"glm-5.3":                 {efforts: []string{"low", "high", "max"}, defaultEffort: "high"},
	"glm-5.3-flash":           {efforts: []string{"low", "high", "max"}, defaultEffort: "high"},
	"kimi-k3":                 {efforts: []string{"medium"}},
	"kimi-k3.1-pro":           {efforts: []string{"low", "high", "max"}, defaultEffort: "high"},
	"kimi-k3.1-pro-highspeed": {efforts: []string{"low", "high", "max"}, defaultEffort: "high"},
	"gpt-5.6-sol":             {efforts: []string{"low", "medium", "high", "xhigh", "max"}, defaultEffort: "high"},
	"gpt-5.6-terra":           {efforts: []string{"low", "medium", "high", "xhigh", "max"}, defaultEffort: "high"},
	"gpt-5.6-luna":            {efforts: []string{"low", "medium", "high", "xhigh", "max"}, defaultEffort: "high"},
	"gpt-6-astra":             {efforts: []string{"low", "medium", "high", "xhigh", "max"}, defaultEffort: "high"},
	"gpt-6-sol":               {efforts: []string{"low", "medium", "high", "xhigh"}, defaultEffort: "high"},
	"gpt-6-luna":              {efforts: []string{"low", "medium", "high", "xhigh"}, defaultEffort: "high"},
	"gpt-6.1-sol":             {efforts: []string{"low", "medium", "high", "xhigh"}, defaultEffort: "high"},
	"grok-4.7":                {efforts: []string{"low", "medium", "high", "xhigh"}, defaultEffort: "high"},
	"gemini-3.8-flash":        {efforts: []string{"low", "medium", "high"}, defaultEffort: "medium"},
	"minimax-m3.1-flash":      {efforts: []string{"low", "medium", "high", "xhigh", "max"}, defaultEffort: "max"},
}

// staticEffortCap 按 realm 取静态兜底条目；未命中返回 zero effortCap（efforts=nil）。
// realm 经 realmKey 归一化（空 → "cn"），与 efforts 缓存桶同口径。
func staticEffortCap(realm, model string) effortCap {
	table := cnEffortFallback
	if realmKey(realm) == "global" {
		table = globalEffortFallback
	}
	return table[model]
}

// EffortListing 计算模型在 /v1/models 应暴露的 effort 能力（三级查找 + 默认档防御）。
//
// remoteEfforts/remoteDefault 为远端（FetchModels / global 探测）已解析值；
// remoteEfforts 非空时以其为权威（不回落到静态表），否则落到产品级静态兜底表；
// 两者皆无 → efforts 返回 nil（调用方省略字段，不输出空数组）。
//
// defaultEffort 仅在「efforts 非空且 default 命中 efforts」时才返回
// （对齐参考仓库 resolveModel 的 `defaultEffort ∈ efforts` 防御：不宣称不支持的默认档）。
// remoteDefault 空串不回落到静态默认档——默认档随档位表同源：remote 有档位就用 remote 默认档，
// 静态兜底档位就用静态默认档，避免跨源拼接出「档位是静态、默认档是 remote」的矛盾组合。
func EffortListing(realm, model string, remoteEfforts []string, remoteDefault string) (efforts []string, defaultEffort string) {
	var src effortCap
	switch {
	case len(remoteEfforts) > 0:
		src = effortCap{efforts: remoteEfforts, defaultEffort: remoteDefault}
	default:
		src = staticEffortCap(realm, model)
	}
	if len(src.efforts) == 0 {
		return nil, ""
	}
	efforts = append([]string(nil), src.efforts...)
	if src.defaultEffort != "" && containsEffort(efforts, src.defaultEffort) {
		defaultEffort = src.defaultEffort
	}
	return efforts, defaultEffort
}

// containsEffort 档位成员判定（精确匹配，对齐参考仓库 `efforts.includes(defaultEffort)`）。
func containsEffort(efforts []string, want string) bool {
	for _, e := range efforts {
		if e == want {
			return true
		}
	}
	return false
}

// globalEffortMap global 域降级用的 effort 能力表：静态兜底表为基，远端桶覆盖（权威优先）。
//
// prepareBody 对 global 请求调此函数（而非直接用远端桶），因为 global 上游可能不下发
// supportedEfforts——此时也必须按产品静态表降级（issue #84：deepseek-v4.1-flash 国际版
// 只认 high，客户端传 low/max 必降级到 high，否则上游 400 毁掉请求）。
// 语义对齐参考仓库 effortsFor（remoteMeta → productFallback → 静态表），只取前两级：
// 远端桶（探测已解析）→ 本产品静态表（本文件），缺档位即无（不再到通用静态表）。
func globalEffortMap(remoteEfforts map[string][]string, remoteDefaults map[string]string) (map[string][]string, map[string]string) {
	return effortFallbackMap(globalEffortFallback, remoteEfforts, remoteDefaults)
}

// cnEffortMap CN 域降级/补档用的 effort 能力表：与 globalEffortMap 同口径
// （静态兜底为基，远端桶覆盖）。prepareBody 对 cn 请求调此函数，使请求侧解析
// 与 /v1/models 的 EffortListing 声明同源——声明什么档位就按什么档位落地。
func cnEffortMap(remoteEfforts map[string][]string, remoteDefaults map[string]string) (map[string][]string, map[string]string) {
	return effortFallbackMap(cnEffortFallback, remoteEfforts, remoteDefaults)
}

// effortFallbackMap 通用合并：静态兜底表为基，远端桶/默认档覆盖（仅非空项）。
func effortFallbackMap(table map[string]effortCap, remoteEfforts map[string][]string, remoteDefaults map[string]string) (map[string][]string, map[string]string) {
	efforts := make(map[string][]string, len(table)+len(remoteEfforts))
	defs := make(map[string]string, len(table)+len(remoteDefaults))
	for id, cap := range table {
		efforts[id] = append([]string(nil), cap.efforts...)
		if cap.defaultEffort != "" {
			defs[id] = cap.defaultEffort
		}
	}
	for id, v := range remoteEfforts {
		if len(v) > 0 {
			efforts[id] = v
		}
	}
	for id, v := range remoteDefaults {
		if v != "" {
			defs[id] = v
		}
	}
	return efforts, defs
}
