// 选号失败的成因拆解（issue #276）。
package pool

import (
	"fmt"
	"time"
)

// NoPick 一次「Pick 返回 nil」的成因拆解，每个账号按 pick 的实际判据顺序归一类。
//
// 为什么需要它：no_healthy_account 是一句笼统的「没有可用账号」，但池子里有 20 个有余额
// 的号时，用户只能看到这句话——真实原因可能是保留积分停牌、积分保底、在途占满、账号冷却
// 或本次轮转已试过，五种成因的处置完全不同（充钱 / 调阈值 / 提并发 / 等冷却 / 换号）。
// 这与 ModelBlocked 解决的是同一类问题（号在，但一个都选不中）。
type NoPick struct {
	Total        int // 本域账号数（realm 过滤后）
	Healthy      int // 过了全部判据却没被选中：正常应为 0，非 0 说明 pick 里还有别的闸门
	Paused       int // 暂停选号
	Disabled     int // 已禁用
	Cooling      int // 账号级冷却（模型级冷却走 model_blocked，不在此处）
	InFlightFull int // 在途名额占满（pool.max_in_flight）
	Reserve      int // 保留积分停牌（pool.reserve_credits，默认 10）
	Floor        int // 积分保底（pool.credit_floor，默认 0 = 关闭）
	Tried        int // 本次请求已试过（MaxRotate 轮转）
}

// String 紧凑单行，供日志与 503 的 gateway_hint 直接引用。
func (n NoPick) String() string {
	return fmt.Sprintf("total=%d healthy=%d cooling=%d paused=%d disabled=%d in_flight_full=%d reserve=%d floor=%d tried=%d",
		n.Total, n.Healthy, n.Cooling, n.Paused, n.Disabled, n.InFlightFull, n.Reserve, n.Floor, n.Tried)
}

// NoPickForRealm 复刻 pick 的排除顺序给账号归类。tried 为 nil 时不计轮转。
//
// 判据顺序必须与 pick 一致，否则「选不到」与「归到哪一类」会自相矛盾：
// disabled/paused → 在途占满 → 保留积分 → 健康（模型感知）→ 积分保底 → 可选。
func (p *Pool) NoPickForRealm(model, realm string, tried map[string]bool) NoPick {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	var n NoPick
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		n.Total++
		healthy := e.healthy(now)
		if model != "" {
			healthy = e.healthyForModel(now, model)
		}
		switch {
		case tried != nil && tried[uid]:
			n.Tried++
		case e.disabled:
			n.Disabled++
		case e.paused:
			n.Paused++
		case p.inFlightFull(e):
			n.InFlightFull++
		case p.reserveBlocked(e, p.modelFreeOf(e, model)):
			n.Reserve++
		case !healthy:
			n.Cooling++
		case p.floorBlockedForRealmModel(e, model, realm, now):
			n.Floor++
		default:
			n.Healthy++
		}
	}
	return n
}
