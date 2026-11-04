package server

// 告警观测源适配器（server/alert_source.go）：把池健康与 WAF 状态按 alert.Source
// 的形状交出去。这里钉两件事——域过滤真的按域算，以及 Disabled 与 /metrics 同口径
// 合并 d+p（否则告警看板会把「暂停选号」的号算成健康）。

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestAlertSourceHealthByRealm(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
		&auth.Auth{UID: "c2", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
		&auth.Auth{UID: "g1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"},
	)
	h := NewHandler(Config{Pool: p})
	src := h.AlertSource()
	if src == nil {
		t.Fatal("接了池就应有观测源")
	}

	cn := src.Health("cn")
	if cn.Total != 2 || cn.Healthy != 2 {
		t.Fatalf("cn=%+v want total/healthy 2/2", cn)
	}
	global := src.Health("global")
	if global.Total != 1 || global.Healthy != 1 {
		t.Fatalf("global=%+v want total/healthy 1/1", global)
	}

	// 暂停一个 CN 号：它不再算健康，且要计入 Disabled（d+p 合并口径）。
	if !p.Pause("c1") {
		t.Fatal("暂停失败")
	}
	cn = src.Health("cn")
	if cn.Healthy != 1 {
		t.Fatalf("暂停后 cn healthy=%d want 1", cn.Healthy)
	}
	if cn.Disabled < 1 {
		t.Fatalf("暂停的号应计入 Disabled（d+p 合并），cn=%+v", cn)
	}
}

// WAF 未触发时是 false；触发后适配器要如实透出（告警规则靠它判 waf_ip_block）。
func TestAlertSourceWAFActive(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith()})
	src := h.AlertSource()
	if src.WAFActive() {
		t.Fatal("初始不应激活")
	}
	// 触发 IP 级拦截：窗内两个**不同** UID 接连命中 WAF 403 即激活（wafIPThreshold）。
	h.wafIP.noteWaf("u1")
	if src.WAFActive() {
		t.Fatal("单个号命中还不该激活")
	}
	h.wafIP.noteWaf("u2")
	if !src.WAFActive() {
		t.Fatal("激活后应透出 true")
	}
}

// 没接池时返回 nil —— 监视器拿到 nil 会静默跳过，而不是 panic。
func TestAlertSourceNilPool(t *testing.T) {
	h := NewHandler(Config{})
	if got := h.AlertSource(); got != nil {
		t.Fatalf("无池应返回 nil，实际 %T", got)
	}
}
