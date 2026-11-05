package server

// data: 载荷提取：规范允许 `data:` 后没有空格。messages / responses 适配器此前只认
// `data: `（带空格），一旦帧源变化（不再经 StreamHint）整条流会被静默丢光，
// 表现为「流式返回空内容且无错误」。

import "testing"

func TestCutDataPayload(t *testing.T) {
	cases := []struct {
		frame string
		want  string
		ok    bool
	}{
		{`data: {"a":1}`, `{"a":1}`, true},   // 带空格（StreamHint 的输出形态）
		{`data:{"a":1}`, `{"a":1}`, true},    // 无空格（规范允许）
		{`data:  {"a":1}`, ` {"a":1}`, true}, // 只去掉**一个**前导空格
		{`data:`, ``, true},                  // 空载荷
		{`event: x`, ``, false},              // 不是 data 帧
		{``, ``, false},
	}
	for _, c := range cases {
		got, ok := cutDataPayload(c.frame)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("cutDataPayload(%q) = (%q,%v), want (%q,%v)", c.frame, got, ok, c.want, c.ok)
		}
	}
}
