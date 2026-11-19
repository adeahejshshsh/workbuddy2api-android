// w3_frontend_key_test.go 管理员密钥不再落 localStorage 的回归钉（W3-04）。
//
// 为什么必须在 JS 层测而不是只做源码文本断言：这段逻辑的**失效方式**是「某处又
// 直接写了 localStorage」—— 源码断言能挡住 `localStorage.setItem(LS_KEY` 这种字面
// 写法，但挡不住换个变量名。这里把 app.js 顶部的存储块切出来在沙箱里真跑一遍：
// 写入后 localStorage 必须一次都没被 set，读取优先级必须是
// 注入变量 > 内存 > sessionStorage。
package panel

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestAppJSAdminKeyStorage(t *testing.T) {
	nodeBin(t)
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const LS_KEY =');
const end = src.indexOf('/* 自举令牌（W3-02）');
if (start < 0 || end < 0) throw new Error('key storage block not found');

const calls = { lsSet: [], lsRemove: [], ssSet: [], ssRemove: [] };
function mkStore(kind) {
  const m = new Map();
  return {
    getItem: k => (m.has(k) ? m.get(k) : null),
    setItem: (k, v) => { m.set(k, v); calls[kind + 'Set'].push(k); },
    removeItem: k => { m.delete(k); calls[kind + 'Remove'].push(k); },
  };
}
const ctx = {
  localStorage: mkStore('ls'),
  sessionStorage: mkStore('ss'),
  window: {},
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\n' +
  'this.setAuthKey=setAuthKey;this.authKey=authKey;' +
  'this.reset=()=>{authKeyMem="";};', ctx);

const out = {};
// ① 写入：只进 sessionStorage + 内存，localStorage 一次都不许 set。
ctx.setAuthKey('admin-secret-0123456789');
out.lsSet = calls.lsSet.slice();
out.ssSet = calls.ssSet.slice();
out.lsRemove = calls.lsRemove.slice();
out.mem = ctx.authKey();

// ② 模拟「同标签页刷新」：清掉内存变量，sessionStorage 里还在。
ctx.reset();
out.afterReset = ctx.authKey();

// ③ 宿主注入优先于 sessionStorage（MainActivity 注入的那份不进任何存储）。
ctx.window.__WB2A_ADMIN_KEY = 'injected-key-abcdef';
ctx.reset();
out.injected = ctx.authKey();

// ④ 清空：内存与 sessionStorage 都要清掉（注入变量是宿主每轮重新给的，不归这里管，
// 断言前先撤掉，否则读到的永远是注入值）。
ctx.window.__WB2A_ADMIN_KEY = '';
ctx.setAuthKey('');
out.cleared = ctx.authKey();
process.stdout.write(JSON.stringify(out));
`
	f, err := os.CreateTemp(t.TempDir(), "appjs-key-*.cjs")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()
	raw, err := nodeCmd(f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("admin key 存储用例失败: %v\n%s", err, raw)
	}
	var got struct {
		LsSet      []string `json:"lsSet"`
		SsSet      []string `json:"ssSet"`
		LsRemove   []string `json:"lsRemove"`
		Mem        string   `json:"mem"`
		AfterReset string   `json:"afterReset"`
		Injected   string   `json:"injected"`
		Cleared    string   `json:"cleared"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("输出不是 JSON: %v (%s)", err, raw)
	}
	if len(got.LsSet) != 0 {
		t.Fatalf("管理员密钥被写进了 localStorage：%v（W3-04 原样复现）", got.LsSet)
	}
	if len(got.SsSet) != 1 || got.SsSet[0] != "wb2api.key" {
		t.Fatalf("sessionStorage 写入 = %v want [wb2api.key]", got.SsSet)
	}
	if len(got.LsRemove) == 0 {
		t.Fatal("setAuthKey 应顺手删掉 localStorage 里的历史明文副本")
	}
	if got.Mem != "admin-secret-0123456789" {
		t.Fatalf("内存读取 = %q", got.Mem)
	}
	if got.AfterReset != "admin-secret-0123456789" {
		t.Fatalf("清掉内存后应从 sessionStorage 读回，得到 %q", got.AfterReset)
	}
	if got.Injected != "injected-key-abcdef" {
		t.Fatalf("注入变量必须优先于 sessionStorage，得到 %q", got.Injected)
	}
	if got.Cleared != "" {
		t.Fatalf("清空后仍读到 %q", got.Cleared)
	}
}

// TestAppJSNeverPersistsAuthKeyInLocalStorage 源码级兜底：出现
// localStorage.setItem(LS_KEY 或 localStorage.getItem(LS_KEY 的**赋值/读取**（迁移那一处
// 只允许出现一次，见 migrateLegacyAuthKey）就直接红。
func TestAppJSNeverPersistsAuthKeyInLocalStorage(t *testing.T) {
	raw, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	if strings.Contains(src, "localStorage.setItem(LS_KEY") {
		t.Fatal("app.js 仍在把密钥写进 localStorage（W3-04）")
	}
	if n := strings.Count(src, "localStorage.getItem(LS_KEY)"); n > 1 {
		t.Fatalf("localStorage.getItem(LS_KEY) 出现 %d 次：只允许 migrateLegacyAuthKey 里那一次", n)
	}
	if !strings.Contains(src, "function authKey()") || !strings.Contains(src, "function setAuthKey(") {
		t.Fatal("读取/写入必须统一走 authKey()/setAuthKey()")
	}
}
