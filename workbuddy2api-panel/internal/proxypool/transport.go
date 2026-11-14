package proxypool

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

)

type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// TLSOverFunc 在已建好的隧道上做出站 TLS 握手（由 upstream 注入，见 SetTLSOver）。
type TLSOverFunc func(ctx context.Context, raw net.Conn, addr string) (net.Conn, error)

var errProxyAuth = errors.New("proxy authentication failed")

// SetTLSOver 注入「在隧道上握手」的函数：经代理的 HTTPS 与直连用同一套 TLS 指纹、请求头顺序与流量计量。
// 不注入时由 net/http 在隧道上做原生握手（仍然经代理，只是指纹是 Go 的）。已建的连接按新口径重建。
func (m *Manager) SetTLSOver(f TLSOverFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tlsOver = f
	for id, tr := range m.transports {
		tr.CloseIdleConnections()
		delete(m.transports, id)
	}
}

func (m *Manager) transportLocked(n node) (*http.Transport, error) {
	if tr := m.transports[n.ID]; tr != nil {
		return tr, nil
	}
	u, err := parseURL(n.URL)
	if err != nil {
		return nil, err
	}
	tr := m.base.Clone()
	tr.Proxy = nil
	// 基础传输层的 DialTLSContext 是直连的（upstream 的 TLS 指纹拨号），不能沿用：SOCKS 代理的 HTTPS 会
	// 绕过代理直接出网；HTTP 代理的 HTTPS 会被 net/http 改成它自己的 CONNECT + 原生握手，指纹白做。
	// HTTPS 目标一律「经代理建隧道 → 在隧道上握手」（handshake），明文 HTTP 目标照旧走标准代理转发。
	tlsBase := m.base.TLSClientConfig
	over := m.tlsOver
	nativeTLS := func(ctx context.Context, raw net.Conn, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		cfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if tlsBase != nil {
			cfg = tlsBase.Clone()
		}
		cfg.ServerName, cfg.NextProtos = host, nil
		tc := tls.Client(raw, cfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			raw.Close()
			return nil, err
		}
		return tc, nil
	}
	handshake := nativeTLS
	if over != nil {
		handshake = over
	}
	if u.Scheme == "http" || u.Scheme == "https" {
		connect := m.connectTunnel(u, tlsBase)
		// HTTPS 目标不交给 net/http 的代理逻辑（它会用原生握手）；明文 HTTP 目标用标准的 absolute-form
		// 转发——不少商用代理只放行 CONNECT 到 443。
		tr.Proxy = func(r *http.Request) (*url.URL, error) {
			if r.URL.Scheme == "https" {
				return nil, nil
			}
			return u, nil
		}
		// 明文目标连代理本身沿用基础传输层的 DialContext（带流量计量）。
		tr.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr == u.Host {
				// https 代理承载明文 HTTP 目标：net/http 要的是到代理本身的 TLS，不是隧道。
				raw, err := m.dialer.DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				return nativeTLS(ctx, raw, addr)
			}
			raw, err := connect(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return handshake(ctx, raw, addr)
		}
	} else {
		var auth *socksAuth
		if u.User != nil {
			password, _ := u.User.Password()
			auth = &socksAuth{User: u.User.Username(), Password: password}
		}
		// 本仓自带的 SOCKS5 拨号器（见 socks5.go）：不引 golang.org/x/net/proxy，
		// 构建环境拉不到 Go 模块。语义与上游等价（CONNECT + RFC 1929 认证）。
		dialer, err := newSocks5Dialer(u.Host, auth, m.dialer)
		if err != nil {
			return nil, errors.New("create SOCKS dialer failed")
		}
		tunnel := func(ctx context.Context, network, address string) (net.Conn, error) {
			if m.dialer.Timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, m.dialer.Timeout)
				defer cancel()
			}
			if u.Scheme == "socks5" {
				host, port, err := net.SplitHostPort(address)
				if err != nil {
					return nil, err
				}
				if net.ParseIP(host) == nil {
					resolver := m.dialer.Resolver
					if resolver == nil {
						resolver = net.DefaultResolver
					}
					ips, err := resolver.LookupIPAddr(ctx, host)
					if err != nil {
						return nil, err
					}
					if len(ips) == 0 {
						return nil, errors.New("DNS returned no addresses")
					}
					address = net.JoinHostPort(ips[0].IP.String(), port)
				}
			}
			return dialer.DialContext(ctx, network, address)
		}
		tr.DialContext = tunnel
		tr.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			raw, err := tunnel(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return handshake(ctx, raw, addr) // 握手用原始主机名（socks5 在隧道里已就地解析成 IP）
		}
	}
	m.transports[n.ID] = tr
	return tr, nil
}

