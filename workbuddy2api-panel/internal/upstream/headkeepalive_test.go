package upstream

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// delayedReader 第一次 Read 前先睡 delay（上游 200 已回、首帧迟到）。
type delayedReader struct {
	r     io.Reader
	delay time.Duration
	slept bool
}

func (d *delayedReader) Read(p []byte) (int, error) {
	if !d.slept {
		d.slept = true
		time.Sleep(d.delay)
	}
	return d.r.Read(p)
}

const hbStream = `data: {"id":"c","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n" +
	`data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
	"data: [DONE]\n\n"

// TestHeadKeepaliveTicksUntilFirstFrame 首帧迟到：等待期间按间隔补注释帧，首帧之后一条都不再补。
func TestHeadKeepaliveTicksUntilFirstFrame(t *testing.T) {
	rec := httptest.NewRecorder()
	r := &delayedReader{r: strings.NewReader(hbStream), delay: 60 * time.Millisecond}
	if err := StreamHint(rec, r, nil, WithHeadKeepalive(10*time.Millisecond)); err != nil {
		t.Fatalf("StreamHint: %v", err)
	}
	body := rec.Body.String()
	first := strings.Index(body, "data: ")
	if first < 0 || strings.Count(body[:first], ": keepalive\n\n") < 2 {
		t.Fatalf("首帧迟到期间心跳不足:\n%s", body)
	}
	if strings.Contains(body[first:], ": keepalive") {
		t.Fatalf("首帧之后还在心跳:\n%s", body)
	}
	if ct := rec.Result().Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("心跳提交的 Content-Type=%q", ct)
	}
}

// TestHeadKeepaliveQuietWhenFast 首帧及时到达：一条注释都不写（正常流零改动）。
func TestHeadKeepaliveQuietWhenFast(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := StreamHint(rec, strings.NewReader(hbStream), nil, WithHeadKeepalive(time.Second)); err != nil {
		t.Fatalf("StreamHint: %v", err)
	}
	if strings.Contains(rec.Body.String(), ": keepalive") {
		t.Fatalf("首帧及时到达却写了心跳:\n%s", rec.Body)
	}
}

// failResponseWriter 写必失败（模拟客户端已断开）。
type failResponseWriter struct {
	h    http.Header
	code int
}

func (w *failResponseWriter) Header() http.Header {
	if w.h == nil {
		w.h = http.Header{}
	}
	return w.h
}
func (w *failResponseWriter) Write([]byte) (int, error) { return 0, errors.New("client gone") }
func (w *failResponseWriter) WriteHeader(c int)         { w.code = c }

// blockingReader 永久阻塞在 Read（模拟「上游 200 已回、首帧一直不来」）。
type blockingReader struct{ release chan struct{} }

func (r *blockingReader) Read([]byte) (int, error) {
	<-r.release
	return 0, io.EOF
}

// TestHeadKeepaliveStopsWaitOnClientGone W1-05 回归。
//
// 缺陷：首帧窗口内心跳写失败只置 hbDead，不中断等待 —— 协程钉在「上游下一次吐字节」
// 上（ChatHTTP.Timeout=0，只能等 IdleTimeout；idle_timeout=0 则永久泄漏），
// 协程 + 上游连接 + 账号在途名额一起被占。本用例的核心断言是**及时返回**。
func TestHeadKeepaliveStopsWaitOnClientGone(t *testing.T) {
	br := &blockingReader{release: make(chan struct{})}
	defer close(br.release) // 放走辅助协程的阻塞读，避免泄漏到其它用例

	w := &failResponseWriter{}
	done := make(chan error, 1)
	go func() {
		done <- StreamHint(w, br, nil, WithHeadKeepalive(5*time.Millisecond))
	}()

	select {
	case err := <-done:
		if !IsClientWriteError(err) {
			t.Fatalf("客户端断开应按写客户端失败收尾，得到 %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("W1-05：客户端已断，StreamHint 仍在等上游首帧（协程被钉住）")
	}
}
