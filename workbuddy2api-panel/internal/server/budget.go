// budget.go 积分闸：当日累计上限 + 突发积分 + 连续低缓存率。
// 只加观测到的 usage.credit（缺失≠0）。按 CST 自然日惰性归零，进程内不落盘。
package server

import (
	"fmt"
	"sync"
	"time"
)

// cstZone 与 scheduler 的 CST 日界同一口径。server 不 import scheduler。
var cstZone = time.FixedZone("CST", 8*60*60)

func cstDay(t time.Time) string { return t.In(cstZone).Format("2006-01-02") }

// 突发/缓存闸的固定口径。issue #229：hy4-preview 这类模型 140k 上下文一次对话 20+
// 积分、缓存率极低时，当日上限要烧到很晚才触发，来不及止血。
// shortcut: 窗口 5 分钟、连败 5 轮、触发后 60s 半开探测都写死；需要更细粒度再拆配置。
const (
	budgetBurstWindow = 5 * time.Minute
	budgetCacheStreak = 5
	budgetCacheCool   = 60 * time.Second
)

// budgetSample 一次已观测扣费，供突发窗口求和。
type budgetSample struct {
	at     time.Time
	credit float64
}

type dailyBudget struct {
	mu       sync.Mutex
	limit    float64
	day      string
	used     float64
	rejected int64

	// 突发积分闸：budgetBurstWindow 内累计 credit 达到 burstLimit 即拒（0=关）。
	burstLimit float64
	burst      []budgetSample

	// 缓存率闸：连续 budgetCacheStreak 轮命中率低于 cacheLowRate（%，0=关）即拒。
	cacheLowRate  float64
	cacheStreak   int
	cacheCoolTill time.Time

	lastReject string // "burst" / "cache"，仅用于错误信息与面板诊断
}

func newDailyBudget(limit float64) *dailyBudget {
	return &dailyBudget{limit: limit, day: cstDay(time.Now())}
}

func (b *dailyBudget) setLimit(limit float64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.limit = limit
	b.mu.Unlock()
}

// setGuards 热改两道快速闸。<=0 关闭对应闸，已累计的窗与连败保留。
func (b *dailyBudget) setGuards(burstLimit, cacheLowRate float64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.burstLimit = burstLimit
	b.cacheLowRate = cacheLowRate
	b.mu.Unlock()
}

// admit 严格小于才放行。返回拒绝原因（空串=放行），到顶的拒绝单独计数。
func (b *dailyBudget) admit() (bool, string) {
	if b == nil {
		return true, ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	now := time.Now()
	if b.limit > 0 && b.used >= b.limit {
		b.rejected++
		b.lastReject = "daily"
		return false, ""
	}
	if b.burstLimit > 0 {
		b.trimBurstLocked(now)
		sum := 0.0
		for _, s := range b.burst {
			sum += s.credit
		}
		if sum >= b.burstLimit {
			b.rejected++
			b.lastReject = "burst"
			return false, fmt.Sprintf(
				"credit burst guard: %.2f credits within %s (limit %.2f); 该模型近期单位时间耗分过高，稍后再试",
				sum, budgetBurstWindow, b.burstLimit)
		}
	}
	if b.cacheLowRate > 0 && b.cacheStreak >= budgetCacheStreak && now.Before(b.cacheCoolTill) {
		b.rejected++
		b.lastReject = "cache"
		return false, fmt.Sprintf(
			"cache hit rate below %.1f%% for %d consecutive rounds; 前缀缓存未命中导致按全量计费，冷却 %s 后自动放行",
			b.cacheLowRate, b.cacheStreak, time.Until(b.cacheCoolTill).Truncate(time.Second))
	}
	return true, ""
}

func (b *dailyBudget) add(credit float64, observed bool) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	if observed && credit > 0 {
		b.used += credit
		if b.burstLimit > 0 {
			now := time.Now()
			b.trimBurstLocked(now)
			b.burst = append(b.burst, budgetSample{at: now, credit: credit})
		}
	}
}

// observeCache 记一轮缓存率。has=false（上游没给缓存字段）不参与判定，避免把
// 「模型不报缓存」误读成「零命中」。
func (b *dailyBudget) observeCache(hit, miss int64, has bool) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	if !has || hit+miss <= 0 || b.cacheLowRate <= 0 {
		return
	}
	if float64(hit)/float64(hit+miss)*100 < b.cacheLowRate {
		b.cacheStreak++
		if b.cacheStreak >= budgetCacheStreak {
			b.cacheCoolTill = time.Now().Add(budgetCacheCool)
		}
		return
	}
	b.cacheStreak = 0
}

func (b *dailyBudget) snapshot() (used, limit float64, rejected int64, lastReject string) {
	if b == nil {
		return 0, 0, 0, ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	return b.used, b.limit, b.rejected, b.lastReject
}

// SetBudgetLimit 热改当日上限。<=0 关闭闸，已累计的 used 保留。
func (h *Handler) SetBudgetLimit(limit float64) {
	if h == nil || h.budget == nil {
		return
	}
	h.budget.setLimit(limit)
}

// SetBudgetGuards 热改突发/缓存闸。<=0 关闭对应闸。
func (h *Handler) SetBudgetGuards(burstLimit, cacheLowRate float64) {
	if h == nil || h.budget == nil {
		return
	}
	h.budget.setGuards(burstLimit, cacheLowRate)
}

func (b *dailyBudget) trimBurstLocked(now time.Time) {
	cut := now.Add(-budgetBurstWindow)
	i := 0
	for i < len(b.burst) && !b.burst[i].at.After(cut) {
		i++
	}
	if i > 0 {
		b.burst = b.burst[i:]
	}
}

func (b *dailyBudget) rollLocked() {
	if today := cstDay(time.Now()); today != b.day {
		b.day = today
		b.used = 0
		b.rejected = 0
		b.burst = nil
		b.cacheStreak = 0
		b.cacheCoolTill = time.Time{}
		b.lastReject = ""
	}
}
