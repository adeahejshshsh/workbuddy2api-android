package httpauth

import (
	"net/http/httptest"
	"testing"
)

// W5-04 回归：SetAllowEmptyKey(false) 的语义是「空 key 只允许回环」，
// 不是「全拒」——Android 内嵌/单机自托管的流量都来自 127.0.0.1。
func TestW5_04EmptyKeyFailClosedAllowsLoopbackOnly(t *testing.T) {
	SetAllowEmptyKey(false)
	t.Cleanup(func() { SetAllowEmptyKey(true) })

	loop := httptest.NewRequest("GET", "/", nil)
	loop.RemoteAddr = "127.0.0.1:54321"
	if !VerifyBearer(loop, "") {
		t.Error("回环 + 空 key 应放行（本机部署）")
	}
	if !VerifyDual(loop, "") {
		t.Error("回环 + 空 key 应放行 VerifyDual")
	}

	remote := httptest.NewRequest("GET", "/", nil)
	remote.RemoteAddr = "192.0.2.10:1234"
	if VerifyBearer(remote, "") {
		t.Error("非回环 + 空 key 必须拒绝")
	}
	// 伪造 X-Forwarded-For 不能把自己变成回环。
	remote.Header.Set("X-Forwarded-For", "127.0.0.1")
	if VerifyBearer(remote, "") {
		t.Error("X-Forwarded-For 是客户端可写头，不能作为回环判据")
	}

	// IPv6 回环同样要认。
	v6 := httptest.NewRequest("GET", "/", nil)
	v6.RemoteAddr = "[::1]:9999"
	if !VerifyBearer(v6, "") {
		t.Error("[::1] + 空 key 应放行")
	}
}
