package upstream

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestGuardEmptyTextPartsShapes 逐形态钉住「哪些 text 段会被补、哪些不动」。
//
// 形态矩阵来自上游实测（2026-11-08，kimi-k3.1-pro + cn 号池；见 empty_text_part.go
// 文件头）：只有「空串 / null / 缺失」三种会被判 11151，`"   "` 与未识别 type 放行。
// 因此补位的**边界**和补位本身一样重要——多补一格就是在改上游本来接受的报文。
func TestGuardEmptyTextPartsShapes(t *testing.T) {
	cases := []struct {
		name string
		in   string // 单条消息的 content
		want string // 期望的 content（JSON 字面量）
	}{
		{"空串补位", `[{"type":"text","text":""}]`, `[{"type":"text","text":" "}]`},
		{"null 补位", `[{"type":"text","text":null}]`, `[{"type":"text","text":" "}]`},
		{"缺失补位", `[{"type":"text"}]`, `[{"type":"text","text":" "}]`},
		{"纯空白不动", `[{"type":"text","text":"   "}]`, `[{"type":"text","text":"   "}]`},
		{"未识别 type 不动", `[{"type":"textarea","text":""}]`, `[{"type":"textarea","text":""}]`},
		{"非数组对象不动", `{"type":"text","text":""}`, `{"type":"text","text":""}`},
		{"普通字符串不动", `"hello"`, `"hello"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := `{"model":"m","messages":[{"role":"user","content":` + c.in + `}]}`
			out := PrepareBodyOpt([]byte(body), false)
			var obj map[string]any
			if err := json.Unmarshal(out, &obj); err != nil {
				t.Fatalf("输出不是合法 JSON: %v (%s)", err, out)
			}
			got, err := json.Marshal(obj["messages"].([]any)[0].(map[string]any)["content"])
			if err != nil {
				t.Fatal(err)
			}
			// 期望值也要归一化（对象键序无关）
			var want, gotAny any
			if err := json.Unmarshal([]byte(c.want), &want); err != nil {
				t.Fatalf("want 不是合法 JSON: %v", err)
			}
			if err := json.Unmarshal(got, &gotAny); err != nil {
				t.Fatal(err)
			}
			wb, _ := json.Marshal(want)
			gb, _ := json.Marshal(gotAny)
			if string(gb) != string(wb) {
				t.Fatalf("content 变成 %s，期望 %s", gb, wb)
			}
		})
	}
}

// TestGuardEmptyTextPartsSniffedString 字符串信封信封被上游当 parts 嗅探：
// 工具结果里抓到的表单/按钮数组（`[{"type":"text","text":""},…]`）正是这个形态，
// 它会变成一个空文本段 → 11151。这里钉住「补位并重新序列化」，同时钉住反例：
// 普通文本、解析失败、空数组、非对象数组都必须一字不动（否则正常工具结果会被
// 无谓地重写，下游按原文比对的逻辑会漂）。
func TestGuardEmptyTextPartsSniffedString(t *testing.T) {
	msg := func(content any) map[string]any {
		return map[string]any{"role": "tool", "content": content}
	}

	// 命中：补位后重新序列化
	hit := `[{"type":"text","text":""},{"type":"text","text":"ok"}]`
	msgs := []any{msg(hit)}
	guardEmptyTextParts(msgs)
	got, _ := msgs[0].(map[string]any)["content"].(string)
	if got == hit {
		t.Fatalf("嗅探命中的字符串应被补位，实际原样：%q", got)
	}
	var parts []map[string]any
	if err := json.Unmarshal([]byte(got), &parts); err != nil {
		t.Fatalf("补位后不是合法 JSON: %v (%q)", err, got)
	}
	if parts[0]["text"] != " " || parts[1]["text"] != "ok" {
		t.Fatalf("补位结果不对：%+v", parts)
	}

	// 反例：都不该被改写
	for _, c := range []string{
		"普通工具结果文本",
		"[1,2,3]",                      // 非对象数组
		"[]",                           // 空数组
		`[{"type":"text"`,              // 解析失败
		`{"type":"text","text":""}`,    // 对象（不是数组）
		`[{"type":"text","text":"x"}]`, // 没有空段
	} {
		msgs := []any{msg(c)}
		guardEmptyTextParts(msgs)
		if got := msgs[0].(map[string]any)["content"]; got != c {
			t.Errorf("不该改写 %q，实际变成 %q", c, got)
		}
	}
}

// TestGuardEmptyTextPartsKeepsNonTextParts 多模态：图片等非 text 段必须原引用保留
// （只克隆要改的 text 段）。同时钉住「同一消息里 text 与 image 混排时下标不变」——
// 删段派生的实现会在这里红。
func TestGuardEmptyTextPartsKeepsNonTextParts(t *testing.T) {
	img := map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AA"}}
	msgs := []any{map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{"type": "text", "text": ""},
			img,
		},
	}}
	guardEmptyTextParts(msgs)
	parts := msgs[0].(map[string]any)["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("段数变了：%d，删段会改段落下标", len(parts))
	}
	if reflect.ValueOf(parts[1]).Pointer() != reflect.ValueOf(img).Pointer() {
		t.Fatalf("image 段应保持原引用（未被克隆/改写），实际 %#v", parts[1])
	}
	if parts[0].(map[string]any)["text"] != " " {
		t.Fatalf("空 text 段未补位：%#v", parts[0])
	}
}

// TestMirrorUpstreamReasoning 镜像只对「按 reasoning 读历史」的家族生效，且只抄不造。
func TestMirrorUpstreamReasoning(t *testing.T) {
	body := func(model string, msgs string) []byte {
		return []byte(`{"model":"` + model + `","messages":[` + msgs + `]}`)
	}
	asst := func(extra string) string {
		return `{"role":"assistant","content":"答"` + extra + `}`
	}

	// 命中：六个家族各来一发（含 realm 前缀与大小写）
	for _, model := range []string{"kimi-k3.1-pro", "cn:glm-5.3", "minimax-m3", "MiMo-7B", "step-5", "space-bunny-x"} {
		out := PrepareBodyOpt(body(model, asst(`,"reasoning_content":"想过了"`)), false)
		if !strings.Contains(string(out), `"reasoning":"想过了"`) {
			t.Errorf("%s：reasoning_content 未镜像进 reasoning，输出 %s", model, out)
		}
	}

	// 反例一：不在名单里的模型一字不加（凭空多键会让原模型看到陌生字段）
	for _, model := range []string{"gpt-5", "qwen3-max"} {
		out := PrepareBodyOpt(body(model, asst(`,"reasoning_content":"想过了"`)), false)
		if strings.Contains(string(out), `"reasoning":"`) {
			t.Errorf("%s：不该镜像，输出 %s", model, out)
		}
	}

	// deepseek 不走这份：它的 reasoning 由 thinking.go 的 backfillReasoningContent
	// 供给（含空值占位）。这里只钉「两条路径互不干扰」——deepseek 仍然有 reasoning，
	// 但那是既有行为，不是本次新增。
	out := PrepareBodyOpt(body("deepseek-v4.1-flash", asst(`,"reasoning_content":"想过了"`)), false)
	if !strings.Contains(string(out), `"reasoning":"想过了"`) {
		t.Errorf("deepseek 的 reasoning 应仍由 backfillReasoningContent 提供：%s", out)
	}

	// 反例二：reasoning 已有非空值时不得覆盖（上游自己的版本更权威）
	out = PrepareBodyOpt(body("kimi-k3.1-pro", asst(`,"reasoning":"上游写的","reasoning_content":"客户端的"`)), false)
	if !strings.Contains(string(out), `"reasoning":"上游写的"`) {
		t.Errorf("已有的非空 reasoning 被覆盖了：%s", out)
	}

	// 反例三：空/缺失的 reasoning_content 不制造字段（与 deepseek 那份的纪律相反）
	for _, extra := range []string{``, `,"reasoning_content":""`, `,"reasoning_content":null`} {
		out := PrepareBodyOpt(body("kimi-k3.1-pro", asst(extra)), false)
		if strings.Contains(string(out), `"reasoning":`) {
			t.Errorf("空 reasoning_content 不该产生 reasoning 字段（extra=%q）：%s", extra, out)
		}
	}

	// 反例四：非 assistant 角色不镜像（工具结果里的同名字段是数据不是思考）
	out = PrepareBodyOpt(body("kimi-k3.1-pro", `{"role":"tool","content":"x","reasoning_content":"别抄我"}`), false)
	if strings.Contains(string(out), `"reasoning":"别抄我"`) {
		t.Errorf("tool 消息不该被镜像：%s", out)
	}
}

// TestReadsUpstreamReasoning 判据本身：前缀命中表 + 空串/裸名的边界。
//
// 前缀匹配是**故意宽松**的（与上游实现同口径）：`step` 会同时命中 `steps`、`glm`
// 会命中 `glm-free`。收紧成精确模型名表会让新上架的 kimi/glm 型号静默漏掉，而多
// 命中一个陌生名字的代价只是多发一个没人读的键。
func TestReadsUpstreamReasoning(t *testing.T) {
	yes := []string{"kimi", "kimi-k2", "glm-9.9", "glm-free", "minimax-m3", "mimo-7b", "step-5-preview", "steps", "space-bunny", "global:glm-5.2", "  Kimi-K3  "}
	no := []string{"", "deepseek-v4.1-flash", "gpt-5.6-sol", "qwen3-max", "claude-sonnet-4"}
	for _, m := range yes {
		if !readsUpstreamReasoning(m) {
			t.Errorf("%q 应命中", m)
		}
	}
	for _, m := range no {
		if readsUpstreamReasoning(m) {
			t.Errorf("%q 不该命中", m)
		}
	}
}
