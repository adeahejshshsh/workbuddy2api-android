package auth

// 企业版判定的权威判据是上游 account.type，不是 enterpriseId 有没有值。
//
// 现场：国服个人号也会带一个 enterpriseId（个人空间/默认租户），只看它会把**任意账号**
// 判成企业版 —— 面板显示「企业版」标签，签到 / 成长任务 / 连登 / 旅行被门控掉，用户看到
// 的就是「全池都不签到了」。官方 CLI 的判据是 isPersonalAccount(type=="personal")
// 优先，isEnterpriseEditionAccount 才用 type ∈ {ultimate, exclusive} 或 enterpriseId。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestIsEnterprisePrefersAccountType(t *testing.T) {
	cases := []struct {
		name string
		a    *Auth
		want bool
	}{
		{"personal + 有 enterpriseId → 个人号", &Auth{AccountType: "personal", EnterpriseID: "ent-1"}, false},
		{"PERSONAL 大小写不敏感", &Auth{AccountType: " Personal ", EnterpriseID: "ent-1"}, false},
		{"ultimate → 企业版（即使没有 enterpriseId）", &Auth{AccountType: "ultimate"}, true},
		{"exclusive → 企业版", &Auth{AccountType: "exclusive"}, true},
		{"无 type + enterpriseId → 回落到旧判据", &Auth{EnterpriseID: "ent-1"}, true},
		{"无 type + 无 enterpriseId → 个人号", &Auth{}, false},
		{"占位值 0 不算企业", &Auth{EnterpriseID: "0"}, false},
		{"占位值 null 不算企业", &Auth{EnterpriseID: "null"}, false},
		{"占位值 personal 不算企业", &Auth{EnterpriseID: "personal"}, false},
		{"空白不算企业", &Auth{EnterpriseID: "   "}, false},
	}
	for _, tc := range cases {
		if got := tc.a.IsEnterprise(); got != tc.want {
			t.Errorf("%s: IsEnterprise()=%v want %v", tc.name, got, tc.want)
		}
	}
}

// TestEnterpriseTypeRoundTrip 落盘要保留 type，且占位 enterpriseId 不写回文件。
func TestEnterpriseTypeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	a := &Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt",
		AccountType: "personal", EnterpriseID: "0", Nickname: "n"}
	a.FilePath = filepath.Join(dir, "workbuddy-u1.json")
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(a.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	// 文件是缩进 JSON（"type": "personal"），用解析而不是字面匹配
	var doc struct {
		Account struct {
			Type         string `json:"type"`
			EnterpriseID string `json:"enterpriseId"`
		} `json:"account"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("落盘不是合法 JSON: %v\n%s", err, raw)
	}
	if doc.Account.Type != "personal" {
		t.Fatalf("type 没落盘: %s", raw)
	}
	if doc.Account.EnterpriseID != "" {
		t.Fatalf("占位 enterpriseId 不该写回文件: %q", doc.Account.EnterpriseID)
	}
	got, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.AccountType != "personal" {
		t.Fatalf("type 丢失: %q", got.AccountType)
	}
	if got.EnterpriseID != "" {
		t.Fatalf("占位值 enterpriseId 应归一成空，实际 %q", got.EnterpriseID)
	}
	if got.IsEnterprise() {
		t.Fatal("personal 账号被判成企业版（本次要修的 bug）")
	}
}
