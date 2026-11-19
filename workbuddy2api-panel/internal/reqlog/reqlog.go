// Package reqlog 记录脱敏的请求级指标与可选 JSONL 归档。
//
// 内存指标有界保存最近 100 条并维护进程级计数；磁盘归档只写请求元数据，
// 不写提示词、响应正文、Authorization 或其它凭证。可选的调用来源（客户端 IP /
// User-Agent，见 Event.ClientIP/UserAgent）由 server 按配置开关决定是否填充。
// 归档队列满时丢弃并计数，不允许日志写盘阻塞模型请求。
package reqlog

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	recentLimit     = 100
	defaultFileMax  = int64(16 << 20)
	defaultQueue    = 1024
	defaultReadMax  = 1000
	archiveFileGlob = "requests-*.jsonl"

	// defaultRetentionDays 归档保留天数缺省值（审查 C7-017）。
	// 配置项是 logging.request_retention_days（缺省 7），这里兜住「直接 New(Config{})
	// 或配了 <=0」的情形 —— 没有这个兜底，retention<=0 会让 cutoff 落在今天，
	// 启动清理把**当天所有归档**删光。
	defaultRetentionDays = 7

	// maxEventBytes 单条归档事件的字节硬上限（审查 C7-014 P1）。
	//
	// 为什么必须有：开了 request_archive_context 时 Context 可以是整段对话（MB 级），
	// 而轮转判据是「写完这条会不会超过 FileMaxBytes」。一条 8MB 的事件意味着
	// **每条**事件都触发一次轮转、每条一个新文件 —— 文件数与 inode 无界增长，
	// 磁盘几天就满。截断到 1MiB 后（Context 换成 truncated 标记）单文件最多
	// 超出一条事件的长度，文件数重新被 MaxBytes/FileMaxBytes 夹住。
	maxEventBytes = 1 << 20

	// closeFlushTimeout Close 等写循环落盘的上限（审查 C7-021）。
	// 存储满 / 慢卡时写循环会卡在磁盘 IO 上，无限等待等于把进程退出也拖住
	// （run.sh 的停机预算是 8s，超时即被强杀，反而丢更多）。
	closeFlushTimeout = 2 * time.Second

	// statsCacheTTL Snapshot 归档统计的缓存时长（审查 C7-023）。
	// 面板每 2s 轮询一次，每次 ReadDir + 全量 Stat 在文件上千时是持续的无谓 IO。
	statsCacheTTL = 5 * time.Second
)

// readScanFileBudget / readScanByteBudget 面板检索的扫描预算（审查 C7-018）。
//
// 窄 filter（按 IP / UA 搜）命中稀疏时，read 会从最新文件一路回溯到读完整目录
// （默认 100MiB），每块都做 JSON 解析。手机端一次「运行日志」搜索就能把 CPU/IO
// 打满，所以给文件数与字节数都设上限，超预算即返回已读到的部分。
// var 而不是 const：测试把它压小，避免真造上百个归档文件。
var (
	readScanFileBudget = 128
	readScanByteBudget = int64(128 << 20)
)

// NewRequestID 生成不含用户信息的本地请求 ID。
func NewRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return fmt.Sprintf("req-%x", b[:])
	}
	return fmt.Sprintf("req-%d", time.Now().UnixNano())
}

const (
	OutcomeSuccess     = "success"
	OutcomeHTTPError   = "http_error"
	OutcomeStreamError = "stream_error"
	OutcomeInterrupted = "interrupted"
)

// Config 归档参数。Enabled=false 时仍保留内存指标。
type Config struct {
	Dir           string
	Enabled       bool
	RetentionDays int
	MaxBytes      int64
	FileMaxBytes  int64
	QueueSize     int
}

// Event 是一条脱敏请求记录。Account 只保存“昵称(uid8)”标签，不保存完整 UID。
//
// ClientIP / UserAgent 是**调用来源**：面板「运行日志」用它回答"这条请求是谁打进来的"。
// 二者由 server 侧按 logging.request_client_info 开关决定是否填充（关掉即保持空串，
// 归档里不会出现来源字段）——来源信息比 token 计数敏感，运营可自行决定是否落盘。
// 仍然不写提示词、响应正文、Authorization 或其它凭证。
type Event struct {
	Time      time.Time `json:"time"`
	RequestID string    `json:"request_id"`
	Path      string    `json:"path"`
	Account   string    `json:"account,omitempty"`
	Model     string    `json:"model,omitempty"`
	// ModelEff 模型降级（pool.model_fallback）后**实际**发往上游的模型名；
	// 与 Model 不同才填充。记录的是出站线协议原值（通常裸名），不造域前缀——
	// 造前缀会让「面板显示的名字」与「上游真正收到的名字」不一致，排障时对不上。
	ModelEff string `json:"model_eff,omitempty"`
	// Realm 实际服务本次请求的账号所在域（cn / global）。cn 与 global 有同名模型，
	// 而 Model / ModelEff 常是不带域前缀的裸名：面板的模型可用性靠它区分
	// 「这条成功流量属于哪个域的模型」，缺了它裸名只能两边都认。空 = 没选到账号。
	Realm            string `json:"realm,omitempty"`
	Status           int    `json:"status"`
	OK               bool   `json:"ok"`
	Outcome          string `json:"outcome"`
	DurationMs       int64  `json:"duration_ms"`
	TTFBMs           int64  `json:"ttfb_ms,omitempty"`
	Attempts         int    `json:"attempts,omitempty"`
	PromptTokens     int64  `json:"prompt_tokens,omitempty"`
	CompletionTokens int64  `json:"completion_tokens,omitempty"`
	TotalTokens      int64  `json:"total_tokens,omitempty"`
	// ToolsDeclared / ToolCalls 本轮的「工具面」观测量（issue #44）：声明了几个工具、
	// 实际拿到几个工具调用。ToolsDeclared>0 且 ToolCalls==0 就是一次「零工具调用
	// 收尾」—— 上游把本该调工具的回合当成正常完成，客户端据此认为任务做完、工具一次
	// 没跑。只记数量，不记工具名与参数。
	ToolsDeclared int `json:"tools_declared,omitempty"`
	ToolCalls     int `json:"tool_calls,omitempty"`
	// Credit 不能 omitempty（报告 #16）：上游回 credit=0（免费模型/限免时段）时
	// 键会消失，前端 Number(undefined) → NaN → 渲染成「—」（未观测），
	// 把「实测 0 credit」显示成「没观测到」。存在性由 HasCredit 表达。
	Credit    float64 `json:"credit"`
	HasCredit bool    `json:"credit_known"`
	// CacheHitTokens / CacheMissTokens 上游前缀缓存命中/未命中 token（issue #92）。
	// 上游未回该维度时两者皆零值省略；hit=0 + miss>0 即整段未命中。
	CacheHitTokens  int64  `json:"cache_hit_tokens,omitempty"`
	CacheMissTokens int64  `json:"cache_miss_tokens,omitempty"`
	ClientIP        string `json:"client_ip,omitempty"`
	UserAgent       string `json:"user_agent,omitempty"`
	// Context 本轮会话上下文（system + messages / 出图 prompt 原文），由
	// logging.request_archive_context 决定是否填充；关掉时保持 nil，事件里不出现该字段。
	// 比 IP/UA 敏感得多（是用户的实际对话内容），所以单独一个开关、缺省关闭。
	Context json.RawMessage `json:"context,omitempty"`
}

