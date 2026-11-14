package upstream

import (
	"testing"
	"time"
)

// 到期日按计量周期推算时，桶上要能看出来：面板据此标注「推算」，运维不会把推算值
// 当权威扣费截止（issue #200）。
func TestPackageEndAtMarksEstimated(t *testing.T) {
	// 真实扣费截止（毫秒时间戳 / ExpiredTime）→ 不是推算。
	if _, ok, est := packageEndAt(1770000000000, "", ""); !ok || est {
		t.Fatalf("DeductionEndTime 应给出真实截止：ok=%v est=%v", ok, est)
	}
	if _, ok, est := packageEndAt(0, "2026-12-01 00:00:00", "2026-12-31 00:00:00"); !ok || est {
		t.Fatalf("ExpiredTime 应优先于 CycleEndTime：ok=%v est=%v", ok, est)
	}
	// 只有 CycleEndTime → 推算。
	if _, ok, est := packageEndAt(0, "", "2026-12-31 00:00:00"); !ok || !est {
		t.Fatalf("只有 CycleEndTime 时应标记为推算：ok=%v est=%v", ok, est)
	}
	// 都没有 → 无到期时刻。
	if _, ok, _ := packageEndAt(0, "", ""); ok {
		t.Fatal("三个字段都缺时不该给出到期时刻")
	}
}

// 推算 WARN 按账号限频：余额刷新每 5 分钟一轮，不限频会让每个号每天刷 288 行同样的日志。
func TestEstimatedExpiryWarnRateLimited(t *testing.T) {
	old := estimatedExpiryWarnEvery
	estimatedExpiryWarnEvery = time.Hour
	t.Cleanup(func() { estimatedExpiryWarnEvery = old })
	uid := "test-uid-rate-limit"
	now := time.Now()
	if !estimatedExpiryWarnDue(uid, now) {
		t.Fatal("首次应允许写 WARN")
	}
	if estimatedExpiryWarnDue(uid, now.Add(time.Minute)) {
		t.Fatal("同一小时内不该重复写")
	}
	if !estimatedExpiryWarnDue(uid, now.Add(2*time.Hour)) {
		t.Fatal("过了间隔应重新允许")
	}
}
