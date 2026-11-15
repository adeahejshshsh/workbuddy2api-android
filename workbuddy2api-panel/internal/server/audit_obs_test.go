package server

// 审查 10.4（W3-C9）server 工具与观测段落的回归用例。每条对应一个审查编号：
//
//	C9-MF-01 回读名额只靠调用方 Close 归还、无超时回收 → TestMediaSlotLeaseReclaim* /
//	         TestMediaSlotReleaseIsIdempotent
//	C9-MF-03 错误路径手写归还、与 once 两套语义   → TestMediaSlotExpiredCloseDoesNotSteal
//	C9-MT-01/02/05 label 基数与淘汰                → TestMetricsEvictKeepsActiveModel /
//	                                                 TestMetricsModelLabelSanitized
//	C9-MT-03 非 200 的成功（异步受理）被算 failed  → TestMetricsAsyncAcceptedCountsAsSuccess
//	metrics 总计行漏累加 CH/CM                     → TestMetricsTotalIncludesCacheTokens
//	W3C8-21 drain 后不重置读截止时间（连接污染）   → TestBodyLimitDrainResetsReadDeadline
//	bodylimit「静默截断」                          → TestBodyLimiterNeverSilentlyTruncates

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// —— media_fetch 名额租约（C9-MF-01 / C9-MF-03） ——

// resetMediaSlots 清空名额通道与租约表，避免用例之间互相污染。
func resetMediaSlots(t *testing.T) {
	t.Helper()
	clean := func() {
		mediaLeases.mu.Lock()
		mediaLeases.exp = map[uint64]time.Time{}
		mediaLeases.mu.Unlock()
		for {
			select {
			case <-mediaFetchSlots:
			default:
				return
			}
		}
	}
	clean()
	t.Cleanup(clean)
}

// expireLease 把某条租约的过期时刻拨到过去（模拟「调用方忘了 Close，时间过去了 TTL」）。
func expireLease(lease uint64) {
	mediaLeases.mu.Lock()
	mediaLeases.exp[lease] = time.Now().Add(-time.Second)
	mediaLeases.mu.Unlock()
}

// TestMediaSlotLeaseReclaimedWhenExpired 是 C9-MF-01 的核心用例：
// 取一个名额**不 Close**（模拟调用方漏 Close / panic 在 defer 之前），
// 把租约时间拨到过去，再取名额必须成功 —— 证明过期回收真的生效，
// 而不是「几次之后媒体抓取全卡死」。
func TestMediaSlotLeaseReclaimedWhenExpired(t *testing.T) {
	resetMediaSlots(t)
	old := mediaBusyWait
	mediaBusyWait = 50 * time.Millisecond
	t.Cleanup(func() { mediaBusyWait = old })

	leaked, err := acquireMediaSlot(context.Background())
	if err != nil {
		t.Fatalf("第一次取名额失败：%v", err)
	}
	if live, _ := mediaSlotLeaseStats(); live != 1 {
		t.Fatalf("存活租约 = %d, want 1", live)
	}
	_, before := mediaSlotLeaseStats()

	expireLease(leaked) // 漏 Close 的名额「老去」

	// 关键断言：不先 Close 那个名额，也要能取到新的。
	second, err := acquireMediaSlot(context.Background())
	if err != nil {
		t.Fatalf("过期租约没有被回收，第二次取名额失败：%v", err)
	}
	_, after := mediaSlotLeaseStats()
	if after != before+1 {
		t.Fatalf("过期回收计数 = %d, want %d（租约过期必须被强制收回）", after, before+1)
	}
	// 通道里只剩第二个名额这一枚令牌（被回收的那枚已经取走）。
	if n := len(mediaFetchSlots); n != 1 {
		t.Fatalf("名额令牌数 = %d, want 1", n)
	}
	releaseMediaSlot(second)
	if n := len(mediaFetchSlots); n != 0 {
		t.Fatalf("归还后名额令牌数 = %d, want 0", n)
	}
}