// connectTunnel 经 HTTP / HTTPS 代理用 CONNECT 建到目标的 TCP 隧道（替代 Transport.Proxy，好在隧道上
// 用自己的 TLS 握手）。407 返回 errProxyAuth；握手阶段受 ctx 取消与截止时间约束。
func (m *Manager) connectTunnel(u *url.URL, tlsBase *tls.Config) dialFunc {
	auth := ""
	if u.User != nil {
		password, _ := u.User.Password()
		auth = base64.StdEncoding.EncodeToString([]byte(u.User.Username() + ":" + password))
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := m.dialer.DialContext(ctx, "tcp", u.Host)
		if err != nil {
			return nil, err
		}
		stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Unix(1, 0)) })
		defer stop()
		// 建隧道最多等拨号超时：请求级截止时间可能长达整个回复（120s），只认它的话，
		// 接了 TCP 却不回 CONNECT 的代理能把请求挂满整段。
		var dl time.Time
		if m.dialer.Timeout > 0 {
			dl = time.Now().Add(m.dialer.Timeout)
		}
		if cd, ok := ctx.Deadline(); ok && (dl.IsZero() || cd.Before(dl)) {
			dl = cd
		}
		conn.SetDeadline(dl)
		if u.Scheme == "https" {
			cfg := &tls.Config{MinVersion: tls.VersionTLS12}
			if tlsBase != nil {
				cfg = tlsBase.Clone() // 与到代理本身的 TLS 同一份配置（证书校验口径一致）
			}
			cfg.ServerName, cfg.NextProtos = u.Hostname(), nil
			tc := tls.Client(conn, cfg)
			if err := tc.HandshakeContext(ctx); err != nil {
				conn.Close()
				return nil, err
			}
			conn = tc
		}
		req := "CONNECT " + addr + " HTTP/1.1\r\nHost: " + addr + "\r\n"
		if auth != "" {
			req += "Proxy-Authorization: Basic " + auth + "\r\n"
		}
		if _, err := io.WriteString(conn, req+"\r\n"); err != nil {
			conn.Close()
			return nil, err
		}
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
		if err != nil {
			conn.Close()
			return nil, err
		}
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusProxyAuthRequired:
			conn.Close()
			return nil, errProxyAuth
		case resp.StatusCode != http.StatusOK:
			conn.Close()
			return nil, fmt.Errorf("proxy CONNECT: %s", resp.Status)
		}
		if !stop() { // ctx 已取消：AfterFunc 已经或即将把截止时间设成过去
			conn.Close()
			return nil, ctx.Err()
		}
		conn.SetDeadline(time.Time{})
		if br.Buffered() > 0 {
			return &bufferedConn{Conn: conn, r: br}, nil
		}
		return conn, nil
	}
}

// bufferedConn 隧道建好时代理已多发了字节（读进了 bufio）：先吐完缓冲再读连接。
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// Do selects once before Client.Do so redirects and the response body keep the route.
// The caller's Client is copied; shared clients and live transports are never mutated.
func (m *Manager) Do(client *http.Client, req *http.Request, uid, realm string) (*http.Response, error) {
	m.mu.Lock()
	n, proxied, err := m.selectLocked(uid, realm)
	var tr *http.Transport
	if err == nil && proxied {
		tr, err = m.transportLocked(n)
	}
	m.mu.Unlock()
	if err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	if !proxied {
		return client.Do(req)
	}
	selected := *client
	selected.Transport = tr
	resp, err := selected.Do(req)
	if err != nil {
		reason := "proxy request failed"
		if errors.Is(err, context.Canceled) {
			reason = "request canceled"
		} else if errors.Is(err, context.DeadlineExceeded) {
			reason = "request timed out"
		} else if errors.Is(err, errProxyAuth) {
			reason = "proxy authentication failed"
		}
		if !errors.Is(err, context.Canceled) {
			// 调用方自己取消不说明代理有问题，不清节点上别的请求能复用的空闲连接。
			tr.CloseIdleConnections()
		}
		return nil, &Error{ProxyID: n.ID, Reason: reason, cause: err}
	}
	if resp.StatusCode == http.StatusProxyAuthRequired {
		_ = resp.Body.Close()
		return nil, &Error{ProxyID: n.ID, Reason: "proxy authentication failed"}
	}
	resp.Body = &proxyBody{ReadCloser: resp.Body, id: n.ID, transport: tr}
	return resp, nil
}

type proxyBody struct {
	io.ReadCloser
	id        string
	transport *http.Transport
	closed    atomic.Bool
}

