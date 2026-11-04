package upstream

// 工具 schema 的 enum 缺 type 时补推断类型（上游 2026-10-29 `49f9db4` #273）。
//
// Kimi 系对参数 schema 严格校验：属性只写 enum 不写 type 直接 400 11133
// 「Invalid request parameters」——而 DSH 等客户端的整套工具定义里就有这种写法，
// 于是该模型对这些客户端等于全废。

import (
	"encoding/json"
	"testing"
)

func schemaOf(t *testing.T, raw string) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		t.Fatalf("bad fixture: %v", err)
	}
	return obj
}

func TestEnumMissingTypeInferred(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string // "" = 不该补
	}{
		{"全字符串", `{"enum":["a","b"]}`, "string"},
		{"全整数", `{"enum":[1,2,3]}`, "integer"},
		{"含小数", `{"enum":[1,2.5]}`, "number"},
		{"全布尔", `{"enum":[true,false]}`, "boolean"},
		{"混合标量（不猜）", `{"enum":["a",1]}`, ""},
		{"含 null（不猜）", `{"enum":[null,"a"]}`, ""},
		{"已有 type 不覆盖", `{"type":"string","enum":[1,2]}`, "string"},
	}
	for _, c := range cases {
		obj := schemaOf(t, c.raw)
		unescapePatternLiteralEscapes(obj)
		got, _ := obj["type"].(string)
		if got != c.want {
			t.Errorf("%s: type=%q want %q", c.name, got, c.want)
		}
	}
}

// 嵌套在 properties / items 里的也要补到（Kimi 校验的是整棵树）。
func TestEnumTypeInferredNested(t *testing.T) {
	obj := schemaOf(t, `{"type":"object","properties":{"mode":{"enum":["fast","safe"]}},"items":{"enum":[1.5]}}`)
	unescapePatternLiteralEscapes(obj)
	props := obj["properties"].(map[string]any)
	if got, _ := props["mode"].(map[string]any)["type"].(string); got != "string" {
		t.Errorf("嵌套 properties 未补: %q", got)
	}
	if got, _ := obj["items"].(map[string]any)["type"].(string); got != "number" {
		t.Errorf("items 未补: %q", got)
	}
}

// enum 取值必须一字不改（补类型不能动值）。
func TestEnumValuesUntouched(t *testing.T) {
	obj := schemaOf(t, `{"enum":["a","b"]}`)
	unescapePatternLiteralEscapes(obj)
	vals := obj["enum"].([]any)
	if len(vals) != 2 || vals[0] != "a" || vals[1] != "b" {
		t.Fatalf("enum 值被改动: %v", vals)
	}
}

// 走**真实管线**（prepareBodyOptFull 用 dec.UseNumber()）验证数字 enum 补类型。
// 此前用例都用默认 Unmarshal 造夹具（数字是 float64），于是「生产路径上数字是
// json.Number」这个事实被完全绕过 —— 修复实际上从未生效，用例却一直绿。
func TestEnumTypeFilledThroughRealPipeline(t *testing.T) {
	cases := []struct {
		name string
		enum string
		want string
	}{
		{"整数 enum", `[1,2,3]`, "integer"},
		{"小数 enum", `[1.5,2.5]`, "number"},
		{"大整数 enum（UseNumber 才保得住原文）", `[9007199254740993,9007199254740995]`, "integer"},
		{"字符串 enum", `["a","b"]`, "string"},
		{"布尔 enum", `[true,false]`, "boolean"},
		{"混合标量不猜", `[1,"a"]`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := []byte(`{"model":"kimi-k2","messages":[],"tools":[{"type":"function",` +
				`"function":{"name":"f","parameters":{"type":"object","properties":{` +
				`"p":{"enum":` + c.enum + `}}}}}]}`)
			// 真实主改写管线（与 chat 出站同一条）
			out := PrepareBodyOptWithEffortPolicy(src, false, nil, nil, "cn", nil)
			var obj map[string]any
			if err := json.Unmarshal(out, &obj); err != nil {
				t.Fatalf("输出不是合法 JSON: %v", err)
			}
			params := obj["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
			p := params["properties"].(map[string]any)["p"].(map[string]any)
			got, _ := p["type"].(string)
			if got != c.want {
				t.Fatalf("enum=%s 补出的 type=%q want %q（生产路径上数字是 json.Number）", c.enum, got, c.want)
			}
		})
	}
}
