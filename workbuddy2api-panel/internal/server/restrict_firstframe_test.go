// restrict_firstframe_test.go 首帧就撞 11140 时的处置（d3edea90）。
//
// 此前这条路径只 fail 不停止轮转：HTTP 200 已经开流、错误以帧的形式到达，handler 把它
// 记成 stream error 后照样换下一个号——而上游把这条判在设备/出口上，换号等于把下一个号
// 也送进去（hub #422：一次任务 7 个号全封）。
package server

import (
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 11140 账号风控限制文书（首帧形态）。
const firstFrameRestricted = `data: {"error":{"message":"账号存在异常行为，已被限制使用","code":"11140"}}` + "\n\n" +
	"data: [DONE]\n\n"

func TestFirstFrameRestrictionStopsRotation(t *testing.T) {
	var calls atomic.Int32
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls.Add(1)
		return 200, firstFrameRestricted, true
	})
	h := NewHandler(Config{
		Pool: testPoolWith(
			&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "a", ExpiresAt: 9999999999},
			&auth.Auth{UID: "g2", Domain: "www.workbuddy.ai", AccessToken: "b", ExpiresAt: 9999999999},
		),
		Upstream:  up,
		MaxRotate: 3,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"global:deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if got := calls.Load(); got != 1 {
		t.Fatalf("首帧 11140 后不该再换号，上游被调用了 %d 次", got)
	}
	// 被打中的那个号必须被禁用（设备/出口级判据），而不是软冷却十分钟后又被抽中。
	// 选号是随机的，所以按「至少一个号被禁用」断言，不假设打到 g1 还是 g2。
	disabled := 0
	for _, uid := range []string{"g1", "g2"} {
		if st, ok := h.cfg.Pool.Status(uid); ok && st.Disabled {
			disabled++
		}
	}
	if disabled != 1 {
		t.Fatalf("应恰好一个号被禁用，实际 %d 个", disabled)
	}
	if body := rec.Body.String(); !strings.Contains(body, "error") {
		t.Fatalf("客户端应看到错误: %s", body)
	}
}
