package proxypool

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// connectProxy 最小的 CONNECT 代理测试替身：校验 Basic 认证，记录 CONNECT 目标并打通隧道。
func connectProxy(t *testing.T, overTLS bool) (*httptest.Server, <-chan string) {
	t.Helper()
	seen := make(chan string, 8)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "Basic dXNlcjpzZWNyZXQ=" {
			w.WriteHeader(http.StatusProxyAuthRequired)
			return
		}
		if r.Method != http.MethodConnect {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		seen <- r.Host
		up, err := net.DialTimeout("tcp", r.Host, time.Second)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		down, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			_ = up.Close()
			return
		}
		_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = rw.Flush()
		tunnel(down, up)
	}))
	if overTLS {
		srv.StartTLS()
	} else {
		srv.Start()
	}
	t.Cleanup(srv.Close)
	return srv, seen
}

// 经代理的 HTTPS：不能调用基础传输层的直连 TLS 拨号（SOCKS 会绕过代理直接出网，HTTP 代理会让指纹白做），
// 要在经代理建好的隧道上调用注入的握手函数，地址是目标主机而不是代理。
func TestProxiedHTTPSUsesTunnelAndInjectedHandshake(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer target.Close()
	targetURL, _ := url.Parse(target.URL)

	for _, scheme := range []string{"http", "https", "socks5", "socks5h"} {
		t.Run(scheme, func(t *testing.T) {
			var proxyURL string
			var seen <-chan string
			if strings.HasPrefix(scheme, "http") {
				srv, s := connectProxy(t, scheme == "https")
				proxyURL, seen = strings.Replace(srv.URL, "://", "://user:secret@", 1), s
			} else {
				addr, s := socksFixture(t, targetURL.Host)
				proxyURL, seen = scheme+"://user:secret@"+addr, s
			}
			base := http.DefaultTransport.(*http.Transport).Clone()
			base.Proxy = nil
			base.DisableKeepAlives = true                                // 每个请求都重新建连，下面才能逐个请求断言经过了代理
			base.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // https 代理的本地自签证书
			direct := func(context.Context, string, string) (net.Conn, error) {
				t.Error("基础传输层的直连拨号被调用：经代理的 HTTPS 绕过了代理")
				return nil, errors.New("direct dial")
			}
			base.DialContext, base.DialTLSContext = direct, direct
			m, err := New("", base, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			var mu sync.Mutex
			var handshakes []string
			m.SetTLSOver(func(ctx context.Context, raw net.Conn, addr string) (net.Conn, error) {
				mu.Lock()
				handshakes = append(handshakes, addr)
				mu.Unlock()
				tc := tls.Client(raw, &tls.Config{InsecureSkipVerify: true}) // 本地自签证书
				if err := tc.HandshakeContext(ctx); err != nil {
					raw.Close()
					return nil, err
				}
				return tc, nil
			})
			settings := DefaultSettings()
			settings.URL = target.URL + "/"
			if err := m.Configure(settings); err != nil {
				t.Fatal(err)
			}
			id := addProxy(t, m, proxyURL)
			probe(t, m, id)
			if st := m.Snapshot().Proxies[0]; st.Status != "healthy" {
				t.Fatalf("probe through %s: %s %s", scheme, st.Status, st.LastError)
			}
			<-seen // 探测那次的隧道
			mu.Lock()
			handshakes = nil
			mu.Unlock()
			req, _ := http.NewRequest("GET", target.URL, nil)
			resp, err := m.Do(&http.Client{Timeout: 2 * time.Second}, req, "u", "cn")
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if string(body) != "ok" {
				t.Fatalf("body=%q", body)
			}
			select {
			case <-seen:
			default:
				t.Fatal("代理没有看到到目标的隧道请求")
			}
			mu.Lock()
			defer mu.Unlock()
			if len(handshakes) == 0 || handshakes[0] != targetURL.Host {
				t.Fatalf("注入的握手应在隧道上对目标 %s 调用，实际 %v", targetURL.Host, handshakes)
			}
		})
	}
}

// CONNECT 被拒（407）时如实报代理认证失败，而不是笼统的请求失败。
func TestConnectProxyAuthFailureReason(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer target.Close()
	srv, _ := connectProxy(t, false)
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	base.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	m, err := New("", base, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	settings := DefaultSettings()
	settings.URL = target.URL + "/"
	if err := m.Configure(settings); err != nil {
		t.Fatal(err)
	}
	id := addProxy(t, m, strings.Replace(srv.URL, "://", "://user:secret@", 1))
	probe(t, m, id)
	if err := m.Update(id, Input{Password: "WRONG"}); err != nil {
		t.Fatal(err)
	}
	// 改密码会重置健康状态：直接拿传输层发请求，验证 CONNECT 的 407 被识别。
	m.mu.Lock()
	tr, err := m.transportLocked(m.data.Nodes[0])
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", target.URL, nil)
	_, err = (&http.Client{Transport: tr, Timeout: 2 * time.Second}).Do(req)
	if !errors.Is(err, errProxyAuth) {
		t.Fatalf("CONNECT 407 应识别为代理认证失败，得到 %v", err)
	}
}
