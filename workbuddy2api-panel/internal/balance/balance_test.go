package balance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 固定时钟，避免依赖真实时间。
func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func TestAddRespectsInterval(t *testing.T) {
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local)
	r := New("")
	cur := now
	r.clock = func() time.Time { return cur }

	if !r.Add("u1", 100) {
		t.Fatal("首次采样应被接受")
	}
	// 同一时刻重复调用：节流拒绝
	if r.Add("u1", 101) {
		t.Error("间隔不足应被节流拒绝")
	}
	// 推进 29 分钟：仍拒绝
	cur = now.Add(29 * time.Minute)
	if r.Add("u1", 102) {
		t.Error("29 分钟仍不足采样间隔，应拒绝")
	}
	// 推进 30 分钟：接受
	cur = now.Add(30 * time.Minute)
	if !r.Add("u1", 103) {
		t.Error("满 30 分钟应被接受")
	}
	if got := len(r.points); got != 2 {
		t.Errorf("点数 = %d, 期望 2", got)
	}
}

func TestAddSkipsEmptyUID(t *testing.T) {
	r := New("")
	if r.Add("", 100) {
		t.Error("空 uid 不应记录")
	}
	if len(r.points) != 0 {
		t.Errorf("点数 = %d, 期望 0", len(r.points))
	}
}

// 关键行为：超期原始点折叠为日粒度，每账号每天只留「当日最后一个值」。
func TestRollupFoldsToDailyKeepLast(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.Local)
	r := New("")
	// 手工构造 3 天前的多个点（同一天 3 个，值递增）
	d0 := base
	r.points = []Point{
		{TS: d0.Add(0 * time.Hour).Unix(), UID: "u1", Credits: 500},
		{TS: d0.Add(2 * time.Hour).Unix(), UID: "u1", Credits: 480},
		{TS: d0.Add(4 * time.Hour).Unix(), UID: "u1", Credits: 460}, // 当日最后
	}
	// 现在 = 31 天后 → 超过 detailedKeep(30 天)，上面 3 个点全部超期
	now := base.Add(31 * 24 * time.Hour)
	r.Rollup(now)

	if got := len(r.points); got != 1 {
		t.Fatalf("折叠后点数 = %d, 期望 1（每天 1 个）", got)
	}
	p := r.points[0]
	if p.Day == "" {
		t.Error("折叠点应带 Day 标记")
	}
	if p.Credits != 460 {
		t.Errorf("应保留当日最后一个值 460, 得到 %d", p.Credits)
	}
}

// 多账号折叠互相独立：同名日不同账号不得互相覆盖。
func TestRollupPerUID(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.Local)
	r := New("")
	r.points = []Point{
		{TS: base.Unix(), UID: "u1", Credits: 100},
		{TS: base.Add(time.Hour).Unix(), UID: "u2", Credits: 200},
	}
	r.Rollup(base.Add(72 * time.Hour))
	if got := len(r.points); got != 2 {
		t.Errorf("两个账号应各留一个日点, 得到 %d", got)
	}
}

// 未超期的点不得被折叠（边界：刚过 detailedKeep 才折）。
func TestRollupKeepsRecent(t *testing.T) {
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local)
	r := New("")
	r.points = []Point{{TS: now.Add(-time.Hour).Unix(), UID: "u1", Credits: 999}}
	r.Rollup(now)
	if len(r.points) != 1 || r.points[0].Day != "" {
		t.Error("近期点不应被折叠")
	}
}

// 超期日粒度点应被丢弃（dayKeep 上限）。
func TestRollupPrunesOldDays(t *testing.T) {
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local)
	old := now.Add(-400 * 24 * time.Hour)
	r := New("")
	r.points = []Point{{TS: old.Unix(), UID: "u1", Credits: 1, Day: old.In(time.Local).Format(dayLayout)}}
	r.Rollup(now)
	if len(r.points) != 0 {
		t.Errorf("超期日点应被丢弃, 得到 %d", len(r.points))
	}
}

