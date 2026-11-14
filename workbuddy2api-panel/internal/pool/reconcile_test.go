package pool

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 池侧：Reconcile 对新号返回 Added，对「同到期不同令牌」返回 Unchanged（不换指针），
// 对「更晚到期」返回 Updated 且**保持同一个 *Auth 指针**（在途请求与刷新共享它）。
func TestPoolReconcileKeepsPointer(t *testing.T) {
	p := New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 1000, Domain: "www.workbuddy.ai"}
	p.Add(a)

	if got := p.Reconcile(&auth.Auth{UID: "u1", AccessToken: "at-1b", ExpiresAt: 1000, Domain: "www.workbuddy.ai"}); got != ReconcileUnchanged {
		t.Fatalf("同到期不同令牌应 Unchanged，实际 %v", got)
	}
	if got := p.Reconcile(&auth.Auth{UID: "u1", AccessToken: "at-2", ExpiresAt: 2000, Domain: "www.workbuddy.ai"}); got != ReconcileUpdated {
		t.Fatalf("更晚到期应 Updated，实际 %v", got)
	}
	if cur := p.AuthByUID("u1"); cur != a {
		t.Fatal("Reconcile 不该换 *Auth 指针（在途请求与刷新共享它）")
	}
	if got := p.Reconcile(&auth.Auth{UID: "u2", AccessToken: "at-u2", ExpiresAt: 2000, Domain: "www.workbuddy.ai"}); got != ReconcileAdded {
		t.Fatalf("新号应 Added，实际 %v", got)
	}
}
