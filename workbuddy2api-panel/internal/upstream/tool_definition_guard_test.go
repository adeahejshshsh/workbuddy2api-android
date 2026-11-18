// tool_definition_guard_test.go 上游工具定义校验的三处收口（上游 733942a）。
//
// 三处都是 2026-11-18 在 www.workbuddy.cn 上逐形态单独实测的产物（每个样本只带一处
// 缺陷，见 payload.go 的 schemaNumberKeys 注释与 isToolSchemaRejected 的说明）：
//
//  1. 11152 工具名不合规/重名 必须归 ErrBadParams，否则落 ErrClient → 喂连败 + 轮转
//     全池 → 末端 503；
//  2. 关键字取值类型错（minimum:"1" 等）出站前纠偏，别指望上游宽容；
//  3. ErrBadParams 的 gateway_hint 要指出问题在 tools，否则客户端只看到「工具定义
//     不合规」，看不出该改哪儿。
package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestClassify11152ToolName(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want ErrKind
	}{
		{"紧凑 code", `{"code":11152,"msg":"the tool name is invalid or duplicated"}`, ErrBadParams},
		{"code 带空格", `{"code": 11152, "msg":"x"}`, ErrBadParams},
		{"code 字符串", `{"code":"11152","msg":"x"}`, ErrBadParams},
		{"嵌套在 extError", `{"extError":{"code":"invalid_request_error"},"code":11152}`, ErrBadParams},
		{"相邻 code 不误伤", `{"code":111520,"msg":"x"}`, ErrClient},
	} {
		if got := Classify(400, tc.body); got != tc.want {
			t.Errorf("%s: Classify(400, %s) = %v, want %v", tc.name, tc.body, got, tc.want)
		}
	}
	if got := FrameKind(`{"error":{"message":"x","code":11152}}`); got != ErrBadParams {
		t.Errorf("FrameKind(11152 frame) = %v, want ErrBadParams", got)
	}
}

func TestGatewayHintToolDefinition(t *testing.T) {
	for _, body := range []string{
		`{"code":11129,"msg":"invalid function call parameters"}`,
		`{"code":11152,"msg":"the tool name is invalid or duplicated"}`,
	} {
		got := GatewayHint(ErrBadParams, body, HintContext{})
		if !strings.Contains(got, "tool definition") {
			t.Errorf("%s: hint = %q, want one naming tool definitions", body, got)
		}
	}
	// 11101 请求体 JSON 畸形与工具无关，不能指向 tools（客户端会去翻自己的 schema）。
	if got := GatewayHint(ErrBadParams, `{"code":11101,"msg":"Unmarshal chat params failed"}`, HintContext{}); got != "" {
		t.Errorf("11101 hint = %q, want empty", got)
	}
}

// TestCoerceSchemaKeywordTypes 上游按 Go 强类型反序列化 schema：字符串形态的数值/布尔
// 关键字、数字形态的字符串关键字、非数组的 enum、非布尔的 additionalProperties 都整体
// 400（code=11129）。纠不动就删键——留着非法值等于保证整颗工具必然被拒。
func TestCoerceSchemaKeywordTypes(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function",` +
		`"function":{"name":"f","parameters":{"type":"object","properties":{` +
		`"n":{"type":"integer","minimum":"1","maximum":"abc"},` +
		`"c":{"type":"array","items":{"type":"string"},"uniqueItems":"true"},` +
		`"d":{"type":"string","description":123},` +
		`"e":{"type":"string","enum":"x"},` +
		`"o":{"type":"object","properties":{},"additionalProperties":"false"}},` +
		`"required":["n","n","c"]}}}]}`
	var obj map[string]any
	if err := json.Unmarshal(PrepareBodyOpt([]byte(body), false), &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	params := obj["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
	props := params["properties"].(map[string]any)

	if got := props["n"].(map[string]any)["minimum"]; got != float64(1) {
		t.Errorf("minimum = %v (%T), want 1 (number)", got, got)
	}
	if _, ok := props["n"].(map[string]any)["maximum"]; ok {
		t.Errorf("maximum \"abc\" should be dropped, got %v", props["n"])
	}
	if got := props["c"].(map[string]any)["uniqueItems"]; got != true {
		t.Errorf("uniqueItems = %v (%T), want true (bool)", got, got)
	}
	if _, ok := props["d"].(map[string]any)["description"]; ok {
		t.Errorf("numeric description should be dropped, got %v", props["d"])
	}
	if _, ok := props["e"].(map[string]any)["enum"]; ok {
		t.Errorf("non-array enum should be dropped, got %v", props["e"])
	}
	if got := props["o"].(map[string]any)["additionalProperties"]; got != false {
		t.Errorf("additionalProperties = %v (%T), want false (bool)", got, got)
	}
	// required 重复项同样 400 11129（真机：["a","a"]），必须去重。
	req := params["required"]
	if got, ok := req.([]any); !ok || len(got) != 2 || got[0] != "n" || got[1] != "c" {
		t.Errorf("required = %v, want [n c] (deduped)", req)
	}
}

// TestTopLevelRefInlinedOrDropped 顶层 $ref 实测 400 11129（嵌套 $ref + $defs 反而 200），
// 所以只处理顶层：能按 $defs 展开就保住真实参数，展不开也必须删掉。
func TestTopLevelRefInlinedOrDropped(t *testing.T) {
	params := func(body string) map[string]any {
		t.Helper()
		var obj map[string]any
		if err := json.Unmarshal(PrepareBodyOpt([]byte(body), false), &obj); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return obj["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
	}

	t.Run("有 $defs 时展开", func(t *testing.T) {
		p := params(`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function",` +
			`"function":{"name":"f","parameters":{"$ref":"#/$defs/Foo",` +
			`"$defs":{"Foo":{"type":"object","properties":{"x":{"type":"string"}}}}}}}]}`)
		if _, ok := p["$ref"]; ok {
			t.Fatalf("$ref should be gone, got %v", p)
		}
		props, _ := p["properties"].(map[string]any)
		if _, ok := props["x"]; !ok {
			t.Fatalf("inlined properties missing, got %v", p)
		}
	})

	t.Run("无 $defs 时只删 ref", func(t *testing.T) {
		p := params(`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function",` +
			`"function":{"name":"f","parameters":{"$ref":"#/$defs/Foo"}}}]}`)
		if _, ok := p["$ref"]; ok {
			t.Fatalf("$ref should be gone, got %v", p)
		}
		if p["type"] != "object" {
			t.Fatalf("type = %v, want object", p["type"])
		}
	})
}
