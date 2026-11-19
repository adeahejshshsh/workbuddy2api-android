// Package livecfg 运行期可变配置的并发安全持有者。
//
// 背景：进程启动时读入的配置是普通字段（读多写零），但管理面板允许在线改配置，
// 于是少量"可热生效"的字段需要有并发安全的读写点。此处用不可变快照 + atomic 指针：
// 读方 Load 拿到一致视图，写方 Store 整体替换，无锁无数据竞争。
//
// 只承载**读路径深、热改需求强**的少数字段；池参数/排程参数等各有既有 setter
// （pool.SetBreaker、scheduler.Reconfigure 等），不重复收编到这里。
package livecfg

import (
	"strings"
	"sync/atomic"
	"time"
)

// Snapshot 一次读取的不可变配置视图。
type Snapshot struct {
	APIKey string // 网关/面板共同鉴权密钥；空 = 不鉴权
	// AdminAPIKey 面板专用门禁（admin_api_key）：与发给下游的 APIKey 分开，
	// 避免「客户端 key 即全池管理员」。面板敏感接口要求它非空（见 panel/panel.go）。
	AdminAPIKey          string
	SoftCooldown         time.Duration // 429 软冷却基数（<=0 时调用方回退内置默认）
	SanitizeFingerprints bool          // 出站请求体指纹脱敏
	RecordClientInfo     bool          // 请求日志是否记录调用来源（客户端 IP / UA）
	// WatermarkStripEnabled 去水印（裁白边）端点与 MCP 工具的总开关
	// （features.watermark_strip_enabled，缺省 false）。热生效。
	WatermarkStripEnabled bool
	// EmbeddingsEnabled 实验特性 /v1/embeddings 总开关（features.embeddings_enabled，
	// 缺省 false）。热生效：关着时端点回 403 embeddings_disabled——不是 404，
	// 「功能存在、只是没开」与「上游没有这个协议」必须能区分开。
	EmbeddingsEnabled bool
	// ThreeDEnabled 混元 3D 生成总开关（features.three_d_enabled）；默认 false。
	// 关着时端点回 403 three_d_disabled，不碰上游、不签发凭证。
	ThreeDEnabled bool
	// ToolOnlyRescue 「说一句就收尾」的安全续写（只采纳工具调用）。
	// 见 internal/server/toolonly.go：features.tool_only_rescue，**缺省关闭**——
	// 判据只能看「末条是工具结果 + 正文短」，而 agent 循环里正常收尾也长这样，
	// 线上实测会把正常结尾当成截断、白打一次上游。需要时必须显式打开。
	ToolOnlyRescue bool
	// NarrationOff 预告式收尾救援是否被关闭（features.narration_enabled 取反）。
	//
	// 取反是为了让零值 = 开启：没接 livecfg 的裸用/测试路径不必初始化就保持既有行为。
	// 为什么终于给了开关（issue #273 第二条评论）：这条救援是往对话里插一条 user 消息
	// 再打一次上游，agent 类客户端分辨不出它与真实用户输入的区别 —— 实测被当成用户
	// 授权执行了 9 分钟工具调用，且客户端历史里完全查不到这条。
	NarrationOff bool
	// RecordContext 请求事件是否记录会话上下文（system + messages / 出图 prompt）。
	// 比 IP/UA 敏感得多（是用户的实际对话内容），所以单独一个开关、缺省关闭。
	RecordContext bool
	// ModelFallback 模型降级路由表（pool.model_fallback）：6004 模型级限流时改用
	// 的等价模型（裸名 → 裸名）。nil = 不降级，且不写持久状态。
	ModelFallback map[string]string
	// ModelAliases 客户端名 → 真实模型名（键已归一化为小写 "realm:裸名"；
	// 裸名配置同时登记 cn/global 两个域）。nil = 不映射。issue #30。
	ModelAliases map[string]string
	// HiddenModels models.hidden 的归一化集合（键 "realm:裸名"）。面板「下游可见」
	// 开关据此显示当前状态；nil = 不隐藏任何模型。
	HiddenModels map[string]struct{}
	// Thinking* 网关思考档位策略（config thinking 节）的只读镜像。出站改写由
	// upstream.Client 自己持有 EffortPolicy；这里只给面板「网关档位」列算**当前生效值**，
	// 让模型列表与真实注入口径一致（含按模型覆盖 model_defaults 的域精确键）。
	ThinkingEnabled        bool
	ThinkingDefaultEffort  string
	ThinkingClientPriority bool
	ThinkingOverrideScope  string
	ThinkingModelDefaults  map[string]string
}

// Holder 原子持有当前快照。
type Holder struct {
	p atomic.Pointer[Snapshot]
}

// New 以初始快照构建。
func New(s Snapshot) *Holder {
	h := &Holder{}
	h.Store(s)
	return h
}

