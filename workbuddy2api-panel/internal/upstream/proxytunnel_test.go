// proxytunnel_test.go 代理隧道的回归测试。
//
// 要钉住的事只有一件：**代理开着的时候，TLS 指纹与头发送顺序不能丢**。
// 这曾经是静默失效——`http.Transport` 配了代理就自己 CONNECT + crypto/tls 握手，
// `DialTLSContext` 完全不参与，前面做的 JA3/JA4 与头序对齐全部白做。

package upstream

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// withProxy 临时替换出站代理解析（生产用 http.ProxyFromEnvironment，它会缓存 env，
// 测试里改环境变量读不到新值）。
func withProxy(t *testing.T, pu *url.URL) {
	t.Helper()
	old := outboundProxy
	outboundProxy = func(*http.Request) (*url.URL, error) { return pu, nil }
	t.Cleanup(func() { outboundProxy = old })
}

// startConnectProxy 起一个最小 HTTP CONNECT 代理：记录 CONNECT 目标，然后双向转发。
func startConnectProxy(t *testing.T) (string, func() []string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	var mu sync.Mutex
	var seen []string

	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				line, rerr := br.ReadString('\n')
				if rerr != nil {
					return
				}
				// 把请求头读完（CONNECT 没有 body）
				for {
					l, e := br.ReadString('\n')
					if e != nil || strings.TrimSpace(l) == "" {
						break
					}
				}
				fields := strings.Fields(strings.TrimSpace(line))
				if len(fields) < 2 || fields[0] != "CONNECT" {
					io.WriteString(c, "HTTP/1.1 400 Bad Request\r\n\r\n")
					return
				}
				target := fields[1]
				mu.Lock()
				seen = append(seen, target)
				mu.Unlock()

				up, derr := net.DialTimeout("tcp", target, 3*time.Second)
				if derr != nil {
					io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
					return
				}
				defer up.Close()
				io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
				go io.Copy(up, br) // 注意用 br：隧道里的字节可能已被它缓冲
				io.Copy(c, up)
			}(c)
		}
	}()

	return ln.Addr().String(), func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := make([]string, len(seen))
		copy(out, seen)
		return out
	}
}

// startSOCKS5Proxy 起一个最小 SOCKS5 代理（无认证），记录 CONNECT 目标。
func startSOCKS5Proxy(t *testing.T) (string, func() []string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("socks listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	var mu sync.Mutex
	var seen []string

	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				// ① 问候
				head := make([]byte, 2)
				if _, e := io.ReadFull(c, head); e != nil {
					return
				}
				methods := make([]byte, int(head[1]))
				if _, e := io.ReadFull(c, methods); e != nil {
					return
				}
				c.Write([]byte{0x05, 0x00}) // 选无认证
				// ② 请求：05 01 00 03 len host port
				req := make([]byte, 4)
				if _, e := io.ReadFull(c, req); e != nil || req[3] != 0x03 {
					return
				}
				hl := make([]byte, 1)
				if _, e := io.ReadFull(c, hl); e != nil {
					return
				}
				host := make([]byte, int(hl[0]))
				if _, e := io.ReadFull(c, host); e != nil {
					return
				}
				pb := make([]byte, 2)
				if _, e := io.ReadFull(c, pb); e != nil {
					return
				}
				target := fmt.Sprintf("%s:%d", host, int(pb[0])<<8|int(pb[1]))
				mu.Lock()
				seen = append(seen, target)
				mu.Unlock()

				up, derr := net.DialTimeout("tcp", target, 3*time.Second)
				if derr != nil {
					c.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
					return
				}
				defer up.Close()
				c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
				go io.Copy(up, c)
				io.Copy(c, up)
			}(c)
		}
	}()

	return ln.Addr().String(), func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := make([]string, len(seen))
		copy(out, seen)
		return out
	}
}

