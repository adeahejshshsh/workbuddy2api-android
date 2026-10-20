package panel

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// bug #1 回归：这些 goroutine 不是 net/http 起的，Go 不会替它们 recover ——
// 任务链里大量对上游响应的弱类型解析，一旦 panic 死的是**整个网关进程**，
// 而不是"这次任务失败"。这里用一个必然 panic 的 upstream（返回非法形状）验证
// refreshCreditsAsync 会自己 recover，不会把测试进程带走。
func TestRefreshCreditsAsyncRecoversPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 200 + 非 JSON：让上游解析路径走到异常分支（真实场景里就是形状变了）
		w.Write([]byte("not-json-at-all"))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	pn := &Panel{cfg: Config{
		Pool:     p,
		Upstream: &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL},
	}}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// 若内部没有 recover，这里会 panic 并直接打死测试进程（测试框架会报 fatal）
		pn.refreshCreditsAsync(p.AuthByUID("u1"))
	}()
	wg.Wait()
	// 再等一小会让 goroutine 收尾（refreshCreditsAsync 内部另起 goroutine）
	done := make(chan struct{})
	go func() { defer close(done); pn.refreshCreditsAsync(p.AuthByUID("u1")) }()
	<-done
}