// TestMediaSlotExpiredCloseDoesNotSteal 钉住 C9-MF-03：租约被过期回收后，
// 原持有者再 Close 不能再动通道 —— 否则会把**别人**的名额一起还掉，容量被放大。
func TestMediaSlotExpiredCloseDoesNotSteal(t *testing.T) {
	resetMediaSlots(t)

	a, err := acquireMediaSlot(context.Background())
	if err != nil {
		t.Fatalf("取名额 A 失败：%v", err)
	}
	b, err := acquireMediaSlot(context.Background())
	if err != nil {
		t.Fatalf("取名额 B 失败：%v", err)
	}
	if n := len(mediaFetchSlots); n != 2 {
		t.Fatalf("令牌数 = %d, want 2", n)
	}

	expireLease(a)
	if got := mediaLeases.reclaim(time.Now()); got != 1 {
		t.Fatalf("reclaim = %d, want 1", got)
	}
	// A 的持有者姗姗来迟地 Close（等价于 releaseMediaSlot(a)）：租约已不在表里。
	releaseMediaSlot(a)
	if n := len(mediaFetchSlots); n != 1 {
		t.Fatalf("过期租约的迟到归还偷走了别人的名额：令牌数 = %d, want 1（只剩 B）", n)
	}
	if live, _ := mediaSlotLeaseStats(); live != 1 {
		t.Fatalf("存活租约 = %d, want 1", live)
	}
	releaseMediaSlot(b)
	if n := len(mediaFetchSlots); n != 0 {
		t.Fatalf("令牌数 = %d, want 0", n)
	}
}

// TestMediaSlotReleaseIsIdempotent 归还必须幂等：重复归还不能让容量变成 5、6…
func TestMediaSlotReleaseIsIdempotent(t *testing.T) {
	resetMediaSlots(t)
	old := mediaBusyWait
	mediaBusyWait = 20 * time.Millisecond
	t.Cleanup(func() { mediaBusyWait = old })

	lease, err := acquireMediaSlot(context.Background())
	if err != nil {
		t.Fatalf("取名额失败：%v", err)
	}
	releaseMediaSlot(lease)
	releaseMediaSlot(lease) // 双还
	releaseMediaSlot(lease)
	if n := len(mediaFetchSlots); n != 0 {
		t.Fatalf("重复归还后令牌数 = %d, want 0（容量被放大）", n)
	}

	// 容量仍是 4：第 5 个必须拿不到。
	held := make([]uint64, 0, cap(mediaFetchSlots))
	for i := 0; i < cap(mediaFetchSlots); i++ {
		l, err := acquireMediaSlot(context.Background())
		if err != nil {
			t.Fatalf("第 %d 个名额取不到：%v", i+1, err)
		}
		held = append(held, l)
	}
	if _, err := acquireMediaSlot(context.Background()); err != errMediaBusy {
		t.Fatalf("第 5 个名额 err = %v, want errMediaBusy", err)
	}
	for _, l := range held {
		releaseMediaSlot(l)
	}
}

// —— /metrics 口径（C9-MT-01/02/03/05 + 总计行） ——

// withFreshPromMetrics 换一份干净的 promMetrics，用例结束还原（它是包级状态）。
func withFreshPromMetrics(t *testing.T) {
	t.Helper()
	promMetrics.mu.Lock()
	oldBy, oldSince := promMetrics.by, promMetrics.since
	promMetrics.by = map[string]*promAcc{}
	promMetrics.since = time.Now()
	promMetrics.mu.Unlock()
	t.Cleanup(func() {
		promMetrics.mu.Lock()
		promMetrics.by, promMetrics.since = oldBy, oldSince
		promMetrics.mu.Unlock()
	})
}

