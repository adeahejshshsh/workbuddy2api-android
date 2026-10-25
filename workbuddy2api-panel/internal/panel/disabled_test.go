package panel

// 禁用状态的分类标签与时间维度（上游 2026-10-24 `4b489a4`）。
//
// 为什么要分类：四种禁用成因的处置方式完全不同 —— 上游封禁要申诉（revive 无效）、
// 登录失效要重登、鉴权失败要查账号类型、手动禁用随时可恢复。只给一行原始 reason，
// 运维得先读懂才敢动手。

import (
	"os"
	"strings"
	"testing"
)

func TestAppJSDisabledKindAndAge(t *testing.T) {
	nodeBin(t)
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function disabledKind(');
const end = src.indexOf('function renderAccounts(');
if (start < 0 || end < 0) throw new Error('disabled helpers not found');
const ctx = { Date, Number, String, Math, isFinite };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.disabledKind=disabledKind; this.disabledAge=disabledAge;', ctx);

const kind = r => ctx.disabledKind(r);
const now = Date.now();
const iso = ms => new Date(now - ms).toISOString();
process.stdout.write(JSON.stringify({
  // 12153 / 网关 401：登录失效（需重登，解冻无效）
  session12153: kind('上游 12153 session dead（业务码明说登录态失效，需重新登录）'),
  session401:   kind('网关 401 × 3 且 refresh 验证失败（token 已确认失效）'),
  // 11140：上游封禁（需申诉）
  banned:       kind('11140 request illegal'),
  // 手动：随时可解冻
  manual:       kind('面板手动禁用（管理员操作）'),
  // 未知：兜底
  unknown:      kind(''),
  ages: [
    ctx.disabledAge(iso(30 * 1000)),
    ctx.disabledAge(iso(5 * 60 * 1000)),
    ctx.disabledAge(iso(3 * 3600 * 1000)),
    ctx.disabledAge(iso(2 * 86400 * 1000)),
    ctx.disabledAge(''),
    ctx.disabledAge('not-a-date'),
  ],
}));`
	f, err := os.CreateTemp(t.TempDir(), "disabled-kind-*.cjs")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()
	out, err := nodeCmd(f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("disabled-kind node test failed: %v\n%s", err, out)
	}
	const want = `{"session12153":{"label":"登录失效","tone":"warn","advice":"需重新登录，解冻无效"},"session401":{"label":"登录失效","tone":"warn","advice":"需重新登录，解冻无效"},"banned":{"label":"上游封禁","tone":"bad","advice":"需在官方申诉，解冻无效"},"manual":{"label":"手动禁用","tone":"mute","advice":"随时可解冻"},"unknown":{"label":"已禁用","tone":"mute","advice":"见下方原因"},"ages":["刚刚","5 分钟前","3 小时前","2 天前","",""]}`
	if got := strings.TrimSpace(string(out)); got != want {
		t.Fatalf("disabledKind/disabledAge=%s\nwant %s", got, want)
	}
}
