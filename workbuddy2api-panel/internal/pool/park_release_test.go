package pool

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 热重载暂存期间的在途请求释放不能丢（审查 C3）：Release 只查 byUID 时，被 ParkMany
// 移走的号查不到，inFlight 不减 —— 复活后该号永久满载、再不被选中，直到重启。
func TestReleaseWhileParkedDoesNotLeak(t *testing.T) {
	p := New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"}
	p.Add(a)
	if !p.Acquire("u1") {
		t.Fatal("Acquire 失败")
	}
	if got := p.InFlight("u1"); got != 1 {
		t.Fatalf("inFlight=%d want 1", got)
	}
	// 模拟热重载把这个号暂存出池（文件暂时不见了）。
	if removed := p.ParkMany([]string{"u1"}); len(removed) != 1 {
		t.Fatalf("ParkMany=%v want [u1]", removed)
	}
	// 在途请求结束时释放：必须落到 parked 的原 entry 上。
	p.Release("u1")
	// 文件回来，号复活：inFlight 应该是 0，否则这个号再也不被选中。
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"})
	if got := p.InFlight("u1"); got != 0 {
		t.Fatalf("复活后 inFlight=%d want 0（暂存期间的 Release 丢了）", got)
	}
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("复活后账号应回到池里")
	}
	if st.InFlight != 0 {
		t.Fatalf("状态里的 inFlight=%d want 0", st.InFlight)
	}
}
