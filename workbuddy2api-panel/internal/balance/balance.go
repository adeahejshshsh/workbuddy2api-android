// Package balance 按固定间隔采样账号积分余额，供面板「余额趋势」视图画折线。
//
// 与 internal/usage 的区别（两者刻意分开，不合并）：
//   - usage 是**事件驱动**：每次聊天请求 Add 一次，记录的是「流量」（token 数），
//     没有请求就没有数据点；
//   - 本包是**定时驱动**：每 sampleInterval 主动查一次余额，记录的是「存量」
//     （某时刻的水位值），没有请求也照常采样。
//     把存量塞进请求驱动的桶里会出现「空闲时段无采样点 → 折线断裂」，那是数据缺陷
//     而非实现细节，故独立成包。
//
// 保留策略（两级粒度，总量有界）：
//   - 近 detailedKeep 内：原始采样点（默认 30 分钟一个）
//   - 更早：折叠为日粒度（每天取当日最后一个采样值 = 当日收盘水位），保留 detailKeep
//
// 落盘：与 state 文件同目录的 balance_history.json，原子替换 + 防抖刷新，重启不丢。
package balance

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	// sampleInterval 采样间隔：30 分钟一个点（1440 分钟 / 30 = 每天 48 点）。
	sampleInterval = 30 * time.Minute

	// detailedKeep 原始采样点的保留时长；超出后折叠为日粒度。
	// 30 天 × 48 点 ≈ 1440 点/账号，折线图可读且文件很小。
	detailedKeep = 30 * 24 * time.Hour

	// dayKeep 日粒度点的保留时长；更早的丢弃（总量有界）。
	dayKeep = 365 * 24 * time.Hour

	// flushInterval 防抖落盘间隔。
	flushInterval = 30 * time.Second

	// maxPoints 点数硬上限（所有账号合计）。超过时立即折叠，避免异常把文件撑爆。
	maxPoints = 200_000

	// dayLayout 日粒度键的格式（本地时区，与用户直觉一致）。
	dayLayout = "2006-01-02"

	// expectedPerDay 判定"某日采样完整"所需的最少点数。
	// 理论值 = 24h/30min = 48；留容差取 40（服务重启/短暂停机会让点数略少，
	// 不该因此判定整日不完整）。
	expectedPerDay = 40
)

// Point 一次采样（某账号在某时刻的积分水位）。
// JSON 字段名取短：点数会随时间增长。
type Point struct {
	TS      int64  `json:"t"`           // Unix 秒
	UID     string `json:"u"`           // 账号 uid
	Credits int64  `json:"c"`           // 该时刻积分余额
	Day     string `json:"d,omitempty"` // 非空 = 日粒度折叠点（值形如 2006-01-02）

	// 仅日粒度折叠点（Day != ""）有意义：折叠时把当日原始采样算出的
	// 消耗/到账一并记下来。折叠点每天只剩一个水位值，dailyOf 按相邻差值
	// 累加时拿不到任何相邻对，结果恒为 0 —— 表现为「30 天前的日子里
	// 消耗/到账永远是 0」，那是数据被丢掉了，不是真的没消耗。
	// 口径与 Daily.Consumed/Gained 一致（相邻采样下降/上升量之和），同为下界估计。
	Consumed int64 `json:"cons,omitempty"`
	Gained   int64 `json:"gain,omitempty"`
	First    int64 `json:"f,omitempty"` // 折叠点记录的当日首个水位（见 dailyOf）
}

// file 落盘结构。
type file struct {
	Version int     `json:"version"`
	Saved   string  `json:"saved"`
	Points  []Point `json:"points"`
}

