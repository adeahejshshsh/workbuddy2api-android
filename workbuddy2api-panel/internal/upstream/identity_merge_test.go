package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMergeAdjacentIdentityMessages：相邻身份消息合并、边界不动。
func TestMergeAdjacentIdentityMessages(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "system", "content": "a"},
		map[string]any{"role": "developer", "content": "b"},
		map[string]any{"role": "user", "content": "u"},
		map[string]any{"role": "system", "content": "c"},
		map[string]any{"role": "assistant", "content": "x"},
		map[string]any{"role": "system", "content": "d"},
		map[string]any{"role": "system", "content": "e"},
	}
	out, changed := mergeAdjacentIdentityMessages(msgs)
	if !changed {
		t.Fatal("应当发生合并")
	}
	if len(out) != 5 {
		t.Fatalf("消息数 = %d want 5（a+b / u / c / x / d+e）", len(out))
	}
	first := out[0].(map[string]any)["content"].(string)
	if first != "a\n\nb" {
		t.Fatalf("前两条未合并为一条：%q", first)
	}
	if out[1].(map[string]any)["role"].(string) != "user" {
		t.Fatal("user 消息被挪动")
	}
	last := out[4].(map[string]any)["content"].(string)
	if last != "d\n\ne" {
		t.Fatalf("末尾两条未合并：%q", last)
	}
	// 顺序必须完全保留
	got := []string{}
	for _, m := range out {
		got = append(got, m.(map[string]any)["content"].(string))
	}
	want := []string{"a\n\nb", "u", "c", "x", "d\n\ne"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("顺序/内容 = %v want %v", got, want)
	}
}

// TestMergeAdjacentIdentityMessagesSkipsStructured：带额外键或数组 content 的不合并。
func TestMergeAdjacentIdentityMessagesSkipsStructured(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "system", "content": "a", "name": "n"},
		map[string]any{"role": "system", "content": "b"},
		map[string]any{"role": "system", "content": []any{
			map[string]any{"type": "text", "text": "c"},
		}},
		map[string]any{"role": "system", "content": "d"},
	}
	out, changed := mergeAdjacentIdentityMessages(msgs)
	if changed || len(out) != 4 {
		t.Fatalf("结构化消息被合并了：changed=%v len=%d", changed, len(out))
	}
}

// TestPrepareBodyMergesSplitDshPaths：DSH 0.2.2 的「一句一条 system」形态，
// 合并后段级脱敏规则必须命中——出站不得再带原始指纹。
//
// 这是 2026-11-20 线上 400 11128 的最小复现：四段各自都不含完整指纹，
// 拆开时正则一路不命中（mutate：注释掉 mergeAdjacentIdentityMessages 调用 → 本用例红）。
func TestPrepareBodyMergesSplitDshPaths(t *testing.T) {
	body := map[string]any{
		"model": "deepseek-v4.1-flash",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are an AI agent powered by DeepSeek Harness."},
			map[string]any{"role": "system", "content": "Tokens prefixed with @ are paths the user explicitly referenced."},
			map[string]any{"role": "system", "content": "Relative paths resolve against the workspace root; absolute paths identify files or directories on the host."},
			map[string]any{"role": "system", "content": "A trailing slash marks a directory: list it when its contents matter."},
			map[string]any{"role": "system", "content": "Anything else is a file: use the read tool when its contents are needed, and do not claim to have inspected it before reading. @\"...\" quotes a path containing spaces."},
			map[string]any{"role": "user", "content": "继续"},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	out := PrepareBodyOpt(raw, true)

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("出站不是合法 JSON：%v", err)
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("消息数 = %d want 2（5 条 system 合并成 1 条 + user）", len(msgs))
	}
	sys := msgs[0].(map[string]any)["content"].(string)
	for _, bad := range []string{
		"Tokens prefixed with @",
		"quotes a path containing spaces",
		"Relative paths resolve against the workspace root; absolute paths identify",
	} {
		if strings.Contains(sys, bad) {
			t.Fatalf("出站 system 仍含指纹 %q：\n%s", bad, sys)
		}
	}
	if !strings.Contains(sys, "Paths beginning with @") {
		t.Fatalf("段级改写未生效（没有替换文本）：\n%s", sys)
	}
	// 品牌词仍按原规则处理（零宽拆分，肉眼一致、逐字不同）
	if strings.Contains(sys, "DeepSeek Harness") {
		t.Fatalf("品牌词原样出站：\n%s", sys)
	}
	if msgs[1].(map[string]any)["role"].(string) != "user" {
		t.Fatal("user 消息结构被改变")
	}
}

// TestMergeKeepsSingleSystemUntouched：只有一条 system 时零改动（不能给普通请求
// 平白加一次 marshal 差异，prefix cache 会因此全量失效）。
func TestMergeKeepsSingleSystemUntouched(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "system", "content": "a"},
		map[string]any{"role": "user", "content": "u"},
	}
	out, changed := mergeAdjacentIdentityMessages(msgs)
	if changed {
		t.Fatal("单条 system 不该发生合并")
	}
	if &out[0] != &msgs[0] {
		t.Skip("slice 复用与否不作强约束")
	}
}
