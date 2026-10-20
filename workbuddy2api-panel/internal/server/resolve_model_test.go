package server

import "testing"

// TestResolveModelRealm 钉住 realm 前缀协议本身：只有精确的小写 cn/global 才剥离，
// 其它（含大写、未知前缀、无前缀）一律按裸名 + cn 处理。
func TestResolveModelRealm(t *testing.T) {
	cases := []struct {
		in          string
		realm, bare string
	}{
		{"deepseek-v4.1-flash", "cn", "deepseek-v4.1-flash"},
		{"cn:deepseek-v4.1-flash", "cn", "deepseek-v4.1-flash"},
		{"global:deepseek-v4.1-flash", "global", "deepseek-v4.1-flash"},
		{"CN:deepseek-v4.1-flash", "cn", "CN:deepseek-v4.1-flash"}, // 前缀大小写敏感
		{"us:deepseek-v4.1-flash", "cn", "us:deepseek-v4.1-flash"}, // 未知前缀不剥离
		{"cn:", "cn", ""},
		{"", "cn", ""},
	}
	for _, c := range cases {
		realm, bare := resolveModel(c.in)
		if realm != c.realm || bare != c.bare {
			t.Errorf("resolveModel(%q) = (%q,%q)，期望 (%q,%q)", c.in, realm, bare, c.realm, c.bare)
		}
	}
}

// TestResolveModelAlias 钉住兼容别名：官方名 deepseek-flash 必须落到 WorkBuddy 目录里
// 的真名 deepseek-v4.1-flash，且带不带 realm 前缀都生效。
//
// 为什么要这个别名：DSH 的模型标签按官方 id "deepseek-flash" 判定 1017，
// 而 WorkBuddy 上游只认 "deepseek-v4.1-flash"——网关不做映射，请求会原样转给上游
// 被按未知模型拒绝（换号无效）。
func TestResolveModelAlias(t *testing.T) {
	cases := []struct {
		in          string
		realm, bare string
	}{
		{"deepseek-flash", "cn", "deepseek-v4.1-flash"},
		{"cn:deepseek-flash", "cn", "deepseek-v4.1-flash"},
		{"global:deepseek-flash", "global", "deepseek-v4.1-flash"},
		// 真名不受影响（幂等）
		{"deepseek-v4.1-flash", "cn", "deepseek-v4.1-flash"},
		{"global:deepseek-v4.1-flash", "global", "deepseek-v4.1-flash"},
		// 近似名一个都不能被误映射：v4 不是 v4.1，-sg 是另一条线
		{"deepseek-v4-flash", "cn", "deepseek-v4-flash"},
		{"deepseek-flash-sg", "cn", "deepseek-flash-sg"},
		{"DeepSeek-Flash", "cn", "DeepSeek-Flash"},
		{"deepseek-flashx", "cn", "deepseek-flashx"},
	}
	for _, c := range cases {
		realm, bare := resolveModel(c.in)
		if realm != c.realm || bare != c.bare {
			t.Errorf("resolveModel(%q) = (%q,%q)，期望 (%q,%q)", c.in, realm, bare, c.realm, c.bare)
		}
	}
}

// TestAliasModelIdempotent 别名表自身不许出现指向别名、或指向不存在模型的死链。
func TestAliasModelIdempotent(t *testing.T) {
	for from, to := range modelAliases {
		if from == to {
			t.Errorf("别名 %q 指向自己", from)
		}
		if _, again := modelAliases[to]; again {
			t.Errorf("别名 %q → %q 是二段跳，请直接指向最终真名", from, to)
		}
		if got := aliasModel(to); got != to {
			t.Errorf("aliasModel(%q) = %q，真名必须原样返回", to, got)
		}
	}
}