// Filter 用于从归档中筛选最近记录。字符串字段一律「包含」匹配（大小写不敏感），
// 便于面板用一段 IP 前缀或 UA 片段捞请求；From/To 是闭区间（零值 = 该侧不设界），
// 供「今天 / 近 7 天 / 自定义区间」这类时间查询使用。
type Filter struct {
	Outcome   string
	Account   string
	Model     string
	ClientIP  string
	UserAgent string
	From      time.Time
	To        time.Time
}

// ArchiveStats 归档存储状态。
type ArchiveStats struct {
	Enabled       bool   `json:"enabled"`
	Dir           string `json:"dir,omitempty"`
	Files         int    `json:"files"`
	Bytes         int64  `json:"bytes"`
	DroppedWrites uint64 `json:"dropped_writes"`
	LastError     string `json:"last_error,omitempty"`
	// TruncatedWrites 因超过 maxEventBytes 被截断（丢 Context）后仍落盘的条数（审查 C7-014）。
	TruncatedWrites uint64 `json:"truncated_writes,omitempty"`
	// OversizedWrites 截断后仍然超限、只能整条丢弃的条数（不该发生，出现即要查）。
	OversizedWrites uint64 `json:"oversized_writes,omitempty"`
	// ReadErrors 面板检索时读失败的归档文件次数（审查 C7-026）：此前静默 continue，
	// 「少了数据」和「文件坏了」在看板上分不开。
	ReadErrors uint64 `json:"read_errors,omitempty"`
}

// Snapshot 一次面板读取的完整指标快照。
type Snapshot struct {
	StartedAt       time.Time    `json:"started_at"`
	Completed       int64        `json:"completed"`
	InFlight        int64        `json:"in_flight"`
	Succeeded       int64        `json:"succeeded"`
	Failed          int64        `json:"failed"`
	SuccessRate     float64      `json:"success_rate"`
	HTTPSuccessRate float64      `json:"http_success_rate"`
	AvgDurationMs   float64      `json:"avg_duration_ms"`
	Recent          []Event      `json:"recent"`
	Archive         ArchiveStats `json:"archive"`
}

// Recorder 并发安全的有界请求指标与归档记录器。
type Recorder struct {
	mu          sync.Mutex
	started     time.Time
	inFlight    int64
	completed   int64
	succeeded   int64
	httpSuccess int64
	durationSum int64
	recent      []Event
	recentNext  int // 环写指针：recent 满后，下一次覆盖的位置（= 当前最旧的元素）
	archive     *archiveWriter
}

// New 创建记录器；Dir 为空或 Enabled=false 时只启用内存指标。
func New(cfg Config) *Recorder {
	if cfg.FileMaxBytes <= 0 {
		cfg.FileMaxBytes = defaultFileMax
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueue
	}
	r := &Recorder{started: time.Now(), recent: make([]Event, 0, recentLimit)}
	r.archive = newArchiveWriter(cfg)
	return r
}

// Begin 标记一个请求进入处理。
func (r *Recorder) Begin() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.inFlight++
	r.mu.Unlock()
}

// Record 记录一个请求完成事件并写入归档队列。
func (r *Recorder) Record(e Event) {
	if r == nil {
		return
	}
	// 毒值入口统一消毒（W5-02）：Credit 来自上游响应，NaN/±Inf 会让整个
	// Snapshot 的 json.Marshal 永久失败 —— /status 与面板运行日志 500，直到
	// 这条事件被 100 条新事件挤出。必须在入环前清掉，归档路径也才拿得到干净值。
	e = sanitizeEvent(e)
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	if e.Outcome == "" {
		if e.Status == 200 && e.OK {
			e.Outcome = OutcomeSuccess
		} else {
			e.Outcome = OutcomeHTTPError
		}
	}
	r.mu.Lock()
	if r.inFlight > 0 {
		r.inFlight--
	}
	r.completed++
	if e.OK {
		r.succeeded++
	}
	if e.Status >= 200 && e.Status < 300 {
		r.httpSuccess++
	}
	r.durationSum += e.DurationMs
	// 真环写（bug #14）：此前虽然不再 append 整个切片，但仍要
	// `copy(r.recent, r.recent[1:])` —— 持锁状态下搬 99 个 Event（20 多个字段），
	// 高 QPS 下是每请求一次的 O(n) 无谓搬运。改成下标覆盖：写 O(1)，
	// Snapshot 时按环序展开一次。
	if len(r.recent) < recentLimit {
		r.recent = append(r.recent, e)
	} else {
		r.recent[r.recentNext] = e
		r.recentNext = (r.recentNext + 1) % recentLimit
	}
	r.mu.Unlock()
	if r.archive != nil {
		r.archive.enqueue(e)
	}
}

