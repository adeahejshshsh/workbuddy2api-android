package upstream

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"
)

// TestMonitorBodyIdleCutoff 静默超过 idle（配小值）→ Read 返回错误（context canceled）。
func TestMonitorBodyIdleCutoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 模拟真实 resp.Body：阻塞直到请求 context 被 cancel 才返回 error。
	body := monitorBody(&ctxBoundReader{ctx: ctx}, 50*time.Millisecond, cancel)
	defer body.Close()
	if _, err := body.Read(make([]byte, 16)); err == nil {
		t.Fatal("expect read error after idle cutoff")
	}
}

// TestMonitorBodyRenewsOnActivity 持续活跃（周期吐数据，总时长 > idle）→ 流不被掐。
func TestMonitorBodyRenewsOnActivity(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 每 30ms 吐 1 字节；idle=80ms。总读 10 字节耗时 300ms > idle：
	// 若空闲续命失效（绝对 deadline 语义）早在 80ms 就被掐断。
	body := monitorBody(nopCloserBody{Reader: periodicReader{period: 30 * time.Millisecond}}, 80*time.Millisecond, cancel)
	defer body.Close()
	buf := make([]byte, 1)
	for i := 0; i < 10; i++ {
		if _, err := io.ReadFull(body, buf); err != nil {
			t.Fatalf("read %d: %v (active stream must not be cut)", i, err)
		}
	}
}

// TestMonitorBodyDisabledWhenIdleZero idle<=0 直接返回原底流。
// bug #9：idle<=0 时不启动空闲监控，但**必须**把 cancel 交给返回的 body ——
// 原先直接 `return rc` 把 cancel 丢了，派生 ctx 只能等父 ctx 结束才释放
// （长连接场景每个请求留一个 ctx 节点，go vet 的 lostcancel 也会报）。
func TestMonitorBodyDisabledWhenIdleZero(t *testing.T) {
	cancelled := false
	cancel := func() { cancelled = true }
	rc := nopCloserBody{Reader: &oneByteReader{}}
	out := monitorBody(rc, 0, cancel)
	if _, ok := out.(*idleMonitoringBody); ok {
		t.Fatal("idle<=0 不该启动空闲监控")
	}
	buf := make([]byte, 1)
	if _, err := out.Read(buf); err != nil || buf[0] != 'a' {
		t.Fatalf("read=%q err=%v", buf, err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !cancelled {
		t.Fatal("Close 必须调用 cancel，否则派生 ctx 泄漏到父 ctx 结束")
	}
}

// TestMonitorBodyCloseStopsGoroutine Close 停掉 goroutine，无泄漏（等效监控循环 + 短周期）。
func TestMonitorBodyCloseStopsGoroutine(t *testing.T) {
	m := &idleMonitoringBody{
		rc:       &ctxBoundReader{ctx: context.Background()},
		lastRead: time.Now(),
		stopCh:   make(chan struct{}),
		cancel:   func() {},
	}
	stopped := make(chan struct{})
	go func() {
		tk := time.NewTicker(idleTick(40 * time.Millisecond))
		defer tk.Stop()
		for {
			select {
			case <-m.stopCh:
				close(stopped)
				return
			case <-tk.C:
			}
		}
	}()
	m.Close()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("goroutine not stopped after Close")
	}
}

// —— helpers ——

// ctxBoundReader 模拟 net/http resp.Body：Read 阻塞在 ctx.Done() 上，ctx cancel 后返回错误。
type ctxBoundReader struct{ ctx context.Context }

func (r *ctxBoundReader) Read([]byte) (int, error) {
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

func (r *ctxBoundReader) Close() error { return nil }

type periodicReader struct{ period time.Duration }

func (r periodicReader) Read(p []byte) (int, error) {
	time.Sleep(r.period)
	p[0] = 'x'
	return 1, nil
}

type oneByteReader struct{ done bool }

func (r *oneByteReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	p[0] = 'a'
	return 1, nil
}

type nopCloserBody struct{ io.Reader }

func (nopCloserBody) Close() error { return nil }

func TestStreamConnReusedAfterDone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range []string{`data: {"choices":[{"delta":{"content":"hi"}}]}`, "data: [DONE]"} {
			_, _ = io.WriteString(w, f+"\n\n")
			w.(http.Flusher).Flush()
		}
		// 上游在 [DONE] 之后过一会儿才发 chunked 结束块：读者在这之前就已关闭。
		time.Sleep(20 * time.Millisecond)
	}))
	defer srv.Close()
	cl := &http.Client{Transport: newTransport()}
	defer cl.CloseIdleConnections()

	stream := func() (reused bool) {
		ctx, cancel := context.WithCancel(context.Background())
		ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) { reused = i.Reused }})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, nil)
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body := monitorBody(resp.Body, time.Minute, cancel)
		br := bufio.NewReader(body)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				t.Fatalf("没读到 [DONE]：%v", err)
			}
			if strings.TrimSpace(line) == "data: [DONE]" {
				break
			}
		}
		body.Close()
		return reused
	}
	stream()
	for i := 0; i < 5; i++ {
		if !stream() {
			t.Fatalf("第 %d 次流式请求没有复用连接", i+2)
		}
	}
}

// closeCounter 记 Close 次数与 Close 之后的 Read：幂等的 Close 只关一次底流，之后也不再读它。
type closeCounter struct {
	closes, readsAfterClose int
}

func (c *closeCounter) Read(p []byte) (int, error) {
	if c.closes > 0 {
		c.readsAfterClose++
		return 0, errors.New("read on closed body")
	}
	return 0, io.EOF
}

func (c *closeCounter) Close() error { c.closes++; return nil }

// Close 幂等：调用链里两层各关一次是常态，第二次不能再读流尾、再关底流。
func TestIdleBodyCloseIsIdempotent(t *testing.T) {
	under := &closeCounter{}
	canceled := 0
	b := monitorBody(under, time.Minute, func() { canceled++ })
	for range 3 {
		if err := b.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if under.closes != 1 || under.readsAfterClose != 0 || canceled != 1 {
		t.Fatalf("closes=%d readsAfterClose=%d canceled=%d, want 1/0/1", under.closes, under.readsAfterClose, canceled)
	}
}
