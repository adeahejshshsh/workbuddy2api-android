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
)

const promMetricsCap = 512

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
}

var promMetrics = struct {
	mu    sync.Mutex
	since time.Time
	by    map[string]*promAcc
}{since: time.Now(), by: map[string]*promAcc{}}

func noteProm(s *chatStat, total time.Duration) {
	model := s.model
	if model == "" {
		model = "-"
	}
	promMetrics.mu.Lock()
	defer promMetrics.mu.Unlock()
	mm := promMetrics.by[model]
	if mm == nil {
		if len(promMetrics.by) >= promMetricsCap {
			return
		}
		mm = &promAcc{}
		promMetrics.by[model] = mm
	}
	mm.requests++
	if s.status == http.StatusOK {
		mm.success++
	} else {
		mm.failed++
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