// recentInOrderLocked 把环写缓冲展开成"旧→新"顺序（调用方必须持 r.mu）。
// 未写满时就是底层切片本身；写满后从最旧的槽位（recentNext）开始绕一圈。
func (r *Recorder) recentInOrderLocked() []Event {
	if len(r.recent) < recentLimit || r.recentNext == 0 {
		return r.recent
	}
	out := make([]Event, 0, len(r.recent))
	out = append(out, r.recent[r.recentNext:]...)
	out = append(out, r.recent[:r.recentNext]...)
	return out
}

// Snapshot 返回进程内指标和归档状态。
func (r *Recorder) Snapshot() Snapshot {
	if r == nil {
		return Snapshot{}
	}
	r.mu.Lock()
	// 第三道口（W5-02）：环里可能存在本进程升级前/测试注入的历史毒值，
	// 序列化前再消毒一次，保证 Snapshot 永远可 marshal。
	recent := reversedEvents(r.recentInOrderLocked())
	for i := range recent {
		recent[i] = sanitizeEvent(recent[i])
	}
	s := Snapshot{
		StartedAt: r.started,
		Completed: r.completed,
		InFlight:  r.inFlight,
		Succeeded: r.succeeded,
		// 内部环序展开成"旧→新"，对外契约是"新→旧"（bug #17/#14）。
		Recent: recent,
	}
	if r.completed > 0 {
		s.Failed = r.completed - r.succeeded
		s.SuccessRate = round1(float64(r.succeeded) / float64(r.completed) * 100)
		s.HTTPSuccessRate = round1(float64(r.httpSuccess) / float64(r.completed) * 100)
		s.AvgDurationMs = float64(r.durationSum) / float64(r.completed)
	}
	r.mu.Unlock()
	if r.archive != nil {
		s.Archive = r.archive.stats()
	}
	return s
}

// ReadArchive 返回最近的归档事件（按时间倒序）。limit<=0 时回落 200，最大 1000。
func (r *Recorder) ReadArchive(limit int, filter Filter) ([]Event, error) {
	if r == nil || r.archive == nil {
		return nil, nil
	}
	return r.archive.read(limit, filter)
}

// Close 刷盘并停止后台归档。
func (r *Recorder) Close() {
	if r == nil || r.archive == nil {
		return
	}
	r.archive.close()
}

// sanitizeEvent 把非有限的 Credit 归零并标记为“未观测”（W5-02）。
// 姊妹包 usage 已有同款 sanitizeNonFinite；这里只处理唯一一个浮点业务字段，
// 用显式判断而不是 JSON 往返（快、且不会误伤其它字段）。
func sanitizeEvent(e Event) Event {
	if math.IsNaN(e.Credit) || math.IsInf(e.Credit, 0) {
		e.Credit = 0
		e.HasCredit = false
	}
	return e
}

func round1(v float64) float64 {
	return float64(int(v*10+0.5)) / 10
}

type archiveWriter struct {
	cfg  Config
	ch   chan Event
	stop chan struct{}
	done chan struct{}
	// purgeCh 手动清空归档：删文件必须由写循环执行（它是 file/buf 的唯一持有者），
	// 否则会把正在写的文件删成「已删除但仍在写」，日志从此静默丢失。
	purgeCh   chan chan PurgeResult
	closeOnce sync.Once
	// enqMu 让「检查 enabled + 入队」与 close 互斥（bug #8）。
	enqMu  sync.RWMutex
	closed atomic.Bool // Close 后置位：enqueue 直接丢弃（review 观察项 3）
	// lastPrune 上次清理时刻（只由 run 协程读写，无需加锁）。
	lastPrune time.Time
	dropped   atomic.Uint64
	// truncated / oversized 归档事件截断观测（审查 C7-014）：截断条数与整条丢弃条数。
	truncated atomic.Uint64
	oversized atomic.Uint64
	// readErrs 检索期读失败的归档文件数（审查 C7-026）。
	readErrs  atomic.Uint64
	lastErrMu sync.Mutex
	lastErr   string

	// 统计缓存（审查 C7-023）：面板每 2s 轮询一次 Snapshot，而目录扫描是
	// ReadDir + 每个文件 Stat。缓存 5s，命中期间只刷新 dropped 这类计数器。
	statsMu     sync.Mutex
	statsAt     time.Time
	statsFiles  int
	statsBytes  int64
	statsCached bool

	file *os.File
	buf  *bufio.Writer
	path string
	day  string
	size int64
	// idxDay / nextIdx 当前日期的下一个轮转序号（审查 C7-015）。
	// 此前每次轮转都从 i=1 逐个 os.Stat 探测，一天轮转 N 个文件就是 O(N²) 次 stat；
	// Android 共享存储单次 stat 数毫秒，轮转抖动可达秒级，写循环卡住 → 队列积压 → 丢事件。
	// 改成「换日时一次 ReadDir 求出最大序号，之后自增」，每次轮转只 stat 一次。
	idxDay  string
	nextIdx int
}

func newArchiveWriter(cfg Config) *archiveWriter {
	if !cfg.Enabled || strings.TrimSpace(cfg.Dir) == "" {
		return &archiveWriter{cfg: cfg}
	}
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = defaultRetentionDays
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 100 << 20
	}
	if cfg.FileMaxBytes <= 0 {
		cfg.FileMaxBytes = defaultFileMax
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueue
	}
	// purgeCh 必须无缓冲：发送成功即说明写循环已收下请求，它一定会回结果。带缓冲时写循环
	// 退出后（或正选中 stop 分支时）请求照样能发进通道却没人处理，<-out 永远等不到 ——
	// HTTP handler 卡死。
	w := &archiveWriter{
		cfg:     cfg,
		ch:      make(chan Event, cfg.QueueSize),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		purgeCh: make(chan chan PurgeResult),
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		w.setErr(err)
		w.cfg.Enabled = false
		close(w.done)
		return w
	}
	// 启动即清理一次（审查 C7-017 / 任务要求「启动与每次滚动时清理超出部分」）：
	// 此前只有 1s ticker 的首轮触发，进程重启后到那一轮之间旧归档仍在；更糟的是
	// 归档目录被换挂载卷/从备份恢复时，旧文件可能已远超保留期与大小上限。
	// 此处尚未起 run 协程，单 goroutine 访问 file/buf，安全。
	w.prune()
	go w.run()
	return w
}

