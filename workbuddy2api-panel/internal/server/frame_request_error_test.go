// frame_request_error_test.go 请求级错误以「HTTP 200 + 首帧 error 帧」出现时的处置
// （上游 ae44d73 的 server 半边，按本仓结构落）。
//
// 语义：错在请求本身（工具定义 11129/11152、请求体 11101、内容拦截、超长、图片、空段、
// 工具配对……）时，同一 body 换任何账号都是同一个拒绝。这类错误不该被记成账号故障，
// 也不该把会话粘性从健康号上摘下来（摘了下次重试就落到冷号，十万~几十万 token 的前缀
// 从头重算）。
//
// 首帧否决由 server 侧的 sseCommitWriter 实现（W2-08）：首个完整事件是 error 帧时
// 先扣住、零字节提交，handler 因此能回真实 HTTP 状态（见 first_frame_veto_test.go）。
// 本文件钉的是「已开流」形态下的账号处置语义：
//   - 请求级错误不记账号故障、不解绑粘性（与 HTTP ≥400 路径的 failKeepBinding 同口径）；
//   - 托管工具探测（probeOnce）不再为请求级错误逐个试号。
//
// 另有一条对照：账号级 error 帧（11130）照旧走 fail（解绑），两条路径不许被合并。
package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
)

// TestChatFirstFrameRequestErrorKeepsBinding 11129 工具定义不合规以首帧 error 帧到达：
// 只打一次上游（不轮转），粘性绑定保留，并且 error 帧带上点名 tools 的 gateway_hint
// ——客户端因此知道该改请求，而不是以为换个账号能成。
func TestChatFirstFrameRequestErrorKeepsBinding(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 200, "data: {\"error\":{\"message\":\"工具定义不合规\",\"code\":11129}}\n\ndata: [DONE]\n\n", true
	})
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"deep1", "deep2"} },
	})
	p := testPoolWith(
		&auth.Auth{UID: "deep1", AccessToken: "at-deep1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "deep2", AccessToken: "at-deep2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})
	sess.Bind("conv-9", "deep1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[],"metadata":{"conversation_id":"conv-9"}}`)))

	if calls["Bearer at-deep1"] != 1 || calls["Bearer at-deep2"] != 0 {
		t.Errorf("calls=%v want 只打粘性号一次（请求级错误不轮转）", calls)
	}
	if uid, ok := st.lastUID("conv-9"); !ok || uid != "deep1" {
		t.Errorf("会话绑应保留 deep1，got %q ok=%v", uid, ok)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "tool definition") {
		t.Errorf("error 帧应带点名 tools 的 gateway_hint（否则客户端只会重试）: %s", body)
	}
	if !strings.Contains(body, "工具定义不合规") {
		t.Errorf("error.message 应透传上游原文: %s", body)
	}
	for _, uid := range []string{"deep1", "deep2"} {
		if s, _ := p.Status(uid); s.Cooling || s.Disabled {
			t.Errorf("账号 %s 不该被罚（请求级错误与账号无关）: %+v", uid, s)
		}
	}
}

// TestChatFirstFrameAccountLevelErrorUnbinds 对照：不在请求级表里的首帧 error 帧
// （11130 落 ErrClient）照旧走 fail —— 解绑粘性，让下一轮重新分配。两条路径的差别
// 是本次改动的全部内容，合并了就说明分支没生效。
func TestChatFirstFrameAccountLevelErrorUnbinds(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, "data: {\"error\":{\"message\":\"something odd\",\"code\":11130}}\n\ndata: [DONE]\n\n", true
	})
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"deep1", "deep2"} },
	})
	p := testPoolWith(
		&auth.Auth{UID: "deep1", AccessToken: "at-deep1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "deep2", AccessToken: "at-deep2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})
	sess.Bind("conv-7", "deep1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[],"metadata":{"conversation_id":"conv-7"}}`)))
	if _, ok := st.lastUID("conv-7"); ok {
		t.Errorf("账号级 error 帧应解绑粘性（fail 路径），绑定仍在: %v", st.LoadBinds())
	}
	if body := rec.Body.String(); !strings.Contains(body, "something odd") {
		t.Errorf("错误原文应透传给客户端: %s", body)
	}
}