func TestSnapshotSeriesStats(t *testing.T) {
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local)
	r := New("")
	r.points = []Point{
		{TS: now.Add(-2 * time.Hour).Unix(), UID: "u1", Credits: 1000},
		{TS: now.Add(-1 * time.Hour).Unix(), UID: "u1", Credits: 700},
		{TS: now.Unix(), UID: "u1", Credits: 900},
		{TS: now.Unix(), UID: "u2", Credits: 50},
	}
	r.SetName("u1", "主号")
	got, _ := r.Snapshot(time.Time{})
	if len(got) != 2 {
		t.Fatalf("序列数 = %d, 期望 2", len(got))
	}
	// 按最新值降序：u1(900) 在前
	if got[0].UID != "u1" {
		t.Errorf("首个序列应为 u1, 得到 %s", got[0].UID)
	}
	s := got[0]
	if s.Current != 900 || s.Min != 700 || s.Max != 1000 || s.Change != -100 {
		t.Errorf("统计错误: current=%d min=%d max=%d change=%d", s.Current, s.Min, s.Max, s.Change)
	}
	if s.Name != "主号" {
		t.Errorf("昵称未透出: %q", s.Name)
	}
}

// since 过滤：只返回窗口内的点。
func TestSnapshotSinceFilter(t *testing.T) {
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local)
	r := New("")
	r.points = []Point{
		{TS: now.Add(-48 * time.Hour).Unix(), UID: "u1", Credits: 1},
		{TS: now.Add(-1 * time.Hour).Unix(), UID: "u1", Credits: 2},
	}
	got, _ := r.Snapshot(now.Add(-24 * time.Hour))
	if len(got) != 1 || len(got[0].Points) != 1 {
		t.Fatalf("窗口过滤失败: %+v", got)
	}
	if got[0].Points[0].Credits != 2 {
		t.Errorf("应只保留窗口内的点")
	}
}

// 落盘 + 重载：数据不丢，且 lastSample 以最后点恢复（避免重启后立刻补点）。
func TestPersistAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "balance_history.json")
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local)

	r := New(path)
	r.clock = fixedClock(now)
	r.Add("u1", 1234)
	r.Save()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("文件未生成: %v", err)
	}
	// 权限 0600（含账号数据）
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("权限 = %o, 期望 600", fi.Mode().Perm())
	}

	r2 := New(path)
	got, _ := r2.Snapshot(time.Time{})
	if len(got) != 1 || got[0].Current != 1234 {
		t.Fatalf("重载失败: %+v", got)
	}
	if len(r2.lastSample) == 0 {
		t.Error("重载后每账号的 lastSample 应恢复")
	}
}

