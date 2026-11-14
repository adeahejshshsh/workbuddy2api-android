// ring.go 固定容量的结构化日志环形缓冲（并发安全，实现 io.Writer）。
// main 把 log 包输出与 chat 表格日志经 MultiWriter 镜像进来，面板
// /panel/api/logs 读取快照；超出容量的旧行按 FIFO 淘汰。
//
// 每行入环时按前缀规则归类频道（chat=对话请求表格行 / task=任务动作 /
// sys=系统与其它），面板日志视图按频道筛选——对话流量大时任务结果不被冲掉。
package panel

import (
	"regexp"
	"strings"
	"sync"
	"time"
)

// 日志频道。
const (
	ChChat = "chat"
	ChTask = "task"
	ChSys  = "sys"
)

// LogEntry 单条日志（时间戳取写入时刻；log 包行的行首日期时间已被剥离）。
type LogEntry struct {
	TS   time.Time `json:"ts"`
	Ch   string    `json:"ch"`
	Text string    `json:"text"`
}

// taskPrefixes 任务动作日志的行首标识（scheduler 与 panel 的既有口径）。
var taskPrefixes = []string{
	"school ", "streak-bonus ", "travel ", "blackcat ", "lottery ",
	"checkin ", "activity ", "keepalive ", "balance ", "user-resource ",
	"panel: 任务", "panel: 一键", "panel: checkin", "panel: 手动",
	"panel: 队列", "panel: 券码",
	// BUG-25：以下前缀此前漏了，任务执行/领奖/跳过名单的日志全被归到 sys 频道，
	// 面板按「任务」筛选时看不到。
	"autotask ", "panel: 领取任务奖励", "panel: 全部接受", "panel: 跳过名单更新",
}

