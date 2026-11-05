package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSanitizeCoversToolsDescription tools[].function.description 也要走脱敏。
//
// 脱敏此前只进 sanitizeMessages：messages[].content 被洗，而客户端自带的工具描述
// 原样出站。工具描述与 system prompt 同源、都是固定模板句，属于同一类黑名单高发区
// （上游 issue #395 的盲区；#308 有「非空 tools 触发 11-128」的先例，但 tools 内容
// 脱敏并未验证过是否真被拦——所以这里只钉「行为与 messages 一致」，不声称拦得住）。
func TestSanitizeCoversToolsDescription(t *testing.T) {
	// DSH @ 路径段的原文（sanitizeDshPathsRe 覆盖的那一段），放进工具描述里。
	fp := "Tokens prefixed with @ are paths the user explicitly referenced. " +
		"Relative paths are resolved against the workspace root; absolute paths point to " +
		"files or directories on the host. A trailing slash marks a directory, so list it " +
		"when its contents matter. Anything else is a file: open it with the read tool when " +
		"its contents are needed, and never claim to have inspected it before reading. " +
		"The @\"...\" form only quotes a path that contains spaces."
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function",` +
		`"function":{"name":"read_file","description":` + quote(fp) + `}}]}`

	out := PrepareBodyOpt([]byte(body), true)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("输出不是合法 JSON: %v (%s)", err, out)
	}
	fn := obj["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	desc := fn["description"].(string)
	if desc == fp {
		t.Fatalf("tools 描述未被净化，指纹原样出站")
	}
	if !strings.Contains(desc, "Paths beginning with @ refer to files") {
		t.Fatalf("应是同义改写而不是删除，got %q", desc)
	}

	// 反例：没有指纹的描述必须一字不动（避免为了覆盖 tools 把正常描述改写）。
	clean := `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function",` +
		`"function":{"name":"x","description":"Read a file from disk."}}]}`
	var obj2 map[string]any
	if err := json.Unmarshal(PrepareBodyOpt([]byte(clean), true), &obj2); err != nil {
		t.Fatalf("反例输出非法: %v", err)
	}
	if got := obj2["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["description"]; got != "Read a file from disk." {
		t.Fatalf("无指纹描述被误改: %q", got)
	}
}

// quote 把字符串包成 JSON 字面量（正文里有引号与反斜杠，手写容易错）。
func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
