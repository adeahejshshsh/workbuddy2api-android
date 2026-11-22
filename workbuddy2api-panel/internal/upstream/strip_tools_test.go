package upstream

import (
	"encoding/json"
	"testing"
)

// TestStripToolDescriptionsRemovesOnlyDescription 11128 最后一层抢救：
// 只摘 description，name / parameters 与其它顶层字段必须原样保留。
func TestStripToolDescriptionsRemovesOnlyDescription(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}],` +
		`"tools":[` +
		`{"type":"function","function":{"name":"dshmarketplace_search","description":"Search the DeepSeek Harness plugin catalogue","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}},` +
		`{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}` +
		`]}`)
	out, ok := StripToolDescriptions(body)
	if !ok {
		t.Fatal("应当摘掉至少一个 description")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatalf("输出必须仍是合法 JSON: %v", err)
	}
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(top["tools"], &tools); err != nil {
		t.Fatalf("tools 结构被破坏: %v", err)
	}
	fnOf := func(i int) map[string]json.RawMessage {
		var fn map[string]json.RawMessage
		if err := json.Unmarshal(tools[i]["function"], &fn); err != nil {
			t.Fatalf("第 %d 个工具的 function 被破坏: %v", i, err)
		}
		return fn
	}
	if len(tools) != 2 {
		t.Fatalf("工具条数不能变：want 2 got %d", len(tools))
	}
	if _, has := fnOf(0)["description"]; has {
		t.Fatal("第一个工具的 description 必须被摘掉")
	}
	if string(fnOf(0)["name"]) != `"dshmarketplace_search"` {
		t.Fatalf("name 必须保留，got %s", fnOf(0)["name"])
	}
	if _, has := fnOf(0)["parameters"]; !has {
		t.Fatal("parameters 必须保留（摘描述不能动 schema）")
	}
	// 顶层其它字段逐字节不变
	if string(top["model"]) != `"deepseek-v4.1-flash"` {
		t.Fatalf("model 被改写：%s", top["model"])
	}
	if string(top["stream"]) != "true" {
		t.Fatalf("stream 被改写：%s", top["stream"])
	}
}

// TestStripToolDescriptionsNoop 没有可摘的东西时必须返回 false，调用方据此避免无意义重试。
func TestStripToolDescriptionsNoop(t *testing.T) {
	cases := map[string]string{
		"没有 tools":   `{"model":"m","messages":[]}`,
		"tools 为空":   `{"model":"m","tools":[]}`,
		"工具无描述":      `{"tools":[{"type":"function","function":{"name":"a"}}]}`,
		"tools 不是数组": `{"tools":{"x":1}}`,
		"空 body":     ``,
		"非法 JSON":    `{not json`,
	}
	for name, body := range cases {
		if _, ok := StripToolDescriptions([]byte(body)); ok {
			t.Errorf("%s：不应报告「摘到了东西」", name)
		}
	}
}

// TestStripToolDescriptionsKeepsOtherTools 混合场景：只有一个工具有描述时，
// 其余工具必须原样保留（不能被整体重写成空）。
func TestStripToolDescriptionsKeepsOtherTools(t *testing.T) {
	body := []byte(`{"tools":[{"type":"function","function":{"name":"a","description":"x"}},` +
		`{"type":"function","function":{"name":"b","parameters":{"type":"object"}}}]}`)
	out, ok := StripToolDescriptions(body)
	if !ok {
		t.Fatal("应当摘掉 a 的 description")
	}
	var top map[string]json.RawMessage
	_ = json.Unmarshal(out, &top)
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(top["tools"], &tools); err != nil {
		t.Fatalf("tools 解析失败: %v", err)
	}
	var fnB map[string]json.RawMessage
	if err := json.Unmarshal(tools[1]["function"], &fnB); err != nil {
		t.Fatalf("第二个工具解析失败: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("工具条数变化：%d", len(tools))
	}
	if string(fnB["name"]) != `"b"` {
		t.Fatalf("无描述的工具被改坏：%s", fnB["name"])
	}
}