// tsPrefixRe log 包默认 flags（日期 时间）产生的行首时间戳。
var tsPrefixRe = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} `)

// classifyLine 按行首特征归类频道。
func classifyLine(line string) string {
	if strings.HasPrefix(line, "| #") { // chat 表格日志（server/logging.go logChatRow）
		return ChChat
	}
	for _, p := range taskPrefixes {
		if strings.HasPrefix(line, p) {
			return ChTask
		}
	}
	return ChSys
}

// Ring 日志环形缓冲。
// storeQueueMax 待落盘队列上限：磁盘卡住时内存不无界增长，超出的行只留在环里。
const storeQueueMax = 10000

// Ring 日志环形缓冲。
//
// 落盘**不在 Write 里做**：Ring 是 log 的输出，Write 运行时调用方还持着 logger 的锁，
// 落盘路径上任何一次慢盘都会让整个进程的日志随之卡死。所以 Write 只入环、排队并发信号，
// 由后台 goroutine 在锁外写；写得慢时多次变更合成一次。进程退出前调 Flush 把尾巴写完。
type Ring struct {
	mu      sync.Mutex
	entries []LogEntry
	head    int // 下一次写入位置
	size    int // 已写入条数（<= cap）
	cap     int
	// store 全频道按天落盘（见 logstore.go）。nil = 未启用（纯内存）。
	store        *LogStore
	storeQ       []LogEntry // 待落盘的行，FIFO
	storeDropped int        // 磁盘跟不上、队列满时丢掉的行数（面板展示）

	wake    chan struct{} // 容量 1：有待落盘的内容
	flushMu sync.Mutex    // 串行化落盘：队列按 FIFO 写，文件里的顺序就是入环顺序
	loop    sync.Once
}

// SetLogStore 挂上按天落盘存储，并用它回填最近日志。
//
// 回填来源是 store：内存环每频道只有 500 条、重启归零，而「昨天下午那波 429 从
// 几点开始」正是要靠文件回答的问题。回填只灌内存环（有上限），不重建全部历史。
func (r *Ring) SetLogStore(store *LogStore) error {
	r.mu.Lock()
	r.store = store
	r.loop.Do(func() { go r.persistLoop() })
	r.mu.Unlock()
	if store == nil {
		return nil
	}
	back := store.Backfill(0)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cap <= 0 {
		r.cap = 500
	}
	if r.entries == nil {
		r.entries = make([]LogEntry, r.cap)
	}
	for _, entry := range back {
		// 回填按时间升序逐条灌入：环是 FIFO，顺序错了「最近 N 条」就不是最近的。
		r.entries[r.head] = entry
		r.head = (r.head + 1) % r.cap
		if r.size < r.cap {
			r.size++
		}
	}
	return nil
}

// NewRing 构建容量为 capacity 的日志环（非正值回退 500）。
func NewRing(capacity int) *Ring {
	if capacity <= 0 {
		capacity = 500
	}
	return &Ring{cap: capacity, wake: make(chan struct{}, 1)}
}

// Write 按 \n 切分入环（实现 io.Writer）。空行丢弃；超容量淘汰最旧行。
func (r *Ring) Write(p []byte) (int, error) {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	// 零值设防（BUG-14）：&Ring{}（cap==0）时下面会 r.entries[0] 越界、
	// (head+1)%0 除零 panic。NewRing 有兜底，但类型本身不该崩。
	if r.cap <= 0 {
		r.cap = 500
	}
	for _, line := range strings.Split(strings.TrimRight(string(p), "\r\n"), "\n") {
		if line == "" {
			continue
		}
		text := tsPrefixRe.ReplaceAllString(line, "")
		// 真环写（bug 13）：此前是 `append` 再 `entries[overflow:]` —— 每写一行
		// 容量少 1，容量耗尽时整块重分配并拷贝 cap 条，等于「每 cap 行日志一次
		// 500 元素的拷贝 + 一次分配」，白白给 GC 加压力（聊天流水在高 QPS 下
		// 是每请求一行）。这里固定数组 + 头指针，写入 O(1) 且零分配。
		if r.entries == nil {
			r.entries = make([]LogEntry, r.cap)
		}
		entry := LogEntry{TS: now, Ch: classifyLine(text), Text: text}
		r.entries[r.head] = entry
		// 只入队，不在锁内落盘（见 Ring 注释）。队列按入环顺序 FIFO，文件里的顺序
		// 就是入环顺序；写慢了多次变更合成一次 Flush。
		if r.store != nil {
			if len(r.storeQ) < storeQueueMax {
				r.storeQ = append(r.storeQ, entry)
			} else {
				r.storeDropped++
			}
		}
		r.head = (r.head + 1) % r.cap
		if r.size < r.cap {
			r.size++
		}
	}
	if r.store != nil && len(r.storeQ) > 0 {
		select {
		case r.wake <- struct{}{}:
		default: // 已有待处理的信号：后台 goroutine 会把所有排队行一起写掉
		}
	}
	return len(p), nil
}

// Flush 同步把排队的行写盘（后台 goroutine 与停机、导出前调用）。
// 不能在持有 logger 锁时调用：写盘本身不能再回流进日志。
func (r *Ring) Flush() {
	r.flushMu.Lock()
	defer r.flushMu.Unlock()
	r.mu.Lock()
	q, store := r.storeQ, r.store
	r.storeQ = nil
	r.mu.Unlock()
	if store == nil {
		return
	}
	for _, e := range q {
		store.Append(e)
	}
}

// persistLoop 后台落盘：信号容量为 1，多行变更自然合成一次写。
func (r *Ring) persistLoop() {
	for range r.wake {
		r.Flush()
	}
}

// Snapshot 按写入顺序返回缓冲内全部条目（拷贝，调用方可安全持有）。
func (r *Ring) Snapshot() []LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cap <= 0 {
		return nil // 零值 Ring（BUG-14）：没有容量就没有内容
	}
	out := make([]LogEntry, 0, r.size)
	if r.size < r.cap {
		// 未写满：有效数据在 [0, size)
		out = append(out, r.entries[:r.size]...)
		return out
	}
	// 已写满：从最旧的槽位（head）绕一圈
	out = append(out, r.entries[r.head:]...)
	out = append(out, r.entries[:r.head]...)
	return out
}