// configured 配置上是否启用归档（与运行期是否已关闭无关）：
// 读历史与 Snapshot 用它 —— Close 之后仍要能读归档、仍要如实报告 Enabled。
func (w *archiveWriter) configured() bool {
	return w != nil && w.cfg.Enabled && w.cfg.Dir != "" && w.done != nil
}

// enabled 当前还能不能写：配置启用且未关闭。
// closed 标志（review 观察项 3）：Close 之后 done 通道仍非 nil，只靠 done != nil
// 判定会让 enqueue 继续往无人读取的 ch 里塞（最多丢 1024 条，语义脏）。
func (w *archiveWriter) enabled() bool {
	return w.configured() && !w.closed.Load()
}

func (w *archiveWriter) enqueue(e Event) {
	// 「检查 + 入队」必须与 close 互斥（bug #8）：此前 enqueue 先查 enabled() 再写
	// ch，中间有窗口 —— close 已置 closed 且 run 的 drain 读到空 default 返回后，
	// 这个事件才写进没人读的通道：既不落盘、也不进 DroppedWrites（指标显示"没丢"），
	// 归档里少最后几条请求且查不出来。读写锁把两步变成原子。
	w.enqMu.RLock()
	defer w.enqMu.RUnlock()
	// 区分「配置未启用」与「运行期已关闭」（bug #6）：归档本来就没开时**不计数** ——
	// 否则每条请求都算一次"丢弃"，面板显示"归档已关闭"而 dropped_writes 一直涨，
	// 接监控的人会以为归档在丢数据。
	if !w.configured() {
		return
	}
	if w.closed.Load() {
		w.dropped.Add(1) // 停机窗口内的事件：计入丢弃，别再静默
		return
	}
	select {
	case w.ch <- e:
	default:
		w.dropped.Add(1)
	}
}

// pruneInterval prune 的最小间隔（bug #20）。
const pruneInterval = 10 * time.Minute

func (w *archiveWriter) run() {
	defer close(w.done)
	// 写循环整体兜一层 recover（审查 C7-022）：一次 marshal/写盘异常不能让
	// 「清空归档」的 HTTP 请求永久阻塞（它等的是这个协程的回包），也不能让后续
	// 事件静默消失而面板看不出来（LastError 会亮）。
	defer func() {
		if r := recover(); r != nil {
			w.setErr(fmt.Errorf("归档写循环 panic: %v", r))
			log.Printf("WARN: [reqlog] 归档写循环 panic: %v（归档停止，内存指标不受影响）", r)
		}
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case e := <-w.ch:
			w.writeEvent(e)
		case out := <-w.purgeCh:
			w.replyPurge(out)
		case <-ticker.C:
			w.flush()
			// prune 节流到 10 分钟一次（bug #20）：它要 os.ReadDir + 每个文件 Stat，
			// 而保留策略是按天/按总大小，根本不需要秒级检查。Android 共享存储上
			// 每秒一次全目录 stat 是持续的无谓 IO。
			if time.Since(w.lastPrune) >= pruneInterval {
				w.lastPrune = time.Now()
				w.prune()
			}
		case <-w.stop:
			for {
				select {
				case e := <-w.ch:
					w.writeEvent(e)
				default:
					w.flush()
					w.closeFile()
					return
				}
			}
		}
	}
}

// replyPurge 执行清空并**保证**回包（审查 C7-022）。
//
// 此前 purgeNow 的任何 panic（marshal/写盘/删文件）都会让写循环退出、调用方
// `return <-out` 永久阻塞 —— 「清空归档」按钮把 HTTP 连接挂死。这里用一层
// recover 保证无论发生什么都回一个结果。只能由 run 协程调用。
func (w *archiveWriter) replyPurge(out chan PurgeResult) {
	res := PurgeResult{}
	func() {
		defer func() {
			if r := recover(); r != nil {
				w.setErr(fmt.Errorf("清空归档 panic: %v", r))
				log.Printf("WARN: [reqlog] 清空归档 panic: %v", r)
			}
		}()
		res = w.purgeNow()
	}()
	out <- res
}

// clipBytes 按字节上限截断，且不切断 UTF-8 多字节序列（日志里不出现乱码）。
func clipBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut]
}

// encodeEvent 把事件编码成一行，并对超大事件做硬截断（审查 C7-014 P1）。
//
// 返回 nil = 这条事件写不出去。可恢复的编码失败计入 dropped；截断后仍超限
// 计入 oversized（两条都必须让面板看得见，W5-02）。
func (w *archiveWriter) encodeEvent(e *Event) []byte {
	// 归档是第二道口（W5-02）：即使调用方绕过 Record（测试/未来新路径）塞进毒值，
	// json.Marshal 也不能失败返回 nil —— 那会静默丢事件且 dropped 不计数。
	clean := sanitizeEvent(*e)
	e = &clean
	raw, err := json.Marshal(e)
	if err != nil {
		w.setErr(err)
		// W5-02：编码失败返回 nil 的路径必须计入 dropped，否则归档少了一条
		// 而面板显示 dropped_writes 不变，属于「静默丢事件」。
		w.dropped.Add(1)
		return nil
	}
	if len(raw) <= maxEventBytes {
		return raw
	}
	// 唯一的 MB 级字段是 Context（request_archive_context 打开时的整段会话）；
	// 换成标记后事件仍然可用：时间/路径/模型/状态/token 都在，只是正文不落盘。
	if n := len(e.Context); n > 0 {
		e.Context = json.RawMessage(fmt.Sprintf(`{"truncated":true,"bytes":%d}`, n))
		if raw, err = json.Marshal(e); err != nil {
			w.setErr(err)
			w.dropped.Add(1)
			return nil
		}
		if len(raw) <= maxEventBytes {
			w.truncated.Add(1)
			return raw
		}
	}
	// 极端形态：其它字段被塞满（模型名 / UA 里塞几 MB）。丢掉 Context 并截短
	// 客户端可控的长字符串，保证一定落得下去。
	e.Context = nil
	e.Path = clipBytes(e.Path, 256)
	e.Model = clipBytes(e.Model, 128)
	e.ModelEff = clipBytes(e.ModelEff, 128)
	e.Account = clipBytes(e.Account, 128)
	e.ClientIP = clipBytes(e.ClientIP, 64)
	e.UserAgent = clipBytes(e.UserAgent, 256)
	if raw, err = json.Marshal(e); err != nil {
		w.setErr(err)
		w.dropped.Add(1)
		return nil
	}
	if len(raw) > maxEventBytes {
		w.oversized.Add(1)
		w.setErr(fmt.Errorf("归档事件超过 %d 字节且无法截断，已丢弃", maxEventBytes))
		return nil
	}
	w.truncated.Add(1)
	return raw
}

