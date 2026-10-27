package main

// 启动横幅（上游 2026-10-27 `297c78a` 的落点）。
//
// 横幅要回答两个排障问题：「密钥是不是我设的那把」「它到底从哪儿读的」。
// 前者靠掩码（不能泄露，但要能核对首尾），后者靠来源（env / 文件 / 内置默认）——
// 后两种情况下改配置文件都不会生效，排查方向完全不同（issue #155）。

import (
	"strings"
	"testing"
)

func TestMaskSecret(t *testing.T) {
	cases := map[string]string{
		"":          "(未设置)",
		"a":         "*",
		"abcdefg":   "*******",
		"abcdefgh":  "ab****gh",
		"sk-123456": "sk*****56",
	}
	for in, want := range cases {
		if got := maskSecret(in); got != want {
			t.Errorf("maskSecret(%q)=%q want %q", in, got, want)
		}
	}
	// 长密钥同样只留首尾各两位：中间一律星号，不泄露长度之外的任何信息。
	long := "ck_abcdefghijklmnopqrstuvwxyz"
	got := maskSecret(long)
	if !strings.HasPrefix(got, "ck") || !strings.HasSuffix(got, "yz") {
		t.Fatalf("首尾应保留两位: %q", got)
	}
	if strings.Contains(got, "defgh") {
		t.Fatalf("中段泄露了明文: %q", got)
	}
}

func TestAPIKeySource(t *testing.T) {
	t.Setenv("WB2A_API_KEY", "")
	if got := apiKeySource(""); got != "内置默认值" {
		t.Errorf("无文件无 env: got %q", got)
	}
	if got := apiKeySource("config.json"); got != "配置文件 config.json" {
		t.Errorf("有文件: got %q", got)
	}
	t.Setenv("WB2A_API_KEY", "k-from-env")
	if got := apiKeySource("config.json"); got != "环境变量 WB2A_API_KEY" {
		t.Errorf("env 优先（改配置文件不生效）: got %q", got)
	}
}

func TestListenBanner(t *testing.T) {
	got := listenBanner("127.0.0.1:7863", "config.json", "sk-secret-value")
	if strings.Contains(got, "sk-secret-value") {
		t.Fatalf("横幅泄露了完整密钥: %s", got)
	}
	for _, want := range []string{"127.0.0.1:7863", "配置文件 config.json", "/panel/"} {
		if !strings.Contains(got, want) {
			t.Errorf("横幅缺 %q: %s", want, got)
		}
	}
}
