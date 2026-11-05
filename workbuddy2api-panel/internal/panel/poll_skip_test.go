package panel

import (
	"os"
	"strings"
	"testing"
)

// TestAppJSPollSkipsUnchangedRender 轮询载荷没变时不重建账号表。
//
// 面板每 5 秒轮询 overview，此前无条件 renderAccounts：重建 innerHTML 会打断行内
// 焦点与悬停（29 个号就是每 5 秒抖一次）。借鉴 workbuddy2api-hub 的「轮询指纹跳过」。
// 断言打在两处可观测契约上：同载荷连轮两次只渲染一次，载荷变了必须再渲染。
func TestAppJSPollSkipsUnchangedRender(t *testing.T) {
	// 用本仓的 nodeBin/nodeCmd（自带 linker64 代跑兜底）：直接 exec.Command("node", …)
	// 在 app 私有目录里会 permission denied。
	nodeBin(t)
	script := `const fs = require('fs'), vm = require('vm'), assert = require('assert');
const src = fs.readFileSync(process.argv[2], 'utf8');
const s = src.indexOf("let overviewSig = ''");
const e = src.indexOf("$('accBody').addEventListener", s);
assert.ok(s >= 0 && e > s, 'overviewSig slice anchors not found: s=' + s + ' e=' + e);
assert.ok(src.slice(s, e).includes('async function loadOverview('), 'slice missed loadOverview');

const dom = new Proxy(function () {}, { get: (t, k) => (k === Symbol.toPrimitive ? () => '' : dom), set: () => true, apply: () => dom });
const store = { payload: null };
const calls = { accounts: 0, locks: 0 };
const ctx = {
  $: () => dom, document: dom, window: dom, localStorage: dom, location: dom,
  String, Number, Math, Date, Object, Array, Set, JSON, RegExp, console, encodeURIComponent,
  api: async () => store.payload,
  esc: x => String(x == null ? '' : x),
  setHTML: () => {}, toast: () => {},
  renderUpdateStatus: () => {}, renderExpirySnapshot: () => {},
  renderAccounts: () => { calls.accounts++; },
  renderModelLocks: () => { calls.locks++; },
  startPolling: () => {},
  pollAutoRefresh: true, pollIntervalMs: 5000, updateData: null,
};
vm.createContext(ctx);
vm.runInContext(src.slice(s, e), ctx);

(async () => {
  const base = { total: 1, healthy: 1, cooling: 0, disabled: 0, paused: 0, sticky_sessions: 0,
    in_flight_full: 0, uptime_sec: 10, redis_mode: 'memory', version: 't',
    accounts: [{ uid: 'u1', credits: 5 }], model_locks: [] };
  store.payload = base;
  await ctx.loadOverview(true);
  await ctx.loadOverview(true);
  assert.strictEqual(calls.accounts, 1, '同载荷重复轮询不该重建账号表，实际 ' + calls.accounts + ' 次');
  store.payload = Object.assign({}, base, { accounts: [{ uid: 'u1', credits: 6 }] });
  await ctx.loadOverview(true);
  assert.strictEqual(calls.accounts, 2, '载荷变化后必须重渲染');
  console.log(JSON.stringify({ accounts: calls.accounts, locks: calls.locks }));
})().catch(err => { console.error(err); process.exit(1); });`

	f, err := os.CreateTemp(t.TempDir(), "pollskip-*.cjs")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatalf("write script: %v", err)
	}
	f.Close()

	out, err := nodeCmd(f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("node failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), `"accounts":2`) {
		t.Fatalf("unexpected output: %s", out)
	}
}
