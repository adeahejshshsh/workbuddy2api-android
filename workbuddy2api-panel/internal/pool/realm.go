// 分池选号域：按 realm（cn/global）过滤选号与可用集合。realm=="" 退化为现状。
package pool

import (
	"sort"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// expiringVirtualSlots 快过期账号在新会话候选集中的虚拟实例权重。
// 3:1 是温和偏好，不是固定比例：账号组成变化会自然改变最终占比。
const expiringVirtualSlots = 3

// IsAvailableForModel 报告单个账号对该模型是否可选（healthyForModel + 未占满在途）。
//
// 用途（bug #13）：会话粘性的快路径只需要校验"绑定号此刻还能不能用"，
// 而 availableSet 会为了一次命中构建整池集合并排序 —— 账号多、QPS 高时是热路径上的
// 固定开销。这里单点查，复杂度 O(1)。
func (p *Pool) IsAvailableForModel(uid, model string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	now := time.Now()
	return e.healthyForModel(now, model) && !p.inFlightFull(e)
}

// PickExistingAuth 按 uids 顺序返回第一个仍持有凭证的账号，全都不在返回 nil。
//
// 为什么需要（bug P1-2 / B20）：AvailableUIDsForRealm 与 AuthByUID 是两次独立加锁
// 查询，两次之间账号可能被面板移除 / SyncToDir 剔除。各处若只取 uids[0]，取不到就
// 静默放弃该域 —— 表现为 /v1/models 偶发返回空、启动预热静默跳过、日志里零线索。
func (p *Pool) PickExistingAuth(uids []string) *auth.Auth {
	for _, uid := range uids {
		if a := p.AuthByUID(uid); a != nil {
			return a
		}
	}
	return nil
}

// AvailableUIDsForRealm 同 AvailableUIDs，但仅返回 Realm()==realm 的账号。
// realm=="" 退化为 AvailableUIDs（现状语义）。
func (p *Pool) AvailableUIDsForRealm(realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		// 积分保底 / 保留积分（审查 C5-84）：本系列是**生产调用方**（面板 /v1/models、
		// 启动预热、handler 选号对齐）用的「可用集合」。只判 healthy+inFlightFull 时，
		// 触底号会出现在可用集合里、而 pick 永远不选它 —— 两侧口径裂缝的典型症状是
		// 「面板说 N 个可用，请求却报 no_healthy_account」，或会话层绑定后每请求
		// 一次 unbind 抖动。无模型上下文 → 走证据式保底（见 floorBlockedNoModelLocked）。
		if p.floorBlockedNoModelLocked(e, e.a.Realm(), now) {
			continue
		}
		if p.reserveBlocked(e, p.modelFreeOf(e, "")) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// WeightedAvailableUIDsForModelRealm 返回带虚拟实例权重的可用账号列表。
//
// 普通账号出现 1 次；有效快过期账号出现 expiringVirtualSlots 次。调用方继续按
// 原有序列表哈希，即可让新会话对快过期账号形成温和偏好。重复项按 UID 排序后
// 展开，保证同一账号拓扑下不同进程得到一致列表。
//
// 该方法是现有 AvailableUIDsForModelRealm 的增量入口，不改变旧方法语义，也不
// 修改配置、状态或 Redis schema。prefer_expiring=false 时退化为逐账号一次。
func (p *Pool) WeightedAvailableUIDsForModelRealm(model, realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		// 与 pick 同口径（审查 C5-84）：可用集合必须叠加积分保底与保留积分。
		// 这条路径供会话粘性的加权分配（cmd/server/wiring.go）用 —— 少了保底判定，
		// 会话会把流量绑到 pick 侧已停单的号上，每次请求多一次解绑 + 一条 WARN。
		if p.floorBlockedForRealmModel(e, model, e.a.Realm(), now) {
			continue
		}
		if p.reserveBlocked(e, p.modelFreeOf(e, model)) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)

	out := make([]string, 0, len(uids))
	for _, uid := range uids {
		slots := 1
		if p.preferExpiring {
			// 显式判 ok（审查 C5-86）：uid 来自同锁内的遍历，当前恒存在；但
			// expiringNow 直接解引用 e，写法一旦被复用到「可能已删除」的场景就是
			// nil panic。判一下的成本可忽略，换掉一类脆弱写法。
			if e, ok := p.byUID[uid]; ok && expiringNow(e, now) {
				slots = expiringVirtualSlots
			}
		}
		for i := 0; i < slots; i++ {
			out = append(out, uid)
		}
	}
	return out
}

// AvailableUIDsForModelRealm 同 AvailableUIDsForModel，但仅返回 Realm()==realm 的账号
// （6004 模型豁免照常生效）。realm=="" 退化为 AvailableUIDsForModel。
func (p *Pool) AvailableUIDsForModelRealm(model, realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		// 与 pick 同口径（审查 C5-84）：可用集合必须叠加积分保底与保留积分。
		// 这条路径供会话粘性的加权分配（cmd/server/wiring.go）用 —— 少了保底判定，
		// 会话会把流量绑到 pick 侧已停单的号上，每次请求多一次解绑 + 一条 WARN。
		if p.floorBlockedForRealmModel(e, model, e.a.Realm(), now) {
			continue
		}
		if p.reserveBlocked(e, p.modelFreeOf(e, model)) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}
