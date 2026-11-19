package pool

import (
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestNoPickForRealmBreakdown 每个账号按 pick 的排除顺序归一类（issue #276）：
// 号在、有余额，但「选不到」的成因各不相同，503 文案靠这份拆解才能说清是谁挡的。
func TestNoPickForRealmBreakdown(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	for _, uid := range []string{"parked", "ok", "dead", "busy"} {
		p.Add(&auth.Auth{UID: uid})
	}
	p.SetCredits("parked", 5, 100) // 余额已知 5 <= reserve 10 → 停牌
	p.SetCredits("ok", 500, 500)
	p.SetCredits("busy", 500, 500)
	p.SetReserveCredits(10)
	p.Disable("dead", "test")
	p.SetMaxInFlight(1)
	if !p.Acquire("busy") {
		t.Fatal("占在途名额失败，用例前提不成立")
	}

	n := p.NoPickForRealm("", "", nil)
	want := NoPick{Total: 4, Healthy: 1, Reserve: 1, Disabled: 1, InFlightFull: 1}
	if n != want {
		t.Fatalf("拆解不符: got %+v want %+v", n, want)
	}

	// 轮转试过的号单列，不重复计入其它类（否则「MaxRotate 用完」会被误读成「号都挂了」）。
	n = p.NoPickForRealm("", "", map[string]bool{"ok": true})
	if n.Tried != 1 || n.Healthy != 0 || n.Total != 4 {
		t.Fatalf("tried 归类错误: %+v", n)
	}
	if s := n.String(); !strings.Contains(s, "tried=1") || !strings.Contains(s, "reserve=") {
		t.Fatalf("String 缺字段: %s", s)
	}

	// realm 过滤：别的域的号不进本域拆解。
	p.Add(&auth.Auth{UID: "gl", Domain: "www.workbuddy.ai"})
	if got := p.NoPickForRealm("", "global", nil); got.Total != 1 || got.Healthy != 1 {
		t.Fatalf("realm 过滤失效: %+v", got)
	}
}
