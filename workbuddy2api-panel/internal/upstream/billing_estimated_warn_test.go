package upstream

import (
	"fmt"
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

// TestEstimatedExpiryWarnMapPruned W1-13 回归：限频表必须有界。
//
// 缺陷：包级 sync.Map 只写不删，uid 来自导入账号，删号/换号后条目永久残留；
// 网关 7×24 常驻、账号反复导入导出 → 单调增长，且 sync.Map 连长度都观测不到。
func TestEstimatedExpiryWarnMapPruned(t *testing.T) {
	oldLimit, oldEvery := estimatedExpiryWarnLimit, estimatedExpiryWarnEvery
	estimatedExpiryWarnLimit, estimatedExpiryWarnEvery = 4, time.Hour
	estimatedExpiryWarnMu.Lock()
	estimatedExpiryWarned = make(map[string]time.Time)
	estimatedExpiryWarnMu.Unlock()
	t.Cleanup(func() {
		estimatedExpiryWarnLimit, estimatedExpiryWarnEvery = oldLimit, oldEvery
		estimatedExpiryWarnMu.Lock()
		estimatedExpiryWarned = make(map[string]time.Time)
		estimatedExpiryWarnMu.Unlock()
	})

	now := time.Now()
	// 塞满到上限：4 条都是 3 小时前写的（超过 2 个间隔 → 可清）。
	for i := 0; i < 4; i++ {
		if !estimatedExpiryWarnDue(fmt.Sprintf("stale-%d", i), now.Add(-3*time.Hour)) {
			t.Fatalf("首次应允许写 WARN（stale-%d）", i)
		}
	}
	// 第 5 个不同 uid：触发清理，4 条陈旧条目应被清掉。
	if !estimatedExpiryWarnDue("fresh", now) {
		t.Fatal("新 uid 首次应允许写 WARN")
	}
	estimatedExpiryWarnMu.Lock()
	n := len(estimatedExpiryWarned)
	_, hasFresh := estimatedExpiryWarned["fresh"]
	estimatedExpiryWarnMu.Unlock()
	if n > 1 || !hasFresh {
		t.Fatalf("陈旧条目未被清理：len=%d（应只剩 1 条 fresh）", n)
	}
	// 清理不能改变限频语义：同一间隔内 fresh 仍然只允许一次。
	if estimatedExpiryWarnDue("fresh", now.Add(time.Minute)) {
		t.Fatal("清理后限频语义被破坏（同一间隔重复写 WARN）")
	}
}
