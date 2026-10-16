package pool

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestModelTierGatePerRealm 等级闸门必须**分域**判定。
//
// 用户实测：列表里留着 global:gpt-6-astra，一发就 503 —— 两个 global 号早被判定
// "等级不支持"（负缓存 + 等级事实），而池里还有个 CN 号等级未知，于是老的"全池"判据
// 放过了它；可 global 前缀的模型永远选不到 CN 号，客户端挑中必失败。
func TestModelTierGatePerRealm(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "g2", Domain: "www.workbuddy.ai"})

	p.NoteModelTierDenied("g1", "personal_free_user", "gpt-6-astra")
	p.NoteModelTierDenied("g2", "personal_free_user", "gpt-6-astra")

	if !p.ModelDeniedForAllAccountsInRealm("gpt-6-astra", "global") {
		t.Fatal("global 域两个号都已知不支持 → 该域应隐藏该模型")
	}
	if p.ModelDeniedForAllAccountsInRealm("gpt-6-astra", "cn") {
		t.Fatal("CN 域只有一个等级未知的号 → 不该隐藏（保守）")
	}
	if p.ModelDeniedForAllAccounts("gpt-6-astra") {
		t.Fatal("全池判定不该为 true（CN 号等级未知）—— 分域判定才是对的")
	}
	if p.ModelDeniedForAllAccountsInRealm("gpt-6-astra", "nonexistent") {
		t.Fatal("该域没有任何账号 → 不该下结论")
	}
	// 学到 CN 号的等级后，CN 域也能独立判定（互不影响）。
	p.NoteModelTierDenied("cn1", "enterprise_user", "glm-5.3")
	if !p.ModelDeniedForAllAccountsInRealm("glm-5.3", "cn") {
		t.Fatal("CN 域唯一号已知不支持 → 该域应隐藏")
	}
	if p.ModelDeniedForAllAccountsInRealm("glm-5.3", "global") {
		t.Fatal("global 域没有任何 glm-5.3 事实 → 不该隐藏")
	}
}
