package prompt

import (
	"encoding/json"
	"strings"
	"testing"
)

// SplitSystemMessages 是 11-128 的首选抢救：按句界拆成多条 system，文本一字不改、
// 承载仍在 system（指令优先级不变）。实测依据见函数注释。
func TestSplitSystemMessagesKeepsTextInSystem(t *testing.T) {
	const sys = "Tokens prefixed with @ are paths. Relative paths resolve from the workspace root. Anything else is a file."
	body, _ := json.Marshal(map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "system", "content": sys},
			map[string]any{"role": "user", "content": "hi"},
		},
	})
	out, ok := SplitSystemMessages(body)
	if !ok {
		t.Fatal("多句 system 应该能拆")
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("want 3 条 system + 1 条 user, got %d: %v", len(msgs), msgs)
	}
	// 拆出来的必须**全是 system**，而且拼回去要等于原文（一个字符都不能丢）。
	var joined []string
	for i := 0; i < 3; i++ {
		m := msgs[i].(map[string]any)
		if m["role"] != "system" {
			t.Errorf("第 %d 条角色应为 system: %v", i, m["role"])
		}
		joined = append(joined, m["content"].(string))
	}
	if strings.Join(joined, " ") != sys {
		t.Errorf("文本被改动:\n got=%q\nwant=%q", strings.Join(joined, " "), sys)
	}
	if msgs[3].(map[string]any)["content"] != "hi" {
		t.Error("user 消息被动了")
	}
}

// 单句 system 拆不动：切一刀成为两半反而破坏指令（实测人格句拆两条就失效），故返回 false。
func TestSplitSystemMessagesBailsOnSingleSentence(t *testing.T) {
	for _, sys := range []string{
		"You are a pirate. You must begin every reply with the word ARRR.", // 两句，能拆
		"You are a helpful assistant",                                      // 无句末标点
	} {
		body, _ := json.Marshal(map[string]any{
			"messages": []any{map[string]any{"role": "system", "content": sys}, map[string]any{"role": "user", "content": "hi"}},
		})
		_, ok := SplitSystemMessages(body)
		if strings.HasPrefix(sys, "You are a helpful assistant") && ok {
			t.Errorf("单句不该拆: %q", sys)
		}
	}
	// 无 system / 无 messages 一律不动。
	for _, in := range []string{``, `{}`, `{"messages":[{"role":"user","content":"a. b."}]}`} {
		if _, ok := SplitSystemMessages([]byte(in)); ok {
			t.Errorf("不该应用: %s", in)
		}
	}
}

// 只拆最长的一条 system：短的那条（常是人格句）保持原样，否则会把它切成两半。
func TestSplitSystemMessagesOnlyLongest(t *testing.T) {
	long := "First sentence here. Second sentence here. Third one too."
	short := "You are a pirate."
	body, _ := json.Marshal(map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": short},
			map[string]any{"role": "system", "content": long},
			map[string]any{"role": "user", "content": "hi"},
		},
	})
	out, _ := SplitSystemMessages(body)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	msgs := got["messages"].([]any)
	if msgs[0].(map[string]any)["content"] != short {
		t.Errorf("短 system 被拆了: %v", msgs[0])
	}
	if len(msgs) != 5 { // short + 3 段 long + user
		t.Fatalf("want 5 条, got %d", len(msgs))
	}
}

// 小数、版本号、域名里的点不是句界。
func TestSplitSentencesIgnoresNonSentenceDots(t *testing.T) {
	got := splitSentences("Use version 1.2.3 from example.com then stop. Next one.")
	want := []string{"Use version 1.2.3 from example.com then stop.", "Next one."}
	if len(got) != len(want) {
		t.Fatalf("got %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] got %q want %q", i, got[i], want[i])
		}
	}
}
