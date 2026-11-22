package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestChatModelMissingFailsFastWithoutPenalty 上游 400 + code 11102 / 404「service info
// not found」（issue #280 第二条评论）：模型 id 在上游不存在时，换遍全池都是同一句——
// 修复前每个号白打一次（MaxRotate）+ 各写一笔 (账号,模型) 负缓存，一轮下来整池该模型
// 被锁 2 分钟，末端还回「model_unavailable + 建议换号重试」这种没用的话。
//
// 真号实测（线上号池，display name vs id）：
//
//	DeepSeek-V4.1-Flash      → 400 model_unavailable, 4.15s（三个号全 11102）
//	cn:deepseek-v4.1-flash   → 200, 0.67s
//
// 模型 id 大小写敏感，客户端拿 /v1/models 的 display name 当 id 发就会撞这个。
func TestChatModelMissingFailsFastWithoutPenalty(t *testing.T) {
	const upstreamBody = `{"code":11102,"msg":"model [DeepSeek-V4.1-Flash] service info not found","requestId":"01a1fbd8ad008cc0a46ab499d34a54f1","retry_policy":{"retry":false},"displayMsg":{"zh":"当前模型不可用，请切换模型"}}`
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

	for i := 1; i <= 3; i++ {
		before := calls["Bearer at-bad"] + calls["Bearer at-good"]
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"DeepSeek-V4.1-Flash","messages":[{"role":"user","content":"hi"}]}`)))
		if got := calls["Bearer at-bad"] + calls["Bearer at-good"] - before; got != 1 {
			t.Fatalf("run %d: 上游调用 %d 次 want 1（模型 id 不存在，换号无用）calls=%v", i, got, calls)
		}
		if rec.Code != 400 {
			t.Fatalf("run %d: code=%d body=%s (want 400 fail-fast)", i, rec.Code, rec.Body)
		}
		body := rec.Body.String()
		if strings.Contains(body, "no_healthy_account") {
			t.Errorf("run %d: 11102 不得落回 no_healthy_account: %s", i, body)
		}
		if !strings.Contains(body, "11102") {
			t.Errorf("run %d: 400 body 应透传上游 11102 原文: %s", i, body)
		}
		// hint 必须点名「换号无用」：修复前那句「retry on another account」正是 issue
		// 报告的行为反差来源。
		if !strings.Contains(body, "switching accounts will not help") {
			t.Errorf("run %d: hint 应说明换号无用: %s", i, body)
		}
	}
	for _, uid := range []string{"bad", "good"} {
		st, _ := p.Status(uid)
		if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.BreakerFails != 0 ||
			st.ConsecutiveFails != 0 || !st.DegradeUntil.IsZero() {
			t.Errorf("%s: 11102 不得罚号: %+v", uid, st)
		}
		// 也不该写 (账号,模型) 负缓存：模型级错误刷进「模型锁池」只会误导运维。
		if blk := p.ModelBlocked("DeepSeek-V4.1-Flash"); blk.Blocked {
			t.Errorf("%s: 11102 不该写模型级负缓存: %+v", uid, blk)
		}
	}
}

// TestChatModelForbiddenStillRotates 11158「该账号类型无权使用该模型」与 11102 相反：
// 同模型在不同账号类型上权限不同，换到有权限的号真能成功（真号实测 global:gpt-6-luna
// → 11158 not allowed for user type [personal_free_user]，轮转后由 Pro 号接住），
// 所以必须保留换号 + (账号,模型) 负缓存。这条用例防「顺手把 11158 也改成 fail-fast」。
func TestChatModelForbiddenStillRotates(t *testing.T) {
	const upstreamBody = `{"code":11158,"msg":"model [gpt-6-luna] is not allowed for user type [personal_free_user]","requestId":"01a1fbd8bad808d9a0cd215629c5396f"}`
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 400, upstreamBody, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "free1", AccessToken: "at-1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "free2", AccessToken: "at-2", ExpiresAt: 9999999999},
	)
	p.SetCredits("free1", 2000, 0)
	p.SetCredits("free2", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-6-luna","messages":[{"role":"user","content":"hi"}]}`)))
	if got := calls["Bearer at-1"] + calls["Bearer at-2"]; got < 2 {
		t.Fatalf("11158 必须换号重试（另一账号类型可能有权限），实际只打了 %d 次", got)
	}
	if rec.Code == 200 {
		t.Fatalf("两个号都无权限时应报错，得到 200")
	}
	if !strings.Contains(rec.Body.String(), "11158") {
		t.Errorf("应透传上游 11158 原文: %s", rec.Body.String())
	}
}
