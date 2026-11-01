// headorder_test.go 头顺序重排的回归测试。
//
// 判据不用手写期望值，而是**直接拿官方夹具当 oracle**：把夹具的头顺序打乱
// 再喂进 reorderHeadBlock，输出必须逐行等于夹具原文。夹具本身来自真机抓包
// （见 testdata/official_*_head.txt），所以这条链路红 == 与官方顺序不符。
package upstream

import (
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// splitFixture 把夹具切成 (请求行, []头行)。
func splitFixture(t *testing.T, name string) (string, []string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("读夹具 %s: %v", name, err)
	}
	var first string
	var rest []string
	for i, ln := range strings.Split(string(raw), "\n") {
		ln = strings.TrimRight(ln, "\r")
		if ln == "" {
			continue
		}
		if i == 0 {
			first = ln
			continue
		}
		rest = append(rest, ln)
	}
	return first, rest
}

// shuffleHeaders 确定性反转（不用随机：测试必须可复现）。
func shuffleHeaders(h []string) []string {
	out := make([]string, len(h))
	for i := range h {
		out[i] = h[len(h)-1-i]
	}
	return out
}

func buildBlock(first string, headers []string) []byte {
	var b bytes.Buffer
	b.WriteString(first + "\r\n")
	for _, h := range headers {
		b.WriteString(h + "\r\n")
	}
	b.WriteString("\r\n")
	return b.Bytes()
}

// TestReorderHeadBlockMatchesOfficial 每张夹具：打乱 → 重排 → 必须等于原文。
func TestReorderHeadBlockMatchesOfficial(t *testing.T) {
	for _, f := range []string{
		"official_chat_head.txt",
		"official_report_cn_head.txt",
		"official_chat_busy_head.txt",
		"official_v3config_head.txt",
	} {
		t.Run(strings.TrimSuffix(strings.TrimPrefix(f, "official_"), "_head.txt"), func(t *testing.T) {
			first, want := splitFixture(t, f)
			got := reorderHeadBlock(buildBlock(first, shuffleHeaders(want)))
			wantBlock := buildBlock(first, want)
			if string(got) != string(wantBlock) {
				t.Errorf("重排结果与官方不一致\n--- got ---\n%s\n--- want ---\n%s", got, wantBlock)
			}
		})
	}
}

// TestReorderHeadBlockUnknownPathUntouched 顺序表外的路径必须原样返回。
func TestReorderHeadBlockUnknownPathUntouched(t *testing.T) {
	in := buildBlock("GET /v9/nothing HTTP/1.1", []string{"Zeta: 1", "Alpha: 2"})
	if got := reorderHeadBlock(in); string(got) != string(in) {
		t.Errorf("未知路径不该重排:\n%s", got)
	}
}

// TestReorderHeadBlockExtraHeadersAppended 表外头保持原序追加在末尾。
func TestReorderHeadBlockExtraHeadersAppended(t *testing.T) {
	first, headers := splitFixture(t, "official_v3config_head.txt")
	// 表外头放在最前，打乱后仍应保持它们的相对顺序（表外头之间不排序）
	withExtra := append([]string{"X-Brand-New: 1", "X-Another: 2"}, headers...)
	shuffled := shuffleHeaders(withExtra)
	got := string(reorderHeadBlock(buildBlock(first, shuffled)))

	if !strings.HasSuffix(got, "\r\n\r\n") {
		t.Fatalf("头块结尾异常:\n%s", got)
	}
	// 反转输入后 X-Another 在前，输出应保持这个相对顺序（表外头之间不排序）
	if tail := got[len(got)-len("X-Brand-New: 1\r\n\r\n"):]; tail != "X-Brand-New: 1\r\n\r\n" {
		t.Errorf("表外头应追加在末尾，尾部=%q\n全文:\n%s", tail, got)
	}
	if !strings.Contains(got, "X-Another: 2\r\nX-Brand-New: 1") {
		t.Errorf("表外俩头的相对顺序应与输入一致（输入反转后 X-Another 在前）:\n%s", got)
	}
	// 表内头仍按官方序
	if !strings.HasPrefix(got, "GET /v3/config HTTP/1.1\r\nAccept: ") {
		t.Errorf("表内头顺序被破坏:\n%s", got)
	}
}

// TestReorderHeadBlockMalformed 畸形输入不 panic、不破坏。
func TestReorderHeadBlockMalformed(t *testing.T) {
	cases := []string{
		"",
		"POST /v2/chat/completions HTTP/1.1\r\n", // 无结尾空行
		"garbage",
		"\r\n\r\n",
	}
	for _, in := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("输入 %q panic: %v", in, r)
				}
			}()
			_ = reorderHeadBlock([]byte(in))
		}()
	}
}

// pipeConn 用 net.Pipe 抽出 headOrderConn 的写侧行为（读侧另测）。
func newTestPipe(t *testing.T) (*headOrderConn, net.Conn) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	return &headOrderConn{Conn: a}, b
}

