package upstream

// T-3 / 审查 F-12：SetDNSServers 是**全站出站行为**的配置面。
//
// cmd/server/main.go 从 config 的 upstream.dns_servers 注入，newDialer() 读它给
// net.Dialer 挂自定义 Resolver —— 也就是说这一行配置写错，不是「某个功能降级」，
// 而是**所有上游请求的域名解析全部失败**。此前全仓零用例（111 个测试文件里
// SetDNSServers 零命中），配置面炸全站而套件全绿。
//
// 本文件守三条不变量：
//  1. 空串/纯空白项不得进入生效列表。否则会挂上一个「永远连不上」的 Resolver，
//     系统默认解析被顶掉 → 全站 DNS 挂（运维留空是最常见的写法）。
//  2. 裸 IP 必须自动补 :53。少补端口 → DialContext 直接报 "missing port in
//     address"，一个号都刷不出来。
//  3. 配置的服务器必须**真的**被用来解析，不是只存进 atomic 就完事（存了不接线
//     同样表现为「配了没用」，排查方向完全相反）。

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// withDNS 注入 dns_servers 并在用例结束后还原成「系统默认」。
//
// dnsServers 是包级 atomic.Value，newDialer() 每次构造都读它：不还原会让同包其余
// 111 个测试文件的出站拨号都挂上这里注入的假 Resolver（表现为一堆互不相关的用例
// 超时，且没人能看出是本文件泄漏的）。
func withDNS(t *testing.T, list []string) {
	t.Helper()
	SetDNSServers(list)
	t.Cleanup(func() { SetDNSServers(nil) })
}

// TestSetDNSServersNormalizesInput 输入清洗：去空白、丢空项、空集=回落系统默认。
func TestSetDNSServersNormalizesInput(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil 回落系统默认", nil, nil},
		{"空切片回落系统默认", []string{}, nil},
		{"裸 IP 原样保留", []string{"1.1.1.1"}, []string{"1.1.1.1"}},
		{"带端口的形态原样保留", []string{"8.8.8.8:53"}, []string{"8.8.8.8:53"}},
		{"前后空白被裁掉", []string{" 1.1.1.1 ", "\t8.8.8.8:53\n"}, []string{"1.1.1.1", "8.8.8.8:53"}},
		{"IPv6 字面量保留方括号形态", []string{"[::1]:53"}, []string{"[::1]:53"}},
		// 关键一条：配置里留了空项（`"dns_servers": ["", ""]` 这种手写产物）时
		// 必须等价于「没配」。若把空串当有效服务器，newDialer 会挂上 Resolver，
		// 而它的 Dial 会对每个域名尝试 dial "" → 系统默认解析被彻底顶掉。
		{"全空项等价于没配", []string{"", "   ", "\t"}, nil},
		{"混合时空项被剔除", []string{"", "1.1.1.1", "  "}, []string{"1.1.1.1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withDNS(t, c.in)
			if got := dnsServerList(); !slices.Equal(got, c.want) {
				t.Fatalf("dnsServerList()=%q want %q", got, c.want)
			}
			// 空配置必须让 Dialer 完全没有自定义 Resolver（= 系统默认）。
			d := newDialer()
			if len(c.want) == 0 {
				if d.Resolver != nil {
					t.Fatal("空 dns_servers 仍挂了自定义 Resolver —— 系统默认解析会被顶掉")
				}
				return
			}
			if d.Resolver == nil {
				t.Fatal("配了 dns_servers 却没有挂 Resolver：配置形同未接线")
			}
		})
	}
}

// TestSetDNSServersCopiesCallerSlice 入参切片被调用方复用/改写时不得影响生效值。
//
// main.go 是把 config 结构体里的切片直接传进来的；那份切片后续会被配置热重载原地
// 改写（config 快照复用同一底层数组）。若这里直接 Store(list)，一次热重载就能把
// 出站 DNS 改成别的值，而日志/面板仍显示旧配置。
func TestSetDNSServersCopiesCallerSlice(t *testing.T) {
	in := []string{"1.1.1.1", "8.8.8.8"}
	withDNS(t, in)
	in[0] = "9.9.9.9" // 调用方原地改写
	if got := dnsServerList(); got[0] != "1.1.1.1" {
		t.Fatalf("生效列表跟着入参切片变了：%q（SetDNSServers 必须复制入参）", got)
	}
}

