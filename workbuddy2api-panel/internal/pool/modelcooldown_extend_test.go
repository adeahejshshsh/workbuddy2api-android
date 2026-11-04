package pool

// 11102 负缓存（BlockModelBackoff）与 6004 模型级限流共用 modelCooldowns 同一个 key。
// 前者 TTL 上限 10 分钟，后者 Until 是上游重置墙钟（可达数小时，带 ResetAt）。
//
// 此前 BlockModelBackoff 是**整体覆盖**：一次 11102 抖动就把 6 小时保护砍成 2 分钟、
// ResetAt 丢失、AuditOnly 被抹成 false —— 2 分钟后号又被派到该模型、再撞一次 6004。
// 这与 Cooldown / CooldownSoftRate / CooldownModelBusy 三处「只延长不缩短」的守卫
// 是同一口径，唯独这里漏了。

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func cooldownTestPool() *Pool {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	return p
}

// 已有 6004 长冷却时，11102 不得缩短它。
func TestBlockModelBackoffDoesNotShorten6004(t *testing.T) {
	p := cooldownTestPool()
	resetAt := time.Now().Add(6 * time.Hour)
	p.CooldownSoftForModel("u1", 10*time.Minute, resetAt, "glm-5.2", "6004 model rate limit")

	p.mu.RLock()
	before := p.byUID["u1"].modelCooldowns["glm-5.2"]
	p.mu.RUnlock()
	if before.Until.Before(time.Now().Add(5 * time.Hour)) {
		t.Fatalf("前置条件不成立：6004 冷却应达数小时，实际 Until=%v", before.Until)
	}

	p.BlockModelBackoff("u1", "glm-5.2", "11102 model not available")

	p.mu.RLock()
	after := p.byUID["u1"].modelCooldowns["glm-5.2"]
	p.mu.RUnlock()
	if after.Until.Before(before.Until) {
		t.Fatalf("11102 把 6004 的 %v 冷却砍成了 %v（只允许延长）", before.Until, after.Until)
	}
	if !after.ResetAt.Equal(before.ResetAt) {
		t.Fatalf("11102 丢掉了上游重置墙钟：before=%v after=%v", before.ResetAt, after.ResetAt)
	}
	if after.AuditOnly {
		t.Fatal("11102 是硬阻断，不该保留 AuditOnly（那会让选号忽略它）")
	}
	if after.Hits != 1 {
		t.Fatalf("11102 命中计数=%d want 1", after.Hits)
	}
}

// 反向：没有既有条目（或既有条目更短）时，11102 照常写入自己的 TTL。
func TestBlockModelBackoffWritesWhenNoLongerEntry(t *testing.T) {
	p := cooldownTestPool()
	p.BlockModelBackoff("u1", "glm-5.2", "11102 model not available")

	p.mu.RLock()
	got := p.byUID["u1"].modelCooldowns["glm-5.2"]
	p.mu.RUnlock()
	if !got.Until.After(time.Now()) {
		t.Fatalf("应写入有效冷却，Until=%v", got.Until)
	}
	if got.Hits != 1 {
		t.Fatalf("Hits=%d want 1", got.Hits)
	}
	if got.Until.After(time.Now().Add(modelBlockMaxTTL + time.Minute)) {
		t.Fatalf("11102 的 TTL 不该超过上限 %v，实际 %v", modelBlockMaxTTL, got.Until.Sub(time.Now()))
	}

	// 再撞一次：命中计数递增（驱动指数退避）。
	p.BlockModelBackoff("u1", "glm-5.2", "11102 model not available")
	p.mu.RLock()
	got2 := p.byUID["u1"].modelCooldowns["glm-5.2"]
	p.mu.RUnlock()
	if got2.Hits != 2 {
		t.Fatalf("第二次命中后 Hits=%d want 2", got2.Hits)
	}
}
