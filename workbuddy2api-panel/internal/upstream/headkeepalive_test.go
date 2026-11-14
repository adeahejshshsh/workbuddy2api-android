package upstream

import (
	"io"
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
