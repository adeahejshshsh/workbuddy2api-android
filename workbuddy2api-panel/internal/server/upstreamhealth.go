// upstreamhealth.go 上游「劣化但不报错」的滑窗告警。
//
// 问题：网关只在**请求失败**时说话。上游慢到不可用时，请求仍然回 200、表格流水
// 每行都是 success，一条 WARN 都没有。实测现场（2026-11-07 15:00–15:10）：
//
//	那 10 分钟 TTFB 中位 17.8s、最大 39.2s（当天其它时段中位 2.1–2.8s、最大 6.3s）
//	6 条请求全部 out=success，零告警；客户端在自己的 60s 超时里连撞后停摆，
//	请求量从 100 条/30min 掉到 6 条/小时。等上游开始回 500，劣化期已经过去了。
//
// 已有的 internal/alert 看不到这个：它盯的是**池子健康**（可用数/冷却/熔断/WAF），
// 不盯真实流量的时延。也看不到「半死」——账号都健康，只是每个请求都要 20 秒。
//
// 做法：所有 chat 请求都在 chatStat.done() 收口，这里按 5 分钟滑窗采样，
// 命中「慢请求占比」或「5xx 占比」就聚合打**一条** WARN，10 分钟内不重复。
// 纯观测：不改任何请求路径、不影响选号、不发额外上游请求。
package server

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

const (
	// upHealthWindow 滑窗长度。
	upHealthWindow = 5 * time.Minute
	// upHealthWarnEvery 两次告警的最小间隔（劣化会持续，不能每来一条就打一行）。
	upHealthWarnEvery = 10 * time.Minute
	// upHealthMinSample 样本太少不判——单请求撞上冷启动就会误报。
	upHealthMinSample = 10
	// upHealthSlowTTFB 单请求「慢」的阈值。取 8s：正常时段中位 2–3s，
	// 8s 已是 P95 之外的量级，且低于常见客户端 60s 超时的一半。
	upHealthSlowTTFB = 8 * time.Second
	// upHealthSlowRatio 慢请求占比阈值。
	upHealthSlowRatio = 0.30
	// upHealthFailRatio 5xx 占比阈值（且至少 upHealthMinFail 条）。
	upHealthFailRatio = 0.10
	upHealthMinFail   = 3
	// upHealthBuckets 滑窗按固定桶计数：每次 record 只动一个桶、扫 20 个桶，
	// 与流量无关。此前逐样本存切片，窗口满载时每个请求在全局锁内拷贝并扫描 4096 条（约 66µs）。
	// 代价是窗口边界粗到一个桶（15s）。
	upHealthBuckets = 20
	upHealthBucket  = upHealthWindow / upHealthBuckets
)

// upBucket 一个时间桶内的请求计数；id 是桶的序号（Unix 纳秒 / 桶宽），环上复用时据此识别过期。
type upBucket struct {
	id                int64
	total, slow, fail int
	worst             time.Duration
}

// upHealth 滑窗状态。
type upHealth struct {
	mu       sync.Mutex
	buckets  [upHealthBuckets]upBucket
	lastWarn time.Time
	// now 可注入（测试用固定时钟推进窗口）；nil 回退 time.Now。
	now func() time.Time
}

// chatUpHealth 进程级单例：handler 只写，测试可整体替换。
var chatUpHealth = &upHealth{}

func (u *upHealth) clock() time.Time {
	if u.now != nil {
		return u.now()
	}
	return time.Now()
}

// record 采样一条 chat 请求结果。只收两类样本：
//
//	成功且有 TTFB 观测 —— 用于「慢」的比例
//	5xx               —— 用于「坏」的比例
//
// 499 / interrupted 一律丢弃：那是调用方走了，既不代表上游快也不代表上游坏；
// 混进来会同时污染两个比例——用户随手点个停止就能把告警打出来。
func (u *upHealth) record(status int, outcome string, ttfb time.Duration) {
	if outcome == reqlog.OutcomeInterrupted || status == 499 || status == 0 {
		return
	}
	okSample := status >= 200 && status < 300 && ttfb > 0
	fail := status >= 500
	if !okSample && !fail {
		return
	}
	now := u.clock()

	u.mu.Lock()
	defer u.mu.Unlock()
	id := now.UnixNano() / int64(upHealthBucket)
	b := &u.buckets[id%upHealthBuckets]
	if b.id != id {
		*b = upBucket{id: id}
	}
	b.total++
	if fail {
		b.fail++
	} else {
		if ttfb >= upHealthSlowTTFB {
			b.slow++
		}
		b.worst = max(b.worst, ttfb)
	}

	total, slow, fails, worst := u.windowLocked(id)
	if total < upHealthMinSample {
		return
	}
	slowBad := float64(slow) >= float64(total)*upHealthSlowRatio
	failBad := fails >= upHealthMinFail && float64(fails) >= float64(total)*upHealthFailRatio
	if !slowBad && !failBad {
		return
	}
	if !u.lastWarn.IsZero() && now.Sub(u.lastWarn) < upHealthWarnEvery {
		return
	}
	u.lastWarn = now
	// 一条聚合行：窗口、样本量、慢/坏的数量与占比、最慢值。够运维判断要不要
	// 提前摘上游或调大客户端超时，又不至于逐请求刷屏。
	log.Printf("WARN: [server] upstream degraded: 最近 %s 内 %d 条 chat 请求，"+
		"TTFB>=%s 占 %d%%（最慢 %s），5xx 占 %d%%——上游整体劣化，"+
		"请检查 provider 状态或调大客户端超时",
		upHealthWindow, total, upHealthSlowTTFB, percent(slow, total),
		worst.Round(100*time.Millisecond), percent(fails, total))
}

// windowLocked 汇总桶序号 (cur-upHealthBuckets, cur] 内的计数，更早的桶视为已滑出窗口。
func (u *upHealth) windowLocked(cur int64) (total, slow, fails int, worst time.Duration) {
	for _, b := range u.buckets {
		if b.total == 0 || b.id <= cur-upHealthBuckets || b.id > cur {
			continue
		}
		total += b.total
		slow += b.slow
		fails += b.fail
		worst = max(worst, b.worst)
	}
	return
}

// percent 整数百分比（日志够用，避免 %.0f 的浮点噪音）。
func percent(n, total int) int {
	if total == 0 {
		return 0
	}
	return n * 100 / total
}

// summary 当前窗口统计（面板 / 测试用）。
func (u *upHealth) summary() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	total, slow, fails, _ := u.windowLocked(u.clock().UnixNano() / int64(upHealthBucket))
	return fmt.Sprintf("samples=%d slow=%d fails=%d lastWarn=%s",
		total, slow, fails, u.lastWarn.Format(time.RFC3339))
}
