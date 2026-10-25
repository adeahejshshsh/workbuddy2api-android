package auth

// 凭据导入的两个归一/域表修正（上游 2026-10-24 `f0c427d`）：
//   1) expiresAt 单位：官方 CLI 写毫秒，本仓内部按秒；
//   2) isGlobalDomain 要认官方 CLI 国际站域 *.codebuddy.ai。
//
// 两者都不改会「静默」出错：前者让 token 永不刷新（一个月后全 401），
// 后者让国际站账号被判成 cn（错域路由，token 只在签发域可用）。

import "testing"

func TestNormalizeExpiresAt(t *testing.T) {
	cases := []struct {
		name string
		in   int64
		want int64
	}{
		{"秒不动", 1795254516, 1795254516},
		{"毫秒归一", 1795254516410, 1795254516},
		{"零值不动", 0, 0},
		{"负数不动（视为未设置）", -1, -1},
		{"阈值本身按秒处理", expiresAtMillisThreshold, expiresAtMillisThreshold},
		{"阈值 +1 按毫秒处理", expiresAtMillisThreshold + 1, (expiresAtMillisThreshold + 1) / 1000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := normalizeExpiresAt(c.in); got != c.want {
				t.Fatalf("normalizeExpiresAt(%d)=%d want %d", c.in, got, c.want)
			}
		})
	}
}

// 两条解析分支（嵌套形 / 扁平形）都必须归一：只修一条，另一种形态照样静默失效。
func TestParseNormalizesExpiresAtBothShapes(t *testing.T) {
	nested := []byte(`{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1795254516410,"domain":"www.workbuddy.ai"},"account":{"uid":"u1"}}`)
	flat := []byte(`{"accessToken":"at","refreshToken":"rt","expiresAt":1795254516410,"domain":"www.workbuddy.ai","uid":"u1"}`)
	for name, raw := range map[string][]byte{"嵌套形": nested, "扁平形": flat} {
		t.Run(name, func(t *testing.T) {
			a, err := Parse(raw)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if a.ExpiresAt != 1795254516 {
				t.Fatalf("expiresAt=%d want 1795254516（毫秒必须归一成秒）", a.ExpiresAt)
			}
		})
	}
}

// 官方 CLI 国际站域 www.codebuddy.ai 必须算 global，否则国际站账号被判成 cn。
func TestIsGlobalDomainRecognizesCodebuddyAI(t *testing.T) {
	for _, d := range []string{"www.codebuddy.ai", "codebuddy.ai", "WWW.CodeBuddy.AI", "www.workbuddy.ai", "workbuddy.ai"} {
		if !isGlobalDomain(d) {
			t.Errorf("isGlobalDomain(%q)=false want true", d)
		}
	}
	for _, d := range []string{"copilot.tencent.com", "www.codebuddy.cn", "codebuddy.ai.evil.test", ""} {
		if isGlobalDomain(d) {
			t.Errorf("isGlobalDomain(%q)=true want false", d)
		}
	}
}
