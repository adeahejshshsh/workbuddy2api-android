// window.go 额度窗口登记表（借鉴 ardeyouxipianyi/workbuddy2api-hub 的 wb_window_probe.py）。
//
// 补的是一个此前完全空着的半段：上游的免费额度窗口按**首次请求**起算。某条
// (账号, 模型) 的配额打满后，6004 的 429 会给出一个「将在 … 重置」的绝对时刻。
// 到点之后如果没有任何请求进来，新一轮窗口就不会开始——额度一直空转，下一次真实
// 请求来的时候窗口起点才被动落到那一刻，整整晚一截。
//
// 我们原本的处理到「解除冷却」为止：CooldownSoftForModel 把 Until 对齐上游墙钟，
// 到点 healthyForModel 放行——这是**被动**的，只是允许请求进来，不是让窗口开始计时。
//
// 本文件负责登记侧：429 带重置时刻时记一条窗口。到点的执行在 scheduler（那里才有
// upstream 客户端），探针成功后调 ClearQuotaCooldown 清模型级冷却。
//
// 为什么不复用 modelCooldowns：那张表是**路由**语义（不可选/可选），6004 条目在
// Until 到期的瞬间就被 pruneExpiredModelCooldowns 删掉——正好是我们要处理的时刻。
// 窗口是**排程**语义（到点该发一个请求），生命周期比冷却长，必须独立存。
//
// 与参照实现的差异：不做 quota_scan（主动轮询还没被限流的配额线）。那是另一种
// 功能——它要用探针去发现「哪条线已经满了」，而本文件只处理「已经知道满了、只等
// 起窗」的那一段。把两者混在一起会让探针请求量随账号×模型数量增长，而这一段是
// 每个已登记窗口只发一次。
package pool

import (
	"sort"
	"time"
)

// windowKey 窗口键：账号 + 裸模型名（不含域前缀）。
//
// 为什么不带域：同一账号只属于一个域，键里再加 realm 是冗余维度，只会让查表多一次
// 拼接、让面板多一列解释。
func windowKey(uid, model string) string { return uid + "\x00" + model }

// ModelWindow 一个已登记的额度窗口（对外只读视图）。
type ModelWindow struct {
	// UID 账号；Model 裸模型名（上游认的那个，不含 cn:/global: 前缀）。
	UID   string `json:"uid"`
	Model string `json:"model"`
	// ResetAt 上游给的重置时刻——这是新一轮窗口该起算的时刻。
	ResetAt time.Time `json:"reset_at"`
	// Attempts 到点后已重试次数（上游常比宣称的时刻晚几秒放开）。
	Attempts int `json:"attempts"`
	// LastTry / LastResult 上次探针的时刻与结果摘要（面板与排障用）。
	LastTry    time.Time `json:"last_try,omitempty"`
	LastResult string    `json:"last_result,omitempty"`
	// Quota true = 这次是 6004「使用量已超出频率限制」判定额度用满，
	// 而不是普通 429 临时限流。只作展示与日志区分，不改变探针行为。
	Quota bool `json:"quota,omitempty"`
}

// stateModelWindow 窗口的持久化形态（与 ModelWindow 同构，字段名对齐）。
type stateModelWindow struct {
	UID        string    `json:"uid"`
	Model      string    `json:"model"`
	ResetAt    time.Time `json:"reset_at"`
	Attempts   int       `json:"attempts,omitempty"`
	LastTry    time.Time `json:"last_try,omitempty"`
	LastResult string    `json:"last_result,omitempty"`
	Quota      bool      `json:"quota,omitempty"`
}

// windowMaxAge 窗口过期这么久还没被处理就丢弃：账号大概已删除或这条额度线早已
// 换了一轮，再探也是白打上游。
const windowMaxAge = 7 * 24 * time.Hour

