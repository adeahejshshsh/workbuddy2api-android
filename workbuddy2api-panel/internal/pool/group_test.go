package pool

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestSetGroupPersists 分组标签落盘、跨重启不丢（issue #159）。
//
// 分组若只存浏览器，换台机器就「组没了」——而组内批量操作是写操作，静默退回
// 「全部账号」等于扩大作用域，所以成员关系必须在服务端。这里钉住往返：
// 落盘 → 重载 → Status.Group 仍是原值，且取消分组（空串）也落盘。
func TestSetGroupPersists(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	if !p.SetGroup("u1", " 批量A ") {
		t.Fatal("SetGroup 应返回 true")
	}
	if st, _ := p.Status("u1"); st.Group != "批量A" {
		t.Fatalf("分组应去掉首尾空白: %q", st.Group)
	}
	p.Flush()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if st, _ := p2.Status("u1"); st.Group != "批量A" {
		t.Fatalf("重载后分组应保持: %q", st.Group)
	}
	if !p2.SetGroup("u1", "") {
		t.Fatal("清空分组应返回 true")
	}
	p2.Flush()

	p3 := New(fp)
	p3.Add(&auth.Auth{UID: "u1"})
	if st, _ := p3.Status("u1"); st.Group != "" {
		t.Fatalf("清空分组后重载应为空: %q", st.Group)
	}
}

// TestSetGroupUnknownUID 不存在的 uid 返回 false（供面板回 404，别静默成功）。
func TestSetGroupUnknownUID(t *testing.T) {
	p := New("")
	if p.SetGroup("nope", "A") {
		t.Error("SetGroup 未知 uid 应返回 false")
	}
}

// TestSetGroupTruncatesLongLabel 超长标签按 rune 截断到 groupMaxRunes。
//
// 面板输入框限了 24 字符，但脚本可以直接打接口；不截断的话每次落盘都会把
// state.json 里这一行放大到用户发多少就多大。
func TestSetGroupTruncatesLongLabel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	long := strings.Repeat("组", groupMaxRunes+5)
	p.SetGroup("u1", long)
	st, _ := p.Status("u1")
	if n := len([]rune(st.Group)); n != groupMaxRunes {
		t.Fatalf("超长分组应按 rune 截到 %d，实际 %d", groupMaxRunes, n)
	}
}
