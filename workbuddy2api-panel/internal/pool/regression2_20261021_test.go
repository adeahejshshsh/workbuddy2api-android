package pool

import (
	"testing"
	"time"
)

// bug 1：账号级冷却不许连带清掉 11102/11158 的负缓存与退避计数。
//
// 这些条目是「这个号就是用不了这个模型」的上游事实，与账号级软冷却无关；
// 连 Hits 一起删会让 6h→12h→24h 的退避从零重学，还会让下次请求再去撞同一个必败模型。
func TestClearRoutingKeepsModelNegativeCache(t *testing.T) {
	e := &entry{modelCooldowns: map[string]modelCooldown{
		"m-11102": {Reason: "11102 model not found", Hits: 2},
		"m-11158": {Reason: "11158 tier denied", Hits: 1},
		"m-6004":  {Reason: "6004 model rate limit", Until: time.Now().Add(time.Hour)},
		"m-audit": {Reason: "audit", AuditOnly: true},
	}}
	clearRoutingModelCooldownsLocked(e)

	if mc, ok := e.modelCooldowns["m-11102"]; !ok || mc.Hits != 2 {
		t.Errorf("11102 负缓存被清了：%+v ok=%v（Hits 必须保留）", mc, ok)
	}
	if _, ok := e.modelCooldowns["m-11158"]; !ok {
		t.Error("11158 负缓存被清了")
	}
	if _, ok := e.modelCooldowns["m-6004"]; ok {
		t.Error("6004 参与路由的冷却应当被清掉")
	}
	if _, ok := e.modelCooldowns["m-audit"]; !ok {
		t.Error("AuditOnly 台账应当保留")
	}
}
