package httpauth

// 第二轮代码审计（CODE-AUDIT-FULL.md 10.9 / W2-AU-01）的回归用例。
//
// W2-AU-01：key == "" 时 VerifyBearer/VerifyDual 恒 true = fail-open。
// 修法不是把默认改成 fail-closed（那会让所有「回环 + 无密钥」部署直接 401，
// 也推翻 server/panel 的既有语义），而是给一个**显式可关**的进程级开关，
// 并在用例里把两条语义都钉住。

import (
	"net/http/httptest"
	"testing"
)

// 默认语义不变：空 key 仍表示「未启用鉴权」，既有部署零回归。
func TestEmptyKeyDefaultStillFailOpen(t *testing.T) {
	if !AllowEmptyKey() {
		t.Fatal("默认必须是 fail-open（空 key = 未启用鉴权）")
	}
	r := httptest.NewRequest("GET", "/", nil)
	if !VerifyBearer(r, "") {
		t.Error("默认语义下空 key 应放行 VerifyBearer")
	}
	if !VerifyDual(r, "") {
		t.Error("默认语义下空 key 应放行 VerifyDual")
	}
}

// 显式关闭后：空 key 的非回环请求一律拒绝（配置漏填密钥时必须能失败关闭）。
// W5-04 起回环请求仍放行，见 w5_04_loopback_test.go；本用例的 httptest 默认
// RemoteAddr 是 192.0.2.1（非回环），覆盖的正是「远端必须被拒」。
func TestEmptyKeyFailClosedWhenDisabled(t *testing.T) {
	SetAllowEmptyKey(false)
	t.Cleanup(func() { SetAllowEmptyKey(true) })

	if AllowEmptyKey() {
		t.Fatal("开关未生效")
	}
	// 带不带头都拒：空 key 下没有任何请求应当通过。
	for _, hdr := range []string{"", "Bearer anything", "Bearer "} {
		r := httptest.NewRequest("GET", "/", nil)
		if hdr != "" {
			r.Header.Set("Authorization", hdr)
		}
		if VerifyBearer(r, "") {
			t.Errorf("关闭后 VerifyBearer 仍放行（authz=%q）", hdr)
		}
		if VerifyDual(r, "") {
			t.Errorf("关闭后 VerifyDual 仍放行（authz=%q）", hdr)
		}
	}
}

// 开关只影响「空 key」分支：非空 key 的比较语义与常量时间路径完全不变。
func TestNonEmptyKeyUnaffectedBySwitch(t *testing.T) {
	SetAllowEmptyKey(false)
	t.Cleanup(func() { SetAllowEmptyKey(true) })

	const key = "sk-abc123"
	ok := httptest.NewRequest("GET", "/", nil)
	ok.Header.Set("Authorization", "Bearer "+key)
	if !VerifyBearer(ok, key) {
		t.Error("正确密钥不该被开关影响")
	}
	bad := httptest.NewRequest("GET", "/", nil)
	bad.Header.Set("Authorization", "Bearer nope")
	if VerifyBearer(bad, key) {
		t.Error("错误密钥必须被拒")
	}
}
