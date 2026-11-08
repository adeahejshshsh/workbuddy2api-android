package panel

// 请求记录「模型」列的降级呈现：请求 A、实际用 B 成功时显示 `A ⤳ B`，
// 未降级时保持单值（不能给每条记录都挂一个箭头，否则噪声盖过信号）。

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestAppJSRequestLogModelCell(t *testing.T) {
	nodeBin(t)
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const escStart = src.indexOf('function esc(');
const escEnd = src.indexOf('function ago(');
const start = src.indexOf('function reqModelCell');
const end = src.indexOf('function reqMatch');
if ([escStart, escEnd, start, end].some(v => v < 0)) throw new Error('helpers not found');
const ctx = { String, Number };
vm.createContext(ctx);
vm.runInContext(src.slice(escStart, escEnd) + src.slice(start, end) + '\nthis.reqModelCell=reqModelCell;', ctx);
process.stdout.write(JSON.stringify({
  plain: ctx.reqModelCell({ model: 'glm-5.3' }),
  same: ctx.reqModelCell({ model: 'glm-5.3', model_eff: 'glm-5.3' }),
  fallback: ctx.reqModelCell({ model: 'deepseek-v4.1-flash', model_eff: 'deepseek-v4-flash' }),
}));`
	f, err := os.CreateTemp(t.TempDir(), "model-cell-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := nodeCmd(f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got struct {
		Plain    string `json:"plain"`
		Same     string `json:"same"`
		Fallback string `json:"fallback"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode %s: %v", out, err)
	}
	if got.Plain != "glm-5.3" {
		t.Fatalf("未降级应是纯模型名，got %q", got.Plain)
	}
	if got.Same != "glm-5.3" {
		t.Fatalf("model_eff 与 model 相同时不该挂箭头，got %q", got.Same)
	}
	if !strings.Contains(got.Fallback, "⤳") || !strings.Contains(got.Fallback, "deepseek-v4-flash") {
		t.Fatalf("降级行应含 ⤳ 与实际模型，got %q", got.Fallback)
	}
}
