package proxypool

// 本文件是本仓自带的 SOCKS5 拨号器，用来**替代**上游依赖的 `golang.org/x/net/proxy`。
//
// 为什么不用那个包：它是本模块唯一的外部依赖，而本仓的构建与 CI 都在拉不到模块的
// 环境里跑（`proxy.golang.org` / 各镜像的 DNS 都不通，见 AGENT.md「离线」一节）。
// 为了 3 个符号（Auth / SOCKS5 / ContextDialer）引入一个模块，代价是「整个
// internal/proxypool 编译不过」，不值当。这里按 RFC 1928 / RFC 1929 实现同样的语义，
// 只保留本模块真正用到的路径：CONNECT + 可选用户名口令认证。
//
// 与 x/net/proxy 的行为对齐点（改的时候别破坏）：
//   - 目标地址按 SOCKS5 的域名形态下发（ATYP=3），由代理侧解析 —— 这正是
//     `socks5h` 与 `socks5` 的区别所在，本地解析由调用方（transport.go）自己做；
//   - 认证方式协商失败、REP 非 0 一律返回带原因的 error（不静默降级成直连，
//     否则「配了代理却走直连」这种事故没人能查）；
//   - DialContext 支持 ctx 取消/超时：建连与握手都受同一个 ctx 约束。

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// socksAuth 用户名口令认证（RFC 1929）。nil = 不认证。
type socksAuth struct {
	User     string
	Password string
}

const (
	socks5Version      = 0x05
	socksAuthVersion   = 0x01
	socksMethodNone    = 0x00
	socksMethodUserPwd = 0x02
	socksMethodNoMatch = 0xFF
	socksCmdConnect    = 0x01
	socksAtypIPv4      = 0x01
	socksAtypDomain    = 0x03
	socksAtypIPv6      = 0x04
)

// socks5Dialer 经指定 SOCKS5 代理建 TCP 隧道。
type socks5Dialer struct {
	proxyAddr string    // host:port
	auth      *socksAuth
	forward   *net.Dialer
}

// newSocks5Dialer 造一个拨号器。forward 为到代理本身的底层拨号器（带超时与流量计量）。
func newSocks5Dialer(proxyAddr string, auth *socksAuth, forward *net.Dialer) (*socks5Dialer, error) {
	if proxyAddr == "" {
		return nil, errors.New("socks5: empty proxy address")
	}
	if forward == nil {
		forward = &net.Dialer{Timeout: 30 * time.Second}
	}
	return &socks5Dialer{proxyAddr: proxyAddr, auth: auth, forward: forward}, nil
}

