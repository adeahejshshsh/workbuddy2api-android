package panel

// 「积分与 token 按小时」表（上游 2026-10-27 `e3ec2f4` 的落点）。
//
// 这张表原先只有静态表头、没有任何渲染代码 —— 用户永远看到一张空表。
// 这里钉住四条：最新时段在最上、时段标签带日期、无积分样本留「—」不拿 0 顶替、
// 空数据给专门空态（而不是一片空白）。

import (
	"os"
	"strings"
	"testing"
)

func TestAppJSRenderUsageHours(t *testing.T) {
	nodeBin(t)
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function renderUsageHours(');
const end = src.indexOf('// 维度切换 / 排序控件。');
if (start < 0 || end < 0) throw new Error('renderUsageHours not found');

const el = { innerHTML: '' };
const ctx = {
  String, Number, Date, isNaN,
  // 与 app.js 同口径的最小桩：这两个函数在切片之外，行为另有测试覆盖。
  parsePointTime: p => { const s = p.t.length === 13 ? p.t + ':00:00' : p.t + 'T00:00:00'; const d = new Date(s); return isNaN(d.getTime()) ? null : d.getTime(); },
  fmtTok: n => String(Number(n || 0)),
  fmtCredit: n => String(Number(n || 0)),
  esc: s => String(s),
  $: id => (id === 'usHourBody' ? el : null),
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderUsageHours=renderUsageHours;', ctx);

// ① 混合窗口：小时点 + 日点，乱序输入 → 输出按时间降序。
ctx.renderUsageHours([
  { t: '2026-10-01', scope: 'day', requests: 3, total_tokens: 300, credits: 1 },
  { t: '2026-10-03T10', scope: 'hour', requests: 1, prompt_tokens: 10, completion_tokens: 20, total_tokens: 30, credits: 0.5 },
  { t: '2026-10-03T12', scope: 'hour', requests: 2, prompt_tokens: 40, completion_tokens: 60, total_tokens: 100, credits: 0 },
]);
const mixed = el.innerHTML;

// ② 空数据 → 专门空态。
ctx.renderUsageHours([]);
const empty = el.innerHTML;

// ③ 脏时间戳被跳过（不该渲染成 NaN 行）。
ctx.renderUsageHours([{ t: 'not-a-time', scope: 'hour', requests: 9 }]);
const dirty = el.innerHTML;

const i12 = mixed.indexOf('10-03 12:00'), i10 = mixed.indexOf('10-03 10:00'), iDay = mixed.indexOf('10-01');
process.stdout.write(JSON.stringify({
  // 12 点那行必须在 10 点那行之前（最新在最上），日点在最后。
  ordered: i12 !== -1 && i10 !== -1 && iDay !== -1 && i12 < i10 && i10 < iDay,
  // 无积分样本（credits=0）→ 积分列与 token/积分 列都是「—」
  dashCount: (mixed.match(/—/g) || []).length,
  emptyHasHint: empty.indexOf('暂无数据') !== -1,
  emptyColspan: empty.indexOf('colspan="8"') !== -1,
  dirtyNoNaN: dirty.indexOf('NaN') === -1,
}));`
	f, err := os.CreateTemp(t.TempDir(), "usage-hours-*.cjs")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()
	out, err := nodeCmd(f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("按小时表 node 用例失败: %v\n%s", err, out)
	}
	const want = `{"ordered":true,"dashCount":2,"emptyHasHint":true,"emptyColspan":true,"dirtyNoNaN":true}`
	if got := strings.TrimSpace(string(out)); got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}
