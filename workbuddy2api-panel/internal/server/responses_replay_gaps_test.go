// 回放缺口回归：入站历史里那些「上游没有对应物」的项不能被造成消息，也不能把配对的
// 工具调用/结果拆散。对应上游 94a6879（Chat 形状 tool 结果保留 tool_call_id）、
// fdf05ea（跳过托管工具回执等无内容未知项）、f668215（compaction 密文不当摘要）。
//
// 用例按形态拆成子测试：每种形态单独判失败，红的时候一眼看出是哪一类项漏了。
package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// replayWire 把一个 /v1/responses 请求走完整条出站管线（含 cleanupOrphanToolCalls），
// 返回上游实际收到的 messages。
func replayWire(t *testing.T, body string) []map[string]any {
	t.Helper()
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) {
		return 200, `data: {"id":"r1","choices":[{"index":0,"delta":{"content":"ok"}}]}` + "\n\n" +
			`data: {"id":"r1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
			"data: [DONE]\n\n"
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))
	if len(*reqs) == 0 {
		t.Fatalf("没有发出上游请求，响应: %d %s", rec.Code, rec.Body.String())
	}
	var wire struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal([]byte((*reqs)[0]), &wire); err != nil {
		t.Fatalf("出站 body 解析失败: %v", err)
	}
	return wire.Messages
}

func replayRoles(out []any) string {
	var roles []string
	for _, m := range out {
		mm, _ := m.(map[string]any)
		roles = append(roles, asString(mm["role"]))
	}
	return strings.Join(roles, ",")
}

// emptyUserMessages 数出站里的 user 空消息（回放漏项造成的假 user）。
func emptyUserMessages(msgs []map[string]any) int {
	n := 0
	for _, m := range msgs {
		if asString(m["role"]) != "user" {
			continue
		}
		if strings.TrimSpace(asString(m["content"])) == "" {
			n++
		}
	}
	return n
}

// TestResponsesReplayChatShapedToolKeepsPair Chat 形状的 tool 消息必须带着 tool_call_id
// 出站：丢了它，cleanupOrphanToolCalls 看到的是一条无结果的 tool_call 加一条无主 tool
// 消息，会把这一对调用与结果一起删掉，上游读到的历史里这轮工具调用凭空消失。
func TestResponsesReplayChatShapedToolKeepsPair(t *testing.T) {
	body := `{"model":"glm-5.2","stream":false,` +
		`"tools":[{"type":"function","name":"noop","parameters":{"type":"object","properties":{}}}],"input":[` +
		`{"role":"user","content":"edit"},` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"ci","type":"function","function":{"name":"noop","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"ci","name":"noop","content":"完成"},` +
		`{"role":"user","content":"继续"}]}`
	msgs := replayWire(t, body)
	var callID, resultID, resultName string
	for _, m := range msgs {
		switch m["role"] {
		case "assistant":
			if tcs, _ := m["tool_calls"].([]any); len(tcs) > 0 {
				callID = asString(tcs[0].(map[string]any)["id"])
			}
		case "tool":
			resultID, resultName = asString(m["tool_call_id"]), asString(m["name"])
		}
	}
	if callID != "ci" || resultID != "ci" || resultName != "noop" {
		t.Fatalf("Chat 形状工具配对被拆：call=%q result=%q name=%q 出站=%v", callID, resultID, resultName, msgs)
	}
}

// TestResponsesReplaySkipsHostedToolItems 托管工具与其他不认识、没有 role 也没有内容的
// 项（web_search_call、image_generation_call、mcp_call 等）不生成消息：此前落入默认
// 分支变成一条 user 空消息，既拆开同一批工具调用、把工具图插进并行 tool 结果之间
// （11148），又清空调用前暂存的思考。image_generation_call 的 base64 成品绝不能出站。
func TestResponsesReplaySkipsHostedToolItems(t *testing.T) {
	think := map[string]any{"type": "reasoning", "summary": []any{
		map[string]any{"type": "summary_text", "text": "先查再画"}}}

	t.Run("夹在同一批工具调用之间", func(t *testing.T) {
		blob := strings.Repeat("iVBORw0KGgo", 4096)
		out := convertInputItems([]any{
			map[string]any{"role": "user", "content": "查完画图"},
			think,
			map[string]any{"type": "web_search_call", "id": "ws_1", "status": "completed",
				"action": map[string]any{"type": "search", "query": "q"}},
			map[string]any{"type": "function_call", "call_id": "c1", "name": "noop", "arguments": "{}"},
			map[string]any{"type": "image_generation_call", "id": "ig_1", "status": "completed", "result": blob},
			map[string]any{"type": "function_call", "call_id": "c2", "name": "noop", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "c1", "output": "一"},
			map[string]any{"type": "function_call_output", "call_id": "c2", "output": "二"},
		}, nil)
		if got := replayRoles(out); got != "user,assistant,tool,tool" {
			t.Fatalf("角色序列 %s：托管项造出了消息或拆开了批次（出站 %v）", got, out)
		}
		if tcs, _ := out[1].(map[string]any)["tool_calls"].([]any); len(tcs) != 2 {
			t.Fatalf("同一批工具调用被托管项拆成两批：%v", out)
		}
		if _, reasoning, _ := reasoningTestCall(t, out[1]); reasoning != "先查再画" {
			t.Fatalf("托管项清掉了调用前的思考：%q", reasoning)
		}
		if raw, _ := json.Marshal(out); strings.Contains(string(raw), "iVBORw0KGgo") {
			t.Fatal("image_generation_call 的 base64 成品出站了")
		}
	})

	t.Run("夹在并行工具结果之间", func(t *testing.T) {
		img := []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64,aGk="}}
		out := convertInputItems([]any{
			map[string]any{"role": "user", "content": "看图"},
			map[string]any{"type": "function_call", "call_id": "c1", "name": "view", "arguments": "{}"},
			map[string]any{"type": "function_call", "call_id": "c2", "name": "view", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "c1", "output": img},
			map[string]any{"type": "mcp_call", "id": "mcp_1", "server_label": "s", "name": "t", "arguments": "{}", "output": "x"},
			map[string]any{"type": "function_call_output", "call_id": "c2", "output": "二"},
		}, nil)
		if got := replayRoles(out); got != "user,assistant,tool,tool,user" {
			t.Fatalf("角色序列 %s：图片被插进了并行 tool 结果之间（出站 %v）", got, out)
		}
	})

	t.Run("compaction 是前瞻边界", func(t *testing.T) {
		// compaction 在主循环里清空暂存思考，前瞻也要把它当边界：其前的思考归前一条
		// assistant，而不是暂存后被 compaction 清掉。
		out := convertInputItems([]any{
			map[string]any{"role": "user", "content": "问"},
			map[string]any{"type": "message", "role": "assistant", "content": "答"},
			think,
			map[string]any{"type": "compaction", "encrypted_content": "前情提要 已改完"},
			map[string]any{"type": "message", "role": "assistant", "content": "再答"},
		}, nil)
		if _, reasoning, _ := reasoningTestCall(t, out[1]); reasoning != "先查再画" {
			t.Fatalf("compaction 前的思考丢了：%v", out)
		}
	})

	t.Run("带内容的未知项照旧送达", func(t *testing.T) {
		out := convertInputItems([]any{map[string]any{"type": "future_note", "content": "保留我"}}, nil)
		if len(out) != 1 || out[0].(map[string]any)["content"] != "保留我" {
			t.Fatalf("带内容的未知项被丢：%v", out)
		}
	})
}

// TestResponsesReplayCompactionNeverInjectsBlob compaction 交接摘要的兜底：真实的
// encrypted_content 是密文（URL-safe base64、标准 base64 都有），解不出摘要时不能把
// 密文当成摘要塞给上游；解出的摘要为空不造消息；纯文本摘要有长度上限。
func TestResponsesReplayCompactionNeverInjectsBlob(t *testing.T) {
	for _, tc := range []struct{ name, enc string }{
		{"URL-safe 密文", "gAAAAABnX2_pQ-" + strings.Repeat("Zm9vYmFyYmF6_-", 64)},
		{"标准 base64 非 JSON", "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8gISIjJCUmJygpKissLS4v"},
		{"摘要为空的 JSON", "eyJ2IjoxLCJzdW1tYXJ5IjoiIn0="},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := convertInputItems([]any{
				map[string]any{"type": "compaction", "encrypted_content": tc.enc},
				map[string]any{"role": "user", "content": "继续"},
			}, nil)
			if len(out) != 1 {
				t.Fatalf("不该生成交接摘要：%v", out)
			}
		})
	}
	t.Run("纯文本摘要封顶", func(t *testing.T) {
		long := strings.Repeat("改了 a.go 与 b.go，测试已通过。", 8192)
		out := convertInputItems([]any{
			map[string]any{"type": "compaction", "encrypted_content": long},
			map[string]any{"role": "user", "content": "继续"},
		}, nil)
		if len(out) != 2 {
			t.Fatalf("纯文本摘要被丢：%d 条", len(out))
		}
		content := asString(out[0].(map[string]any)["content"])
		if len(content) > compactionSummaryMax+256 || !strings.Contains(content, "改了 a.go") {
			t.Fatalf("纯文本摘要没有封顶：%d 字节", len(content))
		}
	})
}

