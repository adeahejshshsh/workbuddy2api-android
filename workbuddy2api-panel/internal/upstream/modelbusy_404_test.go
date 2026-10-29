package upstream

// 国际版没有 POST /v2/chat/busy（上游 2026-10-29 `09f2eeb`，5.9.3 真机 158 条 flow：
// 10 次全 404，同一 token 直连复验也 404，国内版同路径 200 —— 官方客户端自己也在打
// 这条不存在的路径，是服务端没部署）。
//
// 繁忙度是**纯观测**数据：拿到 404 既不该每轮重试，也不该让上层把「观测失败」记成
// 错误（面板会把 busy 失败并进目录错误列表，进而拒绝写目录缓存）。

import (
	"io"
	"net/http"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestModelBusyGlobal404RemembersUnsupported(t *testing.T) {
	calls := 0
	c := &Client{GlobalEnabled: true, HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(stringsReader(`{"error_msg":"404 Route Not Found"}`)),
			Request:    r,
		}, nil
	})}}
	global := &auth.Auth{UID: "u1", AccessToken: "at", Domain: "www.workbuddy.ai"}

	out, err := c.ModelBusy(global, []string{"m1"})
	if err != nil {
		t.Fatalf("global 404 不该报错（繁忙度是纯观测）: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("应返回空表，got %v", out)
	}
	if !c.modelBusyUnsupported.Load() {
		t.Fatal("应记住该域无此端点")
	}
	// 第二次：直接短路，不再打上游（每轮刷新都打 = 稳定制造 404 流量）。
	if _, err := c.ModelBusy(global, []string{"m2"}); err != nil || calls != 1 {
		t.Fatalf("第二次应短路：calls=%d err=%v", calls, err)
	}

	// 隔离：CN 域不吃这个记忆，404 仍按错误上抛。
	calls = 0
	cn := &auth.Auth{UID: "u2", AccessToken: "at", Domain: "copilot.tencent.com"}
	if _, err := c.ModelBusy(cn, []string{"m1"}); err == nil {
		t.Fatal("CN 域 404 不该被吞成成功")
	}
	if calls != 1 {
		t.Fatalf("CN 域应真的打了一次，calls=%d", calls)
	}
}

// stringsReader 避免额外 import strings 只为一行。
func stringsReader(s string) io.Reader { return &strReader{s: s} }

type strReader struct {
	s string
	i int
}

func (r *strReader) Read(p []byte) (int, error) {
	if r.i >= len(r.s) {
		return 0, io.EOF
	}
	n := copy(p, r.s[r.i:])
	r.i += n
	return n, nil
}
