package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// newKeepaliveSched 构造带单个账号 + 可控上游的调度器，返回 (s, p, a, refreshCalls)。
// expiresAt: 账号的 ExpiresAt；window: 周期保活临期窗口（0 = 不传，走 New 的默认）。
func newKeepaliveSched(t *testing.T, expiresAt int64, window time.Duration) (*Scheduler, *pool.Pool, *auth.Auth, *atomic.Int32) {
	t.Helper()
	var refreshCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls.Add(1)
		w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
	}))
	t.Cleanup(srv.Close)

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: expiresAt}
	p.Add(a)

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up, KeepaliveRefreshWindow: window})
	return s, p, a, &refreshCalls
}

// scheduledKeepalive 走排程路径（selective=true），与 runBatch 里 taskKeepalive 的分支一致。
func scheduledKeepalive(s *Scheduler) { s.runKeepalive(context.Background(), false, true) }

// TestScheduledKeepaliveSkipsFreshToken 排程保活只刷新临期窗口内的账号：
// token 还很新鲜（距过期 30 天，窗口 7 天）→ 排程路径不刷新；手动入口照常全量。
func TestScheduledKeepaliveSkipsFreshToken(t *testing.T) {
	fresh := time.Now().Add(30 * 24 * time.Hour).Unix()
	s, _, a, calls := newKeepaliveSched(t, fresh, 7*24*time.Hour)

	scheduledKeepalive(s)
	if calls.Load() != 0 {
		t.Fatalf("临期窗口外账号不应被排程保活刷新: calls=%d", calls.Load())
	}
	if a.AccessToken != "old" {
		t.Errorf("token 不应被改动: %q", a.AccessToken)
	}

	// 手动全量保活语义不变：同一账号立即刷新。
	s.RunKeepaliveNow()
	if calls.Load() != 1 {
		t.Errorf("手动保活应全量刷新: calls=%d", calls.Load())
	}
	if a.AccessToken != "new" {
		t.Errorf("手动保活后 token 应更新: %q", a.AccessToken)
	}
}

// TestScheduledKeepaliveRefreshesWithinWindow 窗口内（剩余 3 天 < 窗口 7 天）→ 排程刷新。
func TestScheduledKeepaliveRefreshesWithinWindow(t *testing.T) {
	soon := time.Now().Add(3 * 24 * time.Hour).Unix()
	s, _, a, calls := newKeepaliveSched(t, soon, 7*24*time.Hour)

	scheduledKeepalive(s)
	if calls.Load() != 1 {
		t.Fatalf("窗口内账号应被刷新: calls=%d", calls.Load())
	}
	if a.AccessToken != "new" {
		t.Errorf("token 应更新: %q", a.AccessToken)
	}
}

// TestScheduledKeepaliveExpiryMissingOrExpired 无 expiry / 已过期的账号必须被排程刷新
// （NeedsRefresh 的兜底语义，避免「不知道何时过期」的号被窗口漏掉）。
func TestScheduledKeepaliveExpiryMissingOrExpired(t *testing.T) {
	for name, exp := range map[string]int64{
		"missing":  0,
		"negative": -1,
		"expired":  time.Now().Add(-time.Hour).Unix(),
	} {
		t.Run(name, func(t *testing.T) {
			s, _, a, calls := newKeepaliveSched(t, exp, 7*24*time.Hour)
			scheduledKeepalive(s)
			if calls.Load() != 1 {
				t.Errorf("expiry=%d 应被排程刷新: calls=%d", exp, calls.Load())
			}
			if a.AccessToken != "new" {
				t.Errorf("token 应更新: %q", a.AccessToken)
			}
		})
	}
}

// TestScheduledKeepaliveNegativeWindowNoFilter 负窗口 = 不过滤（退化为全量，与手动同语义）。
func TestScheduledKeepaliveNegativeWindowNoFilter(t *testing.T) {
	fresh := time.Now().Add(30 * 24 * time.Hour).Unix()
	s, _, _, calls := newKeepaliveSched(t, fresh, -1)

	scheduledKeepalive(s)
	if calls.Load() != 1 {
		t.Errorf("负窗口应全量刷新: calls=%d", calls.Load())
	}
}

// TestScheduledKeepaliveDefaultWindowWhenUnset 未配置窗口（0）时 New 回落 7 天，
// 且新鲜账号被排程路径跳过（证明默认窗口真的生效）。
func TestScheduledKeepaliveDefaultWindowWhenUnset(t *testing.T) {
	s, _, _, calls := newKeepaliveSched(t, time.Now().Add(30*24*time.Hour).Unix(), 0)
	if got := s.keepaliveRefreshWindow(); got != 7*24*time.Hour {
		t.Fatalf("未配置时窗口应回落 7 天: got=%v", got)
	}
	scheduledKeepalive(s)
	if calls.Load() != 0 {
		t.Errorf("默认窗口下新鲜账号不应刷新: calls=%d", calls.Load())
	}
}

// TestSetKeepaliveRefreshWindowHot 热更新窗口立即生效（面板保存配置后的路径）。
func TestSetKeepaliveRefreshWindowHot(t *testing.T) {
	fresh := time.Now().Add(3 * 24 * time.Hour).Unix()
	s, _, _, calls := newKeepaliveSched(t, fresh, 7*24*time.Hour)
	s.SetKeepaliveRefreshWindow(24 * time.Hour) // 剩余 3 天 > 1 天 → 不再刷新
	scheduledKeepalive(s)
	if calls.Load() != 0 {
		t.Errorf("窗口收窄后不应刷新: calls=%d", calls.Load())
	}
}
