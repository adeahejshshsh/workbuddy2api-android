package panel

import (
	"os"
	"testing"
)

// TestAppJSCfgRealmTabSelfHide 配置页分域切换不许把 tab 按钮自己过滤掉。
//
// 背景：applyCfgRealmFilter 用 `#cfgForm [data-realm]` 全选，而三个 tab 按钮本身
// 就带 data-realm 且就在 #cfgForm 里。于是选「国际版」时同一段逻辑把「全部」「国内版」
// 两个按钮一起 display:none，tab 栏塌成只剩一个——字段过滤其实是对的，坏的是导航。
// 这种错 Go 层全绿、JS 语法检查也发现不了，只能跑真实函数数元素。
func TestAppJSCfgRealmTabSelfHide(t *testing.T) {
	// 用本仓的 nodeBin/nodeCmd（自带 linker64 代跑兜底）：直接 exec.Command("node", …)
	// 在 app 私有目录里会 permission denied，错误被 CombinedOutput 吞成空串，
	// 表现为「失败但没有任何输出」。
	nodeBin(t)
	script := `const fs = require('fs'), vm = require('vm'), assert = require('assert');
const src = fs.readFileSync(process.argv[2], 'utf8');

const s = src.indexOf('function applyCfgRealmFilter(');
const e = src.indexOf('\n}\n', s) + 2;
assert.ok(s >= 0 && e > s, 'applyCfgRealmFilter block not found');

// 最小 DOM 桩：tab 栏里的按钮 closest('#cfgRealmTabs') 命中，字段不命中。
const tabsHost = {};
const mk = (realm, inTabs) => {
  const el = {
    realm, style: {}, on: false,
    dataset: { realm },
    getAttribute: k => (k === 'data-realm' ? realm : null),
    closest: sel => (inTabs && sel === '#cfgRealmTabs' ? tabsHost : null),
    classList: { toggle(cls, v) { el.on = v; } },
  };
  return el;
};
const tabs = ['all', 'cn', 'global'].map(r => mk(r, true));
const fields = [mk('cn', false), mk('cn', false), mk('global', false), mk('global', false)];
const all = tabs.concat(fields);

const tabsBox = { querySelectorAll: () => tabs };
const ctx = {
  String, Array, console,
  cfgRealmFilter: 'all',
  $: id => (id === 'cfgRealmTabs' ? tabsBox : null),
  document: { querySelectorAll: sel => (sel === '#cfgForm [data-realm]' ? all : []) },
};
vm.createContext(ctx);
vm.runInContext(src.slice(s, e) + '\nthis.apply = applyCfgRealmFilter;', ctx);

const hidden = el => el.style.display === 'none';
const fieldStates = () => fields.map(f => f.style.display);
const assertTabsIntact = (when) =>
  assert.ok(!tabs.some(hidden), when + '：三个 tab 都不许被隐藏（实际 ' + JSON.stringify(tabs.map(b => String(b.style.display))) + '）');

ctx.cfgRealmFilter = 'global';
ctx.apply();
assertTabsIntact('选「国际版」');
assert.deepStrictEqual(fieldStates(), ['none', 'none', '', ''], '国际版只留 global 字段');
assert.deepStrictEqual(tabs.map(b => b.on), [false, false, true], '只有「国际版」tab 高亮');

ctx.cfgRealmFilter = 'cn';
ctx.apply();
assertTabsIntact('选「国内版」');
assert.deepStrictEqual(fieldStates(), ['', '', 'none', 'none'], '国内版只留 cn 字段');
assert.deepStrictEqual(tabs.map(b => b.on), [false, true, false], '只有「国内版」tab 高亮');

ctx.cfgRealmFilter = 'all';
ctx.apply();
assertTabsIntact('选「全部」');
assert.ok(!fields.some(hidden), '「全部」要显示所有分域字段');
console.log('CFG REALM OK');
`
	f, err := os.CreateTemp(t.TempDir(), "cfg-realm-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	cmd := nodeCmd(f.Name(), "app.js")
	cmd.Dir = "." // 测试工作目录 = internal/panel
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cfg realm tab test failed:\n%s", out)
	}
}
