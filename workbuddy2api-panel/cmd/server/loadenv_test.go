package main

import (
	"os"
	"path/filepath"
	"testing"
)

// bug P0-1 回归：最常见的一种部署形态是「config.json 只写 listen，密钥走环境变量」。
// 此前的 Load 先 ParseConfigInto（内部就 normalize → checkListenKey 用文件里的空 key），
// 在 applyEnv 之前就把进程拦下：「不是回环地址，必须配置非空 api_key」，尽管
// WB2A_API_KEY 明明有值。
func TestLoadEnvKeySatisfiesNonLoopbackListen(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "config.json")
	body := `{"listen":"0.0.0.0:7863","api_key":""}`
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WB2A_API_KEY", "sk-from-env")

	c, err := Load(fp)
	if err != nil {
		t.Fatalf("Load 失败（env 里的 api_key 必须在 normalize 之前生效）: %v", err)
	}
	if c.APIKey != "sk-from-env" {
		t.Fatalf("APIKey=%q want sk-from-env", c.APIKey)
	}
	if c.Listen != "0.0.0.0:7863" {
		t.Errorf("Listen=%q", c.Listen)
	}
}

// 对照：没有 env 也没配 key 时，非回环监听仍必须被拦下（守卫不能被削弱）。
func TestLoadNonLoopbackWithoutKeyStillFails(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "config.json")
	body := `{"listen":"0.0.0.0:7863","api_key":""}`
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WB2A_API_KEY", "")
	if _, err := Load(fp); err == nil {
		t.Fatal("非回环 + 空 key 必须报错")
	}
}

// bug P1-2 回归：把配置项写成 ""（用户以为"删掉 = 用默认"）不能导致启动失败。
func TestNormalizeEmptyDurationFieldsFallBack(t *testing.T) {
	raw := []byte(`{
		"listen":"127.0.0.1:7863",
		"cooldown":{"soft_rate":""},
		"session_sticky":{"ttl":"","gc_interval":""}
	}`)
	c, err := ParseConfig(raw)
	if err != nil {
		t.Fatalf("空时长必须回落默认而不是报错: %v", err)
	}
	if c.SoftRateDur != 600e9 { // 600s
		t.Errorf("SoftRateDur=%v want 600s", c.SoftRateDur)
	}
	if c.SessionTTL != 30*60e9 {
		t.Errorf("SessionTTL=%v want 30m", c.SessionTTL)
	}
	if c.SessionGCInterval != 5*60e9 {
		t.Errorf("SessionGCInterval=%v want 5m", c.SessionGCInterval)
	}
}
