package pool

import (
	"testing"
	"time"
)

// TestEgressStreakIncrementsAndResets 钉住出口级计数的两条语义：
//
//	同一窗口内连续失败 → 1,2,3…（阶梯据此翻倍）
//	超过 egressWindow 再失败 → 从 1 重新开始（一次旧抖动不该抬高新一轮的退避）
func TestEgressStreakIncrementsAndResets(t *testing.T) {
	p := New("")
	now := time.Now()
	for i := 1; i <= 3; i++ {
		if got := p.NoteEgressFailure("direct:cn", now); got != i {
			t.Fatalf("第 %d 次连续失败 got=%d want %d", i, got, i)
		}
		if got := p.EgressStreak("direct:cn", now); got != i {
			t.Fatalf("EgressStreak 第 %d 次 got=%d want %d", i, got, i)
		}
		now = now.Add(time.Second)
	}
	// 窗口内（59s）仍算同一轮
	now = now.Add(egressWindow - time.Second)
	if got := p.NoteEgressFailure("direct:cn", now); got != 4 {
		t.Fatalf("窗口内应接着上一轮: got=%d want 4", got)
	}
	// 超过窗口：这一轮作废
	now = now.Add(egressWindow + time.Second)
	if got := p.NoteEgressFailure("direct:cn", now); got != 1 {
		t.Fatalf("超过窗口应重新计数: got=%d want 1", got)
	}
}

// TestEgressStreakExpiresWithoutTraffic 长时间没有新失败时，EgressStreak 必须报 0——
// 否则一次抖动过后，这条出口会永远背着旧阶梯（面板与排障都读到错的当前状态）。
func TestEgressStreakExpiresWithoutTraffic(t *testing.T) {
	p := New("")
	now := time.Now()
	p.NoteEgressFailure("direct:global", now)
	p.NoteEgressFailure("direct:global", now)
	if got := p.EgressStreak("direct:global", now); got != 2 {
		t.Fatalf("got=%d want 2", got)
	}
	if got := p.EgressStreak("direct:global", now.Add(egressWindow+time.Second)); got != 0 {
		t.Fatalf("过期后应报 0: got=%d", got)
	}
}

// TestEgressKeysAreIndependent 两个域的直连出口是两条链路：cn 抖动不得抬高 global 的阶梯。
func TestEgressKeysAreIndependent(t *testing.T) {
	p := New("")
	now := time.Now()
	for range 3 {
		p.NoteEgressFailure("direct:cn", now)
	}
	if got := p.EgressStreak("direct:global", now); got != 0 {
		t.Fatalf("global 不该受 cn 影响: got=%d", got)
	}
	if got := p.NoteEgressFailure("direct:global", now); got != 1 {
		t.Fatalf("global 应从 1 起: got=%d", got)
	}
}

// TestEgressEmptyKeyIgnored 拿不到出口身份时不能悄悄记进共享桶——那等于把「张冠李戴」
// 换个地方重演（所有账号都算到同一个空键上）。
func TestEgressEmptyKeyIgnored(t *testing.T) {
	p := New("")
	now := time.Now()
	if got := p.NoteEgressFailure("", now); got != 0 {
		t.Fatalf("空键应返回 0: got=%d", got)
	}
	if got := p.EgressStreak("", now); got != 0 {
		t.Fatalf("空键应返回 0: got=%d", got)
	}
	if len(p.egress) != 0 {
		t.Fatalf("空键不应写入计数表: %v", p.egress)
	}
}

// TestEgressTableBounded 代理池可随时导入/删除节点，键会跟着变；计数表必须有上限，
// 否则长时间运行会随节点增删无限增长。
func TestEgressTableBounded(t *testing.T) {
	p := New("")
	now := time.Now()
	for i := 0; i < maxEgressKeys+20; i++ {
		p.NoteEgressFailure("p:node-"+string(rune('a'+i%26))+string(rune('a'+i/26)), now)
	}
	if len(p.egress) > maxEgressKeys+1 {
		t.Fatalf("计数表超限未回收: %d 项", len(p.egress))
	}
}
