package panel

// tasklive_test.go 钉住「实时任务」端点的行为，都是「看板会不会变成压测 / 一个号
// 挂了会不会整块空掉 / 页面卡不卡」的直接判据：
//   - TTL 内复用缓存，fresh=1 有 15 秒下限（连点刷新不该每下都打一轮上游）；
//   - 单账号失败只标在该行，且每个账号每轮**恰好一次**请求（失败不换口径重试）；
//   - 禁用号不出现在结果里、也不打上游；
//   - 国际号只走默认口径（不带小程序头），国内号走小程序口径；
//   - 一轮比请求愿意等的久时，请求带 refreshing=true 返回上一份快照，不叠轮；
//   - 全部领完的号只带计数、不带任务表。
//
// 前端用例（node 沙箱）在文件末尾：进入页面只启动一次、间隔档位、行渲染、数据
// 没变不重画、refreshing 续约。本仓没有 node 时整组跳过（与其它前端用例一致）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// liveFake 实时任务测试的假上游：按 X-User-Id 区分账号，记录每次请求的口径头。
type liveFake struct {
	calls   atomic.Int32
	mu      sync.Mutex
	mpCalls map[string]string // uid -> X-Client-Platform
	failUID string
	doneUID string        // 这个号的任务全部已领
	block   chan struct{} // 非 nil 时请求阻塞到它被关闭
}

