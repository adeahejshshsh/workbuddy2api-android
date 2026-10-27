// Package redisstore 封装 Upstash（Redis）持久化，并提供内存降级（Noop）。
//
// 设计约束：Upstash 走公网 TLS，单次 RTT 可能 50~300ms，因此所有写操作都是
// fire-and-forget（后台 goroutine + 失败仅 debug 日志），读操作只发生在启动时
// （加载粘性会话镜像、恢复冷却/熔断快照）。内存为主、Redis 为辅。
//
// 未配置 url / 连接失败时降级为 Noop：一切功能照常工作（纯内存模式），
// 上层只打一条启动警告日志。
package redisstore

import (
	"context"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// keyTTL 粘性会话镜像 + 状态快照的默认 TTL（redis 侧兜底，防脏数据长期滞留）。
const keyTTL = 7 * 24 * time.Hour

// writeConcurrencyLimit fire-and-forget 异步写的在途上限（发现 4：写 goroutine
// 无信号量限制，高写入速率下可瞬时堆积）。超过的排队不丢弃——写语义不变（见 goWrite）。
const writeConcurrencyLimit = 8

// Store 只放本期需要的方法。上下文由实现内部构造（读操作配短超时，写操作 fire-and-forget）。
type Store interface {
	// SetBind 异步镜像粘性会话绑定（key→uid），带 TTL。
	SetBind(key, uid string, ttl time.Duration)
	// DelBind 异步删除粘性会话绑定。
	DelBind(key string)
	// LoadBinds 全量读取粘性会话绑定（key→uid，key 已剥前缀）；仅在启动时调用（同步）。
	// 供冷启动恢复粘性映射（防重启丢粘性）。
	LoadBinds() map[string]string
	// SaveState 异步写池状态 JSON 快照（与本地 state.json 并存，仅作恢复备份）。
	SaveState(data []byte)
	// LoadState 读池状态快照；仅在启动时调用（同步）。
	LoadState() ([]byte, bool)
	// Close 关停 Store：Upstash 等待已提交的异步写全部执行完再关底层连接
	// （停机语义：最后一笔 Redis 镜像必须写完），之后新提交的写直接丢弃；幂等。
	// Noop 为空操作。进程退出前在 pool.Close() 之后调用。
	Close() error
}

const (
	bindPrefix  = "wb2api:bind:"
	stateKey    = "wb2api:state"
	readTimeout = 3 * time.Second
)

// New 根据 url+token 构建 Store。
//   - url 为空 → Noop（纯内存模式）
//   - url 已是完整 rediss:// URL 则直接 ParseURL；否则用 token 组装 rediss://default:token@host:6379
//   - Ping 失败 → Noop + 启动警告（硬性降级要求：不因 Redis 不可用而失败）
func New(url, token string) Store {
	if url == "" {
		log.Printf("[redisstore] upstash 未配置，进入纯内存模式（Noop 降级）")
		return Noop{}
	}

	full := normalizeURL(url, token)
	opt, err := redis.ParseURL(full)
	if err != nil {
		log.Printf("[redisstore] 警告: redis 连接串解析失败 (%v)，降级 Noop", err)
		return Noop{}
	}
	opt.ReadTimeout = readTimeout
	opt.WriteTimeout = readTimeout
	client := redis.NewClient(opt)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		log.Printf("[redisstore] 警告: upstash 连接失败 (%v)，降级 Noop（纯内存模式）", err)
		_ = client.Close()
		return Noop{}
	}
	log.Printf("[redisstore] upstash 已连接 (addr=%s)", opt.Addr)
	return &Upstash{client: client} // 队列/done/worker 由 init() 懒初始化
}

// normalizeURL 把 url+token 归一化为可直接 ParseURL 的完整 rediss:// URL。
// 若 url 本身已含 scheme（rediss://、redis://、https://...upstash.io 等）：
//   - rediss:// 或 redis:// 原样返回（已是完整连接串）
//   - 其余（如 https://xxx.upstash.io）剥掉 "://" 前缀只取 host，再按
//     "rediss://default:<token>@<host>:6379" 组装
func normalizeURL(url, token string) string {
	if len(url) >= 8 && (url[:8] == "rediss:/" || url[:7] == "redis:/") {
		return url
	}
	host := url
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	return "rediss://default:" + token + "@" + host + ":6379"
}

