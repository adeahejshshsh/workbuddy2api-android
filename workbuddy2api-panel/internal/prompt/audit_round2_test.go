package prompt

// 第二轮代码审计（CODE-AUDIT-FULL.md 10.17 / W2-C6-01~10）的回归用例。
//
// 每个用例都用注释钉住「审查编号 + 修之前会怎么错」，避免以后有人按直觉
// 把口径改回去（尤其 01 的转义/字段顺序与 04 的句界判定）。

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------- W2-C6-01 ----

// 审查 W2-C6-01：三个改写函数此前用 json.Marshal —— `<>&` 被写成 \u003c，
// 且顶层 map 键被字典序重排。透传型网关对上游是字节级改写，这两件事都会让
// 上游的原文/前缀匹配（prompt cache、指纹判定）与客户端实际请求不一致。
func TestRewriteNoHTMLEscapeAndKeepsFieldOrder(t *testing.T) {
	in := []byte(`{"zeta":1,"alpha":{"b":"x<y&z"},"messages":[` +
		`{"role":"system","content":"old"},` +
		`{"role":"user","content":"a<b>c&d"}]}`)
	out := Rewrite(in, "GW")
	s := string(out)

	// 1) 不转义：客户端原文必须逐字保留。
	if strings.Contains(s, `\u003c`) || strings.Contains(s, `\u003e`) || strings.Contains(s, `\u0026`) {
		t.Fatalf("HTML 转义未关闭: %s", s)
	}
	if !strings.Contains(s, `a<b>c&d`) {
		t.Fatalf("user 原文被改写: %s", s)
	}
	// 2) 未触碰的字段必须原文保留（连内部空白/转义形态都不动）。
	if !strings.Contains(s, `"alpha":{"b":"x<y&z"}`) {
		t.Fatalf("未触碰字段被重编: %s", s)
	}
	// 3) 顶层字段顺序保持原样（zeta 在 alpha 之前）。
	zi, ai := strings.Index(s, `"zeta"`), strings.Index(s, `"alpha"`)
	if zi < 0 || ai < 0 || zi > ai {
		t.Fatalf("顶层字段被重排: %s", s)
	}
	// 4) 语义仍然正确：头部一条 system = 网关提示词。
	if got := firstSystem(t, out); got != "GW" {
		t.Fatalf("头部 system=%q", got)
	}
}

