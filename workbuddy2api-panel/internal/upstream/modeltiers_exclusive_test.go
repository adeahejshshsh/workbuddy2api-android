package upstream

import "testing"

// accessMode=exclusive 才是「免费号调不通」的机器可读口径；CN 那条「订阅优先」只有文案，
// 混成同一判据会把能用的国内模型错标成禁选（issue #235 的教训）。
func TestApplyModelTiersExclusive(t *testing.T) {
	badge := func(s string) *struct {
		Label string `json:"label"`
	} {
		return &struct {
			Label string `json:"label"`
		}{Label: s}
	}
	out := map[string]ModelInfo{
		"pro-model":      {ID: "pro-model"},
		"priority-model": {ID: "priority-model"},
	}
	applyModelTiers(out, []v3ModelTier{
		{Enabled: true, Priority: 200, ModelIDs: []string{"pro-model"}, AccessMode: "exclusive", Badge: badge("Pro Only")},
		{Enabled: true, Priority: 100, ModelIDs: []string{"priority-model"}, Badge: badge("订阅优先")},
	})
	if !out["pro-model"].Exclusive {
		t.Error("accessMode=exclusive 的模型应标 Exclusive")
	}
	if out["priority-model"].Exclusive {
		t.Error("只有「订阅优先」文案、没有 accessMode 的不算门控（会误拒能用的请求）")
	}
}

// ModelExclusive 按域分桶：同一模型名在另一域没有门控时不能串味。
func TestModelExclusivePerRealm(t *testing.T) {
	c := &Client{}
	c.storeModelRates("global", []ModelInfo{{ID: "pro-model", Exclusive: true}, {ID: "free-model"}})
	c.storeModelRates("cn", []ModelInfo{{ID: "pro-model"}})
	if !c.ModelExclusive("global", "pro-model") {
		t.Error("global:pro-model 应被标为 exclusive")
	}
	if c.ModelExclusive("cn", "pro-model") {
		t.Error("cn 域未标 exclusive，不该串味")
	}
	if c.ModelExclusive("global", "free-model") || c.ModelExclusive("global", "") {
		t.Error("未标 exclusive 的模型 / 空模型名不该被拦")
	}
}

// 付费状态「没观测过」不等于免费：PaidUser 的第二个返回值必须能区分这两件事。
func TestPaidUserUnknownVsFree(t *testing.T) {
	c := &Client{}
	if _, known := c.PaidUser("u1"); known {
		t.Fatal("没记过就该是 unknown")
	}
	c.NotePaidUser("u1", false)
	if paid, known := c.PaidUser("u1"); !known || paid {
		t.Fatalf("PaidUser = %v,%v want false,true", paid, known)
	}
	c.NotePaidUser("u1", true)
	if paid, _ := c.PaidUser("u1"); !paid {
		t.Fatal("后一次观测应覆盖前一次")
	}
}
