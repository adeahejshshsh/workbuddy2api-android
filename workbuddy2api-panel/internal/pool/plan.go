// plan.go 账号套餐分类：从上游积分包名判定「免费版 / 体验版 / 活动号 / 标准版」。
//
// 为什么要分类：账号的「来头」决定它的稳定性——免费版额度最小、最容易撞 6004；活动号
// 的积分全靠赠送包，用完就没了；标准版有正式订阅。面板标出来，运维一眼知道该指望哪个号。
//
// 判定依据是**上游下发的包名文本**，故用关键词宽松匹配（上游随时改文案），且原始包名
// 照旧透出（面板「积分构成」页逐包列出），匹配不上时不猜——返回 unknown。
package pool

import "strings"

// PlanKind 账号套餐类型。
type PlanKind string

const (
	PlanFree     PlanKind = "free"     // 免费版
	PlanTrial    PlanKind = "trial"    // 体验版 / 试用
	PlanActivity PlanKind = "activity" // 积分全部来自活动赠送包
	PlanStandard PlanKind = "standard" // 有正式订阅（标准版/专业版等）
	PlanUnknown  PlanKind = "unknown"  // 无包信息 / 无法判定
)

// planStrong 强信号关键词，顺序即优先级。
//
// 为什么强信号先判：一个账号可能**同时**有正式订阅和活动包，那时订阅才是它的真实身份
// （活动包只是添头），所以「免费/体验/标准版」出现在任一包里就先定案。
var planStrong = []struct {
	kind PlanKind
	keys []string
}{
	{PlanFree, []string{"free plan", "免费版", "free tier", "免费套餐"}},
	{PlanTrial, []string{"trial", "体验版", "试用", "体验套餐"}},
	{PlanStandard, []string{"标准版", "专业版", "团队版", "企业版", "standard", "pro plan", "premium", "plus", "订阅"}},
}

// planActivityKeys 活动/赠送包关键词：共同点是积分由活动发放而非订阅额度。
var planActivityKeys = []string{
	"拉新", "裂变", "权益包", "赠送", "活动", "邀请", "新人",
	"bonus pack", "promo", "reward",
}

// ClassifyPlan 由包名列表判定账号类型，并返回命中的证据（供面板展示）。
//
// packages 为空 = 无数据（查询失败或真的没有包）→ PlanUnknown，**不猜**。
func ClassifyPlan(packages []string) (PlanKind, string) {
	if len(packages) == 0 {
		return PlanUnknown, ""
	}
	low := make([]string, 0, len(packages))
	for _, p := range packages {
		low = append(low, strings.ToLower(p))
	}
	for _, g := range planStrong {
		for _, l := range low {
			for _, k := range g.keys {
				if strings.Contains(l, strings.ToLower(k)) {
					return g.kind, k
				}
			}
		}
	}
	// 没有强信号：每个包都命中活动关键词才算活动号（有一个不是就不敢下结论）。
	for _, l := range low {
		if !hasAny(l, planActivityKeys) {
			return PlanUnknown, ""
		}
	}
	return PlanActivity, "全部为活动/赠送包"
}

func hasAny(s string, keys []string) bool {
	for _, k := range keys {
		if strings.Contains(s, strings.ToLower(k)) {
			return true
		}
	}
	return false
}