func (w *archiveWriter) writeEvent(e Event) {
	raw := w.encodeEvent(&e)
	if raw == nil {
		return
	}
	// 分桶按**写入时刻**的本地日（审查 C7-020 / C7-016）：
	//   - 按事件时间分桶时，跨零点的迟到/乱序事件会让 day 反复切换，每来一条就
	//     closeFile + ReadDir + OpenFile 一次（写放大）；
	//   - prune 的 cutoff 用 time.Now()（进程本地时区），文件名却来自事件自带时区
	//     （容器里常是 UTC），边界日整天的归档会被多删或少删一天。
	// 两者统一到「写入时刻 + 本地时区」后，文件名、排序、cutoff 是同一把尺子。
	day := time.Now().Format("2006-01-02")
	if w.file == nil || w.day != day {
		if err := w.openFile(day, false); err != nil {
			w.setErr(err)
			return
		}
	}
	// 轮转判据只看「当前文件是否已经到达上限」（审查 C7-014）：此前用
	// size+len(raw) > FileMaxBytes，一条超大事件会让**每条**事件都轮转、每条一个
	// 新文件，文件数无界。现在单条事件先被截到 maxEventBytes，且只在文件已有内容
	// 且到上限时才换文件 —— 单文件最多超出一条事件的长度，文件数重新被
	// MaxBytes / FileMaxBytes 夹住。
	if w.size > 0 && w.size >= w.cfg.FileMaxBytes {
		if err := w.openFile(day, true); err != nil {
			w.setErr(err)
			return
		}
		// 每次滚动顺手清理超出保留期/大小上限的文件（审查 C7-017 + 任务要求）：
		// 滚动是天然的低频触发点（每 FileMaxBytes 一次），长期不重启的部署也不必
		// 等 ticker 的 10 分钟节流。
		w.lastPrune = time.Now()
		w.prune()
	}
	if _, err := w.buf.Write(raw); err != nil {
		w.setErr(err)
		return
	}
	if err := w.buf.WriteByte('\n'); err != nil {
		w.setErr(err)
		return
	}
	w.size += int64(len(raw)) + 1
}

func (w *archiveWriter) openFile(day string, rotate bool) error {
	w.closeFile()
	if err := os.MkdirAll(w.cfg.Dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(w.cfg.Dir, "requests-"+day+".jsonl")
	if rotate {
		path = w.nextPath(day)
	} else if latest := latestArchiveForDay(w.cfg.Dir, day); latest != "" {
		path = filepath.Join(w.cfg.Dir, latest)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	w.file = f
	w.buf = bufio.NewWriterSize(f, 64<<10)
	w.path = path
	w.day = day
	w.size = info.Size()
	// 文件数变了：统计缓存立刻作废（字节数的 5 秒滞后无所谓，文件数会让
	// 面板的「N 个文件」看起来卡住）。
	w.invalidateStats()
	return nil
}

// nextPath 返回该日期的下一个轮转文件路径（审查 C7-015）。
//
// 序号由内存自增维护：换日或首次轮转时用一次 ReadDir 求出该日已有的最大序号，
// 之后每次只 stat 一个候选路径。此前是「从 i=1 逐个 os.Stat」，一天轮转 N 个文件
// 就是 O(N²) 次 stat，Android 共享存储上能把写循环卡到秒级。
func (w *archiveWriter) nextPath(day string) string {
	if w.idxDay != day {
		w.idxDay = day
		w.nextIdx = latestIndexForDay(w.cfg.Dir, day) + 1
	}
	base := filepath.Join(w.cfg.Dir, "requests-"+day)
	for {
		path := fmt.Sprintf("%s.%d.jsonl", base, w.nextIdx)
		w.nextIdx++
		if !fileExists(path) {
			return path
		}
	}
}

// latestIndexForDay 该日期已存在的最大轮转序号（只有基准文件或没有文件时 = 0）。
func latestIndexForDay(dir, day string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	best := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "requests-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		if d, idx := archiveNameKey(name); d == day && idx > best {
			best = idx
		}
	}
	return best
}

func latestArchiveForDay(dir, day string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	best := ""
	bestIndex := -1
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "requests-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		if d, idx := archiveNameKey(name); d == day && idx > bestIndex {
			bestIndex, best = idx, name
		}
	}
	return best
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// PurgeResult 清空归档的结果：删掉的文件数与字节数。
type PurgeResult struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
}

// Purge 清空归档目录里的请求事件文件（面板「清空归档」）。返回删除量。
// 只删 requests-*.jsonl（与定期 prune 同口径），目录里其它文件不碰。
// 归档关闭时返回零值：没有可清的东西，不算失败。
func (r *Recorder) Purge() (PurgeResult, error) {
	if r == nil || r.archive == nil {
		return PurgeResult{}, nil
	}
	return r.archive.purge()
}

