package pool

// 11102 负缓存 TTL 守卫（上游 2026-10-28 `4f79fc8`）：6h/24h → 2m/10m。
//
// 11102 不总是「这号真没这模型」—— 路径级差异、上游抖动、分类误判都会给同一个码。
// 原来一次误判就把 (账号,模型) 锁死 6 小时，多号轮一圈就是整池该模型 503，
// 而实测同号两分钟即恢复。这条钉住基数与封顶，防止有人凭直觉调回小时级。

import (
	"testing"
	"time"
)

func TestModelBlockTTLGuard(t *testing.T) {
	if modelBlockBaseTTL != 2*time.Minute {
		t.Fatalf("基数应为 2m（小时级会把一次误判放大成整池停摆），got %v", modelBlockBaseTTL)
	}
	if modelBlockMaxTTL != 10*time.Minute {
		t.Fatalf("封顶应为 10m，got %v", modelBlockMaxTTL)
	}
	if modelBlockMaxTTL < modelBlockBaseTTL {
		t.Fatalf("封顶不得小于基数: base=%v max=%v", modelBlockBaseTTL, modelBlockMaxTTL)
	}
}