// ScheduleModelWindow 登记一个额度窗口，返回是否新增/更新了记录。
//
// resetAt 必须在未来：已经过去的重置时刻说明窗口已经开始了（或者这是我们自己在
// 解析上的偏差），登记它只会在下一轮巡检里立刻触发一次无意义探针。
//
// 同一个窗口的重复 429（一次限流会连着撞好几次）时间戳一致，这里返回 false 不重写
// ——落盘由 dirty 标志触发，无谓地一直置 dirty 会让 flusher 每 5 秒写一次盘。
func (p *Pool) ScheduleModelWindow(uid, model string, resetAt time.Time, quota bool, now time.Time) bool {
	if uid == "" || model == "" || resetAt.IsZero() || !resetAt.After(now) {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.windows == nil {
		p.windows = map[string]*ModelWindow{}
	}
	// 顺手清掉过期太久的条目：窗口只在 429 时新增，长期运行的部署里被删除的账号
	// 会留下永不触发的记录，没有清理就会一直跟着 state.json 落盘。
	p.pruneWindowsLocked(now)
	key := windowKey(uid, model)
	if w, ok := p.windows[key]; ok && w.ResetAt.Equal(resetAt) {
		if quota && !w.Quota {
			w.Quota = true
			p.dirty.Store(true)
			return true
		}
		return false
	}
	p.windows[key] = &ModelWindow{UID: uid, Model: model, ResetAt: resetAt, Quota: quota}
	p.dirty.Store(true)
	return true
}

// ModelWindows 返回当前全部窗口的快照，按 ResetAt 升序（最该处理的在前）。
func (p *Pool) ModelWindows() []ModelWindow {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.windowListLocked()
}

func (p *Pool) windowListLocked() []ModelWindow {
	out := make([]ModelWindow, 0, len(p.windows))
	for _, w := range p.windows {
		out = append(out, *w)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ResetAt.Equal(out[j].ResetAt) {
			return out[i].UID < out[j].UID
		}
		return out[i].ResetAt.Before(out[j].ResetAt)
	})
	return out
}

// DueModelWindows 已到点（ResetAt <= now）的窗口，按 ResetAt 升序。
func (p *Pool) DueModelWindows(now time.Time) []ModelWindow {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var out []ModelWindow
	for _, w := range p.windows {
		if !w.ResetAt.After(now) {
			out = append(out, *w)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ResetAt.Equal(out[j].ResetAt) {
			return out[i].UID < out[j].UID
		}
		return out[i].ResetAt.Before(out[j].ResetAt)
	})
	return out
}

// NextModelWindowDue 最近一个窗口的到点时刻；没有待处理窗口时 ok=false。
//
// 排程用：把下一次唤醒钉在重置那一刻附近，而不是等一个固定周期——固定周期意味着
// 平均半个周期的空转，正是本功能要消除的那段。
func (p *Pool) NextModelWindowDue() (time.Time, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var next time.Time
	for _, w := range p.windows {
		if next.IsZero() || w.ResetAt.Before(next) {
			next = w.ResetAt
		}
	}
	return next, !next.IsZero()
}

// RecordWindowProbe 记一次探针结果。resetAt 非零表示上游又给了更晚的重置时刻
// （这条额度线确实进入新一轮等待），此时计数归零；resetAt 为零表示「还没放开」，
// 由调用方把 ResetAt 推到重试时刻并给出累计次数。
func (p *Pool) RecordWindowProbe(uid, model string, resetAt time.Time, attempts int, result string, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	w, ok := p.windows[windowKey(uid, model)]
	if !ok {
		return false
	}
	if !resetAt.IsZero() {
		w.ResetAt = resetAt
	}
	if !now.IsZero() {
		w.LastTry = now
	}
	w.Attempts = attempts
	if len(result) > 200 {
		result = result[:200]
	}
	w.LastResult = result
	p.dirty.Store(true)
	return true
}

// DropModelWindow 撤销一条窗口记录（探针成功、放弃、或账号已不在池中）。
func (p *Pool) DropModelWindow(uid, model string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := windowKey(uid, model)
	if _, ok := p.windows[key]; !ok {
		return false
	}
	delete(p.windows, key)
	p.dirty.Store(true)
	return true
}

