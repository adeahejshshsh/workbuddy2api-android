// 只发 user_id 的客户端（ExtractKey 的 anti-monopoly 闸返回空）此前每轮都换号，
// 而上游前缀缓存按账号隔离（cache_key 掺 uid），换号 = 全量 miss。实测同一份
// 3.6k body 连发两次：只带 user_id 时落两个不同账号、miss 3634/3634；带
// conversation_id 或什么都不带时第二轮命中 3456/3634（95%）。
//
// 这里钉的是「绑定键换成内容派生键」这一步本身，不依赖轮换随机性：ExtractKey
// 仍返回空（契约不变），但绑定落在 DeriveCacheKey(body) 上。
package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
)

func TestChatStickyBindsUserIDOnlyClientByDerivedKey(t *testing.T) {
	const body = `{"model":"glm-5.2","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}],"metadata":{"user_id":"u-1"}}`
	if k := session.ExtractKey([]byte(body)); k != "" {
		t.Fatalf("anti-monopoly 闸应仍返回空，got %q", k)
	}
	derived := session.DeriveCacheKey([]byte(body))
	if derived == "" {
		t.Fatal("派生键不该为空")
	}

	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"a", "b"} },
	})
	p := testPoolWith(
		&auth.Auth{UID: "a", AccessToken: "at-a", ExpiresAt: 9999999999},
		&auth.Auth{UID: "b", AccessToken: "at-b", ExpiresAt: 9999999999},
	)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
		if rec.Code != 200 {
			t.Fatalf("第 %d 次 code=%d body=%s", i+1, rec.Code, rec.Body)
		}
	}

	binds := st.LoadBinds()
	if len(binds) != 1 {
		t.Fatalf("应只有一条绑定（派生键），got %v", binds)
	}
	if uid, ok := binds[derived]; !ok || (uid != "a" && uid != "b") {
		t.Fatalf("绑定键应为派生键 %q，got %v", derived, binds)
	}
}
