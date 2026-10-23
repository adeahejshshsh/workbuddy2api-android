// spread.go 账号级摊开（issue #45 方案 A）：账号遍历型任务不再「同一秒打全池」，
// 每号在窗口内自己的偏移上执行。
//
// 只解决时间相关性：同一出口 IP 上多号在几秒内做同一动作，是最好认的形状。不解决
// 设备/IP 指纹，别当成「防封」。
package scheduler

import (
	"context"
	"fmt"
	"hash/fnv"
	"sort"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/cst"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// accountSpread 本轮各账号的执行时刻（绝对时间，计划生成时刻为起点）。
type accountSpread struct {
	at map[string]time.Time
}

// newAccountSpread 生成计划：偏移 = hash(任务|uid|日) % 窗口，冲突（间距 < minGap）
// 时把靠后的推开。
//
// 随机 + 推开，而不是 i×minGap：等距只是把「一起打」换成「按固定节拍打」，一样是
// 可识别的时间形状。偏移当天确定性（哈希含日期）：重启不重排，否则刚错开的位置又
// 被打乱回去。
//
// shortcut: 推开后越界的偏移钳到窗口末尾，账号数 × minGap 接近窗口时末尾会重叠；
// 真到那一步该调大窗口或调小 minGap，而不是把这里做成环形分配。
func newAccountSpread(kind taskKind, uids []string, window, minGap time.Duration, now time.Time) *accountSpread {
	secs := int64(window / time.Second)
	if secs <= 0 || len(uids) == 0 {
		return nil
	}
	gap := int64(minGap / time.Second)
	if gap < 0 {
		gap = 0
	}
	day := cst.Day(now)
	type entry struct {
		uid string
		off int64
	}
	plan := make([]entry, 0, len(uids))
	h := fnv.New32a()
	for _, uid := range uids {
		h.Reset()
		_, _ = fmt.Fprintf(h, "%s|%s|%s", kind, uid, day)
		plan = append(plan, entry{uid: uid, off: int64(h.Sum32()) % secs})
	}
	sort.Slice(plan, func(i, j int) bool { return plan[i].off < plan[j].off })
	for i := 1; i < len(plan); i++ {
		if min := plan[i-1].off + gap; plan[i].off < min {
			plan[i].off = min
		}
		if plan[i].off >= secs {
			plan[i].off = secs - 1
		}
	}
	e := &accountSpread{at: make(map[string]time.Time, len(plan))}
	for _, p := range plan {
		e.at[p.uid] = now.Add(time.Duration(p.off) * time.Second)
	}
	return e
}

// wait 等到该账号的偏移时刻；计划为 nil（未启用）或账号不在计划里时立即放行。
// false = ctx 取消（优雅停机不等睡满）。
func (a *accountSpread) wait(ctx context.Context, uid string) bool {
	if a == nil {
		return true
	}
	at, ok := a.at[uid]
	if !ok {
		return true
	}
	return sleepCtx(ctx, time.Until(at))
}

// order 按计划时刻升序返回账号列表；计划为 nil（未启用/手动触发）时原样返回。
//
// 遍历必须按计划时刻、而不是 Poll.List() 的 uid 序：uid 序下若排在前面的账号计划
// 时刻更晚，后面的账号会被逐个堵住，最后挤在同一秒跑完（issue #45 实测 10 个账号
// 塌缩成 2–3 个执行时刻）。按计划序遍历时每个 wait 都是最后一次，实际时刻 = 计划时刻。
func (a *accountSpread) order(list []pool.Status) []pool.Status {
	if a == nil || len(list) == 0 {
		return list
	}
	out := append([]pool.Status(nil), list...)
	sort.SliceStable(out, func(i, j int) bool {
		ti, iok := a.at[out[i].UID]
		tj, jok := a.at[out[j].UID]
		if !iok || !jok {
			return iok && !jok // 不在计划里的账号排到最后（它们本来就不等）
		}
		return ti.Before(tj)
	})
	return out
}

// spreadParams 摊开窗口与最小间隔（窗口 <= 0 = 关闭）。与其它排程参数同口径在
// schedMu 下快照，避免面板保存配置时的并发写。
// spreadEnabled 摊开是否启用（窗口 > 0）。面板/日志用它区分「关闭」与「计划为空」。
func (s *Scheduler) spreadEnabled() bool {
	w, _ := s.spreadParams()
	return w > 0
}

func (s *Scheduler) spreadParams() (window, minGap time.Duration) {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return time.Duration(s.cfg.AccountSpreadMinutes) * time.Minute,
		time.Duration(s.cfg.AccountSpreadMinGapSeconds) * time.Second
}

// spreadPlan 取本轮摊开计划。on=false（面板手动触发）恒为 nil：手动入口要的是
// 立刻跑完，不是散在半小时里。
func (s *Scheduler) spreadPlan(kind taskKind, on bool, now time.Time) *accountSpread {
	if !on {
		return nil
	}
	window, gap := s.spreadParams()
	if window <= 0 {
		return nil
	}
	list := s.cfg.Pool.List()
	uids := make([]string, len(list))
	for i, st := range list {
		uids[i] = st.UID
	}
	return newAccountSpread(kind, uids, window, gap, now)
}
