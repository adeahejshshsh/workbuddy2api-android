// clientpick.go 「客户端 picker 口径」：/v3/config 的 data.agents 里那个默认 agent
// 的 models 数组，就是官方桌面客户端模型菜单的数据源。
//
// 为什么必须按它裁剪（2026-10-29 真机实测，5.9.3 两域）：
//
//	CN  www.workbuddy.cn  data.models 58 条，data.agents[cli].models 16 条
//	INTL www.workbuddy.ai data.models 25 条，data.agents[cli].models 23 条
//
// data.models 是**平台全量目录**（含 codewise-* 补全模型、hy3-b/hy3-c/hy3-x 这类
// 路由变体、default-1.1/1.2 这类历史别名、以及已被 disabled 的 wb-lite）。客户端
// 从不把它们全画出来：菜单项只取 agents[].models，再叠三条客户端自己的规则。
// 按 data.models 照单全列（本网关此前的行为）等于把一堆客户端根本不显示、点了还会
// 11102 的内部 id 暴露给下游——用户侧表现为「模型列表和官方客户端对不上」。
//
// 客户端侧的三条规则（渲染层源码实测，见下）：
//
//  1. 只列默认 agent 的 models（resolveConversationAgent：tags 含 default → name==cli
//     → 首个非空 models）。CN 实测菜单 17 项 = agents[cli].models 16 条 − auto −
//     hy3-x + 快速/均衡/极致 三个预设，逐项对得上。
//  2. ModelRateLimitCap.lines[].paidId 不单列：客户端把它并进 freeId 那一行，免费额度
//     撞顶后再切过去（filterVisiblePaidLineModels）。CN 是 hy3 → hy3-x；
//     INTL 是 deepseek-v4.1-flash → deepseek-v4.1-flash-sg。
//  3. 快速/均衡/极致（fast-model / balanced-model / deep-model）是客户端的三个速度
//     预设，即使不在 agents[].models 里也照样出现在菜单顶部；而 auto / auto-chat
//     是「自动模式」开关本身（isAutoModelId 集合），不是菜单行。
//
// 口径缺失即不裁剪（保留上游全量）：上游没下发 agents 时（老版本、企业端点、
// 私有化部署）我们无从得知客户端会显示什么，此时宁可照旧全列也不臆造白名单。
package upstream

import "strings"

// autoModeIDs 客户端当作「自动模式」开关、不画成菜单行的 id
// （渲染层 isAutoModelId 集合去掉三个速度预设后的余集）。
//
// CN 的 agents[cli].models 里带着 auto，但它不是菜单项：点了「自动」是切路由模式，
// 由 isAutoModelId 单独处理。企业端点探测（fetchEnterpriseModels）与 picker 口径
// 共用这一份判据——两边各写一个字符串常量，将来上游加 auto-lite 之类必然漏一处。

// autoModeIDs / isAutoModeID 见 client.go（企业端点探测也在用，单点定义）。

// pickPresetIDs 客户端的三个速度预设（菜单顶部「快速 / 均衡 / 极致」）。
// 顺序即客户端顺序；不在目录里的跳过（INTL 无 deep-model）。
var pickPresetIDs = []string{"fast-model", "balanced-model", "deep-model"}

// v3ConfigAgent /v3/config data.agents 单条。只有 name/models/tags 参与可见性判定。
type v3ConfigAgent struct {
	Name   string   `json:"name"`
	Models []string `json:"models"`
	Tags   []string `json:"tags"`
}

// v3RateLimitLine productFeaturesConfig.ModelRateLimitCap.lines 单条：
// 免费额度撞顶后客户端从 freeId 切到 paidId，两个 id 在菜单里只占一行。
type v3RateLimitLine struct {
	FreeID string `json:"freeId"`
	PaidID string `json:"paidId"`
}

