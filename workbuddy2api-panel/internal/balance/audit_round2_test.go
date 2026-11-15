package balance

// 第二轮代码审计（CODE-AUDIT-FULL.md 10.16 / W2-C2-39~47）的回归用例。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------- W2-C2-39 ----

// 审查 W2-C2-39：注释承诺「同一采样窗口内重复调用只保留最后一个值」，实现却
// 直接丢弃 —— 签到/领奖发生在窗口内时，曲线记的是跳变前的旧值（最长滞后 30 分钟）。
func TestAddMergesWithinSampleWindow(t *testing.T) {
	t0 := time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local)
	r := New("")
	cur := t0
	r.clock = func() time.Time { return cur }

	if !r.Add("u1", 100) {
		t.Fatal("首次采样应被接受")
	}
	// 窗口内第二次：不新增点（返回 false），但水位要更新成最新值。
	cur = t0.Add(10 * time.Minute)
	if r.Add("u1", 900) {
		t.Error("窗口内不应新增采样点（返回 false）")
	}
	if len(r.points) != 1 {
		t.Fatalf("点数 = %d, 期望 1（窗口内合并）", len(r.points))
	}
	if r.points[0].Credits != 900 {
		t.Fatalf("窗口内水位未更新: %d（修前会保留跳变前的 100）", r.points[0].Credits)
	}
	// 面板读到的「当前余额」必须是新值。
	series, _ := r.Snapshot(time.Time{})
	if len(series) != 1 || series[0].Current != 900 {
		t.Fatalf("Snapshot 当前值=%v", series)
	}
}

// 合并只能命中**本账号本窗口**的点：别的账号、以及窗口外的点都不能被改。
func TestAddMergeDoesNotTouchOtherAccounts(t *testing.T) {
	t0 := time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local)
	r := New("")
	cur := t0
	r.clock = func() time.Time { return cur }

	r.Add("A", 100)
	r.Add("B", 200) // B 的点在切片尾部
	cur = t0.Add(5 * time.Minute)
	r.Add("A", 999) // A 在窗口内 → 只改 A 自己的点

	if len(r.points) != 2 {
		t.Fatalf("点数 = %d, 期望 2", len(r.points))
	}
	for _, p := range r.points {
		switch p.UID {
		case "A":
			if p.Credits != 999 {
				t.Errorf("A 应更新为 999, 得到 %d", p.Credits)
			}
		case "B":
			if p.Credits != 200 {
				t.Errorf("B 被误改: %d", p.Credits)
			}
		}
	}
}

// ---------------------------------------------------------------- W2-C2-40 ----

// 审查 W2-C2-40：load 此前只过滤 UID 空与 TS<=0 —— 畸形 Credits 会污染所有
// 后续差值统计（并让累加溢出）。
func TestLoadDropsOutOfRangeCredits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "b.json")
	raw := `{"version":1,"saved":"x","points":[` +
		`{"t":100,"u":"ok","c":5},` +
		`{"t":200,"u":"big","c":9000000000000000000},` +
		`{"t":300,"u":"neg","c":-9000000000000000000}]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	r := New(path)
	if len(r.points) != 1 || r.points[0].UID != "ok" {
		t.Fatalf("越界 Credits 应被丢弃, 得到 %+v", r.points)
	}
}

// 审查 W2-C2-40：点数上限此前只在 Add 里生效，一个手改的超大文件可以绕过它。
func TestLoadCapsPointCount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "b.json")
	var b strings.Builder
	b.WriteString(`{"version":1,"saved":"x","points":[`)
	for i := 0; i < maxPoints+5; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"t":%d,"u":"u%d","c":1}`, i+1, i%7)
	}
	b.WriteString(`]}`)
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	r := New(path)
	if len(r.points) != maxPoints {
		t.Fatalf("点数应被截到 maxPoints=%d, 得到 %d", maxPoints, len(r.points))
	}
	// 保留的是最近的（TS 最大的）那批。
	if last := r.points[len(r.points)-1].TS; last != int64(maxPoints+5) {
		t.Fatalf("应保留最近的点, 末点 TS=%d", last)
	}
}

// 审查 W2-C2-40：文件大小上限（用稀疏文件，不占真实磁盘）。
func TestLoadRejectsOversizeHistoryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "b.json")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxHistoryBytes + 1); err != nil {
		f.Close()
		t.Skipf("本机不支持稀疏文件: %v", err)
	}
	f.Close()

	r := New(path) // 载入失败 → 空历史 + 备份（见 W2-C2-43 用例）
	if len(r.points) != 0 {
		t.Fatalf("超大文件不该被载入, 得到 %d 点", len(r.points))
	}
}

// ---------------------------------------------------------------- W2-C2-41/42 ----

// 审查 W2-C2-41/42：Snapshot 必须在锁内复制、锁外聚合 —— 返回的序列不能与
// 内存切片共享底层数组（否则窗口内合并的就地写会改到已返回的数据）。
func TestSnapshotReturnsIsolatedCopy(t *testing.T) {
	t0 := time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local)
	r := New("")
	cur := t0
	r.clock = func() time.Time { return cur }
	r.Add("u1", 100)

	series, _ := r.Snapshot(time.Time{})
	if len(series) != 1 || series[0].Current != 100 {
		t.Fatalf("首次快照=%+v", series)
	}
	// 窗口内合并（就地改 Credits）不得影响已返回的快照。
	cur = t0.Add(5 * time.Minute)
	r.Add("u1", 777)
	if series[0].Points[0].Credits != 100 || series[0].Current != 100 {
		t.Fatalf("已返回的快照被就地写污染: %+v", series[0])
	}
	// 新快照看到的是新值。
	again, _ := r.Snapshot(time.Time{})
	if again[0].Current != 777 {
		t.Fatalf("新快照未看到合并后的值: %+v", again[0])
	}
}

