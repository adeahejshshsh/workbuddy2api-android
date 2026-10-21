package panel

import (
	"strconv"
	"strings"
	"testing"
)

// bug 13：环写改固定数组后，快照顺序必须仍是「写入顺序」，且写满后淘汰最旧。
func TestRingWrapKeepsOrderAndEvictsOldest(t *testing.T) {
	r := NewRing(4)
	for i := 0; i < 7; i++ {
		if _, err := r.Write([]byte("line-" + strconv.Itoa(i) + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	got := r.Snapshot()
	if len(got) != 4 {
		t.Fatalf("快照 %d 条，期望 4（cap=4）", len(got))
	}
	want := []string{"line-3", "line-4", "line-5", "line-6"}
	for i, w := range want {
		if !strings.HasSuffix(got[i].Text, w) {
			t.Fatalf("第 %d 条 = %q，期望 %q（写入顺序、最旧已淘汰）", i, got[i].Text, w)
		}
	}
}

// 未写满时快照只含有效条目（不能把底层数组的空槽吐出去）。
func TestRingSnapshotBeforeFull(t *testing.T) {
	r := NewRing(8)
	for i := 0; i < 3; i++ {
		_, _ = r.Write([]byte("l" + strconv.Itoa(i) + "\n"))
	}
	if got := r.Snapshot(); len(got) != 3 {
		t.Fatalf("快照 %d 条，期望 3", len(got))
	}
}