// ClearQuotaCooldown 探针成功后清掉该 (账号, 模型) 的 6004 模型级冷却。
//
// at 传**这条窗口自己的 ResetAt**（不是 time.Now()）：探针证明的是「这条窗口对应的
// 那轮限额已经过去」，只有 ResetAt 不晚于它的冷却才该被清。
//
// 只清 **ResetAt 非零且不晚于 at** 的条目：11102 负缓存（ResetAt 零值）与「探针
// 期间又撞上的更新一轮限额」（ResetAt 更晚）都不能被一次成功抹掉——后者尤其危险，
// 那是上游刚给出的新墙钟，清掉等于立刻又去撞一次 429。
//
// 账号级冷却一概不碰：那是网络故障/余额类问题留下的，与本文件的额度窗口无关。
func (p *Pool) ClearQuotaCooldown(uid, model string, at time.Time) bool {
	if uid == "" || model == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	mc, ok := e.modelCooldowns[model]
	if !ok || mc.ResetAt.IsZero() || mc.ResetAt.After(at) {
		return false
	}
	delete(e.modelCooldowns, model)
	if len(e.modelCooldowns) == 0 {
		e.modelCooldowns = nil
	}
	p.dirty.Store(true)
	return true
}

// WindowProbeBlocked 该号此刻不该为 model 发窗口探针的原因；空串 = 可以发。
//
// 探针是一次真实的对话请求，会扣积分、在上游留下记录，所以与选号同一套人工与积分闸门：
// 停用、暂停的号不自动唤醒；余额触及保留积分或积分保底、模型收费时不花这笔钱；
// 已知免费号不碰订阅专属模型。冷却不在此列：到点时冷却正该过期，探针本身就是在问上游。
func (p *Pool) WindowProbeBlocked(uid, model string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	switch {
	case !ok:
		return "账号已不在池中"
	case e.disabled:
		return "账号已停用"
	case e.paused:
		return "账号已暂停"
	case p.reserveBlocked(e, p.modelFreeOf(e, model)):
		return "余额触及保留积分"
	case p.floorBlockedForRealmModel(e, model, e.a.Realm(), time.Now()):
		return "余额低于积分保底且模型收费"
	case p.exclusiveBlockedForRealmModel(e, model, e.a.Realm()):
		return "免费号调不通该模型"
	}
	return ""
}

// pruneWindowsLocked 丢弃过期太久的窗口（惰性清理，防 map 无限增长）。
// 调用方必须已持有 p.mu 写锁。
func (p *Pool) pruneWindowsLocked(now time.Time) {
	for k, w := range p.windows {
		if w.ResetAt.IsZero() || w.ResetAt.Before(now.Add(-windowMaxAge)) {
			delete(p.windows, k)
		}
	}
	// 刻意不把空表置 nil：本函数在 ScheduleModelWindow 的写锁内、紧接着 map 赋值
	// 调用，置 nil 会让那次赋值 panic（nil map）。空 map 与 nil map 在读取侧等价
	// （range/len/查表都安全），不值得为省一个 map header 埋这个坑。
}

// loadWindowsLocked 从持久化记录恢复窗口表（惰性剔除过期太久与结构破损条目）。
func (p *Pool) loadWindowsLocked(windows map[string]stateModelWindow, now time.Time) {
	if len(windows) == 0 {
		return
	}
	for k, s := range windows {
		if s.UID == "" || s.Model == "" || s.ResetAt.IsZero() || s.ResetAt.Before(now.Add(-windowMaxAge)) {
			continue
		}
		if p.windows == nil {
			p.windows = map[string]*ModelWindow{}
		}
		p.windows[k] = &ModelWindow{
			UID:        s.UID,
			Model:      s.Model,
			ResetAt:    s.ResetAt,
			Attempts:   s.Attempts,
			LastTry:    s.LastTry,
			LastResult: s.LastResult,
			Quota:      s.Quota,
		}
	}
}

// windowsStateLocked 收集窗口表为持久化形态（只写未过期条目）。
func (p *Pool) windowsStateLocked(now time.Time) map[string]stateModelWindow {
	if len(p.windows) == 0 {
		return nil
	}
	out := make(map[string]stateModelWindow, len(p.windows))
	for k, w := range p.windows {
		if w.ResetAt.IsZero() || w.ResetAt.Before(now.Add(-windowMaxAge)) {
			continue
		}
		out[k] = stateModelWindow{
			UID:        w.UID,
			Model:      w.Model,
			ResetAt:    w.ResetAt,
			Attempts:   w.Attempts,
			LastTry:    w.LastTry,
			LastResult: w.LastResult,
			Quota:      w.Quota,
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
