package server

// W2-09 回归：轮转前的「能否强制 tool_choice」预判不许重序列化整份 body。
//
// forceToolChoiceRequired 解码整份 body 后还会 marshal 一遍输出；旧调用点
// `_, canForceTools := forceToolChoiceRequired(body)` 把结果丢掉，十万 token
// 的请求每轮白付一次全量 marshal。新增 canForceToolChoiceRequired 只判定。

import (
	"strings"
	"testing"
)

func TestCanForceToolChoiceDoesNotMarshalBody(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}]}`)

	before := forceToolChoiceMarshal.Load()
	if !canForceToolChoiceRequired(body) {
		t.Fatal("该 body 应可改为 tool_choice=required")
	}
	if got := forceToolChoiceMarshal.Load() - before; got != 0 {
		t.Fatalf("预判路径发生了 %d 次整份 body 重序列化（W2-09 的放大点又回来了）", got)
	}

	out, ok := forceToolChoiceRequired(body)
	if !ok {
		t.Fatal("真正改写应成功")
	}
	if got := forceToolChoiceMarshal.Load() - before; got != 1 {
		t.Fatalf("真正改写应恰好 marshal 1 次，实际 %d", got)
	}
	if !strings.Contains(string(out), `"tool_choice":"required"`) {
		t.Fatalf("改写结果缺 tool_choice=required：%s", out)
	}

	// 对照组：钉死 tool_choice / 没有 tools 的 body 两边都应为 false。
	for _, b := range []string{
		`{"model":"m","tools":[],"tool_choice":"none"}`,
		`{"model":"m","tool_choice":"auto"}`,
	} {
		if canForceToolChoiceRequired([]byte(b)) {
			t.Fatalf("不该判为可改：%s", b)
		}
		if _, ok := forceToolChoiceRequired([]byte(b)); ok {
			t.Fatalf("不应改写：%s", b)
		}
	}
}
