package panel

// 首页刷新异常不再静默（上游 2026-10-26 `35d5be8`）。
//
// 上游 issue #241 的排查过程说明：真正的坑不是那个未声明变量本身，而是渲染异常在
// 5 秒轮询（quiet=true）时被完全吞掉 —— 现象是「统计正常、表格空白、控制台干净」，
// 只能靠反推。这条用例把「轮询路径也必须留痕」钉住。

import (
	"os"
	"strings"
	"testing"
)

func TestAppJSLoadOverviewNeverSwallowsError(t *testing.T) {
	nodeBin(t)
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('async function loadOverview(');
const end = src.indexOf("$('accBody').addEventListener(");
if (start < 0 || end < 0) throw new Error('loadOverview not found');

const errLog = [], toasts = [];
let failRender = true;
const ctx = {
  Math, Number, Promise,
  console: { error: (...a) => errLog.push(a.join(' ')), log() {}, warn() {} },
  toast: (...a) => toasts.push(a.join(' ')),
  api: async () => ({ total: 1, healthy: 1, cooling: 0, disabled: 0, paused: 0,
    accounts: [], credits: 0, credits_total: 0, sticky_sessions: 0, version: '1.2.3',
    redis_mode: 'memory', uptime_sec: 60, in_flight_full: 0 }),
  $: id => (id === 'expBox' ? null : {}),
  renderAccounts: () => { if (failRender) throw new Error('boom: 渲染炸了'); },
  renderModelLocks: () => {},
  renderExpirySnapshot: () => {},
  lastPackages: null,
  overviewData: null,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.loadOverview=loadOverview;', ctx);

(async () => {
  // ① 轮询（quiet）路径：渲染抛错 → 必须 console.error 留痕，但不打扰用户。
  await ctx.loadOverview(true);
  const quiet = { errs: errLog.length, toasts: toasts.length };

  // ② 手动刷新（quiet=false）：留痕 + toast。
  errLog.length = 0; toasts.length = 0;
  await ctx.loadOverview(false);
  const loud = { errs: errLog.length, toasts: toasts.length };

  // ③ 正常路径不误报，且返回响应体（报告 #1 的契约）。
  failRender = false; errLog.length = 0; toasts.length = 0;
  const got = await ctx.loadOverview(true);
  const ok = { errs: errLog.length, toasts: toasts.length, version: got && got.version };

  process.stdout.write(JSON.stringify({ quiet, loud, ok }));
})();`
	f, err := os.CreateTemp(t.TempDir(), "overview-err-*.cjs")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()
	out, err := nodeCmd(f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("overview 静默吞异常 node 用例失败: %v\n%s", err, out)
	}
	const want = `{"quiet":{"errs":1,"toasts":0},"loud":{"errs":1,"toasts":1},"ok":{"errs":0,"toasts":0,"version":"1.2.3"}}`
	if got := strings.TrimSpace(string(out)); got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}
