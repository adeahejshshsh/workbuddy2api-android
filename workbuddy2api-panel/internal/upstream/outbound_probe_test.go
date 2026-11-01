// outbound_probe_test.go 出站报文探针（手工审计用，默认跳过）。
//
// 跑法：WB2A_PROBE=1 go test ./internal/upstream -run TestProbeOutbound -v
//
// 作用：把各出站路径**真实写出的字节**抓下来（裸 TCP 抓明文请求头 + 抓 TLS
// ClientHello），用于对照官方客户端抓包做「头顺序 / 自动补头 / JA3 输入」审计。
//
// 必须经 c.do() 发请求：它是唯一出站口，头名拼写纠正（normalizeHeaderSpelling）
// 就在里面。直接用 c.HTTP.Do() 会绕过生产逻辑，看到的不是线上真实字节。
//
// 常规 `go test ./...` 会跳过——它会 bind 本地端口并往 stdout 打印大段报文。
package upstream

import (
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// requireProbe 未设置 WB2A_PROBE 时跳过（默认不进 CI 的安静测试集）。
func requireProbe(t *testing.T) {
	t.Helper()
	if os.Getenv("WB2A_PROBE") == "" {
		t.Skip("set WB2A_PROBE=1 to run outbound probe")
	}
}

// rawCaptured 起一个裸 TCP 监听，收回一次请求的原始字节，然后回一个正常响应。
func rawCaptured(t *testing.T, https bool, build func(base string)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	scheme := "http"
	if https {
		scheme = "https"
	}
	base := scheme + "://" + ln.Addr().String()

	got := make(chan []byte, 1)
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			got <- nil
			return
		}
		defer conn.Close()
		if https {
			// 抓 ClientHello 后就断开（不需要真握手）。
			conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			buf := make([]byte, 4096)
			n, _ := conn.Read(buf)
			got <- buf[:n]
			return
		}
		var all []byte
		conn.SetReadDeadline(time.Now().Add(400 * time.Millisecond))
		tmp := make([]byte, 8192)
		for {
			n, rerr := conn.Read(tmp)
			if n > 0 {
				all = append(all, tmp[:n]...)
				if i := bytes.Index(all, []byte("\r\n\r\n")); i >= 0 {
					head := string(all[:i])
					var clen int
					for _, line := range strings.Split(head, "\r\n") {
						if strings.HasPrefix(strings.ToLower(line), "content-length:") {
							fmt.Sscanf(strings.TrimSpace(line[len("content-length:"):]), "%d", &clen)
						}
					}
					if len(all) >= i+4+clen {
						break
					}
				}
			}
			if rerr != nil {
				break
			}
			conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		}
		got <- all
		body := `{"code":0,"data":{}}`
		fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	}()

	build(base)

	select {
	case b := <-got:
		return string(b)
	case <-time.After(6 * time.Second):
		return "<timeout>"
	}
}

func dumpBlock(title, raw string) {
	fmt.Printf("\n===== %s =====\n", title)
	if raw == "" {
		fmt.Println("<empty>")
		return
	}
	if i := strings.Index(raw, "\r\n\r\n"); i >= 0 {
		raw = raw[:i]
	}
	fmt.Println(raw)
}

func probeClient() *Client {
	c := New()
	c.UserAgent = ""
	c.DeviceToken = ""
	c.PassthroughIP = false
	return c
}

