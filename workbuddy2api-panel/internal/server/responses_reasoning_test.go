// /v1/responses 入站回放的思考保真（上游 #252）：input 里的 reasoning 项不再整项丢弃，
// 而是按原样补回发给上游的消息（消息级 reasoning_content）。多轮长任务里模型看不到自己
// 上一轮的思考，读 reasoning 的模型家族（kimi/glm/minimax 等）多轮一致性校验就会退化，
// 用户看到的是「越干越笨」。
package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// reasoningTestCall 提取一条出站消息的 role / reasoning_content / 首个 tool_call id。
func reasoningTestCall(t *testing.T, m any) (role, reasoning string, callID string) {
	t.Helper()
	mm, ok := m.(map[string]any)
	if !ok {
		t.Fatalf("出站消息不是 map：%v", m)
	}
	role, _ = mm["role"].(string)
	reasoning, _ = mm["reasoning_content"].(string)
	if tcs, ok := mm["tool_calls"].([]any); ok && len(tcs) > 0 {
		if tc, ok := tcs[0].(map[string]any); ok {
			callID, _ = tc["id"].(string)
		}
	}
	return role, reasoning, callID
}

// TestResponsesReasoningTwoTurn 两轮「reasoning → assistant」回放：各自带各自的思考与
// 正文，绝不合并成一条。修复前 reasoning 项被整项丢弃，assistant 历史没有思考字段。
func TestResponsesReasoningTwoTurn(t *testing.T) {
	items := []any{
		map[string]any{"role": "user", "content": "第一轮"},
		map[string]any{"type": "reasoning", "summary": []any{
			map[string]any{"type": "summary_text", "text": "推理一"}}},
		map[string]any{"type": "message", "role": "assistant", "content": "回答一"},
		map[string]any{"type": "reasoning", "summary": []any{
			map[string]any{"type": "summary_text", "text": "推理二"}}},
		map[string]any{"type": "message", "role": "assistant", "content": "回答二"},
	}
	out := convertInputItems(items, nil)
	var assistants []map[string]any
	for _, m := range out {
		if mm, ok := m.(map[string]any); ok && mm["role"] == "assistant" {
			assistants = append(assistants, mm)
		}
	}
	if len(assistants) != 2 {
		t.Fatalf("相邻两轮 assistant 被合并或丢失：%v", out)
	}
	if assistants[0]["reasoning_content"] != "推理一" || assistants[1]["reasoning_content"] != "推理二" {
		t.Fatalf("各轮思考错配：a0=%v a1=%v", assistants[0]["reasoning_content"], assistants[1]["reasoning_content"])
	}
	if assistants[0]["content"] != "回答一" || assistants[1]["content"] != "回答二" {
		t.Fatalf("各轮正文错配：a0=%v a1=%v", assistants[0]["content"], assistants[1]["content"])
	}
}

// TestResponsesReasoningConsecutive 连续多个 reasoning 项同属后一段 assistant：按 "\n"
// 拼接、一段不丢。
func TestResponsesReasoningConsecutive(t *testing.T) {
	items := []any{
		map[string]any{"role": "user", "content": "solve"},
		map[string]any{"type": "reasoning", "summary": []any{
			map[string]any{"type": "summary_text", "text": "第一段"}}},
		map[string]any{"type": "reasoning", "summary": []any{
			map[string]any{"type": "summary_text", "text": "第二段"}}},
		map[string]any{"type": "message", "role": "assistant", "content": "答案"},
	}
	out := convertInputItems(items, nil)
	if len(out) != 2 {
		t.Fatalf("消息数不对：%v", out)
	}
	role, reasoning, _ := reasoningTestCall(t, out[1])
	if role != "assistant" || reasoning != "第一段\n第二段" {
		t.Fatalf("连续思考拼接错误：role=%s reasoning=%q", role, reasoning)
	}
}

// TestResponsesReasoningGrouped 思考组被元数据项（additional_tools / local_shell_call）
// 隔开时仍归后继 assistant；元数据项自身不产生消息（修复前会变成一条假 user 空消息）。
func TestResponsesReasoningGrouped(t *testing.T) {
	items := []any{
		map[string]any{"role": "user", "content": "第一轮"},
		map[string]any{"type": "message", "role": "assistant", "content": "回答零"},
		map[string]any{"type": "reasoning", "summary": []any{
			map[string]any{"type": "summary_text", "text": "推理二之一"}}},
		map[string]any{"type": "additional_tools", "tools": []any{}},
		map[string]any{"type": "local_shell_call", "call_id": "meta"},
		map[string]any{"type": "reasoning", "summary": []any{
			map[string]any{"type": "summary_text", "text": "推理二之二"}}},
		map[string]any{"type": "message", "role": "assistant", "content": "回答二"},
	}
	out := convertInputItems(items, nil)
	if len(out) != 3 {
		t.Fatalf("元数据项被造成了消息或消息丢失：%v", out)
	}
	role0, reasoning0, _ := reasoningTestCall(t, out[1])
	role1, reasoning1, _ := reasoningTestCall(t, out[2])
	if role0 != "assistant" || reasoning0 != "" {
		t.Fatalf("前一条 assistant 不应带思考：%v", out[1])
	}
	if role1 != "assistant" || reasoning1 != "推理二之一\n推理二之二" {
		t.Fatalf("跨元数据思考组归位错误：role=%s reasoning=%q", role1, reasoning1)
	}
}

