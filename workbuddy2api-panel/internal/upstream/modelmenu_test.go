package upstream

// 模型菜单口径（上游 2026-10-29 `c5ac20f`）。
//
// 这一条钉住「auto / auto-chat 不是菜单项」：CN 的 agents[cli].models 里带着 auto，
// 不过滤就会让 /v1/models 多一条客户端根本不显示的 auto（实测 17 项菜单变 18 条），
// 客户端选中它上游回 11102。

import "testing"

func TestAutoModeIDNotAMenuItem(t *testing.T) {
	for _, id := range []string{"auto", "auto-chat"} {
		if !isAutoModeID(id) {
			t.Errorf("%q 应判为自动模式开关（不是菜单项）", id)
		}
	}
	// 反例：近似但不同的 id 不能被误判，否则真模型会被静默摘掉。
	for _, id := range []string{"auto-lite", "autopilot", "auto_chat", "Auto", ""} {
		if isAutoModeID(id) {
			t.Errorf("%q 被误判成自动模式", id)
		}
	}
}
