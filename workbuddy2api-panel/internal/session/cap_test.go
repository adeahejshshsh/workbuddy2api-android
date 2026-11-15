package session

import (
	"testing"
	"time"
)

// 会话表必须有硬上限（审查 P2 无界增长）：客户端可传任意唯一键，脚本能把它顶爆。
func TestSessionEntriesCapped(t *testing.T) {
	r := New(Config{})
	now := time.Now()
	r.mu.Lock()
	// 直接塞到超过上限，避免跑 2 万次 bind。
	for i := 0; i < maxSessionEntries+500; i++ {
		r.entries[string(rune(i))+"-"+string(rune(i*7))] = entry{uid: "u1", lastActive: now.Add(-time.Duration(i) * time.Second)}
	}
	r.trimLocked()
	n := len(r.entries)
	r.mu.Unlock()
	if n > maxSessionEntries {
		t.Fatalf("未被限流：%d > %d", n, maxSessionEntries)
	}
	if n < maxSessionEntries/2 {
		t.Fatalf("淘汰过度：%d（应淘汰到约 90%% 水位）", n)
	}
}