// TestProbeOutboundRequestBytes 抓六条出站链路的原始请求字节。
func TestProbeOutboundRequestBytes(t *testing.T) {
	requireProbe(t)
	c := probeClient()
	cn := &auth.Auth{UID: "uid-cn-0001", AccessToken: "AT-CN", RefreshToken: "RT-CN", Nickname: "cn", Domain: "www.workbuddy.cn"}
	gl := &auth.Auth{UID: "uid-gl-0001", AccessToken: "AT-GL", RefreshToken: "RT-GL", Nickname: "gl", Domain: "www.workbuddy.ai"}
	meta := ChatMeta{ConversationID: "01a1815d-dacf-75f1-9a00-3f6f00000001", ConversationRequestID: "01a1815ddacf75f19a003f6f00000002", CacheKeyID: "sess-1"}

	raw := rawCaptured(t, false, func(base string) {
		req, _ := http.NewRequest(http.MethodPost, base+"/v2/chat/completions", bytes.NewReader([]byte(`{"model":"x","messages":[]}`)))
		c.ChatHeaders(req, cn, "", meta)
		resp, err := c.do(req)
		if err != nil {
			fmt.Println("chat CN err:", err)
			return
		}
		resp.Body.Close()
	})
	dumpBlock("chat · CN 个人号（ChatHeaders）", raw)

	raw = rawCaptured(t, false, func(base string) {
		req, _ := http.NewRequest(http.MethodPost, base+"/v2/chat/completions", bytes.NewReader([]byte(`{"model":"x","messages":[]}`)))
		c.ChatHeaders(req, gl, "", meta)
		resp, err := c.do(req)
		if err != nil {
			fmt.Println("chat GL err:", err)
			return
		}
		resp.Body.Close()
	})
	dumpBlock("chat · global 个人号（ChatHeaders）", raw)

	raw = rawCaptured(t, false, func(base string) {
		req, _ := http.NewRequest(http.MethodPost, base+"/v2/report", bytes.NewReader([]byte(`[]`)))
		c.CommonHeaders(req, cn)
		req.Header.Set("Authorization", "Bearer AT-CN")
		resp, err := c.do(req)
		if err != nil {
			fmt.Println("rpc err:", err)
			return
		}
		resp.Body.Close()
	})
	dumpBlock("RPC 层（CommonHeaders，/v2/report）", raw)

	raw = rawCaptured(t, false, func(base string) {
		req, _ := http.NewRequest(http.MethodPost, base+"/v2/plugin/auth/token/refresh", strings.NewReader("{}"))
		c.RefreshHeaders(req, cn)
		resp, err := c.do(req)
		if err != nil {
			fmt.Println("refresh err:", err)
			return
		}
		resp.Body.Close()
	})
	dumpBlock("refresh（RefreshHeaders）", raw)

	raw = rawCaptured(t, false, func(base string) {
		req, _ := http.NewRequest(http.MethodPost, base+"/v2/billing/meter/get-user-resource", strings.NewReader("{}"))
		c.BillingHeaders(req, cn)
		resp, err := c.do(req)
		if err != nil {
			fmt.Println("billing err:", err)
			return
		}
		resp.Body.Close()
	})
	dumpBlock("billing（BillingHeaders）", raw)

	c.ChatBaseCN = "http://127.0.0.1:1" // 占位，真值在 build 里覆盖
	raw = rawCaptured(t, false, func(base string) {
		c2 := probeClient()
		c2.ChatBaseCN = base
		c2.WebBaseCN = base
		_ = c2.ReportDesktopEvent(cn, DesktopEvent{"eventCode": "agent_task_created"})
	})
	dumpBlock("桌面 /v2/report（ReportDesktopEvent）", raw)

	raw = rawCaptured(t, false, func(base string) {
		c3 := probeClient()
		c3.WebBaseCN = base
		_ = c3.ReportWebEvent(cn, "library_doc_intro_click", "https://www.workbuddy.cn/library", "intro", "简介")
	})
	dumpBlock("Web /v2/report（ReportWebEvent）", raw)
}

