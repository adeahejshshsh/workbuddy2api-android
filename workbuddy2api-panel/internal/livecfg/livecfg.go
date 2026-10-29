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
	APIKey               string        // 网关/面板共同鉴权密钥；空 = 不鉴权
	SoftCooldown         time.Duration // 429 软冷却基数（<=0 时调用方回退内置默认）
	SanitizeFingerprints bool          // 出站请求体指纹脱敏
	RecordClientInfo     bool          // 请求日志是否记录调用来源（客户端 IP / UA）
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
	out := make(map[string]struct{}, len(hidden)*2)
	for _, m := range hidden {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if prefix, bare, ok := strings.Cut(m, ":"); ok && (prefix == "cn" || prefix == "global") {
			out[prefix+":"+bare] = struct{}{}
		} else {
			out["cn:"+m] = struct{}{}
			out["global:"+m] = struct{}{}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Store 整体替换快照。
func (h *Holder) Store(s Snapshot) { h.p.Store(&s) }

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
