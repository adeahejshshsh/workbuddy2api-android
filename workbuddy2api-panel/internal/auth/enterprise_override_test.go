package auth

// 人工定性「是否企业版」：自动判据看 type / enterpriseId，而国服个人号常带默认租户的
// enterpriseId —— 覆盖值必须能压过它，且两种文件形态（旧扁平 / 现嵌套）都要认。

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestEnterpriseOverrideBeatsAutoDetection(t *testing.T) {
	// enterpriseId 非空 → 自动判成企业版；强制个人后必须翻过来。
	a := &Auth{UID: "u1", AccessToken: "at", EnterpriseID: "e-123"}
	if !a.IsEnterprise() {
		t.Fatal("前置：带 enterpriseId 应判企业版")
	}
	a.EnterpriseOverride = "personal"
	if a.IsEnterprise() {
		t.Fatal("强制个人应压过 enterpriseId")
	}
	// type=ultimate 通常判企业；强制个人同样压过。
	b := &Auth{UID: "u2", AccessToken: "at", AccountType: "ultimate"}
	if !b.IsEnterprise() {
		t.Fatal("前置：type=ultimate 应判企业版")
	}
	b.EnterpriseOverride = "personal"
	if b.IsEnterprise() {
		t.Fatal("强制个人应压过 type=ultimate")
	}
	// 反向：type=personal 但被强制企业。
	c := &Auth{UID: "u3", AccessToken: "at", AccountType: "personal"}
	if c.IsEnterprise() {
		t.Fatal("前置：type=personal 应判个人")
	}
	c.EnterpriseOverride = "enterprise"
	if !c.IsEnterprise() {
		t.Fatal("强制企业应压过 type=personal")
	}
}

// TestEnterpriseOverrideOldFlatFormat 旧版本文件是扁平形（accessToken 在顶层）。
// 覆盖值必须能从旧形态读出来，且切换后写回不丢关键字段。
func TestEnterpriseOverrideOldFlatFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workbuddy-u1.json")
	old := `{"uid":"u1","accessToken":"at-old","refreshToken":"rt-old","expiresAt":1760000000,
		"domain":"https://api.workbuddy.cn","enterpriseId":"e-9","type":"ultimate",
		"enterprise_override":"personal","custom_note":"keep-me"}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	a, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if a.EnterpriseOverride != "personal" {
		t.Fatalf("旧扁平文件的 override 没读到: %q", a.EnterpriseOverride)
	}
	if a.IsEnterprise() {
		t.Fatal("override=personal 应压过 type=ultimate")
	}
	a.FilePath = path
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	// 升级成嵌套形后：token 保留、未知键保留、override 仍在。
	saved, _ := os.ReadFile(path)
	var doc map[string]any
	if err := json.Unmarshal(saved, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["custom_note"] != "keep-me" {
		t.Fatalf("未知键被丢了: %v", doc)
	}
	acc, _ := doc["account"].(map[string]any)
	if acc == nil || acc["enterprise_override"] != "personal" {
		t.Fatalf("override 未写回 account: %v", doc)
	}
	// 扁平遗留键应被清掉（避免外部工具读到过期 token）。
	if _, ok := doc["accessToken"]; ok {
		t.Fatal("扁平遗留 accessToken 应被清理")
	}
}

// TestEnterpriseOverrideClearedRemovesKey 清空覆盖 = 恢复自动判定，文件里不该留空键
// （外部工具会把 "enterprise_override": "" 读成「显式设了空」）。
func TestEnterpriseOverrideClearedRemovesKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workbuddy-u2.json")
	a := &Auth{UID: "u2", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1760000000,
		FilePath: path, EnterpriseOverride: "enterprise"}
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	if err := a.SetEnterpriseOverride(""); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	acc, _ := doc["account"].(map[string]any)
	if acc != nil {
		if _, ok := acc["enterprise_override"]; ok {
			t.Fatalf("清空后不该留键: %v", acc)
		}
	}
	// 重新解析：覆盖为空 → 回到自动判定。
	back, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if back.EnterpriseOverride != "" {
		t.Fatalf("重启后 override 应为空: %q", back.EnterpriseOverride)
	}
}

func TestNormalizeEnterpriseOverride(t *testing.T) {
	cases := map[string]string{
		"": "", "auto": "", "0": "",
		"enterprise": "enterprise", "ENT": "enterprise", "企业版": "enterprise",
		"personal": "personal", "个人": "personal", "Personal ": "personal",
		"whatever": "",
	}
	for in, want := range cases {
		if got := NormalizeEnterpriseOverride(in); got != want {
			t.Errorf("NormalizeEnterpriseOverride(%q)=%q want %q", in, got, want)
		}
	}
}

// TestPersonalOverrideBypassesEnterpriseSession 「企业切换会话」是第二层判据：
// 只覆盖 IsEnterprise() 不够——面板改成个人版后任务面板仍回「企业上下文」，
// 用户看到的就是「我改了但没用」（实测反馈）。
func TestPersonalOverrideBypassesEnterpriseSession(t *testing.T) {
	// 构造 token_source=enterprise_switch 的合成 JWT（不校验签名）。
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"token_source":"enterprise_switch"}`))
	tok := "h." + payload + ".s"

	a := &Auth{UID: "u9", AccessToken: tok}
	if !a.IsEnterpriseSession() {
		t.Fatal("前置：企业切换令牌应判企业上下文")
	}
	if !a.IsEnterpriseToken() {
		t.Fatal("前置：IsEnterpriseToken 应为 true")
	}
	a.EnterpriseOverride = "personal"
	if a.IsEnterpriseSession() {
		t.Fatal("强制个人后不该再判企业上下文")
	}
	// 令牌事实本身不变（告警仍能看见）。
	if !a.IsEnterpriseToken() {
		t.Fatal("IsEnterpriseToken 只反映令牌，不受覆盖影响")
	}
	// 自动判定（无覆盖）时行为不变。
	a.EnterpriseOverride = ""
	if !a.IsEnterpriseSession() {
		t.Fatal("无覆盖时应回到企业上下文")
	}
}
