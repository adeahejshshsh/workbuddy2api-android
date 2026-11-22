package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestChatBackendUnsupportedFailsFastWithoutPenalty 上游 400 + code 11103
// （"Backend [hunyuan-stream] is not supported"，issue #280 真机形态：客户端把生图
// 模型当 chat 协议模型发、或装了带上游不支持工具的 MCP）→ 请求级错误：不轮转、
// 不罚账号，直接 400 透传。
//
// 修复前的代价链：11103 不在 Classify 的请求级白名单里 → 落 ErrClient → 每个健康号
// 都被换上去打一遍（MaxRotate=3）+ 各喂一笔连败 → 5 次达阈把号降权出池 → 末端
// 503 no_healthy_account。用户看到的正是「上游明明说换模型就行，网关却报了整池不可用」。
func TestChatBackendUnsupportedFailsFastWithoutPenalty(t *testing.T) {
	const upstreamBody = `{"code":11103,"msg":"Backend [hunyuan-stream] is not supported","requestId":"01a1f2d8f5b2d75cc8d39a66c42193","retry_policy":{"retry":false},"displayMsg":{"zh":"模型配置异常，请切换其他模型"}}`
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 400, upstreamBody, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0)
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up})

	for i := 1; i <= 6; i++ {
		before := calls["Bearer at-bad"] + calls["Bearer at-good"]
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2"}`)))
		if got := calls["Bearer at-bad"] + calls["Bearer at-good"] - before; got != 1 {
			t.Fatalf("run %d: 上游调用 %d 次 want 1（不轮转）calls=%v", i, got, calls)
		}
		if rec.Code != 400 {
			t.Fatalf("run %d: code=%d body=%s (want 400 fail-fast)", i, rec.Code, rec.Body)
		}
		body := rec.Body.String()
		if strings.Contains(body, "no_healthy_account") {
			t.Errorf("run %d: 11103 不得落回 no_healthy_account: %s", i, body)
		}
		if !strings.Contains(body, "11103") {
			t.Errorf("run %d: 400 body 应透传上游 11103 原文: %s", i, body)
		}
		// hint 必须点名「换号无用」，否则客户端会以为换个号重试能过。
		if !strings.Contains(body, "switching accounts will not help") {
			t.Errorf("run %d: hint 应说明换号无用: %s", i, body)
		}
	}
	for _, uid := range []string{"bad", "good"} {
		st, _ := p.Status(uid)
		if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.BreakerFails != 0 ||
			st.ConsecutiveFails != 0 || !st.DegradeUntil.IsZero() {
			t.Errorf("%s: 11103 不得罚号: %+v", uid, st)
		}
	}
}
