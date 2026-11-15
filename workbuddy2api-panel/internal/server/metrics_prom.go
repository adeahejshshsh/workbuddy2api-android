// metrics_prom.go GET /metrics，Prometheus 文本 0.0.4。零第三方依赖。
// 请求计数在 chatStat.done 现记；池健康走 Pool.RealmHealth。不输出 uid。
package server

import (
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

const promMetricsCap = 512

// promEvictSample 淘汰时最多检查的条目数（审查 C9-MT-02）。
//
// 此前 map 满后每次新模型名都在全局锁内遍历 512 条找「请求数最少」的那个 ——
// 高频轮换模型名的流量会把统计路径变成串行热点（每请求一次 O(512) 扫描，且全程持锁）。
// Go 的 map 迭代起点是随机的，取前 N 个就是一份均匀抽样：O(N) 常量开销、
// 淘汰质量足够（见 promEvictVictimLocked 的取舍）。
const promEvictSample = 16

// promModelLabelMax / promOtherLabel label 收敛参数（审查 C9-MT-05 / C9-MT-01）。
//
// label 值完全由客户端的 model 字段决定：不收敛的话，持 key 者可以把任意长串
// （甚至 key 片段）当模型名写进 /metrics 与看板留存。超长或含控制字符的名字统一
// 归到 other 桶 —— 这类名字本来就进不了 modelList，面板也没有它的曲线可看。
const (
	promModelLabelMax = 64
	promOtherLabel    = "other"
)

type promAcc struct {
	requests, success, failed, streaming int64
	ttfbSum                              float64
	ttfbN                                int64
	latSum                               float64
	genSec                               float64
	genTok                               int64 // 只统计「有生成段」的行：与 genSec 同一边界，否则均值分子含非流式、分母不含，虚高
	compTok                              int64
	cacheHit, cacheMiss                  int64
	credit                               float64
	// lastSeen 最近一次有请求落在该模型上的时刻（审查 C9-MT-01）。
	// 淘汰用它而不是「请求数最少」：真实模型的曲线是**最近**还在动的，
	// 一次性垃圾名（requests=1 但早已不再出现）才是该被挤掉的。
	lastSeen time.Time
}

var promMetrics = struct {
	mu    sync.Mutex
	since time.Time
	by    map[string]*promAcc
}{since: time.Now(), by: map[string]*promAcc{}}

// promModelLabel 把客户端给的模型名收敛成 label 值（审查 C9-MT-05）。
func promModelLabel(model string) string {
	if model == "" {
		return "-"
	}
	if len(model) > promModelLabelMax {
		return promOtherLabel
	}
	for _, r := range model {
		if r < 0x20 || r == 0x7f {
			return promOtherLabel
		}
	}
	return model
}

// promFailed 判定这一行算成功还是失败（审查 C9-MT-03）。
//
// 此前只看 `status == 200`：视频 / 3D 的异步受理回 202，而这些路径根本不回填
// status（st.status 保持 0），于是「任务已受理」在 /metrics 里被算成 failed，
// 值班看板按失败率报警时误判。改成先看 outcome —— 收口处（chatStat.done）是唯一
// 同时知道语义与状态码的地方 —— 再按状态码区间兜底。
func promFailed(s *chatStat) bool {
	switch s.outcome {
	case reqlog.OutcomeHTTPError, reqlog.OutcomeStreamError, reqlog.OutcomeInterrupted:
		return true
	}
	if s.status == 0 {
		// 未回填状态：异步受理（202）/ 提前 return 的成功路径。
		return false
	}
	return s.status < 200 || s.status >= 400
}

// promEvictVictimLocked 在 map 满时挑一个最该被挤掉的模型名（调用方必须持 promMetrics.mu）。
//
// 抽样 promEvictSample 条，淘汰「最久没出现」的那条，同刻再比请求数（审查 C9-MT-01）。
// 为什么不是全表扫「请求数最少」：真实模型可能长时间零流量（例如夜间没人用），
// 用请求数当唯一判据会把它当垃圾名清掉；而 LRU 语义下「最近还在动的」一定留住，
// 攻击者刷出来的一次性名字才是首选淘汰对象。
func promEvictVictimLocked() string {
	victim := ""
	var victimSeen time.Time
	var victimReq int64
	seen := 0
	for k, v := range promMetrics.by {
		if seen >= promEvictSample {
			break
		}
		seen++
		switch {
		case victim == "":
		case v.lastSeen.Before(victimSeen):
		case v.lastSeen.Equal(victimSeen) && v.requests < victimReq:
		default:
			continue
		}
		victim, victimSeen, victimReq = k, v.lastSeen, v.requests
	}
	return victim
}

func noteProm(s *chatStat, total time.Duration) {
	model := promModelLabel(s.model)
	promMetrics.mu.Lock()
	defer promMetrics.mu.Unlock()
	mm := promMetrics.by[model]
	if mm == nil {
		if len(promMetrics.by) >= promMetricsCap {
			// 满了就**淘汰一条最久没出现的**，而不是静默丢弃新模型。此前直接 return：
			// 持 key 连发几百个不同模型名就能把 map 填满垃圾名，之后真实模型的统计
			// 全部不再更新，直到重启 —— /metrics 与面板模型统计一起失真。
			if victim := promEvictVictimLocked(); victim != "" {
				delete(promMetrics.by, victim)
			}
		}
		mm = &promAcc{}
		promMetrics.by[model] = mm
	}
	mm.lastSeen = time.Now()
	mm.requests++
	if promFailed(s) {
		mm.failed++
	} else {
		mm.success++
	}
	if s.mode == "stream" {
		mm.streaming++
	}
	totalMS := float64(total.Milliseconds())
	mm.latSum += totalMS
	gen := 0.0
	if s.ttfb > 0 {
		// 缺 TTFB 观测（非流式）时刻意不计生成时长：totalMS 含预填，混进均值会把
		// 平均速率拉成不可比的低值。与 tokensPerSecond 同一条规则。
		mm.ttfbSum += float64(s.ttfb.Milliseconds())
		mm.ttfbN++
		gen = totalMS - float64(s.ttfb.Milliseconds())
	}
	if s.toks > 0 {
		mm.compTok += int64(s.toks)
		if gen > 0 {
			mm.genSec += gen / 1000
			mm.genTok += int64(s.toks)
		}
	}
	if s.hasCache {
		mm.cacheHit += int64(s.cacheHit)
		mm.cacheMiss += int64(s.cacheMiss)
	}
	if s.hasCredit && s.credit > 0 {
		mm.credit += s.credit
	}
}

type ModelStatPayload struct {
	Model            string
	Success          int64
	Failed           int64
	Streaming        int64
	PromptTokens     int64
	CompletionTokens int64
	CacheHitTokens   int64
	CacheMissTokens  int64
	CacheWriteTokens int64
	Credit           float64
	AvgTTFBMS        float64
	AvgLatencyMS     float64
	TokensPerSec     float64
}

type MetricsSnapshot struct {
	Since  time.Time
	Total  ModelStatPayload
	Models []ModelStatPayload
}

func MetricsSnapshotOf() MetricsSnapshot {
	promMetrics.mu.Lock()
	defer promMetrics.mu.Unlock()
	out := MetricsSnapshot{Since: promMetrics.since}
	var tot promAcc
	for name, mm := range promMetrics.by {
		out.Models = append(out.Models, deriveProm(name, mm))
		tot.requests += mm.requests
		tot.success += mm.success
		tot.failed += mm.failed
		tot.streaming += mm.streaming
		tot.ttfbSum += mm.ttfbSum
		tot.ttfbN += mm.ttfbN
		tot.latSum += mm.latSum
		tot.genSec += mm.genSec
		tot.genTok += mm.genTok
		tot.compTok += mm.compTok
		// 缓存命中/未命中此前漏累加（审查 C9-MT 口径）：按模型的行有 CH/CM，
		// 总计行却恒为 0 —— 谁按 Total 算整体命中率都会得到「0 命中」。
		tot.cacheHit += mm.cacheHit
		tot.cacheMiss += mm.cacheMiss
		tot.credit += mm.credit
	}
	out.Total = deriveProm("total", &tot)
	sort.Slice(out.Models, func(i, j int) bool { return out.Models[i].Model < out.Models[j].Model })
	return out
}

func deriveProm(name string, mm *promAcc) ModelStatPayload {
	p := ModelStatPayload{
		Model: name, Success: mm.success, Failed: mm.failed, Streaming: mm.streaming,
		CompletionTokens: mm.compTok, Credit: mm.credit,
		CacheHitTokens: mm.cacheHit, CacheMissTokens: mm.cacheMiss,
	}
	if mm.ttfbN > 0 {
		p.AvgTTFBMS = mm.ttfbSum / float64(mm.ttfbN)
	}
	if mm.requests > 0 {
		p.AvgLatencyMS = mm.latSum / float64(mm.requests)
	}
	if mm.genSec > 0 {
		// 分子用 genTok（只含有生成段的行），与 genSec 同一批样本：用 compTok 会把
		// 非流式行的 token 计进分子却不算时间，均值虚高。
		p.TokensPerSec = float64(mm.genTok) / mm.genSec
	}
	return p
}

var promRealms = []struct{ label, realm string }{{"cn", "cn"}, {"global", "global"}, {"all", ""}}

var promPoolStates = []string{
	"healthy", "cooling", "disabled", "in_flight_full",
	"breaker", "degraded", "manual_disabled", "model_cooled",
}

type promRealmHealth struct {
	label string
	h     pool.RealmHealth
}

type promWriter struct {
	sb   strings.Builder
	seen map[string]bool
}

func newPromWriter() *promWriter { return &promWriter{seen: map[string]bool{}} }

func (w *promWriter) family(name, help, typ string) {
	if w.seen[name] {
		return
	}
	w.seen[name] = true
	w.sb.WriteString("# HELP ")
	w.sb.WriteString(name)
	w.sb.WriteByte(' ')
	w.sb.WriteString(strings.ReplaceAll(strings.ReplaceAll(help, `\`, `\\`), "\n", `\n`))
	w.sb.WriteByte('\n')
	w.sb.WriteString("# TYPE ")
	w.sb.WriteString(name)
	w.sb.WriteByte(' ')
	w.sb.WriteString(typ)
	w.sb.WriteByte('\n')
}

func (w *promWriter) sample(name string, value float64, labels ...string) {
	w.sb.WriteString(name)
	if len(labels) > 0 {
		w.sb.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				w.sb.WriteByte(',')
			}
			w.sb.WriteString(labels[i])
			w.sb.WriteString(`="`)
			v := strings.ReplaceAll(labels[i+1], `\`, `\\`)
			v = strings.ReplaceAll(v, `"`, `\"`)
			v = strings.ReplaceAll(v, "\n", `\n`)
			w.sb.WriteString(v)
			w.sb.WriteByte('"')
		}
		w.sb.WriteByte('}')
	}
	w.sb.WriteByte(' ')
	w.sb.WriteString(formatPromValue(value))
	w.sb.WriteByte('\n')
}

func formatPromValue(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func promPoolStateValue(h pool.RealmHealth, state string) int {
	switch state {
	case "healthy":
		return h.Healthy
	case "cooling":
		return h.Cooling
	case "disabled":
		// 口径 = 「不可用」：禁用 + 暂停选号（暂停只关选号、照常保号，但对抓指标的
		// 使用者而言同样是「这个号现在不接流量」）。RealmHealth 把两者分开计，这里
		// 显式合并，保持既有 metrics 契约逐位不变。
		return h.Disabled + h.Paused
	case "in_flight_full":
		return h.InFlightFull
	case "breaker":
		return h.Breaker
	case "degraded":
		return h.Degraded
	case "manual_disabled":
		return h.ManualDisabled
	case "model_cooled":
		return h.ModelCooled
	}
	return 0
}

func writePromMetrics(snap MetricsSnapshot, health []promRealmHealth, sticky int, explore int64, wafActive bool) string {
	w := newPromWriter()
	w.family("wb2api_pool_accounts_total", "Accounts in the pool.", "gauge")
	for _, rh := range health {
		w.sample("wb2api_pool_accounts_total", float64(rh.h.Total), "realm", rh.label)
	}
	w.family("wb2api_pool_accounts", "Accounts by realm and state.", "gauge")
	for _, rh := range health {
		for _, st := range promPoolStates {
			w.sample("wb2api_pool_accounts", float64(promPoolStateValue(rh.h, st)), "realm", rh.label, "state", st)
		}
	}
	w.family("wb2api_pool_in_flight", "In-flight requests.", "gauge")
	for _, rh := range health {
		w.sample("wb2api_pool_in_flight", float64(rh.h.InFlight), "realm", rh.label)
	}
	w.family("wb2api_sticky_sessions", "Sticky sessions.", "gauge")
	w.sample("wb2api_sticky_sessions", float64(sticky))
	w.family("wb2api_cost_explore_events_total", "costTier explore events.", "counter")
	w.sample("wb2api_cost_explore_events_total", float64(explore))
	w.family("wb2api_waf_ip_block_active", "1 when IP-level WAF block is active.", "gauge")
	if wafActive {
		w.sample("wb2api_waf_ip_block_active", 1)
	} else {
		w.sample("wb2api_waf_ip_block_active", 0)
	}
	since := 0.0
	if !snap.Since.IsZero() {
		since = float64(snap.Since.Unix())
	}
	w.family("wb2api_stats_since_timestamp_seconds", "Stats window start, unix seconds.", "gauge")
	w.sample("wb2api_stats_since_timestamp_seconds", since)
	w.family("wb2api_requests_total", "Chat requests.", "counter")
	w.sample("wb2api_requests_total", float64(snap.Total.Success), "status", "success")
	w.sample("wb2api_requests_total", float64(snap.Total.Failed), "status", "failed")
	w.family("wb2api_model_requests_total", "Chat requests by model.", "counter")
	for _, m := range snap.Models {
		w.sample("wb2api_model_requests_total", float64(m.Success), "model", m.Model, "status", "success")
		w.sample("wb2api_model_requests_total", float64(m.Failed), "model", m.Model, "status", "failed")
	}
	w.family("wb2api_model_credit_total", "Observed upstream credit by model.", "counter")
	for _, m := range snap.Models {
		w.sample("wb2api_model_credit_total", m.Credit, "model", m.Model)
	}
	w.family("wb2api_model_requests_streaming_total", "Streaming chat requests by model.", "counter")
	for _, m := range snap.Models {
		w.sample("wb2api_model_requests_streaming_total", float64(m.Streaming), "model", m.Model)
	}
	w.family("wb2api_model_tokens_total", "Tokens by model. Prompt is 0 here: the chat log does not keep it.", "counter")
	for _, m := range snap.Models {
		w.sample("wb2api_model_tokens_total", float64(m.PromptTokens), "model", m.Model, "type", "prompt")
		w.sample("wb2api_model_tokens_total", float64(m.CompletionTokens), "model", m.Model, "type", "completion")
	}
	w.family("wb2api_model_prompt_cache_tokens_total", "Upstream prompt cache tokens by model. Hit rate = hit/(hit+miss).", "counter")
	for _, m := range snap.Models {
		w.sample("wb2api_model_prompt_cache_tokens_total", float64(m.CacheHitTokens), "model", m.Model, "type", "hit")
		w.sample("wb2api_model_prompt_cache_tokens_total", float64(m.CacheMissTokens), "model", m.Model, "type", "miss")
	}
	// 总计行（审查 C9-MT 口径）：此前 /metrics 只有按模型的 CH/CM，没有全站合计，
	// 而 MetricsSnapshot.Total 里的 CH/CM 又漏累加（恒 0）。两处一起补上，
	// 抓指标的人不用再对 512 条按模型序列做 sum by (type)。
	w.family("wb2api_prompt_cache_tokens_total", "Upstream prompt cache tokens, all models. Hit rate = hit/(hit+miss).", "counter")
	w.sample("wb2api_prompt_cache_tokens_total", float64(snap.Total.CacheHitTokens), "type", "hit")
	w.sample("wb2api_prompt_cache_tokens_total", float64(snap.Total.CacheMissTokens), "type", "miss")
	w.family("wb2api_model_avg_ttfb_seconds", "Average TTFB by model.", "gauge")
	for _, m := range snap.Models {
		w.sample("wb2api_model_avg_ttfb_seconds", m.AvgTTFBMS/1000, "model", m.Model)
	}
	w.family("wb2api_model_avg_latency_seconds", "Average latency by model.", "gauge")
	for _, m := range snap.Models {
		w.sample("wb2api_model_avg_latency_seconds", m.AvgLatencyMS/1000, "model", m.Model)
	}
	w.family("wb2api_model_tokens_per_second", "Completion tokens per second by model.", "gauge")
	for _, m := range snap.Models {
		w.sample("wb2api_model_tokens_per_second", m.TokensPerSec, "model", m.Model)
	}
	return w.sb.String()
}

func (h *Handler) promMetrics(w http.ResponseWriter, r *http.Request) {
	snap := MetricsSnapshotOf()
	var health []promRealmHealth
	var explore int64
	if h.cfg.Pool != nil {
		for _, rl := range promRealms {
			health = append(health, promRealmHealth{label: rl.label, h: h.cfg.Pool.RealmHealth(rl.realm)})
		}
		explore, _ = h.cfg.Pool.CostExploreStatus()
	}
	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(writePromMetrics(snap, health, sticky, explore, h.wafIP.active())))
}

func (h *Handler) WAFActive() bool { return h.wafIP.active() }