// ---------------------------------------------------------------- W2-C2-43 ----

// 审查 W2-C2-43：载入失败后任意一次 flush 都会原地覆盖损坏文件。现在先改名备份。
func TestCorruptHistoryIsBackedUp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "b.json")
	if err := os.WriteFile(path, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	r := New(path)
	if len(r.points) != 0 {
		t.Fatalf("损坏文件应按空历史启动, 得到 %d 点", len(r.points))
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("损坏文件应已被改名（否则下一次 flush 会原地覆盖）")
	}
	matches, err := filepath.Glob(filepath.Join(dir, "b.json.corrupt.*"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("应有且只有一个备份文件, got=%v err=%v", matches, err)
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil || string(raw) != `{not json` {
		t.Fatalf("备份内容应原样保留: %q err=%v", raw, err)
	}
}

// ---------------------------------------------------------------- W2-C2-44 ----

// 审查 W2-C2-44：Day 解析失败的畸形点此前被永远保留（裁剪对它失效）。
func TestPruneDropsMalformedDayPoints(t *testing.T) {
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local)
	r := New("")
	r.points = []Point{
		{TS: now.Add(-1 * time.Hour).Unix(), UID: "u1", Credits: 1},                     // 原始点：保留
		{TS: now.Add(-48 * time.Hour).Unix(), UID: "u1", Credits: 2, Day: "not-a-date"}, // 畸形：丢
		{TS: now.Add(-48 * time.Hour).Unix(), UID: "u1", Credits: 3, Day: "2020-01-01"}, // 超期：丢
	}
	r.Rollup(now)
	if len(r.points) != 1 || r.points[0].Day != "" {
		t.Fatalf("畸形/超期日粒度点应被丢弃, 得到 %+v", r.points)
	}
}

// ---------------------------------------------------------------- W2-C2-45 ----

// 审查 W2-C2-45：差值与累加此前无溢出保护 —— 接近 int64 极值的 Credits 会把
// gained/consumed 累加成负数（前端显示负消耗）。
func TestFlowAccumulationClampsOverflow(t *testing.T) {
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local)
	old := now.Add(-40 * 24 * time.Hour)
	r := New("")
	r.points = []Point{
		{TS: old.Unix(), UID: "u1", Credits: 0},
		{TS: old.Add(time.Hour).Unix(), UID: "u1", Credits: 1<<62 - 1},
		{TS: old.Add(2 * time.Hour).Unix(), UID: "u1", Credits: 0},
	}
	r.Rollup(now)

	var folded *Point
	for i := range r.points {
		if r.points[i].Day != "" {
			folded = &r.points[i]
		}
	}
	if folded == nil {
		t.Fatalf("应生成日粒度点: %+v", r.points)
	}
	if folded.Gained <= 0 || folded.Consumed <= 0 {
		t.Fatalf("累加溢出成负值: gained=%d consumed=%d", folded.Gained, folded.Consumed)
	}
}

// ---------------------------------------------------------------- W2-C2-46 ----

// 审查 W2-C2-46：数据目录此前 0755，同机其他用户可枚举文件名。统一 0700。
func TestDataDirPermissionIs0700(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "data")
	r := New(filepath.Join(dir, "balance_history.json"))
	r.Add("u1", 10)
	r.Save()

	st, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o700 {
		t.Fatalf("数据目录权限 = %04o, 期望 0700", perm)
	}
}

// ---------------------------------------------------------------- W2-C2-47 ----

// 审查 W2-C2-47：Snapshot 返回的「数据截至」此前用 time.Now()，与 Add/dailyOf
// 的注入时钟口径不一致。
func TestSnapshotUsesInjectedClock(t *testing.T) {
	fixed := time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local)
	r := New("")
	r.clock = func() time.Time { return fixed }
	r.Add("u1", 1)
	if _, at := r.Snapshot(time.Time{}); !at.Equal(fixed) {
		t.Fatalf("Snapshot 时间 = %v, 期望注入时钟 %v", at, fixed)
	}
}

// 兜底：flush 落盘内容与内存解耦（W2-C2-42）—— 落盘后再就地改内存，文件内容不变。
func TestFlushWritesIndependentCopy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "b.json")
	r := New(path)
	t0 := time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local)
	cur := t0
	r.clock = func() time.Time { return cur }
	r.Add("u1", 100)
	r.Save()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Points) != 1 || f.Points[0].Credits != 100 {
		t.Fatalf("落盘内容 = %+v", f.Points)
	}
	// 就地合并（W2-C2-39）只改内存；文件必须仍是落盘时的 100。
	cur = t0.Add(time.Minute)
	r.Add("u1", 555)
	raw2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f2 file
	if err := json.Unmarshal(raw2, &f2); err != nil {
		t.Fatal(err)
	}
	if f2.Points[0].Credits != 100 {
		t.Fatalf("落盘文件被内存就地写改到了: %+v", f2.Points)
	}
}
