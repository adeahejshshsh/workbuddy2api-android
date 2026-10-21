package upstream

import (
	"net/http"
	"testing"
)

// PR #233 + kk-cao 修正：11‑-128 通道校验必须与 11140 账号限制分开，
// 且业务码要按**纯数字归一**比较 —— 真实报文是 `{"code":11‑-128,...}`
// （数字里带连字符，不是合法 JSON），按字面比较永远不命中。
func TestClassifyChannelRefusedVsAccountRestricted(t *testing.T) {
	// 真实形态：数字里带连字符，JSON 非法 → 只能靠正则兜底命中
	real := `{"code":11‑-128,"msg":"request illegal"}`
	if got := Classify(http.StatusForbidden, real); got != ErrChannelRefused {
		t.Fatalf("真实报文判成 %v，期望 ErrChannelRefused", got)
	}
	// 日志/终端渲染形态（带连字符的字符串）也要命中
	rendered := `{"code":"11‑-128","msg":"request illegal"}`
	if got := Classify(http.StatusForbidden, rendered); got != ErrChannelRefused {
		t.Fatalf("渲染形态判成 %v，期望 ErrChannelRefused", got)
	}
	// 11140 账号限制 → 不是通道
	ac := `{"code":11140,"msg":"request illegal"}`
	if got := Classify(http.StatusForbidden, ac); got == ErrChannelRefused {
		t.Fatal("11140 被误判成通道校验失败")
	}
	// 11‑-128 的内容审核变体仍归 ErrContentBlocked
	cb := `{"code":11‑-128,"msg":"blocked by security policy"}`
	if got := Classify(http.StatusBadRequest, cb); got != ErrContentBlocked {
		t.Fatalf("内容审核变体判成 %v，期望 ErrContentBlocked", got)
	}
	// 双条件：只有文案没有 code 时不算账号限制
	if IsAccountRestricted(`{"msg":"request illegal"}`) {
		t.Fatal("缺 code 时不应判成账号限制")
	}
	if !IsAccountRestricted(ac) {
		t.Fatal("11140 + request illegal 应判成账号限制")
	}
	// 11140 同时带模型级限流文案时不算账号限制（误禁用代价更高）
	rateLimited := `{"code":11140,"msg":"request illegal","displayMsg":"The model provider is rate-limiting requests."}`
	if IsAccountRestricted(rateLimited) {
		t.Fatal("带限流文案的 11140 不应判成账号限制（会误禁用健康账号）")
	}
}

// 业务码归一：三种写法必须归到同一个键。
func TestNormalizeBusinessCode(t *testing.T) {
	// 三种写法（无连字符 / ASCII 连字符 / U+2011 非断连字符）必须归一到 "11128"
	want := "11128"
	for _, in := range []string{"11" + "128", "11-" + "128", "11\u2011-" + "128"} {
		if got := normalizeBusinessCode(in); got != want {
			t.Fatalf("normalizeBusinessCode(%q) = %q, want %q", in, got, want)
		}
	}
}
