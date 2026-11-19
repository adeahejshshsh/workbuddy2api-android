package reqlog

import (
	"encoding/json"
	"math"
	"testing"
)

// W5-02 回归：Credit 的 NaN/±Inf 必须在 Record / encodeEvent / Snapshot 三处被消毒。
// 旧实现下：Record 收下毒值 → Snapshot json.Marshal 恒失败（/status 500）；
// encodeEvent 失败返回 nil → 归档静默丢事件且 dropped 不计数。
func TestW5_02NonFiniteCreditSanitized(t *testing.T) {
	r := New(Config{})
	r.Record(Event{Status: 200, OK: true, Credit: math.NaN(), HasCredit: true})

	snap := r.Snapshot()
	if _, err := json.Marshal(snap); err != nil {
		t.Fatalf("Snapshot 应可序列化，实际: %v", err)
	}
	if len(snap.Recent) != 1 {
		t.Fatalf("Recent=%d，期望 1", len(snap.Recent))
	}
	if snap.Recent[0].Credit != 0 || snap.Recent[0].HasCredit {
		t.Fatalf("NaN 应被归零并标记未观测，实际 credit=%v known=%v",
			snap.Recent[0].Credit, snap.Recent[0].HasCredit)
	}

	// encodeEvent 也必须能落盘（返回非 nil），且内容是合法 JSON。
	w := &archiveWriter{}
	raw := w.encodeEvent(&Event{Status: 200, OK: true, Credit: math.Inf(1), HasCredit: true})
	if raw == nil {
		t.Fatal("encodeEvent 不应因 Inf 返回 nil（归档静默丢事件）")
	}
	var got Event
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("归档行不是合法 JSON: %v", err)
	}
	if got.Credit != 0 || got.HasCredit {
		t.Fatalf("Inf 应被归零并标记未观测，实际 credit=%v known=%v", got.Credit, got.HasCredit)
	}
}

// 有限值不能被误伤：0 是合法的“实测 0 credit”，必须保留 HasCredit。
func TestW5_02FiniteCreditKept(t *testing.T) {
	r := New(Config{})
	r.Record(Event{Status: 200, OK: true, Credit: 0, HasCredit: true})
	snap := r.Snapshot()
	if !snap.Recent[0].HasCredit || snap.Recent[0].Credit != 0 {
		t.Fatalf("credit=0 应保留为已观测，实际 %+v", snap.Recent[0])
	}
}

// 编码失败（非法 RawMessage）返回 nil 时必须计入 dropped，不能静默丢事件。
func TestW5_02EncodeFailureCountsDropped(t *testing.T) {
	w := &archiveWriter{}
	if raw := w.encodeEvent(&Event{Status: 200, OK: true, Context: json.RawMessage(`{bad`)}); raw != nil {
		t.Fatalf("非法 Context 应编码失败，实际 %q", string(raw))
	}
	if got := w.dropped.Load(); got != 1 {
		t.Fatalf("编码失败应计 dropped=1，实际 %d", got)
	}
}
