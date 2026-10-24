// logging.go 请求级表格日志：每个 /v1/chat/completions 请求结束后打印一行到 stdout。
package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// maxUserAgentLen 归档与面板展示保留的 UA 字节上限。UA 是客户端完全可控的
// 自由文本（浏览器动辄 150+ 字符，恶意客户端可以塞几 KB），落盘前必须截断，
// 否则一条请求就能把归档行撑大。截断只影响展示，不影响请求处理。
const maxUserAgentLen = 200

// chatSeq 进程级请求序号。
var chatSeq atomic.Int64

// chatLogEnabled 聊天表格日志总开关。生产恒 true；
// 测试包经 TestMain 置 false 关闭 stdout 噪音，需要断言行输出的测试用 withChatLog 临时开启（R5）。
var chatLogEnabled = true

// chatLogOut 聊天表格日志的输出目标。生产默认 os.Stdout；main 在启用管理面板时
// 经 SetChatLogOutput 注入 MultiWriter，把每行镜像进 /panel/api/logs 的环形缓冲，
// stdout 行为不变。需在开始服务前调用一次（无并发竞争窗口）。
var chatLogOut io.Writer = os.Stdout

// SetChatLogOutput 替换聊天表格日志输出目标（仅 main 启动期调用一次）。
func SetChatLogOutput(w io.Writer) { chatLogOut = w }

// chatStat 单个 chat 请求的日志统计；handler 挂 defer，请求出口后落一行。
type chatStat struct {
	start            time.Time
	model            string
	mode             string // "stream" | "sync"
	uid              string // 完整 uid，展示时只取前 8 位
	nick             string // 账号昵称（随选号同步），流水行经 logfmt.Label 拼成 "昵称(uid8)"
	ttfb             time.Duration
	toks             int // <0 表示 usage 缺失 → 显示 "-"
	status           int
	requestID        string
	outcome          string
	attempts         int
	credit           float64
	hasCredit        bool
	promptTokens     int64
	completionTokens int64
	totalTokens      int64
	// 缓存命中观测（usage.prompt_cache_hit_tokens / miss）：上游给到才有效。
	// 供 usage 桶命中率维度与 reqlog 逐次记录；缺失时 hasCache=false 不参与统计。
	cacheHit  int64
	cacheMiss int64
	hasCache  bool

	// 调用来源（客户端 IP / User-Agent）。空 = 未采集（logging.request_client_info
	// 关闭，或非 chat 路径），展示层一律以 "-" 兜底。
	clientIP  string
	userAgent string

	logged bool
}

// newChatStat 以请求进入 handler 的时刻为起点构造统计对象；toks 默认 -1（usage 缺失）。
func newChatStat(now time.Time, body []byte, stream bool) *chatStat {
	mode := "sync"
	if stream {
		mode = "stream"
	}
	return &chatStat{start: now, model: parseModelFromBody(body), mode: mode, toks: -1}
}

// done 幂等落一行表格日志。
func (s *chatStat) done() {
	if s.logged {
		return
	}
	s.logged = true
	logChatRowEx(s.ttfb, time.Since(s.start), s.model, s.mode, s.uid, s.nick, s.status, s.toks,
		s.requestID, s.outcome, s.attempts, s.credit, s.hasCredit, s.clientIP, s.userAgent)
}

