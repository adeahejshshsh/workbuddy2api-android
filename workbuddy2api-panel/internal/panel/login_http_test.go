package panel

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestLoginHTTPDisablesH2 钉住「登录 client 必须禁 h2」这条（2026-10-24 实案）：
// 默认 transport 经 ALPN 协商 h2，半死 h2 流被复用时表现为
// 「context deadline exceeded (Client.Timeout exceeded while awaiting headers)」——
// 国内版拿不到授权链接、登录轮询卡住看着像服务端卡死。
// 判据：TLSNextProto 非 nil（真正禁 h2 的唯一写法，见 internal/upstream/transport.go）
// 且响应头/TLS 超时已配齐。
func TestLoginHTTPDisablesH2(t *testing.T) {
	tr, ok := loginHTTP.Transport.(*http.Transport)
	if !ok || tr == nil {
		t.Fatal("loginHTTP 必须有显式 *http.Transport（默认 transport 会启用 h2）")
	}
	if tr.TLSNextProto == nil {
		t.Error("TLSNextProto 为 nil = 标准库会注入默认 h2 映射；必须 make 一个空映射才真正禁 h2")
	}
	if tr.ResponseHeaderTimeout <= 0 {
		t.Error("ResponseHeaderTimeout 必须为正：否则半死连接会一直等响应头到总超时")
	}
	if tr.TLSHandshakeTimeout <= 0 || tr.DialContext == nil {
		t.Error("建连/TLS 超时必须配齐")
	}
}

// TestDoJSONRetriesTransportFailure 纯传输失败（一个响应都没拿到）应自动重试；
// 这是 2026-10-24 DNS 抖动（127.0.0.11 server misbehaving）下「单发失败即报错」
// 的兜底。
func TestDoJSONRetriesTransportFailure(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			// 第一次直接断开：客户端拿到 EOF/连接重置，status=0，应触发重试。
			if hj, ok := w.(http.Hijacker); ok {
				if c, _, err := hj.Hijack(); err == nil {
					_ = c.Close()
					return
				}
			}
		}
		_, _ = io.WriteString(w, `{"code":0,"msg":"OK","data":{"state":"s1","authUrl":"https://x/login"}}`)
	}))
	defer srv.Close()

	old := loginHTTP
	loginHTTP = &http.Client{Timeout: 5 * time.Second}
	defer func() { loginHTTP = old }()

	data, status, err := doJSON(loginRoute{}, http.MethodPost, srv.URL, nil, []byte("{}"), siteForRealm("cn"))
	if err != nil {
		t.Fatalf("传输失败后应重试成功，实际 err=%v status=%d", err, status)
	}
	if status != 200 {
		t.Fatalf("status=%d want 200", status)
	}
	if len(data) == 0 {
		t.Fatal("data 为空")
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 2 {
		t.Fatalf("上游被调用 %d 次，want 2（第一次断开 + 一次重试）", got)
	}
}

// TestDoJSONDoesNotRetryBusinessError 已拿到响应（业务 code != 0）绝不重试：
// 登录轮询每秒都在打 token 端点，pending 时返回 11217 "login ing"，
// 重试会把轮询拖慢 3 倍且毫无意义。
func TestDoJSONDoesNotRetryBusinessError(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		_, _ = io.WriteString(w, `{"code":11217,"msg":"login ing..."}`)
	}))
	defer srv.Close()

	old := loginHTTP
	loginHTTP = &http.Client{Timeout: 5 * time.Second}
	defer func() { loginHTTP = old }()

	_, status, err := doJSON(loginRoute{}, http.MethodGet, srv.URL, nil, nil, siteForRealm("cn"))
	if err == nil {
		t.Fatal("业务错误应返回 err")
	}
	if status != 200 {
		t.Fatalf("status=%d want 200（有响应就不该重试）", status)
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Fatalf("业务错误被调用 %d 次，want 1（不得重试）", got)
	}
}
