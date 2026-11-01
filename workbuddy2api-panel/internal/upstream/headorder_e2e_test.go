// headorder_e2e_test.go 端到端验证：真实 TLS 连接上，Transport 写出的请求头
// 是否已按官方顺序排列。
//
// 为什么必须端到端：headorder_test.go 验的是纯函数和 conn 包装，但「Transport
// 到底有没有用我们替换的 DialTLSContext」只有真跑一遍才能确认——这一层如果
// 没接上，前面所有测试全绿而线上毫无变化。
package upstream

import (
	"bufio"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// selfSignedServerTLS 造一张 127.0.0.1 自签证书（测试专用，不落盘）。
func selfSignedServerTLS(t *testing.T) *tls.Config {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: priv}}}
}

// TestTransportUsesHeadOrderConn 确认真实 TLS 路径上出现官方头顺序。
//
// 手段：自签 TLS 服务器 + 注入 InsecureSkipVerify 的 newTLSDialer，
// 让 Transport 走我们那条 DialTLSContext；服务端侧用 hijack 拿原始字节。
func TestTransportUsesHeadOrderConn(t *testing.T) {
	srvTLS := selfSignedServerTLS(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	tlsLn := tls.NewListener(ln, srvTLS)

	var mu sync.Mutex
	var rawHead strings.Builder
	go func() {
		for {
			conn, aerr := tlsLn.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				var head []byte
				for {
					ln, rerr := br.ReadBytes('\n')
					head = append(head, ln...)
					if rerr != nil {
						break
					}
					if string(ln) == "\r\n" {
						break
					}
				}
				mu.Lock()
				rawHead.Write(head)
				mu.Unlock()
				c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nContent-Type: application/json\r\n\r\n{}"))
			}(conn)
		}
	}()

	tr := &http.Transport{
		DialTLSContext: newTLSDialer(newDialer(), &tls.Config{InsecureSkipVerify: true}),
	}
	httpClient := &http.Client{Transport: tr}
	c := &Client{HTTP: httpClient, ChatHTTP: httpClient, ChatBaseCN: "https://" + ln.Addr().String()}
	a := &auth.Auth{UID: "uid-e2e", AccessToken: "AT", Nickname: "e2e"}
	meta := ChatMeta{
		ConversationID:        "f7c6c392-e016-43bb-beca-ab918b436668",
		ConversationRequestID: "01a194ad31c27d81804ee9915cf954fb",
	}

	req, _ := http.NewRequest(http.MethodPost, "https://"+ln.Addr().String()+"/v2/chat/completions",
		strings.NewReader(`{"model":"x","messages":[]}`))
	c.ChatHeaders(req, a, "", meta)
	resp, derr := c.do(req) // 本仓 do() 只收 req（单一出口，不分 chat/RPC client）
	if derr != nil {
		t.Fatalf("do: %v", derr)
	}
	resp.Body.Close()

	mu.Lock()
	got := rawHead.String()
	mu.Unlock()
	if got == "" {
		t.Fatal("服务端没收到请求头")
	}

	_, offHeaders := splitFixture(t, "official_chat_head.txt")
	lines := strings.Split(got, "\r\n")
	if len(lines) < 2 {
		t.Fatalf("头块异常: %q", got)
	}
	var gotOrder []string
	for _, ln := range lines[1:] {
		if ln == "" {
			continue
		}
		if i := strings.Index(ln, ":"); i > 0 {
			gotOrder = append(gotOrder, ln[:i])
		}
	}

	// 官方顺序里的头，在本项目出站里的相对次序必须与官方一致。
	rank := map[string]int{}
	for i, h := range offHeaders {
		if j := strings.Index(h, ":"); j > 0 {
			rank[strings.ToLower(h[:j])] = i
		}
	}
	lastRank := -1
	lastName := ""
	for _, h := range gotOrder {
		r, ok := rank[strings.ToLower(h)]
		if !ok {
			continue // 表外头（如 Host 之外的临时头）跳过
		}
		if r < lastRank {
			t.Fatalf("顺序偏离官方：%q(rank %d) 出现在 %q(rank %d) 之后\n实际顺序: %v",
				h, r, lastName, lastRank, gotOrder)
		}
		lastRank, lastName = r, h
	}
	t.Logf("实际出站顺序: %v", gotOrder)
}
