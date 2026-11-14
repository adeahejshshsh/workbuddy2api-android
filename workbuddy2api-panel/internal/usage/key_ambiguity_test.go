package usage

import (
	"testing"
	"time"
)

// 桶键不能用裸分隔符拼（审查 A8）：model 是客户端透传的字符串，带 `|` 的名字能拼出
// 与另一账号/另一模型相同的键，用量与积分就记到别人头上。
func TestAddKeyNotAmbiguous(t *testing.T) {
	r := New("")
	now := time.Now()
	// 裸拼 "scope|realm|uid|model|rate" 时，这两组会得到同一个键。
	r.Add(now, "cn", "u1|x", "m", Delta{PromptTokens: 1, HasPromptTokens: true}, true)
	r.Add(now, "cn", "u1", "x|m", Delta{PromptTokens: 2, HasPromptTokens: true}, true)
	snap := r.SnapshotWithRates(1, nil, nil)
	// 两个不同的 (uid, model) 各自成桶 → by_account 与 by_model 各 2 条；撞键就会各 1 条。
	if len(snap.ByAccount) != 2 || len(snap.ByModel) != 2 {
		t.Fatalf("桶键撞了：by_account=%d by_model=%d（want 2/2）", len(snap.ByAccount), len(snap.ByModel))
	}
}
