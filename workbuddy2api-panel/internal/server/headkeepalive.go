package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// headKeepaliveWriter 在「已建连但上游首帧还没到」的空窗里向客户端补 SSE 注释帧。
//
// 为什么需要：StreamHint 是「首帧否决」设计——上游第一个有效帧到达前一个字节都不写
// （internal/upstream/sse.go 的 headOpen 窗口），好处是首帧就是错误时可以静默换号，
// 代价是客户端从建连起零字节干等，等满整个 TTFB。大上下文把这个空窗拉到秒级：
// 线上实测 160–320k 档 TTFB 中位 2.6s / P90 4.8s，320k+ 档 P90 6.1s，而
// 「零内容 + client_gone」的中断全部落在 3.46~4.09s 这个带上。
//
// 注释帧（": " 开头的行）是 SSE 规范定义的 no-op：客户端解析器忽略它，唯一作用是
// 让连接上「一直有数据流动」，使「等首字节」的计时器不会触发。
//
// 三条不变量：
//   - 与真实写共用一把锁：注释帧绝不插进写了一半的事件帧中间。
//   - 首个真实字节写出后立即停手（started），正常流一条注释都不多写。
//   - 停手是终态：之后的空窗不再补注释（流中静默由 continueReader.maybeKeepalive 负责，
//     两处不叠加，否则同一条流会出现两套节奏）。
//
// 代价（注释帧会提前提交响应头，这件事是不可逆的）：
//   - 之后的 w.Header().Set(...) 一律失效，所以 arm() 必须在「账号已选定、成功头
//     （X-Wb-Account / X-Accel-Buffering）已写完」之后才调用。
//   - 全号失败时不能再回 503 + JSON，只能 200 + 协议内 error 帧 + [DONE]
//     （见 writeSSEErrorFrame）。
type headKeepaliveWriter struct {
	http.ResponseWriter
	every time.Duration

	started   atomic.Bool // 已写出真实帧（终态：停手）
	committed atomic.Bool // 已写出任何字节（响应头已提交）

	ticks atomic.Int64 // 实际发出的注释帧数（测试与观测用）

	mu        sync.Mutex
	closeOnce sync.Once
	cancel    context.CancelFunc
	done      chan struct{}
	// firstFrame 在首个真实帧写出时关闭：心跳协程据此立刻退出，而不是等下一次
	// tick 才发现（否则首个慢流在 every 窗口内的所有写都还要过锁）。
	firstFrame chan struct{}
}

// newHeadKeepaliveWriter 包装 w 并立即起心跳协程。every<=0 时返回 nil（调用方据此跳过）。
// ctx 结束或调用 stop() 时协程退出。
func newHeadKeepaliveWriter(ctx context.Context, w http.ResponseWriter, every time.Duration) *headKeepaliveWriter {
	if every <= 0 {
		return nil
	}
	cctx, cancel := context.WithCancel(ctx)
	k := &headKeepaliveWriter{
		ResponseWriter: w, every: every, cancel: cancel,
		done: make(chan struct{}), firstFrame: make(chan struct{}),
	}
	go k.loop(cctx)
	return k
}

// markStarted 记录「首个真实帧已写出」，并让心跳协程立刻退出（只第一次生效）。
func (k *headKeepaliveWriter) markStarted() {
	k.committed.Store(true)
	if k.started.CompareAndSwap(false, true) {
		k.closeOnce.Do(func() { close(k.firstFrame) })
	}
}

// loop 只在「还没有任何真实帧」时补注释；一旦 started 就退出。
func (k *headKeepaliveWriter) loop(ctx context.Context) {
	defer close(k.done)
	t := time.NewTicker(k.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-k.firstFrame:
			// 真实帧已写出：心跳使命完成，协程立刻退出（长流里有上万个 tick，
			// 留着空转只是白唤醒，而且会让热路径继续过锁）。
			return
		case <-t.C:
			if k.started.Load() {
				return
			}
			k.writeComment()
		}
	}
}

