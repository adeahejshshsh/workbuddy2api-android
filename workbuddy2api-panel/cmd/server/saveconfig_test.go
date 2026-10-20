package main

import (
	"testing"
)

// B2 回归：这些字段在启动期被装配进 upstream/handler/auth，保存路径不碰它们，
// 必须出现在「需重启」清单里，否则面板会谎报「已立即生效」。
func TestRestartRequiredFieldsCoversAssemblyTimeFields(t *testing.T) {
	must := []string{
		"global.enabled", "global.chat_base", "global.billing_base",
		"upstream.client_version", "upstream.cli_version", "upstream.client_name",
		"upstream.device_token", "upstream.device_token_file", "upstream.passthrough_ip",
		"prompt.mode", "prompt.file",
		"server.max_body_mb",
	}
	// 全字段都"变了"：清单应当把它们全部报出来。
	prev := map[string]any{}
	next := map[string]any{}
	for _, k := range must {
		setDotted(next, k, "changed-"+k)
	}
	got := restartRequiredFields(prev, next)
	seen := map[string]bool{}
	for _, k := range got {
		seen[k] = true
	}
	for _, k := range must {
		if !seen[k] {
			t.Errorf("restart_required 清单缺 %q —— 面板会提示「已立即生效」，实际直到重启都不变", k)
		}
	}
}

// 没变化的字段不该出现在清单里（避免"狼来了"：每次保存都提示重启）。
func TestRestartRequiredFieldsOnlyChanged(t *testing.T) {
	prev := map[string]any{}
	next := map[string]any{}
	setDotted(prev, "listen", "127.0.0.1:7863")
	setDotted(next, "listen", "127.0.0.1:7863") // 相同
	setDotted(prev, "auth_dir", "./auths")
	setDotted(next, "auth_dir", "./auths2") // 变了

	got := restartRequiredFields(prev, next)
	if len(got) != 1 || got[0] != "auth_dir" {
		t.Fatalf("got %v, want 只有 auth_dir", got)
	}
}

// B1 回归：保存路径必须先 applyEnv 再 normalize，否则 WB2A_API_KEY 会被文件值覆盖。
// 这里直接钉住 applyEnv 的语义：它把 env 值写进 Config，且对未设置字段不动作。
func TestApplyEnvOverridesConfigFileValue(t *testing.T) {
	t.Setenv("WB2A_API_KEY", "sk-from-env")
	t.Setenv("WB2A_SOFT_RATE", "900s")

	c := Default()
	c.APIKey = "sk-from-file" // 模拟"从 config.json 读出来的值"
	c.Cooldown.SoftRate = "600s"

	applyEnv(c)

	if c.APIKey != "sk-from-env" {
		t.Fatalf("applyEnv 未覆盖 api_key: %q（保存路径漏了这一步就会把鉴权密钥清空）", c.APIKey)
	}
	if c.Cooldown.SoftRate != "900s" {
		t.Fatalf("applyEnv 未覆盖 soft_rate: %q", c.Cooldown.SoftRate)
	}
}

// setDotted 按 "a.b.c" 写入嵌套 map（测试辅助）。
func setDotted(m map[string]any, path string, v any) {
	cur := m
	parts := splitDots(path)
	for i, seg := range parts {
		if i == len(parts)-1 {
			cur[seg] = v
			return
		}
		next, ok := cur[seg].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[seg] = next
		}
		cur = next
	}
}

func splitDots(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '.' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	return append(out, cur)
}
