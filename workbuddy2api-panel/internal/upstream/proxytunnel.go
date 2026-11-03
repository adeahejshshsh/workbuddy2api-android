// proxytunnel.go 经代理建立到目标的**原始 TCP 隧道**，让 TLS 握手仍由我们自己做。
//
// 为什么必须自己建隧道：`http.Transport` 只要配了代理，HTTPS 目标就由它自己
// CONNECT + `crypto/tls` 握手 —— 那时 `DialTLSContext` 根本不参与，于是
//
//	· TLS 指纹（JA3/JA4）退回 Go 原生形态
//	· headOrderConn 不在链路上，头发送顺序退回 Go 的 key 排序
//
// 也就是说：**代理开着，前面十几轮做的指纹对齐全部失效**。SOCKS 下更糟，Go 用它
// 自己的 socks dialer，同样绕开我们。
//
// 所以这里把代理接管过来：先把隧道建好，再把「裸连接」交回 newTLSDialer 做指纹握手。
//
// 支持 scheme：http / https（CONNECT，后者先与代理握 TLS）、socks5 / socks5h
// （CONNECT，恒以域名形式发目标，让代理解析 —— 避免本机 DNS 泄露）。
package upstream

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// outboundProxy 解析目标该走的代理。包级变量是为了让测试注入 —— 生产恒为
// http.ProxyFromEnvironment（它内部缓存 env，测试里改环境变量后再调它读不到新值）。
var outboundProxy = http.ProxyFromEnvironment

// proxyForAddr 解析该 addr（host:port）应走的代理；nil = 直连。
//
// 直接复用 net/http 的 env 语义（HTTPS_PROXY / ALL_PROXY / NO_PROXY 及其小写变体），
// 不自己重写一遍 —— 那套匹配规则（域名后缀、CIDR、通配）比看起来复杂得多。
func proxyForAddr(addr string) (*url.URL, error) {
	req, err := http.NewRequest(http.MethodGet, "https://"+addr, nil)
	if err != nil {
		return nil, err
	}
	return outboundProxy(req)
}

// dialOutbound 建立到 addr 的裸 TCP 连接：有代理走隧道，无代理直连。
func dialOutbound(ctx context.Context, d *net.Dialer, addr string) (net.Conn, error) {
	pu, err := proxyForAddr(addr)
	if err != nil || pu == nil {
		return d.DialContext(ctx, "tcp", addr)
	}
	conn, err := dialTunnel(ctx, d, pu, addr)
	if err != nil {
		return nil, fmt.Errorf("proxy %s → %s: %w", pu.Redacted(), addr, err)
	}
	return conn, nil
}

// dialTunnel 按代理 scheme 分发到具体实现。
func dialTunnel(ctx context.Context, d *net.Dialer, pu *url.URL, target string) (net.Conn, error) {
	switch strings.ToLower(pu.Scheme) {
	case "http", "https":
		return dialHTTPConnect(ctx, d, pu, target)
	case "socks5", "socks5h":
		return dialSOCKS5(ctx, d, pu, target)
	default:
		// 不认识的 scheme 直接报错而不是静默直连 —— 静默直连等于把本机 IP 暴露给上游。
		return nil, fmt.Errorf("不支持的代理 scheme %q（支持 http/https/socks5/socks5h）", pu.Scheme)
	}
}

// proxyAddr 代理自身的 host:port（缺省端口按 scheme 补）。
func proxyAddr(pu *url.URL) string {
	if pu.Port() != "" {
		return pu.Host
	}
	port := "80"
	switch strings.ToLower(pu.Scheme) {
	case "https":
		port = "443"
	case "socks5", "socks5h":
		port = "1080"
	}
	return net.JoinHostPort(pu.Hostname(), port)
}

// dialHTTPConnect 经 HTTP(S) 代理建隧道：dial 代理 →（https 代理先握 TLS）→ CONNECT。
func dialHTTPConnect(ctx context.Context, d *net.Dialer, pu *url.URL, target string) (net.Conn, error) {
	conn, err := d.DialContext(ctx, "tcp", proxyAddr(pu))
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(pu.Scheme, "https") {
		tc := tlsClientTo(conn, pu.Hostname())
		if herr := tc.HandshakeContext(ctx); herr != nil {
			conn.Close()
			return nil, fmt.Errorf("与 https 代理握手失败: %w", herr)
		}
		conn = tc
	}

	var b strings.Builder
	b.WriteString("CONNECT " + target + " HTTP/1.1\r\n")
	b.WriteString("Host: " + target + "\r\n")
	if u := pu.User; u != nil {
		pass, _ := u.Password()
		cred := base64.StdEncoding.EncodeToString([]byte(u.Username() + ":" + pass))
		b.WriteString("Proxy-Authorization: Basic " + cred + "\r\n")
	}
	b.WriteString("\r\n")
	if _, werr := io.WriteString(conn, b.String()); werr != nil {
		conn.Close()
		return nil, werr
	}

	br := bufio.NewReader(conn)
	resp, rerr := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if rerr != nil {
		conn.Close()
		return nil, fmt.Errorf("读 CONNECT 应答失败: %w", rerr)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		if resp.StatusCode == http.StatusProxyAuthRequired {
			return nil, fmt.Errorf("代理要求认证（407）：检查 %s 里的用户名密码", pu.Scheme)
		}
		return nil, fmt.Errorf("CONNECT 被代理拒绝：%s", resp.Status)
	}
	// bufio 可能已经把隧道里的首批字节读进来了，必须带着它返回，否则丢数据。
	return &bufferedConn{Conn: conn, r: br}, nil
}

