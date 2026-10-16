package pool

import (
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestModelBlockReason11158 11158（账号等级不够）必须与 11102 分开记 reason：
// handler 末端据此把笼统的 503 no_healthy_account 换成精确的
// model_not_allowed_for_account_type（用户实测："你改成 503 了"）。
func TestModelBlockReason11158(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})

	// 两个号都被 11158 挡住 → ModelBlocked 必须报 Blocked 且 reason 以 11158 开头。
	p.BlockModelBackoff("u1", "gpt-6-astra", upstream.ModelNotAllowedReason)
	if st := p.ModelBlocked("gpt-6-astra"); st.Blocked {
		t.Fatalf("只挡了一个号，不该报模型级阻塞: %+v", st)
	}
	p.BlockModelBackoff("u2", "gpt-6-astra", upstream.ModelNotAllowedReason)
	st := p.ModelBlocked("gpt-6-astra")
	if !st.Blocked || st.Count != 2 {
		t.Fatalf("两个号都被挡应报 Blocked/Count=2，实际 %+v", st)
	}
	if !strings.HasPrefix(st.Reason, "11158") {
		t.Fatalf("reason 未保留 11158 前缀（末端无法据此换精确 code）: %q", st.Reason)
	}

	// 该模型实测又通了 → 11158 条目同样要被清掉（与 11102 同口径）。
	p.BlockModelClear("u1", "gpt-6-astra")
	p.BlockModelClear("u2", "gpt-6-astra")
	if st := p.ModelBlocked("gpt-6-astra"); st.Blocked {
		t.Fatalf("清掉后不该再报阻塞: %+v", st)
	}

	// 6004（模型限流，有自己的上游重置墙钟）不得被 BlockModelClear 抹掉。
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(time.Hour), "glm-5.3", "6004 model rate limit")
	p.BlockModelClear("u1", "glm-5.3")
	if !p.byUID["u1"].modelCooled(time.Now(), "glm-5.3") {
		t.Fatal("6004 条目被 BlockModelClear 误清")
	}
}

// TestModelBlockedThenTierGate 组合口径：号被负缓存避开后，等级事实仍能独立判定
// （末端"本轮没打到上游"也要给精确 code 的依据）。
func TestModelBlockedThenTierGate(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteModelTierDenied("u1", "personal_free_user", "gpt-6-astra")
	p.BlockModelBackoff("u1", "gpt-6-astra", upstream.ModelNotAllowedReason)

	if !p.ModelDeniedForAllAccounts("gpt-6-astra") {
		t.Fatal("等级事实应判定全池不支持该模型")
	}
	if st := p.ModelBlocked("gpt-6-astra"); !st.Blocked || !strings.HasPrefix(st.Reason, "11158") {
		t.Fatalf("模型级阻塞应报 11158 原因: %+v", st)
	}
}
