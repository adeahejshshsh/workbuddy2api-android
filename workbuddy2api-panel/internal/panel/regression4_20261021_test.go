package panel

import (
	"strings"
	"testing"
)

// BUG-14：Ring 零值不许 panic（cap==0 时曾越界 + 除零）。
func TestRingZeroValueDoesNotPanic(t *testing.T) {
	var r Ring
	if _, err := r.Write([]byte("hello\n")); err != nil {
		t.Fatalf("零值 Write 报错: %v", err)
	}
	if got := r.Snapshot(); len(got) == 0 {
		t.Fatal("零值 Ring 设防后应当能正常收行")
	}
	if got := r.Snapshot(); !strings.Contains(got[0].Text, "hello") {
		t.Fatalf("内容不对：%+v", got)
	}
}
