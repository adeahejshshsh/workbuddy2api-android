package proxypool

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
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

func tunnel(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(a, b); _ = a.Close() }()
	go func() { defer wg.Done(); _, _ = io.Copy(b, a); _ = b.Close() }()
	wg.Wait()
}

func TestHTTPAndHTTPSProxyAuthenticationAndCONNECT(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" {
			t.Error("proxy credentials leaked to origin")
		}
		_, _ = io.WriteString(w, "through tunnel")
	}))
	defer target.Close()
	for _, scheme := range []string{"http", "https"} {
		t.Run(scheme, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Proxy-Authorization") != "Basic dXNlcjpzZWNyZXQ=" {
					w.WriteHeader(407)
					return
				}
				if r.Method != "CONNECT" {
					// Probe uses an HTTP URL; this also exercises absolute-form requests.
					if r.URL.Host != "health.invalid" {
						t.Errorf("proxy request target=%s", r.URL)
					}
					w.WriteHeader(204)
					return
				}
				up, err := net.DialTimeout("tcp", r.Host, time.Second)
				if err != nil {
					w.WriteHeader(502)
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
			})
			var proxy *httptest.Server
			if scheme == "https" {
				proxy = httptest.NewTLSServer(handler)
			} else {
				proxy = httptest.NewServer(handler)
			}
			defer proxy.Close()
			base := http.DefaultTransport.(*http.Transport).Clone()
			base.Proxy = nil
			base.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // Local TLS fixtures only.
			m, err := New("", base, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			settings := DefaultSettings()
			settings.URL = "http://health.invalid/"
			if err := m.Configure(settings); err != nil {
				t.Fatal(err)
			}
			id := addProxy(t, m, strings.Replace(proxy.URL, "://", "://user:secret@", 1))
			probe(t, m, id)
			req, _ := http.NewRequest("GET", target.URL, nil)
			resp, err := m.Do(&http.Client{Timeout: 2 * time.Second}, req, "account", "cn")
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if string(body) != "through tunnel" {
				t.Fatalf("body=%q", body)
			}
			if err := m.Update(id, Input{Password: "WRONG"}); err != nil {
				t.Fatal(err)
			}
			probe(t, m, id)
			if m.Snapshot().Proxies[0].Status == "healthy" {
				t.Fatal("407 accepted as health success")
			}
		})
	}
}

// socksFixture is a small protocol test peer, not application proxy logic.
func socksFixture(t *testing.T, target string) (string, <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	seen := make(chan string, 64)
	var wg sync.WaitGroup
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(3 * time.Second))
				br := bufio.NewReader(c)
				var greeting [2]byte
				if _, err := io.ReadFull(br, greeting[:]); err != nil {
					return
				}
				if _, err := io.CopyN(io.Discard, br, int64(greeting[1])); err != nil {
					return
				}
				_, _ = c.Write([]byte{5, 2})
				var authHead [2]byte
				if _, err := io.ReadFull(br, authHead[:]); err != nil {
					return
				}
				username := make([]byte, authHead[1])
				_, _ = io.ReadFull(br, username)
				passwordLen, err := br.ReadByte()
				if err != nil {
					return
				}
				password := make([]byte, passwordLen)
				_, _ = io.ReadFull(br, password)
				if string(username) != "user" || string(password) != "secret" {
					_, _ = c.Write([]byte{1, 1})
					return
				}
				_, _ = c.Write([]byte{1, 0})
				var head [4]byte
				if _, err := io.ReadFull(br, head[:]); err != nil {
					return
				}
				length := 0
				switch head[3] {
				case 1:
					length = 4
				case 4:
					length = 16
				case 3:
					n, _ := br.ReadByte()
					length = int(n)
				default:
					return
				}
				address := make([]byte, length)
				if _, err := io.ReadFull(br, address); err != nil {
					return
				}
				var port [2]byte
				_, _ = io.ReadFull(br, port[:])
				host := string(address)
				if head[3] != 3 {
					host = net.IP(address).String()
				}
				seen <- fmt.Sprintf("%d:%s:%d", head[3], host, binary.BigEndian.Uint16(port[:]))
				up, err := net.DialTimeout("tcp", target, time.Second)
				if err != nil {
					return
				}
				_, _ = c.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
				_ = c.SetDeadline(time.Time{})
				tunnel(c, up)
			}()
		}
	}()
	t.Cleanup(func() { _ = ln.Close(); <-done; wg.Wait() })
	return ln.Addr().String(), seen
}

func TestSOCKSAuthenticationAndDNSModes(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "socks") }))
	defer target.Close()
	targetURL, _ := url.Parse(target.URL)
	for _, scheme := range []string{"socks5", "socks5h"} {
		t.Run(scheme, func(t *testing.T) {
			addr, seen := socksFixture(t, targetURL.Host)
			base := http.DefaultTransport.(*http.Transport).Clone()
			base.Proxy = nil
			base.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // Local TLS fixture.
			m, err := New("", base, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			host := "localhost"
			if scheme == "socks5h" {
				host = "remote-only.invalid"
			}
			settings := DefaultSettings()
			settings.URL = "https://" + net.JoinHostPort(host, targetURL.Port()) + "/"
			if err := m.Configure(settings); err != nil {
				t.Fatal(err)
			}
			id := addProxy(t, m, scheme+"://user:secret@"+addr)
			probe(t, m, id)
			select {
			case observed := <-seen:
				if scheme == "socks5h" && !strings.HasPrefix(observed, "3:remote-only.invalid:") {
					t.Fatalf("remote DNS=%q", observed)
				}
				if scheme == "socks5" && !strings.HasPrefix(observed, "1:") && !strings.HasPrefix(observed, "4:") {
					t.Fatalf("local DNS did not send an IP: %q", observed)
				}
			case <-time.After(time.Second):
				t.Fatal("SOCKS peer saw no target")
			}
			req, _ := http.NewRequestWithContext(context.Background(), "GET", settings.URL, nil)
			resp, err := m.Do(&http.Client{Timeout: time.Second}, req, "u", "cn")
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if string(body) != "socks" {
				t.Fatalf("response=%q", body)
			}
		})
	}
}