// 经 HTTP CONNECT 代理：流量确实过代理，且 ClientHello 仍是官方指纹。
func TestTunnelThroughHTTPConnectProxy(t *testing.T) {
	proxyAddr, seen := startConnectProxy(t)
	pu, _ := url.Parse("http://" + proxyAddr)
	withProxy(t, pu)

	d := newDialer()
	cfg := &tls.Config{InsecureSkipVerify: true, ServerName: "www.workbuddy.cn"}
	tlsDial := newTLSDialer(d, cfg, TLSFingerprintNode)

	raw := captureClientHello(t, func(addr string) (net.Conn, error) {
		// 握手必然失败（对端不回 ClientHello 之外的任何东西），错误忽略；
		// 但**不能**返回 (nil, nil) —— captureClientHello 会在 err==nil 时
		// conn.Close()，nil 直接 panic（这个坑第一次跑就踩到了）。
		conn, _ := tlsDial(context.Background(), "tcp", addr)
		if conn == nil {
			return nil, net.ErrClosed
		}
		return conn, nil
	})

	got := seen()
	if len(got) == 0 {
		t.Fatal("代理没有收到 CONNECT —— 请求根本没走代理（等于本机 IP 直接暴露）")
	}
	f := parseClientHello(t, raw)
	if ja3 := f.ja3(); ja3 != OfficialJA3 {
		t.Errorf("经代理后 JA3 跑偏\n  want: %s\n  got : %s", OfficialJA3, ja3)
	}
}

// 经 SOCKS5 代理：同上。SOCKS 分支更危险——Go 原生实现会用自己的 dialer，
// 曾出现过「配置了代理但整条请求从本机 IP 出网」。
func TestTunnelThroughSOCKS5Proxy(t *testing.T) {
	proxyAddr, seen := startSOCKS5Proxy(t)
	pu, _ := url.Parse("socks5://" + proxyAddr)
	withProxy(t, pu)

	d := newDialer()
	cfg := &tls.Config{InsecureSkipVerify: true, ServerName: "www.workbuddy.cn"}
	tlsDial := newTLSDialer(d, cfg, TLSFingerprintNode)

	raw := captureClientHello(t, func(addr string) (net.Conn, error) {
		// 握手必然失败（对端不回 ClientHello 之外的任何东西），错误忽略；
		// 但**不能**返回 (nil, nil) —— captureClientHello 会在 err==nil 时
		// conn.Close()，nil 直接 panic（这个坑第一次跑就踩到了）。
		conn, _ := tlsDial(context.Background(), "tcp", addr)
		if conn == nil {
			return nil, net.ErrClosed
		}
		return conn, nil
	})

	got := seen()
	if len(got) == 0 {
		t.Fatal("SOCKS5 代理没有收到 CONNECT —— 请求绕过代理直连了")
	}
	// 目标必须以**域名**形式发出（本机不做 DNS，避免泄露）
	if !strings.HasPrefix(got[0], "127.0.0.1:") {
		t.Errorf("SOCKS5 目标 = %q，本用例目标是 127.0.0.1（IP 形式由 ATYP 决定）", got[0])
	}
	f := parseClientHello(t, raw)
	if ja3 := f.ja3(); ja3 != OfficialJA3 {
		t.Errorf("经 SOCKS5 后 JA3 跑偏\n  want: %s\n  got : %s", OfficialJA3, ja3)
	}
}

// 不认识的 scheme 必须报错，不能静默直连（那等于暴露本机 IP）。
func TestTunnelRejectsUnknownScheme(t *testing.T) {
	pu, _ := url.Parse("ftp://127.0.0.1:1")
	withProxy(t, pu)
	d := newDialer()
	if _, err := dialOutbound(context.Background(), d, "example.com:443"); err == nil {
		t.Fatal("未知代理 scheme 应报错")
	}
}

// 无代理时行为不变（回归：接管代理不能把直连搞坏）。
func TestNoProxyStillDialsDirect(t *testing.T) {
	withProxy(t, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, aerr := ln.Accept()
		if aerr == nil {
			c.Close()
		}
	}()
	d := newDialer()
	conn, derr := dialOutbound(context.Background(), d, ln.Addr().String())
	if derr != nil {
		t.Fatalf("直连失败: %v", derr)
	}
	conn.Close()
}
