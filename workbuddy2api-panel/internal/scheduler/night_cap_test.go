package scheduler

// 夜间参与上限（上游 2026-10-26 `a24c10a`）。
//
// 夜猫子是整个任务体系里唯一「整任务都是真实模型对话」的（逐条 ChatStream 发短
// 对话），指纹最重；深夜全池同 IP 齐做真实对话最像机器人。

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

func TestCapByCredits(t *testing.T) {
	mk := func(uid string, c int64) pool.Status { return pool.Status{UID: uid, Credits: c} }
	// cap<=0 或候选不超上限：原样返回（同一底层数组，调用方可能依赖）。
	list := []pool.Status{mk("a", 3), mk("b", 9)}
	if got := capByCredits(list, 0); len(got) != 2 || &got[0] != &list[0] {
		t.Fatal("cap 0 必须 no-op 且返回同一 slice")
	}
	if got := capByCredits(list, 5); len(got) != 2 {
		t.Fatal("cap >= len 必须 no-op")
	}

	// 收窄为积分 Top-N。
	list = []pool.Status{mk("a", 100), mk("b", 500), mk("c", 300), mk("d", 400), mk("e", 200)}
	got := capByCredits(list, 2)
	if len(got) != 2 {
		t.Fatalf("want 2, got %d", len(got))
	}
	// Top-2 必须是 b(500) 与 d(400)，顺序不限但集合确定。
	set := map[string]bool{}
	for _, st := range got {
		set[st.UID] = true
	}
	if !set["b"] || !set["d"] {
		t.Fatalf("want {b,d}, got %v", set)
	}
	// 原切片不被打乱（Pool.List 的返回不该被改序）。
	if list[0].UID != "a" {
		t.Fatal("输入切片不能被打乱")
	}
}

// 积分未知（全 0）时仍按上限收窄——降级可行，不该因为余额没刷到就全池参加。
func TestCapByCreditsUnknownCredits(t *testing.T) {
	list := []pool.Status{{UID: "a"}, {UID: "b"}, {UID: "c"}}
	if got := capByCredits(list, 2); len(got) != 2 {
		t.Fatalf("want 2, got %d", len(got))
	}
}

// SetNightAccountCap / NightAccountCap 往返，负数归 0（不限）。
func TestNightAccountCapSetter(t *testing.T) {
	s := New(Config{})
	if got := s.NightAccountCap(); got != 0 {
		t.Fatalf("缺省应为 0（不限），got %d", got)
	}
	s.SetNightAccountCap(3)
	if got := s.NightAccountCap(); got != 3 {
		t.Fatalf("got %d want 3", got)
	}
	s.SetNightAccountCap(-1)
	if got := s.NightAccountCap(); got != 0 {
		t.Fatalf("负数应归 0，got %d", got)
	}
}

// 先剔不参与的号、再按积分取前 N。反过来会让「被禁用的高积分老号」占满名额，
// 夜猫子整晚一个对话都不发且没有任何日志。
func TestNightCandidatesFiltersBeforeCapping(t *testing.T) {
	list := []pool.Status{
		{UID: "dead1", Credits: 9000, Disabled: true},
		{UID: "dead2", Credits: 8000, Disabled: true},
		{UID: "paused", Credits: 7000, Paused: true},
		{UID: "ok1", Credits: 300},
		{UID: "ok2", Credits: 100},
	}
	got := nightCandidates(list, false, 2)
	if len(got) != 2 {
		t.Fatalf("want 2, got %d (%v)", len(got), got)
	}
	set := map[string]bool{}
	for _, st := range got {
		set[st.UID] = true
	}
	if !set["ok1"] || !set["ok2"] {
		t.Fatalf("应选健康的 ok1/ok2，实际 %v —— 被禁用的高积分号占满了名额", set)
	}

	// includeDisabledInTasks=true 时禁用号可以参加（与既有门控同口径），暂停号仍剔除。
	got = nightCandidates(list, true, 3)
	set = map[string]bool{}
	for _, st := range got {
		set[st.UID] = true
	}
	if !set["dead1"] || !set["dead2"] || !set["ok1"] || set["paused"] {
		t.Fatalf("includeDisabled 语义不符: %v", set)
	}

	// 全部不参与 → 空候选（不该 panic，也不该退回全池）。
	if got := nightCandidates([]pool.Status{{UID: "x", Disabled: true}}, false, 2); len(got) != 0 {
		t.Fatalf("全禁用应得空候选，got %v", got)
	}
}
