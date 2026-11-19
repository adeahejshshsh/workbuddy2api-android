// debugdump_test.go W1-01 回归：WB2A_DEBUG_CHAT 的调试输出不得泄露凭据。
//
// 判据是「输出里没有明文凭据」，不是「输出长得像什么」：只要 token 的子串
// 出现在日志里，导出一次 docker logs 就等于交出账号池。
package upstream

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

func TestDebugDumpMasksCredentials(t *testing.T) {
	const (
		access = "eyJhbGciOiJIUzI1NiJ9.access-token-payload"
		device = "dev-token-0123456789abcdef"
		bearer = "Bearer " + access
	)
	req, err := http.NewRequest(http.MethodPost, "https://www.codebuddy.cn/v2/chat/completions", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", bearer)
	req.Header.Set("X-Device-Token", device)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", "req-1")
	// 账号可识别信息也不该进日志（白名单外）。
	req.Header.Set("X-User-Id", "uid-should-not-appear")
	req.Header.Set("X-Domain", "corp.example.com")

	var buf bytes.Buffer
	dumpDebugChatRequest(&buf, req)
	out := buf.String()

	// 1) 凭据本体一个字节都不能出现。
	if strings.Contains(out, access) {
		t.Fatalf("access token 明文进日志：\n%s", out)
	}
	if strings.Contains(out, device) {
		t.Fatalf("X-Device-Token 明文进日志：\n%s", out)
	}
	if strings.Contains(out, "uid-should-not-appear") || strings.Contains(out, "corp.example.com") {
		t.Fatalf("白名单外的账号头进日志：\n%s", out)
	}
	// 2) 该留的诊断信息要留：白名单头 + 打码后的 Authorization（前 8 位 + 长度）。
	if !strings.Contains(out, "Content-Type: application/json") || !strings.Contains(out, "X-Request-ID: req-1") {
		t.Fatalf("白名单头没打印，排障信息丢失：\n%s", out)
	}
	want := "Bearer " + access[:8] + "… (len=" + itoaTest(int64(len(access))) + ")"
	if !strings.Contains(out, want) {
		t.Fatalf("Authorization 未按「前 8 位 + 长度」打码，期望含 %q：\n%s", want, out)
	}
	// 3) 不能出现完整的 Authorization 行（哪怕是方案 + 全 token）。
	if strings.Contains(out, "Bearer "+access) {
		t.Fatalf("Authorization 未打码：\n%s", out)
	}
}

// 短凭据连前 8 位也不给（那种长度下前缀已是大半把 key）。
func TestMaskDebugSecretShortFullyHidden(t *testing.T) {
	const short = "abcdefghijkl" // 12 <= 2*8
	got := maskDebugSecret(short)
	if strings.Contains(got, "abcdefgh") {
		t.Fatalf("短凭据泄露了前缀：%q", got)
	}
	if !strings.Contains(got, "len=12") {
		t.Fatalf("短凭据应保留长度信息：%q", got)
	}
}
