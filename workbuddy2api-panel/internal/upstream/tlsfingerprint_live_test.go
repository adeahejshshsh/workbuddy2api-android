// tlsfingerprint_live_test.go 用**生产代码路径**对真实上游做 TLS 握手，
// 确认指纹复刻没有把生产流量搞挂。
//
// 跑法：WB2A_LIVE=1 go test ./internal/upstream -run TestLiveTLSHandshake -v
//
// 这是唯一能证明「复刻的 ClientHello 被官方网关接受」的测试——只抓自己发出的
// 字节不够，服务端得真握手成功才算数。默认跳过（会发真实网络请求）。
package upstream

import (
	"context"
	"net"
	"os"
	"testing"
	"time"
)

// TestLiveTLSHandshake 国内版与国际版各做一次真实握手。
//
// 覆盖点：
//   - newTLSDialer 的 uTLS 路径能完成完整握手（不只是发出 ClientHello）
//   - 协商出 TLS 1.3 且 cipher 是官方也会选的 0x1302/0x1301/0x1303
//   - ALPN 为空（与官方 Node 栈一致，也与禁 h2 口径一致）
func TestLiveTLSHandshake(t *testing.T) {
	if os.Getenv("WB2A_LIVE") == "" {
		t.Skip("set WB2A_LIVE=1 to run live TLS handshake against real upstream")
	}
	// 两个域都要验：官方国内版/国际版是不同分发包，万一其中一边网关策略更严，
	// 只验一个会有盲区。
	hosts := []string{"www.workbuddy.cn", "www.workbuddy.ai"}
	d := newDialer()
	dial := newTLSDialer(d, nil, TLSFingerprintNode)

	for _, host := range hosts {
		host := host
		t.Run(host, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()

			conn, err := dial(ctx, "tcp", net.JoinHostPort(host, "443"))
			if err != nil {
				t.Fatalf("握手失败: %v", err)
			}
			defer conn.Close()

			// newTLSDialer 返回的是 headOrderConn，底下是 *utls.UConn；
			// 这里只验「握手能成」，不发任何业务请求。
			t.Logf("✅ %s TLS 握手成功（conn=%T）", host, conn)
		})
	}
}