// purge 把清空请求交给写循环并等它做完。归档已停止（done 关闭）时直接返回错误，
// 否则会永久阻塞调用方（HTTP handler 卡死）。
//
// purgeCh 无缓冲（见 newArchiveWriter）：发送成功就说明写循环已经收下请求，它一定会回结果。
func (w *archiveWriter) purge() (PurgeResult, error) {
	if !w.enabled() {
		return PurgeResult{}, nil
	}
	out := make(chan PurgeResult, 1)
	select {
	case w.purgeCh <- out:
	case <-w.done:
		return PurgeResult{}, errors.New("请求归档已停止，无法清理")
	}
	// 回包与「写循环已退出」二选一（审查 C7-022）：run 的 recover 兜住 panic 后
	// done 会关闭，这里必须也能退出，否则 HTTP handler 永久挂住。
	select {
	case res := <-out:
		return res, nil
	case <-w.done:
		return PurgeResult{}, errors.New("请求归档已停止，无法清理")
	}
}

// purgeNow 在写循环内清空归档：先落盘关文件（否则删掉的 inode 还在被 buf 写），
// 再按 prune 同口径删 requests-*.jsonl。closeFile 已把 day 清空，下一条事件会重开新文件。
// 只能由 run() 调用 —— file/buf 归写循环所有，其他 goroutine 碰会与写入竞争。
func (w *archiveWriter) purgeNow() PurgeResult {
	// 先把队列里待写的事件写完再删：Record 是异步入队的，直接删会让「刚记录的 N 条」
	// 在删除之后才落盘 —— 文件立刻又出现，清不干净。
	w.drain()
	w.flush()
	w.closeFile()
	entries, err := os.ReadDir(w.cfg.Dir)
	if err != nil {
		return PurgeResult{}
	}
	var out PurgeResult
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "requests-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(w.cfg.Dir, entry.Name())
		info, statErr := entry.Info()
		if err := os.Remove(path); err == nil {
			out.Files++
			if statErr == nil {
				out.Bytes += info.Size()
			}
		}
	}
	// 目录被清空：轮转序号必须重新算，否则下一条事件会写到 requests-<day>.N.jsonl
	// 而不是基准文件（读侧仍能读，但序号无意义地一路涨）。
	w.idxDay = ""
	w.nextIdx = 0
	w.invalidateStats()
	return out
}

// drain 把写入队列里已入队的事件全部写掉（非阻塞，队列空即返回）。
func (w *archiveWriter) drain() {
	for {
		select {
		case e := <-w.ch:
			w.writeEvent(e)
		default:
			return
		}
	}
}

func (w *archiveWriter) flush() {
	if w.buf == nil {
		return
	}
	if err := w.buf.Flush(); err != nil {
		w.setErr(err)
	}
}

func (w *archiveWriter) closeFile() {
	if w.buf != nil {
		_ = w.buf.Flush()
	}
	if w.file != nil {
		_ = w.file.Close()
	}
	w.file = nil
	w.buf = nil
	w.path = ""
	w.day = ""
	w.size = 0
}

func (w *archiveWriter) prune() {
	if !w.enabled() {
		return
	}
	// 保留天数兜底（审查 C7-017）：<=0 会让 cutoff 落在今天，把当天归档一起删光。
	days := w.cfg.RetentionDays
	if days <= 0 {
		days = defaultRetentionDays
	}
	entries, err := os.ReadDir(w.cfg.Dir)
	if err != nil {
		w.setErr(err)
		return
	}
	type item struct {
		path  string
		size  int64
		mtime time.Time
		day   string
		idx   int
		dated bool // 文件名里的日期可解析；否则退回 mtime
	}
	items := make([]item, 0, len(entries))
	var total int64
	// cutoff 与写盘分桶必须同一把尺子（审查 C7-016）：writeEvent 现在按
	// 「写入时刻 + 进程本地时区」生成文件名，这里也用 time.Now() 的本地日，
	// 于是 UTC 容器 / 跨时区主机上不会再出现「边界日整天的归档被多删或少删」。
	cutoff := time.Now().AddDate(0, 0, -days)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "requests-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(w.cfg.Dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}
		day, idx := archiveNameKey(entry.Name())
		it := item{path: path, size: info.Size(), mtime: info.ModTime(), day: day, idx: idx}
		if _, derr := time.ParseInLocation("2006-01-02", day, time.Local); derr == nil {
			it.dated = true
		}
		items = append(items, it)
		total += info.Size()
	}
	// 排序与过期判断都必须用文件名日期（bug #8）：目录被整体拷贝、从备份恢复、
	// 容器换挂载卷之后所有文件 mtime 都变成"现在"，按 mtime 判断会让 RetentionDays
	// 永不生效（只能靠 MaxBytes 兜底，而且 MaxBytes 淘汰同样按 mtime 优先删错文件）。
	// 归档文件名 requests-<day>[.<idx>].jsonl 里的日期就是时间序（read 侧早已按它排序）。
	sort.Slice(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.dated && b.dated {
			if a.day != b.day {
				return a.day < b.day
			}
			if a.idx != b.idx {
				return a.idx < b.idx
			}
		} else if a.dated != b.dated {
			return a.dated // 能解析日期的排在前面（更可能有"真实"年龄）
		}
		if a.mtime.Equal(b.mtime) {
			return a.path < b.path
		}
		return a.mtime.Before(b.mtime)
	})
	cutoffDay := cutoff.Format("2006-01-02")
	removed, removedBytes := 0, int64(0)
	for _, it := range items {
		if it.path == w.path {
			continue
		}
		expired := it.mtime.Before(cutoff)
		if it.dated {
			expired = it.day < cutoffDay
		}
		if expired || total > w.cfg.MaxBytes {
			if err := os.Remove(it.path); err == nil {
				total -= it.size
				removed++
				removedBytes += it.size
			}
		}
	}
	if removed > 0 {
		log.Printf("[reqlog] 归档清理：删除 %d 个文件（%d 字节），保留 %d 天 / 上限 %d MiB",
			removed, removedBytes, days, w.cfg.MaxBytes>>20)
		w.invalidateStats()
	}
}

// reversedEvents 返回倒序副本（内部按旧→新存，对外要新→旧）。
func reversedEvents(in []Event) []Event {
	out := make([]Event, len(in))
	for i, e := range in {
		out[len(in)-1-i] = e
	}
	return out
}

