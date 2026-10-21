package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// bug #9：桶数超上限时必须真的淘汰，而不是"触发了一次什么都不删的 Rollup"。
//
// Rollup 只折叠**早于 hourlyKeep**（90 天）的小时桶，而异常流量产生的全是「现在」
// 的桶 —— 所以在把上限压到 5 之后，造 8 个当前小时的桶，修复前一个都删不掉。
func TestRollupDropsOldestHourBucketsOverCap(t *testing.T) {
	old := maxBuckets
	maxBuckets = 5
	defer func() { maxBuckets = old }()

	r := New("")
	now := time.Now()
	for i := 0; i < 8; i++ {
		scope := "h:" + now.Add(-time.Duration(i)*time.Hour).Format(hourLayout)
		r.buckets[scope+"|cn|u"+strconv.Itoa(i)+"|m|1"] = &bucket{Scope: scope, Req: 1}
	}
	r.Rollup(now)
	if len(r.buckets) > maxBuckets {
		t.Fatalf("超限后桶数 = %d，期望 <= %d（最旧的小时桶必须被淘汰）", len(r.buckets), maxBuckets)
	}
	// 淘汰的必须是最旧的：最新那小时要还在
	newest := "h:" + now.Format(hourLayout) + "|cn|u0|m|1"
	if _, ok := r.buckets[newest]; !ok {
		t.Error("最新小时桶被误删了：淘汰顺序必须按时间从旧到新")
	}
}

// bug #12：load 必须给桶数封顶，且丢掉 Scope 非法的脏桶。
func TestLoadCapsBucketCountAndSkipsBadScope(t *testing.T) {
	old := maxBuckets
	maxBuckets = 5
	defer func() { maxBuckets = old }()

	now := time.Now()
	f := file{Version: 1}
	for i := 0; i < 14; i++ {
		scope := "h:" + now.Add(-time.Duration(i)*time.Hour).Format(hourLayout)
		f.Buckets = append(f.Buckets, bucket{Scope: scope, Realm: "cn", UID: "u", Model: "m" + strconv.Itoa(i), Rate: "1"})
	}
	// 两条脏数据：scope 既不是 h: 也不是 d:
	f.Buckets = append(f.Buckets,
		bucket{Scope: "x:garbage", UID: "u", Model: "bad1"},
		bucket{Scope: "", UID: "u", Model: "bad2"})

	path := filepath.Join(t.TempDir(), "usage.json")
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	r := New(path)
	if got, want := len(r.buckets), 2*maxBuckets; got != want {
		t.Fatalf("加载后桶数 = %d，期望封顶在 %d（含脏数据被丢弃）", got, want)
	}
}