// DialContext 建到 address 的隧道（ctx 取消会中断建连与握手）。
func (d *socks5Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("socks5: unsupported network %q", network)
	}
	conn, err := d.forward.DialContext(ctx, "tcp", d.proxyAddr)
	if err != nil {
		return nil, err
	}
	// ctx 取消 → 立刻打断阻塞的读写（握手里可能正等一个不回话的代理）。
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if err := d.handshake(conn, address); err != nil {
		_ = conn.Close()
		return nil, err
	}
	// 握手完成：清掉临时截止时间，后续由调用方按需设置。
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func (d *socks5Dialer) handshake(conn net.Conn, address string) error {
	// 1) 方法协商
	methods := []byte{socksMethodNone}
	if d.auth != nil {
		methods = []byte{socksMethodUserPwd}
	}
	req := append([]byte{socks5Version, byte(len(methods))}, methods...)
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("socks5: greet write: %w", err)
	}
	var resp [2]byte
	if _, err := io.ReadFull(conn, resp[:]); err != nil {
		return fmt.Errorf("socks5: greet read: %w", err)
	}
	if resp[0] != socks5Version {
		return fmt.Errorf("socks5: unexpected version %d", resp[0])
	}
	switch resp[1] {
	case socksMethodNone:
		// 代理选了「免认证」：即便我们提供了口令也照它走（与 x/net/proxy 一致）。
	case socksMethodUserPwd:
		if d.auth == nil {
			return errors.New("socks5: proxy requires authentication but no credentials configured")
		}
		if err := d.authUserPass(conn); err != nil {
			return err
		}
	case socksMethodNoMatch:
		return errors.New("socks5: no acceptable authentication method")
	default:
		return fmt.Errorf("socks5: unsupported auth method %#x", resp[1])
	}

	// 2) CONNECT
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("socks5: bad target %q: %w", address, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return fmt.Errorf("socks5: bad port %q", portStr)
	}
	req = []byte{socks5Version, socksCmdConnect, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			req = append(req, socksAtypIPv4)
			req = append(req, v4...)
		} else {
			req = append(req, socksAtypIPv6)
			req = append(req, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return errors.New("socks5: target hostname too long")
		}
		req = append(req, socksAtypDomain, byte(len(host)))
		req = append(req, host...)
	}
	req = binary.BigEndian.AppendUint16(req, uint16(port))
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("socks5: connect write: %w", err)
	}

	// 3) 应答：VER REP RSV ATYP BND.ADDR BND.PORT
	var head [4]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return fmt.Errorf("socks5: connect read: %w", err)
	}
	if head[0] != socks5Version {
		return fmt.Errorf("socks5: unexpected version %d in reply", head[0])
	}
	if head[1] != 0x00 {
		return fmt.Errorf("socks5: connect rejected: %s", socksReplyText(head[1]))
	}
	switch head[3] {
	case socksAtypIPv4:
		if _, err := io.ReadFull(conn, make([]byte, 4+2)); err != nil {
			return fmt.Errorf("socks5: bind addr read: %w", err)
		}
	case socksAtypIPv6:
		if _, err := io.ReadFull(conn, make([]byte, 16+2)); err != nil {
			return fmt.Errorf("socks5: bind addr read: %w", err)
		}
	case socksAtypDomain:
		var l [1]byte
		if _, err := io.ReadFull(conn, l[:]); err != nil {
			return fmt.Errorf("socks5: bind domain len: %w", err)
		}
		if _, err := io.ReadFull(conn, make([]byte, int(l[0])+2)); err != nil {
			return fmt.Errorf("socks5: bind domain read: %w", err)
		}
	default:
		return fmt.Errorf("socks5: unknown bind address type %#x", head[3])
	}
	return nil
}

// authUserPass RFC 1929：VER ULEN UNAME PLEN PASSWD。
func (d *socks5Dialer) authUserPass(conn net.Conn) error {
	u, p := d.auth.User, d.auth.Password
	if len(u) > 255 || len(p) > 255 {
		return errors.New("socks5: credential too long")
	}
	buf := make([]byte, 0, 3+len(u)+len(p))
	buf = append(buf, socksAuthVersion, byte(len(u)))
	buf = append(buf, u...)
	buf = append(buf, byte(len(p)))
	buf = append(buf, p...)
	if _, err := conn.Write(buf); err != nil {
		return fmt.Errorf("socks5: auth write: %w", err)
	}
	var resp [2]byte
	if _, err := io.ReadFull(conn, resp[:]); err != nil {
		return fmt.Errorf("socks5: auth read: %w", err)
	}
	if resp[1] != 0x00 {
		return errors.New("socks5: authentication failed")
	}
	return nil
}

// socksReplyText REP 码的人话（排查日志里能直接看出是代理拒绝还是目标不可达）。
func socksReplyText(rep byte) string {
	switch rep {
	case 0x01:
		return "general SOCKS server failure"
	case 0x02:
		return "connection not allowed by ruleset"
	case 0x03:
		return "network unreachable"
	case 0x04:
		return "host unreachable"
	case 0x05:
		return "connection refused"
	case 0x06:
		return "TTL expired"
	case 0x07:
		return "command not supported"
	case 0x08:
		return "address type not supported"
	default:
		return fmt.Sprintf("reply code %#x", rep)
	}
}