// 审查 W2-C6-01 的另一半：Append / SplitSystemMessages 也必须走同一条序列化口径。
func TestAppendAndSplitNoHTMLEscape(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"a<b>c"}]}`)
	out := Append(body, "GW & <tag>")
	if !strings.Contains(string(out), `a<b>c`) || !strings.Contains(string(out), `GW & <tag>`) {
		t.Fatalf("Append 引入了 HTML 转义: %s", out)
	}

	splitBody := []byte(`{"messages":[` +
		`{"role":"system","content":"First one. Second one."},` +
		`{"role":"user","content":"keep <this> & that"}]}`)
	split, ok := SplitSystemMessages(splitBody)
	if !ok {
		t.Fatal("应能拆分")
	}
	if !strings.Contains(string(split), `keep <this> & that`) {
		t.Fatalf("拆分把 user 原文改写了: %s", split)
	}
}

// ---------------------------------------------------------------- W2-C6-02 ----

// 审查 W2-C6-02：MoveSystemToUser 此前用裸 json.Unmarshal，>2^53 的 seed/微秒
// 时间戳被落成 float64 后回写成近似值。这条请求级抢救路径反而静默改坏请求。
func TestMoveSystemToUserPreservesBigInts(t *testing.T) {
	body := []byte(`{"seed":9007199254740993,"metadata":{"ts_us":1792623687458123},` +
		`"messages":[` +
		`{"role":"system","content":"sys rules"},` +
		`{"role":"user","content":"hi","ts_us":1792623687458123}]}`)
	out, ok := MoveSystemToUser(body, "neutral")
	if !ok {
		t.Fatal("应搬运成功")
	}
	for _, want := range []string{"9007199254740993", "1792623687458123"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("大整数被改写（缺 %s）: %s", want, out)
		}
	}
	if strings.Contains(string(out), "9.007199254740992e+15") {
		t.Fatalf("出现科学计数法: %s", out)
	}
}

// 审查 W2-C6-02：SplitSystemMessages 同口径 —— 拆分不得动任何数字原文。
func TestSplitSystemMessagesPreservesBigInts(t *testing.T) {
	body := []byte(`{"seed":9007199254740993,"metadata":{"ts_us":1792623687458123},` +
		`"messages":[` +
		`{"role":"system","content":"First sentence here. Second sentence here."},` +
		`{"role":"user","content":"hi"}]}`)
	out, ok := SplitSystemMessages(body)
	if !ok {
		t.Fatal("应能拆分")
	}
	for _, want := range []string{"9007199254740993", "1792623687458123"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("大整数被改写（缺 %s）: %s", want, out)
		}
	}
}

// ---------------------------------------------------------------- W2-C6-03 ----

// 审查 W2-C6-03（角色保真）：拆出来的每条都沿用原 role，不再把 developer
// 静默重建成 system。
func TestSplitSystemMessagesKeepsDeveloperRole(t *testing.T) {
	body := []byte(`{"messages":[` +
		`{"role":"developer","content":"Rule one applies. Rule two applies. Rule three applies."},` +
		`{"role":"user","content":"hi"}]}`)
	out, ok := SplitSystemMessages(body)
	if !ok {
		t.Fatal("developer 多句内容应能拆分")
	}
	roles := systemRoles(t, out)
	if len(roles) != 4 { // 3 段 developer + user
		t.Fatalf("roles=%v", roles)
	}
	for i := 0; i < 3; i++ {
		if roles[i] != "developer" {
			t.Fatalf("第 %d 条角色被改写: %v", i, roles)
		}
	}
}

// 审查 W2-C6-03（段数上限）：此前 N 无上限，千句 system 会把 messages 膨胀
// 上千条（上游 400/413、计费与截断策略异常）。超限即放弃拆分。
func TestSplitSystemMessagesCapsPartCount(t *testing.T) {
	var b strings.Builder
	for i := 0; i < maxSplitParts+1; i++ {
		fmt.Fprintf(&b, "Sentence number %d here. ", i)
	}
	body, _ := json.Marshal(map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": b.String()},
			map[string]any{"role": "user", "content": "hi"},
		},
	})
	out, ok := SplitSystemMessages(body)
	if ok {
		t.Fatalf("段数超上限不该拆（会膨胀成 %d 条）", maxSplitParts+1)
	}
	if string(out) != string(body) {
		t.Fatal("放弃拆分时必须原样返回")
	}
}

// ---------------------------------------------------------------- W2-C6-04 ----

// 审查 W2-C6-04：只按 ". " 切会把代码块/编号列表/URL 从中间切断。
func TestSplitSentencesSkipsFenceListAndURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			// 围栏内的 "a. b" 不是句界（修之前会被切成 "fmt.Println(\"a." + "b\")"）。
			name: "围栏代码块",
			in:   "Use this. Example:\n```go\nfmt.Println(\"a. b\")\n```\nDone now.",
			want: []string{"Use this.", "Example:\n```go\nfmt.Println(\"a. b\")\n```\nDone now."},
		},
		{
			// 行首 "1." / "2." 是列表标记，不是句界（修之前 "1. First do this." 会被切碎）。
			name: "编号列表",
			in:   "Steps:\n1. First do this.\n2. Then that.",
			want: []string{"Steps:\n1. First do this.", "2. Then that."},
		},
		{
			// URL 后接空格不是句界（修之前会在 ".com. then" 处切断）。
			name: "URL",
			in:   "See https://example.com. then continue with the rest.",
			want: []string{"See https://example.com. then continue with the rest."},
		},
		{
			// 正常英文句子仍要切（不能因为加固而漏切）。
			name: "正常句界",
			in:   "First sentence here. Second sentence here.",
			want: []string{"First sentence here.", "Second sentence here."},
		},
	}
	for _, c := range cases {
		got := splitSentences(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("%s: got %q want %q", c.name, got, c.want)
		}
		for i := range c.want {
			if got[i] != c.want[i] {
				t.Fatalf("%s: [%d] got %q want %q", c.name, i, got[i], c.want[i])
			}
		}
	}
}