// chatStatsReader 在流式透传时抓取 SSE 末帧的 usage.completion_tokens 精确值，
// 并记录首个 data 帧的 TTFB；原始字节原样返回给下游透传。
// 注意：不做 rune 估算，token 数一律采信上游 usage。
type chatStatsReader struct {
	br                  *bufio.Reader
	start               time.Time
	ttfb                time.Duration
	seen                bool // 已见过首个 data 帧（TTFB 只记一次）
	promptTokens        int
	completionTokens    int
	totalTokens         int
	hasPromptTokens     bool
	hasCompletionTokens bool
	hasTotalTokens      bool
	// credit 上游末帧 usage.credit（本次真实扣费积分），供成本台账（NoteModelCost）。
	hasCredit  bool
	credit     float64
	errorFrame bool
	// cacheHit/cacheMiss 上游末帧 usage.prompt_cache_hit_tokens / miss_tokens，
	// 供用量桶的命中率维度与 reqlog 逐次记录（issue #92）。
	hasCacheHit  bool
	cacheHit     int
	cacheMiss    int
	hasCacheMiss bool
	// issue #44 观测：本流是否出现过非空 delta.tool_calls、最后一个非空 finish_reason。
	// 两者组合用于识别「声明了 tools 却拿到 stop + 零 tool_calls」的静默回合。
	sawToolCalls bool
	finishReason string
	// thinkTokens 末帧 usage 里的**思考 token 数**（上游把思考算进 completion_tokens：
	// 实测 completion=100 里 91 是思考）。用于解释「速率看着慢」。
	thinkTokens int
	hasThink    bool
	// frames 本流收到的 data 帧数（不含 [DONE] 与注释行）。断流排障时「断了多少帧」
	// 比「断了」有用得多：0 帧=上游压根没开始吐，几百帧=说到一半被掐。
	frames int
	// contentBytes 累计正文字节数（delta.content 之和）。静默回合日志用。
	contentBytes int
	pend         []byte // 已读未返回的行缓存
	pendErr      error  // 与 pend 伴随的 EOF（bug 12）：必须等尾巴吐完再返回
}

// newChatStatsReaderSince 以 since 为 TTFB 计时起点（通常是请求进入 handler 的时刻）。
func newChatStatsReaderSince(r io.Reader, since time.Time) *chatStatsReader {
	return &chatStatsReader{br: bufio.NewReaderSize(r, 64*1024), start: since}
}

// TTFB 返回首个 data 帧到达耗时；无帧时为 0。
func (s *chatStatsReader) TTFB() time.Duration { return s.ttfb }

// Frames 本流已收到的 data 帧数（不含 [DONE]/注释行）。断流日志用它区分
// 「首帧之前就断」与「吐了几百帧后被掐」。
func (s *chatStatsReader) Frames() int { return s.frames }

// ContentBytes 本流累计正文字节数（delta.content 之和，不含 reasoning_content）。
// 静默回合日志用它区分「模型什么都没说」与「模型改说人话、把工具晾在一边」。
func (s *chatStatsReader) ContentBytes() int { return s.contentBytes }

// Tokens 返回末帧 usage.completion_tokens 与是否缺失；无 usage 时 ok=false。
func (s *chatStatsReader) Tokens() (int, bool) { return s.completionTokens, s.hasCompletionTokens }

// SawToolCalls 报告本流是否出现过非空 delta.tool_calls（issue #44 观测）。
func (s *chatStatsReader) SawToolCalls() bool { return s.sawToolCalls }

// FinishReason 本流最后一个非空 finish_reason；上游没给过则为空串。
func (s *chatStatsReader) FinishReason() string { return s.finishReason }

// Credit 返回末帧 usage.credit（本次真实扣费积分）与是否缺失。
func (s *chatStatsReader) Credit() (float64, bool) { return s.credit, s.hasCredit }

// TotalTokens 返回末帧 usage.total_tokens 与是否缺失。
func (s *chatStatsReader) TotalTokens() (int, bool) { return s.totalTokens, s.hasTotalTokens }

// Usage 返回流式响应中已收到的 token usage 字段。
func (s *chatStatsReader) Usage() pool.TokenUsageDelta {
	return pool.TokenUsageDelta{
		HasPromptTokens:     s.hasPromptTokens,
		PromptTokens:        int64(s.promptTokens),
		HasCompletionTokens: s.hasCompletionTokens,
		CompletionTokens:    int64(s.completionTokens),
		HasTotalTokens:      s.hasTotalTokens,
		TotalTokens:         int64(s.totalTokens),
	}
}

// ssePayload 从一行 SSE 里取出 data 负载（"data:" 后空格可选）。
// 透传层（upstream.Aggregate）与统计层（chatStatsReader）共用同一口径。
func ssePayload(line string) (string, bool) {
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "data:") {
		return "", false
	}
	return strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "), true
}