// clientPick 一次 /v3/config 响应推导出的「客户端菜单可见集合」。
// nil 表示上游没给 agents（无从判定），调用方按既有全量口径处理。
type clientPick struct {
	order []string        // 默认 agent 的 models 原序
	paid  map[string]bool // ModelRateLimitCap 的 paidId 集合（不单列）
}

// newClientPick 从 agents + rate-limit lines 推导可见性；没有可用 agents 时返回 nil。
func newClientPick(agents []v3ConfigAgent, lines []v3RateLimitLine) *clientPick {
	ag := defaultPickerAgent(agents)
	if ag == nil || len(ag.Models) == 0 {
		return nil
	}
	p := &clientPick{order: make([]string, 0, len(ag.Models)), paid: map[string]bool{}}
	for _, id := range ag.Models {
		if s := strings.TrimSpace(id); s != "" {
			p.order = append(p.order, s)
		}
	}
	for _, ln := range lines {
		if s := strings.TrimSpace(ln.PaidID); s != "" {
			p.paid[s] = true
		}
	}
	if len(p.order) == 0 {
		return nil
	}
	return p
}

// defaultPickerAgent 客户端选默认 agent 的同一条优先级：tags 含 default → name==cli
// → 第一个 models 非空的。三者都没有时返回 nil（口径缺失，调用方不裁剪）。
func defaultPickerAgent(agents []v3ConfigAgent) *v3ConfigAgent {
	for i := range agents {
		for _, t := range agents[i].Tags {
			if strings.EqualFold(strings.TrimSpace(t), "default") {
				return &agents[i]
			}
		}
	}
	for i := range agents {
		if strings.EqualFold(strings.TrimSpace(agents[i].Name), "cli") {
			return &agents[i]
		}
	}
	for i := range agents {
		if len(agents[i].Models) > 0 {
			return &agents[i]
		}
	}
	return nil
}

// allows 报告 id 是否属于客户端菜单会显示的那一批。p 为 nil → 恒 true
// （口径缺失不裁剪）。
func (p *clientPick) allows(id string) bool {
	if p == nil {
		return true
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	if isAutoModeID(id) || p.paid[id] {
		return false
	}
	for _, o := range p.order {
		if o == id {
			return true
		}
	}
	// 速度预设不在 agent 名单里也可能被菜单列出（CN 的 快速/均衡/极致 即如此）。
	for _, o := range pickPresetIDs {
		if o == id {
			return true
		}
	}
	return false
}

// ordered 按客户端菜单顺序返回可见 id，只保留 byID 里真实存在的条目
// ——目录缺条的 id 不臆造。p 为 nil → 返回 nil（调用方走既有全量分支）。
//
// 顺序规则（两域真机菜单实测，2026-10-29）：
//
//	菜单 = [不在 agent 名单里的速度预设，按 快速/均衡/极致] + [agent 原序过滤后]
//
// CN：agents 里没有 fast/balanced/deep，故三个预设被前置，得 快速/均衡/极致/…
// INTL：agents 本身就含 fast-model/balanced-model（default-model 打头），
//
//	预设已在名单内不再前置，结果 = 纯 agent 原序。两条真机菜单都逐条对上。
//
// 曾经写成「预设一律前置」：INTL 菜单首项是 default-model（Auto），前置会把
// Fast/Balanced 顶到 Auto 前面，顺序与客户端不符（id 集合对、顺序错）。
func (p *clientPick) ordered(byID map[string]ModelInfo) []string {
	if p == nil {
		return nil
	}
	inOrder := make(map[string]bool, len(p.order))
	for _, id := range p.order {
		inOrder[id] = true
	}
	seen := make(map[string]bool, len(p.order)+len(pickPresetIDs))
	out := make([]string, 0, len(p.order)+len(pickPresetIDs))
	add := func(id string) {
		if seen[id] || !p.allows(id) {
			return
		}
		if _, ok := byID[id]; !ok {
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	for _, id := range pickPresetIDs {
		if !inOrder[id] {
			add(id)
		}
	}
	for _, id := range p.order {
		add(id)
	}
	return out
}
