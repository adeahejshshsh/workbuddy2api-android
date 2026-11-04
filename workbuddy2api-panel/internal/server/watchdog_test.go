package server

import (
	"strings"
	"testing"
)

func mkModel(id string, extra ...any) map[string]any {
	m := map[string]any{"id": id, "object": "model"}
	for i := 0; i+1 < len(extra); i += 2 {
		m[extra[i].(string)] = extra[i+1]
	}
	return m
}

// 指纹必须与顺序无关：同一份目录换个顺序不该报警。
func TestCatalogFingerprintOrderIndependent(t *testing.T) {
	a := []map[string]any{mkModel("cn:a"), mkModel("cn:b")}
	b := []map[string]any{mkModel("cn:b"), mkModel("cn:a")}
	fa, _ := catalogFingerprint(a)
	fb, _ := catalogFingerprint(b)
	if fa != fb {
		t.Errorf("顺序影响指纹: %s vs %s", fa, fb)
	}
}

// 任一字段变化都要变（上游改档位表/能力旗标时模型名不变，是最容易漏报的一类）。
func TestCatalogFingerprintDetectsFieldChange(t *testing.T) {
	base := []map[string]any{mkModel("cn:a", "can_disable_thinking", true)}
	changed := []map[string]any{mkModel("cn:a", "can_disable_thinking", false)}
	f1, _ := catalogFingerprint(base)
	f2, _ := catalogFingerprint(changed)
	if f1 == f2 {
		t.Error("字段变化未反映到指纹")
	}
}

func TestCatalogDiff(t *testing.T) {
	cases := []struct {
		prev, cur []string
		want      []string
	}{
		{[]string{"cn:a"}, []string{"cn:a", "cn:b"}, []string{"新增", "cn:b"}},
		{[]string{"cn:a", "cn:b"}, []string{"cn:a"}, []string{"下线", "cn:b"}},
		{[]string{"cn:a"}, []string{"cn:a"}, []string{"字段有改动"}},
		{[]string{}, []string{"cn:a", "cn:b"}, []string{"新增", "cn:a, cn:b"}},
	}
	for _, c := range cases {
		got := catalogDiff(c.prev, c.cur)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%v -> %v: %q 缺少 %q", c.prev, c.cur, got, w)
			}
		}
	}
}

// 大量变化时消息要截断，别把 webhook 刷爆。
func TestCatalogDiffTruncates(t *testing.T) {
	cur := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		cur = append(cur, "cn:m"+string(rune('a'+i)))
	}
	got := catalogDiff(nil, cur)
	if !strings.Contains(got, "等 20 个") {
		t.Errorf("未截断: %q", got)
	}
}