// parseSSELine 解析一行 "data: {...}"：首帧记 TTFB，含 usage 时采信精确 completion_tokens。
func (s *chatStatsReader) parseSSELine(line string) {
	line = strings.TrimRight(line, "\r\n")
	// 与透传层同口径（issue ISSUE-10）：SSE 规范里 "data:" 后的空格是可选的。
	// 只认 "data: " 时，上游发 data:{...} 会让正文正常透传、但 TTFB/token/credit
	// 全部统计不到 —— 面板与用量账本表现为"成功但 0 token"。
	payload, ok := ssePayload(line)
	if !ok {
		return
	}
	if payload == "[DONE]" {
		return
	}
	s.frames++
	if !s.seen {
		s.seen = true
		s.ttfb = time.Since(s.start)
	}
	var chunk struct {
		Error json.RawMessage `json:"error"`
		// issue #44：只取 delta.tool_calls 与 finish_reason 两个字段。encoding/json
		// 会跳过未声明的键（含 content），不为正文分配内存。
		Choices []struct {
			Delta struct {
				ToolCalls []json.RawMessage `json:"tool_calls"`
				// Content 只用来量长度（静默回合判据），不保留正文本身。
				Content string `json:"content"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens         *int     `json:"prompt_tokens"`
			CompletionTokens     *int     `json:"completion_tokens"`
			TotalTokens          *int     `json:"total_tokens"`
			Credit               *float64 `json:"credit"`
			PromptCacheHitTokens *int     `json:"prompt_cache_hit_tokens"`
			PromptCacheMissTok   *int     `json:"prompt_cache_miss_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal([]byte(payload), &chunk) != nil {
		return
	}
	if len(chunk.Error) > 0 {
		s.errorFrame = true
	}
	for _, c := range chunk.Choices {
		// 上游每帧都带 "tool_calls":[]（空数组），所以判据是长度而不是字段存在。
		if len(c.Delta.ToolCalls) > 0 {
			s.sawToolCalls = true
		}
		s.contentBytes += len(c.Delta.Content)
		if c.FinishReason != "" {
			s.finishReason = c.FinishReason
		}
	}
	if chunk.Usage == nil {
		return
	}
	if chunk.Usage.PromptTokens != nil {
		s.hasPromptTokens = true
		s.promptTokens = *chunk.Usage.PromptTokens
	}
	if chunk.Usage.CompletionTokens != nil {
		s.hasCompletionTokens = true
		s.completionTokens = *chunk.Usage.CompletionTokens
	}
	if chunk.Usage.TotalTokens != nil {
		s.hasTotalTokens = true
		s.totalTokens = *chunk.Usage.TotalTokens
	}
	if chunk.Usage.Credit != nil {
		s.hasCredit = true
		s.credit = *chunk.Usage.Credit
	}
	if chunk.Usage.PromptCacheHitTokens != nil {
		s.hasCacheHit = true
		s.cacheHit = *chunk.Usage.PromptCacheHitTokens
	}
	if chunk.Usage.PromptCacheMissTok != nil {
		s.hasCacheMiss = true
		s.cacheMiss = *chunk.Usage.PromptCacheMissTok
	}
}

// CacheTokens 返回末帧 usage 的缓存命中 / 未命中 token 数。miss 缺失时按
// prompt - hit 推导；hit 与 miss 均不可得时 ok=false（不参与命中率统计）。
func (s *chatStatsReader) CacheTokens() (hit, miss int64, ok bool) {
	// 对称推导（上游 issue #170）：miss 缺失时按 prompt-hit 推；反过来，hit 为 0
	// 或缺失但 miss 可信（prompt > miss，即确有命中量）时按 prompt-miss 反推 hit。
	// 不反推的话，上游「接近全命中时回 hit=0 + miss=3」会被误判成整段未命中。
	if !s.hasCacheHit && !s.hasCacheMiss {
		return 0, 0, false
	}
	hit = int64(s.cacheHit)
	miss = int64(s.cacheMiss)
	if !s.hasCacheMiss {
		if !s.hasPromptTokens || s.promptTokens < s.cacheHit {
			// miss 与 prompt 都推不出来 → **不参与命中率统计**（报告 #14）。
			// 此前返回 (hit, 0, true)，分母只有 hit，命中率恒 100%，
			// noteCacheTokens 还会在没有任何 miss 观测时把模型记成 everHit。
			return 0, 0, false
		}
		miss = int64(s.promptTokens - s.cacheHit)
	} else if s.cacheHit == 0 && s.hasPromptTokens && s.promptTokens > s.cacheMiss {
		hit = int64(s.promptTokens - s.cacheMiss)
	}
	return hit, miss, true
}

// cacheTokensFromUsage 从非流式聚合响应的 usage 里取缓存命中/未命中，
// 与流式侧 CacheTokens 的推导口径对称（上游 issue #170）。
func cacheTokensFromUsage(usage map[string]any) (hit, miss int64, ok bool) {
	h, okH := upstream.UsageCacheHitTokens(usage)
	m, okM := upstream.UsageCacheMissTokens(usage)
	switch {
	case okH:
		// miss 缺失时按 prompt-hit 推导（与流式侧 CacheTokens 同口径，报告 #4）：
		// 此前直接返回 m 的零值 → hit/(hit+0) 恒 100%，缓存告警也永不触发。
		if okM {
			return int64(h), int64(m), true
		}
		if p, okP := upstream.UsagePromptTokens(usage); okP && p >= h {
			return int64(h), int64(p - h), true
		}
		return int64(h), 0, false // 推不出 miss：不参与命中率统计，避免假 100%
	case okM:
		if p, okP := upstream.UsagePromptTokens(usage); okP && p > m {
			return int64(p - m), int64(m), true
		}
		return 0, int64(m), true
	}
	return 0, 0, false
}

// SawErrorFrame 报告流中是否透传过 SSE error 帧。
func (s *chatStatsReader) SawErrorFrame() bool { return s.errorFrame }

// Read 返回原始数据，同时解析统计 TTFB/token。
func (s *chatStatsReader) Read(p []byte) (int, error) {
	// 空 buffer 显式短路（bug #10）：io.Reader 的约定是 len(p)==0 时应返回 (0,nil)，
	// 写清楚是为了别让阅读者以为下面那句 copy 的 0 是丢数据。
	if len(p) == 0 {
		return 0, nil
	}
	if len(s.pend) > 0 {
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		if len(s.pend) == 0 {
			// 尾巴吐完，这才轮到把寄存的 EOF 交出去（bug 12）
			err := s.pendErr
			s.pendErr = nil
			return n, err
		}
		return n, nil
	}
	line, err := s.br.ReadString('\n')
	if line != "" {
		s.parseSSELine(line)
		n := copy(p, line)
		if n < len(line) {
			s.pend = []byte(line[n:])
			// 尾巴没吐完就**不能**返回 EOF（bug 12）：调用方拿到 (n, io.EOF) 会认为
			// 流结束，pend 里剩下的字节永远读不出去（v1.28.3 的回归 —— 当时只想着
			// 「数据与错误一起返回」，漏了 pend 的存在）。EOF 寄存到下一次调用。
			s.pendErr = err
			return n, nil
		}
		s.pend = nil
		s.pendErr = nil
		return n, err
	}
	return 0, err
}

// rewriteModel 把 outbound chat body 的 model 字段替换为 bare（保留其余字段原样）。
// 仅当 bare != 原 model 时由 chatCompletions 调用；body 不可解析时原样返回（不二次错误化）。
func rewriteModel(body []byte, bare string) []byte {
	if len(body) == 0 || bare == "" {
		return body
	}
	// UseNumber 保住数字原文（bug 11）：默认解码把整数落成 float64，超过 2^53 的
	// seed/微秒时间戳会回写成 9007199254740992 甚至科学计数法。对「透传型」网关
	// 来说这属于静默改写，排查时对不上客户端实际发出的 JSON。
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return body
	}
	if cur, ok := obj["model"].(string); !ok || cur == bare {
		return body
	}
	obj["model"] = bare
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// parseModelFromBody 从请求 JSON 取 model 字段，缺省标 "-"。
func parseModelFromBody(body []byte) string {
	var obj struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &obj); err != nil || obj.Model == "" {
		return "-"
	}
	return obj.Model
}

// usageDeltaFromResponse 从非流式聚合响应中提取明确存在的 token 字段。
func usageDeltaFromResponse(resp map[string]any) pool.TokenUsageDelta {
	delta := pool.TokenUsageDelta{}
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return delta
	}
	read := func(key string) (int64, bool) {
		v, ok := u[key]
		if !ok {
			return 0, false
		}
		switch n := v.(type) {
		case float64:
			return int64(n), true
		case float32:
			return int64(n), true
		case int:
			return int64(n), true
		case int64:
			return n, true
		case json.Number:
			i, err := n.Int64()
			return i, err == nil
		default:
			return 0, false
		}
	}
	if n, ok := read("prompt_tokens"); ok {
		delta.HasPromptTokens, delta.PromptTokens = true, n
	}
	if n, ok := read("completion_tokens"); ok {
		delta.HasCompletionTokens, delta.CompletionTokens = true, n
	}
	if n, ok := read("total_tokens"); ok {
		delta.HasTotalTokens, delta.TotalTokens = true, n
	}
	return delta
}

