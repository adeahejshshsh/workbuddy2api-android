package scheduler

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/cst"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// TestSpreadDeterministic 同一天同一任务同一批账号 → 偏移不变（重启不重排）。
func TestSpreadDeterministic(t *testing.T) {
	uids := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, cst.Loc)
	first := newAccountSpread(taskCheckin, uids, 30*time.Minute, 0, now)
	second := newAccountSpread(taskCheckin, uids, 30*time.Minute, 0, now)
	if first == nil || second == nil {
		t.Fatal("窗口 > 0 时应生成计划")
	}
	for _, uid := range uids {
		if !first.at[uid].Equal(second.at[uid]) {
			t.Fatalf("%s 偏移不稳定：%v vs %v", uid, first.at[uid], second.at[uid])
		}
	}
	// 换一天应换一批偏移：日期没进哈希就会全同。
	nextDay := now.AddDate(0, 0, 1)
	other := newAccountSpread(taskCheckin, uids, 30*time.Minute, 0, nextDay)
	same := 0
	for _, uid := range uids {
		if first.at[uid].Sub(now) == other.at[uid].Sub(nextDay) {
			same++
		}
	}
	if same == len(uids) {
		t.Fatal("跨天偏移全部相同，日期没进哈希")
	}
}

// TestSpreadRespectsMinGap 偏移撞车时靠后的被推开：同批任意相邻间隔 ≥ minGap，
// 且全部落在窗口内。
func TestSpreadRespectsMinGap(t *testing.T) {
	uids := make([]string, 10)
	for i := range uids {
		uids[i] = string(rune('a' + i))
	}
	window, gap := 30*time.Minute, 90*time.Second
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, cst.Loc)
	plan := newAccountSpread(taskTravel, uids, window, gap, now)

	offs := make([]time.Duration, 0, len(uids))
	for _, uid := range uids {
		off := plan.at[uid].Sub(now)
		if off < 0 || off >= window {
			t.Fatalf("%s 偏移 %v 越出窗口 %v", uid, off, window)
		}
		offs = append(offs, off)
	}
	sort.Slice(offs, func(i, j int) bool { return offs[i] < offs[j] })
	for i := 1; i < len(offs); i++ {
		if d := offs[i] - offs[i-1]; d < gap {
			t.Fatalf("排序后相邻间隔 %v < %v", d, gap)
		}
	}
}

// TestSpreadDisabled 窗口 <= 0 与空池不生成计划；nil 计划 / 计划外账号的 wait 立即放行。
func TestSpreadDisabled(t *testing.T) {
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, cst.Loc)
	if plan := newAccountSpread(taskCheckin, []string{"a"}, 0, 0, now); plan != nil {
		t.Fatal("窗口 0 应返回 nil")
	}
	if plan := newAccountSpread(taskCheckin, nil, time.Minute, 0, now); plan != nil {
		t.Fatal("空池应返回 nil")
	}
	var plan *accountSpread
	if !plan.wait(context.Background(), "a") {
		t.Fatal("nil 计划应立即放行")
	}
	// 池在本轮中途新增的账号不在计划里：立即执行，不睡。
	real := newAccountSpread(taskCheckin, []string{"a"}, time.Hour, 0, now)
	if !real.wait(context.Background(), "newcomer") {
		t.Fatal("计划外账号应立即放行")
	}
}

// TestSpreadManualEntryNeverSpreads 面板手动触发不受摊开影响（否则一次点击要等半小时）。
func TestSpreadManualEntryNeverSpreads(t *testing.T) {
	s := New(Config{AccountSpreadMinutes: 30, AccountSpreadMinGapSeconds: 60})
	if plan := s.spreadPlan(taskCheckin, false, time.Now()); plan != nil {
		t.Fatal("手动入口不应生成摊开计划")
	}
	if !s.spreadEnabled() {
		t.Fatal("窗口 > 0 时 spreadEnabled 应为 true")
	}
}

// TestSpreadOrderFollowsPlannedTime 遍历顺序必须按计划时刻升序：这样每个 wait 都是
// 最后一次，实际开始时刻 = 计划时刻。uid 序下若前面的账号计划时刻更晚，后面的会被
// 逐个堵住、最后挤在同一秒跑完（issue #45 实测：10 个账号塌缩成 2–3 个执行时刻）。
func TestSpreadOrderFollowsPlannedTime(t *testing.T) {
	uids := make([]string, 10)
	for i := range uids {
		uids[i] = string(rune('a' + i))
	}
	now := time.Date(2026, 10, 22, 21, 0, 0, 0, cst.Loc)
	plan := newAccountSpread(taskCheckin, uids, 45*time.Minute, 60*time.Second, now)
	if plan == nil {
		t.Fatal("窗口 > 0 时应生成计划")
	}
	list := make([]pool.Status, len(uids))
	for i, uid := range uids {
		list[i] = pool.Status{UID: uid}
	}
	cur := now
	seen := make(map[string]bool, len(uids))
	for _, st := range plan.order(list) {
		if seen[st.UID] {
			t.Fatalf("账号 %s 重复", st.UID)
		}
		seen[st.UID] = true
		at := plan.at[st.UID]
		if at.After(cur) {
			cur = at
		}
		if !cur.Equal(at) {
			t.Fatalf("账号 %s 在计划序下仍被前一账号推迟：计划 %v，实际 %v", st.UID, at, cur)
		}
	}
	if len(seen) != len(uids) {
		t.Fatalf("order 丢了账号：%d/%d", len(seen), len(uids))
	}
}

// TestSpreadUIDOrderCollapses 回归证据：同一计划按 uid 序串行等待（修复前的行为）
// 会塌缩——不同执行时刻数明显少于账号数。钉住「问题真实存在」，防止有人把
// order() 改回 uid 序。
func TestSpreadUIDOrderCollapses(t *testing.T) {
	uids := make([]string, 10)
	for i := range uids {
		uids[i] = string(rune('a' + i))
	}
	now := time.Date(2026, 10, 22, 21, 0, 0, 0, cst.Loc)
	plan := newAccountSpread(taskCheckin, uids, 45*time.Minute, 60*time.Second, now)
	cur := now
	moments := make(map[time.Time]bool, len(uids))
	for _, uid := range uids {
		if at := plan.at[uid]; at.After(cur) {
			cur = at
		}
		moments[cur] = true
	}
	if len(moments) >= len(uids) {
		t.Fatalf("uid 序竟然没有塌缩（%d/%d），回归证据失效", len(moments), len(uids))
	}
}

// TestSpreadOrderNilPassthrough nil 计划（未启用/手动触发）不重排。
func TestSpreadOrderNilPassthrough(t *testing.T) {
	list := []pool.Status{{UID: "b"}, {UID: "a"}}
	var plan *accountSpread
	got := plan.order(list)
	if len(got) != 2 || got[0].UID != "b" || got[1].UID != "a" {
		t.Fatalf("nil 计划不应重排：%+v", got)
	}
}
