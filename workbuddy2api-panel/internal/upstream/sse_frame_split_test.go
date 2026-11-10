package upstream

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// 切分器本身：只认首尾相接的 JSON 值序列，其他一律退回单元素。
func TestSplitConcatenatedJSON(t *testing.T) {
	for _, tc := range []struct {
		name  string
		in    string
		want  []string
		parts int
	}{
		{
			name:  "两个对象空格分隔（issue #197 的现场形态）",
			in:    `{"id":"a","choices":[]} {"id":"b","choices":[]}`,
			parts: 2,
			want:  []string{`{"id":"a","choices":[]}`, `{"id":"b","choices":[]}`},
		},
		{
			name:  "两个对象无分隔",
			in:    `{"id":"a"}{"id":"b"}`,
			parts: 2,
			want:  []string{`{"id":"a"}`, `{"id":"b"}`},
		},
		{
			name:  "三个对象含换行分隔",
			in:    "{\"i\":1}\n{\"i\":2}\n{\"i\":3}",
			parts: 3,
			want:  []string{`{"i":1}`, `{"i":2}`, `{"i":3}`},
		},
		{
			name:  "字符串里的花括号与转义引号不能算结构",
			in:    `{"t":"}{ \"x\" }"} {"t":"ok"}`,
			parts: 2,
			want:  []string{`{"t":"}{ \"x\" }"}`, `{"t":"ok"}`},
		},
		{
			name:  "数组与对象混排",
			in:    `[1,2][{"a":1}]`,
			parts: 2,
			want:  []string{`[1,2]`, `[{"a":1}]`},
		},
		{name: "单个对象不拆", in: `{"id":"a"}`, parts: 1},
		{name: "半截对象不拆", in: `{"id":"a"} {"id":`, parts: 1},
		{name: "括号不配平不拆", in: `{"id":"a"}}}}`, parts: 1},
		{name: "纯文本不拆", in: `hello world`, parts: 1},
		{name: "空串", in: ``, parts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := splitConcatenatedJSON(tc.in)
			if len(got) != tc.parts {
				t.Fatalf("拆出 %d 份 want %d：%q", len(got), tc.parts, got)
			}
			if tc.want != nil {
				for i := range tc.want {
					if got[i] != tc.want[i] {
						t.Fatalf("第 %d 份 = %q want %q", i, got[i], tc.want[i])
					}
				}
			}
		})
	}
}

// 端到端：上游把两个 chunk 粘在一行时，客户端必须收到两个**独立的** data 帧。
//
// 变异判据：去掉 writeFrame 里的 splitConcatenatedJSON 分支，本用例会退化成
// 一个 data 帧、且帧内是 `{a} {b}`——正是 issue #197 里客户端 JSON.parse 失败
// 的那个形态。
func TestStreamSplitsGluedFrames(t *testing.T) {
	glued := `data: {"id":"cmb-1","object":"chat.completion.chunk","model":"glm-5.3-flash","choices":[{"index":0,"delta":{"reasoning_content":"User"},"finish_reason":""}]}` +
		` {"id":"cmb-1","object":"chat.completion.chunk","model":"glm-5.3-flash","choices":[{"index":0,"delta":{"content":"你好"},"finish_reason":""}]}` + "\n\n"
	upstreamBody := glued + "data: [DONE]\n\n"

	rec := httptest.NewRecorder()
	if err := StreamHint(rec, strings.NewReader(upstreamBody), nil); err != nil {
		t.Fatalf("StreamHint: %v", err)
	}
	out := rec.Body.String()

	if !strings.Contains(out, "\n\ndata: ") {
		t.Fatalf("帧之间没有空行分隔：%q", out)
	}
	events := splitSSEEvents(out)
	dataEvents := 0
	for _, ev := range events {
		lines := strings.Split(ev, "\n")
		var payload string
		n := 0
		for _, l := range lines {
			if strings.HasPrefix(l, "data:") {
				n++
				payload = strings.TrimSpace(strings.TrimPrefix(l, "data:"))
			}
		}
		if n == 0 {
			continue
		}
		if n > 1 {
			t.Fatalf("一个事件里有 %d 个 data 行（客户端会拼成一份 JSON）：%q", n, ev)
		}
		if payload == "[DONE]" {
			continue
		}
		dataEvents++
		if !json.Valid([]byte(payload)) {
			t.Fatalf("帧不是单个合法 JSON：%q", payload)
		}
		if strings.Contains(payload, "}{") || strings.Count(payload, `"id"`) > 1 {
			t.Fatalf("帧内塞了多个对象：%q", payload)
		}
	}
	// 两个内容帧 + 可能的收尾帧（上游没给 finish_reason，StreamHint 会补一帧截断终态）
	if dataEvents < 2 {
		t.Fatalf("应至少拆出 2 个内容帧，实际 %d：%q", dataEvents, out)
	}
	if !strings.Contains(out, `"reasoning_content":"User"`) || !strings.Contains(out, `"content":"你好"`) {
		t.Fatalf("拆开后内容丢失：%q", out)
	}
	if c := strings.Count(out, "data: [DONE]"); c != 1 {
		t.Fatalf("[DONE] 应为 1 个，实际 %d：%q", c, out)
	}
}

// 切不动时保持原行为：原样透传，且不丢字节。
func TestStreamKeepsUnparsablePayload(t *testing.T) {
	body := "data: {\"broken\":\n\n" + "data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	_ = StreamHint(rec, strings.NewReader(body), nil)
	if !strings.Contains(rec.Body.String(), `data: {"broken":`) {
		t.Fatalf("不可解析的帧必须原样透传：%q", rec.Body.String())
	}
}

// splitSSEEvents 按规范切事件（"\\n\\n"）。
func splitSSEEvents(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var out []string
	for _, ev := range strings.Split(s, "\n\n") {
		if strings.TrimSpace(ev) != "" {
			out = append(out, ev)
		}
	}
	return out
}