// completionTokens 从 Aggregate 返回的响应中提取 usage.completion_tokens；缺失返回 -1。
func completionTokens(resp map[string]any) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return -1
	}
	v, ok := u["completion_tokens"].(float64)
	if !ok {
		return -1
	}
	return int(v)
}

// uidPrefix 只显示 uid 前 8 位；空 uid 显示 "-"。
//
// 实现委托 logfmt.UID8，避免 "截 8 位" 的规则在 server 与 logfmt 两处各写一份而走样。
func uidPrefix(uid string) string {
	return logfmt.UID8(uid)
}

type requestTraceKey struct{}

// requestTrace 在一次 chat 请求内共享标识与最终统计，ServeHTTP 出口统一记账。
type requestTrace struct {
	id    string
	start time.Time
	stat  *chatStat
	// 调用来源，进入 handler 时一次性采集（见 ServeHTTP / captureClientInfo）。
	clientIP  string
	userAgent string
}

// captureClientInfo 采集调用来源（客户端 IP + 截断后的 UA）。开关关闭时保持空串：
// 来源信息比 token 计数敏感，是否落盘由 logging.request_client_info 决定。
func (t *requestTrace) captureClientInfo(r *http.Request) {
	if t == nil || r == nil {
		return
	}
	t.clientIP = clientIPForLog(r)
	t.userAgent = logfmt.Truncate(r.UserAgent(), maxUserAgentLen)
}

