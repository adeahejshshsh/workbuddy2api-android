package panel

// 备注写不进浏览器存储时必须返回 false（上游 2026-10-29 `9232c89` 的前置契约）。
//
// 原来 storeRemarks 用空 catch 吞掉 localStorage 的异常，而保存流程无条件 toast
// 「备注已保存」—— 隐私模式或配额满时界面说保存成功、刷新后全丢，是静默的数据丢失。
// 契约改为返回 bool，调用方据此报错，这里钉住这个返回值。

import (
	"encoding/json"
	"os"
	"testing"
)

func TestAppJSStoreRemarksReportsFailure(t *testing.T) {
	nodeBin(t)
	script := `const fs = require('fs'), vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const s = src.indexOf('function storeRemarks(map) {');
const e = src.indexOf('\n}\n', s);
if (s < 0 || e < 0) throw new Error('storeRemarks not found');
const code = "const LS_REMARKS = 'k';\nlet remarksCache = null;\n" + src.slice(s, e + 3) +
  '\nthis.storeRemarks = storeRemarks; this.cache = () => remarksCache;';
function run(setItem) {
  const ctx = { JSON, localStorage: { setItem } };
  vm.createContext(ctx);
  vm.runInContext(code, ctx);
  return ctx;
}
const out = {};
const ok = run(() => {});
out.okReturn = ok.storeRemarks({ u1: 'A' });
out.okCache = JSON.stringify(ok.cache());
const bad = run(() => { throw new Error('QuotaExceededError'); });
out.badReturn = bad.storeRemarks({ u1: 'A' });
out.badCache = JSON.stringify(bad.cache());
process.stdout.write(JSON.stringify(out));`
	f, err := os.CreateTemp(t.TempDir(), "remarks-*.cjs")
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
	var got struct {
		OkReturn  bool   `json:"okReturn"`
		OkCache   string `json:"okCache"`
		BadReturn bool   `json:"badReturn"`
		BadCache  string `json:"badCache"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("bad output: %v\n%s", err, out)
	}
	if !got.OkReturn {
		t.Error("写入成功时必须返回 true")
	}
	if got.OkCache != `{"u1":"A"}` {
		t.Errorf("写入成功后缓存应为本值，得到 %q", got.OkCache)
	}
	if got.BadReturn {
		t.Error("localStorage 抛异常时必须返回 false —— 否则界面谎报「已保存」、刷新后丢失")
	}
	if got.BadCache != `{"u1":"A"}` {
		t.Errorf("写盘失败时内存缓存仍应更新（本次会话内还能看到），得到 %s", got.BadCache)
	}
}
