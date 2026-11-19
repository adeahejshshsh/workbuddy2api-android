// egress_header_safe_test.go W1-06 回归：身份头必须在唯一出站口被清洗。
//
// 缺陷：X-User-Id / X-Enterprise-Id / X-Domain 十余处 Set 都没过 HeaderSafe。
// 这三个值来自账号落盘数据（uid / 企业号 / domain 可由导入的 auths/*.json 提供），
// 含 CRLF 时 net/http 拒绝整个请求 → 每次出站都失败 → 被记成上游故障 →
// 单号 5 次降权出池。一个畸形字段就能把好号耗死。
package upstream

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPrepareOutboundSanitizesIdentityHeaders(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://example.invalid/v2/chat/completions", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-User-Id", "u1\r\nX-Injected: evil")
	req.Header.Set("X-Enterprise-Id", "e1\nX-Injected2: evil")
	req.Header.Set("X-Domain", "d1\r\n")

	prepareOutbound(req)

	for _, k := range []string{"X-User-Id", "X-Enterprise-Id", "X-Domain"} {
		if v := req.Header.Get(k); strings.ContainsAny(v, "\r\n") {
			t.Fatalf("%s 仍含 CR/LF：%q", k, v)
		}
	}
	// 注入出来的新头不能凭空出现。
	if req.Header.Get("X-Injected") != "" || req.Header.Get("X-Injected2") != "" {
		t.Fatalf("CRLF 注入出了新头：%v", req.Header)
	}
	// 可打印内容必须逐字保留（只剥控制字符，不改语义）。
	if got := req.Header.Get("X-User-Id"); got != "u1X-Injected: evil" {
		t.Fatalf("清洗后内容变了：%q", got)
	}
}

// TestEgressWithCRLFIdentityHeaderStillSends 端到端：畸形身份头不能再让整条出站失败。
func TestEgressWithCRLFIdentityHeaderStillSends(t *testing.T) {
	var gotUID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUID = r.Header.Get("X-User-Id")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client()}
	req, err := http.NewRequest(http.MethodPost, srv.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-User-Id", "uid\r\nX-Injected: 1")

	resp, err := c.do(req)
	if err != nil {
		t.Fatalf("畸形身份头导致整条出站失败（W1-06 的原始症状）：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if strings.ContainsAny(gotUID, "\r\n") {
		t.Fatalf("服务端收到未清洗的头：%q", gotUID)
	}
}