// clientIPForLog 提取用于日志展示的客户端 IP。
//
// 与 upstream.ExtractClientIP 的差别：后者只认代理头（X-Forwarded-For 首段 →
// X-Real-IP），因为它的用途是把客户端 IP **透传给上游**，回落到网关自身地址会
// 污染上游风控判据；日志场景相反——直连（无反代）时 RemoteAddr 就是唯一线索，
// 必须回落，否则面板里所有来源都显示 "-"。代理头优先保证反代后拿到真实客户端。
func clientIPForLog(r *http.Request) string {
	if r == nil {
		return ""
	}
	if ip := upstream.ExtractClientIP(r); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}

// dashIfEmpty 空串统一显示 "-"（来源字段未采集时不留空白列）。
func dashIfEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func requestTraceFrom(r *http.Request) *requestTrace {
	if r == nil {
		return nil
	}
	tr, _ := r.Context().Value(requestTraceKey{}).(*requestTrace)
	return tr
}

func (t *requestTrace) event(status int) reqlog.Event {
	e := reqlog.Event{
		Time:      t.start,
		RequestID: t.id,
		Path:      "/v1/chat/completions",
		Status:    status,
	}
	duration := time.Since(t.start)
	e.DurationMs = duration.Milliseconds()
	if e.DurationMs < 1 {
		e.DurationMs = 1
	}
	if t.stat != nil {
		s := t.stat
		e.Account = logfmt.Label(s.uid, s.nick)
		e.Model = s.model
		e.Outcome = s.outcome
		e.TTFBMs = s.ttfb.Milliseconds()
		e.Attempts = s.attempts
		e.PromptTokens = s.promptTokens
		e.CompletionTokens = s.completionTokens
		e.TotalTokens = s.totalTokens
		e.Credit = s.credit
		e.HasCredit = s.hasCredit
		e.CacheHitTokens = s.cacheHit
		e.CacheMissTokens = s.cacheMiss
	}
	e.ClientIP = t.clientIP
	e.UserAgent = t.userAgent
	if e.Outcome == "" {
		if status >= 200 && status < 300 {
			e.Outcome = reqlog.OutcomeSuccess
		} else {
			e.Outcome = reqlog.OutcomeHTTPError
		}
	}
	e.OK = status >= 200 && status < 300 && e.Outcome == reqlog.OutcomeSuccess
	return e
}

