package usage

import (
	"path/filepath"
	"testing"
	"time"
)

// W5-01 回归：Save → New（重启）→ 同键 Add → SnapshotWindow 不能把同一逻辑桶算两次。
// 旧实现 load() 用裸 "|" 拼键、Add 用 keyPart，重启后内存里出现两个桶；
// 这里断言「总请求数 == 2」且「桶数 == 1」。
func TestW5_01RestartSameKeyNotDoubleCounted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	now := time.Now().In(time.Local)

	r1 := New(path)
	r1.Add(now, "cn", "u1", "m1", Delta{}, true)
	r1.Save()

	r2 := New(path)
	// load 本身必须直接用规范键：如果它还在用裸 "|"，就会留下一个非规范键，
	// 靠 migrateLegacyKeysLocked 兜底时会把 dirty 置真。这里先钉住「迁移零触发」，
	// 否则把第 451 行改回裸 "|" 也会被迁移掩盖，回归就测不出来了。
	r2.mu.Lock()
	loadedBuckets, loadedDirty := len(r2.buckets), r2.dirty
	r2.mu.Unlock()
	if loadedBuckets != 1 || loadedDirty {
		t.Fatalf("load 应直接产出 1 个规范键且不触发旧键迁移，实际 buckets=%d dirty=%v",
			loadedBuckets, loadedDirty)
	}
	r2.Add(now, "cn", "u1", "m1", Delta{}, true)

	snap := r2.SnapshotWindow(Window{}, nil, nil)
	if snap.Totals.Requests != 2 {
		t.Fatalf("重启后同键累加：Requests=%d，期望 2（旧键未归一会翻倍/拆桶）", snap.Totals.Requests)
	}
	if snap.Buckets != 1 {
		t.Fatalf("重启后同键应只有一个桶，实际 %d 个", snap.Buckets)
	}
}

// 旧键迁移：内存里若已有裸 "|" 键，load 的归一必须把它并到规范键，且不丢累计量。
func TestW5_01LegacyKeysMigrated(t *testing.T) {
	r := New("")
	now := time.Now().In(time.Local)
	scope := "h:" + now.Format(hourLayout)
	rawKey := scope + "|cn|u1|m1|1.5"
	r.buckets[rawKey] = &bucket{Scope: scope, Realm: "cn", UID: "u1", Model: "m1", Rate: "1.5", Req: 3}
	r.buckets[bucketKey(scope, "cn", "u1", "m1", "1.5")] = &bucket{Scope: scope, Realm: "cn", UID: "u1", Model: "m1", Rate: "1.5", Req: 2}

	if n := r.migrateLegacyKeysLocked(); n != 1 {
		t.Fatalf("应迁移 1 个旧键，实际 %d", n)
	}
	if _, ok := r.buckets[rawKey]; ok {
		t.Fatal("旧键应已删除")
	}
	got := r.buckets[bucketKey(scope, "cn", "u1", "m1", "1.5")]
	if got == nil || got.Req != 5 {
		t.Fatalf("迁移应合并累计量，实际 %+v", got)
	}
}
