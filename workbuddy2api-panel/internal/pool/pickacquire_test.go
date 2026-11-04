package pool

// 选号与在途占位必须是**一步**（check-then-act 收敛）。
//
// 此前是 pick（只读 inFlightFull 过滤）→ Acquire（CAS 占位）两步，中间没有保留：
// 池里 1 个健康号、max_in_flight=1、N 个并发同时进时，N 次 pick 都返回同一个号，
// 只有 1 个 CAS 成功，其余把这个号记进 tried 重选、本轮再也选不到它 → 503，
// 而空位其实马上就出现。

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func acquireTestPool(maxInFlight int) *Pool {
	p := New("")
	p.SetRandomSource(func(int64) int64 { return 0 })
	p.SetMaxInFlight(maxInFlight)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	p.SetCredits("u1", 1000, 0)
	return p
}

func TestPickAcquireReservesInFlight(t *testing.T) {
	p := acquireTestPool(1)

	// 第一次：拿到号，并且**名额已被占用**。
	a := p.PickAcquireExcludingForRealm(nil, "", "cn")
	if a == nil || a.UID != "u1" {
		t.Fatalf("第一次应选到 u1，实际 %v", a)
	}
	// 第二次：名额已满 → 必须返回 nil（而不是再返回同一个号让调用方去撞 Acquire）。
	if b := p.PickAcquireExcludingForRealm(nil, "", "cn"); b != nil {
		t.Fatalf("名额已满时不该再选到号，实际 %v（占位没在选号锁内完成）", b.UID)
	}
	// 释放后应能再次选到。
	p.Release("u1")
	if c := p.PickAcquireExcludingForRealm(nil, "", "cn"); c == nil {
		t.Fatal("Release 后应能再次选到号")
	}
}

// 对照组：普通 Pick 仍**不**占位（粘性路径与只读调用方依赖这个语义）。
func TestPickDoesNotReserve(t *testing.T) {
	p := acquireTestPool(1)
	if a := p.PickExcludingForRealm(nil, "", "cn"); a == nil {
		t.Fatal("普通选号应成功")
	}
	// 没占位 → 再选还是同一个号（名额仍空）。
	if b := p.PickExcludingForRealm(nil, "", "cn"); b == nil {
		t.Fatal("普通选号不占位，第二次也应成功")
	}
	st, _ := p.Status("u1")
	if st.InFlight != 0 {
		t.Fatalf("普通选号不该占用在途名额，InFlight=%d", st.InFlight)
	}
}

// 并发下「拿到号」的次数不得超过名额上限——这是本次修复的核心不变量。
func TestPickAcquireNeverExceedsInFlightCap(t *testing.T) {
	const limit = 3
	p := acquireTestPool(limit)

	got := make(chan *auth.Auth, 16)
	for i := 0; i < 16; i++ {
		go func() { got <- p.PickAcquireExcludingForRealm(nil, "", "cn") }()
	}
	ok := 0
	for i := 0; i < 16; i++ {
		if a := <-got; a != nil {
			ok++
		}
	}
	if ok != limit {
		t.Fatalf("并发选中次数=%d，应恰好等于名额上限 %d（多了=超额，少了=空位浪费）", ok, limit)
	}
}
