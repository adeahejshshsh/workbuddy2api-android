package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const kimiToolBody = `{"model":"kimi-k3.1-pro","messages":[{"role":"user","content":"test"}],"tools":[{"type":"function","function":{"name":"lookup","strict":true,"parameters":{"type":"object","$schema":"schema","$defs":{"Item":{"type":"string","enum":["a","b"]}},"properties":{"x":{"$ref":"#/$defs/Item"},"y":{"$ref":"#/$defs/Item"}}}}},{"type":"function","function":{"name":"mcp__rea__inspect","parameters":{"type":"object"}}},{"type":"function","function":{"name":"mcp__tencent_docs__read","parameters":{"type":"object"}}}],"metadata":{"keep":1234567890123456789}}`

func TestKimiToolsCompatibility(t *testing.T) {
	out := prepareKimiTools([]byte(kimiToolBody), "cn")
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	tools := got["tools"].([]any)
	if len(tools) != 3 {
		t.Fatalf("tools=%v", tools) // 只展开 $ref，不裁剪任何工具
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["strict"] != true {
		t.Fatal("lost function metadata")
	}
	params := fn["parameters"].(map[string]any)
	if _, ok := params["$defs"]; ok {
		t.Fatal("definitions remain")
	}
	if _, ok := params["$schema"]; ok {
		t.Fatal("schema marker remains")
	}
	props := params["properties"].(map[string]any)
	for _, key := range []string{"x", "y"} {
		v := props[key].(map[string]any)
		if v["type"] != "string" || len(v["enum"].([]any)) != 2 {
			t.Fatalf("ref %s=%v", key, v)
		}
	}
	if !strings.Contains(string(out), "1234567890123456789") {
		t.Fatal("unrelated number changed")
	}
}

func TestKimiToolsScopeAndForcedChoice(t *testing.T) {
	for _, tc := range []struct{ body, realm string }{
		{strings.ReplaceAll(kimiToolBody, "kimi-k3.1-pro", "deepseek-v4.1-flash"), "cn"},
		{kimiToolBody, "global"}, {`invalid`, "cn"}, {`{"model":"kimi","tools":[]}`, "cn"},
	} {
		if got := string(prepareKimiTools([]byte(tc.body), tc.realm)); got != tc.body {
			t.Fatalf("changed passthrough body: %s", got)
		}
	}
	body := strings.Replace(kimiToolBody, `"messages":`, `"tool_choice":{"type":"function","function":{"name":"mcp__rea__inspect"}},"messages":`, 1)
	body = strings.Replace(body, `"model":"kimi`, `"model":"cn:kimi`, 1)
	var got map[string]any
	_ = json.Unmarshal(prepareKimiTools([]byte(body), "cn"), &got)
	if len(got["tools"].([]any)) != 3 {
		t.Fatal("forced tool must remain declared") // 三个都该在：不裁剪
	}
}

func TestKimiSchemaRecursiveAndMissingRefs(t *testing.T) {
	defs := map[string]any{"Loop": map[string]any{"$ref": "#/$defs/Loop"}}
	for _, ref := range []string{"Loop", "Missing"} {
		out := inlineKimiSchema(map[string]any{"$ref": "#/$defs/" + ref}, defs, map[string]bool{}, 0)
		b, err := json.Marshal(out)
		if err != nil || strings.Contains(string(b), "$ref") {
			t.Fatalf("unresolved ref: %s %v", b, err)
		}
	}
}

func TestChatStreamKimiToolsOnWire(t *testing.T) {
	var outbound []byte
	c := testClient(func(r *http.Request) (*http.Response, error) {
		outbound, _ = io.ReadAll(r.Body)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: [DONE]\n\n"))}, nil
	})
	rc, status, _, err := c.ChatStream(&auth.Auth{AccessToken: "test", UID: "test"}, []byte(kimiToolBody), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("status=%d err=%v", status, err)
	}
	rc.Close()
	if strings.Contains(string(outbound), "$ref") || !strings.Contains(string(outbound), `"name":"lookup"`) ||
		!strings.Contains(string(outbound), "mcp__rea__inspect") || !strings.Contains(string(outbound), "mcp__tencent_docs__read") {
		t.Fatalf("bad wire body: %s", outbound)
	}
}

func TestKimiSchemaExpansionBudget(t *testing.T) {
	// A tiny input can expand exponentially when each definition references the
	// preceding one twice. Abort the rewrite instead of allocating that tree.
	defs := map[string]any{"Leaf": map[string]any{"type": "string"}}
	last := "Leaf"
	for i := 0; i < 20; i++ {
		name := last + "x"
		defs[name] = map[string]any{"allOf": []any{map[string]any{"$ref": "#/$defs/" + last}, map[string]any{"$ref": "#/$defs/" + last}}}
		last = name
	}
	body, err := json.Marshal(map[string]any{"model": "kimi-k3.1-pro", "tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "test", "parameters": map[string]any{"$defs": defs, "properties": map[string]any{"x": map[string]any{"$ref": "#/$defs/" + last}}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := prepareKimiTools(body, "cn"); string(got) != string(body) {
		t.Fatal("expansion budget should leave original request intact")
	}
}