func (w *archiveWriter) read(limit int, filter Filter) ([]Event, error) {
	if !w.configured() {
		return nil, nil
	}
	if limit <= 0 {
		limit = 200
	}
	if limit > defaultReadMax {
		limit = defaultReadMax
	}
	entries, err := os.ReadDir(w.cfg.Dir)
	if err != nil {
		return nil, err
	}
	// 文件读取顺序无关紧要：结果一律按事件时间排序。不能依赖归档文件的 mtime 还原时间
	// 顺序——同一秒内连续轮转写出的多个文件 mtime 经常完全相同（Linux 文件时间戳粒度粗），
	// os.ReadDir 的字典序又会把装着最早事件的基准文件 requests-<day>.jsonl 排在
	// requests-<day>.N.jsonl 之后；目录被整体拷贝 / 恢复备份后 mtime 更不可信。
	// 先按文件名**倒序**（issue ISSUE-12）：文件名里的日期/序号就是时间序，
	// 从最新往回读，凑够 limit 条即停。此前是「全目录逐文件逐行读 + 全量排序」，
	// 100MiB 归档下每次面板刷新都要扫上百万行（手机端 CPU 明显）。
	// 文件名序只用于「先读哪一批」；最终结果仍按事件时间排序（下面 sort），
	// 所以同秒轮转、目录被拷贝后 mtime 失真这些情况都不影响正确性。
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "requests-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		names = append(names, name)
	}
	// 按 (日期, 序号) **数值**倒序：字典序会把 requests-<day>.jsonl（序号 0）
	// 排在 requests-<day>.10.jsonl 之后（'j' > '1'），直接倒排就变成"先读最旧的"。
	sort.Slice(names, func(i, j int) bool {
		di, ii := archiveNameKey(names[i])
		dj, ij := archiveNameKey(names[j])
		if di != dj {
			return di > dj
		}
		return ii > ij
	})
	var out []Event
	// 扫描预算（审查 C7-018）：窄 filter（按 IP/UA 搜）命中稀疏时，此前会从最新
	// 文件一路回溯到读完整目录（默认 100MiB，每块都做 JSON 解析）—— 手机端一次
	// 搜索就能把 CPU/IO 打满。超预算即返回已读到的部分（面板本来就只展示 limit 条）。
	scannedFiles := 0
	scannedBytes := int64(0)
	readErrs := 0
	for _, name := range names {
		if scannedFiles >= readScanFileBudget || scannedBytes >= readScanByteBudget {
			break
		}
		// 单文件读失败只跳过（bug #7）：并发 prune 刚把它删掉、目录被手工清理、
		// 单个文件权限丢失，这些都不该让整个「运行日志」页 500 ——
		// 少几条记录远好过整页打不开（目录级读失败在更上面已经上抛了）。
		// 但要计数（审查 C7-026）：否则「面板少数据」永远查不出原因。
		rows, nbytes, rerr := readFileTailScan(filepath.Join(w.cfg.Dir, name), filter, limit-len(out))
		scannedFiles++
		scannedBytes += nbytes
		if rerr != nil {
			readErrs++
			continue
		}
		out = append(out, rows...)
		// 凑够 limit 就没必要再往前读（文件名日期序 = 时间序）。
		if len(out) >= limit {
			break
		}
	}
	if readErrs > 0 {
		w.readErrs.Add(uint64(readErrs))
	}
	// 契约：按事件时间倒序返回最近 limit 条（同一时刻用 Stable 保留落盘先后）。
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// archiveNameKey 把 requests-<day>[.<idx>].jsonl 拆成 (day, idx)。
// idx 缺失 = 0（该日期的第一个文件，装着最早的事件）。
func archiveNameKey(name string) (day string, idx int) {
	rest := strings.TrimSuffix(strings.TrimPrefix(name, "requests-"), ".jsonl")
	if i := strings.LastIndex(rest, "."); i >= 0 {
		if n, err := strconv.Atoi(rest[i+1:]); err == nil {
			return rest[:i], n
		}
	}
	return rest, 0
}

// readFileTail 从文件尾部倒着读，凑够 limit 条匹配事件就停（bug #11）。
//
// 归档是追加写的（旧→新），所以"最新 N 条"就在文件末尾。此前 readFile 会把
// 整个文件（单文件上限 16MB ≈ 10 万行）逐行 Unmarshal 一遍，面板每刷一次
// 就是一次十万行 JSON 解析；注释写的"凑够 limit 即停"只在文件之间生效。
// 返回顺序是旧→新，调用方仍会按事件时间统一排序。
//
// readFileTail 兼容入口（既有回归用例 readback 用它当「尾部读」的 oracle）。
func readFileTail(path string, filter Filter, limit int) ([]Event, error) {
	rows, _, err := readFileTailScan(path, filter, limit)
	return rows, err
}

