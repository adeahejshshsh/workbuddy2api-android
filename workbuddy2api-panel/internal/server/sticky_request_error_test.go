package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
)

// 请求级错误（内容拦截 / prompt 超长 / 图片无效 / 工具不匹配）不能解绑会话粘性。
//
// 线上现象（用户反馈）：报错一次之后缓存就一直不生效。原因是这些错误与账号无关
// （同一个 body 换任何号都会撞），但此前走的是统一的 fail()，它会把会话解绑；下一轮
// 客户端重试时被分配到另一个号上，十万~几十万 token 的前缀从头重算。上游
// content_blocked 成片出现时更会连环换号，每一跳都是全量 miss。
//
// 断言方式：用记录绑定表的假 Store（newBindStore）直接看「解绑有没有写进存储」，
// 而不是只看内存里的 Resolve——后者在账号都被租用时本来就可能为空，测不出差异
// （第一版就这么漏掉了变异）。
func TestRequestLevelErrorKeepsSessionBinding(t *testing.T) {
	const body = `{"model":"cn:deepseek-v4.1-flash","conversation_id":"conv-keep",` +
		`"messages":[{"role":"user","content":"hi"}]}`
	key := session.ExtractKey([]byte(body))
	if key == "" {
		t.Fatal("测试前提失败：body 应能提取出会话键")
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
		return 400, `{"code":"11-128","msg":"Illegal API invocation from an unapproved channel"}`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})

	// 会话已经粘在 a 上（那个号上的前缀缓存是热的）。
	sess.Bind(key, "a")
	if uid, ok := st.LoadBinds()[key]; !ok || uid != "a" {
		t.Fatalf("前置绑定没写进存储: %v", st.LoadBinds())
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != 400 {
		t.Fatalf("内容拦截应回 400，实际 %d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "11-128") {
		t.Fatalf("应透传上游原文:\n%s", rec.Body.String())
	}

	binds := st.LoadBinds()
	if uid, ok := binds[key]; !ok || uid != "a" {
		t.Fatalf("请求级错误不该解绑会话（客户端重试会落到冷号上全量重算）：binds=%v", binds)
	}
}
