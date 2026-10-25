package main

// 登录业务码分流（对齐 CLI 2.174.0 的 SignLicenseError）。
//
// 终态许可错误必须立刻把上游 msg 抛给用户并退出，不能落进「登录未完成（请确认已在
// 浏览器完成登录）」那句无意义提示 —— 用户会一直重试一个永远不会成功的登录。
// 没有可登录的真实账号时，靠 httptest 把判据钉死。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/loginsite"
)

// captureExit 把 fatal 的退出改成进程内可捕获。
func captureExit(t *testing.T) *int {
	t.Helper()
	old := exitFunc
	code := -1
	exitFunc = func(c int) { code = c; panic("exit") }
	t.Cleanup(func() { exitFunc = old })
	return &code
}

func writeState(t *testing.T, state, realm string) string {
	t.Helper()
	raw, _ := json.Marshal(loginState{State: state, Realm: realm})
	p := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
	return p
}

// runPollExpectFatal 跑一次 poll，断言它走了 fatal（而非打印「请确认已登录」）。
func runPollExpectFatal(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	code := captureExit(t)
	var errBuf bytes.Buffer
	oldW := errWriter
	errWriter = &errBuf
	t.Cleanup(func() { errWriter = oldW })
	var out bytes.Buffer
	func() {
		defer func() { _ = recover() }() // fatal → panic("exit")
		runPoll(srv.URL, "https://www.codebuddy.cn", realmCN, writeState(t, "st-1", realmCN), srv.Client(), &out)
	}()
	if *code != 1 {
		t.Fatalf("应走 fatal 退出（code=1），实际 code=%d stderr=%s", *code, errBuf.String())
	}
	return errBuf.String()
}

func TestPollFatalLicenseCodes(t *testing.T) {
	for _, c := range []int{loginsite.CodeLicenseSeatLimit, loginsite.CodeLicenseExpired, loginsite.CodeTrialExpired} {
		body := `{"code":` + itoa(c) + `,"msg":"许可已过期，请续费"}`
		msg := runPollExpectFatal(t, body)
		// 必须把**上游 msg** 抛给用户，而不是那句无意义的「请确认已在浏览器完成登录」。
		if !strings.Contains(msg, "许可已过期") {
			t.Fatalf("code=%d 没透出上游 msg: %s", c, msg)
		}
		if strings.Contains(msg, "waiting for login") {
			t.Fatalf("code=%d 落进了 pending 提示: %s", c, msg)
		}
	}
}

// 反向保护：pending 码不该走 fatal（要继续提示用户去浏览器完成登录）。
func TestPollPendingCodeDoesNotFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":11217,"msg":"login ing"}`))
	}))
	defer srv.Close()
	code := captureExit(t)
	var errBuf bytes.Buffer
	oldW := errWriter
	errWriter = &errBuf
	t.Cleanup(func() { errWriter = oldW })
	var out bytes.Buffer
	func() {
		defer func() { _ = recover() }()
		runPoll(srv.URL, "https://www.codebuddy.cn", realmCN, writeState(t, "st-2", realmCN), srv.Client(), &out)
	}()
	if *code != 1 {
		t.Fatalf("pending 码也应 fatal 退出（提示用户去登录），code=%d", *code)
	}
	if !strings.Contains(errBuf.String(), "waiting for login") {
		t.Fatalf("pending 码应提示去浏览器完成登录，实际: %s", errBuf.String())
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