// TestMetricsTotalIncludesCacheTokens 钉住「总计行漏累加 cache 指标」：
// 按模型的行有 CH/CM，MetricsSnapshot.Total 与全站合计序列也必须有，且数值相等。
func TestMetricsTotalIncludesCacheTokens(t *testing.T) {
	withFreshPromMetrics(t)
	noteProm(&chatStat{model: "glm-5.3", status: 200, outcome: "success", hasCache: true, cacheHit: 300, cacheMiss: 100}, time.Second)
	noteProm(&chatStat{model: "glm-5.3", status: 200, outcome: "success", hasCache: true, cacheHit: 50, cacheMiss: 50}, time.Second)
	noteProm(&chatStat{model: "deepseek-v4", status: 200, outcome: "success", hasCache: true, cacheHit: 20, cacheMiss: 30}, time.Second)

	snap := MetricsSnapshotOf()
	if snap.Total.CacheHitTokens != 370 || snap.Total.CacheMissTokens != 180 {
		t.Fatalf("Total CH/CM = %d/%d, want 370/180", snap.Total.CacheHitTokens, snap.Total.CacheMissTokens)
	}
	var sumHit, sumMiss int64
	for _, m := range snap.Models {
		sumHit += m.CacheHitTokens
		sumMiss += m.CacheMissTokens
	}
	if sumHit != snap.Total.CacheHitTokens || sumMiss != snap.Total.CacheMissTokens {
		t.Fatalf("总计行 %d/%d 与按模型求和 %d/%d 不一致", snap.Total.CacheHitTokens, snap.Total.CacheMissTokens, sumHit, sumMiss)
	}
	out := writePromMetrics(snap, nil, 0, 0, false)
	if !strings.Contains(out, "wb2api_prompt_cache_tokens_total{type=\"hit\"} 370") {
		t.Fatalf("/metrics 缺全站缓存命中合计：\n%s", out)
	}
	if !strings.Contains(out, "wb2api_prompt_cache_tokens_total{type=\"miss\"} 180") {
		t.Fatalf("/metrics 缺全站缓存未命中合计：\n%s", out)
	}
}

// TestMetricsAsyncAcceptedCountsAsSuccess 钉住 C9-MT-03：
// 视频 / 3D 的异步受理（202，且这些路径不回填 status）不能被算成 failed，
// 而显式错误 outcome 必须算 failed。
func TestMetricsAsyncAcceptedCountsAsSuccess(t *testing.T) {
	withFreshPromMetrics(t)
	noteProm(&chatStat{model: "hy-3d", status: 0, mode: "3d"}, time.Second) // 202 受理：status 未回填
	noteProm(&chatStat{model: "hy-3d", status: http.StatusAccepted, mode: "video"}, time.Second)
	noteProm(&chatStat{model: "hy-3d", status: 0, outcome: "http_error", mode: "3d"}, time.Second)
	noteProm(&chatStat{model: "hy-3d", status: http.StatusBadGateway, outcome: "http_error"}, time.Second)

	snap := MetricsSnapshotOf()
	if snap.Total.Success != 2 || snap.Total.Failed != 2 {
		t.Fatalf("success/failed = %d/%d, want 2/2（202 受理必须算成功）", snap.Total.Success, snap.Total.Failed)
	}
}

// TestMetricsModelLabelSanitized 钉住 C9-MT-05 / C9-MT-01：label 值有长度与字符集上限，
// 超长/含控制字符的模型名归到 other 桶（否则持 key 者能往 /metrics 与看板塞任意长串）。
func TestMetricsModelLabelSanitized(t *testing.T) {
	withFreshPromMetrics(t)
	noteProm(&chatStat{model: "", status: 200, outcome: "success"}, time.Second)
	noteProm(&chatStat{model: strings.Repeat("m", promModelLabelMax+1), status: 200, outcome: "success"}, time.Second)
	noteProm(&chatStat{model: "bad\x00name", status: 200, outcome: "success"}, time.Second)
	noteProm(&chatStat{model: "glm-5.3", status: 200, outcome: "success"}, time.Second)

	promMetrics.mu.Lock()
	_, hasDash := promMetrics.by["-"]
	_, hasOther := promMetrics.by[promOtherLabel]
	_, hasGlm := promMetrics.by["glm-5.3"]
	size := len(promMetrics.by)
	long := ""
	for k := range promMetrics.by {
		if len(k) > promModelLabelMax {
			long = k
		}
	}
	promMetrics.mu.Unlock()

	if !hasDash || !hasOther || !hasGlm {
		t.Fatalf("label 收敛不符合预期（-:%v other:%v glm:%v）", hasDash, hasOther, hasGlm)
	}
	if size != 3 {
		t.Fatalf("label 数量 = %d, want 3（超长名与控制字符名应合并进 other）", size)
	}
	if long != "" {
		t.Fatalf("label 超过 %d 字节：%q", promModelLabelMax, long)
	}
}

