package scheduler

// 抖动 key 必须随轮次变化：jitterFrac 的散列输入是「key + 进程种子」，key 恒为 "bal"
// 时每轮算出的偏移完全相同 —— 那不是「间隔在抖」，而是「恒定间隔 + 一个固定相位」，
// 防机械节拍的目标根本没达成（只是把 5 分钟变成 5 分 47 秒这种新的恒定值）。

import (
	"testing"
	"time"
)

func TestPeriodicJitterVariesByKey(t *testing.T) {
	setJitterSeed(12345)
	t.Cleanup(func() { setJitterSeed(0) })

	const interval = 5 * time.Minute
	const frac = 0.3

	seen := map[time.Duration]bool{}
	for round := 0; round < 8; round++ {
		d := periodicJitter(interval, frac, "bal:"+itoa(round))
		// 每个偏移都必须落在 ±frac×interval 内
		limit := time.Duration(float64(interval) * frac)
		if d < -limit || d > limit {
			t.Fatalf("round=%d 偏移 %v 超出 ±%v", round, d, limit)
		}
		seen[d] = true
	}
	// 8 轮里至少出现 4 种不同偏移（若 key 不随轮次变，这里恒为 1 种）
	if len(seen) < 4 {
		t.Fatalf("偏移只出现 %d 种，抖动没有随轮次变化：%v", len(seen), seen)
	}

	// 同一个 key 必须稳定（同一轮内重排/热改不该跳）。
	a := periodicJitter(interval, frac, "bal:3")
	b := periodicJitter(interval, frac, "bal:3")
	if a != b {
		t.Fatalf("同一 key 的偏移不稳定：%v vs %v", a, b)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
