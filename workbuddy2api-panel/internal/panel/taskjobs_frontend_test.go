package panel

// taskjobs_frontend_test.go 账号任务弹窗的定时刷新（上游 38dca39 的后半段）。
//
// 上游同名文件测的是「作业轮询 + 任务静默刷新」两个定时器；本仓 v1.23.0 下线了任务
// 中心与作业列表，弹窗里只剩任务表本身，所以这里改成钉住本仓实际存在的两条行为：
//   - 弹窗打开即起 30s 静默刷新，标签页在后台时不发请求，关窗即停；
//   - 静默刷新（loadTasks(true)）不闪「查询中」、失败保留上一份列表；手动刷新照旧。

import (
	"os"
	"path/filepath"
	"testing"
)

func runDialogNode(t *testing.T, name, script string) {
	t.Helper()
	nodeBin(t) // node 不可用时跳过；必要时配置 linker64 代跑
	path := filepath.Join(t.TempDir(), name+".cjs")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := nodeCmd(path, "app.js")
	cmd.Dir = "." // 测试工作目录 = internal/panel
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node test failed: %v\n%s", err, out)
	}
}

// 弹窗开着期间 30s 一轮静默刷新；标签页在后台不刷（弹窗忘了关时不该继续打上游），
// 关窗必须把定时器清掉。
func TestAppJSTaskDialogRefreshSkipsHiddenTab(t *testing.T) {
	runDialogNode(t, "taskdialog-hidden", `const fs = require('fs'), vm = require('vm'), assert = require('assert');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('let taskUID = null'), end = src.indexOf('async function loadTasks(');
assert(start > 0 && end > start);
const nodes = new Map(), timers = new Map(), loads = [];
let tick = 0;
const doc = {hidden: false};
const node_ = id => ({textContent: '', hidden: false, innerHTML: '', className: '', value: '',
  classList: {add(){}, remove(){}}, addEventListener(){}});
const ctx = {
  console, Promise, Object, Array, String, Number, Map, encodeURIComponent, document: doc,
  $: id => { if (!nodes.has(id)) nodes.set(id, node_(id)); return nodes.get(id); },
  api: () => Promise.resolve({job: null}), toast(){}, esc: String, confirm: () => true,
  openVeil(){}, restoreFocus(){}, loadOverview(){},
  loadTasks: silent => loads.push(!!silent),
  setInterval: (fn, ms) => { const id = ++tick; timers.set(id, {fn, ms}); return id; },
  clearInterval: id => timers.delete(id),
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end), ctx);
(async () => {
  vm.runInContext("openTasks('cn-a')", ctx);
  assert.strictEqual(timers.size, 1, '开窗应起 30s 静默刷新');
  const timer = [...timers.values()][0];
  assert.strictEqual(timer.ms, 30000);
  loads.length = 0;
  timer.fn(); assert.deepStrictEqual(loads, [true], '前台应静默刷新');
  doc.hidden = true; loads.length = 0;
  timer.fn(); assert.deepStrictEqual(loads, [], '后台标签页不该刷新');
  doc.hidden = false;
  vm.runInContext('closeTasks()', ctx);
  assert.strictEqual(timers.size, 0, '关窗即停');
  console.log('dialog refresh OK');
})().catch(err => { console.error(err); process.exitCode = 1; });`)
}

// 静默刷新：不闪「查询中」、失败保留上一份列表；手动刷新仍是可见的那一套。
func TestAppJSTaskSilentRefreshKeepsList(t *testing.T) {
	runDialogNode(t, "taskdialog-silent", `const fs = require('fs'), vm = require('vm'), assert = require('assert');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('async function loadTasks(silent) {'), end = src.indexOf("$('taskBody').addEventListener('click'");
assert(start > 0 && end > start);
const nodes = new Map();
let apiFail = true;
const node_ = id => ({textContent: '', hidden: false, innerHTML: '', className: '', value: '',
  classList: {add(){}, remove(){}}, addEventListener(){}});
const ctx = {
  console, Promise, Object, Array, String, Number, Map, encodeURIComponent,
  document: {hidden: false},
  taskUID: '', tasksSeq: 0, taskGen: 0,
  $: id => { if (!nodes.has(id)) nodes.set(id, node_(id)); return nodes.get(id); },
  api: () => apiFail ? Promise.reject(new Error('boom')) : Promise.resolve({tasks: []}),
  esc: s => String(s == null ? '' : s), autoTaskLabel: () => '', toast(){}, loadOverview(){},
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end), ctx);
const flush = async () => { for (let i = 0; i < 10; i++) await Promise.resolve(); };
(async () => {
  // 这一段只切出了函数体，没有顶层 $() 调用：节点要用 ctx.$ 现取。
  ctx.$('taskTable').hidden = false;
  ctx.$('taskBody').innerHTML = '<tr><td>旧行</td></tr>';
  vm.runInContext("taskUID = 'cn-a'", ctx);

  vm.runInContext('loadTasks(true)', ctx); await flush();
  assert.strictEqual(ctx.$('taskBody').innerHTML, '<tr><td>旧行</td></tr>', '静默失败保留上一份列表');
  assert(!ctx.$('taskState').innerHTML.includes('查询中'), '静默刷新不闪查询中');
  assert.strictEqual(ctx.$('taskTable').hidden, false);

  vm.runInContext('loadTasks(false)', ctx); // 不 await：先看同步的「查询中」
  assert(ctx.$('taskState').innerHTML.includes('查询中'), '手动刷新仍要显示查询中');
  await flush();
  assert.strictEqual(ctx.$('taskState').className, 'state err');
  assert(ctx.$('taskState').textContent.includes('boom'), ctx.$('taskState').textContent);
  console.log('silent refresh OK');
})().catch(err => { console.error(err); process.exitCode = 1; });`)
}