// Upstash 真实现：redis.Client 封装。
//
// 写调度：固定 writeWorkers 个 worker 从有界队列 q 取活执行。
// 入队成功即保证执行（Close 会排空），队列满则丢弃并计数（dropped），
// Close 后提交的写直接丢弃 —— 既不丢「已接受」的写，也不让 goroutine 无界堆积。
type Upstash struct {
	client *redis.Client
	// done 关停标志（Close 关闭）。懒初始化，测试可直接构造零值（client=nil）。
	done chan struct{}
	// q 待执行写队列（有界）。满了就丢弃并计数 —— 镜像本来就是尽力而为，
	// 「丢弃」远好过「按 QPS 堆积 goroutine」：此前 goWrite 每次起一个 goroutine，
	// sem 只限「同时在跑」、不限「在排队」，Redis 慢一拍就是
	// 请求数 × 等待时间 个 goroutine（review H5）。
	q chan func()
	// initOnce 懒初始化（零值 Upstash 也能用，测试直接构造）。
	initOnce sync.Once
	// closeOnce 保证 Close 幂等（多次调用只关一次 done）。
	closeOnce sync.Once
	// submitMu 收窄 goWrite 的提交/关停竞态：提交点与 close(done) 互斥后，
	// 「Close 前提交的写必然执行」不依赖 goroutine 调度时序。
	submitMu sync.Mutex
	// wg 已入队未完成的写（Close 排空用）。
	wg sync.WaitGroup
	// workerWG 写 worker 自身（Close 关队列后等它们退出）。
	workerWG sync.WaitGroup
	// qClosed 队列已关（防重复 close panic）。
	qClosed bool
	// dropped 队列满被丢弃的写数（观测用）。
	dropped atomic.Int64
}

// 写队列：worker 数 = 并发在途上限，队列深度 = 允许排队的写数。
const (
	writeQueueLimit = 1024
	writeWorkers    = writeConcurrencyLimit
)

// init 懒初始化：建队列与 done，起固定数量的写 worker。
func (u *Upstash) init() {
	u.initOnce.Do(func() {
		if u.done == nil {
			u.done = make(chan struct{})
		}
		if u.q == nil {
			u.q = make(chan func(), writeQueueLimit)
		}
		for i := 0; i < writeWorkers; i++ {
			u.workerWG.Add(1)
			go func() {
				defer u.workerWG.Done()
				for fn := range u.q {
					fn()
					u.wg.Done()
				}
			}()
		}
	})
}

// goWrite 以 fire-and-forget 方式提交 fn：入队成功即保证执行（Close 会排空），
// 队列满则丢弃并计数，Close 之后提交的直接丢弃。
func (u *Upstash) goWrite(fn func()) {
	u.init()
	u.submitMu.Lock()
	select {
	case <-u.done:
		u.submitMu.Unlock()
		return // 关停后提交的写：直接丢弃（进程已在退出）
	default:
	}
	u.wg.Add(1)
	select {
	case u.q <- fn:
		u.submitMu.Unlock()
	default:
		// 队列满：丢弃（尽力而为），绝不再起 goroutine 排队。
		u.wg.Done()
		u.dropped.Add(1)
		u.submitMu.Unlock()
	}
}

// Dropped 返回因队列满被丢弃的异步写数量（观测/测试用）。
func (u *Upstash) Dropped() int64 {
	if u == nil {
		return 0
	}
	return u.dropped.Load()
}

// Close 的两段等待上限（变量而非常量：测试要把它们压到毫秒级来验证「有界返回」）。
var (
	closeDrainTimeout  = 10 * time.Second // 等在途/排队的写跑完
	closeWorkerTimeout = 5 * time.Second  // 等 worker 退出
)

// Close 等待已提交的异步写全部执行完毕，再关底层 redis 连接；幂等。
// 之后新提交的写直接丢弃（goWrite 的 done 检查）。停机路径在 pool.Close() 后调用：
// pool 的最后一次 Flush→SaveState 已提交，本方法保证它写完才返回。
func (u *Upstash) Close() error {
	u.init()
	// 先关 done 再等 wg：与 goWrite 的 submitMu 互斥后，此时「已提交的写」集合
	// 已冻结（此后新提交的直接丢弃），wg.Wait 排空即覆盖在途 + 排队两层——
	// 原 sem 探测法在排队写尚未跑到抢槽点时会误判已排空（83d18ae 竞态）。
	u.submitMu.Lock()
	u.closeOnce.Do(func() { close(u.done) })
	u.submitMu.Unlock()
	done := make(chan struct{})
	go func() { u.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(closeDrainTimeout):
		// 兜底超时（单写上限 5s，10s 富余）：卡死的写不应阻塞进程退出。
		log.Printf("[redisstore] WARN: Close 等待在途写超时，放弃（镜像可能未写完）")
	}
	// 排空后再关队列：worker 退出，之后不会再有写被消费（goWrite 已因 done 直接丢弃）。
	u.submitMu.Lock()
	if !u.qClosed {
		close(u.q)
		u.qClosed = true
	}
	u.submitMu.Unlock()
	// worker 退出也必须**有界**（bug B1）：上面那个 10s 超时只约束了第一段等待，
	// 紧接着无界的 workerWG.Wait() 又把 Close 的实际耗时放开——单个 worker 卡在
	// 一次慢写（Upstash 重连时可达 Read/WriteTimeout 量级）就能把 main 的
	// `defer store.Close()` 拖穿 run.sh 的 8s 停机预算，最后 kill -9 把那次 Flush 夺走。
	drained := make(chan struct{})
	go func() { u.workerWG.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(closeWorkerTimeout):
		log.Printf("[redisstore] WARN: Close 等待 worker 退出超时，放弃（镜像可能未写完）")
	}
	return u.closeClient()
}

