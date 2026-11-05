package server

// 退避的**界**此前从未被测：TestMain 把 rotateBackoffBase 置 0 加速轮转用例，
// 于是 backoffAfter 恒返回 0 —— 翻倍、封顶、抖动三条逻辑全是裸奔区。
// 改动 jitterFraction 或 rotateBackoffCap（例如把封顶从 8s 调到 80s）时全套测试照绿，
// 线上表现为「一次 429 后客户端多等一分钟」。这里按 slotwait_test 的手法临时恢复基数。

import (
	"testing"
	"time"
)

func withBackoffBase(t *testing.T, base time.Duration) {
	t.Helper()
	old := rotateBackoffBase
	rotateBackoffBase = base
	t.Cleanup(func() { rotateBackoffBase = old })
}

// 指数翻倍 + 封顶：n 次之后应落在 base·2^n（未封顶时）的 ±25% 抖动区间内。
func TestBackoffAfterDoublesAndCaps(t *testing.T) {
	const base = 100 * time.Millisecond
	withBackoffBase(t, base)

	cases := []struct {
		n    int
		want time.Duration // 抖动前的目标值
	}{
		{0, base},
		{1, base * 2},
		{2, base * 4},
		{3, base * 8},          // 800ms，仍未到封顶
		{10, rotateBackoffCap}, // 远超封顶 → 钳到 8s
		{99, rotateBackoffCap},
	}
	for _, c := range cases {
		got := backoffAfter(c.n)
		lo := time.Duration(float64(c.want) * (1 - jitterFraction))
		hi := time.Duration(float64(c.want) * (1 + jitterFraction))
		if got < lo || got > hi {
			t.Errorf("backoffAfter(%d) = %v，应落在 [%v, %v]（目标 %v ±%.0f%%）",
				c.n, got, lo, hi, c.want, jitterFraction*100)
		}
	}
}

// 基数置 0（TestMain 的形态）必须恒 0：否则轮转用例会白等。
func TestBackoffAfterZeroBase(t *testing.T) {
	withBackoffBase(t, 0)
	for _, n := range []int{0, 1, 5, 99} {
		if got := backoffAfter(n); got != 0 {
			t.Fatalf("base=0 时 backoffAfter(%d)=%v，应为 0", n, got)
		}
	}
}

// jitterDur 的区间与零值语义。
func TestJitterDurBounds(t *testing.T) {
	if got := jitterDur(0); got != 0 {
		t.Fatalf("jitterDur(0)=%v，应为 0", got)
	}
	const d = time.Second
	lo := time.Duration(float64(d) * (1 - jitterFraction))
	hi := time.Duration(float64(d) * (1 + jitterFraction))
	for i := 0; i < 200; i++ { // 抖动是随机的，多采几次才可能碰到边界
		if got := jitterDur(d); got < lo || got > hi {
			t.Fatalf("jitterDur(%v)=%v，应落在 [%v, %v]", d, got, lo, hi)
		}
	}
}