// TestNewDialerDNSResolverContract 配了 DNS 时 Dialer 的两条硬约束：
// 必须 PreferGo（系统 stub resolver 不认自定义服务器），且超时/keepalive 与
// 集中定义的常量一致（这两项是半死连接的第一道闸，见 transport.go 顶部注释）。
func TestNewDialerDNSResolverContract(t *testing.T) {
	withDNS(t, []string{"1.1.1.1"})
	d := newDialer()
	if d.Resolver == nil {
		t.Fatal("配了 dns_servers 却没有 Resolver")
	}
	if !d.Resolver.PreferGo {
		t.Fatal("Resolver.PreferGo=false：cgo/系统 stub 解析器会忽略自定义 DNS 服务器")
	}
	if d.Timeout != dialTimeout {
		t.Errorf("Dialer.Timeout=%v want %v", d.Timeout, dialTimeout)
	}
	if d.KeepAlive != dialKeepAlive {
		t.Errorf("Dialer.KeepAlive=%v want %v", d.KeepAlive, dialKeepAlive)
	}
}

// TestDNSServerBareAddressDefaultsToPort53 裸 IP 必须补 :53。
//
// 观测手段是 dial 出来的 conn 的 RemoteAddr：它直接暴露「实际拨到哪个地址」，
// 比断言字符串拼接函数更硬 —— 拼错端口（比如补成 5353）同样会被抓住。
func TestDNSServerBareAddressDefaultsToPort53(t *testing.T) {
	withDNS(t, []string{"127.0.0.1"})
	d := newDialer()
	if d.Resolver == nil || d.Resolver.Dial == nil {
		t.Fatal("裸 IP 没有挂上 Resolver.Dial")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := d.Resolver.Dial(ctx, "udp", "")
	if err != nil {
		// 个别内核会立刻回 ECONNREFUSED（127.0.0.1:53 无监听）。这时唯一能验的
		// 就是「不是缺端口」—— 缺端口说明 JoinHostPort 的兜底分支被删了。
		if strings.Contains(err.Error(), "missing port") {
			t.Fatalf("裸 IP 没有补默认端口 53：%v", err)
		}
		t.Logf("本机对 127.0.0.1:53 的 UDP connect 失败（%v），仅校验未出现缺端口错误", err)
		return
	}
	defer conn.Close()
	if got := conn.RemoteAddr().String(); got != "127.0.0.1:53" {
		t.Fatalf("裸 IP 拨到了 %s，应为 127.0.0.1:53（默认端口不是 53）", got)
	}
}

// TestDNSServerExplicitPortIsUsed 显式 host:port 必须按原样拨号（不重复补端口）。
func TestDNSServerExplicitPortIsUsed(t *testing.T) {
	dns := startFakeDNS(t, net.IPv4(127, 0, 0, 1))
	withDNS(t, []string{dns.addr})
	d := newDialer()
	if d.Resolver == nil {
		t.Fatal("配了 dns_servers 却没有 Resolver")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := d.Resolver.Dial(ctx, "udp", "")
	if err != nil {
		t.Fatalf("按配置的 %s 拨号失败：%v", dns.addr, err)
	}
	defer conn.Close()
	if got := conn.RemoteAddr().String(); got != dns.addr {
		t.Fatalf("拨到 %s，配置里写的是 %s", got, dns.addr)
	}
}

// TestDNSResolverActuallyResolvesThroughConfiguredServer 配置的服务器必须真的
// 被用于解析（不是只存进 atomic）。用 .invalid 顶级域：系统解析必然失败，
// 只有走我们注入的假 DNS 才可能拿到结果。
func TestDNSResolverActuallyResolvesThroughConfiguredServer(t *testing.T) {
	dns := startFakeDNS(t, net.IPv4(127, 0, 0, 1))
	withDNS(t, []string{dns.addr})
	d := newDialer()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addrs, err := d.Resolver.LookupHost(ctx, "wbtest-dns-probe.invalid")
	if err != nil {
		t.Fatalf("经配置的 DNS 解析失败：%v", err)
	}
	if !slices.Contains(addrs, "127.0.0.1") {
		t.Fatalf("解析结果 %q 不含假 DNS 应答的 127.0.0.1", addrs)
	}
	if dns.hits.Load() == 0 {
		t.Fatal("假 DNS 一次查询都没收到：解析没走配置的服务器")
	}
}

// TestOutboundTransportUsesConfiguredDNS 端到端：newTransport 的出站请求必须经过
// 配置的 DNS。这是「配置写错 → 全站 DNS 挂」在正向的完整证明 —— 只测 newDialer
// 不够，newTransport 是唯一被 Client 实际使用的入口（若它绕开 newDialer 自己
// 造一个裸 Dialer，前几条用例全绿而线上仍然不走配置）。
func TestOutboundTransportUsesConfiguredDNS(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("解析 httptest 地址: %v", err)
	}

	dns := startFakeDNS(t, net.IPv4(127, 0, 0, 1))
	withDNS(t, []string{dns.addr})

	tr := newTransport()
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	resp, err := client.Get("http://wbtest-dns-egress.invalid:" + port + "/ping")
	if err != nil {
		t.Fatalf("经配置 DNS 的出站请求失败（真实部署形态下这就是全站 5xx）：%v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("code=%d body=%q want 200 ok", resp.StatusCode, body)
	}
	if hits.Load() != 1 {
		t.Fatalf("目标服务器收到 %d 次请求 want 1", hits.Load())
	}
	if dns.hits.Load() == 0 {
		t.Fatal("出站请求没有经过配置的 DNS 服务器")
	}
}

// fakeDNS 最小 A 记录应答器（真实 UDP socket，不打桩 Resolver.Dial）。
//
// 用真实 socket 是刻意的：要验的正是「配置的地址真的被 dial 到、应答真的被用上」，
// 一旦把 Resolver.Dial 替换成假函数，被测的接线就整个被替掉了。
type fakeDNS struct {
	addr string
	hits atomic.Int32
	ip   net.IP
}

// startFakeDNS 起一个只回答 A 查询的 DNS 服务器（其余查询回 NODATA）。
func startFakeDNS(t *testing.T, ip net.IP) *fakeDNS {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起假 DNS 失败: %v", err)
	}
	f := &fakeDNS{addr: pc.LocalAddr().String(), ip: ip.To4()}
	if f.ip == nil {
		t.Fatalf("假 DNS 只支持 IPv4 应答，收到 %v", ip)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, peer, err := pc.ReadFrom(buf)
			if err != nil {
				return // socket 已关：用例结束
			}
			f.hits.Add(1)
			if resp := dnsAResponse(buf[:n], f.ip); resp != nil {
				_, _ = pc.WriteTo(resp, peer)
			}
		}
	}()
	return f
}