// closeClient 关底层 redis 连接（client 为 nil——测试构造——时跳过）。
func (u *Upstash) closeClient() error {
	if u.client == nil {
		return nil
	}
	return u.client.Close()
}

func bindKey(key string) string { return bindPrefix + key }

// SetBind 异步镜像粘性会话绑定。
func (u *Upstash) SetBind(key, uid string, ttl time.Duration) {
	if ttl <= 0 {
		ttl = keyTTL
	}
	u.goWrite(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := u.client.Set(ctx, bindKey(key), uid, ttl).Err(); err != nil {
			log.Printf("[redisstore] debug: SetBind %s: %v", key, err)
		}
	})
}

// DelBind 异步删除粘性会话绑定。
func (u *Upstash) DelBind(key string) {
	u.goWrite(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := u.client.Del(ctx, bindKey(key)).Err(); err != nil {
			log.Printf("[redisstore] debug: DelBind %s: %v", key, err)
		}
	})
}

// SaveState 异步写池状态 JSON 快照。
func (u *Upstash) SaveState(data []byte) {
	u.goWrite(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := u.client.Set(ctx, stateKey, data, keyTTL).Err(); err != nil {
			log.Printf("[redisstore] debug: SaveState: %v", err)
		}
	})
}

// LoadState 同步读池状态快照。
func (u *Upstash) LoadState() ([]byte, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
	defer cancel()
	v, err := u.client.Get(ctx, stateKey).Bytes()
	if err != nil {
		return nil, false
	}
	return v, true
}

// LoadBinds 全量读取粘性会话绑定（SCAN bind:* 前缀）。
func (u *Upstash) LoadBinds() map[string]string {
	out := map[string]string{}
	ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
	defer cancel()
	// 先收键、再按批 MGET（上游 2026-10-27 `7f44d06`）：逐键 GET 是 N+1 次公网
	// 往返，Upstash 单次 RTT 50-300ms，几百条绑定必然撞上 3s 总超时 —— 而 iterator
	// 超时只是安静地停下来，冷启动恢复多少全凭运气，日志里一个字都没有。
	keys := make([]string, 0, 256)
	iter := u.client.Scan(ctx, 0, bindPrefix+"*", 200).Iterator()
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	if err := iter.Err(); err != nil {
		// 扫描失败/超时：恢复不完整必须说出来（否则「粘性全丢」只能靠用户体感发现）。
		log.Printf("WARN: [redisstore] 粘性绑定 SCAN 失败，本次恢复不完整: %v", err)
	}
	const batch = 100
	for start := 0; start < len(keys); start += batch {
		end := start + batch
		if end > len(keys) {
			end = len(keys)
		}
		chunk := keys[start:end]
		vals, err := u.client.MGet(ctx, chunk...).Result()
		if err != nil {
			log.Printf("WARN: [redisstore] 粘性绑定 MGET 失败（第 %d-%d 条），本次恢复不完整: %v", start, end, err)
			continue
		}
		for i, key := range chunk {
			if i >= len(vals) || vals[i] == nil {
				continue
			}
			v, ok := vals[i].(string)
			if !ok || v == "" {
				continue
			}
			out[strings.TrimPrefix(key, bindPrefix)] = v
		}
	}
	return out
}

// Noop 纯内存降级：所有方法空实现。
type Noop struct{}

func (Noop) SetBind(string, string, time.Duration) {}
func (Noop) DelBind(string)                        {}
func (Noop) LoadBinds() map[string]string          { return nil }
func (Noop) SaveState([]byte)                      {}
func (Noop) LoadState() ([]byte, bool)             { return nil, false }
func (Noop) Close() error                          { return nil }
