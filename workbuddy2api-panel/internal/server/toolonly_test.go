package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// segToolOnlyStop 首段：模型刚看完工具结果，只写一句就以句号收尾、零工具调用。
// 线上原样形态（17:29:45 acct=150****5392，prompt 106,507，27 个工具，35 token）。
const segToolOnlyStop = `data: {"id":"t1","choices":[{"index":0,"delta":{"content":"这是高价值面，需要走通验证。先看完整表单。"}}]}` + "\n\n" +
	`data: {"id":"t1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":106507,"completion_tokens":35,"total_tokens":106542}}` + "\n\n" +
	"data: [DONE]\n\n"

// segToolOnlyToolCall 续写段：直接给工具调用（期望被采纳）。
const segToolOnlyToolCall = `data: {"id":"t2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_9","function":{"name":"read_file","arguments":"{\"path\":\"/etc/hostname\"}"}}]}}]}` + "\n\n" +
	`data: {"id":"t2","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":106600,"completion_tokens":20,"total_tokens":106620}}` + "\n\n" +
	"data: [DONE]\n\n"

// segToolOnlyText 续写段：只有正文（期望整段被丢弃）。
const segToolOnlyText = `data: {"id":"t3","choices":[{"index":0,"delta":{"content":"另外补充一段无关紧要的说明，客户端不该看到它。"}}]}` + "\n\n" +
	`data: {"id":"t3","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":106600,"completion_tokens":18,"total_tokens":106618}}` + "\n\n" +
	"data: [DONE]\n\n"

// toolLoopBody 末条是工具结果的 agent 请求（长 prompt 由首段 usage 提供）。
const toolLoopBody = `{"model":"cn:deepseek-v4.1-flash","max_tokens":8192,` +
	`"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}],` +
	`"messages":[{"role":"user","content":"查 user-store 的批量赋值"},` +
	`{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{}"}}]},` +
	`{"role":"tool","tool_call_id":"c1","content":"ok"}]}`

func newToolOnlyReader(t *testing.T, up *upstream.Client, body, seg string) *continueReader {
	t.Helper()
	r := newContinueReaderWithFloor(context.Background(), up,
		&auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999},
		[]byte(body), "1.2.3.4", upstream.ChatMeta{}, io.NopCloser(strings.NewReader(seg)), 8192)
	// 这些用例验证的是「开关打开时」的行为；默认关闭由 TestToolOnlyOffByDefault 覆盖。
	r.toolOnlyRescue = true
	return r
}

// TestToolOnlyAdoptsToolCall 续写段给工具调用 → 采纳，客户端收到工具分片。
func TestToolOnlyAdoptsToolCall(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, segToolOnlyToolCall })
	r := newToolOnlyReader(t, up, toolLoopBody, segToolOnlyStop)
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("续写请求数=%d want 1", len(*reqs))
	}
	got := reqBody(t, (*reqs)[0])
	msgs, _ := got["messages"].([]any)
	last, _ := msgs[len(msgs)-1].(map[string]any)
	if s, _ := last["content"].(string); !strings.Contains(s, "直接调用工具") {
		t.Fatalf("续写指令应要求直接调用工具，实际 %q", s)
	}
	s := string(out)
	if !strings.Contains(s, `"tool_calls"`) || !strings.Contains(s, "read_file") {
		t.Fatalf("客户端应收到工具调用分片:\n%s", s)
	}
	if !strings.Contains(s, `"finish_reason":"tool_calls"`) {
		t.Fatalf("终态应为 tool_calls:\n%s", s)
	}
}

// TestToolOnlyDiscardsText 续写段只有正文 → 整段丢弃，客户端看到的内容与接线前一致。
func TestToolOnlyDiscardsText(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, segToolOnlyText })
	r := newToolOnlyReader(t, up, toolLoopBody, segToolOnlyStop)
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("应尝试续写一次，实际 %d 次", len(*reqs))
	}
	s := string(out)
	if strings.Contains(s, "无关紧要的说明") {
		t.Fatalf("续写段的正文必须被丢弃，却下发了:\n%s", s)
	}
	if !strings.Contains(s, "先看完整表单。") {
		t.Fatalf("首段正文应照常保留:\n%s", s)
	}
	if !strings.Contains(s, `"finish_reason":"stop"`) {
		t.Fatalf("应回滚到首段的 stop 终态:\n%s", s)
	}
	// 回滚必须回放**首段**的终态帧（id=t1），而不是续写段那一份（id=t3）：
	// 两段都是 stop，只有帧里的 id 能区分，这里就是钉这个的。
	if !strings.Contains(s, `"id":"t1"`) {
		t.Fatalf("终态帧应来自首段:\n%s", s)
	}
	if strings.Contains(s, `"id":"t3"`) {
		t.Fatalf("续写段的帧不该下发:\n%s", s)
	}
	// 终态只能出现一次（首段的），不能既有 stop 又有 tool_calls。
	if strings.Contains(s, "tool_calls") {
		t.Fatalf("没有工具调用时不该出现 tool_calls:\n%s", s)
	}
}