// dnsAResponse 把一条 A 查询拼成应答；非 A 查询回 NODATA（ANCOUNT=0），
// 让 Go 解析器自然地只用 A 记录。
func dnsAResponse(q []byte, ip net.IP) []byte {
	if len(q) < 12 {
		return nil
	}
	// 跳过 12 字节头，逐段走 QNAME，定位问题段末尾（QTYPE/QCLASS）。
	i := 12
	for i < len(q) {
		l := int(q[i])
		if l == 0 {
			i++
			break
		}
		if l&0xC0 != 0 { // 查询里不会出现压缩指针，出现就放弃
			return nil
		}
		i += 1 + l
	}
	if i+4 > len(q) {
		return nil
	}
	qend := i + 4
	qtype := binary.BigEndian.Uint16(q[qend-4 : qend-2])

	out := make([]byte, 0, qend+16)
	out = append(out, q[0], q[1]) // 原样回显 ID
	out = append(out, 0x81, 0x80) // QR=1 RD=1 RA=1 RCODE=0
	out = append(out, 0x00, 0x01) // QDCOUNT=1
	if qtype == 1 {               // A
		out = append(out, 0x00, 0x01) // ANCOUNT=1
	} else {
		out = append(out, 0x00, 0x00) // NODATA
	}
	out = append(out, 0x00, 0x00, 0x00, 0x00) // NSCOUNT / ARCOUNT
	out = append(out, q[12:qend]...)          // 回显问题段
	if qtype != 1 {
		return out
	}
	out = append(out, 0xC0, 0x0C)             // 名字指向偏移 12
	out = append(out, 0x00, 0x01, 0x00, 0x01) // TYPE=A CLASS=IN
	out = append(out, 0x00, 0x00, 0x00, 0x3C) // TTL=60
	out = append(out, 0x00, 0x04)             // RDLENGTH=4
	return append(out, ip...)                 // 4 字节地址
}