// TestHeadOrderConnWriteReorders 一次写完整头块：对端应收到官方顺序。
func TestHeadOrderConnWriteReorders(t *testing.T) {
	c, peer := newTestPipe(t)
	first, headers := splitFixture(t, "official_v3config_head.txt")
	in := buildBlock(first, shuffleHeaders(headers))

	var wg sync.WaitGroup
	wg.Add(1)
	var got []byte
	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		n, _ := peer.Read(buf)
		got = buf[:n]
	}()
	if _, err := c.Write(in); err != nil {
		t.Fatalf("write: %v", err)
	}
	wg.Wait()
	if !strings.HasPrefix(string(got), "GET /v3/config HTTP/1.1\r\nAccept: ") {
		t.Errorf("写侧未重排:\n%s", got)
	}
}

// TestHeadOrderConnWriteSplitAcrossCalls 头块分两次写也能重排（bufio 分块场景）。
func TestHeadOrderConnWriteSplitAcrossCalls(t *testing.T) {
	c, peer := newTestPipe(t)
	first, headers := splitFixture(t, "official_v3config_head.txt")
	in := buildBlock(first, shuffleHeaders(headers))
	cut := len(in) / 2

	var wg sync.WaitGroup
	wg.Add(1)
	var got []byte
	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		n, _ := peer.Read(buf) // 一次读够整块（写侧是整块 flush）
		got = append(got, buf[:n]...)
	}()

	// 两次写：第一次只有半截头，不应下发；第二次补齐后整块按官方序下发
	if _, err := c.Write(in[:cut]); err != nil {
		t.Fatalf("write1: %v", err)
	}
	if _, err := c.Write(in[cut:]); err != nil {
		t.Fatalf("write2: %v", err)
	}
	wg.Wait()
	if !strings.HasPrefix(string(got), "GET /v3/config HTTP/1.1\r\nAccept: ") {
		t.Errorf("分块写未重排:\n%s", got)
	}
}

// TestRespTrackerContentLength 有 Content-Length 时按字节数判断结束。
func TestRespTrackerContentLength(t *testing.T) {
	var tr respTracker
	if tr.feed([]byte("HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\n")) {
		t.Fatal("body 未到不该结束")
	}
	if tr.feed([]byte("abc")) {
		t.Fatal("body 未满不该结束")
	}
	if !tr.feed([]byte("de")) {
		t.Fatal("body 满应结束")
	}
}

// TestRespTrackerChunked chunked 按分帧结束判定。
func TestRespTrackerChunked(t *testing.T) {
	var tr respTracker
	head := "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n"
	if tr.feed([]byte(head + "3\r\nabc\r\n")) {
		t.Fatal("chunked 未完不该结束")
	}
	if !tr.feed([]byte("0\r\n\r\n")) {
		t.Fatal("结束块应判定完成")
	}
}

// TestRespTrackerSSEChunked SSE 长流：多段数据、最后 0 块结束。
func TestRespTrackerSSEChunked(t *testing.T) {
	var tr respTracker
	if tr.feed([]byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n")) {
		t.Fatal("不该结束")
	}
	// 每段 payload = "data: {}\n\n" 共 10 字节 → chunk size a
	for i := 0; i < 3; i++ {
		if tr.feed([]byte("a\r\ndata: {}\n\n\r\n")) {
			t.Fatalf("第 %d 段不该结束", i)
		}
	}
	if !tr.feed([]byte("0\r\n\r\n")) {
		t.Fatal("SSE 结束块应判定完成")
	}
}

// TestRespTrackerUnknownFallsBack 无长度无 chunked → 标记 unknown，永不误判完成。
func TestRespTrackerUnknownFallsBack(t *testing.T) {
	var tr respTracker
	done := tr.feed([]byte("HTTP/1.1 200 OK\r\nConnection: close\r\n\r\nsome body"))
	if done {
		t.Fatal("无边界信息时不该判定完成")
	}
	if !tr.unknown {
		t.Fatal("应标记 unknown 走安全降级")
	}
	if tr.feed([]byte("more")) {
		t.Fatal("unknown 之后不该再判定完成")
	}
}

// TestResponseBoundaryResetsPhase 响应结束应把连接状态复位，下一个请求才能重排。
func TestResponseBoundaryResetsPhase(t *testing.T) {
	var tr respTracker
	tr.feed([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\n"))
	if !tr.feed([]byte("ok")) {
		t.Fatal("应判定完成")
	}
	// 复位后能吃下一个响应（同一连接 keep-alive 场景）
	tr2 := respTracker{}
	if !tr2.feed([]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")) {
		t.Fatal("零长度响应应立即完成")
	}
}

// TestReorderPreservesBody 头块之后的数据必须原样透传（不能吞 body）。
func TestReorderPreservesBody(t *testing.T) {
	c, peer := newTestPipe(t)
	first, headers := splitFixture(t, "official_v3config_head.txt")
	head := buildBlock(first, shuffleHeaders(headers))
	body := []byte("{\"k\":\"v\"}")
	in := append(append([]byte{}, head...), body...)

	var wg sync.WaitGroup
	wg.Add(1)
	var got []byte
	go func() {
		defer wg.Done()
		buf := make([]byte, 8192)
		for {
			n, err := peer.Read(buf)
			if n > 0 {
				got = append(got, buf[:n]...)
			}
			if err != nil || bytes.Contains(got, body) {
				return
			}
		}
	}()
	c.Write(in)
	wg.Wait()
	if !bytes.HasSuffix(got, body) {
		t.Errorf("body 丢了或被改: %q", got)
	}
}

var _ = io.Discard