// TestResponsesReasoningToolBatch 工具调用轮：调用前的思考挂上工具批次 assistant；
// 结果轮后的总结思考挂总结轮 assistant；角色序列 user → assistant(tool) → tool →
// assistant。修复前工具批次 assistant 没有思考字段。
func TestResponsesReasoningToolBatch(t *testing.T) {
	items := []any{
		map[string]any{"role": "user", "content": "查询"},
		map[string]any{"type": "reasoning", "summary": []any{
			map[string]any{"type": "summary_text", "text": "先查工具"}}},
		map[string]any{"type": "function_call", "call_id": "c1", "name": "noop", "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": "c1", "output": "完成"},
		map[string]any{"type": "reasoning", "summary": []any{
			map[string]any{"type": "summary_text", "text": "再总结"}}},
		map[string]any{"type": "message", "role": "assistant", "content": "结论"},
	}
	out := convertInputItems(items, nil)
	var roles []string
	for _, m := range out {
		r, _, _ := reasoningTestCall(t, m)
		roles = append(roles, r)
	}
	want := []string{"user", "assistant", "tool", "assistant"}
	if strings.Join(roles, ",") != strings.Join(want, ",") {
		t.Fatalf("角色序列错误：%v（出站 %v）", roles, out)
	}
	_, reasoningBatch, callID := reasoningTestCall(t, out[1])
	if callID != "c1" || reasoningBatch != "先查工具" {
		t.Fatalf("工具批次思考错挂：call=%s reasoning=%q", callID, reasoningBatch)
	}
	_, reasoningFinal, _ := reasoningTestCall(t, out[3])
	if reasoningFinal != "再总结" {
		t.Fatalf("总结轮思考丢失：%q", reasoningFinal)
	}
}

// TestResponsesReasoningChatShaped Chat 形状回放（客户端把 chat messages 直接塞进
// input）：assistant 的内联 reasoning_content 与 tool_calls 必须保留——丢 tool_calls
// 会造出孤儿 tool 消息（上游 11148），丢思考则该轮裸奔。
func TestResponsesReasoningChatShaped(t *testing.T) {
	items := []any{
		map[string]any{"role": "user", "content": "edit"},
		map[string]any{"role": "assistant", "content": "", "reasoning_content": "先调用工具",
			"tool_calls": []any{map[string]any{"id": "ci", "type": "function",
				"function": map[string]any{"name": "noop", "arguments": "{}"}}}},
		map[string]any{"role": "tool", "tool_call_id": "ci", "content": "完成"},
	}
	out := convertInputItems(items, nil)
	if len(out) != 3 {
		t.Fatalf("消息数不对：%v", out)
	}
	role, reasoning, callID := reasoningTestCall(t, out[1])
	if role != "assistant" || reasoning != "先调用工具" || callID != "ci" {
		t.Fatalf("Chat 形状内联字段被剥：role=%s reasoning=%q call=%s", role, reasoning, callID)
	}
	if r, _ := out[2].(map[string]any)["role"].(string); r != "tool" {
		t.Fatalf("tool 消息角色错位：%v", out[2])
	}
}

// TestResponsesReasoningStaleBoundary 过期思考不跨 user 边界泄漏：reasoning 后紧跟新的
// user 消息（无 assistant 落点），该段思考就地丢弃，不挂到后续轮次。
func TestResponsesReasoningStaleBoundary(t *testing.T) {
	items := []any{
		map[string]any{"role": "user", "content": "第一轮"},
		map[string]any{"type": "reasoning", "summary": []any{
			map[string]any{"type": "summary_text", "text": "过期推理"}}},
		map[string]any{"role": "user", "content": "新请求"},
		map[string]any{"type": "message", "role": "assistant", "content": "新回答"},
	}
	out := convertInputItems(items, nil)
	if len(out) != 3 {
		t.Fatalf("消息数不对：%v", out)
	}
	role, reasoning, _ := reasoningTestCall(t, out[2])
	if role != "assistant" || reasoning != "" {
		t.Fatalf("过期思考跨边界泄漏：role=%s reasoning=%q", role, reasoning)
	}
}

