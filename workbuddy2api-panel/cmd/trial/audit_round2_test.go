package main

// 第二轮代码审计（CODE-AUDIT-FULL.md 10.16 / W2-C2-01~06）的回归用例。

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// ---------------------------------------------------------------- W2-C2-01 ----

// 审查 W2-C2-01：err.Error() 此前直接拼进报表并打到 stdout（docker exec 日志/
// CI 日志/工单附件都会留存）。默认只输出类别码，原文走 -debug 且必须先脱敏。
func TestClassifyTrialHidesRawErrorByDefault(t *testing.T) {
	debugDetail = false
	t.Cleanup(func() { debugDetail = false })

	raw := errors.New("post https://api.example.com/v1/trial: Bearer ck_abcdefghijklmnop 401")
	st, detail := classifyTrial(false, raw)
	if st != trialFailed {
		t.Fatalf("status=%v", st)
	}
	if detail != errKindClaim {
		t.Fatalf("detail 应只含类别码, 得到 %q", detail)
	}
	if strings.Contains(detail, "ck_") || strings.Contains(detail, "http") {
		t.Fatalf("detail 泄露了原文: %q", detail)
	}
}

// -debug 打开后仍不能输出凭据：Bearer / access_token 之类一律脱敏。
func TestFailureDetailRedactsWhenDebug(t *testing.T) {
	debugDetail = true
	t.Cleanup(func() { debugDetail = false })

	raw := errors.New(`post https://api.example.com/v1/trial: Bearer ck_abcdefghijklmnop ` +
		`{"access_token":"at_1234567890","refresh_token":"rt_0987654321"} 401`)
	detail := failureDetail(&trialError{kind: errKindClaim, err: raw})
	if !strings.HasPrefix(detail, errKindClaim+": ") {
		t.Fatalf("debug 下应带类别码前缀: %q", detail)
	}
	for _, secret := range []string{"ck_abcdefghijklmnop", "at_1234567890", "rt_0987654321"} {
		if strings.Contains(detail, secret) {
			t.Fatalf("凭据未脱敏（%s）: %q", secret, detail)
		}
	}
	if !strings.Contains(detail, "***") {
		t.Fatalf("应留下脱敏占位: %q", detail)
	}
}

func TestRedactDetailTruncates(t *testing.T) {
	got := redactDetail(strings.Repeat("x", 500))
	if len(got) > 120 {
		t.Fatalf("未截断: len=%d", len(got))
	}
}

// ---------------------------------------------------------------- W2-C2-02 ----

