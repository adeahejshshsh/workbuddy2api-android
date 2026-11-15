package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// 写桶必须归一到本地时区（审查 U5）：读/折叠侧用 time.Local 解释桶串，
// 调用方传 UTC 时会让桶串与解释口径错位，折叠到错误的日。
func TestAddNormalizesToLocal(t *testing.T) {
	r := New("")
	utc := time.Date(2026, 11, 15, 3, 0, 0, 0, time.UTC)
	r.Add(utc, "cn", "u1", "m1", Delta{}, true)
	local := utc.In(time.Local)
	want := "h:" + local.Format(hourLayout)
	r.mu.Lock()
	_, ok := r.buckets[keyPart(want)+keyPart("cn")+keyPart("u1")+keyPart("m1")+keyPart("")]
	r.mu.Unlock()
	if !ok {
		t.Fatalf("桶键应使用本地时区 %q", want)
	}
}

// total=0 但 pt/ct 非 0 时要按 pt+ct 兜底（审查 U7），否则总量列系统性少算。
func TestAddFallsBackWhenTotalZero(t *testing.T) {
	r := New("")
	r.Add(time.Now(), "cn", "u1", "m1", Delta{
		PromptTokens: 10, HasPromptTokens: true,
		CompletionTokens: 5, HasCompletion: true,
		TotalTokens: 0, HasTotal: true, // 上游给了 total: 0
	}, true)
	snap := r.SnapshotWithRates(1, nil, nil)
	if snap.Totals.TotalTokens != 15 {
		t.Fatalf("总量=%d，期望 15（total=0 时应回落到 pt+ct）", snap.Totals.TotalTokens)
	}
}

// 桶数上限要在 Add 就生效（审查 U6）：此前只在 30s 一次的 Rollup 里检查，
// 那个窗口内能被灌进任意多新键。
func TestAddRefusesNewBucketsAtCap(t *testing.T) {
	old := maxBuckets
	maxBuckets = 3
	defer func() { maxBuckets = old }()

	r := New("")
	now := time.Now()
	for i := 0; i < 10; i++ {
		r.Add(now, "cn", "u1", "m"+strconv.Itoa(i), Delta{}, true)
	}
	r.mu.Lock()
	n := len(r.buckets)
	dropped := r.droppedNewBuckets
	r.mu.Unlock()
	if n > maxBuckets {
		t.Fatalf("桶数=%d 超过上限 %d", n, maxBuckets)
	}
	if dropped == 0 {
		t.Fatal("被拒的新键应有计数（否则面板无法解释「某个模型突然没统计」）")
	}
	// 已有键仍要能累加：限的是新键，不是全部写入。
	before := r.buckets[keyPart("h:"+now.Format(hourLayout))+keyPart("cn")+keyPart("u1")+keyPart("m0")+keyPart("")]
	_ = before
	r.Add(now, "cn", "u1", "m0", Delta{}, true)
}

// 文件版本比本进程新时不能按旧结构硬读（审查 U8）：要备份后从空表开始，
// 否则第一次 flush 会把新版数据按旧格式写回、彻底毁掉。
func TestLoadRejectsNewerFileVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	f := file{Version: fileVersion + 1}
	f.Buckets = []bucket{{Scope: "h:" + time.Now().Format(hourLayout), Realm: "cn", UID: "u", Model: "m"}}
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	r := New(path)
	r.mu.Lock()
	n := len(r.buckets)
	r.mu.Unlock()
	if n != 0 {
		t.Fatalf("新版格式不该被读入，实际恢复了 %d 个桶", n)
	}
	if _, err := os.Stat(path + ".v" + strconv.Itoa(fileVersion+1) + ".bak"); err != nil {
		t.Fatalf("应留下备份文件：%v", err)
	}
}
