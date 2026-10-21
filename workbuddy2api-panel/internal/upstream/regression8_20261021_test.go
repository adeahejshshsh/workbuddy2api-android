package upstream

import (
	"net/http"
	"testing"
)

// PR #233：11‑-128 通道校验失败必须与 11140 账号限制分开
// （两者 msg 同为 "request illegal"，只看文案会把健康账号硬禁用）。
func TestClassifyChannelRefusedVsAccountRestricted(t *testing.T) {
	// 11‑-128 通道校验失败 → ErrChannelRefused（不罚账号）
	ch := `{"code":11‑-128,"msg":"request illegal"}`
	if got := Classify(http.StatusForbidden, ch); got != ErrChannelRefused {
		t.Fatalf("11‑-128 判成 %v，期望 ErrChannelRefused", got)
	}
	// 11140 账号限制 → ErrAccountFault（不误判成通道）
	ac := `{"code":11140,"msg":"request illegal"}`
	if got := Classify(http.StatusForbidden, ac); got == ErrChannelRefused {
		t.Fatal("11140 被误判成通道校验失败")
	}
	// 11‑-128 的内容审核变体仍归 ErrContentBlocked（不是通道）
	cb := `{"code":11‑-128,"msg":"blocked by security policy"}`
	if got := Classify(http.StatusBadRequest, cb); got != ErrContentBlocked {
		t.Fatalf("内容审核变体判成 %v，期望 ErrContentBlocked", got)
	}
	// 双条件判定：只有文案没有 code 时不算通道失败
	if IsAccountRestricted(`{"msg":"request illegal"}`) {
		t.Fatal("缺 code 时不应判成账号限制")
	}
	if IsAccountRestricted(`{"code":11140,"msg":"request illegal"}`) != true {
		t.Fatal("11140 + request illegal 应判成账号限制")
	}
}
