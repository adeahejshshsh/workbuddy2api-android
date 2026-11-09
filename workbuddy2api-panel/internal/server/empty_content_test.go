package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestChatEmptyContentFailsFastWithoutPenalty 上游 11151（a message has empty content）
// 是**请求内容**级错误：历史里某个 text 段是空串/缺失，同一 body 换任何账号都会被同
// 一处拒绝。出站前 empty_text_part.go 会补位，这里钉的是补位漏网时的兜底：
//
//   - 只打一次上游（不轮转全池，否则 MaxRotate 次白打 + 每号一笔连败）；
//   - 末端 400 + 上游原文透传（不能报成 503「账号不可用」，那会让客户端对必然失败的
//     请求无限重试）；
//   - 账号零惩罚（不冷却/不熔断/不加 err_total/不喂连败）。
func TestChatEmptyContentFailsFastWithoutPenalty(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 400, `{"code":11151,"msg":"a message has empty content","requestId":"eee111511"}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "ec1", AccessToken: "at-ec1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "ec2", AccessToken: "at-ec2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != 400 {
		t.Fatalf("code=%d body=%s (want 400 fail-fast，非 503)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "11151") {
		t.Errorf("400 body 应透传上游原文: %s", rec.Body)
	}
	// 不钉「打的是 ec1」：选号按权重加权随机，整包连跑时全局随机源状态不同，
	// 先选中 ec2 是合法结果（upstream 的用例钉死账号，在本仓会偶发假红）。
	total := calls["Bearer at-ec1"] + calls["Bearer at-ec2"]
	if total != 1 {
		t.Errorf("calls=%v want 共 1 次上游（fail-fast 不轮转）", calls)
	}
	for _, uid := range []string{"ec1", "ec2"} {
		st, _ := p.Status(uid)
		if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.BreakerFails != 0 || st.ConsecutiveFails != 0 {
			t.Errorf("ErrEmptyContent 不得惩罚账号（%s）: %+v", uid, st)
		}
	}
}

// bodySinkUpstream 记录上游**实际收到的 body**：空段补位必须发生在出站序列化之前，
// 只断言客户端输入或只断言响应都证明不了这一点。
type bodySinkUpstream struct {
	mu   sync.Mutex
	body string
}

func (s *bodySinkUpstream) client() *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			s.mu.Lock()
			s.body = string(raw)
			s.mu.Unlock()
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

// TestChatEmptyTextPartRepairedBeforeUpstream 与上一条互补：客户端历史里真的带着空
// text 段时，网关应当就地补位、请求照常成功——客户端不该因为自己历史里有个空段
// 就永久卡死在 400（这条钉「修好」，上一条钉「报清楚」）。
func TestChatEmptyTextPartRepairedBeforeUpstream(t *testing.T) {
	for _, model := range []string{"glm-5.2", "deepseek-v4.1-flash"} {
		t.Run(model, func(t *testing.T) {
			sink := &bodySinkUpstream{}
			p := testPoolWith(&auth.Auth{UID: "ec3", AccessToken: "at-ec3", ExpiresAt: 9999999999})
			h := NewHandler(Config{Pool: p, Upstream: sink.client()})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
				`{"model":"`+model+`","messages":[{"role":"user","content":[{"type":"text","text":""}]}]}`)))

			if rec.Code != 200 {
				t.Fatalf("status=%d body=%s（空 text 段应被补位后照常成功）", rec.Code, rec.Body)
			}
			var obj map[string]any
			if err := json.Unmarshal([]byte(sink.body), &obj); err != nil {
				t.Fatalf("上游收到的 body 不是 JSON: %v (%s)", err, sink.body)
			}
			// 按角色找用户消息：出站前可能被补了一条 fallback system（是既有行为），
			// 用下标 0 会读到那条 system 的字符串 content。
			var userMsg map[string]any
			for _, raw := range obj["messages"].([]any) {
				if m, ok := raw.(map[string]any); ok && m["role"] == "user" {
					userMsg = m
					break
				}
			}
			if userMsg == nil {
				t.Fatalf("上游 body 里没有 user 消息: %s", sink.body)
			}
			parts, ok := userMsg["content"].([]any)
			if !ok {
				t.Fatalf("user 的 content 应保持数组形态（段被吞了？）: %#v", userMsg["content"])
			}
			if len(parts) != 1 {
				t.Fatalf("段数变了（删段会改段落下标）：%d", len(parts))
			}
			if got := parts[0].(map[string]any)["text"]; got != " " {
				t.Fatalf("上游收到的空 text 段未补位：%#v", got)
			}
		})
	}
}