// readFileTailScan 同 readFileTail，额外返回本文件被读过的字节数（审查 C7-018 的扫描预算用）。
func readFileTailScan(path string, filter Filter, limit int) ([]Event, int64, error) {
	if limit <= 0 {
		return nil, 0, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	const chunk = 64 << 10
	// pending 更靠后那块留下的不完整首行（审查 C7-019）。
	// 此前是 `data := append(buf, carry...)`：每块都把整条 carry 重新复制一遍，
	// 单行 8MB 时 128 块 × 平均 4MB ≈ 512MB 临时分配。现在只把 pending 拼到
	// 本块**最后一行**上，复制量按行算而不是按块算。
	var pending []byte
	var newest []Event // 收集到的匹配事件（新→旧）
	scanned := int64(0)
	offset := st.Size()
	for offset > 0 && len(newest) < limit {
		n := int64(chunk)
		if offset < n {
			n = offset
		}
		offset -= n
		buf := make([]byte, n)
		if _, rerr := f.ReadAt(buf, offset); rerr != nil && !errors.Is(rerr, io.EOF) {
			return nil, scanned, rerr
		}
		scanned += n
		lines := bytes.Split(buf, []byte("\n"))
		if len(pending) > 0 && len(lines) > 0 {
			lines[len(lines)-1] = append(lines[len(lines)-1], pending...)
			pending = nil
		}
		if offset > 0 {
			// 本块首行可能被块边界切断：留给更靠前的那块拼接
			pending = append([]byte(nil), lines[0]...)
			lines = lines[1:]
		} else {
			pending = nil
		}
		for i := len(lines) - 1; i >= 0 && len(newest) < limit; i-- {
			if len(lines[i]) == 0 {
				continue
			}
			// 超过单条事件上限的行不可能是我们写出的合法事件（写侧已截断到
			// maxEventBytes，审查 C7-014）：直接跳过，省掉一次超大 JSON 解析。
			if len(lines[i]) > maxEventBytes {
				continue
			}
			var e Event
			if json.Unmarshal(lines[i], &e) != nil || !filter.match(e) {
				continue
			}
			newest = append(newest, e)
		}
	}
	out := make([]Event, len(newest))
	for i, e := range newest {
		out[len(newest)-1-i] = e
	}
	return out, scanned, nil
}

// readFile 全量逐行读（只给回归用例当「全量读」oracle 用，生产路径走 readFileTailScan）。
//
// 审查 C7-024 把它记为死代码，实测不成立：regression_20261021_test.go:28 用它作为
// 「尾部读结果必须与全量读一致」的对照。保留，并在注释里标明用途，避免下一个人再删一次。
func readFile(path string, filter Filter) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	var out []Event
	for scanner.Scan() {
		var e Event
		if json.Unmarshal(scanner.Bytes(), &e) != nil || !filter.match(e) {
			continue
		}
		out = append(out, e)
	}
	return out, scanner.Err()
}

func (f Filter) match(e Event) bool {
	if f.Outcome != "" && e.Outcome != f.Outcome {
		return false
	}
	if !containsFold(e.Account, f.Account) {
		return false
	}
	// 同时匹配请求模型与降级后实际模型：请求 A 实际用 B 成功时，按任一搜索都该命中。
	if !containsFold(e.Model, f.Model) && !containsFold(e.ModelEff, f.Model) {
		return false
	}
	if !containsFold(e.ClientIP, f.ClientIP) {
		return false
	}
	if !containsFold(e.UserAgent, f.UserAgent) {
		return false
	}
	if !f.From.IsZero() && e.Time.Before(f.From) {
		return false
	}
	if !f.To.IsZero() && e.Time.After(f.To) {
		return false
	}
	return true
}

// containsFold 大小写不敏感的子串匹配；needle 为空视为命中（不筛该字段）。
func containsFold(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

func (w *archiveWriter) stats() ArchiveStats {
	// Enabled 报的是「配置上是否启用」，不是「当前还能不能写」：Close 之后
	// Snapshot 仍应如实反映归档已启用（否则运维看到 Enabled 变 false 会以为配置错了）。
	s := ArchiveStats{
		Enabled:         w.configured(),
		Dir:             w.cfg.Dir,
		LastError:       w.errString(),
		TruncatedWrites: w.truncated.Load(),
		OversizedWrites: w.oversized.Load(),
		ReadErrors:      w.readErrs.Load(),
	}
	if !s.Enabled {
		// 未启用：不报 dropped（那是"没开"造成的，不是"丢了"，bug #6）
		return s
	}
	s.DroppedWrites = w.dropped.Load()
	files, bytes, ok := w.cachedStats()
	if !ok {
		files, bytes = w.scanStats()
		if files < 0 {
			s.LastError = w.errString()
			return s
		}
		w.storeStats(files, bytes)
	}
	s.Files, s.Bytes = files, bytes
	return s
}

// scanStats 真正扫一遍归档目录；返回 files<0 表示目录读失败。
func (w *archiveWriter) scanStats() (int, int64) {
	entries, err := os.ReadDir(w.cfg.Dir)
	if err != nil {
		w.setErr(err)
		return -1, 0
	}
	files := 0
	var bytes int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "requests-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		if info, err := entry.Info(); err == nil {
			files++
			bytes += info.Size()
		}
	}
	return files, bytes
}

// cachedStats 5 秒内的目录统计（审查 C7-023）：面板每 2s 轮询一次 Snapshot，
// 每次 ReadDir + 全量 Stat 在文件上千时是持续的无谓 IO，还与 prune 争同一目录。
func (w *archiveWriter) cachedStats() (int, int64, bool) {
	w.statsMu.Lock()
	defer w.statsMu.Unlock()
	if !w.statsCached || time.Since(w.statsAt) > statsCacheTTL {
		return 0, 0, false
	}
	return w.statsFiles, w.statsBytes, true
}

func (w *archiveWriter) storeStats(files int, bytes int64) {
	w.statsMu.Lock()
	w.statsFiles, w.statsBytes, w.statsAt, w.statsCached = files, bytes, time.Now(), true
	w.statsMu.Unlock()
}

func (w *archiveWriter) invalidateStats() {
	w.statsMu.Lock()
	w.statsCached = false
	w.statsMu.Unlock()
}

func (w *archiveWriter) close() {
	if !w.enabled() {
		return
	}
	w.closeOnce.Do(func() {
		// 写锁：等在途 enqueue 落完再置位 + 停 run（bug #8）。
		w.enqMu.Lock()
		w.closed.Store(true)
		close(w.stop)
		w.enqMu.Unlock()
		// 有上限地等待落盘（审查 C7-021）：写循环卡在磁盘 IO（存储满 / 慢卡）时
		// 无限等 `<-w.done` 会把进程退出一起拖住，最终被 run.sh 的停机预算强杀，
		// 反而丢得更多。超时只记一行 WARN。
		select {
		case <-w.done:
		case <-time.After(closeFlushTimeout):
			log.Printf("WARN: [reqlog] 归档写循环 %s 内未退出，放弃等待（最后一批事件可能未落盘）", closeFlushTimeout)
		}
	})
}

func (w *archiveWriter) setErr(err error) {
	if err == nil {
		return
	}
	w.lastErrMu.Lock()
	w.lastErr = err.Error()
	w.lastErrMu.Unlock()
}

func (w *archiveWriter) errString() string {
	w.lastErrMu.Lock()
	defer w.lastErrMu.Unlock()
	return w.lastErr
}
