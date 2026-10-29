// webagent_test.go 网页通道（console/as 云 agent 会话）的协议钉。
//
// 三个钉对应三条「写错就静默失效」的细节（见 webagent.go 文件头）：
//
//  1. ACP 的 Authorization 必须是 /session 返回的**沙箱 token**；
//     用账号 access token 会 401 `12801:鉴权失败`，而 SSE 那步照样 200——
//     最容易误判成「链路通了只是没输出」。
//  2. 网页通道用浏览器身份（Chromium UA + Origin/Referer /app + X-User-Id），
//     不是桌面端 RPC 指纹（WorkBuddy UA + X-Requested-With、无 Origin）。
//  3. 刚建会话时沙箱可能还没分配（link/token 为空），要短暂重试而不是判死。
package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

type acpFake struct {
	srv     *httptest.Server
	mu      sync.Mutex
	auth    []string
	ua      []string
	referer []string
	origin  []string
	methods []string
	bodies  []string
	sessHit int
	status  string
}

func newACPFake(t *testing.T) *acpFake {
	t.Helper()
	f := &acpFake{status: "completed"}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.ua = append(f.ua, r.Header.Get("User-Agent"))
		f.referer = append(f.referer, r.Header.Get("Referer"))
		f.origin = append(f.origin, r.Header.Get("Origin"))
		f.mu.Unlock()
		switch {
		case r.URL.Path == "/acp":
			if r.Method == http.MethodGet {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Acp-Connection-Id", "conn-1")
				w.WriteHeader(200)
				fmt.Fprint(w, "data: {\"method\":\"session/update\",\"params\":{\"update\":{\"sessionUpdate\":\"agent_message_chunk\"}}}\n\n")
				return
			}
			raw, _ := io.ReadAll(r.Body)
			var msg struct {
				Method string `json:"method"`
			}
			_ = json.Unmarshal(raw, &msg)
			f.mu.Lock()
			f.auth = append(f.auth, r.Header.Get("Authorization"))
			f.methods = append(f.methods, msg.Method)
			f.bodies = append(f.bodies, string(raw))
			f.mu.Unlock()
			w.WriteHeader(202)
			return
		case strings.HasSuffix(r.URL.Path, "/session"):
			f.mu.Lock()
			f.sessHit++
			hit := f.sessHit
			f.mu.Unlock()
			if hit == 1 {
				// 第一次：沙箱还没分配（真实时序里刚建会话时会这样）。
				fmt.Fprint(w, `{"code":0,"data":{"sessionId":"c1","cwd":"/workspace"}}`)
				return
			}
			fmt.Fprintf(w, `{"code":0,"data":{"link":%q,"token":"sandbox-token","sessionId":"c1","cwd":"/workspace"}}`, f.srv.URL+"/acp")
			return
		case r.URL.Path == "/console/as/conversations/":
			f.mu.Lock()
			f.bodies = append(f.bodies, readAllString(r.Body))
			f.mu.Unlock()
			fmt.Fprint(w, `{"code":0,"data":{"id":"c1"}}`)
			return
		default:
			f.mu.Lock()
			st := f.status
			f.mu.Unlock()
			fmt.Fprintf(w, `{"code":0,"data":{"status":%q}}`, st)
			return
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func readAllString(r io.Reader) string {
	raw, _ := io.ReadAll(r)
	return string(raw)
}

func webTestAuth() *auth.Auth {
	return &auth.Auth{UID: "u-web", AccessToken: "app-token", ExpiresAt: 9999999999}
}

// TestCreateWebConversationUsesWebIdentity 建会话必须用网页身份的请求头与请求体。
//
// 反向代价：混用桌面端指纹（WorkBuddy UA + X-Requested-With）会让上游把两个前端
// 的流量糊成一个，两边都不像——这是本仓库反复踩过的坑（见 headers.go 的注释史）。
func TestCreateWebConversationUsesWebIdentity(t *testing.T) {
	f := newACPFake(t)
	c := &Client{HTTP: f.srv.Client(), ChatBaseCN: f.srv.URL}
	a := webTestAuth()

	id, err := c.CreateWebConversation(context.Background(), a, "Hi", "deepseek-v4.1-flash")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id != "c1" {
		t.Fatalf("id=%q want c1", id)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.ua) == 0 || !strings.HasPrefix(f.ua[0], "Mozilla/5.0") {
		t.Errorf("UA=%q want 浏览器 UA（网页通道不是桌面指纹）", f.ua)
	}
	if f.origin[0] != f.srv.URL {
		t.Errorf("Origin=%q want %q", f.origin[0], f.srv.URL)
	}
	if f.referer[0] != f.srv.URL+"/app" {
		t.Errorf("Referer=%q want %q", f.referer[0], f.srv.URL+"/app")
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(f.bodies[0]), &body); err != nil {
		t.Fatalf("body parse: %v (%q)", err, f.bodies[0])
	}
	if body["conversationOrigin"] != webConversationOrigin {
		t.Errorf("conversationOrigin=%v want %s（桌面端自己建的是 legacy_workbuddy_local，"+
			"不是网页通道，不算每日活跃）", body["conversationOrigin"], webConversationOrigin)
	}
	if _, ok := body["plugins"]; !ok {
		t.Error("缺 plugins：网页端建会话固定带 weixinpay 内置插件")
	}
}

// TestRunWebAgentTurnUsesSandboxToken ACP 每一步都必须用沙箱 token 鉴权。
func TestRunWebAgentTurnUsesSandboxToken(t *testing.T) {
	f := newACPFake(t)
	c := &Client{HTTP: f.srv.Client(), ChatBaseCN: f.srv.URL}
	a := webTestAuth()

	sess, err := c.WebConversationSession(context.Background(), a, "c1")
	if err != nil {
		t.Fatalf("session（首次空 link 应重试成功）: %v", err)
	}
	if sess.Token != "sandbox-token" {
		t.Fatalf("token=%q", sess.Token)
	}
	res, err := c.RunWebAgentTurn(context.Background(), a, "c1", sess, "Hi", "deepseek-v4.1-flash")
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if res.Status != "completed" || res.Chunks == 0 {
		t.Fatalf("res=%+v want completed 且 chunks>0", res)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.auth) != 4 {
		t.Fatalf("ACP 请求数=%d want 4（initialize/load/set_model/prompt）: %v", len(f.auth), f.methods)
	}
	for i, got := range f.auth {
		if got != "Bearer sandbox-token" {
			t.Errorf("%s Authorization=%q want \"Bearer sandbox-token\"（账号 token 会 401 12801）", f.methods[i], got)
		}
	}
	want := []string{"initialize", "session/load", "session/set_model", "session/prompt"}
	for i, m := range want {
		if f.methods[i] != m {
			t.Errorf("第 %d 个 ACP 方法=%q want %q（顺序固定）", i+1, f.methods[i], m)
		}
	}
}

// TestWebConversationSessionRetriesSandbox 刚建会话时 link/token 可能为空：
// 必须重试到就绪，而不是当场判失败（真实时序里会话状态还是 CREATING）。
func TestWebConversationSessionRetriesSandbox(t *testing.T) {
	f := newACPFake(t)
	c := &Client{HTTP: f.srv.Client(), ChatBaseCN: f.srv.URL}
	if _, err := c.WebConversationSession(context.Background(), webTestAuth(), "c1"); err != nil {
		t.Fatalf("session: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sessHit < 2 {
		t.Fatalf("session 查询次数=%d want >=2（首次空 link 要重试）", f.sessHit)
	}
}

// TestRunWebAgentTurnFailsOnFailedStatus 会话状态 failed 时必须立刻返回错误，
// 不能一直等到超时（调度器是遍历型任务，卡住会拖住整轮）。
func TestRunWebAgentTurnFailsOnFailedStatus(t *testing.T) {
	f := newACPFake(t)
	f.status = "failed"
	c := &Client{HTTP: f.srv.Client(), ChatBaseCN: f.srv.URL}
	a := webTestAuth()
	sess, err := c.WebConversationSession(context.Background(), a, "c1")
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	_, err = c.RunWebAgentTurn(context.Background(), a, "c1", sess, "Hi", "deepseek-v4.1-flash")
	if err == nil {
		t.Fatal("status=failed 必须返回错误")
	}
	if !strings.Contains(err.Error(), "failed") {
		t.Errorf("错误信息应带上游状态: %v", err)
	}
}
