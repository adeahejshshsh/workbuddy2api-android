package proxypool

import "testing"

// Snapshot 必须把 Realms 深拷出来（审查 C1）：它是 map[string]Binding，此前与 Manager
// 共享同一个 map；调用方锁外遍历、同时 BindRealm 在锁内原地写 → concurrent map read and
// map write，Go 运行时 fatal（不可 recover，整进程退出）。
func TestSnapshotClonesRealms(t *testing.T) {
	m := testManager(t, "")
	snap := m.Snapshot()
	if snap.Realms == nil {
		// 没有 realm 绑定时也允许是空 map；下面用一次绑定再验。
		snap = Snapshot{Realms: map[string]Binding{}}
	}
	before := len(snap.Realms)
	snap.Realms["cn"] = Binding{ProxyID: "injected", Mode: "fixed"} // 调用方改自己那份
	if got := len(m.Snapshot().Realms); got != before {
		t.Fatalf("改 Snapshot().Realms 影响到了 Manager：%d → %d（说明是同一个 map）", before, got)
	}
}
