// connection_reuse_e2e_test.go 验证 Connection 头补发没有破坏连接复用。
//
// 补 Connection 头时最危险的副作用不是「多一个头」，而是：
//   - keep-alive 若被误写成 req.Close，Transport 会每次新建连接（性能回退 + 握手放大）
//   - close 若不生效，/v3/config 会占着一条本该释放的连接
//
// 两者都不会让任何断言失败，只会让线上慢下来，所以必须用真实 TLS 连接钉死。
package upstream

import (
	"bufio"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// reuseTestServer 起一个支持 keep-alive 多请求的自签 TLS 服务器，
// 返回地址、接受连接的累计次数、以及「读到 Connection: close 的请求数」。
func reuseTestServer(t *testing.T) (addr string, accepts, closeReqs *int64) {
	t.Helper()
	srvTLS := selfSignedServerTLS(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	tlsLn := tls.NewListener(ln, srvTLS)

	var n, cn int64
	accepts, closeReqs = &n, &cn
	go func() {
		for {
			conn, aerr := tlsLn.Accept()
			if aerr != nil {
				return
			}
			atomic.AddInt64(&n, 1)
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				for {
					// 读到空行为止（请求头结束）
					closed := false
					var closeSeen bool
					for {
						ln, rerr := br.ReadBytes('\n')
						if rerr != nil {
							closed = true
							break
						}
						if strings.HasPrefix(strings.ToLower(string(ln)), "connection:") &&
							strings.Contains(strings.ToLower(string(ln)), "close") {
							closeSeen = true
						}
						if string(ln) == "\r\n" {
							break
						}
					}
					if closed {
						return
					}
					if closeSeen {
						atomic.AddInt64(&cn, 1)
					}
					// Content-Length 明确，客户端才能判定响应边界并复用连接
					if _, werr := c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nContent-Type: application/json\r\n\r\n{}")); werr != nil {
						return
					}
					if closeSeen {
						return // 客户端要求 close：服务端配合断开
					}
				}
			}(conn)
		}
	}()
	return ln.Addr().String(), accepts, closeReqs
}

// TestConnectionHeaderKeepsConnReuse 验证 Connection 头的两种语义都落到了线上：
//
//	/report ×2  → keep-alive，同一条连接处理两个请求（复用没被破坏）
//	/v3/config  → close，占用现有连接但在响应后被关闭（不是另开一条）
//	/report     → 上一条已关，必须新建
//
// 断言「close 之后必然新建连接」而不是「close 时新建连接」：HTTP 语义里
// Connection: close 标记的是本条连接用完即弃，Transport 仍可先用空闲连接。
func TestConnectionHeaderKeepsConnReuse(t *testing.T) {
	addr, accepts, closeReqs := reuseTestServer(t)

	tr := newTransport()
	// newTransport 走生产 TLS 指纹（node 档），自签证书需要跳过校验；
	// 这里用 go 档避开 SNI 与自签的纠缠，测的是连接复用不是指纹。
	tr.DialTLSContext = newTLSDialer(newDialer(), &tls.Config{InsecureSkipVerify: true}, TLSFingerprintGo)
	tr.DisableKeepAlives = false
	tr.MaxIdleConnsPerHost = 4
	defer tr.CloseIdleConnections()

	httpClient := &http.Client{Transport: tr}
	c := &Client{HTTP: httpClient, ChatHTTP: httpClient}
	// auth 只在需要构造真实鉴权头的路径上用；本用例验的是连接复用，do() 不收
	// auth 参数，故这里不再构造（上游那版是 c.do(req, a, chat) 三参形态）。

	fire := func(path string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, "https://"+addr+path, strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("new request %s: %v", path, err)
		}
		req.Header.Set("Accept", "application/json")
		resp, derr := c.do(req) // 本仓 do() 只收 req
		if derr != nil {
			t.Fatalf("do %s: %v", path, derr)
		}
		// 必须把 body 读干净再 Close：net/http 对未读完的响应体不复用连接，
		// 那会让「连接没复用」的诊断失真（测的就不是 Connection 头了）。
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	fire("/v2/report")
	fire("/v2/report")
	// keep-alive 语义下 Transport 会把连接放回空闲池，但它是异步的；
	// 给一个极短的窗口，避免把「还没来得及复用」误判成「复用坏了」。
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt64(accepts); got != 1 {
		t.Fatalf("两个 keep-alive 请求应复用同一条连接，实际建立 %d 条", got)
	}

	fire("/v3/config")
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt64(closeReqs); got != 1 {
		t.Fatalf("/v3/config 必须在线上发出 Connection: close，服务端实际收到 %d 条", got)
	}
	if got := atomic.LoadInt64(accepts); got != 1 {
		t.Fatalf("close 请求应先复用空闲连接（语义是「用完即弃」而非「另开一条」），实际累计 %d 条", got)
	}

	// 上一条已被 close，这次必须重新建连
	fire("/v2/report")
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt64(accepts); got != 2 {
		t.Fatalf("close 之后的下一个请求必须新建连接，累计应为 2，实际 %d", got)
	}
}
