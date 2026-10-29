// webactivity_test.go 网页通道每日活跃腿的回归钉。
//
// 这条腿的存在理由（issue #111 实测）：国际版每日 30/50 积分只认「跑完的网页
// agent 会话」。三条钉分别管：
//
//	CN 账号零调用     —— 国内版没这套机制，建云会话纯属白花钱
//	建/接/跑/收尾全链 —— 少任一步（尤其不接沙箱、不 stop）都会静默失效
//	当日去重          —— activity_hours 多时点时不重复建会话（单号活跃上限 10 条）
package scheduler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// webAgentFake 一个最小的 console/as + ACP 假上游：会话状态直接回 completed。
// 返回的 srv 与统计量供断言。
type webAgentFake struct {
	srv      *httptest.Server
	mu       sync.Mutex
	paths    []string
	acpAuth  []string // 每次 ACP POST 的 Authorization 头
	statuses int32
}

func newWebAgentFake(t *testing.T) *webAgentFake {
	t.Helper()
	f := &webAgentFake{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.mu.Unlock()

		switch {
		case r.URL.Path == "/acp":
			if r.Method == http.MethodGet {
				// SSE 通道：先给一个连接 id，再吐一条 session/update 后收摊。
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Acp-Connection-Id", "conn-1")
				w.WriteHeader(200)
				fmt.Fprint(w, "data: {\"method\":\"session/update\",\"params\":{\"update\":{\"sessionUpdate\":\"agent_message_chunk\"}}}\n\n")
				if fl, ok := w.(http.Flusher); ok {
					fl.Flush()
				}
				return
			}
			f.mu.Lock()
			f.acpAuth = append(f.acpAuth, r.Header.Get("Authorization"))
			f.mu.Unlock()
			if r.Header.Get("Acp-Connection-Id") != "conn-1" {
				w.WriteHeader(400)
				return
			}
			w.WriteHeader(202)
			return
		case strings.HasPrefix(r.URL.Path, "/console/as/conversations/"):
			switch {
			case strings.HasSuffix(r.URL.Path, "/session"):
				fmt.Fprintf(w, `{"code":0,"data":{"link":%q,"token":"sandbox-token","sessionId":"c1","cwd":"/workspace"}}`, f.srv.URL+"/acp")
			case strings.HasSuffix(r.URL.Path, "/stop"), strings.HasSuffix(r.URL.Path, "/delete"):
				fmt.Fprint(w, `{"code":0,"data":{}}`)
			case r.URL.Path == "/console/as/conversations/":
				fmt.Fprint(w, `{"code":0,"data":{"id":"c1"}}`)
			default:
				atomic.AddInt32(&f.statuses, 1)
				fmt.Fprint(w, `{"code":0,"data":{"status":"completed"}}`)
			}
			return
		}
		fmt.Fprint(w, `{"code":0,"data":{}}`)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *webAgentFake) sawPath(suffix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.paths {
		if strings.HasSuffix(p, suffix) {
			return true
		}
	}
	return false
}

// TestWebActivityCNMakesNoCall 国内号不跑网页腿：零上游调用。
//
// 反向代价：漏了这道门控，每个 CN 账号每天都会建一条云会话（上游真实计费资源），
// 而 CN 侧的每日活跃根本不看网页会话。
func TestWebActivityCNMakesNoCall(t *testing.T) {
	f := newWebAgentFake(t)
	p := pool.New("")
	a := &auth.Auth{UID: "c1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	p.Add(a)
	up := &upstream.Client{HTTP: f.srv.Client(), ChatBaseCN: f.srv.URL, BillingBaseCN: f.srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.runWebActivityLeg(context.Background(), a)

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.paths) != 0 {
		t.Fatalf("国内号不该发起任何网页通道调用: %v", f.paths)
	}
}

// TestWebActivityGlobalRunsFullChain 国际号必须走完 建会话 → 取沙箱 → ACP → 轮询 → stop。
//
// 少任何一步的真实后果：
//   - 不取沙箱/不 POST ACP：会话永远停在 working，零输出，不算有效对话（issue #90）；
//   - 不 stop：会话一直占活跃名额，攒满 10 条后当天起再也建不出会话（429/10105）。
func TestWebActivityGlobalRunsFullChain(t *testing.T) {
	withGlobalSwitch(t)
	f := newWebAgentFake(t)
	p := pool.New("")
	a := globalAccount(t, "g1")
	p.Add(a)
	up := &upstream.Client{HTTP: f.srv.Client(), GlobalEnabled: true,
		ChatBaseCN: f.srv.URL, BillingBaseCN: f.srv.URL,
		ChatBaseGlobal: f.srv.URL, BillingBaseGlobal: f.srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.runWebActivityLeg(context.Background(), a)

	for _, want := range []string{"/console/as/conversations/", "/session", "/stop"} {
		if !f.sawPath(want) {
			t.Errorf("缺少 %s 调用，链路不完整: %v", want, f.paths)
		}
	}
	if atomic.LoadInt32(&f.statuses) == 0 {
		t.Error("必须轮询会话状态（status 是「跑完没有」的唯一判据）")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.acpAuth) == 0 {
		t.Fatal("没有发 ACP 请求")
	}
	// 12801 鉴权失败那课：ACP 必须用沙箱 token，不是账号 access token。
	for _, got := range f.acpAuth {
		if got != "Bearer sandbox-token" {
			t.Errorf("ACP Authorization=%q want \"Bearer sandbox-token\"（用账号 token 会 401 12801）", got)
		}
	}
}

// TestWebActivityGlobalOncePerDay 同一天第二次调用不再建会话（activity_hours 多时点时）。
func TestWebActivityGlobalOncePerDay(t *testing.T) {
	withGlobalSwitch(t)
	f := newWebAgentFake(t)
	p := pool.New("")
	a := globalAccount(t, "g1")
	p.Add(a)
	up := &upstream.Client{HTTP: f.srv.Client(), GlobalEnabled: true,
		ChatBaseCN: f.srv.URL, BillingBaseCN: f.srv.URL,
		ChatBaseGlobal: f.srv.URL, BillingBaseGlobal: f.srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.runWebActivityLeg(context.Background(), a)
	first := len(f.paths)
	if first == 0 {
		t.Fatal("首轮没有发起调用")
	}
	s.runWebActivityLeg(context.Background(), a)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.paths) != first {
		t.Fatalf("当日第二轮不该再建会话（首轮 %d 次调用，现在 %d 次）: %v", first, len(f.paths), f.paths)
	}
}