// 损坏文件不应让服务起不来（过滤非法点）。
func TestLoadSkipsInvalid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "balance_history.json")
	raw := `{"version":1,"saved":"x","points":[{"t":0,"u":"u1","c":1},{"t":100,"u":"","c":2},{"t":200,"u":"u2","c":3}]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	r := New(path)
	if len(r.points) != 1 {
		t.Errorf("非法点应被过滤, 得到 %d 个", len(r.points))
	}
}

func TestLoadMissingFileStartsEmpty(t *testing.T) {
	r := New(filepath.Join(t.TempDir(), "nope.json"))
	if len(r.points) != 0 {
		t.Error("缺文件应空历史启动")
	}
}

func TestDescribe(t *testing.T) {
	r := New("")
	r.points = []Point{{TS: 1, UID: "u1", Credits: 1}, {TS: 2, UID: "u1", Credits: 2, Day: "x"}}
	if s := r.Describe(); s == "" {
		t.Error("Describe 不应为空")
	}
}

func TestNilRecorderSafe(t *testing.T) {
	var r *Recorder
	if r.Add("u1", 1) {
		t.Error("nil 应安全返回 false")
	}
	r.Rollup(time.Now()) // 不应 panic
	if got, _ := r.Snapshot(time.Time{}); got != nil {
		t.Error("nil 应返回空")
	}
	r.SetName("u1", "x") // 不应 panic
}

// ⭐ 回归网：节流必须按账号独立。
// 曾经的缺陷：lastSample 是全局单个时间戳，而采样由 RunBalanceRefreshNow 并发
// 触达所有账号 → 同一窗口只有"抢到"的账号落点，其余被静默丢弃，症状是
// 「多个账号只有一条线」。单账号时该缺陷不显形，故当初漏测——本用例专门钉住它。
func TestAddThrottleIsPerAccount(t *testing.T) {
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local)
	r := New("")
	cur := now
	r.clock = func() time.Time { return cur }

	// 同一时刻、三个账号依次采样：全部都应被接受（各自独立窗口）。
	for _, uid := range []string{"A", "B", "C"} {
		if !r.Add(uid, 100) {
			t.Fatalf("账号 %s 首次采样应被接受（节流必须按账号独立）", uid)
		}
	}
	if len(r.points) != 3 {
		t.Fatalf("三个账号各应落一点，共 3 点, 得到 %d", len(r.points))
	}

	// 同一账号在窗口内重复：应被拒。
	if r.Add("A", 101) {
		t.Error("同账号在 30 分钟内重复应被节流")
	}

	// 推进 30 分钟：三个账号都应再次落点。
	cur = now.Add(30 * time.Minute)
	for _, uid := range []string{"A", "B", "C"} {
		if !r.Add(uid, 200) {
			t.Errorf("满 30 分钟后账号 %s 应可再次采样", uid)
		}
	}
	if len(r.points) != 6 {
		t.Errorf("应累计 6 点, 得到 %d", len(r.points))
	}
	// 且每条序列各 2 点（前端一条线一账号的依据）
	got, _ := r.Snapshot(time.Time{})
	if len(got) != 3 {
		t.Fatalf("应有 3 条序列, 得到 %d", len(got))
	}
	for _, s := range got {
		if len(s.Points) != 2 {
			t.Errorf("账号 %s 应有 2 点, 得到 %d", s.UID, len(s.Points))
		}
	}
}

// 回归网：重载后每账号的节流时刻各自恢复（否则重启后会给某账号连补多点）。
func TestReloadRestoresPerAccountThrottle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "b.json")
	t0 := time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local)

	r := New(path)
	cur := t0
	r.clock = func() time.Time { return cur }
	r.Add("A", 1000)
	cur = t0.Add(10 * time.Minute)
	r.Add("B", 2000) // B 晚 10 分钟
	r.Save()

	r2 := New(path)
	if len(r2.points) != 2 {
		t.Fatalf("重载应得 2 点, 得到 %d", len(r2.points))
	}
	// A 的最后采样是 t0，距 t0+10min 仅 10 分钟 → 应被节流
	cur2 := t0.Add(10 * time.Minute)
	r2.clock = func() time.Time { return cur2 }
	if r2.Add("A", 1001) {
		t.Error("A 距上次仅 10 分钟，重载后应仍被节流")
	}
	// B 的最后采样就是此刻 → 也应被节流
	if r2.Add("B", 2001) {
		t.Error("B 恰在采样时刻，应被节流")
	}
	// 满 30 分钟后放行
	cur2 = t0.Add(30 * time.Minute)
	if !r2.Add("A", 1002) {
		t.Error("A 满 30 分钟应放行")
	}
}

// 每日聚合：口径 = 末值 − 首值；跨日要正确切分。
func TestDailyOfBasic(t *testing.T) {
	d1 := time.Date(2026, 9, 16, 8, 0, 0, 0, time.Local)
	d2 := time.Date(2026, 9, 17, 8, 0, 0, 0, time.Local)
	pts := []Point{
		{TS: d1.Unix(), Credits: 1000},
		{TS: d1.Add(30 * time.Minute).Unix(), Credits: 900},
		{TS: d1.Add(time.Hour).Unix(), Credits: 800}, // 9/16 末值 800 → 变化 -200
		{TS: d2.Unix(), Credits: 1200},
		{TS: d2.Add(30 * time.Minute).Unix(), Credits: 1100}, // 9/17 末值 1100 → 变化 -100
	}
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.Local) // 站在 9/18 看，两天都已过完
	got := dailyOf(pts, now)
	if len(got) != 2 {
		t.Fatalf("应 2 天, 得到 %d", len(got))
	}
	if got[0].Day != "2026-09-16" || got[1].Day != "2026-09-17" {
		t.Errorf("日期错误: %s / %s", got[0].Day, got[1].Day)
	}
	if got[0].First != 1000 || got[0].Last != 800 || got[0].Change != -200 {
		t.Errorf("9/16 聚合错误: first=%d last=%d change=%d", got[0].First, got[0].Last, got[0].Change)
	}
	if got[0].Min != 800 || got[0].Max != 1000 {
		t.Errorf("9/16 min/max 错误: %d/%d", got[0].Min, got[0].Max)
	}
	if got[1].Change != -100 {
		t.Errorf("9/17 变化应为 -100, 得到 %d", got[1].Change)
	}
}

// ⭐ 关键边界：今日必须标记为「不完整」——否则"今天只涨一点"会被误读。
func TestDailyOfTodayNotFull(t *testing.T) {
	now := time.Date(2026, 9, 17, 10, 0, 0, 0, time.Local)
	// 今日只采了 3 点（远少于 expectedPerDay=40）
	pts := []Point{
		{TS: now.Add(-2 * time.Hour).Unix(), Credits: 500},
		{TS: now.Add(-time.Hour).Unix(), Credits: 450},
		{TS: now.Unix(), Credits: 400},
	}
	got := dailyOf(pts, now)
	if len(got) != 1 {
		t.Fatalf("应 1 天, 得到 %d", len(got))
	}
	if got[0].Full {
		t.Error("今日必须标记为不完整（Full=false），否则读数会被误读")
	}
}

// 历史完整日（点数达标）应标记 Full=true。
func TestDailyOfPastFullDay(t *testing.T) {
	day := time.Date(2026, 9, 16, 0, 0, 0, 0, time.Local)
	var pts []Point
	for i := 0; i < 48; i++ { // 全天 48 点
		pts = append(pts, Point{TS: day.Add(time.Duration(i) * 30 * time.Minute).Unix(), Credits: 1000 - int64(i)})
	}
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.Local)
	got := dailyOf(pts, now)
	if len(got) != 1 || !got[0].Full {
		t.Errorf("48 点的历史日应 Full=true, 得到 %+v", got)
	}
}

// 点数不足的历史日（服务中途启用）也应标记不完整。
func TestDailyOfPastPartialDay(t *testing.T) {
	day := time.Date(2026, 9, 16, 20, 0, 0, 0, time.Local)
	pts := []Point{
		{TS: day.Unix(), Credits: 100},
		{TS: day.Add(30 * time.Minute).Unix(), Credits: 90},
	}
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.Local)
	got := dailyOf(pts, now)
	if len(got) != 1 {
		t.Fatalf("应 1 天, 得到 %d", len(got))
	}
	if got[0].Full {
		t.Error("仅 2 点的历史日应标记不完整")
	}
}

func TestDailyOfEmpty(t *testing.T) {
	if got := dailyOf(nil, time.Now()); got != nil {
		t.Errorf("空输入应返回 nil, 得到 %v", got)
	}
}

// Snapshot 应把 daily 一并带出（前端每日对照视图的数据来源）。
func TestSnapshotIncludesDaily(t *testing.T) {
	base := time.Date(2026, 9, 16, 8, 0, 0, 0, time.Local)
	r := New("")
	r.points = []Point{
		{TS: base.Unix(), UID: "u1", Credits: 1000},
		{TS: base.Add(time.Hour).Unix(), UID: "u1", Credits: 1200},
	}
	got, _ := r.Snapshot(time.Time{})
	if len(got) != 1 {
		t.Fatalf("应 1 序列")
	}
	if len(got[0].Daily) != 1 {
		t.Fatalf("应带出 1 天 daily, 得到 %d", len(got[0].Daily))
	}
	if got[0].Daily[0].Change != 200 {
		t.Errorf("daily 变化应为 +200, 得到 %d", got[0].Daily[0].Change)
	}
}

// TestSetRealmAppearsInSnapshot realm 注入后必须出现在 Snapshot 的每条序列上，
// 供前端分域切换（cn 与 global 的积分不可跨域求和）。
func TestSetRealmAppearsInSnapshot(t *testing.T) {
	r := New("")
	now := time.Date(2026, 9, 17, 10, 0, 0, 0, time.Local)
	r.clock = func() time.Time { return now }
	r.SetRealm("uid-cn", "cn")
	r.SetRealm("uid-global", "global")
	r.Add("uid-cn", 100)
	r.Add("uid-global", 200)

	series, _ := r.Snapshot(time.Time{})
	got := map[string]string{}
	for _, s := range series {
		got[s.UID] = s.Realm
	}
	if got["uid-cn"] != "cn" {
		t.Errorf("uid-cn realm = %q, want cn", got["uid-cn"])
	}
	if got["uid-global"] != "global" {
		t.Errorf("uid-global realm = %q, want global", got["uid-global"])
	}
}

// TestSetRealmEmptyIgnored 空 realm 不得覆盖已记录的值
// （上游偶发空值不该把已分好组的账号打回 other）。
func TestSetRealmEmptyIgnored(t *testing.T) {
	r := New("")
	r.SetRealm("uid1", "cn")
	r.SetRealm("uid1", "")   // 空值：忽略
	r.SetRealm("", "global") // 空 uid：忽略

	series, _ := r.Snapshot(time.Time{})
	r.Add("uid1", 1)
	series, _ = r.Snapshot(time.Time{})
	if len(series) != 1 {
		t.Fatalf("series len = %d, want 1", len(series))
	}
	if series[0].Realm != "cn" {
		t.Errorf("realm = %q, want cn（空值不应覆盖）", series[0].Realm)
	}
}

// TestSetRealmNilSafe nil Recorder 上调用 SetRealm 不得 panic（防御）。
func TestSetRealmNilSafe(t *testing.T) {
	var r *Recorder
	r.SetRealm("uid", "cn") // 不应 panic
}

// TestSetRealmNotPersisted realm 是展示元数据，不入盘；重启后由面板重新注入。
func TestSetRealmNotPersisted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bal.json")
	r := New(path)
	r.SetRealm("uid1", "cn")
	r.Add("uid1", 500)
	r.flush(true) // force：立即落盘（正常路径是 30s 防抖）
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(raw), `"cn"`) {
		t.Errorf("realm 不应入盘，实际文件含 cn：\n%s", raw)
	}
}

// TestDailyConsumedGained 消耗/到账必须由相邻采样差值分正负累加得出，
// 且与净变化满足恒等式：Change == Gained - Consumed。
//
// 该恒等式是本字段的正确性判据：若破坏（如把跨日差值也累加进来、
// 或漏算首点），三者会对不上。见 dev-principles.md 第十节（不得产出
// 语义上不可能的值：消耗/到账都恒 >= 0）。
func TestDailyConsumedGained(t *testing.T) {
	r := New("")
	// 逐点推进时钟，全部落在同一天内
	base := time.Date(2026, 9, 17, 10, 0, 0, 0, time.Local)
	cur := base
	r.clock = func() time.Time { return cur }
	step := 31 * time.Minute

	// 100 -> 130 (+30 到账) -> 110 (-20 消耗) -> 150 (+40 到账) -> 120 (-30 消耗)
	for _, v := range []int64{100, 130, 110, 150, 120} {
		r.Add("u1", v)
		cur = cur.Add(step)
	}

	series, _ := r.Snapshot(time.Time{})
	if len(series) != 1 || len(series[0].Daily) != 1 {
		t.Fatalf("want 1 series with 1 day, got %+v", series)
	}
	d := series[0].Daily[0]
	if d.Gained != 70 { // 30 + 40
		t.Errorf("Gained = %d, want 70", d.Gained)
	}
	if d.Consumed != 50 { // 20 + 30
		t.Errorf("Consumed = %d, want 50", d.Consumed)
	}
	if d.Change != d.Gained-d.Consumed {
		t.Errorf("恒等式破坏：Change=%d != Gained(%d) - Consumed(%d)", d.Change, d.Gained, d.Consumed)
	}
	if d.Consumed < 0 || d.Gained < 0 {
		t.Errorf("消耗/到账不得为负：consumed=%d gained=%d", d.Consumed, d.Gained)
	}
}

// TestDailyConsumedNotCrossDay 跨日的相邻点不得互相累加：
// 昨日末值 → 今日首值 的差值属于隔夜变化，不是今天的消耗/到账。
func TestDailyConsumedNotCrossDay(t *testing.T) {
	r := New("")
	cur := time.Date(2026, 9, 16, 23, 30, 0, 0, time.Local)
	r.clock = func() time.Time { return cur }
	step := 31 * time.Minute

	r.Add("u1", 100)    // 09-16 23:30
	cur = cur.Add(step) // -> 09-17 00:01
	r.Add("u1", 500)    // 跨日大涨 400：不应算进任一天的 gained
	cur = cur.Add(step)
	r.Add("u1", 480) // 09-17 内跌 20

	series, _ := r.Snapshot(time.Time{})
	if len(series) != 1 {
		t.Fatalf("want 1 series, got %d", len(series))
	}
	days := series[0].Daily
	if len(days) != 2 {
		t.Fatalf("want 2 days, got %d: %+v", len(days), days)
	}
	for _, d := range days {
		if d.Day == "2026-09-16" {
			if d.Gained != 0 || d.Consumed != 0 {
				t.Errorf("09-16 不该有流量（单点无相邻）：gained=%d consumed=%d", d.Gained, d.Consumed)
			}
		}
		if d.Day == "2026-09-17" {
			// 该日只有 500 -> 480 一对，跌 20；500 相对昨日 100 的 +400 不算今日到账
			if d.Gained != 0 {
				t.Errorf("09-17 Gained = %d, want 0（跨日涨幅不该算今日到账）", d.Gained)
			}
			if d.Consumed != 20 {
				t.Errorf("09-17 Consumed = %d, want 20", d.Consumed)
			}
		}
	}
}

// TestDailyZeroFlowIsZero 余额全程不变时，消耗/到账必须都是 0（不是负值、不是残留）。
func TestDailyZeroFlowIsZero(t *testing.T) {
	r := New("")
	cur := time.Date(2026, 9, 17, 10, 0, 0, 0, time.Local)
	r.clock = func() time.Time { return cur }
	for i := 0; i < 3; i++ {
		r.Add("u1", 349)
		cur = cur.Add(31 * time.Minute)
	}
	series, _ := r.Snapshot(time.Time{})
	d := series[0].Daily[0]
	if d.Consumed != 0 || d.Gained != 0 || d.Change != 0 {
		t.Errorf("余额恒定时应为全零：%+v", d)
	}
}

// TestRollupKeepsFlows 折叠不得丢失当日的消耗/到账。
//
// 事故形态（本测试存在的原因）：折叠点每天只剩一个水位，dailyOf 按「相邻
// 采样差值累加」算消耗/到账时拿不到任何相邻对 → 结果恒为 0。前端把这段
// 历史显示为「30 天前每天消耗 0」，数字看着正常，但**是错的**（数据被丢掉了）。
// 这类"静默归零"比报错危险：没有异常、没有空值，只有错的 0。
func TestRollupKeepsFlows(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.Local)
	r := New("")
	// 同日 5 点：500 -> 480（跌 20）-> 600（涨 120）-> 600 -> 550（跌 50）
	// 期望：consumed = 20 + 50 = 70，gained = 120
	r.points = []Point{
		{TS: base.Add(0 * time.Hour).Unix(), UID: "u1", Credits: 500},
		{TS: base.Add(2 * time.Hour).Unix(), UID: "u1", Credits: 480},
		{TS: base.Add(4 * time.Hour).Unix(), UID: "u1", Credits: 600},
		{TS: base.Add(6 * time.Hour).Unix(), UID: "u1", Credits: 600},
		{TS: base.Add(8 * time.Hour).Unix(), UID: "u1", Credits: 550}, // 当日最后
	}
	now := base.Add(31 * 24 * time.Hour)
	r.Rollup(now)

	if len(r.points) != 1 {
		t.Fatalf("折叠后点数 = %d, 期望 1", len(r.points))
	}
	p := r.points[0]
	if p.Credits != 550 {
		t.Errorf("水位应保留当日最后值 550, 得到 %d", p.Credits)
	}
	if p.Consumed != 70 {
		t.Errorf("折叠点 Consumed = %d, want 70（20+50）", p.Consumed)
	}
	if p.Gained != 120 {
		t.Errorf("折叠点 Gained = %d, want 120", p.Gained)
	}

	// 端到端：dailyOf 必须把折叠点自带的值算进当日汇总。
	// 折叠后的点落在 30 天前的窗口里，用 Snapshot 全量取。
	series, _ := r.Snapshot(time.Time{})
	if len(series) != 1 || len(series[0].Daily) != 1 {
		t.Fatalf("期望 1 账号 1 天, 得到 %+v", series)
	}
	d := series[0].Daily[0]
	if d.Consumed != 70 || d.Gained != 120 {
		t.Errorf("Daily 未纳入折叠点流量：consumed=%d gained=%d, want 70/120（折叠日流量被丢掉会显示为 0）",
			d.Consumed, d.Gained)
	}
	// Change 口径不变：末值 − 首值 = 550 − 500 = 50（首值来自原始点，故仍正确）
	if d.Change != 50 {
		t.Errorf("Change = %d, want 50（首末口径不因折叠而变）", d.Change)
	}
}

// TestRollupFlowsPerDayIndependent 折叠时按日分组，消耗/到账不得串天。
func TestRollupFlowsPerDayIndependent(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.Local)
	r := New("")
	r.points = []Point{
		{TS: base.Unix(), UID: "u1", Credits: 1000},
		{TS: base.Add(time.Hour).Unix(), UID: "u1", Credits: 900},       // D1 跌 100
		{TS: base.Add(25 * time.Hour).Unix(), UID: "u1", Credits: 1500}, // D2 首点（不跨日累加）
		{TS: base.Add(26 * time.Hour).Unix(), UID: "u1", Credits: 1400}, // D2 跌 100
	}
	r.Rollup(base.Add(40 * 24 * time.Hour))

	byDay := map[string]Point{}
	for _, p := range r.points {
		byDay[p.Day] = p
	}
	d1, d2 := byDay["2026-08-01"], byDay["2026-08-02"]
	if d1.Consumed != 100 || d1.Gained != 0 {
		t.Errorf("D1 = %+v, want consumed=100 gained=0", d1)
	}
	// D2 的 1500 − 900 = 600 是隔夜涨幅，不得算作 D2 的到账
	if d2.Gained != 0 {
		t.Errorf("D2 Gained = %d, want 0（跨日涨幅不该进折叠值）", d2.Gained)
	}
	if d2.Consumed != 100 {
		t.Errorf("D2 Consumed = %d, want 100", d2.Consumed)
	}
}
