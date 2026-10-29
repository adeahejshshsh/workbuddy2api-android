package panel

// 任务链的上游软限流（429）重试（上游 2026-10-28 `4d643b1`）。
//
// 背景：growth 域频控是分钟级窗口，而一轮全量任务要连打几十次 ListTasks/accept/claim，
// 撞一次就把整条链断在「查询任务失败」——任务动作都是幂等读/写，等一会儿就能过。
// 但也不能傻等：同一账号撞过一次就记窗口，后续动作快失败，否则几十个动作逐个睡
// 会把后台作业卡几十分钟，还占着 taskLocks 把手动触发一起堵死。

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func TestIsSoftRateErr(t *testing.T) {
	if !isSoftRateErr(&upstream.Error{Kind: upstream.ErrSoftRate}) {
		t.Error("ErrSoftRate 应判为软限流")
	}
	// 其它错误等不来，重试只是浪费窗口。
	for _, k := range []upstream.ErrKind{upstream.ErrAccountFault, upstream.ErrSessionDead, upstream.ErrClient} {
		if isSoftRateErr(&upstream.Error{Kind: k}) {
			t.Errorf("%v 不该被当成软限流", k)
		}
	}
	if isSoftRateErr(errors.New("random")) {
		t.Error("非 upstream.Error 不该判为软限流")
	}
}

func TestSoftRateWait(t *testing.T) {
	// 无 Retry-After：按次数线性递增。
	if got := softRateWait(errors.New("x"), 1); got != softRateGap {
		t.Errorf("第 1 次应等 %v，got %v", softRateGap, got)
	}
	// 上游给了更长的 Retry-After：以它为准。
	long := &upstream.Error{Kind: upstream.ErrSoftRate, RetryAfter: 45 * time.Second}
	if got := softRateWait(long, 1); got != 45*time.Second {
		t.Errorf("应采用 Retry-After 45s，got %v", got)
	}
	// 封顶：一个头不能把后台作业拖住。
	huge := &upstream.Error{Kind: upstream.ErrSoftRate, RetryAfter: time.Hour}
	if got := softRateWait(huge, 1); got != softRateGapMax {
		t.Errorf("应封顶到 %v，got %v", softRateGapMax, got)
	}
}

func TestSoftRateGate(t *testing.T) {
	p := &Panel{}
	if d := p.softRateGate("u1"); d != 0 {
		t.Fatalf("未限流时应为 0，got %v", d)
	}
	p.noteSoftRate("u1", 30*time.Second)
	if d := p.softRateGate("u1"); d <= 0 || d > 30*time.Second {
		t.Fatalf("应剩余 ~30s，got %v", d)
	}
	// 别的账号不受影响（按账号隔离）。
	if d := p.softRateGate("u2"); d != 0 {
		t.Fatalf("别的账号不该被带住，got %v", d)
	}
	// 过期自动清：不删的话该账号会被永久快失败。
	p.noteSoftRate("u3", -time.Second)
	if d := p.softRateGate("u3"); d != 0 {
		t.Fatalf("过期后应为 0，got %v", d)
	}
}

// 窗口内再打只会续窗口：必须快失败并给出剩余时间，而不是再睡一遍。
func TestListTasksSoftRateFailsFastInWindow(t *testing.T) {
	p := &Panel{}
	p.noteSoftRate("u1", 20*time.Second)
	_, err := p.listTasksSoftRate(&auth.Auth{UID: "u1"}, false)
	if err == nil {
		t.Fatal("窗口内应直接返回错误")
	}
	if msg := err.Error(); !strings.Contains(msg, "上游限流（429）") || !strings.Contains(msg, "后恢复") {
		t.Fatalf("错误信息应给出剩余等待，got %q", msg)
	}
}