// TestZZProbeClientHello 抓 Go 出站 TLS ClientHello 的关键字段（JA3 的输入）。
func TestProbeOutboundClientHello(t *testing.T) {
	requireProbe(t)
	tr := newTransport()
	c := &Client{HTTP: &http.Client{Transport: tr, Timeout: 3 * time.Second}, ChatHTTP: &http.Client{Transport: tr, Timeout: 3 * time.Second}}
	raw := rawCaptured(t, true, func(base string) {
		req, _ := http.NewRequest(http.MethodGet, base+"/", nil)
		if resp, err := c.HTTP.Do(req); err == nil {
			resp.Body.Close()
		}
	})
	b := []byte(raw)
	if len(b) < 44 {
		t.Fatalf("ClientHello 太短: %d bytes", len(b))
	}
	fmt.Printf("\n===== TLS ClientHello（newTransport，禁 h2） =====\n")
	fmt.Printf("record: type=%d version=0x%04x len=%d\n", b[0], binary.BigEndian.Uint16(b[1:3]), binary.BigEndian.Uint16(b[3:5]))
	p := 5
	if b[p] != 1 {
		t.Fatalf("不是 ClientHello: %d", b[p])
	}
	hsLen := int(b[p+1])<<16 | int(b[p+2])<<8 | int(b[p+3])
	p += 4
	legacyVer := binary.BigEndian.Uint16(b[p : p+2])
	p += 2
	p += 32 // random
	sidLen := int(b[p])
	p++
	sid := b[p : p+sidLen]
	fmt.Printf("session_id=%x\n", sid)
	p += sidLen
	csLen := int(binary.BigEndian.Uint16(b[p : p+2]))
	p += 2
	cs := b[p : p+csLen]
	p += csLen
	cmLen := int(b[p])
	cm := b[p+1 : p+1+cmLen]
	p += 1 + cmLen
	fmt.Printf("handshake_len=%d legacy_version=0x%04x session_id_len=%d compression=%v\n", hsLen, legacyVer, sidLen, cm)
	var csHex []string
	for i := 0; i+1 < len(cs); i += 2 {
		csHex = append(csHex, fmt.Sprintf("%04x", binary.BigEndian.Uint16(cs[i:i+2])))
	}
	fmt.Printf("cipher_suites(%d)=%s\n", len(csHex), strings.Join(csHex, ","))
	if p+2 > len(b) {
		return
	}
	extLen := int(binary.BigEndian.Uint16(b[p : p+2]))
	p += 2
	end := p + extLen
	var exts []string
	for p+4 <= end && p+4 <= len(b) {
		etype := binary.BigEndian.Uint16(b[p : p+2])
		elen := int(binary.BigEndian.Uint16(b[p+2 : p+4]))
		data := b[p+4 : min(p+4+elen, len(b))]
		exts = append(exts, fmt.Sprintf("%d", etype))
		switch etype {
		case 16: // ALPN
			var protos []string
			q := 2
			for q+1 < len(data) {
				l := int(data[q])
				if q+1+l > len(data) {
					break
				}
				protos = append(protos, string(data[q+1:q+1+l]))
				q += 1 + l
			}
			fmt.Printf("  ALPN(16) = %v\n", protos)
		case 10: // supported_groups
			var g []string
			for i := 2; i+1 < len(data); i += 2 {
				g = append(g, fmt.Sprintf("%04x", binary.BigEndian.Uint16(data[i:i+2])))
			}
			fmt.Printf("  groups(10) = %s\n", strings.Join(g, ","))
		case 11: // ec_point_formats
			fmt.Printf("  ec_point_formats(11) = %v\n", data[1:])
		case 43: // supported_versions
			fmt.Printf("  supported_versions(43) = %v\n", data[1:])
		case 13: // signature_algorithms
			var s []string
			for i := 2; i+1 < len(data); i += 2 {
				s = append(s, fmt.Sprintf("%04x", binary.BigEndian.Uint16(data[i:i+2])))
			}
			fmt.Printf("  sigalgs(13) = %s\n", strings.Join(s, ","))
		}
		p += 4 + elen
	}
	fmt.Printf("extensions(%d) = %s\n", len(exts), strings.Join(exts, ","))
}

// TestProbeOutboundTransport 确认 Go 出厂 Transport 的 TLS 配置面（ServerName/NextProtos）。
func TestProbeOutboundTransport(t *testing.T) {
	requireProbe(t)
	_ = tls.VersionTLS13
	tr := newTransport()
	fmt.Printf("\n===== Transport 面 =====\n")
	fmt.Printf("TLSNextProto len=%d (0=允许 h2 / >0=禁 h2)\n", len(tr.TLSNextProto))
	fmt.Printf("ForceAttemptHTTP2=%v DisableCompression=%v DisableKeepAlives=%v\n", tr.ForceAttemptHTTP2, tr.DisableCompression, tr.DisableKeepAlives)
	var _ = net.IPv4len
}