// TestResponsesReplayPreItemsSurviveHostedReceipts 本仓特有的一对关系：网关代执行的托管
// 出图（responsesMeta.preItems）是**回程**产出，客户端下一轮会把它作为
// image_generation_call 项回放进 input。两条路必须分开走——
//   - 入站回执：跳过，不造 user 空消息、不把 base64 成品重发给上游；
//   - 回程 preItems：照旧交付，仍排 output 首位（本仓契约，不能因为入站跳过而丢）。
func TestResponsesReplayPreItemsSurviveHostedReceipts(t *testing.T) {
	cdn, png := imageToolCDN(t)
	f := &imageToolFake{cdn: cdn}
	h := newImageToolHandler(f)

	replayed := strings.Repeat("cmVwbGF5ZWQtYmxvYg", 64) // 上一轮回放的成品（base64 密文样）
	body := `{"model":"glm-5.2","stream":false,"tools":[{"type":"image_generation"}],"input":[` +
		`{"role":"user","content":"画一张中秋电梯广告图"},` +
		`{"type":"image_generation_call","id":"ig_old","status":"completed","result":"` + replayed + `"},` +
		`{"type":"message","role":"assistant","content":"已交付上一张图"}]}`

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Output []map[string]any `json:"output"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("非 JSON 响应：%s", rec.Body.String())
	}
	// 回程 preItems 必须还在首位，且是这一轮新出的图（不是回放的那份）。
	if len(resp.Output) == 0 || asString(resp.Output[0]["type"]) != "image_generation_call" {
		t.Fatalf("output 首位应是本轮 image_generation_call：%v", resp.Output)
	}
	if got := asString(resp.Output[0]["result"]); got == "" || got == replayed {
		t.Fatalf("本轮成品缺失或被回放项顶替：%q", got)
	}
	if got, want := asString(resp.Output[0]["result"]), base64.StdEncoding.EncodeToString(png); got != want {
		t.Fatalf("result 不是本轮成品图的 base64：%q", got)
	}
	// 入站回执被跳过：上游看不到那串回放密文，也没有凭空多出的 user 空消息。
	var wire struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal([]byte(f.chatBodies[len(f.chatBodies)-1]), &wire); err != nil {
		t.Fatalf("出站 body 解析失败：%v", err)
	}
	if n := emptyUserMessages(wire.Messages); n != 0 {
		t.Fatalf("入站托管回执造出了 %d 条 user 空消息：%v", n, wire.Messages)
	}
	if strings.Contains(f.chatBodies[len(f.chatBodies)-1], replayed) {
		t.Fatal("回放的 base64 成品被重发给上游了")
	}
}