// TestResponsesReasoningTrailing 对话末尾的 reasoning（其后没有新 assistant）：归前一条
// assistant，而不是丢掉。
func TestResponsesReasoningTrailing(t *testing.T) {
	items := []any{
		map[string]any{"role": "user", "content": "问"},
		map[string]any{"type": "message", "role": "assistant", "content": "答"},
		map[string]any{"type": "reasoning", "summary": []any{
			map[string]any{"type": "summary_text", "text": "补记思考"}}},
	}
	out := convertInputItems(items, nil)
	if len(out) != 2 {
		t.Fatalf("末尾 reasoning 被造成了消息：%v", out)
	}
	role, reasoning, _ := reasoningTestCall(t, out[1])
	if role != "assistant" || reasoning != "补记思考" {
		t.Fatalf("末尾思考未归位：role=%s reasoning=%q", role, reasoning)
	}
}

// TestResponsesReasoningCompaction compaction 项解码为 system 交接摘要：标准形态
// base64(JSON {"v":1,"summary":...})；解不开的纯文本保原文；空内容不造消息。
//
// 本仓没有网关侧生成 compaction 的路径，这里只钉入站回放：此前 compaction 落进默认分支
// 会被造成一条假 user 空消息，既切断思考组又向上游塞脏数据。
func TestResponsesReasoningCompaction(t *testing.T) {
	enc := base64.StdEncoding.EncodeToString([]byte(`{"v":1,"summary":"前情提要：改了三个文件"}`))
	items := []any{
		map[string]any{"type": "compaction", "encrypted_content": enc},
		map[string]any{"role": "user", "content": "继续"},
	}
	out := convertInputItems(items, nil)
	if len(out) != 2 {
		t.Fatalf("compaction 应解码成一条 system 消息：%v", out)
	}
	sys, _ := out[0].(map[string]any)
	if sys["role"] != "system" {
		t.Fatalf("compaction 摘要应是 system 消息：%v", out[0])
	}
	if content, _ := sys["content"].(string); !strings.Contains(content, "前情提要：改了三个文件") ||
		!strings.HasPrefix(content, "Summary of the earlier conversation (context checkpoint):") {
		t.Fatalf("compaction 摘要内容错误：%q", content)
	}
	// 解码失败（非 base64 的纯文本）：保留原文，不丢摘要。
	out = convertInputItems([]any{
		map[string]any{"type": "context_compaction", "encrypted_content": "纯文本摘要"},
		map[string]any{"role": "user", "content": "继续"},
	}, nil)
	if len(out) != 2 {
		t.Fatalf("纯文本摘要被丢弃：%v", out)
	}
	if content, _ := out[0].(map[string]any)["content"].(string); !strings.Contains(content, "纯文本摘要") {
		t.Fatalf("解码失败应保原文：%v", out[0])
	}
	// 空内容：不造空消息。
	out = convertInputItems([]any{
		map[string]any{"type": "compaction_summary", "encrypted_content": "  "},
		map[string]any{"role": "user", "content": "继续"},
	}, nil)
	if len(out) != 1 {
		t.Fatalf("空 compaction 不应生成消息：%v", out)
	}
}

// TestResponsesReasoningEndToEndWire 端到端：reasoning 回放 → 出站管线 → 上游收到的
// assistant 同时带 reasoning_content 与镜像出的 reasoning（kimi 家族读后者）。这钉住
// 「回放」与既有 mirrorUpstreamReasoning 的衔接：转换层若再丢思考，镜像无从补起。
func TestResponsesReasoningEndToEndWire(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) {
		return 200, `data: {"id":"r1","choices":[{"index":0,"delta":{"content":"ok"}}]}` + "\n\n" +
			`data: {"id":"r1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
			"data: [DONE]\n\n"
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	body := `{"model":"kimi-k3","stream":false,"input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"run"}]},` +
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"思考一"}]},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"答一"}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"again"}]}]}`

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
	for _, m := range wire.Messages {
		if m["role"] != "assistant" {
			continue
		}
		rc, _ := m["reasoning_content"].(string)
		r, _ := m["reasoning"].(string)
		if rc != "思考一" {
			t.Fatalf("上游未收到回放思考（reasoning_content=%q）：%s", rc, (*reqs)[0])
		}
		if r != "思考一" {
			t.Fatalf("kimi 家族的 reasoning 镜像缺失（reasoning=%q）：%s", r, (*reqs)[0])
		}
		return
	}
	t.Fatalf("出站没有 assistant 消息：%s", (*reqs)[0])
}
