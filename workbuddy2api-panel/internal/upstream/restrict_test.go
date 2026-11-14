package upstream

import (
	"net/http"
	"strings"
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

// 泛化措辞「已被限制」不再算账号风控限制：它也出现在模型/功能级限制文案里
// （例如「该模型已被限制使用」），按它判会把模型级限制误当成设备级扫荡、停掉整个域。
func TestIsRestrictedIgnoresGenericWording(t *testing.T) {
	if IsRestricted(`{"code":11140,"msg":"该模型已被限制使用"}`) {
		t.Fatal("泛化「已被限制」不该判成账号风控限制（会误停整个域）")
	}
}

// IsAccountRestriction 两种形态都要认；但 11-128 渠道校验的同一句 "request illegal"
// 属请求级、可自愈，按文案判会停掉轮转、还可能凑成连坐暂停。
func TestIsAccountRestriction(t *testing.T) {
	cases := []struct {
		name, body string
		want       bool
	}{
		{"中文限制文书", `{"code":11140,"msg":"账号存在异常行为，已被限制使用"}`, true},
		{"英文 unusual activity", `{"code":11140,"msg":"unusual activity detected"}`, true},
		{"request illegal + 11140", `{"code":11140,"msg":"request illegal"}`, true},
		{"request illegal + 11-128 渠道校验", `{"code":11128,"msg":"request illegal"}`, false},
		{"泛化已被限制", `{"code":11140,"msg":"该模型已被限制使用"}`, false},
	}
	for _, c := range cases {
		if got := IsAccountRestriction(c.body); got != c.want {
			t.Fatalf("%s: IsAccountRestriction=%v want %v", c.name, got, c.want)
		}
	}
}

// 11134 供应商过载：归 ErrUpstreamBusy，但要与 6020/6021 排队区分提示。
func TestIsProviderOverloadedOnlyByCode(t *testing.T) {
	if !IsProviderOverloaded(`{"code":11134,"msg":"model provider is temporarily unavailable"}`) {
		t.Fatal("code 11134 应判成供应商过载")
	}
	if IsProviderOverloaded(`{"msg":"the model provider is temporarily unavailable"}`) {
		t.Fatal("只有文案、没有结构化 code 时不该判成 11134")
	}
	if hint := GatewayHint(ErrUpstreamBusy, `{"code":11134,"msg":"x"}`, HintContext{}); !strings.Contains(hint, "overloaded") {
		t.Fatalf("11134 的 hint 应说供应商过载而不是队列满: %q", hint)
	}
	if hint := GatewayHint(ErrUpstreamBusy, `{"code":6021,"msg":"queue.full"}`, HintContext{}); !strings.Contains(hint, "queue is full") {
		t.Fatalf("6021 的 hint 应说队列满: %q", hint)
	}
}
