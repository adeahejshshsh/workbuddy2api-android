// schema_normalize_test.go 工具 schema 规整的「不动数据」边界（上游 d6301fe）。
//
// 深度规整（payload.go 的 normalizeSchemaTree）只该改**形态**：缺的类型、非标别名、
// 非法取值类型、容器位置。一旦递归进数据关键字，改的就是客户端声明的取值本身——
// 模型看到的默认值、常量、枚举项全变，而这类改动不会报错、只会让工具行为悄悄变样。
//
// 三条边界：
//  1. default / const / enum / example(s) 里的对象是数据，出现 type/items/required/null
//     也只是数据，必须原样保留；
//  2. dependentRequired / dependencies 的键是属性名（数据），值是属性名数组或子
//     schema —— 键叫 items/type 也不能当关键字改写，子 schema 照常归整；
//  3. const:null / default:null 是有意义的取值（「只能是 null」「缺省为 null」），
//     不是缺失的元数据；description:null 这类元数据照旧剔除。
package upstream

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// normalizeParamsJSON 把一段 parameters JSON 走一遍 normalizeToolParameters，返回结果。
func normalizeParamsJSON(t *testing.T, src string) map[string]any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(src), &v); err != nil {
		t.Fatalf("夹具不是合法 JSON: %v", err)
	}
	out, ok := normalizeToolParameters(v).(map[string]any)
	if !ok {
		t.Fatalf("结果不是 object: %#v", out)
	}
	return out
}

func mustJSON(t *testing.T, src string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(src), &v); err != nil {
		t.Fatalf("夹具不是合法 JSON: %v", err)
	}
	return v
}

func propOf(t *testing.T, params map[string]any, name string) map[string]any {
	t.Helper()
	props, _ := params["properties"].(map[string]any)
	p, ok := props[name].(map[string]any)
	if !ok {
		t.Fatalf("属性 %s 丢了: %#v", name, props)
	}
	return p
}

// TestNormalizeSchemaKeepsDataKeywords default / const / enum / example(s) 的值是数据，
// 不是 schema：里面出现 type/items/required/null 这些键也必须原样保留，否则改的是
// 客户端声明的取值本身（模型看到的默认值、常量都变了）。
func TestNormalizeSchemaKeepsDataKeywords(t *testing.T) {
	params := normalizeParamsJSON(t, `{"type":"object","properties":{
		"mode":{"type":"object","default":{"type":"str","items":["a"],"required":"x","note":null}},
		"tags":{"type":"array","items":{"type":"string"},"examples":[{"required":[null,""]}]},
		"kind":{"const":{"type":"int"}},
		"shape":{"enum":[{"type":"list","items":[1]}]}
	}}`)
	cases := map[string]struct{ key, want string }{
		"mode":  {"default", `{"type":"str","items":["a"],"required":"x","note":null}`},
		"tags":  {"examples", `[{"required":[null,""]}]`},
		"kind":  {"const", `{"type":"int"}`},
		"shape": {"enum", `[{"type":"list","items":[1]}]`},
	}
	for prop, c := range cases {
		got := propOf(t, params, prop)[c.key]
		if want := mustJSON(t, c.want); !reflect.DeepEqual(got, want) {
			t.Errorf("%s.%s 被改写: got %#v want %#v", prop, c.key, got, want)
		}
	}
}

// TestNormalizeSchemaDependencyContainers dependentRequired / dependencies 的键是属性名
// （数据），值是属性名数组或子 schema：键叫 items/type 也不能当 schema 关键字处理，
// 子 schema 照常规整。
func TestNormalizeSchemaDependencyContainers(t *testing.T) {
	params := normalizeParamsJSON(t, `{"type":"object",
		"properties":{"items":{"type":"string"},"type":{"type":"string"},"z":{"type":"string"}},
		"dependentRequired":{"items":["type"]},
		"dependencies":{"type":["items"],"z":{"type":"str"}}
	}`)
	if got, want := params["dependentRequired"], mustJSON(t, `{"items":["type"]}`); !reflect.DeepEqual(got, want) {
		t.Errorf("dependentRequired 被改写: got %#v want %#v", got, want)
	}
	deps, _ := params["dependencies"].(map[string]any)
	if got, want := deps["type"], mustJSON(t, `["items"]`); !reflect.DeepEqual(got, want) {
		t.Errorf("dependencies.type 被改写: got %#v want %#v", got, want)
	}
	if z, _ := deps["z"].(map[string]any); z["type"] != "string" {
		t.Errorf("dependencies 里的子 schema 没有规整: %#v", deps["z"])
	}
}

