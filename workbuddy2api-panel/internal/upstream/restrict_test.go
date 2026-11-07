package upstream

import (
	"net/http"
	"testing"
)

// 11140 的两类形态必须分开：限制文书（设备/出口级，命中即停手）与模型级限流
// （账号健康，走软限流）。判据是 code + 文案双条件，缺一个都会错。
func TestIsRestricted(t *testing.T) {
	// 上游真实中文文书（hub #422 的 403 样例）
	cn := `{"code":11140,"msg":"账号存在异常行为，已被限制使用"}`
	if !IsRestricted(cn) {
		t.Fatal("11140 + 中文限制文书应判成风控限制")
	}
	// 英文形态
	en := `{"code":11140,"msg":"Your account shows unusual activity and has been restricted"}`
	if !IsRestricted(en) {
		t.Fatal("11140 + unusual activity 应判成风控限制")
	}
	// 同一 code 承载模型级限流：账号是健康的，不能当账号终态（会白扔一天）
	rateLimited := `{"code":11140,"msg":"账号存在异常行为","displayMsg":"The model provider is rate-limiting requests."}`
	if IsRestricted(rateLimited) {
		t.Fatal("带限流文案的 11140 不应判成风控限制")
	}
	// 只有文案没有 code：可能是正文/检索回来的网页，不能误伤
	if IsRestricted(`{"msg":"账号存在异常行为，已被限制使用"}`) {
		t.Fatal("缺 code 时不应判成风控限制")
	}
	// 授权形态（request illegal）归 IsAccountRestricted，不归本函数
	if IsRestricted(`{"code":11140,"msg":"request illegal"}`) {
		t.Fatal("request illegal 形态应走 IsAccountRestricted，不归 IsRestricted")
	}
}

// 中文文书必须能被 Classify 认成 ErrAccountFault —— 否则它落进通用 4xx 兜底
// （ErrClient，只换号不罚号），applyErrorPolicy 里的 11140 分支根本走不到。
func TestClassifyChineseRestriction(t *testing.T) {
	cn := `{"code":11140,"msg":"账号存在异常行为，已被限制使用"}`
	if got := Classify(http.StatusForbidden, cn); got != ErrAccountFault {
		t.Fatalf("中文 11140 判成 %v，期望 ErrAccountFault", got)
	}
	// 中文文书不触达通道校验分支（那是 11‑-128 的事）
	if got := Classify(http.StatusForbidden, cn); got == ErrChannelRefused {
		t.Fatal("中文 11140 被误判成通道校验失败")
	}
}
