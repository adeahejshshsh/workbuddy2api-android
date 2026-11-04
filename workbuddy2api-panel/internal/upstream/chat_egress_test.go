package upstream

// chat 流式走的是 `c.chatHTTP().Do(req)`（无总时长上限的专用 client），此前**绕过**
// 了统一出口 do() —— 于是最敏感的那条链上每请求少一行 `Connection`、头名大小写退回
// Go canonical（X-Stainless-Lang 而非官方小写），而唯一的端到端验证走的是 do()，
// 测试永远绿。
//
// 必须抓**原始字节**：Go 的 server 端 `textproto.ReadMIMEHeader` 会把头名重新
// canonical 化，用 httptest 读 r.Header 永远看不到真实拼写（很容易误判成「已对齐」）。

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// rawHTTPStub 起一个裸 TCP 服务：抓一次请求的原始头文本，再回一段最小 SSE。
func rawHTTPStub(t *testing.T) (addr string, captured func() string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	ch := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			ch <- ""
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		var sb strings.Builder
		for {
			line, err := br.ReadString('\n')
			sb.WriteString(line)
			if err != nil || line == "\r\n" {
				break
			}
		}
		ch <- sb.String()
		body := "data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
			"data: [DONE]\n\n"
		fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	}()
	return "http://" + ln.Addr().String(), func() string { return <-ch }
}

func TestChatStreamUsesSameOutboundRewrite(t *testing.T) {
	addr, captured := rawHTTPStub(t)
	c := &Client{HTTP: &http.Client{}, ChatBaseCN: addr, BillingBaseCN: addr}
	a := &auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}

	rc, _, _, err := c.ChatStreamContext(context.Background(), a,
		[]byte(`{"model":"glm-5.2","stream":true,"messages":[]}`), "", ChatMeta{})
	if err != nil {
		t.Fatalf("chat stream: %v", err)
	}
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()

	raw := captured()
	if raw == "" {
		t.Fatal("没抓到请求")
	}
	// ① 官方 chat 恒发 Connection: keep-alive（Go 默认不发这一行）
	if !strings.Contains(raw, "\r\nConnection: keep-alive\r\n") {
		t.Fatalf("chat 出站缺 Connection: keep-alive —— 说明没走 prepareOutbound。原始头：\n%s", raw)
	}
	// ② 官方小写拼写（Go canonical 会是 X-Stainless-Lang）
	if !strings.Contains(raw, "\r\nx-stainless-lang: ") {
		t.Fatalf("x-stainless-lang 未按官方小写形态发出。原始头：\n%s", raw)
	}
	if strings.Contains(raw, "\r\nX-Stainless-Lang: ") {
		t.Fatalf("头名仍是 Go canonical 形态（X-Stainless-Lang）。原始头：\n%s", raw)
	}
}
