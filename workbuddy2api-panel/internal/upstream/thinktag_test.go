package upstream

import "testing"

func TestThinkTagSplit(t *testing.T) {
	think, body := SplitThinkTags("<think:6124c78e>先看代码</think:6124c78e>答案是 42")
	if think != "先看代码" || body != "答案是 42" {
		t.Fatalf("think=%q body=%q", think, body)
	}
}

// 标签被切在任意位置（流式分片）都要拼回同一结果。
func TestThinkTagSplitAcrossChunks(t *testing.T) {
	full := "<think:ab12>CoT</think:ab12>Answer"
	if think, body := SplitThinkTags(full); think != "CoT" || body != "Answer" {
		t.Fatalf("一次性: think=%q body=%q", think, body)
	}
	for cut := 1; cut < len(full); cut++ {
		var tt ThinkTag
		var think, body string
		for _, s := range []string{full[:cut], full[cut:]} {
			a, b := tt.Feed(s)
			think += a
			body += b
		}
		a, b := tt.Flush()
		think += a
		body += b
		if think != "CoT" || body != "Answer" {
			t.Fatalf("cut=%d think=%q body=%q", cut, think, body)
		}
	}
}

// 不以 <think: 开头就不启用：正文里聊到这个 token 不该被改写。
func TestThinkTagDisabledWhenNotAtStart(t *testing.T) {
	const s = "文档里写 <think:abc> 是控制 token"
	if think, body := SplitThinkTags(s); think != "" || body != s {
		t.Fatalf("think=%q body=%q", think, body)
	}
}

// 没命中时首片就要原样吐出（不能为了判断开头而扣住正文）。
func TestThinkTagFastPathEmitsImmediately(t *testing.T) {
	var tt ThinkTag
	if think, body := tt.Feed("你好"); think != "" || body != "你好" {
		t.Fatalf("首片未即时吐出: think=%q body=%q", think, body)
	}
}