// 审查 W2-C6-04（端到端）：拆分后的每条拼回去必须等于原文（一个字符都不许丢）。
func TestSplitSystemMessagesKeepsFenceIntact(t *testing.T) {
	const sys = "Intro sentence. Code:\n```\nx := \"a. b\"\n```\nTail sentence."
	body, _ := json.Marshal(map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": sys},
			map[string]any{"role": "user", "content": "hi"},
		},
	})
	out, ok := SplitSystemMessages(body)
	if !ok {
		t.Fatal("应能拆分")
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	msgs := got["messages"].([]any)
	var parts []string
	for _, m := range msgs[:len(msgs)-1] {
		parts = append(parts, m.(map[string]any)["content"].(string))
	}
	// 围栏内的 "a. b" 必须完整留在同一段里。
	joined := strings.Join(parts, " ")
	if strings.Join(strings.Fields(joined), " ") != strings.Join(strings.Fields(sys), " ") {
		t.Fatalf("文本被改动:\n got=%q\nwant=%q", joined, sys)
	}
	if !strings.Contains(joined, `x := "a. b"`) {
		t.Fatalf("代码块被从中间切断: %q", joined)
	}
}

// ---------------------------------------------------------------- W2-C6-05 ----

// 审查 W2-C6-05：MoveSystemToUser 把 system 原文整段拼进首条 user，此前无长度
// 上限。超限即放弃搬运（截断比不搬更坏：人设/约定会被切掉一半）。
func TestMoveSystemToUserBailsOnOversizeSystem(t *testing.T) {
	big := strings.Repeat("规则。", maxMoveSystemBytes/3+16)
	body, _ := json.Marshal(map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": big},
			map[string]any{"role": "user", "content": "hi"},
		},
	})
	out, ok := MoveSystemToUser(body, "neutral")
	if ok {
		t.Fatal("超长 system 不该搬运")
	}
	if string(out) != string(body) {
		t.Fatal("放弃时必须原样返回")
	}
}

// ---------------------------------------------------------------- W2-C6-06 ----

// 审查 W2-C6-06：Load 此前直接 os.ReadFile，路径无大小限制、错误串回带完整
// 绝对路径。现在：普通文件 + 1 MiB 上限 + 错误只回 basename。
func TestLoadRejectsOversizeFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "huge.md")
	if err := os.WriteFile(fp, make([]byte, maxPromptFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load("custom", fp)
	if err == nil {
		t.Fatal("超限文件必须报错（fail fast）")
	}
	if !errors.Is(err, errPromptTooLarge) {
		t.Fatalf("错误应可 errors.Is 判定: %v", err)
	}
	if strings.Contains(err.Error(), dir) {
		t.Fatalf("错误串泄露了完整路径: %v", err)
	}
	if !strings.Contains(err.Error(), "huge.md") {
		t.Fatalf("错误串应保留文件名便于排障: %v", err)
	}
}

// 审查 W2-C6-06：非普通文件（目录/FIFO/设备）一律拒绝 —— 此前 os.ReadFile 读
// /dev/zero 会把进程内存打满。
func TestLoadRejectsNonRegularFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load("custom", dir); err == nil {
		t.Fatal("目录必须报错")
	} else if !errors.Is(err, errPromptNotRegular) {
		t.Fatalf("错误应可 errors.Is 判定: %v", err)
	}
}

// 上限以内的文件照旧读出来（不要因为加固把正常路径弄坏）。
func TestLoadAcceptsFileAtLimit(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "ok.md")
	want := strings.Repeat("a", maxPromptFileBytes)
	if err := os.WriteFile(fp, []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load("custom", fp)
	if err != nil {
		t.Fatalf("上限内的文件应可读: %v", err)
	}
	if got != want {
		t.Fatalf("内容不一致: len=%d want=%d", len(got), len(want))
	}
}

