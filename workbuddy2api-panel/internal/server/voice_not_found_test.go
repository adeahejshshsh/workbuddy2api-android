package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestSpeechUnknownVoiceFailsFastWithoutPenalty 音色名在上游未配置时，上游回
// **500 + code 10000**「get voice config failed: voice X not found in Rainbow」
// （2026-11-20 真号实测，见 issue #277 的跟进）。
//
// 500 落 ErrServer 的代价与 11103 完全一样：网关轮转全池、每个健康号白记一笔连败，
// 末端把「音色写错了」报成「账号都不可用」。音色表是全局配置，换号不可能变。
func TestSpeechUnknownVoiceFailsFastWithoutPenalty(t *testing.T) {
	const upstreamBody = `{"code":10000,"msg":"get voice config failed: voice alloy not found in Rainbow: AppID:v1_rb1_99ce0ea0 Group:openapi Env:Prod","requestId":"01a1f796"}`
	var calls int
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{
				StatusCode: 500,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(bytes.NewReader([]byte(upstreamBody))),
			}, nil
		})},
		ChatBaseCN:    "https://cn.example",
		BillingBaseCN: "https://cn.example",
	}
	p := testPoolWith(
		&auth.Auth{UID: "a1", AccessToken: "at-1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a2", AccessToken: "at-2", ExpiresAt: 9999999999},
	)
	p.SetCredits("a1", 2000, 0)
	p.SetCredits("a2", 2000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up, TTSEnabled: true})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/audio/speech",
		strings.NewReader(`{"model":"tts-1","input":"你好","voice":"alloy"}`)))

	if calls != 1 {
		t.Fatalf("上游调用 %d 次 want 1（音色错误不该轮转）", calls)
	}
	if rec.Code != 500 {
		t.Fatalf("code=%d body=%s（上游状态码应透传）", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `omit \"voice\"`) || !strings.Contains(body, "f191_female_cn_tts2.5_flash") {
		t.Errorf("hint 应提示省掉 voice 用默认音色: %s", body)
	}
	for _, uid := range []string{"a1", "a2"} {
		st, _ := p.Status(uid)
		if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.BreakerFails != 0 ||
			st.ConsecutiveFails != 0 || !st.DegradeUntil.IsZero() {
			t.Errorf("%s: 音色错误不得罚号: %+v", uid, st)
		}
	}
}
