package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// syncBuf 是并发安全的 ResponseWriter + Flusher，用来观察「注释帧与真实帧的交错」。
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
	hdr http.Header
	fl  int
}

func newSyncBuf() *syncBuf { return &syncBuf{hdr: http.Header{}} }

func (b *syncBuf) Header() http.Header { return b.hdr }
func (b *syncBuf) WriteHeader(int)     {}
func (b *syncBuf) Flush()              { b.mu.Lock(); b.fl++; b.mu.Unlock() }
func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestHeadKeepaliveQuietWhenFirstFrameArrivesFast 首帧快到时一条注释都不发（正常流零开销）。
func TestHeadKeepaliveQuietWhenFirstFrameArrivesFast(t *testing.T) {
	buf := newSyncBuf()
	k := newHeadKeepaliveWriter(context.Background(), buf, 50*time.Millisecond)
	defer k.stop()

	if _, err := k.Write([]byte("data: {\"a\":1}\n\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	time.Sleep(180 * time.Millisecond) // 远超过 3 个间隔

	if n := k.Ticks(); n != 0 {
		t.Fatalf("首帧已写出，却补了 %d 条注释帧", n)
	}
	if got := buf.String(); got != "data: {\"a\":1}\n\n" {
		t.Fatalf("输出被污染: %q", got)
	}
}

// TestHeadKeepaliveTicksWhileWaiting 首帧慢到：按间隔补注释，首帧到达后立即停手。
func TestHeadKeepaliveTicksWhileWaiting(t *testing.T) {
	buf := newSyncBuf()
	k := newHeadKeepaliveWriter(context.Background(), buf, 20*time.Millisecond)
	defer k.stop()

	time.Sleep(110 * time.Millisecond) // 等 5 个间隔
	before := k.Ticks()
	if before < 2 {
		t.Fatalf("等待期间注释帧只有 %d 条，期望 >= 2", before)
	}
	// 注释帧必须先补齐 SSE 响应头，否则客户端拿到的 Content-Type 是空的。
	if ct := buf.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("注释帧未设置 Content-Type: %q", ct)
	}
	if v := buf.Header().Get("X-Accel-Buffering"); v != "no" {
		t.Fatalf("注释帧未设置 X-Accel-Buffering: %q", v)
	}

	if _, err := k.Write([]byte("data: real\n\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	atFirstFrame := k.Ticks()
	time.Sleep(100 * time.Millisecond)
	if got := k.Ticks(); got != atFirstFrame {
		t.Fatalf("首帧写出后又补了注释：%d -> %d", atFirstFrame, got)
	}
	if !strings.HasSuffix(buf.String(), "data: real\n\n") {
		t.Fatalf("首帧之后还有别的内容: %q", buf.String())
	}
}

// TestHeadKeepaliveNeverSplitsFrames 并发压力下注释帧只能落在帧边界上。
func TestHeadKeepaliveNeverSplitsFrames(t *testing.T) {
	buf := newSyncBuf()
	k := newHeadKeepaliveWriter(context.Background(), buf, time.Millisecond)

	const frames = 300
	payload := "data: {\"choices\":[{\"delta\":{\"content\":\"" + strings.Repeat("x", 80) + "\"}}]}\n\n"
	for i := 0; i < frames; i++ {
		if _, err := k.Write([]byte(payload)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	k.stop()

	out := buf.String()
	// 按 SSE 事件分隔切开：每段要么是注释帧，要么是完整的一帧原文。
	for _, seg := range strings.Split(out, "\n\n") {
		if seg == "" {
			continue
		}
		if seg == ": keepalive" {
			continue
		}
		if seg != strings.TrimSuffix(payload, "\n\n") {
			t.Fatalf("切出了非帧边界的内容: %q", seg)
		}
	}
	if got := strings.Count(out, "data: "); got != frames {
		t.Fatalf("真实帧数量 %d != %d（被切碎或有丢失）", got, frames)
	}
}

// overlapWriter 检测「两个 goroutine 同时进入底层 Write」——http.ResponseWriter 不是
// 并发安全的，心跳协程与流式写并发时靠 headKeepaliveWriter 的锁串行化。syncBuf 自带
// 锁会把这个错误吞掉，所以这里用原子计数专门探测重入。
type overlapWriter struct {
	hdr       http.Header
	slowWrite time.Duration
	inWrite   atomic.Int32
	overlaps  atomic.Int32
	writes    atomic.Int32
}

func (o *overlapWriter) Header() http.Header { return o.hdr }
func (o *overlapWriter) WriteHeader(int)     {}
func (o *overlapWriter) Flush()              {}
func (o *overlapWriter) Write(p []byte) (int, error) {
	if o.inWrite.Add(1) > 1 {
		o.overlaps.Add(1)
	}
	defer o.inWrite.Add(-1)
	if o.slowWrite > 0 {
		time.Sleep(o.slowWrite) // 拉宽窗口，让并发 Write 必然撞上
	}
	o.writes.Add(1)
	return len(p), nil
}

func TestHeadKeepaliveNeverConcurrentWrite(t *testing.T) {
	o := &overlapWriter{hdr: http.Header{}, slowWrite: 30 * time.Millisecond}
	k := newHeadKeepaliveWriter(context.Background(), o, 5*time.Millisecond)
	defer k.stop()
	// 先让心跳跑起来（它会先写一条注释，这条本身也慢），
	// 再在注释仍在写的过程中发起真实帧写入 —— 没有锁时两者必然并发进入底层 Write。
	time.Sleep(12 * time.Millisecond)
	if _, err := k.Write([]byte("data: x\n\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if n := o.overlaps.Load(); n != 0 {
		t.Fatalf("底层 Write 被并发进入 %d 次（锁没生效）", n)
	}
}
func TestHeadKeepaliveStopsOnCtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	buf := newSyncBuf()
	k := newHeadKeepaliveWriter(ctx, buf, 10*time.Millisecond)
	time.Sleep(35 * time.Millisecond)
	cancel()
	<-k.done
	n := k.Ticks()
	time.Sleep(40 * time.Millisecond)
	if got := k.Ticks(); got != n {
		t.Fatalf("ctx 取消后仍在补注释: %d -> %d", n, got)
	}
}

// TestCommittedErrorBecomesSSEFrame 响应头已提交时，错误必须降级成协议内 error 帧。
func TestCommittedErrorBecomesSSEFrame(t *testing.T) {
	buf := newSyncBuf()
	k := newHeadKeepaliveWriter(context.Background(), buf, 10*time.Millisecond)
	defer k.stop()
	time.Sleep(30 * time.Millisecond) // 让它先提交响应头
	if !k.Committed() {
		t.Fatal("预期注释帧已提交响应头")
	}

	writeOpenAIErrorHint(k, http.StatusServiceUnavailable, "no_healthy_account", "all accounts busy", "hint-x")

	out := buf.String()
	if strings.Contains(out, `"no_healthy_account"`) && !strings.Contains(out, "data: ") {
		t.Fatalf("已提交的流里塞进了裸 JSON: %q", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("缺 [DONE]: %q", out)
	}
	// 取出最后一条 data 帧，确认是 error 对象且带 gateway_hint。
	var got map[string]any
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "data: {") {
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &got); err != nil {
				t.Fatalf("错误帧不是合法 JSON: %v", err)
			}
		}
	}
	e, ok := got["error"].(map[string]any)
	if !ok {
		t.Fatalf("错误帧缺 error 对象: %v", got)
	}
	if e["code"] != "no_healthy_account" || e["gateway_hint"] != "hint-x" {
		t.Fatalf("错误帧字段不对: %v", e)
	}
}

// TestUncommittedErrorStaysJSON 没有补过注释时，仍然回 503 + JSON（旧行为不变）。
func TestUncommittedErrorStaysJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	k := newHeadKeepaliveWriter(context.Background(), rec, 10*time.Second)
	defer k.stop()

	writeOpenAIErrorHint(k, http.StatusServiceUnavailable, "no_healthy_account", "all accounts busy", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("未提交时应回 503，实际 %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type 应为 JSON，实际 %q", ct)
	}
}

// BenchmarkWriteDirect / BenchmarkWriteWrapped：量心跳包装在热路径（每帧一次写）上的开销。
func BenchmarkWriteDirect(b *testing.B) {
	buf := newSyncBuf()
	payload := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = buf.Write(payload)
	}
}

func BenchmarkWriteWrapped(b *testing.B) {
	buf := newSyncBuf()
	k := newHeadKeepaliveWriter(context.Background(), buf, time.Hour) // 整段基准内不触发心跳
	defer k.stop()
	payload := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = k.Write(payload)
	}
}

// 真实热路径是「写一帧 + flush 一次」（StreamHint 的 flushIfDrained），
// 单独量 Write 会低估包装开销——Flush 也要过锁。
func BenchmarkWriteFlushDirect(b *testing.B) {
	buf := newSyncBuf()
	payload := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = buf.Write(payload)
		buf.Flush()
	}
}

func BenchmarkWriteFlushWrapped(b *testing.B) {
	buf := newSyncBuf()
	k := newHeadKeepaliveWriter(context.Background(), buf, time.Hour)
	defer k.stop()
	payload := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = k.Write(payload)
		k.Flush()
	}
}
