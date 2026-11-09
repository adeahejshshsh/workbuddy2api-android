package upstream

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// flushCounter 记录 Flush 次数；同时收集写出字节。
type flushCounter struct {
	hdr    http.Header
	body   strings.Builder
	status int
	flush  int
}

func (f *flushCounter) Header() http.Header {
	if f.hdr == nil {
		f.hdr = http.Header{}
	}
	return f.hdr
}
func (f *flushCounter) Write(p []byte) (int, error) { return f.body.Write(p) }
func (f *flushCounter) WriteHeader(s int)           { f.status = s }
func (f *flushCounter) Flush()                      { f.flush++ }

// TestStreamFlushOnlyWhenDrained 上游一次给一批帧时，不该每帧都 flush。
//
// 「并发一高就降速」的真实形态：一条长回答被切成上万帧成批到达，每帧一次
// write 系统调用（线上 profile 里 Flush+syscall 占流式 CPU 近一半）。本地缓冲
// 还有完整帧时必须跳过 flush——下一轮立刻就会继续写。
//
// 判据用「次数上限」而不是精确值：只要不是「一帧一次」就说明批量生效了。
func TestStreamFlushOnlyWhenDrained(t *testing.T) {
	const frames = 20
	var sb strings.Builder
	for i := 0; i < frames; i++ {
		sb.WriteString(`data: {"id":"c","choices":[{"index":0,"delta":{"content":"字"}}]}` + "\n\n")
	}
	sb.WriteString("data: [DONE]\n\n")

	w := &flushCounter{}
	if err := Stream(w, strings.NewReader(sb.String())); err != nil {
		t.Fatalf("stream: %v", err)
	}
	// 全部数据一次性可读 → bufio 一次填满，只有末尾/换行边界才会 flush。
	if w.flush >= frames/2 {
		t.Fatalf("flush 次数 %d（%d 帧）：批量未生效，等于每帧一次系统调用", w.flush, frames)
	}
	// 内容必须完整：20 帧正文 + [DONE]。
	if n := strings.Count(w.body.String(), `"content":"字"`); n != frames {
		t.Fatalf("正文帧丢失: %d/%d\n%s", n, frames, w.body.String())
	}
	if !strings.HasSuffix(w.body.String(), "data: [DONE]\n\n") {
		t.Fatalf("[DONE] 缺失:\n%s", w.body.String())
	}
}

// TestStreamFlushesOnPartialLine 缓冲里只剩半行时仍必须 flush。
//
// 半行意味着下一次 ReadString 会阻塞在 socket 上；若此时不 flush，上游一旦在此
// 停顿，客户端会为一个已经生成好的帧干等到下一批数据到达（长思考段的静默正是
// 客户端 idle 超时的主要来源）。
func TestStreamFlushesOnPartialLine(t *testing.T) {
	// 先给一个完整帧 + 半帧（无换行结尾），再给剩余部分。
	full := `data: {"id":"c","choices":[{"index":0,"delta":{"content":"甲"}}]}` + "\n\n"
	partial := `data: {"id":"c","choices":[{"index":0,"delta":{"content":"乙`
	rest := `"}}]}` + "\n\ndata: [DONE]\n\n"

	w := &flushCounter{}
	r := &chunkReader{chunks: []string{full + partial, rest}}
	if err := Stream(w, r); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if w.flush == 0 {
		t.Fatal("半行边界处没有 flush：客户端会在上游停顿时干等")
	}
	if !strings.Contains(w.body.String(), "甲") || !strings.Contains(w.body.String(), "乙") {
		t.Fatalf("正文缺失:\n%s", w.body.String())
	}
}

// chunkReader 按块返回数据，模拟网络分片到达。
type chunkReader struct {
	chunks []string
	i      int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if c.i >= len(c.chunks) {
		return 0, io.EOF
	}
	n := copy(p, c.chunks[c.i])
	if n < len(c.chunks[c.i]) {
		c.chunks[c.i] = c.chunks[c.i][n:]
	} else {
		c.i++
	}
	return n, nil
}