// TestMetricsEvictKeepsActiveModel 钉住 C9-MT-01：map 满时淘汰「最久没出现」的，
// 而不是把仍在服务的真实模型挤掉。
func TestMetricsEvictKeepsActiveModel(t *testing.T) {
	withFreshPromMetrics(t)

	// 填满：全部是很久以前的垃圾模型名（每个只来过 5 次）。
	promMetrics.mu.Lock()
	old := time.Now().Add(-24 * time.Hour)
	for i := 0; i < promMetricsCap; i++ {
		promMetrics.by[modelNameFor(i)] = &promAcc{requests: 5, lastSeen: old}
	}
	promMetrics.mu.Unlock()

	// 真实模型：持续有流量。
	for i := 0; i < 5; i++ {
		noteProm(&chatStat{model: "glm-5.3", status: 200, outcome: "success"}, time.Second)
	}
	// 再来一批一次性垃圾名，逐个触发淘汰。
	for i := 0; i < 40; i++ {
		noteProm(&chatStat{model: "junk-" + modelNameFor(i), status: 200, outcome: "success"}, time.Second)
	}

	promMetrics.mu.Lock()
	acc, ok := promMetrics.by["glm-5.3"]
	size := len(promMetrics.by)
	promMetrics.mu.Unlock()
	if !ok || acc == nil || acc.requests != 5 {
		t.Fatalf("活跃模型被垃圾名挤掉了（存在=%v）", ok)
	}
	if size > promMetricsCap {
		t.Fatalf("label 基数 = %d，超过上限 %d", size, promMetricsCap)
	}
}

// modelNameFor 造一个确定性的模型名（避免 fmt 依赖，便于断言）。
func modelNameFor(i int) string {
	const letters = "abcdefghij"
	return "filler-" + string(letters[i%len(letters)]) + string(letters[(i/len(letters))%len(letters)]) +
		string(letters[(i/100)%len(letters)]) + string(letters[(i/1000)%len(letters)]) + "-x"
}

// —— bodylimit（W3C8-21 / 静默截断） ——

// deadlineRecorder 记录 SetReadDeadline 的调用序列（ResponseController 直接认这个接口）。
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	mu        sync.Mutex
	deadlines []time.Time
}

func (d *deadlineRecorder) SetReadDeadline(t time.Time) error {
	d.mu.Lock()
	d.deadlines = append(d.deadlines, t)
	d.mu.Unlock()
	return nil
}

func (d *deadlineRecorder) calls() []time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]time.Time(nil), d.deadlines...)
}

// TestBodyLimitDrainResetsReadDeadline 钉住 W3C8-21：
// 排空剩余上传时设的读截止时间属于**底层连接**，排空后必须复位成零值 ——
// 否则 keep-alive 复用时下一个请求会带着这个已过期的截止时间，
// 表现为「同一条连接上的第二个请求莫名超时/被断」。
func TestBodyLimitDrainResetsReadDeadline(t *testing.T) {
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	bl := &bodyLimiter{rc: io.NopCloser(strings.NewReader(strings.Repeat("x", 4096))), limit: 1 << 20}
	bl.drain(rec, 1024)

	got := rec.calls()
	if len(got) < 2 {
		t.Fatalf("SetReadDeadline 调用 %d 次，want >= 2（设窗口 + 复位）", len(got))
	}
	if got[0].IsZero() {
		t.Fatalf("第一次设置不该是零值：%v", got[0])
	}
	if last := got[len(got)-1]; !last.IsZero() {
		t.Fatalf("排空后读截止时间没有复位（最后设为 %v），keep-alive 连接会被污染", last)
	}

	// 防御路径（body 不是 bodyLimiter）走同一个 drainTo，语义必须一致。
	rec2 := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	drainBody(rec2, httptest.NewRequest("POST", "/x", strings.NewReader(strings.Repeat("y", 4096))), 1024)
	got2 := rec2.calls()
	if len(got2) < 2 || !got2[len(got2)-1].IsZero() {
		t.Fatalf("drainBody 的读截止时间没有复位：%v", got2)
	}
}

