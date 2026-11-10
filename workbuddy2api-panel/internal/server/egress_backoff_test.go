// egress_backoff_test.go 出口级失败归因（借鉴 ardeyouxipianyi/workbuddy2api-hub 的
// wb_egress.py / issue #271）。
//
// 钉住两件事：
//   1. 直连路径的传输层失败记在**出口**上，不记在账号上——一次链路抖动不该把
//      恰好被轮转到的号逐个降权（hub 现场：4 小时 196 次 URLError 降权 13 个号）。
//   2. 阶梯是秒级且封顶 15s；连续失败到 egressGiveUpStreak 后不再换号重试。
package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestEgressBackoffLadder 阶梯：1s 起、逐次翻倍、封顶 15s、±25% 抖动；
// base<=0（测试）恒 0，streak<=0 不等待。
func TestEgressBackoffLadder(t *testing.T) {
	base := time.Second
	cases := []struct {
		streak int
		want   time.Duration
	}{
		{0, 0},
		{1, 1 * time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
		{5, 15 * time.Second},
		{9, 15 * time.Second}, // 封顶：不做 2^n 溢出
	}
	for _, c := range cases {
		got := egressBackoff(c.streak, base)
		if c.want == 0 {
			if got != 0 {
				t.Errorf("streak=%d got=%v want 0", c.streak, got)
			}
			continue
		}
		lo := time.Duration(float64(c.want) * 0.75)
		hi := time.Duration(float64(c.want) * 1.25)
		if got < lo || got > hi {
			t.Errorf("streak=%d got=%v want 落在 [%v,%v]", c.streak, got, lo, hi)
		}
	}
	if got := egressBackoff(3, 0); got != 0 {
		t.Errorf("base=0（测试模式）必须恒 0，got=%v", got)
	}
}

// egressTransportErrUpstream 所有请求都在传输层失败（模拟直连链路断掉）。
func egressTransportErrUpstream(calls *int) *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			*calls++
			return nil, errors.New("dial tcp: connection refused")
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

// TestTransportFailureChargedToEgressNotAccount ⭐ 核心：三个号、链路全断，
// 一个请求轮转打满 MaxRotate。断言**没有**任何账号背上连败/降权，而出口计数
// 恰好等于实际上游调用次数——这就是「归因正确」的定义。
//
// 旧行为：每次传输层失败都喂 NoteFailures(该轮被选中的号)。链路抖动几秒、
// 请求连发几轮，整池号会一个个被降到出池，而链路本身早已恢复。
func TestTransportFailureChargedToEgressNotAccount(t *testing.T) {
	var calls int
	p := testPoolWith(
		&auth.Auth{UID: "e1", AccessToken: "at-e1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "e2", AccessToken: "at-e2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "e3", AccessToken: "at-e3", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: egressTransportErrUpstream(&calls)})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	// 本仓的传输层终态是 502 upstream_unreachable（带 gateway_hint，客户端能区分
	// 「网关连不上上游」与「上游拒绝/没号」），上游仓是 503。断言按本仓契约。
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d body=%s want 502 upstream_unreachable", rec.Code, rec.Body)
	}
	if calls == 0 {
		t.Fatal("没有任何上游调用：轮转没跑起来，本用例失去意义")
	}
	for _, uid := range []string{"e1", "e2", "e3"} {
		st, _ := p.Status(uid)
		if st.ConsecutiveFails != 0 || !st.DegradeUntil.IsZero() {
			t.Errorf("账号 %s 不该为链路抖动背惩罚: consecutiveFails=%d degradeUntil=%v",
				uid, st.ConsecutiveFails, st.DegradeUntil)
		}
	}
	if got := p.EgressStreak(egressKey(&auth.Auth{UID: "e1"}), time.Now()); got != calls {
		t.Errorf("出口计数=%d want %d（等于本次请求的上游调用数）", got, calls)
	}
}

// TestEgressGiveUpStopsRotating 连续失败到 egressGiveUpStreak 之后，下一个请求
// 只打一次上游就 502：同一出口换号换不到别的链路，轮转只是把同样的等待与失败
// 重复 MaxRotate 遍。
func TestEgressGiveUpStopsRotating(t *testing.T) {
	var calls int
	p := testPoolWith(
		&auth.Auth{UID: "g1", AccessToken: "at-g1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g2", AccessToken: "at-g2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g3", AccessToken: "at-g3", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: egressTransportErrUpstream(&calls)})
	send := func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("code=%d body=%s want 502 upstream_unreachable", rec.Code, rec.Body)
		}
	}
	send()
	first := calls
	if first < egressGiveUpStreak-1 {
		t.Fatalf("首个请求只打了 %d 次上游，没到 egressGiveUpStreak=%d，用例前提不成立",
			first, egressGiveUpStreak)
	}
	send()
	if second := calls - first; second != 1 {
		t.Fatalf("链路已判定为断：第二个请求应只打 1 次上游，实际 %d 次（轮转被放行）", second)
	}
}

// TestEgressStreakSeparatedByRealm cn 与 global 是不同的上游环境：cn 抖动不得
// 让 global 的请求提前放弃轮转。
func TestEgressStreakSeparatedByRealm(t *testing.T) {
	if got := egressKey(&auth.Auth{UID: "x", AccessToken: "a"}); got != "direct:cn" {
		t.Fatalf("无 realm 时应归 cn: %q", got)
	}
	auth.SetGlobalEnabled(true)
	ga := &auth.Auth{UID: "x", AccessToken: "a", Domain: "www.workbuddy.ai"}
	if got := egressKey(ga); got != "direct:global" {
		t.Fatalf("realm=global: %q", got)
	}
	if got := egressKey(nil); got != "direct:cn" {
		t.Fatalf("nil 账号应归 cn: %q", got)
	}
}