// Recorder 并发安全的余额采样记录器。
type Recorder struct {
	mu     sync.Mutex
	path   string
	points []Point // 按 TS 升序维护（Add 时追加，采样本身即时间递增）
	dirty  bool

	// 每账号的上次采样时刻，用于「距上次不足 sampleInterval 则跳过」的节流
	// （防止高频调用把曲线打成锯齿）。
	//
	// ⚠️ 必须**按账号**而非全局：采样由 RunBalanceRefreshNow 并发触达所有账号，
	// 若共用一个时间戳，同一窗口内只有"抢到"的那个账号能落点，其余账号的点被
	// 静默丢弃 —— 症状是「多个账号只有一条线」。单账号时该缺陷不显形，故曾漏测。
	lastSample map[string]time.Time

	// names uid → 昵称，仅供 API 展示（不入盘：账号改名不影响历史数据）。
	names map[string]string

	// realms uid 到账号域（"cn"/"global"）的映射，仅供 API 分组展示（不入盘）。
	//
	// 语义：这是账号的**当前属性**，不是采样时刻的快照。账号重登/换域后，
	// 其全部历史点都会归入新域——有意取舍：realm 极少变化，变了通常意味着
	// 账号重登（历史余额本就该随账号归新域）；若把 realm 写进 Point，内存会
	// 放大 maxPoints 倍且结构复杂化，收益不抵成本。
	realms map[string]string

	clock func() time.Time // 便于测试注入

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// New 构造。path 为空时不做任何落盘（纯内存，用于测试）。
func New(path string) *Recorder {
	r := &Recorder{
		path:       path,
		names:      map[string]string{},
		realms:     map[string]string{},
		clock:      time.Now,
		lastSample: map[string]time.Time{},
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
	if path != "" {
		if err := r.load(); err != nil {
			log.Printf("[balance] 载入失败（按空历史启动）: %v", err)
		}
	}
	return r
}

// Interval 返回采样间隔，供调度器使用（避免两处各写一个魔法数字）。
func Interval() time.Duration { return sampleInterval }

// SetName 记录账号昵称（仅供展示，不落盘）。
func (r *Recorder) SetName(uid, name string) {
	if r == nil || uid == "" {
		return
	}
	r.mu.Lock()
	r.names[uid] = name
	r.mu.Unlock()
}

// SetRealm 记录账号所属域（"cn"/"global"），供面板按域分组展示。
// 与 SetName 同为展示元数据：不入盘，进程重启后由面板重新注入。
// realm 为空时忽略（不清除已有值，避免上游偶发空值抹掉已记录的域）。
func (r *Recorder) SetRealm(uid, realm string) {
	if r == nil || uid == "" || realm == "" {
		return
	}
	r.mu.Lock()
	r.realms[uid] = realm
	r.mu.Unlock()
}

// Add 记录一次采样。同一账号、同一采样窗口内重复调用只保留最后一个值
// （签到/领奖后余额会跳变，取最新值才是真实水位）。
// 与上次采样间隔不足 sampleInterval 时跳过，返回 false。
func (r *Recorder) Add(uid string, credits int64) bool {
	if r == nil || uid == "" {
		return false
	}
	now := r.clock()
	r.mu.Lock()
	defer r.mu.Unlock()

	if last, ok := r.lastSample[uid]; ok && now.Sub(last) < sampleInterval {
		return false
	}
	r.lastSample[uid] = now
	r.points = append(r.points, Point{TS: now.Unix(), UID: uid, Credits: credits})
	r.dirty = true

	// 点数超硬上限：立即折叠一次（异常保护）。
	if len(r.points) > maxPoints {
		r.rollupLocked(now)
	}
	return true
}

// Rollup 把超出 detailedKeep 的原始点折叠为日粒度（每账号每天留最后一个值）。
// 由调度器周期调用；也可在 Add 触发上限时内部调用。
func (r *Recorder) Rollup(now time.Time) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rollupLocked(now)
}

func (r *Recorder) rollupLocked(now time.Time) {
	cutoff := now.Add(-detailedKeep).Unix()

	// 待折叠：超出 detailedKeep 的原始点，按 (uid, 日) 分组取当日最后一个水位，
	// 同时把当日全部原始点按相邻差值累加出消耗/到账（折叠后没有相邻对了，
	// 不在这里算就永久丢失，见 Point.Consumed 注释）。
	//
	// 累加必须按时间顺序进行：Recorder.points 契约是 TS 升序（见结构体注释），
	// 故逐点与同账号同日前一个原始点比较即可，不需要再排序。
	type key struct {
		uid string
		day string
	}
	type fold struct {
		last     Point
		first    int64 // 当日首个水位（写进折叠点，保住 Change 口径）
		consumed int64
		gained   int64
		prev     int64
		hasPrev  bool
	}
	folds := map[key]*fold{}
	var order []key
	var kept []Point
	folded := 0
	for _, p := range r.points {
		if p.Day != "" || p.TS >= cutoff {
			kept = append(kept, p)
			continue
		}
		day := time.Unix(p.TS, 0).In(time.Local).Format(dayLayout)
		k := key{p.UID, day}
		f, ok := folds[k]
		if !ok {
			f = &fold{first: p.Credits}
			folds[k] = f
			order = append(order, k)
		}
		if f.hasPrev {
			if d := p.Credits - f.prev; d > 0 {
				f.gained += d
			} else if d < 0 {
				f.consumed += -d
			}
		}
		f.prev, f.hasPrev = p.Credits, true
		if !ok || p.TS > f.last.TS {
			f.last = p
		}
		folded++
	}
	if folded == 0 {
		r.pruneDaysLocked(now)
		return
	}
	for _, k := range order {
		f := folds[k]
		kept = append(kept, Point{
			TS: f.last.TS, UID: f.last.UID, Credits: f.last.Credits, Day: k.day,
			Consumed: f.consumed, Gained: f.gained, First: f.first,
		})
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].TS < kept[j].TS })
	r.points = kept
	r.dirty = true
	log.Printf("[balance] 折叠 %d 个原始采样点为 %d 个日粒度点（保留 %v 细粒度）", folded, len(order), detailedKeep)

	r.pruneDaysLocked(now)
}