// Read 读响应体时出了网络错误，认为经这个节点的连接可能都坏了，清掉节点的空闲连接。
// 调用方自己造成的错误不算：取消了请求，或 Close 之后又读（net/http 回「read on closed response
// body」，是未导出的错误值，所以靠自己记下是否已关闭）。误清一次就白白丢掉后面请求能复用的连接。
func (b *proxyBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && err != io.EOF {
		if b.closed.Load() || errors.Is(err, context.Canceled) {
			return n, err
		}
		b.transport.CloseIdleConnections()
		err = &Error{ProxyID: b.id, Reason: "proxy response interrupted", cause: err}
	}
	return n, err
}

func (b *proxyBody) Close() error {
	b.closed.Store(true)
	return b.ReadCloser.Close()
}

// Probe uses a separate total deadline and never carries account headers or cookies.
func (m *Manager) Probe(ctx context.Context, id string) error {
	select {
	case m.slots <- struct{}{}:
		defer func() { <-m.slots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return errors.New("proxy pool is closed")
	}
	if m.checking[id] {
		m.mu.Unlock()
		return nil
	}
	var found *node
	for _, n := range m.data.Nodes {
		if n.ID == id {
			copy := n
			found = &copy
			break
		}
	}
	if found == nil {
		m.mu.Unlock()
		return ErrNotFound
	}
	tr, err := m.transportLocked(*found)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	version, settings := m.generation[id], m.data.Settings
	lookupExit := m.exitDueLocked(id, time.Now())
	m.checking[id] = true
	delete(m.requested, id)
	h := m.health[id]
	h.attemptedAt = time.Now()
	m.health[id] = h
	m.mu.Unlock()

	checkCtx, cancel := context.WithTimeout(ctx, time.Duration(settings.TimeoutSeconds)*time.Second)
	defer cancel()
	start := time.Now()
	req, _ := http.NewRequestWithContext(checkCtx, http.MethodGet, settings.URL, nil)
	client := &http.Client{Transport: tr}
	resp, checkErr := client.Do(req)
	message := ""
	if checkErr != nil {
		message = "proxy connection failed"
		if errors.Is(checkCtx.Err(), context.DeadlineExceeded) {
			message = "health check timed out"
		}
	} else {
		_ = resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 400 {
			message = "health endpoint returned HTTP " + strconv.Itoa(resp.StatusCode)
		}
	}
	// 出口画像顺手刷新（过期才查，见 intel.go）。放在测速之后，且失败不影响健康判定：
	// 一个查不到归属的代理照样能跑流量。用父 ctx 而不是 checkCtx——探测已经快把
	// 那个超时用完了。
	elapsed := time.Since(start)
	// 出口画像顺手刷新（过期才查，见 intel.go）：还没打开、或这次探测本身就失败，都不查。
	var intel ExitInfo
	if lookupExit && message == "" {
		intel = fetchExitInfo(ctx, tr) // 经该节点自己的 transport：查到的才是这个节点的出口
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.checking, id)
	if ctx.Err() != nil || m.closed || m.generation[id] != version {
		m.signal()
		return nil
	}
	m.setExitLocked(id, intel)
	h = m.health[id]
	h.checkedAt, h.latencyMS, h.lastError = time.Now(), elapsed.Milliseconds(), message
	if message == "" {
		h.failures = 0
		h.successes++
		if h.status != "unhealthy" || h.successes >= settings.SuccessThreshold {
			h.status = "healthy"
		}
	} else {
		h.successes = 0
		h.failures++
		if h.failures >= settings.FailureThreshold {
			h.status = "unhealthy"
			tr.CloseIdleConnections()
		}
	}
	m.health[id] = h
	return nil
}

// CheckAll shares the global eight-probe bound with the periodic checker.
func (m *Manager) CheckAll(ctx context.Context) {
	s := m.Snapshot()
	jobs := make(chan string)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				_ = m.Probe(ctx, id)
			}
		}()
	}
	for _, n := range s.Proxies {
		select {
		case jobs <- n.ID:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return
		}
	}
	close(jobs)
	wg.Wait()
}

// Run is blocking and must finish before Close. Disabled-by-health nodes remain due.
func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	jobs := make(chan string)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				_ = m.Probe(ctx, id)
			}
		}()
	}
	defer func() { close(jobs); wg.Wait() }()
	for {
		m.mu.Lock()
		var due []string
		for _, n := range m.data.Nodes {
			if !m.checking[n.ID] && (m.requested[n.ID] || (n.Enabled && time.Since(m.health[n.ID].attemptedAt) >= time.Duration(m.data.Settings.IntervalSeconds)*time.Second)) {
				due = append(due, n.ID)
			}
		}
		m.mu.Unlock()
		for _, id := range due {
			select {
			case jobs <- id:
			case <-ctx.Done():
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-m.wake:
		}
	}
}
