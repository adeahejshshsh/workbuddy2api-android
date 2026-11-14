package pool

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestScheduleModelWindowRejectsPastReset 已经过去的重置时刻不该登记：窗口已经开始了
// （或者解析偏差），登记它只会在下一轮巡检里立刻触发一次无意义探针。
func TestScheduleModelWindowRejectsPastReset(t *testing.T) {
	p := New("")
	now := time.Now()
	if p.ScheduleModelWindow("u1", "glm-5.3", now.Add(-time.Second), true, now) {
		t.Fatal("过去的重置时刻不应登记")
	}
	if p.ScheduleModelWindow("", "glm-5.3", now.Add(time.Hour), true, now) {
		t.Fatal("空账号不应登记")
	}
	if p.ScheduleModelWindow("u1", "", now.Add(time.Hour), true, now) {
		t.Fatal("空模型不应登记")
	}
	if got := len(p.ModelWindows()); got != 0 {
		t.Fatalf("不该有窗口记录，got %d", got)
	}
}

// TestScheduleModelWindowIdempotent 同一次限流会连着撞好几次 429，时间戳一致时
// 不重写记录（否则每笔 429 都把 dirty 置起来，flusher 每 5 秒白写一次盘）。
func TestScheduleModelWindowIdempotent(t *testing.T) {
	p := New("")
	now := time.Now()
	reset := now.Add(time.Hour)
	if !p.ScheduleModelWindow("u1", "glm-5.3", reset, false, now) {
		t.Fatal("首次登记应返回 true")
	}
	if p.ScheduleModelWindow("u1", "glm-5.3", reset, false, now) {
		t.Fatal("同一窗口重复登记应返回 false")
	}
	// 补一个 quota 标记算变更（面板要能区分「额度用满」与「临时限流」）。
	if !p.ScheduleModelWindow("u1", "glm-5.3", reset, true, now) {
		t.Fatal("补 quota 标记应返回 true")
	}
	if w := p.ModelWindows()[0]; !w.Quota {
		t.Fatal("quota 标记应已写入")
	}
	// 新的重置时刻是另一个窗口。
	later := now.Add(2 * time.Hour)
	if !p.ScheduleModelWindow("u1", "glm-5.3", later, true, now) {
		t.Fatal("新的重置时刻应覆盖旧记录")
	}
	if got := len(p.ModelWindows()); got != 1 {
		t.Fatalf("窗口应按 (uid, model) 唯一，got %d", got)
	}
	if w := p.ModelWindows()[0]; !w.ResetAt.Equal(later) {
		t.Fatalf("ResetAt=%v want %v", w.ResetAt, later)
	}
}

// TestDueModelWindowsOrdering 到点判定与排序：只回到点的，按 ResetAt 升序。
func TestDueModelWindowsOrdering(t *testing.T) {
	p := New("")
	now := time.Now()
	// 窗口只登记未来的重置时刻（过去的一律拒绝，见上一个用例），所以用「把查询
	// 时刻往后推」来模拟到点，而不是登记一个已经过点的窗口。
	p.ScheduleModelWindow("u1", "a", now.Add(1*time.Minute), false, now)
	p.ScheduleModelWindow("u2", "b", now.Add(2*time.Minute), false, now)
	p.ScheduleModelWindow("u3", "c", now.Add(time.Hour), false, now)

	due := p.DueModelWindows(now.Add(3 * time.Minute))
	if len(due) != 2 {
		t.Fatalf("应有 2 个到点窗口，got %d", len(due))
	}
	if due[0].UID != "u1" || due[1].UID != "u2" {
		t.Fatalf("应按 ResetAt 升序，got %s,%s", due[0].UID, due[1].UID)
	}
	next, ok := p.NextModelWindowDue()
	if !ok {
		t.Fatal("应有下一个到点时刻")
	}
	// 排程据此精确醒来（而不是等固定周期），取的必须是最早的那条。
	if !next.Equal(now.Add(1 * time.Minute)) {
		t.Fatalf("next=%v want %v", next, now.Add(time.Minute))
	}
}