// responseObserver 捕获 handler 实际写出的 HTTP 状态，同时保留 Flusher/Unwrap，
// 避免破坏 SSE 逐帧刷新。
type responseObserver struct {
	http.ResponseWriter
	status int
}

func (o *responseObserver) WriteHeader(code int) {
	if o.status == 0 {
		o.status = code
	}
	o.ResponseWriter.WriteHeader(code)
}

func (o *responseObserver) Write(p []byte) (int, error) {
	if o.status == 0 {
		o.status = http.StatusOK
	}
	return o.ResponseWriter.Write(p)
}

func (o *responseObserver) Flush() {
	if f, ok := o.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (o *responseObserver) Unwrap() http.ResponseWriter { return o.ResponseWriter }

// 请求流水行的固定列宽（显示列宽，非字节）。取固定宽度而不是让内容自然长度撑开，
// 是为了让 stdout 里成百上千行能竖着扫——否则模型名长短不一、中文昵称按字节补空格
// 错位，根本没法用肉眼对齐着一列列看（这正是上一版 11 字节硬截断要解决的问题）。
const (
	// chatModelWidth 覆盖 realm 前缀 + 最长模型名："global:" (7) + "deepseek-v4.1-flash" (19) = 26。
	// 旧的 11 字节截断会把 "cn:deepseek-v4-flash" 切成 "cn:deepseek"，让人误以为是另一个模型。
	chatModelWidth = 26
	// chatAcctWidth 容纳 "昵称(uid8)"：中文昵称按 2 列/字算，5 字中文 + "(xxxxxxxx)" = 20 列。
	chatAcctWidth = 22
	chatTTFBWidth = 8
	chatTokWidth  = 6
	chatRateWidth = 11 // 形如 "183.6tok/s"
)

// logChatRow 打印一行请求级表格日志（输出 chatLogOut，无 log 时间戳前缀）。
//
// 参数：
//   - model：模型名（含 realm 前缀），超 chatModelWidth 截断（模型名是 ASCII，字节截即列宽）；
//   - uid/nick：完整 uid 与账号昵称，经 logfmt.Label 拼成 "昵称(uid8)" 展示——只有
//     uid8 时人眼无法判断是哪个号，要辨认必须再查 auths/，排障多一跳；
//   - toks<0 表示 usage 缺失，显示 "-"。
func logChatRow(ttfb, total time.Duration, model, mode, uid, nick string, status int, toks int) {
	logChatRowEx(ttfb, total, model, mode, uid, nick, status, toks, "", "", 0, 0, false, "", "")
}

// logChatRowEx 是带请求 ID、结果、重试、积分与调用来源字段的扩展流水行。旧调用保持
// 原格式；requestID 非空时才追加扩展字段；来源两参均为空时不追加来源段。
func logChatRowEx(ttfb, total time.Duration, model, mode, uid, nick string, status int, toks int,
	requestID, outcome string, attempts int, credit float64, hasCredit bool,
	clientIP, userAgent string) {
	if !chatLogEnabled {
		return
	}
	seq := chatSeq.Add(1)
	model = logfmt.Pad(logfmt.Truncate(model, chatModelWidth), chatModelWidth)
	// 账号标签只补不截：超宽时宁可让该行变宽，也不丢昵称信息（昵称是排查的主线索）。
	acct := logfmt.Pad(logfmt.Label(uid, nick), chatAcctWidth)
	tokField := "-"
	tokpsField := "-"
	if toks >= 0 {
		tokField = fmt.Sprintf("%d", toks)
		// 速率与用量账本走同一个函数（扣掉 TTFB）。此前这里自己除 total，漏扣首
		// token 等待，于是控制台流水行的 tok/s 与面板数字对不上（issue #34）——
		// 本函数本来就收到了 ttfb，只是没拿它算速率。
		// 算不出速率就保持 "-"（issue B15）：同一列同一语义（无观测）不能出现两种
		// 表示 —— 此前这里打 "0.0tok/s"，运维会把"没测到"读成"测得 0"。
		if tps, ok := tokensPerSecond(int64(toks), total, ttfb); ok {
			tokpsField = fmt.Sprintf("%.1ftok/s", tps)
		}
	}
	ttfbMS := "-"
	if ttfb > 0 {
		ttfbMS = fmt.Sprintf("%dms", ttfb.Milliseconds())
	}
	extra := ""
	if requestID != "" {
		if outcome == "" {
			outcome = reqlog.OutcomeHTTPError
			if status >= 200 && status < 300 {
				outcome = reqlog.OutcomeSuccess
			}
		}
		creditField := "-"
		if hasCredit {
			creditField = fmt.Sprintf("%.4f", credit)
		}
		extra = fmt.Sprintf(" rid=%s | out=%s | try=%d | credit=%s |", requestID, outcome, attempts, creditField)
	}
	// 调用来源：IP 用可解析的裸值（便于 grep），UA 用 ShortUA 压缩后的客户端标签
	// 并加引号（标签内可能含空格，如 `OpenAI/Python 1.30.0` 只会取到 OpenAI/Python）。
	// 两者都未采集时不追加，旧行格式保持不变。
	src := ""
	if clientIP != "" || userAgent != "" {
		ua := "-"
		if s := logfmt.ShortUA(userAgent); s != "" {
			ua = `"` + s + `"`
		}
		src = fmt.Sprintf(" src=%s ua=%s |", dashIfEmpty(clientIP), ua)
	}
	fmt.Fprintf(chatLogOut, "| #%03d | %s | %s | %s | %d | %s | TTFB=%s | tok=%s | %s | total=%.1fs |%s%s\n",
		seq,
		time.Now().Format("15:04:05"),
		model,
		mode,
		status,
		acct,
		logfmt.Pad(ttfbMS, chatTTFBWidth),
		logfmt.Pad(tokField, chatTokWidth),
		logfmt.Pad(tokpsField, chatRateWidth),
		total.Seconds(),
		extra,
		src,
	)
}

// cacheMissSignal 低命中率告警信号（issue #92 P1 轻量化）：同一模型此前观测到过
// 命中、本次大前缀（≥1000 tok）整段未命中（命中率 <10%）时打一条 WARN，每模型
// 10 分钟冷却。只报「本可命中却重算」的可行动信号——新模型/新会话的首请求天然
// 全 miss，不在此列。
type cacheMissSignal struct {
	mu      sync.Mutex
	everHit map[string]bool
	last    map[string]time.Time
}

var cacheMissWarn = &cacheMissSignal{everHit: map[string]bool{}, last: map[string]time.Time{}}

// cacheHitWarnFloor 命中率低于这个比例才算「大面积失效」（注释承诺的 10%）。
const cacheHitWarnFloor = 0.10

// cacheMissWarnEvery 冷却间隔；变量供测试缩短。
var cacheMissWarnEvery = 10 * time.Minute

// noteCacheTokens 记录一次观测：有命中 → 标记该模型可命中；整段未命中且此前
// 命中过 → 触发 WARN（冷却内不重复）。
func (c *cacheMissSignal) noteCacheTokens(model string, prompt, hit, miss int64) {
	if model == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// 按命中率判定，而不是「hit>0 就当作健康」（bug 10）：原实现只要本次有 1 个
	// 命中就 return，于是命中率 1%~9% 这种「前缀缓存大面积失效」的形态永远不告警
	// —— 而那恰恰是费用异常最该提示的场景（注释承诺的就是 <10%）。
	if miss < 0 {
		miss = 0
	}
	total := hit + miss
	if total <= 0 {
		return
	}
	rate := float64(hit) / float64(total)
	if rate >= cacheHitWarnFloor {
		c.everHit[model] = true // 只在该模型确实能命中时置位（语义与注释一致）
		return
	}
	// 小前缀（<1000 tok）的 miss 无告警价值
	if prompt < 1000 {
		return
	}
	if !c.everHit[model] {
		return
	}
	now := time.Now()
	if t, ok := c.last[model]; ok && now.Sub(t) < cacheMissWarnEvery {
		return
	}
	c.last[model] = now
	log.Printf("WARN: [server] cache miss: model=%s 本次大前缀命中率 %.1f%%（prompt=%d hit=%d miss=%d，此前观测到过命中）；上游前缀缓存重算，费用会显著升高", model, rate*100, prompt, hit, miss)
}
