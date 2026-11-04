// budget.go 积分闸：当日累计上限 + 突发积分 + 连续低缓存率。
// 只加观测到的 usage.credit（缺失≠0）。按 CST 自然日惰性归零。
//
// 累计值**落盘**（budgetStateFile）：此前只在进程内存里，重启即清零 —— 崩溃循环 /
// 反复部署下，唯一的成本止损线等于不存在。写入节流到 ≥budgetSaveEvery（每 5 秒最多
// 一次小文件写），日界翻转时强制写一次。
package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

	// 落盘（可选）：path 为空 = 不落盘（纯内存，测试与旧行为）。
	path     string
	lastSave time.Time
}

// budgetSaveEvery 落盘节流间隔：累计值每 5 秒最多写一次，崩溃最多丢 5 秒的用量。
// 多副本部署各写各的文件没有意义（需要共享存储），所以这里只解决单实例重启清零。
const budgetSaveEvery = 5 * time.Second

// budgetState 落盘形态。day 用来判「跨日」：读到旧日期的状态直接丢弃。
type budgetState struct {
	Day  string  `json:"day"`
	Used float64 `json:"used"`
}

// newDailyBudget 构造积分闸。path 非空时从它读回当日累计（跨日/损坏按 0 起算），
// 后续 add 会节流落盘（见 saveLocked）。
func newDailyBudget(limit float64, path string) *dailyBudget {
	b := &dailyBudget{limit: limit, day: cstDay(time.Now()), path: path}
	b.loadBudgetState()
	return b
}

// loadBudgetState 从 path 读回当日累计；跨日/文件缺失/损坏都按「从 0 开始」。
// 读失败不阻断启动（预算闸是保护，不该让网关起不来），只打一行日志。
func (b *dailyBudget) loadBudgetState() {
	if b == nil || b.path == "" {
		return
	}
	raw, err := os.ReadFile(b.path)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Printf("WARN: [budget] 读 %s 失败，按 0 起算: %v\n", b.path, err)
		}
		return
	}
	var st budgetState
	if json.Unmarshal(raw, &st) != nil {
		fmt.Printf("WARN: [budget] %s 不是合法 JSON，按 0 起算\n", b.path)
		return
	}
	if st.Day != cstDay(time.Now()) {
		return // 跨日：昨天的用量与今天无关
	}
	if st.Used > 0 {
		b.used = st.Used
	}
}

// saveLocked 落盘（调用方已持锁）。节流到 budgetSaveEvery；force=true 时立即写。
//
// 写失败只打日志：预算闸是保护手段，写不下去也不该影响服务。
func (b *dailyBudget) saveLocked(now time.Time, force bool) {
	if b == nil || b.path == "" {
		return
	}
	if !force && now.Sub(b.lastSave) < budgetSaveEvery {
		return
	}
	b.lastSave = now
	raw, err := json.Marshal(budgetState{Day: b.day, Used: b.used})
	if err != nil {
		return
	}
	if dir := filepath.Dir(b.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Printf("WARN: [budget] 建目录 %s 失败: %v\n", dir, err)
			return
		}
	}
	// 原子写：临时文件 + rename（同分区 rename 原子），避免半截 JSON 让下次读到 0。
	tmp := b.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		fmt.Printf("WARN: [budget] 写 %s 失败: %v\n", tmp, err)
		return
	}
	if err := os.Rename(tmp, b.path); err != nil {
		fmt.Printf("WARN: [budget] rename %s 失败: %v\n", b.path, err)
	}
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
		now := time.Now()
		if b.burstLimit > 0 {
			b.trimBurstLocked(now)
			b.burst = append(b.burst, budgetSample{at: now, credit: credit})
		}
		b.saveLocked(now, false)
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
	now := time.Now()
	if today := cstDay(now); today != b.day {
		b.day = today
		b.used = 0
		b.rejected = 0
		b.burst = nil
		b.cacheStreak = 0
		b.cacheCoolTill = time.Time{}
		b.lastReject = ""
		// 跨日强制落盘：不写的话磁盘上留着昨天的累计值，进程在同一天内重启会
		// 被 loadBudgetState 的「日期不等就丢弃」判掉——那是对的；但若重启发生在
		// 跨日之后、第一次 add 之前，磁盘仍是旧日期，同样丢弃。这里写一次是为了
		// 让文件里的 day 与新的一天对齐，避免「看起来还有昨天的量」。
		b.saveLocked(now, true)
	}
}