// pruneDaysLocked 丢弃超出 dayKeep 的日粒度点。
func (r *Recorder) pruneDaysLocked(now time.Time) {
	cutoff := now.Add(-dayKeep)
	var kept []Point
	dropped := 0
	for _, p := range r.points {
		if p.Day == "" {
			kept = append(kept, p)
			continue
		}
		t, err := time.ParseInLocation(dayLayout, p.Day, time.Local)
		if err == nil && t.Before(cutoff) {
			dropped++
			continue
		}
		kept = append(kept, p)
	}
	if dropped > 0 {
		r.points = kept
		r.dirty = true
		log.Printf("[balance] 丢弃 %d 个超期日粒度点（保留 %v）", dropped, dayKeep)
	}
}

// ---------------------------------------------------------------- 持久化 ----

func (r *Recorder) load() error {
	raw, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 首次启动：空历史
		}
		return err
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return err
	}
	if f.Version != 1 {
		return fmt.Errorf("不支持的版本 %d", f.Version)
	}
	// 过滤非法值（手改/损坏文件不该让服务起不来）。
	pts := make([]Point, 0, len(f.Points))
	for _, p := range f.Points {
		if p.UID == "" || p.TS <= 0 {
			continue
		}
		pts = append(pts, p)
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].TS < pts[j].TS })
	r.points = pts
	// 按账号恢复「上次采样时刻」（各取最后一个原始点），避免重启后立刻补点导致
	// 曲线密集；只认原始点——日粒度点是折叠产物，不代表上次采样时刻。
	for _, p := range pts {
		if p.Day != "" {
			continue
		}
		at := time.Unix(p.TS, 0)
		if old, ok := r.lastSample[p.UID]; !ok || at.After(old) {
			r.lastSample[p.UID] = at
		}
	}
	return nil
}

func (r *Recorder) flush(force bool) {
	if r == nil || r.path == "" {
		return
	}
	r.mu.Lock()
	if !r.dirty && !force {
		r.mu.Unlock()
		return
	}
	snap := file{Version: 1, Saved: time.Now().Format(time.RFC3339), Points: r.points}
	r.dirty = false
	r.mu.Unlock()

	raw, err := json.Marshal(snap)
	if err != nil {
		log.Printf("[balance] 序列化失败: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		log.Printf("[balance] 建目录失败: %v", err)
		return
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("[balance] 写临时文件失败: %v", err)
		return
	}
	if err := os.Rename(tmp, r.path); err != nil {
		log.Printf("[balance] 原子替换失败: %v", err)
	}
}

// Save 立即落盘（关闭前调用）。
func (r *Recorder) Save() { r.flush(true) }

// Start 启动后台防抖落盘与周期折叠。
// 为什么必须显式 Start：Recorder 只在内存累积，缺了这个协程就永远不落盘
// （重启即丢光历史）——这是很容易漏掉的一环。
func (r *Recorder) Start() {
	if r == nil {
		return
	}
	go func() {
		defer close(r.done)
		t := time.NewTicker(flushInterval)
		defer t.Stop()
		for {
			select {
			case <-r.stop:
				r.flush(true)
				return
			case <-t.C:
				r.Rollup(time.Now()) // 顺带做粒度折叠（含超期裁剪）
				r.flush(false)
			}
		}
	}()
}

// Stop 停止后台协程并最终落盘（进程退出前调用）。
func (r *Recorder) Stop() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() {
		close(r.stop)
		<-r.done
	})
}

// ---------------------------------------------------------------- 查询 ----

