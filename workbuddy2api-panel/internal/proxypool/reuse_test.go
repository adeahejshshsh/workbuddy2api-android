package proxypool

import (
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"
)

// Close 之后又读响应体是调用方自己的事（网关对同一个 body 关两次时，第二次会读到「已关闭」），
// 不能当成代理故障清掉节点的空闲连接：下一个请求仍要复用。
func TestReadAfterCloseKeepsIdleConnections(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer target.Close()
	px, _ := connectProxy(t, false)
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	base.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // 本地自签证书
	m, err := New("", base, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	id := addProxy(t, m, strings.Replace(px.URL, "://", "://user:secret@", 1))
	m.health[id] = health{status: "healthy"}
	do := func() (reused bool) {
		ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{GotConn: func(c httptrace.GotConnInfo) { reused = c.Reused }})
		req, _ := http.NewRequestWithContext(ctx, "GET", target.URL, nil)
		resp, err := m.Do(&http.Client{Timeout: 3 * time.Second}, req, "u", "cn")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if _, err := resp.Body.Read(make([]byte, 1)); err == nil || IsError(err) {
			t.Fatalf("Close 之后再读应原样报调用方错误，不包成代理错误：%v", err)
		}
		return reused
	}
	do()
	if !do() {
		t.Fatal("Close 之后读了一次，节点的空闲连接被清掉了")
	}
}