// Load 返回当前快照（Holder 为 nil 或从未 Store 时返回零值快照，调用方无需判空）。
func (h *Holder) Load() Snapshot {
	if h == nil {
		return Snapshot{}
	}
	if s := h.p.Load(); s != nil {
		return *s
	}
	return Snapshot{}
}

// NormalizeHiddenModels 把用户配置的 hidden 条目归一化为精确 id 集合。
//
// 条目带合法 "cn:"/"global:" 前缀 → 只隐藏该域；裸名 → 两域都隐藏。
// 返回的 map 只读共享，调用方不得修改。
func NormalizeHiddenModels(hidden []string) map[string]struct{} {
	if len(hidden) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(hidden)*4)
	// add 同时登记「配置里的原样拼写」与「小写拼写」。
	//
	// 为什么要双写（审查：热快照口径不一致）：本表的消费方有两套口径 ——
	// 面板用上游返回的**精确 id**（"cn:"+model.ID）查表，而服务端 modelHidden
	// 用 strings.EqualFold 判定（大小写不敏感）。只登记一种拼写时，配
	// `models.hidden: ["GLM-5.2"]` 会出现「上游确实被拦了、面板却显示未隐藏」。
	add := func(realm, bare string) {
		if bare == "" {
			return
		}
		out[realm+":"+bare] = struct{}{}
		if lower := strings.ToLower(bare); lower != bare {
			out[realm+":"+lower] = struct{}{}
		}
	}
	for _, m := range hidden {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if realm, bare, ok := splitHiddenRealm(m); ok {
			add(realm, strings.TrimSpace(bare))
			continue
		}
		add("cn", m)
		add("global", m)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// splitHiddenRealm 识别 "cn:" / "global:" 前缀（**大小写不敏感**）。
//
// 审查（热快照口径）：此前用 `prefix == "cn" || prefix == "global"` 精确比较，
// 写 `CN:glm-5.2` 会被当成**裸模型名** → 登记成 "cn:CN:glm-5.2" 与
// "global:CN:glm-5.2"，既藏错域又永远匹配不上任何模型 id；服务端侧
// splitRealmPrefixFold 早就忽略大小写，两处口径必须一致。
//
// 只认这两个合法前缀：模型名本身可能含冒号（如 `qwen:7b`），不能拿
// strings.Contains 当判据（服务端 modelHidden 的注释记录过这个坑）。
func splitHiddenRealm(m string) (realm, bare string, ok bool) {
	for _, r := range []string{"cn:", "global:"} {
		if len(m) >= len(r) && strings.EqualFold(m[:len(r)], r) {
			return r[:len(r)-1], m[len(r):], true
		}
	}
	return "", m, false
}

// Store 整体替换快照。
//
// 审查（热快照口径）：快照承诺「不可变」，但 map 字段此前与调用方**共享引用** ——
// 装配/保存路径后续改写同一个 map 时，读方会看到半更新的视图（比如别名表只换
// 了一半的键）。这里在写入时克隆一份，让快照真正自洽。克隆只在保存配置时发生，
// 不在请求热路径上。
func (h *Holder) Store(s Snapshot) {
	s.ModelFallback = cloneStringMap(s.ModelFallback)
	s.ModelAliases = cloneStringMap(s.ModelAliases)
	s.ThinkingModelDefaults = cloneStringMap(s.ThinkingModelDefaults)
	if s.HiddenModels != nil {
		m := make(map[string]struct{}, len(s.HiddenModels))
		for k := range s.HiddenModels {
			m[k] = struct{}{}
		}
		s.HiddenModels = m
	}
	h.p.Store(&s)
}

// cloneStringMap 复制 map；nil 保持 nil（调用方用 nil 判定「没配」）。
func cloneStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// NormalizeModelAliases 把 "客户端名=真实名" 条目归一化成查找表。
//
//   - 按第一个 "=" 拆键值，两端 Trim，键转小写（模型名大小写不敏感）；
//   - 键带合法 realm 前缀（cn:/global:）→ 只登记该域；裸名 → 两域都登记；
//   - 无 "="、空键、空值、键==值 → 丢弃（自我映射还可能成环）。
func NormalizeModelAliases(entries []string) map[string]string {
	if len(entries) == 0 {
		return nil
	}
	out := make(map[string]string, len(entries))
	for _, raw := range entries {
		k, v, ok := strings.Cut(raw, "=")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		if k == "" || v == "" || k == strings.ToLower(v) {
			continue
		}
		if realm, bare, has := splitAliasRealm(k); has {
			out[realm+":"+bare] = v
			continue
		}
		out["cn:"+k] = v
		out["global:"+k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// splitAliasRealm 拆 "cn:x" / "global:x" 前缀；非合法前缀返回 has=false。
func splitAliasRealm(k string) (realm, bare string, has bool) {
	for _, r := range []string{"cn:", "global:"} {
		if strings.HasPrefix(k, r) {
			return r[:len(r)-1], k[len(r):], true
		}
	}
	return "", k, false
}