// ---------------------------------------------------------------------------
// 顺带补的两条 transport.go 零覆盖（同属「出站连接层」主题，故落在本文件）：
//   - F-13：roundTripCloseIdle 在**错误处理路径**上被调用（client.go:1658），
//     若它对 rt 的类型断言写错，一次可恢复的传输失败会变成进程级 panic；
//   - newTransport 的加固参数（禁 h2 / 接管 TLS 拨号 / 超时）集中定义却无断言，
//     改动其中任何一个都不会让任何用例变红。
// ---------------------------------------------------------------------------

// countingCloser 既是 RoundTripper 又实现 CloseIdleConnections，用来数调用次数。
type countingCloser struct {
	n int32
}

func (c *countingCloser) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, io.EOF
}
func (c *countingCloser) CloseIdleConnections() { atomic.AddInt32(&c.n, 1) }

// plainRT 只实现 RoundTripper，**不**实现 closeIdler（测试注入的 rtFunc 就是这种）。
type plainRT struct{}

func (plainRT) RoundTrip(*http.Request) (*http.Response, error) { return nil, io.EOF }

// TestRoundTripCloseIdle roundTripCloseIdle 只该对「支持清池」的 rt 动手，且绝不 panic。
//
// 不变量：① 实现了 CloseIdleConnections 的 rt 被调用恰好一次；② nil / 不支持的 rt
// 静默跳过（这里是错误处理路径，任何 panic 都会把可恢复的传输失败升级成进程级故障）。
func TestRoundTripCloseIdle(t *testing.T) {
	c := &countingCloser{}
	roundTripCloseIdle(c)
	if got := atomic.LoadInt32(&c.n); got != 1 {
		t.Fatalf("CloseIdleConnections 被调用 %d 次 want 1（失败连接会留在池里被下个请求复用）", got)
	}
	roundTripCloseIdle(nil)       // 不得 panic
	roundTripCloseIdle(plainRT{}) // 不得 panic
	roundTripCloseIdle(rtFunc(func(*http.Request) (*http.Response, error) { return nil, io.EOF }))
	if got := atomic.LoadInt32(&c.n); got != 1 {
		t.Fatalf("对不支持清池的 rt 也动了手（计数变成 %d）", got)
	}
}

// TestNewTransportHardeningContract 出站 Transport 的加固参数必须与集中定义的常量一致。
//
// 每条都对应一类实测事故（见 transport.go 顶部注释）：
//   - 空 TLSNextProto：否则 ALPN 协商出 h2，半死 h2 流复用表现为 60s 超时；
//   - DialTLSContext 非 nil：否则头序重排拿不到明文请求头（指纹与头序同时失效）；
//   - Proxy 必须 nil：留着 ProxyFromEnvironment 会让 net/http 自己 CONNECT+TLS 握手，
//     指纹与头序全部失效；
//   - 三个超时/池容量参数：半死连接的第一道闸。
func TestNewTransportHardeningContract(t *testing.T) {
	tr := newTransport()
	defer tr.CloseIdleConnections()

	if tr.TLSNextProto == nil {
		t.Fatal("TLSNextProto 为 nil：标准库会注入默认 h2 映射，禁 h2 失效")
	}
	if len(tr.TLSNextProto) != 0 {
		t.Fatalf("TLSNextProto 非空（%d 项）：h2 会被协商出来", len(tr.TLSNextProto))
	}
	if tr.DialTLSContext == nil {
		t.Fatal("DialTLSContext 为 nil：头序重排拿不到明文请求头")
	}
	if tr.DialContext == nil {
		t.Fatal("DialContext 为 nil：newDialer 的超时/keepalive/DNS 全部失效")
	}
	if tr.Proxy != nil {
		t.Fatal("Proxy 非 nil：net/http 会自己 CONNECT+TLS，指纹与头序失效")
	}
	if tr.ResponseHeaderTimeout != responseHeaderTimeout {
		t.Errorf("ResponseHeaderTimeout=%v want %v", tr.ResponseHeaderTimeout, responseHeaderTimeout)
	}
	if tr.IdleConnTimeout != idleConnTimeout {
		t.Errorf("IdleConnTimeout=%v want %v", tr.IdleConnTimeout, idleConnTimeout)
	}
	if tr.MaxIdleConnsPerHost != maxIdleConnsPerHost || tr.MaxIdleConns != maxIdleConns {
		t.Errorf("池容量=%d/%d want %d/%d",
			tr.MaxIdleConns, tr.MaxIdleConnsPerHost, maxIdleConns, maxIdleConnsPerHost)
	}
}