// Series 单账号的余额序列（供面板画折线）。
type Series struct {
	UID     string  `json:"uid"`
	Name    string  `json:"name,omitempty"`
	Points  []Point `json:"points"`
	Current int64   `json:"current"` // 最新值
	Min     int64   `json:"min"`
	Max     int64   `json:"max"`
	Change  int64   `json:"change"`          // 窗口内净变化（末 - 首）
	Daily   []Daily `json:"daily,omitempty"` // 按日聚合（升序），供每日对照视图
	// Realm 账号域（"cn"/"global"），供前端分域切换。
	// cn 与 global 的积分不同源、不可互换，跨域求和语义不成立，故 UI 必须分域展示。
	// 空 = 未注入（未调 SetRealm），前端归入 other 组。
	Realm string `json:"realm,omitempty"`
}

// Daily 单账号某一天的余额变化（供「每日变化对照」）。
//
// 口径（末值 − 首值）：取该日**首次采样**与**最后采样**之差。
//   - 含当日签到/领奖到账，也含当日消耗；不含跨零点时段的变化。
//   - Full=false 表示该日采样不完整（当天尚未结束、或服务当日中途启用），
//     UI 必须显式标注——否则"今天只赚了一点"会误导（其实还没采完一天）。
type Daily struct {
	Day    string `json:"day"`    // 2006-01-02（本地时区）
	First  int64  `json:"first"`  // 当日首个采样值
	Last   int64  `json:"last"`   // 当日最后一个采样值
	Change int64  `json:"change"` // Last - First
	Min    int64  `json:"min"`
	Max    int64  `json:"max"`
	N      int    `json:"n"`    // 当日采样点数
	Full   bool   `json:"full"` // 是否为完整一天（非今日、且点数达到预期）

	// Consumed / Gained 当日「消耗」与「到账」估算量（相邻采样差值分正负累加）。
	//
	// 口径局限（UI 必须标注，见 cross/dev-principles.md 第十一节）：采样间隔 30
	// 分钟，真实事件远密于此（实测 6 小时 2127 次请求 vs 14 个采样点）。同一
	// 窗口内消耗与到账会被合并成一个净差，故 Consumed 是下界估计：窗口内若
	// 既消耗又到账，只能观测到净值，真实消耗可能更高。它仍比只有 Change 有
	// 信息量——Change 为 +262 时看不出「其实消耗了 124」。
	Consumed int64 `json:"consumed"` // 相邻采样下降量之和，恒 >= 0
	Gained   int64 `json:"gained"`   // 相邻采样上升量之和，恒 >= 0
}