// TestClearQuotaCooldownGuards 探针成功后清冷却的两条硬边界：
//
//	11102 负缓存（ResetAt 零值）不能被一次成功抹掉——那是另一类语义；
//	探针期间撞上的更新一轮限额（ResetAt 更晚）更不能清——那是上游刚给的墙钟。
func TestClearQuotaCooldownGuards(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	now := time.Now()

	// 11102 负缓存（ResetAt 零值）
	p.BlockModelBackoff("u1", "m-neg", "11102 model not found")
	if p.ClearQuotaCooldown("u1", "m-neg", now) {
		t.Fatal("11102 负缓存不该被探针成功清掉")
	}
	if _, ok := p.Status("u1"); !ok {
		t.Fatal("账号不应消失")
	}

	// 6004 冷却到未来：本次探针可以清
	reset := now.Add(10 * time.Minute)
	p.CooldownSoftForModel("u1", time.Minute, reset, "m-6004", "6004 rate limited")
	if !p.ClearQuotaCooldown("u1", "m-6004", reset.Add(time.Second)) {
		t.Fatal("重置时刻已过的 6004 冷却应可清")
	}

	// 6004 冷却到更晚：不能被早于它的成功清掉
	later := now.Add(time.Hour)
	p.CooldownSoftForModel("u1", time.Minute, later, "m-new", "6004 rate limited")
	if p.ClearQuotaCooldown("u1", "m-new", now) {
		t.Fatal("ResetAt 比本次更晚的冷却不能被清（上游刚给的墙钟）")
	}
}

// TestModelWindowsPersistRoundTrip 窗口必须跨重启存活：进程内冷却表是易失的，
// 窗口记录跟着丢就等于每次重启都错过重置时刻——正是本机制要治的那段空转。
func TestModelWindowsPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	stateFp := filepath.Join(dir, "state.json")
	now := time.Now()
	reset := now.Add(90 * time.Minute)

	p := New(stateFp)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.ScheduleModelWindow("u1", "glm-5.3", reset, true, now)
	p.RecordWindowProbe("u1", "glm-5.3", reset, 3, "上游还没放开", now)
	p.Flush()

	p2 := New(stateFp)
	p2.SyncToDir(nil)
	w := p2.ModelWindows()
	if len(w) != 1 {
		t.Fatalf("重启后应恢复 1 条窗口，got %d", len(w))
	}
	if !w[0].ResetAt.Equal(reset) {
		t.Fatalf("ResetAt=%v want %v", w[0].ResetAt, reset)
	}
	if w[0].Attempts != 3 || !w[0].Quota || w[0].LastResult == "" {
		t.Fatalf("探针状态应一并恢复: %+v", w[0])
	}
}

// TestModelWindowsPruneStale 过期太久的窗口不该跟着 state.json 一直落盘：
// 那些账号大概早被删了，记录只会让每轮巡检白跑一次。
func TestModelWindowsPruneStale(t *testing.T) {
	p := New("")
	now := time.Now()
	// 直接写入一条过期记录：ScheduleModelWindow 会拒绝过去的时刻（那是它的正确
	// 语义），过期清理要单独验证，所以绕开登记入口。
	p.windows = map[string]*ModelWindow{
		windowKey("u1", "old"): {UID: "u1", Model: "old", ResetAt: now.Add(-windowMaxAge - time.Hour)},
	}
	p.ScheduleModelWindow("u2", "fresh", now.Add(time.Hour), false, now)
	p.pruneWindowsLocked(now)
	w := p.ModelWindows()
	if len(w) != 1 || w[0].Model != "fresh" {
		t.Fatalf("过期窗口应被清掉，got %+v", w)
	}
}

// TestDropModelWindow 撤销后不再出现在巡检里（探针成功/放弃都要走这条）。
func TestDropModelWindow(t *testing.T) {
	p := New("")
	now := time.Now()
	p.ScheduleModelWindow("u1", "m", now.Add(time.Minute), false, now)
	due := now.Add(2 * time.Minute)
	if len(p.DueModelWindows(due)) != 1 {
		t.Fatal("前提：应有 1 条到点窗口")
	}
	if !p.DropModelWindow("u1", "m") {
		t.Fatal("撤销应返回 true")
	}
	if len(p.DueModelWindows(due)) != 0 {
		t.Fatal("撤销后不该再被取出")
	}
	if p.DropModelWindow("u1", "m") {
		t.Fatal("重复撤销应返回 false")
	}
}
