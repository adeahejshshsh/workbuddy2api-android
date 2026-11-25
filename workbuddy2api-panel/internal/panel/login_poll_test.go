package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestAppJSPollLoginResilience 登录轮询必须区分「抖动」与「终态」（借鉴上游 fork
// Albert-PZY f6abee7 / issue #215 复盘）：
//   - 网络抖动 / 5xx：继续轮询（OAuth 可能已在浏览器侧完成，停掉账号就永远不入池）；
//   - 404 / 410：会话作废，停止并提示重新发起；
//   - 成功：作废 packages 缓存（否则「重新登录后点检查还是 401」）并回显余额校验失败。
func TestAppJSPollLoginResilience(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; login poll test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function stopPoll()');
const end = src.indexOf('function closeAdd()');
if (start < 0 || end < start) throw new Error('pollLogin block not found');
const els = {};
function el(id) {
  if (!els[id]) els[id] = { textContent: '', hidden: false };
  return els[id];
}
let apiImpl = async () => ({});
let invalidated = 0;
const ctx = {
  Math, Number, String, JSON, encodeURIComponent, console,
  $: el,
  esc: (s) => String(s == null ? '' : s),
  setInterval: () => 42,
  clearInterval: () => {},
  setTimeout: () => {},
  loadOverview: () => {},
  invalidatePackages: () => { invalidated++; },
  api: (p) => apiImpl(p),
  loginState: 'st1',
  loginTimer: 7,
  loginStartAt: 0,
  // 本仓 app.js 的模块级状态（上游那版没有这三个名字）：轮询重入闸门 loginBusy、
  // 代次 loginGen、失败计数 pollFails。缺了它们，eval 出来的 pollLogin 直接 ReferenceError。
  loginBusy: false,
  loginGen: 0,
  pollFails: 0,
  closeAddTimer: null,
  credTxt: { textContent: '' },
  addReady: { hidden: false },
  addDone: { hidden: false, textContent: '' },
  addErr: { hidden: false, textContent: '' },
  login: {},
  poll: {},
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.pollLogin = pollLogin; this.getFails = () => pollFails; this.getTimer = () => loginTimer;', ctx);

const out = {};
(async () => {
  // 1) 抖动：保留轮询、计一次失败、提示自动重试。
  apiImpl = async () => { const e = new Error('HTTP 502'); e.status = 502; throw e; };
  await ctx.pollLogin();
  out.transientKeepsTimer = ctx.getTimer() !== null;
  out.fails = ctx.getFails();
  out.transientText = els['addErr'].textContent;

  // 2) 终态 410：停止轮询并提示重新发起。
  apiImpl = async () => { const e = new Error('gone'); e.status = 410; throw e; };
  await ctx.pollLogin();
  out.terminalStops = ctx.getTimer() === null;
  out.terminalText = els['addErr'].textContent;

  // 3) 成功：作废 packages 缓存 + 回显余额校验失败。
  apiImpl = async () => ({ done: true, uid: 'u1', nickname: 'n', credits: -1, credits_total: 0, balance_error: '401 invalid token' });
  await ctx.pollLogin();
  out.invalidated = invalidated;
  out.doneText = els['addDone'].textContent;

  // 4) pending 超过 90s：浏览器侧可能已报「登录失败」，面板不能一直无声等待。
  els['addErr'].hidden = true;
  apiImpl = async () => ({ done: false, message: 'code=11217 msg=login ing...' });
  ctx.loginStartAt = Date.now() - 95000;
  await ctx.pollLogin();
  out.slowHintShown = !els['addErr'].hidden;
  out.slowHintText = els['addErr'].textContent;
  process.stdout.write(JSON.stringify(out));
})();`
	f, err := os.CreateTemp(t.TempDir(), "poll-login-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	// 宿主没有可执行的 node（受限环境里私有目录 exec 被拒）时跳过：
	// 这条用例验的是 app.js 的浏览器侧逻辑，缺 node 属环境限制而不是回归。
	if vout, verr := exec.Command(node, "--version").CombinedOutput(); verr != nil {
		t.Skipf("node 不可执行（%v）：%s", verr, vout)
	}
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("node failed: %v\n%s", err, out)
	}
	var got struct {
		TransientKeepsTimer bool   `json:"transientKeepsTimer"`
		Fails               int    `json:"fails"`
		TransientText       string `json:"transientText"`
		TerminalStops       bool   `json:"terminalStops"`
		TerminalText        string `json:"terminalText"`
		Invalidated         int    `json:"invalidated"`
		DoneText            string `json:"doneText"`
		SlowHintShown       bool   `json:"slowHintShown"`
		SlowHintText        string `json:"slowHintText"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("bad output: %v\n%s", err, out)
	}
	if !got.TransientKeepsTimer {
		t.Errorf("网络抖动不该停轮询（OAuth 可能已完成）：%+v", got)
	}
	if got.Fails != 1 {
		t.Errorf("失败计数=%d want 1", got.Fails)
	}
	if !strings.Contains(got.TransientText, "自动重试中（1/") {
		t.Errorf("抖动应提示自动重试，得到 %q", got.TransientText)
	}
	if !got.TerminalStops || !strings.Contains(got.TerminalText, "登录会话已过期") {
		t.Errorf("410 应停止轮询并提示重新发起：%+v", got)
	}
	if got.Invalidated != 1 {
		t.Errorf("登录成功后应作废 packages 缓存（实际 %d 次）", got.Invalidated)
	}
	if !strings.Contains(got.DoneText, "余额校验失败") {
		t.Errorf("余额校验失败必须回显（否则「已添加但一直 401」是静默失败）：%q", got.DoneText)
	}
	if !got.SlowHintShown || !strings.Contains(got.SlowHintText, "重新获取授权链接") {
		t.Errorf("pending 超时后应提示可操作的重试步骤（浏览器报「登录失败」时面板否则无声干等）：%+v", got)
	}
}

// TestLoginPollGoneForUnknownState 未知/过期 state 必须回 410（终态）：前端据此停止轮询
// 并提示重新发起；若与网络抖动共用一个状态码，一次抖动就会让用户放弃等待。
func TestLoginPollGoneForUnknownState(t *testing.T) {
	p := New(Config{})
	rec := httptest.NewRecorder()
	p.loginPoll(rec, httptest.NewRequest("GET", "/panel/api/login/poll?state=nope", nil))
	if rec.Code != http.StatusGone {
		t.Fatalf("code=%d want 410 body=%s", rec.Code, rec.Body)
	}
}

// TestLoginPollProcessingGate 成功分支的副作用（落盘/签到/余额）可能耗时数秒，而前端每
// 3s 轮询一次：并发 poll 必须被闸门挡住，否则整段流程会重跑一遍（重复落盘 + 重复签到）。
func TestLoginPollProcessingGate(t *testing.T) {
	p := New(Config{})
	p.logins = map[string]loginSession{"st": {created: time.Now(), realm: "cn", processing: true}}
	rec := httptest.NewRecorder()
	p.loginPoll(rec, httptest.NewRequest("GET", "/panel/api/login/poll?state=st", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad json: %v (%s)", err, rec.Body)
	}
	if got["done"] != false {
		t.Fatalf("并发 poll 不应宣判完成：%v", got)
	}
	if msg, _ := got["message"].(string); !strings.Contains(msg, "正在写入凭据") {
		t.Fatalf("message=%q want 正在写入凭据", msg)
	}
}