// Snapshot 返回窗口内的余额序列。since 为零值 = 全部。
// 每账号一条序列，点按时间升序。
func (r *Recorder) Snapshot(since time.Time) ([]Series, time.Time) {
	if r == nil {
		return nil, time.Time{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	var cutoff int64
	if !since.IsZero() {
		cutoff = since.Unix()
	}
	byUID := map[string][]Point{}
	for _, p := range r.points {
		if cutoff > 0 && p.TS < cutoff {
			continue
		}
		byUID[p.UID] = append(byUID[p.UID], p)
	}
	out := make([]Series, 0, len(byUID))
	for uid, pts := range byUID {
		sort.Slice(pts, func(i, j int) bool { return pts[i].TS < pts[j].TS })
		s := Series{UID: uid, Name: r.names[uid], Points: pts, Realm: r.realms[uid]}
		if n := len(pts); n > 0 {
			s.Current = pts[n-1].Credits
			// 水位极值与窗口净变化都要把**折叠点的当日首值**算进去：折叠点只剩末值，
			// 只看末值会让窗口首日的水位抬高/压低、净变化失真（见 Point.First）。
			lo, hi := pts[0].Credits, pts[0].Credits
			for _, p := range pts {
				if p.Credits < lo {
					lo = p.Credits
				}
				if p.Credits > hi {
					hi = p.Credits
				}
				if p.Day != "" && p.First != 0 {
					if p.First < lo {
						lo = p.First
					}
					if p.First > hi {
						hi = p.First
					}
				}
			}
			s.Min, s.Max = lo, hi
			// 首点若是折叠点，窗口期初水位应为当日首值而非其末值
			first := pts[0].Credits
			if pts[0].Day != "" && pts[0].First != 0 {
				first = pts[0].First
			}
			s.Change = pts[n-1].Credits - first
			s.Daily = dailyOf(pts, r.clock())
		}
		out = append(out, s)
	}
	// 稳定排序：按最新值降序（主账号在前），再按 uid 保证确定性。
	sort.Slice(out, func(i, j int) bool {
		if out[i].Current != out[j].Current {
			return out[i].Current > out[j].Current
		}
		return out[i].UID < out[j].UID
	})
	return out, time.Now()
}

// dailyOf 把某账号的采样点按本地日历日聚合为每日变化。
//
// 口径：**末值 − 首值**（当日首个采样到最后一个采样）。
// 为何不用「末值 − 昨日末值」：那是跨零点连续口径，会把夜间变化算进今天，
// 与用户对"今天赚了多少"的直觉不符。首末口径更直观、可解释。
//
// 完整性判定：只有**非今日**且**点数达标（>= expectedPerDay）**才标记 Full。
// 今天必然不完整（一天还没过完）；服务中途启用的那天也不完整。
// UI 必须据此标注，否则"今天只涨了一点"会被误读成"今天赚得少"。
func dailyOf(pts []Point, now time.Time) []Daily {
	if len(pts) == 0 {
		return nil
	}
	type acc struct {
		first, last, min, max int64
		n                     int
		consumed, gained      int64 // 相邻采样差值分正负累加（见 Daily 字段注释）
		prev                  int64 // 上一采样值（算相邻差）
		hasPrev               bool
	}
	m := map[string]*acc{}
	var days []string
	for _, p := range pts {
		day := time.Unix(p.TS, 0).In(time.Local).Format(dayLayout)
		a, ok := m[day]
		if !ok {
			// 折叠点自带当日首值：它只剩末值一个水位，first 不能取自身，
			// 否则 Change（末−首）恒为 0，历史日净变化全显示 0。
			first := p.Credits
			if p.Day != "" && p.First != 0 {
				first = p.First
			}
			a = &acc{first: first, last: p.Credits, min: p.Credits, max: p.Credits}
			m[day] = a
			days = append(days, day)
		}
		a.last = p.Credits
		// 相邻采样差值分正负累加：涨 = 到账，跌 = 消耗。跨日时 a 是新建的，
		// hasPrev=false 不会把昨日末值误算进今日。
		if a.hasPrev {
			if d := p.Credits - a.prev; d > 0 {
				a.gained += d
			} else if d < 0 {
				a.consumed += -d
			}
		}
		// 日粒度折叠点自带的消耗/到账是**折叠时算好的当日合计**，必须加进来。
		// 否则折叠日只剩一个点、没有相邻对，结果恒为 0 —— 前端会显示
		// 「30 天前每天消耗 0」，那是数据丢失而非真实无消耗（见 Point.Consumed）。
		// 两种来源不会重复计数：折叠点没有相邻的原始点可比较（hasPrev 为 false
		// 的只有当日首个点），且折叠点本身按天唯一（rollupLocked 保证）。
		a.consumed += p.Consumed
		a.gained += p.Gained
		a.prev = p.Credits
		a.hasPrev = true
		// 折叠点的当日首值也要参与 min/max：折叠只剩末值，当日最低（或最高）
		// 若出现在开盘就会被漏掉。
		lo, hi := p.Credits, p.Credits
		if p.Day != "" && p.First != 0 {
			if p.First < lo {
				lo = p.First
			}
			if p.First > hi {
				hi = p.First
			}
		}
		if lo < a.min {
			a.min = lo
		}
		if hi > a.max {
			a.max = hi
		}
		a.n++
	}
	sort.Strings(days)
	today := now.In(time.Local).Format(dayLayout)
	out := make([]Daily, 0, len(days))
	for _, day := range days {
		a := m[day]
		out = append(out, Daily{
			Day:    day,
			First:  a.first,
			Last:   a.last,
			Change: a.last - a.first,
			Min:    a.min,
			Max:    a.max,
			N:      a.n,

			Consumed: a.consumed,
			Gained:   a.gained,
			Full:     day != today && a.n >= expectedPerDay,
		})
	}
	return out
}

// Describe 返回一行状态摘要（启动日志用）。
func (r *Recorder) Describe() string {
	if r == nil {
		return "off"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	uids := map[string]struct{}{}
	raw, day := 0, 0
	for _, p := range r.points {
		uids[p.UID] = struct{}{}
		if p.Day == "" {
			raw++
		} else {
			day++
		}
	}
	return fmt.Sprintf("%d 点（原始 %d / 日粒度 %d）, %d 账号", len(r.points), raw, day, len(uids))
}