// TestToolOnlyNeedsToolResultLast 末条不是工具结果（闲聊/角色扮演、或模型自己已经
// 回过一轮）不续写——历史里有没有工具轮是另一件事，这一条只钉「末条」。
func TestToolOnlyNeedsToolResultLast(t *testing.T) {
	body := `{"model":"cn:deepseek-v4.1-flash","max_tokens":8192,` +
		`"tools":[{"type":"function","function":{"name":"read_file"}}],` +
		`"messages":[{"role":"user","content":"查 user-store"},` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"c1","content":"ok"},` +
		`{"role":"assistant","content":"这是高价值面，需要走通验证。先看完整表单。"}]}`
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, segToolOnlyToolCall })
	r := newToolOnlyReader(t, up, body, segToolOnlyStop)
	_, _ = io.ReadAll(r)
	if len(*reqs) != 0 {
		t.Fatalf("末条不是工具结果时不该续写，实际 %d 次", len(*reqs))
	}
}

// TestToolOnlyNeedsLongPrompt 短会话（prompt 小）里的一句收尾是正常的，不续写。
func TestToolOnlyNeedsLongPrompt(t *testing.T) {
	short := `data: {"id":"t1","choices":[{"index":0,"delta":{"content":"2+2等于4。"}}]}` + "\n\n" +
		`data: {"id":"t1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":120,"completion_tokens":7,"total_tokens":127}}` + "\n\n" +
		"data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, segToolOnlyToolCall })
	r := newToolOnlyReader(t, up, toolLoopBody, short)
	_, _ = io.ReadAll(r)
	if len(*reqs) != 0 {
		t.Fatalf("短会话不该续写，实际 %d 次", len(*reqs))
	}
}

// TestToolOnlyLeavesColonToNarration 尾部落在预告符上时不抢 narration 的活。
func TestToolOnlyLeavesColonToNarration(t *testing.T) {
	colon := `data: {"id":"t1","choices":[{"index":0,"delta":{"content":"先看完整表单："}}]}` + "\n\n" +
		`data: {"id":"t1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":106507,"completion_tokens":12,"total_tokens":106519}}` + "\n\n" +
		"data: [DONE]\n\n"
	up, _ := fakeContinueUpstream(t, func(call int) (int, string) { return 200, segNarrateContinue })
	r := newToolOnlyReader(t, up, toolLoopBody, colon)
	_, _ = io.ReadAll(r)
	if r.toolOnlyTried {
		t.Fatalf("冒号结尾应交给 narration，不该走安全续写")
	}
}

// TestToolOnlyLongTextNotRescued 成篇正文（远超一次提前收尾的长度）不救。
func TestToolOnlyLongTextNotRescued(t *testing.T) {
	long := strings.Repeat("这是一段完整的分析结论。", 400) // ~4800 字节，> toolOnlyMaxBytes
	seg := `data: {"id":"t1","choices":[{"index":0,"delta":{"content":"` + long + `"}}]}` + "\n\n" +
		`data: {"id":"t1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":106507,"completion_tokens":400,"total_tokens":106907}}` + "\n\n" +
		"data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, segToolOnlyToolCall })
	r := newToolOnlyReader(t, up, toolLoopBody, seg)
	_, _ = io.ReadAll(r)
	if len(*reqs) != 0 {
		t.Fatalf("长正文不该续写，实际 %d 次", len(*reqs))
	}
}

// TestToolOnlyE2EResponses 端到端：/v1/responses 上救回的工具调用必须变成
// function_call item；被丢弃的正文不能出现在客户端流里。
func TestToolOnlyE2EResponses(t *testing.T) {
	for _, tc := range []struct {
		name        string
		second      string
		wantTool    bool
		wantDropped string
	}{
		{"救回工具调用", segToolOnlyToolCall, true, ""},
		{"正文被丢弃", segToolOnlyText, false, "无关紧要的说明"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, segToolOnlyToolCall })
			up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				raw, _ := io.ReadAll(r.Body)
				*reqs = append(*reqs, string(raw))
				body := tc.second
				if len(*reqs) == 1 {
					body = segToolOnlyStop
				}
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader(body)),
				}, nil
			})
			p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
			h := NewHandler(Config{Pool: p, Upstream: up, ToolOnlyRescue: true})

			var buf bytes.Buffer
			old := log.Writer()
			log.SetOutput(&buf)
			defer log.SetOutput(old)

			body := `{"model":"deepseek-v4.1-flash","stream":true,"input":[` +
				`{"role":"user","content":"查 user-store"},` +
				`{"type":"function_call","call_id":"c1","name":"read_file","arguments":"{}"},` +
				`{"type":"function_call_output","call_id":"c1","output":"ok"}],` +
				`"tools":[{"type":"function","name":"read_file","parameters":{"type":"object"}}]}`
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))

			s := rec.Body.String()
			if tc.wantTool && !strings.Contains(s, "function_call") {
				t.Fatalf("应下发 function_call:\n%s", s)
			}
			if !tc.wantTool && strings.Contains(s, "function_call") {
				t.Fatalf("不该下发 function_call:\n%s", s)
			}
			if tc.wantDropped != "" && strings.Contains(s, tc.wantDropped) {
				t.Fatalf("被丢弃的正文不该下发:\n%s", s)
			}
			if !strings.Contains(s, "先看完整表单。") {
				t.Fatalf("首段正文应保留:\n%s", s)
			}
			if !strings.Contains(s, "response.completed") {
				t.Fatalf("应以 response.completed 收尾:\n%s", s)
			}
		})
	}
}

var _ = json.Marshal

// TestToolOnlyOffByDefault 默认关闭：正常收尾不再被判成截断、不白打一次上游。
//
// 线上症状：agent 循环里模型看完工具结果给了一段完整回答（实测 1608B），
// finish=stop、零工具调用——旧实现把它当「说一句就停了」，补发一次只采纳工具调用
// 的续写请求，等于每次正常收尾都多打一次上游。
func TestToolOnlyOffByDefault(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, segToolOnlyToolCall })
	r := newContinueReaderWithFloor(context.Background(), up,
		&auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999},
		[]byte(toolLoopBody), "1.2.3.4", upstream.ChatMeta{},
		io.NopCloser(strings.NewReader(segToolOnlyStop)), 8192)
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 0 {
		t.Fatalf("默认关闭时不该有续写请求，实际 %d 次", len(*reqs))
	}
	if strings.Contains(string(out), "tool_calls") {
		t.Fatalf("不该出现续写段的工具调用:\n%s", out)
	}
	if !strings.Contains(string(out), "先看完整表单") {
		t.Fatalf("首段正文应原样透传:\n%s", out)
	}
}

// TestToolOnlyCoversWidenedWindow 曾经放宽到 7500B 的窗口现在**必须不命中**。
//
// 2026-11-10 21:30 那批反馈（「任务该结束了却一直在工具调用」）证明：正文写到
// 1500~5000B 的「零工具调用收尾」里，有相当比例是模型真的答完了，被续写指令推着
// 又调了一次工具。在能区分「提前收尾」与「答完」之前，这一档不能救。
func TestToolOnlyCoversWidenedWindow(t *testing.T) {
	mid := strings.Repeat("这里是一段排查过程的叙述。", 150) // ~5850 字节
	seg := `data: {"id":"t1","choices":[{"index":0,"delta":{"content":"` + mid + `"}}]}` + "\n\n" +
		`data: {"id":"t1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":106507,"completion_tokens":900,"total_tokens":107407}}` + "\n\n" +
		"data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, segToolOnlyToolCall })
	r := newToolOnlyReader(t, up, toolLoopBody, seg)
	_, _ = io.ReadAll(r)
	if len(*reqs) != 0 {
		t.Fatalf("5.8KB 正文不该续写（会推着已答完的模型再调一次工具），实际续写 %d 次", len(*reqs))
	}
}

// TestToolOnlySkipsToolCallMarkup 正文是「写成文本的工具调用」（DSML/XML）时不能续写。
//
// 客户端会自己解析并执行这段标记；再补一句「继续执行」会让模型发出第二次调用，
// 用户看到的就是「任务结束不了」。线上 DSH 会话实测：40 分钟里 25 次这类误救。
func TestToolOnlySkipsToolCallMarkup(t *testing.T) {
	seg := `data: {"id":"t1","choices":[{"index":0,"delta":{"content":"正在执行 </｜｜DSML｜｜ invoke> </｜｜DSML｜｜ calls>"}}]}` + "\n\n" +
		`data: {"id":"t1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":106507,"completion_tokens":30,"total_tokens":106537}}` + "\n\n" +
		"data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, segToolOnlyToolCall })
	r := newToolOnlyReader(t, up, toolLoopBody, seg)
	_, _ = io.ReadAll(r)
	if len(*reqs) != 0 {
		t.Fatalf("正文含工具调用标记时不该续写，实际 %d 次", len(*reqs))
	}
}

// segToolOnlyLongFinal 首段：正文已经是一段完整总结（>120B、句号收尾）。
// 这是「任务做完了」的正常形态，不该被救援推着再调一次工具。
const segToolOnlyLongFinal = `data: {"id":"t9","choices":[{"index":0,"delta":{"content":"我把三处问题都改完了：热重载的删除确认已经在轮询里真正走完，流式首帧的空窗补了心跳帧，代理订阅的流式数组也认了。` + `全部改动已经提交并推送，CI 两个工作流都通过，服务器没有动。"}}]}` + "\n\n" +
	`data: {"id":"t9","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":106507,"completion_tokens":120,"total_tokens":106627}}` + "\n\n" +
	"data: [DONE]\n\n"

// 续写段把「刚执行过的那次 read_file {}」原样再发一遍。
const segToolOnlyDuplicateCall = `data: {"id":"t4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_10","function":{"name":"read_file","arguments":"{}"}}]}}]}` + "\n\n" +
	`data: {"id":"t4","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":106600,"completion_tokens":12,"total_tokens":106612}}` + "\n\n" +
	"data: [DONE]\n\n"

// B：正文够长且以句末标点收尾 → 不续写。
func TestToolOnlySkipsFinishedAnswer(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, segToolOnlyToolCall })
	r := newToolOnlyReader(t, up, toolLoopBody, segToolOnlyLongFinal)
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 0 {
		t.Fatalf("已完成的答复不该再发续写请求，实际 %d 次", len(*reqs))
	}
	s := string(out)
	if !strings.Contains(s, "全部改动已经提交并推送") {
		t.Fatalf("首段正文应照常下发:\n%s", s)
	}
	if strings.Contains(s, "tool_calls") {
		t.Fatalf("不该出现工具调用:\n%s", s)
	}
}

func TestToolOnlyLooksFinished(t *testing.T) {
	long := strings.Repeat("这是一段已经写完的总结。", 10) // >120B，句号收尾
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"长且句号收尾", long, true},
		{"长但停在冒号", strings.Repeat("我先看一下这些文件：", 10), false},
		{"短句收尾", "好的。", false},
		{"长但无标点", strings.Repeat("继续往下写", 30), false},
		{"空", "", false},
	}
	for _, c := range cases {
		if got := toolOnlyLooksFinished(c.text); got != c.want {
			t.Errorf("%s: got %v want %v (len=%d)", c.name, got, c.want, len(c.text))
		}
	}
}

// A：续写段重发上一次的调用（同名同参）→ 整段丢弃，客户端收不到任何工具分片。
func TestToolOnlyRejectsDuplicateCall(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, segToolOnlyDuplicateCall })
	r := newToolOnlyReader(t, up, toolLoopBody, segToolOnlyStop)
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("应尝试续写一次，实际 %d 次", len(*reqs))
	}
	s := string(out)
	if strings.Contains(s, "tool_calls") || strings.Contains(s, "call_10") {
		t.Fatalf("重发的调用必须被拦下，客户端不该看到:\n%s", s)
	}
	if !strings.Contains(s, "先看完整表单。") {
		t.Fatalf("首段正文应照常保留:\n%s", s)
	}
	if !strings.Contains(s, `"finish_reason":"stop"`) {
		t.Fatalf("应回滚到首段的 stop 终态:\n%s", s)
	}
	if strings.Contains(s, `"id":"t4"`) {
		t.Fatalf("续写段的帧不该下发:\n%s", s)
	}
}

// 参数归一化：历史写 {"a": 1}、续写写 {"a":1} 也算同一次调用。
func TestToolOnlyDuplicateIgnoresArgWhitespace(t *testing.T) {
	body := `{"model":"m","messages":[` +
		`{"role":"user","content":"x"},` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"write_file","arguments":"{\"path\": \"/a\", \"text\": \"hi\"}"}}]},` +
		`{"role":"tool","tool_call_id":"c1","content":"ok"}]}`
	seg := `data: {"id":"t5","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"write_file","arguments":"{\"path\":\"/a\",\"text\":\"hi\"}"}}]}}]}` + "\n\n" +
		`data: {"id":"t5","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":30000,"completion_tokens":5,"total_tokens":30005}}` + "\n\n" +
		"data: [DONE]\n\n"
	up, _ := fakeContinueUpstream(t, func(call int) (int, string) { return 200, seg })
	r := newToolOnlyReader(t, up, body, segToolOnlyStop)
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(out), "write_file") {
		t.Fatalf("仅空白差异的同参数调用也应被判为重复:\n%s", string(out))
	}
}