func (f *liveFake) handler(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(r.URL.Path, "/tasks") {
		http.NotFound(w, r)
		return
	}
	f.calls.Add(1)
	uid := r.Header.Get("X-User-Id")
	f.mu.Lock()
	if f.mpCalls == nil {
		f.mpCalls = map[string]string{}
	}
	f.mpCalls[uid] = r.Header.Get("X-Client-Platform")
	block := f.block
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	if uid == f.failUID {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":401,"msg":"token expired"}`))
		return
	}
	pending := `{"task_code":"chat_5","title":"发起 5 次对话","target_count":5,"progress":{"current":2,"target":5},
			 "reward_credit":100,"accept_status":"accepted"},`
	if uid == f.doneUID {
		pending = ""
	}
	_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[` + pending + `
			{"task_code":"first_buddy","title":"领取一只 Buddy","target_count":1,"progress":{"current":1,"target":1},
			 "reward_credit":300,"accept_status":"claimed"}]}}`))
}

// newLivePanel 起假上游和一个带 cn-1 / cn-2 两个国内号的面板（global 域也指向假上游）。
func newLivePanel(t *testing.T, f *liveFake, extra ...*auth.Auth) *Panel {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)

	up := upstream.New()
	up.HTTP, up.ChatBaseCN, up.ChatBaseGlobal = srv.Client(), srv.URL, srv.URL
	p := pool.New("")
	p.Add(testRealmAuth(t, "cn-1", "cn"))
	p.Add(testRealmAuth(t, "cn-2", "cn"))
	for _, a := range extra {
		p.Add(a)
	}
	return New(Config{Pool: p, Upstream: up})
}

func getLiveJSON(t *testing.T, p *Panel, url string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", url, rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func liveRowOf(out map[string]any, uid string) map[string]any {
	accts, _ := out["accounts"].([]any)
	for _, a := range accts {
		if m, ok := a.(map[string]any); ok && m["uid"] == uid {
			return m
		}
	}
	return nil
}

func TestTasksLiveCountsAndCache(t *testing.T) {
	f := &liveFake{}
	p := newLivePanel(t, f)

	out := getLiveJSON(t, p, "/panel/api/tasks/live")
	if out["cached"] != false {
		t.Fatalf("首轮不该报缓存：%+v", out)
	}
	accts, _ := out["accounts"].([]any)
	if len(accts) != 2 {
		t.Fatalf("accounts=%d want 2", len(accts))
	}
	first := liveRowOf(out, "cn-1")
	if first["pending"].(float64) != 1 || first["claimed"].(float64) != 1 {
		t.Fatalf("计数不对: %+v", first)
	}
	if ts, _ := first["tasks"].([]any); len(ts) != 2 {
		t.Fatalf("还有待办的号应带完整任务表: %+v", first)
	}
	n1 := f.calls.Load()
	if n1 != 2 { // 两个账号各一次
		t.Fatalf("首轮上游调用=%d want 2", n1)
	}

	// 第二轮：TTL 内应直接复用，不再打上游
	out2 := getLiveJSON(t, p, "/panel/api/tasks/live")
	if out2["cached"] != true {
		t.Fatalf("TTL 内应命中缓存: %+v", out2)
	}
	if f.calls.Load() != n1 {
		t.Fatalf("缓存命中仍打了上游：%d -> %d", n1, f.calls.Load())
	}
}

// fresh=1 有下限：上一轮刚结束时连点「立即刷新」（或脚本反复带 fresh）仍回缓存，
// 否则每次都是每个启用账号一次上游 GET，把看板变成压测。过了下限才穿透。
func TestTasksLiveFreshHasFloor(t *testing.T) {
	f := &liveFake{}
	p := newLivePanel(t, f)
	getLiveJSON(t, p, "/panel/api/tasks/live")
	for i := 0; i < 3; i++ {
		if out := getLiveJSON(t, p, "/panel/api/tasks/live?fresh=1"); out["cached"] != true {
			t.Fatalf("下限内的 fresh=1 应回缓存: %+v", out)
		}
	}
	if n := f.calls.Load(); n != 2 {
		t.Fatalf("下限内的 fresh=1 不该打上游：calls=%d want 2", n)
	}

	old := taskLiveFreshFloor
	taskLiveFreshFloor = time.Nanosecond
	t.Cleanup(func() { taskLiveFreshFloor = old })
	time.Sleep(2 * time.Millisecond)
	if out := getLiveJSON(t, p, "/panel/api/tasks/live?fresh=1"); out["cached"] != false {
		t.Fatalf("过了下限 fresh=1 应穿透: %+v", out)
	}
	if n := f.calls.Load(); n != 4 {
		t.Fatalf("calls=%d want 4", n)
	}
}

// 失败号每轮只请求一次：上游 401（token 失效）时再换默认口径重试，在封号潮里一批号
// 同时失效就会把一轮的请求数翻倍，而且换口径不会让 401 变成功。
func TestTasksLivePerAccountFailureIsolated(t *testing.T) {
	f := &liveFake{failUID: "cn-2"}
	p := newLivePanel(t, f)

	out := getLiveJSON(t, p, "/panel/api/tasks/live")
	accts, _ := out["accounts"].([]any)
	if len(accts) != 2 {
		t.Fatalf("一个号失败不该让整块空掉：accounts=%d", len(accts))
	}
	okRow, badRow := liveRowOf(out, "cn-1"), liveRowOf(out, "cn-2")
	if okRow == nil || okRow["error"] != nil {
		t.Fatalf("健康号不该有 error: %+v", okRow)
	}
	if badRow == nil || badRow["error"] == nil || badRow["error"] == "" {
		t.Fatalf("失败号应带 error: %+v", badRow)
	}
	if ts, ok := badRow["tasks"].([]any); ok && len(ts) != 0 {
		t.Fatalf("失败号不该有任务: %+v", badRow)
	}
	if n := f.calls.Load(); n != 2 {
		t.Fatalf("每个号每轮恰好一次请求：calls=%d want 2", n)
	}
}

func TestTasksLiveSkipsDisabledAccounts(t *testing.T) {
	f := &liveFake{}
	p := newLivePanel(t, f)
	p.cfg.Pool.Disable("cn-1", "test")

	out := getLiveJSON(t, p, "/panel/api/tasks/live")
	accts, _ := out["accounts"].([]any)
	if len(accts) != 1 || liveRowOf(out, "cn-1") != nil {
		t.Fatalf("禁用号不该出现在实时任务里：%+v", accts)
	}
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("禁用号不该打上游：calls=%d want 1", n)
	}
}

// 国际号只走默认口径，不带小程序头：该头对国际账号没有验证过的契约（单号任务弹窗
// 同样明确不探 MP）。国内号仍走小程序口径。
func TestTasksLiveGlobalAccountSkipsMiniProgramHeader(t *testing.T) {
	f := &liveFake{}
	p := newLivePanel(t, f, testRealmAuth(t, "gl-1", "global"))

	out := getLiveJSON(t, p, "/panel/api/tasks/live")
	if row := liveRowOf(out, "gl-1"); row == nil || row["realm"] != "global" {
		t.Fatalf("国际号应出现在结果里：%+v", out["accounts"])
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := f.mpCalls["gl-1"]; got != "" {
		t.Fatalf("国际号不该带小程序口径头，实际 X-Client-Platform=%q", got)
	}
	if got := f.mpCalls["cn-1"]; got == "" {
		t.Fatalf("国内号应走小程序口径，实际头为空")
	}
}

// 一轮跑得比请求愿意等的久（账号多、个别上游挂住，单次最长 120s）：请求要在上限内
// 带着 refreshing=true 返回，后到的请求（含 fresh）不能再叠一轮，上游放行后下一次
// 请求拿到完整快照。上游原实现让首个请求一直等到整轮结束，后到的请求 30s 后回 503。
func TestTasksLiveSlowRoundDoesNotBlockOrStack(t *testing.T) {
	old, oldFloor := taskLiveWait, taskLiveFreshFloor
	taskLiveWait, taskLiveFreshFloor = 50*time.Millisecond, 0
	t.Cleanup(func() { taskLiveWait, taskLiveFreshFloor = old, oldFloor })

	f := &liveFake{block: make(chan struct{})}
	p := newLivePanel(t, f)
	// 断言失败时也要放行被阻塞的假上游，否则 httptest.Server.Close 会一直等；
	// 在 newLivePanel 之后注册，保证先于它的 Close 执行（Cleanup 后进先出）。
	var once sync.Once
	release := func() { once.Do(func() { close(f.block) }) }
	t.Cleanup(release)

	start := time.Now()
	out := getLiveJSON(t, p, "/panel/api/tasks/live")
	if time.Since(start) > 5*time.Second {
		t.Fatalf("请求被慢轮次拖住了：%v", time.Since(start))
	}
	if out["refreshing"] != true {
		t.Fatalf("一轮未完成时应报 refreshing：%+v", out)
	}
	if out["cached"] != true {
		t.Fatalf("等待超时后回的是上一份快照，cached 应为 true：%+v", out)
	}
	getLiveJSON(t, p, "/panel/api/tasks/live")
	getLiveJSON(t, p, "/panel/api/tasks/live?fresh=1")

	release()
	deadline := time.Now().Add(5 * time.Second)
	for {
		out = getLiveJSON(t, p, "/panel/api/tasks/live")
		if out["refreshing"] == false {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("放行后一轮仍未结束：%+v", out)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if accts, _ := out["accounts"].([]any); len(accts) != 2 {
		t.Fatalf("放行后应拿到完整快照：%+v", out)
	}
	if n := f.calls.Load(); n != 2 {
		t.Fatalf("慢轮次期间不该叠第二轮：calls=%d want 2", n)
	}
}

// 全部领完的号不带任务表：生产两百上下个号里绝大多数是这种，带上只是每轮白白序列化
// 几百 KB、前端白白重画几千行。计数仍要有，汇总才对得上。
func TestTasksLiveFullyClaimedOmitsTasks(t *testing.T) {
	f := &liveFake{doneUID: "cn-2"}
	p := newLivePanel(t, f)

	out := getLiveJSON(t, p, "/panel/api/tasks/live")
	done, busy := liveRowOf(out, "cn-2"), liveRowOf(out, "cn-1")
	if done["claimed"].(float64) != 1 || done["pending"].(float64) != 0 {
		t.Fatalf("已领完的号计数应仍在: %+v", done)
	}
	if _, has := done["tasks"]; has {
		t.Fatalf("已领完的号不该带任务表: %+v", done)
	}
	if ts, _ := busy["tasks"].([]any); len(ts) != 2 {
		t.Fatalf("还有待办的号应带完整任务表: %+v", busy)
	}
}

/* ── 前端（node 沙箱）──────────────────────────────────────────────── */

// liveNodePrelude 实时任务卡的前端夹具：切出 app.js 的实时任务块、go()、refreshVisible
// 在 node 里求值。各用例只写 body，夹具提供 liveReply / apiFail / timers / timeouts /
// calls / nodes / ctx / flush / liveCalls。
const liveNodePrelude = `
const fs = require('fs'), vm = require('vm'), assert = require('assert');
const src = fs.readFileSync(process.argv[2], 'utf8');
const slice = (from, to) => { const a = src.indexOf(from), b = src.indexOf(to, a); assert(a > 0 && b > a, from); return src.slice(a, b); };
const nodes = new Map(), timers = new Map(), calls = [], timeouts = new Map();
let tick = 0, liveReply = {accounts: []}, innerWrites = 0, apiFail = false;
const node_ = id => {
  const n = {textContent: '', hidden: false, disabled: false, className: '', value: '', style: {}, classList: {add(){}, remove(){}}, addEventListener(){}};
  let html = '';
  Object.defineProperty(n, 'innerHTML', {get: () => html, set: v => { html = v; if (id === 'liveList') innerWrites++; }});
  return n;
};
const doc = {hidden: false, addEventListener(){}, querySelectorAll: () => []};
const ctx = {
  console, Promise, Object, Array, String, Number, Map, JSON, encodeURIComponent, document: doc,
  localStorage: {getItem: () => null, setItem(){}},
  $: id => { if (!nodes.has(id)) nodes.set(id, node_(id)); return nodes.get(id); },
  api: path => { calls.push(path); return apiFail ? Promise.reject(new Error('boom')) : Promise.resolve(path.startsWith('tasks/live') ? liveReply : {}); },
  esc: s => String(s == null ? '' : s).replace(/</g, '&lt;'),
  view: 'accounts', TITLES: {tasks: '任务中心'},
  loadModels(){}, loadModelRules(){}, loadConfig(){}, loadLogs(){}, loadUsage(){}, loadBalance(){}, loadPackages(){},
  loadOverview(){}, openTasks(){},
  setInterval: (fn, ms) => { const id = ++tick; timers.set(id, {fn, ms}); return id; },
  clearInterval: id => timers.delete(id),
  setTimeout: (fn, ms) => { const id = ++tick; timeouts.set(id, {fn, ms}); return id; },
  clearTimeout: id => timeouts.delete(id),
};
vm.createContext(ctx);
vm.runInContext(slice('/* ── 实时任务', '/* ── 开学季任务单元'), ctx);
vm.runInContext(slice('function refreshVisible()', 'function pollIntervalMs()'), ctx);
vm.runInContext(slice('function go(v) {', "document.querySelectorAll('.nav a').forEach(a => a.onclick"), ctx);
const flush = async () => { for (let i = 0; i < 10; i++) await Promise.resolve(); };
const liveCalls = () => calls.filter(p => p.startsWith('tasks/live')).length;
`

// runLiveNode 在夹具后接上用例脚本并执行；没有 node 时跳过（与其它前端测试一致）。
func runLiveNode(t *testing.T, name, body string) {
	t.Helper()
	nodeBin(t) // node 不可用时跳过；必要时配置 linker64 代跑
	script := liveNodePrelude + "\n(async () => {\n" + body + "\n})().catch(err => { console.error(err); process.exitCode = 1; });"
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

// 实时任务卡只在进入页面时启动一次：refreshVisible（5s 全局轮询）反复调用不能重启
// 定时器，否则用户选的刷新间隔与「关」形同虚设；所有档位都不低于后端缓存 TTL。
func TestAppJSLiveTasksStartsOnlyOnEntry(t *testing.T) {
	runLiveNode(t, "live-start", `
  vm.runInContext("go('tasks')", ctx); await flush();
  assert.strictEqual(liveCalls(), 1, '进入本页拉一次');
  assert.strictEqual(timers.size, 1, '只起一个定时器');
  assert.deepStrictEqual([...timers.values()].map(t => t.ms), [120000], '默认 2 分钟一轮');
  assert.strictEqual(vm.runInContext('liveEvery', ctx), 120);
  const choices = vm.runInContext('LIVE_EVERY_CHOICES', ctx);
  assert(choices.every(s => s === 0 || s >= 60), '档位不低于后端缓存 TTL：' + choices);

  const timerIds = [...timers.keys()].join();
  for (let i = 0; i < 12; i++) { vm.runInContext('refreshVisible()', ctx); await flush(); }
  assert.strictEqual(liveCalls(), 1, '全局轮询不该驱动实时任务');
  assert.strictEqual([...timers.keys()].join(), timerIds, '全局轮询不该重置定时器');

  // tick 不得改写用户选的间隔
  [...timers.values()][0].fn(); await flush();
  assert.strictEqual(vm.runInContext('liveEvery', ctx), 120, 'tick 不得改写间隔');

  // 「关」：定时器撤掉，再进页面只拉一次、不再起定时器
  nodes.get('liveEvery').onchange({target: {value: '0'}});
  assert.strictEqual(timers.size, 0);
  vm.runInContext("go('tasks')", ctx); await flush();
  assert.strictEqual(timers.size, 0, '关闭后进入本页不该起定时器');
  assert.strictEqual(liveCalls(), 3);