// TestProbeOnceStopsOnRequestLevelError 托管工具探测体错在请求本身（11129 工具定义不合规、
// 11128 内容拦截等）时换号必然同样被拒，不再逐个试满 webSearchPickTries 个账号；
// 账号级失败（5xx）仍照常换号。
func TestProbeOnceStopsOnRequestLevelError(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		wantCalls  int
	}{
		{"11129 工具定义", `{"code":11129,"msg":"invalid function call parameters"}`, 400, 1},
		{"11152 工具名", `{"code":11152,"msg":"the tool name is invalid or duplicated"}`, 400, 1},
		{"11128 内容拦截", `{"code":11128,"msg":"blocked by security policy"}`, 400, 1},
		{"5xx 照常换号", `{"code":500,"msg":"boom"}`, 500, webSearchPickTries},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			calls := 0
			up := newFakeUpstream(t, func(string) (int, string, bool) {
				mu.Lock()
				calls++
				mu.Unlock()
				return tc.status, tc.body, false
			})
			p := testPoolWith(
				&auth.Auth{UID: "a", AccessToken: "at-a", ExpiresAt: 9999999999},
				&auth.Auth{UID: "b", AccessToken: "at-b", ExpiresAt: 9999999999},
				&auth.Auth{UID: "c", AccessToken: "at-c", ExpiresAt: 9999999999},
			)
			h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
			if _, err := h.probeOnce(context.Background(), "cn", "glm-5.2",
				[]byte(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)); err == nil {
				t.Fatal("探测应失败")
			}
			mu.Lock()
			got := calls
			mu.Unlock()
			if got != tc.wantCalls {
				t.Errorf("上游被调用 %d 次, want %d（请求级错误不该试满 %d 个号）", got, tc.wantCalls, webSearchPickTries)
			}
		})
	}
}

// TestRequestErrorCodesCoversHTTPBranch 请求级表的每一格都必须与 HTTP ≥400 分支的即刻
// 400 同码：同一个 Kind 在「HTTP 400」与「200 首帧」两种到达方式下给出不同 error.code，
// 客户端按码分支的逻辑就会走出两套行为（一条可重试、一条不可重试）。
func TestRequestErrorCodesCoversHTTPBranch(t *testing.T) {
	for _, k := range []string{
		"content_blocked", "prompt_too_long", "image_invalid", "tool_call_mismatch",
		"bad_params", "model_param_invalid", "empty_content", "model_unavailable",
	} {
		found := false
		for _, code := range requestErrorCodes {
			if code == k {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("请求级表缺 %s（HTTP ≥400 分支会即刻 400，首帧路径却当账号故障）", k)
		}
	}
	if len(requestErrorCodes) != 8 {
		t.Errorf("requestErrorCodes 有 %d 项，want 8（新增项须同时确认 HTTP 分支口径）", len(requestErrorCodes))
	}
}

// TestChatHTTP400ToolSchemaRejected 11129 走 HTTP 400（上游最常见形态，不是首帧）：
// 立即回 400 bad_params + 点名 tools 的 gateway_hint，只打一次上游、不罚号、
// 连败计数不动。
//
// 这条是本轮问题的正面回归：此前 11129 落 ErrClient → 逐号轮转 → 每个健康号记一笔
// 连败（ConsecutiveFails++）→ 达 degrade_threshold 后整池被连败降权 → 后续请求全 503。
// 「分类改了但 schema 没修」会在这里表现为上游照旧 400（换个用例），
// 「schema 修了但分类没改」则在这里表现为 calls==3 + ConsecutiveFails>0。
func TestChatHTTP400ToolSchemaRejected(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 400, `{"code":11129,"msg":"invalid function call parameters","displayMsg":{"zh":"工具定义不合规"}}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "a", AccessToken: "at-a", ExpiresAt: 9999999999},
		&auth.Auth{UID: "b", AccessToken: "at-b", ExpiresAt: 9999999999},
		&auth.Auth{UID: "c", AccessToken: "at-c", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":{}}}]}`)))

	if rec.Code != 400 {
		t.Fatalf("code=%d body=%s, want 400（请求级错误不该伪装成 503）", rec.Code, rec.Body)
	}
	var env struct {
		Error struct {
			Code        string `json:"code"`
			Message     string `json:"message"`
			GatewayHint string `json:"gateway_hint"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("响应不是 JSON 错误信封: %v: %s", err, rec.Body)
	}
	if env.Error.Code != "bad_params" {
		t.Errorf("error.code=%q, want bad_params", env.Error.Code)
	}
	if !strings.Contains(env.Error.Message, "11129") {
		t.Errorf("error.message 应透传上游原文: %q", env.Error.Message)
	}
	if !strings.Contains(env.Error.GatewayHint, "tool definition") {
		t.Errorf("gateway_hint 应点名 tools（客户端才知道改哪儿）: %q", env.Error.GatewayHint)
	}
	if calls != 1 {
		t.Errorf("上游被调用 %d 次, want 1（请求级错误不轮转）", calls)
	}
	for _, uid := range []string{"a", "b", "c"} {
		st, _ := p.Status(uid)
		if st.Cooling || st.Disabled {
			t.Errorf("账号 %s 不该被罚: %+v", uid, st)
		}
		if st.ConsecutiveFails != 0 {
			t.Errorf("账号 %s 连败计数=%d，want 0（请求级错误不许喂连败降权）", uid, st.ConsecutiveFails)
		}
	}
}
