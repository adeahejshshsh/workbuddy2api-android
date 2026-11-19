// headorder_conn_reuse_test.go W1-02 回归：keep-alive 连接上的**第二个**响应/请求。
//
// 既有 TestResponseBoundaryResetsPhase 每次用 `respTracker{}` 新实例，所以
// `started` 永不复位这个缺陷一直没被钉住：真实连接上 tracker 是复用的，
// 第二个响应会被当成第一个的 body 消费掉，头序重排在第二条请求起静默失效。
package upstream

import (
	"bytes"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestRespTrackerReuseTwoResponses 同一 tracker 连续两个响应都要正确判定边界。
func TestRespTrackerReuseTwoResponses(t *testing.T) {
	var tr respTracker

	// 第一个响应：Content-Length: 2 + "ok"。
	if !tr.feed([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")) {
		t.Fatal("第一个响应应判定完成")
	}
	// 复位不彻底的特征：started 残留 → 第二个响应的头会被当成 body。
	if tr.started || tr.inHead {
		t.Fatalf("完成后 tracker 未复位：started=%v inHead=%v", tr.started, tr.inHead)
	}

	// 第二个响应：头先到（body 未到）→ 必须**不**判完成；再来 body 才完成。
	if tr.feed([]byte("HTTP/1.1 200 OK\r\nContent-Length: 3\r\n\r\n")) {
		t.Fatal("第二个响应 body 未到就判完成（started 残留的典型症状）")
	}
	if !tr.feed([]byte("abc")) {
		t.Fatal("第二个响应 body 满应判定完成")
	}
}

// TestRespTrackerReuseChunkedSecondResponse chunked 的第二个响应也不能被当成 trailer。
func TestRespTrackerReuseChunkedSecondResponse(t *testing.T) {
	var tr respTracker
	if !tr.feed([]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")) {
		t.Fatal("第一个响应应完成")
	}
	if tr.feed([]byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabc\r\n")) {
		t.Fatal("第二个 chunked 响应未到结束块，不该完成")
	}
	if !tr.feed([]byte("0\r\n\r\n")) {
		t.Fatal("第二个 chunked 响应结束块应判定完成")
	}
}

// TestHeadOrderConnReordersSecondRequest 同一连接两个请求都要按官方顺序出站。
//
// 缺陷形态：第一个响应结束后 tracker 没复位 → 判不出第二个响应边界 →
// phase 不再回到 phaseAwaitRequest → 第二个请求头原样（字母序）写出。
func TestHeadOrderConnReordersSecondRequest(t *testing.T) {
	c, peer := newTestPipe(t)
	// 防挂死：断言失败/协程提前退出时不要让整个测试二进制卡到 -test.timeout。
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	first, headers := splitFixture(t, "official_v3config_head.txt")
	block := buildBlock(first, shuffleHeaders(headers))

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		got1 []byte
		got2 []byte
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 8192)
		n, err := peer.Read(buf) // 第一个请求
		if err != nil {
			return
		}
		mu.Lock()
		got1 = append([]byte{}, buf[:n]...)
		mu.Unlock()
		// 回一个完整响应，让 Read 侧判定边界。
		if _, werr := peer.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")); werr != nil {
			return
		}
		n, err = peer.Read(buf) // 第二个请求
		if err != nil {
			return
		}
		mu.Lock()
		got2 = append([]byte{}, buf[:n]...)
		mu.Unlock()
	}()

	if _, err := c.Write(block); err != nil {
		t.Fatalf("write1: %v", err)
	}
	// 读走响应（Read 侧据此复位相位）。
	rbuf := make([]byte, 512)
	deadline := time.Now().Add(2 * time.Second)
	for {
		n, err := c.Read(rbuf)
		if err != nil {
			break
		}
		if n > 0 && bytes.Contains(rbuf[:n], []byte("ok")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("等待响应超时")
		}
	}
	if _, err := c.Write(block); err != nil {
		t.Fatalf("write2: %v", err)
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	for i, got := range [][]byte{got1, got2} {
		if !strings.HasPrefix(string(got), "GET /v3/config HTTP/1.1\r\nAccept: ") {
			t.Fatalf("第 %d 个请求未按官方顺序重排：\n%s", i+1, got)
		}
	}
}

// scriptedConn 按预设分片返回 Read 数据（不碰真实 socket，时序完全确定）。
type scriptedConn struct {
	net.Conn
	chunks [][]byte
	wrote  bytes.Buffer
}

func (s *scriptedConn) Read(p []byte) (int, error) {
	if len(s.chunks) == 0 {
		return 0, io.EOF
	}
	ch := s.chunks[0]
	s.chunks = s.chunks[1:]
	return copy(p, ch), nil
}
func (s *scriptedConn) Write(p []byte) (int, error) { return s.wrote.Write(p) }
func (s *scriptedConn) Close() error                { return nil }

// TestHeadOrderConnReadResetsTracker 连接层 Read 完成一个响应后必须换新 tracker。
//
// 这是 W1-02 的直接症状断言：第二个响应（chunked）第一片到达时，
// 若 tracker 还是第一个响应的实例（started=true / remain=0），会被立刻判成
// 「已完成」→ 相位在响应体仍在流动时退回 phaseAwaitRequest。
func TestHeadOrderConnReadResetsTracker(t *testing.T) {
	sc := &scriptedConn{chunks: [][]byte{
		[]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"),
		[]byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabc\r\n"),
		[]byte("0\r\n\r\n"),
	}}
	c := &headOrderConn{Conn: sc}
	buf := make([]byte, 256)

	if _, err := c.Read(buf); err != nil {
		t.Fatalf("read1: %v", err)
	}
	if c.phase != phaseAwaitRequest {
		t.Fatalf("第一个响应结束后相位未复位：%v", c.phase)
	}
	if c.resp.started || c.resp.inHead {
		t.Fatalf("第一个响应结束后 tracker 未换新：started=%v inHead=%v", c.resp.started, c.resp.inHead)
	}

	// 第二个响应头 + 一个 chunk 到达：绝不能判完成。
	if _, err := c.Read(buf); err != nil {
		t.Fatalf("read2: %v", err)
	}
	// 判据：tracker 必须已按 chunked 解析第二个响应（chunked=true）。
	// 若还停留在第一个响应（Content-Length）的状态，feedBody 会因 remain=0 立刻
	// 返回 true —— 相位就在响应体仍流动时被复位了。
	if !c.resp.chunked || !c.resp.started {
		t.Fatalf("第二个 chunked 响应未被识别（tracker 仍是上一个响应的状态）：chunked=%v started=%v",
			c.resp.chunked, c.resp.started)
	}
	// 结束块到达才算完成。
	if _, err := c.Read(buf); err != nil {
		t.Fatalf("read3: %v", err)
	}
	if c.phase != phaseAwaitRequest {
		t.Fatal("第二个响应结束后相位未复位")
	}
}
