package upstream

import "strings"

// 上游思考控制 token 的两种形态（hy4 的 vLLM reasoning parser 用 <think:SUF> /
// </think:SUF>，SUF 是模型的 checkpoint token 后缀，不是请求 id）。
const (
	thinkOpen  = "<think:"
	thinkClose = "</think:"
)

// ThinkTag 把漏进正文的思考标签摘出来（issue #194）。
//
// hy4 的 reasoning parser 偶发把整段 CoT 当 content 下发：实测 31 条 assistant
// 消息里 4 条中招，usage 的 reasoning_tokens 是 0（日志 think=0），正文首尾带
// <think:6124c78e> / </think:6124c78e>。chat 协议客户端看到的是正文里混着 CoT；
// Anthropic 客户端更糟——它只认 content_block.type=thinking，内联标签一律当文本。
// 上游 parser 修不了，网关至少不该把控制 token 当正文发出去。
//
// 只在 content 以 <think: 开头时启用：正文中间聊到这几个字不重写。
//
// shortcut: 只认带后缀的 <think:SUF> 形态，不带冒号的裸 <think> 不处理；
// 另一个上游换了 parser 形态再放开。
type ThinkTag struct {
	decided bool   // 已判定这段 content 是否内联思考
	on      bool   // 启用摘除
	inTag   bool   // 当前位置在标签内
	pend    string // 尾部可能是半个标签，等下一片
}

// Feed 吃一片正文，返回本片里的思考与正文（按原顺序拼进各自通道）。
func (t *ThinkTag) Feed(s string) (think, body string) {
	if s == "" {
		return "", ""
	}
	t.pend += s
	if !t.decided {
		// 开头还没看够：先等一片，别把 "<thi" 当正文发出去。
		if len(t.pend) < len(thinkOpen) && strings.HasPrefix(thinkOpen, t.pend) {
			return "", ""
		}
		t.decided = true
		if t.on = strings.HasPrefix(t.pend, thinkOpen); !t.on {
			out := t.pend
			t.pend = ""
			return "", out
		}
	}
	if !t.on {
		out := t.pend
		t.pend = ""
		return "", out
	}
	return t.scan(false)
}

// Flush 吐出尾部残留。流结束（或非流式吃完一次）时必须调用，否则最后几个
// 字符会丢。
func (t *ThinkTag) Flush() (think, body string) {
	if t.pend == "" {
		return "", ""
	}
	if !t.decided {
		t.decided = true
		if t.on = strings.HasPrefix(t.pend, thinkOpen); !t.on {
			out := t.pend
			t.pend = ""
			return "", out
		}
	}
	if !t.on {
		out := t.pend
		t.pend = ""
		return "", out
	}
	return t.scan(true)
}

// scan 消费 t.pend。flush=false 时把可能是半个标签的尾巴留在 pend 里等下一片。
func (t *ThinkTag) scan(flush bool) (think, body string) {
	var tb, bb strings.Builder
	emit := func(s string) {
		if s == "" {
			return
		}
		if t.inTag {
			tb.WriteString(s)
		} else {
			bb.WriteString(s)
		}
	}
	for {
		p := t.pend
		idx, closing := -1, false
		for _, m := range []struct {
			tag     string
			closing bool
		}{{thinkOpen, false}, {thinkClose, true}} {
			if k := strings.Index(p, m.tag); k >= 0 && (idx < 0 || k < idx) {
				idx, closing = k, m.closing
			}
		}
		if idx < 0 {
			keep := 0
			if !flush {
				// 尾巴可能是下一个标签的前半截（最长 len(</think:)-1）。
				for n := min(len(p), len(thinkClose)-1); n > 0; n-- {
					tail := p[len(p)-n:]
					if strings.HasPrefix(thinkClose, tail) || strings.HasPrefix(thinkOpen, tail) {
						keep = n
						break
					}
				}
			}
			emit(p[:len(p)-keep])
			t.pend = p[len(p)-keep:]
			return tb.String(), bb.String()
		}
		emit(p[:idx])
		rest := p[idx:]
		gt := strings.IndexByte(rest, '>')
		if gt < 0 {
			// 标签还没收全：留到下一片。flush 时上游已经结束了，按正文发出去，
			// 少一次改写好过丢内容。
			if flush {
				emit(rest)
				t.pend = ""
			} else {
				t.pend = rest
			}
			return tb.String(), bb.String()
		}
		t.pend = rest[gt+1:]
		t.inTag = !closing
	}
}

// SplitThinkTags 非流式的一次性版本（完整 content 进来，两段出去）。
func SplitThinkTags(s string) (think, body string) {
	var t ThinkTag
	a, b := t.Feed(s)
	c, d := t.Flush()
	return a + c, b + d
}