// bufferedConn 让「已读进 bufio 的字节」不丢：CONNECT 之后对端可能立刻发数据。
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// dialSOCKS5 经 SOCKS5 代理建隧道（RFC 1928 + RFC 1929 用户名/密码认证）。
//
// 目标地址**恒以域名形式**发送（ATYP=0x03），让代理去解析 —— 本机不做 DNS，
// 既避免泄露、也让 socks5h 的语义对所有 socks5 代理一致生效。
func dialSOCKS5(ctx context.Context, d *net.Dialer, pu *url.URL, target string) (net.Conn, error) {
	conn, err := d.DialContext(ctx, "tcp", proxyAddr(pu))
	if err != nil {
		return nil, err
	}
	fail := func(e error) (net.Conn, error) {
		conn.Close()
		return nil, e
	}

	// ① 问候：无认证恒发；带账号时再带上用户名/密码法。
	methods := []byte{0x00}
	useAuth := pu.User != nil
	if useAuth {
		methods = append(methods, 0x02)
	}
	greet := append([]byte{0x05, byte(len(methods))}, methods...)
	if _, werr := conn.Write(greet); werr != nil {
		return fail(werr)
	}
	sel := make([]byte, 2)
	if _, rerr := io.ReadFull(conn, sel); rerr != nil {
		return fail(rerr)
	}
	if sel[0] != 0x05 {
		return fail(fmt.Errorf("不是 SOCKS5 代理（版本字节 0x%02x）", sel[0]))
	}
	switch sel[1] {
	case 0x00:
		// 无认证
	case 0x02:
		if !useAuth {
			return fail(fmt.Errorf("代理要求用户名/密码，但配置里没有"))
		}
		u := pu.User.Username()
		p, _ := pu.User.Password()
		if len(u) > 255 || len(p) > 255 {
			return fail(fmt.Errorf("SOCKS5 用户名/密码超过 255 字节"))
		}
		auth := append([]byte{0x01, byte(len(u))}, u...)
		auth = append(auth, byte(len(p)))
		auth = append(auth, p...)
		if _, werr := conn.Write(auth); werr != nil {
			return fail(werr)
		}
		ar := make([]byte, 2)
		if _, rerr := io.ReadFull(conn, ar); rerr != nil {
			return fail(rerr)
		}
		if ar[1] != 0x00 {
			return fail(fmt.Errorf("SOCKS5 认证失败"))
		}
	case 0xff:
		return fail(fmt.Errorf("SOCKS5 代理不接受任何可用认证方式"))
	default:
		return fail(fmt.Errorf("SOCKS5 代理选了未知认证方式 0x%02x", sel[1]))
	}

	// ② 连接请求：05 01 00 03 <域名长度> <域名> <端口>
	host, portStr, splitErr := net.SplitHostPort(target)
	if splitErr != nil {
		return fail(fmt.Errorf("目标地址不合法 %q: %w", target, splitErr))
	}
	port, perr := strconv.Atoi(portStr)
	if perr != nil || port <= 0 || port > 65535 {
		return fail(fmt.Errorf("目标端口不合法 %q", portStr))
	}
	if len(host) > 255 {
		return fail(fmt.Errorf("目标域名超过 255 字节"))
	}
	req := append([]byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}, host...)
	req = append(req, byte(port>>8), byte(port&0xff))
	if _, werr := conn.Write(req); werr != nil {
		return fail(werr)
	}

	// ③ 应答：05 <REP> 00 <ATYP> <BND.ADDR> <BND.PORT>
	head := make([]byte, 4)
	if _, rerr := io.ReadFull(conn, head); rerr != nil {
		return fail(rerr)
	}
	if head[0] != 0x05 {
		return fail(fmt.Errorf("SOCKS5 应答版本异常 0x%02x", head[0]))
	}
	if head[1] != 0x00 {
		return fail(fmt.Errorf("SOCKS5 连接被拒：%s", socks5ReplyText(head[1])))
	}
	// 把 BND.ADDR/BND.PORT 读掉，否则它们会混进隧道数据。
	var skip int
	switch head[3] {
	case 0x01: // IPv4
		skip = 4 + 2
	case 0x03: // 域名
		l := make([]byte, 1)
		if _, rerr := io.ReadFull(conn, l); rerr != nil {
			return fail(rerr)
		}
		skip = int(l[0]) + 2
	case 0x04: // IPv6
		skip = 16 + 2
	default:
		return fail(fmt.Errorf("SOCKS5 应答地址类型未知 0x%02x", head[3]))
	}
	if _, rerr := io.ReadFull(conn, make([]byte, skip)); rerr != nil {
		return fail(rerr)
	}
	return conn, nil
}

// socks5ReplyText REP 码的可读说明（RFC 1928 §6）。
func socks5ReplyText(rep byte) string {
	switch rep {
	case 0x01:
		return "一般性失败"
	case 0x02:
		return "规则不允许"
	case 0x03:
		return "网络不可达"
	case 0x04:
		return "主机不可达"
	case 0x05:
		return "连接被拒"
	case 0x06:
		return "TTL 过期"
	case 0x07:
		return "不支持的命令"
	case 0x08:
		return "不支持的地址类型"
	}
	return fmt.Sprintf("未知错误码 0x%02x", rep)
}

// tlsClientTo 与「https 代理」握手用的客户端（只保护到代理这一段；隧道内的目标
// TLS 由 newTLSDialer 用指纹档位另做一次）。
func tlsClientTo(conn net.Conn, serverName string) *tls.Conn {
	return tls.Client(conn, &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12})
}