// TestBodyLimiterNeverSilentlyTruncates 记录「bodylimit 有静默截断」这一条的实测结论：
// 超过上限时 Read 返回的是 errBodyTooLarge（可被 errors.Is/As 识别、可映射成 413），
// 不是「读一半就当 EOF」——即本文件没有静默截断问题。
func TestBodyLimiterNeverSilentlyTruncates(t *testing.T) {
	const limit = 16
	bl := &bodyLimiter{rc: io.NopCloser(strings.NewReader(strings.Repeat("z", limit+10))), limit: limit}
	raw, err := io.ReadAll(bl)
	if !bodyTooLarge(err) {
		t.Fatalf("超限读体 err = %v，want errBodyTooLarge（不得静默截断成 %d 字节）", err, len(raw))
	}
	if len(raw) <= limit {
		t.Fatalf("读到的字节 %d 不该 <= 上限 %d（否则调用方会把半截 body 当完整请求）", len(raw), limit)
	}
	if bl.over != true {
		t.Fatal("超限标记没有置位")
	}
}

// TestMetricsLabelEscaping 钉住 C9-MT-04（label 转义实现正确，记录以免后续改动破坏）：
// 客户端可控的模型名里带引号/反斜杠时，exposition 文本必须转义，
// 否则一条 label 就能伪造出额外的 metric 行。
func TestMetricsLabelEscaping(t *testing.T) {
	withFreshPromMetrics(t)
	noteProm(&chatStat{model: `q"uote\back`, status: 200, outcome: "success"}, time.Second)

	out := writePromMetrics(MetricsSnapshotOf(), nil, 0, 0, false)
	if !strings.Contains(out, `model="q\"uote\\back"`) {
		t.Fatalf("label 没有转义（引号/反斜杠会伪造出额外指标行）：\n%s", out)
	}
	// 转义后的 label 里不能出现裸引号：出现即说明伪造出了额外的 label 边界。
	if strings.Contains(out, `model="q"uote`) {
		t.Fatalf("label 边界被引号截断：\n%s", out)
	}
}

// TestMediaFetchByteBudget 钉住 C9-MF-02：名额只数并发个数，不数内存；
// 全局在途字节预算按 Content-Length 先记账，超预算即 errMediaBusy
// （调用方沿用同一条降级路径，不必新增分支）。
func TestMediaFetchByteBudget(t *testing.T) {
	allowMediaLoopback(t)
	oldBudget := mediaInFlightBudget
	mediaInFlightBudget = 100
	t.Cleanup(func() {
		mediaInFlightBudget = oldBudget
		mediaInFlightBytes.Store(0)
	})
	resetMediaSlots(t)

	release := make(chan struct{})
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "60")
		w.WriteHeader(http.StatusOK)
		// 必须 Flush：net/http 会把响应头缓冲到第一次写正文，而客户端要拿到头
		// （openMedia 返回）才会去预占在途字节。
		w.(http.Flusher).Flush()
		<-release // 卡住不吐正文：让第一笔预占留在账上
		_, _ = w.Write([]byte(strings.Repeat("p", 60)))
	}))
	defer cdn.Close()
	defer close(release) // LIFO：先放行处理器再关服务器

	go func() { _, _ = fetchMedia(context.Background(), cdn.URL+"/i.png", 1<<20) }()

	// 等第一笔 60 字节预占落账（fetchMedia 在读到正文前就已记账）。
	deadline := time.Now().Add(3 * time.Second)
	for mediaInFlightBytes.Load() < 60 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := mediaInFlightBytes.Load(); got < 60 {
		t.Fatalf("在途字节 = %d，第一笔预占没有生效", got)
	}

	// 第二笔 60 字节：60+60 > 100 → 必须被预算挡住，而不是先读进内存。
	if _, err := fetchMedia(context.Background(), cdn.URL+"/i.png", 1<<20); err != errMediaBusy {
		t.Fatalf("超预算 err = %v, want errMediaBusy", err)
	}
}
