package credithist

import (
	"path/filepath"
	"testing"
)

// 首次见到某 uid 只建基线，不记流水（否则历史余额会被误报成「刚获得」）。
func TestFirstObservationBuildsBaselineOnly(t *testing.T) {
	l := New("", 10)
	l.Observe("u1", 100)
	if got := l.Read(0); len(got) != 0 {
		t.Fatalf("首次观测不该产生流水，得到 %d 条", len(got))
	}
}

func TestObserveRecordsDelta(t *testing.T) {
	l := New("", 10)
	l.Observe("u1", 100)
	l.Observe("u1", 130)
	l.Observe("u1", 120) // 消耗也要留痕（带符号）
	got := l.Read(0)
	if len(got) != 2 {
		t.Fatalf("流水条数 = %d want 2", len(got))
	}
	// Read 新的在前
	if got[0].Delta != -10 || got[0].Before != 130 || got[0].After != 120 {
		t.Fatalf("最新一条不对: %+v", got[0])
	}
	if got[1].Delta != 30 || got[1].Before != 100 || got[1].After != 130 {
		t.Fatalf("较早一条不对: %+v", got[1])
	}
}

func TestObserveSameBalanceNoEntry(t *testing.T) {
	l := New("", 10)
	l.Observe("u1", 100)
	l.Observe("u1", 100)
	if got := l.Read(0); len(got) != 0 {
		t.Fatalf("余额没变不该留痕，得到 %d 条", len(got))
	}
}

// 超过 maxDelta 基本可断定是上游脏数据/口径切换（如企业「不限量」哨兵）：
// 只更新基线、不留痕，避免一条假流水污染整段历史。
func TestObserveHugeDeltaOnlyUpdatesBaseline(t *testing.T) {
	l := New("", 10)
	l.Observe("u1", 100)
	l.Observe("u1", 100+maxDelta+1)
	if got := l.Read(0); len(got) != 0 {
		t.Fatalf("超限变动不该留痕，得到 %d 条", len(got))
	}
	// 基线已更新：回到 130 时应当以新基线计算
	l.Observe("u1", 130)
	got := l.Read(0)
	if len(got) != 1 || got[0].Before != 100+maxDelta+1 {
		t.Fatalf("基线没更新: %+v", got)
	}
}

func TestReadLimitAndEmptyUIDIgnored(t *testing.T) {
	l := New("", 10)
	l.Observe("  ", 100) // 空白 uid 不该建基线
	l.Observe("u1", 100)
	for i := 0; i < 5; i++ {
		l.Observe("u1", int64(101+i))
	}
	if got := l.Read(2); len(got) != 2 {
		t.Fatalf("limit 未生效: %d", len(got))
	}
	if len := len(l.Read(0)); len != 5 {
		t.Fatalf("全量条数 = %d want 5（空白 uid 不该计入）", len)
	}
}

func TestEntryCapDropsOldest(t *testing.T) {
	l := New("", 3)
	l.Observe("u1", 0)
	for i := 1; i <= 5; i++ {
		l.Observe("u1", int64(i))
	}
	got := l.Read(0)
	if len(got) != 3 {
		t.Fatalf("上限未生效: %d", len(got))
	}
	// 保留的是最后 3 条（5、4、3）
	if got[0].After != 5 || got[2].After != 3 {
		t.Fatalf("丢的不是最旧: %+v", got)
	}
}

// 重启后基线不丢：不能把「上次进程结束时的余额」当成新增。
func TestPersistenceKeepsBaseline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credit-history.json")
	l1 := New(path, 10)
	l1.Observe("u1", 500)

	l2 := New(path, 10)
	l2.Observe("u1", 500) // 与落盘基线相同 → 不该新增流水
	if got := l2.Read(0); len(got) != 0 {
		t.Fatalf("重启后重复观测不该留痕，得到 %d 条", len(got))
	}
	l2.Observe("u1", 540)
	if got := l2.Read(0); len(got) != 1 || got[0].Delta != 40 {
		t.Fatalf("重启后变动没记对: %+v", got)
	}
}

// nil 账本可安全调用（面板未启用积分历史时）。
func TestNilLedgerSafe(t *testing.T) {
	var l *Ledger
	l.Observe("u1", 1) // 不该 panic
	if got := l.Read(0); len(got) != 0 {
		t.Fatal("nil 账本该返回空")
	}
}