`)
}

// 任务行必须真的渲染出内容：曾把 qrowHTML 的 HTML 再当行对象喂一次渲染函数，整行
// 变成空白与 "undefined"。失败号要单独一行，带上游报错。
func TestAppJSLiveTaskRowsRender(t *testing.T) {
	runLiveNode(t, "live-rows", `
  liveReply = {age_sec: 0, accounts: [
    {uid: 'cn-1', nickname: 'n1', pending: 1, claimable: 0, claimed: 1, tasks: [
      {task_code: 'chat_5', title: '发起 5 次对话', current: 2, target: 5, credit: 100},
      {task_code: 'first_buddy', title: '领取一只 Buddy', claimed: true}]},
    {uid: 'cn-2', nickname: 'n2', error: 'token expired'}]};
  vm.runInContext('loadLiveTasks(false)', ctx); await flush();
  const html = nodes.get('liveList').innerHTML;
  assert(html.includes('chat_5') && html.includes('发起 5 次对话') && html.includes('2/5'), '任务行要有内容：' + html);
  assert(!html.includes('undefined'), '任务行不应出现 undefined：' + html);
  assert(html.includes('data-live-uid="cn-1"') && html.includes('token expired'), html);
`)
}

// 数据没变就不重画：账号多时一轮是几百个分组，每个间隔重建一遍 DOM 纯属浪费，还会
// 冲掉用户正在选中的文字。失败提示在恢复后要收起——包括「数据没变」直接早退的路径。
func TestAppJSLiveTasksSkipsUnchangedRedraw(t *testing.T) {
	runLiveNode(t, "live-redraw", `
  liveReply = {age_sec: 0, accounts: [{uid: 'cn-1', nickname: 'n1', pending: 1, claimed: 0, tasks: [{task_code: 'chat_5', title: 'T'}]}]};
  vm.runInContext('loadLiveTasks(false)', ctx); await flush();
  const writes = innerWrites;
  assert(writes > 0);
  liveReply = {age_sec: 30, accounts: liveReply.accounts};
  vm.runInContext('loadLiveTasks(false)', ctx); await flush();
  assert.strictEqual(innerWrites, writes, '数据没变不该重画');
  assert(nodes.get('liveSummary').textContent.includes('30 秒前'), nodes.get('liveSummary').textContent);

  apiFail = true;
  vm.runInContext('loadLiveTasks(false)', ctx); await flush();
  assert.strictEqual(nodes.get('liveNote').hidden, false);
  apiFail = false;
  vm.runInContext('loadLiveTasks(false)', ctx); await flush();
  assert.strictEqual(innerWrites, writes);
  assert.strictEqual(nodes.get('liveNote').hidden, true, '数据没变时恢复后提示也要收起');
`)
}

// 后端那一轮还没跑完时返回的是上一份快照：页面要几秒后自动再取，否则卡片总是比数据
// 慢一个轮询间隔。这只是读服务端缓存；一轮结束（refreshing=false）就停。
func TestAppJSLiveTasksFollowsUpWhileRefreshing(t *testing.T) {
	runLiveNode(t, "live-refreshing", `
  const rows = [{uid: 'cn-1', nickname: 'n1', pending: 1, claimed: 0, tasks: [{task_code: 'chat_5', title: 'T'}]}];
  liveReply = {refreshing: true, accounts: rows};
  vm.runInContext("view = 'tasks'", ctx);
  vm.runInContext('loadLiveTasks(false)', ctx); await flush();
  assert.strictEqual(timeouts.size, 1, '还在刷新就该续约一次再取');
  const before = liveCalls();
  [...timeouts.values()][0].fn(); await flush();
  assert.strictEqual(liveCalls(), before + 1);

  liveReply = {refreshing: false, accounts: rows};
  timeouts.clear();
  vm.runInContext('loadLiveTasks(false)', ctx); await flush();
  assert.strictEqual(timeouts.size, 0, '一轮结束不再续约');
`)
}