// TestNormalizeSchemaKeepsNullValues const:null / default:null 是有意义的取值（「只能是
// null」「缺省为 null」），不是缺失的元数据；description:null 这类元数据照旧剔除。
func TestNormalizeSchemaKeepsNullValues(t *testing.T) {
	params := normalizeParamsJSON(t, `{"type":"object","properties":{
		"x":{"const":null,"description":null},
		"y":{"type":["string","null"],"default":null,"title":null}
	}}`)
	x := propOf(t, params, "x")
	if v, ok := x["const"]; !ok || v != nil {
		t.Errorf("const:null 被删: %#v", x)
	}
	if _, ok := x["description"]; ok {
		t.Errorf("description:null 应剔除: %#v", x)
	}
	y := propOf(t, params, "y")
	if v, ok := y["default"]; !ok || v != nil {
		t.Errorf("default:null 被删: %#v", y)
	}
	if _, ok := y["title"]; ok {
		t.Errorf("title:null 应剔除: %#v", y)
	}
}

// TestNormalizeSchemaNumericKeywordSurvivesUseNumber 本仓主管线用 dec.UseNumber() 解码
// （为保住大整数原文），所以生产路径上的数字关键字是 json.Number 而不是 float64。
//
// 只认 float64 的实现会把**本来就合法**的 `minimum: 1` 判成非法值删掉：上游明明接受，
// 网关却把一条真约束静默抹掉，且用例（默认 Unmarshal 造夹具 → float64）永远绿。
// 数字形态必须原样保留（重序列化仍是原文数字）。
func TestNormalizeSchemaNumericKeywordSurvivesUseNumber(t *testing.T) {
	src := `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function",
		"function":{"name":"f","parameters":{"type":"object","properties":{
		"n":{"type":"integer","minimum":1,"maximum":9007199254740993},
		"s":{"type":"integer","minimum":"2"}}}}}]}`
	out := PrepareBodyOpt([]byte(src), false)
	if !containsAll(string(out), `"minimum":1`, `"maximum":9007199254740993`, `"minimum":2`) {
		t.Fatalf("数字关键字被改写或删除（UseNumber 路径）: %s", out)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	props := obj["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)["properties"].(map[string]any)
	if got, want := props["n"].(map[string]any)["minimum"], float64(1); got != want {
		t.Errorf("n.minimum = %v (%T), want 1", got, got)
	}
}

// containsAll 辅助：全部子串都出现。
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// TestNormalizeSchemaKeepsUnionType 联合类型 `type:["string","null"]` 是合法 schema
// （上游实测 200），深度规整不许把它改写成 object/string —— 那会把「可为 null」
// 静默改成「只能 string」，语义没了。只有 type **缺省**（或空串）且带 properties 的
// 节点才补 object。
func TestNormalizeSchemaKeepsUnionType(t *testing.T) {
	params := normalizeParamsJSON(t, `{"type":"object","properties":{
		"maybe":{"type":["string","null"]},
		"maybeObj":{"type":["object","null"],"properties":{"x":{"type":"string"}}},
		"noType":{"properties":{"y":{"type":"string"}}}
	}}`)
	if got, want := propOf(t, params, "maybe")["type"], mustJSON(t, `["string","null"]`); !reflect.DeepEqual(got, want) {
		t.Errorf("maybe.type 被改写: %#v want %#v", got, want)
	}
	mo := propOf(t, params, "maybeObj")
	if got, want := mo["type"], mustJSON(t, `["object","null"]`); !reflect.DeepEqual(got, want) {
		t.Errorf("maybeObj.type 被改写: %#v want %#v", got, want)
	}
	if got := propOf(t, params, "noType")["type"]; got != "object" {
		t.Errorf("缺省 type 但带 properties 应补 object, got %#v", got)
	}
}
