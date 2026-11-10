// upstreamhealth_test.go 上游劣化滑窗告警的回归钉。
//
// 守的是「200 但慢到不可用」这种**无告警的停摆**：实测现场 TTFB 中位 17.8s、
// 6 条请求全部 success、零 WARN，客户端在自己 60s 超时里连撞后停摆。
package server

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

// newTestHealth 固定时钟的滑窗（测试可推进时间）。
func newTestHealth(now *time.Time) *upHealth {
	h := &upHealth{}
	h.now = func() time.Time { return *now }
	return h
}

// TestUpstreamHealthSlowRatioFires 慢请求占比超阈值必须告警。
func TestUpstreamHealthSlowRatioFires(t *testing.T) {
	now := time.Date(2026, 11, 9, 15, 0, 0, 0, time.Local)
	h := newTestHealth(&now)
	// 10 条里 4 条 20s（其余 2s）：40% >= 30%，且样本数达到 minSample。
	for i := 0; i < 6; i++ {
		h.record(200, reqlog.OutcomeSuccess, 2*time.Second)
	}
	if h.lastWarn.IsZero() == false {
		t.Fatal("样本不足阈值前不该告警")
	}
	for i := 0; i < 4; i++ {
		h.record(200, reqlog.OutcomeSuccess, 20*time.Second)
	}
	if h.lastWarn.IsZero() {
		t.Fatalf("慢请求 40%% 应告警（%s）", h.summary())
	}
}

// TestUpstreamHealthFailRatioFires 5xx 占比超阈值必须告警。
func TestUpstreamHealthFailRatioFires(t *testing.T) {
	now := time.Date(2026, 11, 9, 15, 0, 0, 0, time.Local)
	h := newTestHealth(&now)
	for i := 0; i < 8; i++ {
		h.record(200, reqlog.OutcomeSuccess, time.Second)
	}
	for i := 0; i < 3; i++ {
		h.record(502, reqlog.OutcomeHTTPError, 0)
	}
	if h.lastWarn.IsZero() {
		t.Fatalf("11 条里 3 条 5xx 应告警（%s）", h.summary())
	}
}

// TestUpstreamHealthIgnoresInterrupted 499/中断一律不采样。
//
// 反向代价：这条松掉，用户随手点「停止」就能把慢比例和失败比例同时污染，
// 告警变成噪声源，最后没人看。
func TestUpstreamHealthIgnoresInterrupted(t *testing.T) {
	now := time.Date(2026, 11, 9, 15, 0, 0, 0, time.Local)
	h := newTestHealth(&now)
	for i := 0; i < 20; i++ {
		h.record(499, reqlog.OutcomeInterrupted, 0)
	}
	if len(h.samples) != 0 {
		t.Fatalf("499 不该进样本，实际 %d 条", len(h.samples))
	}
	if !h.lastWarn.IsZero() {
		t.Fatal("499 不该触发告警")
	}
}

// TestUpstreamHealthNeedsMinSample 样本太少不判（冷启动单请求会误报）。
func TestUpstreamHealthNeedsMinSample(t *testing.T) {
	now := time.Date(2026, 11, 9, 15, 0, 0, 0, time.Local)
	h := newTestHealth(&now)
	h.record(200, reqlog.OutcomeSuccess, 30*time.Second)
	h.record(200, reqlog.OutcomeSuccess, 30*time.Second)
	if !h.lastWarn.IsZero() {
		t.Fatal("样本数 < upHealthMinSample 时不该告警")
	}
}

// TestUpstreamHealthWarnSuppressed 劣化持续时不能每来一条就打一行。
func TestUpstreamHealthWarnSuppressed(t *testing.T) {
	now := time.Date(2026, 11, 9, 15, 0, 0, 0, time.Local)
	h := newTestHealth(&now)
	fill := func() {
		for i := 0; i < 10; i++ {
			h.record(200, reqlog.OutcomeSuccess, 20*time.Second)
		}
	}
	fill()
	first := h.lastWarn
	if first.IsZero() {
		t.Fatal("首轮应告警")
	}
	fill()
	if h.lastWarn != first {
		t.Fatal("10 分钟窗口内不该重复告警（会刷屏）")
	}
	// 推进过抑制窗口后，持续劣化应再次告警——劣化还在，不能只报一次就沉默。
	now = now.Add(upHealthWarnEvery + time.Minute)
	fill()
	if !h.lastWarn.After(first) {
		t.Fatal("抑制窗口过后仍劣化，应再次告警")
	}
}

// TestUpstreamHealthWindowSlides 滑出窗口的旧样本不再参与判定。
//
// 没有这条，「上午慢过一次」会让下午一直报警——窗口不滑动，告警就成了
// 「历史上曾经出过事」的墓碑。
func TestUpstreamHealthWindowSlides(t *testing.T) {
	now := time.Date(2026, 11, 9, 15, 0, 0, 0, time.Local)
	h := newTestHealth(&now)
	for i := 0; i < 10; i++ {
		h.record(200, reqlog.OutcomeSuccess, 20*time.Second)
	}
	if h.lastWarn.IsZero() {
		t.Fatal("应告警")
	}
	// 时间推进到窗口之外，再灌入正常样本：慢样本必须被裁掉。
	now = now.Add(upHealthWindow + time.Minute)
	for i := 0; i < 10; i++ {
		h.record(200, reqlog.OutcomeSuccess, 2*time.Second)
	}
	for _, s := range h.samples {
		if s.ttfb >= upHealthSlowTTFB {
			t.Fatalf("窗口外的慢样本未被裁掉: %+v", s)
		}
	}
}
