package upstream

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// flushProbe 记录「已 flush 给客户端的字节」：只有 Flush 时才把已写内容算作客户端可见。
type flushProbe struct {
	hdr     http.Header
	written strings.Builder
	flushed string
}

func (f *flushProbe) Header() http.Header {
	if f.hdr == nil {
		f.hdr = http.Header{}
	}
	return f.hdr
}
func (f *flushProbe) Write(p []byte) (int, error) { return f.written.Write(p) }
func (f *flushProbe) WriteHeader(int)             {}
func (f *flushProbe) Flush()                      { f.flushed = f.written.String() }

// laggedReader 每次 Read 只给一帧（模拟上游逐帧到达、帧间有停顿）。给第 i 帧之前检查第
// i-1 帧是否已经 flush 给客户端：没有的话，现实里客户端要多等一整个帧间隔才能看到它。
type laggedReader struct {
	t      *testing.T
	frames []string
	i      int
	w      *flushProbe
	lagged []int
}

func (r *laggedReader) Read(p []byte) (int, error) {
	if r.i >= len(r.frames) {
		return 0, io.EOF
	}
	if r.i > 0 && !strings.Contains(r.w.flushed, fmt.Sprintf(`"content":"f%d"`, r.i-1)) {
		r.lagged = append(r.lagged, r.i-1)
	}
	n := copy(p, r.frames[r.i])
	r.i++
	return n, nil
}

// TestStreamFlushesBeforeBlockingRead 上游逐帧到达时，每一帧都必须在下一次阻塞读之前
// flush 给客户端。
//
// 「缓冲读空才 flush」的判据若把帧后的分隔空行（"\n"）当成「缓冲里还有完整一行」，
// 就会跳过 flush；下一轮只是吞掉这个空行，然后阻塞在 socket 上等下一帧——于是每一帧都
// 要等到下一帧到达才被推给客户端，整条流慢一拍，思考期的停顿里客户端看到的是旧内容。
func TestStreamFlushesBeforeBlockingRead(t *testing.T) {
	const n = 5
	var frames []string
	for i := 0; i < n; i++ {
		frames = append(frames, fmt.Sprintf(`data: {"id":"c","choices":[{"index":0,"delta":{"content":"f%d"}}]}`, i)+"\n\n")
	}
	frames = append(frames, "data: [DONE]\n\n")
	w := &flushProbe{}
	r := &laggedReader{t: t, frames: frames, w: w}
	if err := Stream(w, r); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(r.lagged) > 0 {
		t.Fatalf("这些帧在下一帧到达前都没 flush（每帧慢一拍）: %v", r.lagged)
	}
}