// writeComment 写一条注释帧。首次写前必须把 SSE 响应头补齐——注释帧会提交响应头，
// 之后再 Set 全部无效（Content-Type 会变成空，反代也可能开始缓冲）。
func (k *headKeepaliveWriter) writeComment() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.started.Load() { // 抢锁期间真实帧可能已经写出去
		return
	}
	if !k.committed.Load() {
		h := k.ResponseWriter.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		// 反代/网关（Caddy、EdgeOne、Cloudflare）按这个头决定不缓冲，缺了会让
		// 注释帧攒在中间层，等于没发。
		h.Set("X-Accel-Buffering", "no")
	}
	if _, err := k.ResponseWriter.Write([]byte(": keepalive\n\n")); err != nil {
		return
	}
	k.committed.Store(true)
	k.ticks.Add(1)
	if f, ok := k.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// heartbeatOver 报告心跳协程是否已彻底退出。退出之后再也不可能有人写注释帧，
// 于是热路径可以完全跳过互斥锁（单请求上万帧时这是主要的包装开销）。
//
// 为什么不能直接用 started 判断：注释协程可能在「通过 in-lock 检查」之后、
// 「真正写出」之前被抢占，此时 started 已是 true，拿它当「可以免锁」的依据
// 会让两条 goroutine 同时进入底层 Write。done 只在协程 return 之后关闭，
// 语义上等价于「不会再有注释帧」，是唯一安全的无锁判据。
func (k *headKeepaliveWriter) heartbeatOver() bool {
	select {
	case <-k.done:
		return true
	default:
		return false
	}
}

// Write 真实数据帧：先进 started 再抢锁，保证注释帧不会插进这一帧的中间。
func (k *headKeepaliveWriter) Write(p []byte) (int, error) {
	k.markStarted()
	if k.heartbeatOver() {
		return k.ResponseWriter.Write(p)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.ResponseWriter.Write(p)
}

// Flush 透传 http.Flusher —— 不实现它 StreamHint 就拿不到 flusher，流式会被整段缓冲。
func (k *headKeepaliveWriter) Flush() {
	f, ok := k.ResponseWriter.(http.Flusher)
	if !ok {
		return
	}
	if k.heartbeatOver() {
		f.Flush()
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	f.Flush()
}

// Unwrap 供 http.ResponseController 追到底层 writer（Go 1.20+ 的 Flush 探测路径）。
func (k *headKeepaliveWriter) Unwrap() http.ResponseWriter { return k.ResponseWriter }

// Committed 报告响应头是否已提交（= 已经写过字节，不能再回非 200 + JSON）。
func (k *headKeepaliveWriter) Committed() bool { return k.committed.Load() }

// Ticks 返回已发出的注释帧数。
func (k *headKeepaliveWriter) Ticks() int64 { return k.ticks.Load() }

// stop 结束心跳协程（幂等；正常路径也靠 ctx 结束，这里只是让测试可确定）。
func (k *headKeepaliveWriter) stop() {
	if k == nil {
		return
	}
	k.cancel()
	<-k.done
}

// writeErrorFrame 在响应头已提交（注释帧已写出）时把失败收敛成协议内错误：
// 一条 error 帧 + 一条 [DONE]，与 StreamHint「已开流只能 200」的兜底同形态。
// 调用方（writeOpenAIError*）据此不再写 JSON —— 往已提交的 SSE 流里塞 JSON
// 会让客户端收到既非帧也非 JSON 的脏数据。
func (k *headKeepaliveWriter) writeErrorFrame(status int, typ, code, msg, hint string) {
	k.writeErrorFrameExtra(status, typ, code, msg, hint, nil)
}

// writeErrorFrameExtra 同 writeErrorFrame，另在 error 对象里附加 extra 字段。
// 响应头已提交后不能再改 HTTP 状态码，而有些客户端（ZCode）只按状态码/业务码
// 判「能不能重试」——把 statusCode=429 这类信号塞进协议内错误体，客户端才能
// 按可重试处理，而不是把整轮对话判死。
func (k *headKeepaliveWriter) writeErrorFrameExtra(status int, typ, code, msg, hint string, extra map[string]any) {
	e := map[string]any{"message": msg, "type": typ, "code": code}
	if hint != "" {
		e["gateway_hint"] = hint
	}
	for key, v := range extra {
		if _, exists := e[key]; !exists {
			e[key] = v
		}
	}
	body, err := json.Marshal(map[string]any{"error": e})
	if err != nil {
		return
	}
	_, _ = k.Write([]byte("data: " + string(body) + "\n\ndata: [DONE]\n\n"))
}