// ---------------------------------------------------------------- W2-C6-07 ----

// 审查 W2-C6-07：Rewrite 此前删除**全部** system/developer（含中途出现的），
// 只保留网关一条。现在只替换开头连续块，中途 system 保留。
func TestRewriteKeepsMidConversationSystem(t *testing.T) {
	body := []byte(`{"messages":[` +
		`{"role":"system","content":"old head"},` +
		`{"role":"user","content":"hi"},` +
		`{"role":"system","content":"mid rules"}]}`)
	out := Rewrite(body, "GW")
	roles := systemRoles(t, out)
	want := []string{"system", "user", "system"}
	if len(roles) != len(want) {
		t.Fatalf("roles=%v want %v", roles, want)
	}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("roles=%v want %v", roles, want)
		}
	}
	if !strings.Contains(string(out), "mid rules") {
		t.Fatalf("中途 system 被删了: %s", out)
	}
	if strings.Contains(string(out), "old head") {
		t.Fatalf("开头 system 应被替换: %s", out)
	}
}

// ---------------------------------------------------------------- W2-C6-08 ----

// 审查 W2-C6-08：Append 在已有 system 之后仍插入网关 system → 请求携带多条
// system（仅接受单条 system 的上游直接 400）。现在合并进首条。
func TestAppendMergesIntoExistingSystem(t *testing.T) {
	body := []byte(`{"messages":[` +
		`{"role":"system","content":"client rules"},` +
		`{"role":"user","content":"hi"}]}`)
	out := Append(body, "GW")
	roles := systemRoles(t, out)
	var systemCount int
	for _, r := range roles {
		if r == "system" {
			systemCount++
		}
	}
	if systemCount != 1 {
		t.Fatalf("want 1 条 system, got %d: %v", systemCount, roles)
	}
	if got := firstSystem(t, out); got != "client rules\n\nGW" {
		t.Fatalf("合并结果=%q（原文须在前、网关提示词在后，一个字符都不丢）", got)
	}
}

// 没有既有 system 时仍然插入（既有行为，别被 08 的改动带坏）。
func TestAppendPrependsWhenNoSystem(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	out := Append(body, "GW")
	roles := systemRoles(t, out)
	if len(roles) != 2 || roles[0] != "system" || roles[1] != "user" {
		t.Fatalf("roles=%v", roles)
	}
	if got := firstSystem(t, out); got != "GW" {
		t.Fatalf("头部 system=%q", got)
	}
}

// 首条 system 是多模态 parts 时，把网关提示词作为文本块追加进去（仍只有一条 system）。
func TestAppendMergesIntoMultimodalSystem(t *testing.T) {
	body := []byte(`{"messages":[` +
		`{"role":"system","content":[{"type":"text","text":"client"}]},` +
		`{"role":"user","content":"hi"}]}`)
	out := Append(body, "GW")
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	msgs := obj["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("不该新增消息: %v", msgs)
	}
	parts := msgs[0].(map[string]any)["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("parts=%v", parts)
	}
	if p := parts[1].(map[string]any); p["type"] != "text" || p["text"] != "GW" {
		t.Fatalf("追加的 part=%v", p)
	}
}

// ---------------------------------------------------------------- W2-C6-10 ----

// 审查 W2-C6-10：多模态分支此前只处理首个 part 即 break —— 首块是 image 时，
// 后面文本块里的身份句永远改不到。现在逐个 text 块尝试，命中即停。
func TestOverrideHarnessIdentityScansAllTextParts(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":[` +
		`{"type":"image_url","image_url":{"url":"data:..."}},` +
		`{"type":"text","text":"You are Old Harness. tail"}]}]}`)
	out := OverrideHarnessIdentity(body, "You are CodeBuddy Code.")
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	msgs := obj["messages"].([]any)
	parts := msgs[0].(map[string]any)["content"].([]any)
	if got := parts[1].(map[string]any)["text"]; got != "You are CodeBuddy Code. tail" {
		t.Fatalf("非首块的身份句没改到: %v", got)
	}
}