// 审查 W2-C2-02：读-解析失败时重读一次（网关 bind-mount 兜底路径是 O_TRUNC
// 原地覆写，并发时可能读到半截文件，重试即可跨过窗口）。
func TestLoadAuthFileRetriesOnce(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "workbuddy-u1.json")
	if err := os.WriteFile(good, []byte(validAuthJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := loadAuthFile(good)
	if err != nil {
		t.Fatalf("合法文件应解析成功: %v", err)
	}
	if a.UID != "u1" {
		t.Fatalf("uid=%q", a.UID)
	}

	// 真坏文件：重试后仍失败 → E_PARSE（不是 E_READ）。
	bad := filepath.Join(dir, "workbuddy-u2.json")
	if err := os.WriteFile(bad, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAuthFile(bad); err == nil {
		t.Fatal("坏文件必须报错")
	} else if kind := errKindOf(err); kind != errKindParse {
		t.Fatalf("类别码=%s, 期望 %s", kind, errKindParse)
	}

	// 文件不存在：E_READ。
	if _, err := loadAuthFile(filepath.Join(dir, "nope.json")); errKindOf(err) != errKindRead {
		t.Fatalf("类别码=%s, 期望 %s", errKindOf(err), errKindRead)
	}
}

const validAuthJSON = `{"auth":{"accessToken":"ck_abcdefgh","refreshToken":"rt_abcdefgh",` +
	`"expiresAt":1893456000000,"domain":"www.workbuddy.cn"},` +
	`"account":{"uid":"u1","nickname":"甲"}}`

// ---------------------------------------------------------------- W2-C2-03 ----

// 审查 W2-C2-03：扫描口径必须与网关 auth.LoadDirReport 一致（workbuddy*.json），
// 且目录读不出来时要报错，不能像 filepath.Glob 那样吞成「0 个文件」。
func TestListAuthFilesMatchesGatewayGlob(t *testing.T) {
	dir := t.TempDir()
	want := []string{"workbuddy-a.json", "workbuddy-old.json", "workbuddy.json"}
	for _, n := range append([]string{"other.json", "workbuddy.json.bak", "notes.txt"}, want...) {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// 目录同名（不应被当成账号文件）。
	if err := os.Mkdir(filepath.Join(dir, "workbuddy-dir.json"), 0o700); err != nil {
		t.Fatal(err)
	}

	files, err := listAuthFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		names = append(names, filepath.Base(f))
	}
	if len(names) != len(want) {
		t.Fatalf("文件=%v want=%v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("文件=%v want=%v（口径必须与 auth.LoadDirReport 一致）", names, want)
		}
	}

	// 目录不存在必须报错（Glob 会返回空列表 + nil）。
	if _, err := listAuthFiles(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("目录不存在应报错")
	}
}

// ---------------------------------------------------------------- W2-C2-04 ----

// 审查 W2-C2-04：报表全量打印 uid（账号标识）会随日志外泄。默认打码。
func TestMaskUID(t *testing.T) {
	const uid = "u_1234567890abcdef"
	if got := maskUID(uid, true); got != "u_123456" {
		t.Fatalf("打码结果=%q（应只留前 8 位，与 logfmt.UID8 同口径）", got)
	}
	if got := maskUID(uid, false); got != uid {
		t.Fatalf("-mask=false 应输出完整 uid, 得到 %q", got)
	}
	if got := maskUID("", true); got != "-" {
		t.Fatalf("空 uid 占位=%q", got)
	}
}

// 解析失败的行没有 uid，只能靠文件名定位：打码时保留前缀/后缀与 uid 前 6 位，
// 否则多个失败行在报表里长得一模一样（"workbuddy"）。
func TestMaskAuthFileName(t *testing.T) {
	cases := map[string]string{
		"workbuddy-u_1234567890abcdef.json": "workbuddy-u_1234….json",
		"workbuddy-a.json":                  "workbuddy-a.json",
		"workbuddy.json":                    "workbuddy.json",
		"":                                  "-",
	}
	for in, want := range cases {
		if got := maskAuthFileName(in); got != want {
			t.Errorf("maskAuthFileName(%q)=%q want %q", in, got, want)
		}
	}
	// rowIdent：成功行用 uid，失败行用文件名。
	if got := rowIdent(trialRow{uid: "u_1234567890", file: "workbuddy-x.json"}, true); got != "u_123456" {
		t.Errorf("成功行应打码 uid, 得到 %q", got)
	}
	if got := rowIdent(trialRow{file: "workbuddy-x.json"}, true); got != "workbuddy-x.json" {
		t.Errorf("失败行应用文件名, 得到 %q", got)
	}
	if got := rowIdent(trialRow{uid: "u_1234567890"}, false); got != "u_1234567890" {
		t.Errorf("-mask=false 应输出完整 uid, 得到 %q", got)
	}
}

// ---------------------------------------------------------------- W2-C2-05 ----

// 审查 W2-C2-05：truncBytes 是死代码（无调用方），且按字节切会切坏 UTF-8。
// 这条用例的存在意义是「删掉之后仍能编译」——它断言按 rune 截断的正确实现还在。
func TestTruncIsRuneSafe(t *testing.T) {
	got := trunc("中文昵称测试", 7) // 7 字节切在第三个汉字中间
	if !strings.HasPrefix("中文昵称测试", got) || len(got) > 7 {
		t.Fatalf("截断结果=%q", got)
	}
	for _, r := range got {
		if r == '\uFFFD' {
			t.Fatalf("切出了半个字符: %q", got)
		}
	}
}

// ---------------------------------------------------------------- W2-C2-06 ----

// 审查 W2-C2-06：realm 三态。此前只有 IsGlobal()/非 global 两态，「判不出来」
// 被当成 CN 静默跳过，或反过来给 CN 号打 global 端点。
func TestRealmOfTriState(t *testing.T) {
	// 显式 realm 优先。
	explicit := &auth.Auth{Domain: "www.workbuddy.cn"}
	if _, err := auth.BackfillRealmFor(explicit, "global"); err != nil {
		t.Fatal(err)
	}
	if got := realmOf(explicit); got != "global" {
		t.Fatalf("显式 realm 应优先（domain 是 cn）: %q", got)
	}

	// 未落盘 realm 的旧文件：按 domain 后缀推断（与 auth.BackfillRealm 同口径）。
	byDomain := &auth.Auth{Domain: "www.workbuddy.ai"}
	if got := realmOf(byDomain); got != "global" {
		t.Fatalf("domain 推断=%q, 期望 global", got)
	}
	cnDomain := &auth.Auth{Domain: "www.workbuddy.cn"}
	if got := realmOf(cnDomain); got != "cn" {
		t.Fatalf("domain 推断=%q, 期望 cn", got)
	}

	// 既无 realm 也无 domain：判不出来 → ""（调用方据此标 UNKNOWN 并跳过）。
	if got := realmOf(&auth.Auth{}); got != "" {
		t.Fatalf("无 realm/domain 应返回空（unknown）, 得到 %q", got)
	}
	if got := realmOf(nil); got != "" {
		t.Fatalf("nil 应返回空, 得到 %q", got)
	}
}
