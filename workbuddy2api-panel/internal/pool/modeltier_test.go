package pool

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestModelTierGateLearning 等级闸门：只在"池内每个号都判得出来用不了"时才隐藏。
func TestModelTierGateLearning(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})

	// 还没学到任何事实 → 不隐藏（宁可多显示，也不藏掉可能能用的模型）。
	if p.ModelDeniedForAllAccounts("gpt-6-astra") {
		t.Fatal("无任何事实时不应隐藏")
	}

	// 只学到 u1 的等级 → u2 等级未知 → 仍不隐藏。
	p.NoteModelTierDenied("u1", "personal_free_user", "gpt-6-astra")
	if p.ModelDeniedForAllAccounts("gpt-6-astra") {
		t.Fatal("还有账号等级未知（判不出来）时不应隐藏")
	}

	// 学到 u2 同等级 → 池内每个号都判得出来用不了 → 隐藏。
	p.NoteModelTierDenied("u2", "personal_free_user", "gpt-6-astra")
	if !p.ModelDeniedForAllAccounts("gpt-6-astra") {
		t.Fatal("池内每个号都已知用不了时应隐藏")
	}

	// 另一个模型没事实 → 不隐藏（互相不污染）。
	if p.ModelDeniedForAllAccounts("deepseek-v4.1-flash") {
		t.Fatal("无事实的模型被误隐藏")
	}
	// 别的等级不能用，不代表本池（等级未学到该模型）会被隐藏。
	p.NoteModelTierDenied("", "enterprise_user", "glm-5.3")
	if p.ModelDeniedForAllAccounts("glm-5.3") {
		t.Fatal("别的等级的事实不应影响本池判定")
	}
	types, pairs, uids := p.TierDenyStats()
	if types != 2 || pairs != 2 || uids != 2 {
		t.Fatalf("统计异常：types=%d pairs=%d uids=%d", types, pairs, uids)
	}
}

// TestModelTierPersist 事实要落盘（与 state 文件同目录），重启后仍在。
func TestModelTierPersist(t *testing.T) {
	dir := t.TempDir()
	stateFp := filepath.Join(dir, "state.json")

	p := New(stateFp)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteModelTierDenied("u1", "personal_free_user", "gpt-6-astra")
	if _, err := os.Stat(filepath.Join(dir, "model_tier.json")); err != nil {
		t.Fatalf("未落盘: %v", err)
	}

	p2 := New(stateFp)
	p2.Add(&auth.Auth{UID: "u1"})
	if !p2.ModelDeniedForAllAccounts("gpt-6-astra") {
		t.Fatal("重启后等级事实丢失")
	}
}
