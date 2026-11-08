package scheduler

// 国际版「网页通道」每日活跃腿（issue #111）的开关（schedule.web_activity_enabled，缺省关）。
//
// 为什么缺省关：上游实测每日 30 积分的判据是活跃上报本身，网页会话不是必要条件；
// 而这条腿每天为每个国际号建一条真实会话、占一条活跃名额（上限 10，漏收尾永久少一条）。
// 开关错成「常开」时用户看到的是：积分没多、活跃名额悄悄变少。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func TestRunActivityNowSkipsWebLegWhenDisabled(t *testing.T) {
	fastActivity(t)
	var webCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/console/as/conversations") {
			webCalls.Add(1)
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
	}))
	defer srv.Close()

	p := pool.New("")
	// global 号：网页腿只对它生效（cn 号在 runWebActivityLeg 内部直接返回）。
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		Domain: "https://api.workbuddy.ai"})
	s := New(Config{
		Pool:                p,
		Upstream:            &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL},
		WebActivityDisabled: true,
	})
	s.RunActivityNow()
	if n := webCalls.Load(); n != 0 {
		t.Fatalf("开关关闭时仍建了 %d 条网页会话（活跃名额会被白白吃掉）", n)
	}

	// 热改打开：下一轮就该尝试建会话（这里只断言「发出了请求」，不模拟完整 ACP 链路）。
	s.SetWebActivityDisabled(false)
	s.RunActivityNow()
	if n := webCalls.Load(); n == 0 {
		t.Fatal("开关打开后应尝试建网页会话")
	}
}
