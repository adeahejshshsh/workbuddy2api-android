package server

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// /v1/models 要透出限时优惠字段（issue #203）。
//
// 背景：`credits` 是上游**牌价**倍率，`promo_*` 才是当前生效折扣（WorkBuddy 客户端
// 显示的就是生效价）。这两个字段此前只在 `/panel/api/models` 里有，OpenAI 兼容端点
// 看不到——第三方启动器只读 /v1/models，于是免费模型显示成收费、或者显示旧牌价。
//
// 判据与面板完全一致（panel.go 里那段）：
//   - PromoFactor 非 nil（上游给了可计算的折扣系数）→ 写 promo_factor + promo_credits；
//   - PromoLabel 非空 → 写 promo_label（「错峰使用」这类只有标签没有系数的也要透出）；
//   - 都没有 → 一个都不写（不编造「原价」）。
//
// 变异判据：删掉 handler.go 里那段 promo 透出，本用例红。
func TestV1ModelsExposesPromoFields(t *testing.T) {
	f := 0.0
	entry := applyModelInfoFields(map[string]any{"id": "cn:free-one"}, upstream.ModelInfo{
		ID:           "free-one",
		Credits:      "0.79",
		PromoFactor:  &f, // 0 = 限时免费
		PromoCredits: "0x",
		PromoLabel:   "限时免费",
	})
	if got, ok := entry["promo_factor"]; !ok || got != 0.0 {
		t.Fatalf("promo_factor = %v (存在=%v)，want 0（限时免费必须能被读出来，不能因为 0 值被省略）", got, ok)
	}
	if got, _ := entry["promo_credits"].(string); got != "0x" {
		t.Fatalf("promo_credits = %q want \"0x\"", got)
	}
	if got, _ := entry["promo_label"].(string); got != "限时免费" {
		t.Fatalf("promo_label = %q want 限时免费", got)
	}
	// 牌价原样保留：启动器要拿它划掉显示。
	if got, _ := entry["credits"].(string); got != "0.79" {
		t.Fatalf("credits = %q want 0.79（牌价不能被折扣覆盖）", got)
	}

	// 只有标签、没有系数（错峰使用）：promo_factor / promo_credits 不写，标签照写。
	half := applyModelInfoFields(map[string]any{"id": "cn:peak"}, upstream.ModelInfo{
		ID: "peak", PromoLabel: "错峰使用",
	})
	if _, ok := half["promo_factor"]; ok {
		t.Fatalf("没有折扣系数时不该写 promo_factor：%v", half)
	}
	if got, _ := half["promo_label"].(string); got != "错峰使用" {
		t.Fatalf("只带标签的优惠也要透出：%q", got)
	}

	// 完全没优惠：三个字段都不出现（别让客户端误判成「免费」）。
	plain := applyModelInfoFields(map[string]any{"id": "cn:paid"}, upstream.ModelInfo{ID: "paid", Credits: "x1.0"})
	for _, k := range []string{"promo_factor", "promo_credits", "promo_label"} {
		if _, ok := plain[k]; ok {
			t.Fatalf("无优惠的模型不该出现 %s：%v", k, plain)
		}
	}
}
