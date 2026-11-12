// checkin_frontend_test.go 钉住签到结果文案：四态必须说出四件不同的事。
//
// 为什么单独一条：判定在 Go 里、文案在 app.js 里，中间只靠 checkin_state 传话。
// 前端一旦退回「checkin_done 为真 → 签到已完成（绿）」，后端判得再准也白费——
// 重复点按钮照样看着像签到成功（hub#875 的原始症状）。
package panel

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAppJSCheckinResultStates(t *testing.T) {
	// 统一走 nodeBin：本机 node 需要 linker64 代跑（SELinux），直接 exec 会 permission denied。
	nodeBin(t)
	script := `const fs=require('fs'),vm=require('vm'),assert=require('assert');
const src=fs.readFileSync(process.argv[2],'utf8');
const start=src.indexOf('function checkinResultNote('),end=src.indexOf('function renderAccounts(');
assert(start>=0&&end>start,'checkinResultNote 未找到');
const ctx={};vm.createContext(ctx);vm.runInContext(src.slice(start,end)+'\nthis.note=checkinResultNote;',ctx);
const note=ctx.note;

const already=note({checkin_state:'already',checkin_done:true,checkin_message:'今天已签到，请明天再来',credits:350});
assert.equal(already.severity,'');
assert(already.message.includes('今天已签到'),'already 要说清今天已经签过：'+already.message);
assert(!already.message.includes('签到成功'),'already 不能说签到成功：'+already.message);
assert(!already.message.includes('+'),'already 不能出现积分增量：'+already.message);

const checked=note({checkin_state:'checked',checkin_done:true,checkin_credit:100,credits:350,credits_total:400});
assert.equal(checked.severity,'ok');
assert(checked.message.includes('+100 积分'),'真签到要报本次发放的积分：'+checked.message);

const noCredit=note({checkin_state:'checked',checkin_done:true,credits:350});
assert.equal(noCredit.severity,'ok');
assert(!noCredit.message.includes('+'),'上游没给积分数字时不得凭空造一个：'+noCredit.message);

const failed=note({checkin_state:'failed',checkin_done:false,checkin_message:'签到失败，已签到天数不足 7 天'});
assert.equal(failed.severity,'err');
assert(failed.message.includes('不足 7 天'),'失败要透传上游原因：'+failed.message);

const pending=note({checkin_state:'pending',checkin_done:false});
assert.equal(pending.severity,'warn');

// 兼容老响应（没有 checkin_state）：checkin_done 语义不变。
assert.equal(note({checkin_done:true,credits:350}).severity,'ok');
assert.equal(note({checkin_done:false,checkin_error:'活动未开启',credits:350}).severity,'err');
assert.notEqual(note({skipped:true,skip_reason:'国际区不适用'}).severity,'ok');
console.log('checkin result states passed');`
	path := filepath.Join(t.TempDir(), "checkin-note.cjs")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := nodeCmd(path, "app.js").CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
