// plan.go 账号套餐分类：从上游积分包名判定「免费版 / 体验版 / 活动号 / 标准版」。
//
// 为什么要分类：账号的「来头」决定它的稳定性——免费版额度最小、最容易撞 6004；活动号
// 的积分全靠赠送包，用完就没了；标准版有正式订阅。面板标出来，运维一眼知道该指望哪个号。
//
// 判定依据是**上游下发的包名文本**，故用关键词宽松匹配（上游随时改文案），且原始包名
// 照旧透出（面板「积分构成」页逐包列出），匹配不上时不猜——返回 unknown。
package pool

import (
	"strings"
	"unicode"
)

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

// planKeyword 预编译的关键词（审查 C5-96）：小写只做一次，且 ASCII 关键词按
// **整词**匹配（审查 C5-95）。
//
// 为什么要整词：此前一律 strings.Contains，于是 "plus" 命中 "surplus"、"premium"
// 命中 "premium-ish" 之类都算标准版，而 "free plan (plus bonus)" 这种包名又会因为
// PlanFree 先判而被判成免费版 —— 分类随文案措辞漂移，且都是静默的。
// 整词匹配后 "credits plus" 这类仍然命中（plus 是独立词），"surplus" 不再命中。
// 中文关键词没有词边界（不分词），继续按子串匹配 —— 中文包名里嵌关键词是常态。
type planKeyword struct {
	raw   string   // 原始关键词（命中时作为证据返回）
	words []string // 非空 = ASCII 整词序列（全小写）；空 = 走子串匹配（CJK）
}

func newPlanKeyword(k string) planKeyword {
	kw := planKeyword{raw: k}
	if isASCIIKeyword(k) {
		kw.words = splitWords(strings.ToLower(k))
	}
	return kw
}

// isASCIIKeyword 关键词是否全为 ASCII（决定用整词还是子串匹配）。
func isASCIIKeyword(s string) bool {
	for _, r := range s {
		if r > unicode.MaxASCII {
			return false
		}
	}
	return true
}

// splitWords 按「非字母数字」切词并小写（下划线/连字符/点都算分隔符）。
func splitWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// containsWords 判断 words 里是否出现完整的 seq 序列（连续词，整词相等）。
func containsWords(words, seq []string) bool {
	if len(seq) == 0 || len(words) < len(seq) {
		return false
	}
	for i := 0; i+len(seq) <= len(words); i++ {
		ok := true
		for j, w := range seq {
			if words[i+j] != w {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// 预编译表（包级一次性构建，避免每次调用重复 ToLower 常量表）。
var (
	planStrongMatchers = func() []struct {
		kind PlanKind
		keys []planKeyword
	} {
		out := make([]struct {
			kind PlanKind
			keys []planKeyword
		}, 0, len(planStrong))
		for _, g := range planStrong {
			keys := make([]planKeyword, 0, len(g.keys))
			for _, k := range g.keys {
				keys = append(keys, newPlanKeyword(k))
			}
			out = append(out, struct {
				kind PlanKind
				keys []planKeyword
			}{kind: g.kind, keys: keys})
		}
		return out
	}()
	planActivityMatchers = func() []planKeyword {
		out := make([]planKeyword, 0, len(planActivityKeys))
		for _, k := range planActivityKeys {
			out = append(out, newPlanKeyword(k))
		}
		return out
	}()
)

// ClassifyPlan 由包名列表判定账号类型，并返回命中的证据（供面板展示）。
//
// packages 为空 = 无数据（查询失败或真的没有包）→ PlanUnknown，**不猜**。
func ClassifyPlan(packages []string) (PlanKind, string) {
	if len(packages) == 0 {
		return PlanUnknown, ""
	}
	low := make([]string, 0, len(packages))
	words := make([][]string, 0, len(packages))
	for _, p := range packages {
		l := strings.ToLower(p)
		low = append(low, l)
		words = append(words, splitWords(l))
	}
	for _, g := range planStrongMatchers {
		for i, l := range low {
			for _, k := range g.keys {
				if matchPlanKeyword(l, words[i], k) {
					return g.kind, k.raw
				}
			}
		}
	}
	// 没有强信号：每个包都命中活动关键词才算活动号（有一个不是就不敢下结论）。
	for i, l := range low {
		if !hasAnyWords(l, words[i], planActivityMatchers) {
			return PlanUnknown, ""
		}
	}
	return PlanActivity, "全部为活动/赠送包"
}

// matchPlanKeyword 单个关键词匹配：ASCII 走整词，CJK 走子串（审查 C5-95）。
func matchPlanKeyword(lower string, words []string, k planKeyword) bool {
	if len(k.words) == 0 {
		return strings.Contains(lower, strings.ToLower(k.raw))
	}
	return containsWords(words, k.words)
}

func hasAnyWords(lower string, words []string, keys []planKeyword) bool {
	for _, k := range keys {
		if matchPlanKeyword(lower, words, k) {
			return true
		}
	}
	return false
}
