// audit_p2_test.go CODE-AUDIT-FULL §10.12（pool 账号池）第二轮修复的回归钉。
//
// 每个用例对应一条审查编号，且都构造成「修复前必红」的形态：
//
//	C5-01 触底号走无模型粘性直取仍被选中
//	C5-02 可用集合里出现选号侧永不选的触底号
//	C5-11 抽签阶段在子集上重算窗口用量均值（两阶段基准不一致）
//	C5-13 余额分母在优先级收窄后算（层内分母变小）
//	C5-14 同一时钟刻度内两次洗牌得到同一置换
//	C5-18 短名单只留 5 个 → 第 6~8 名永远进不了抽签
//	C5-19 每候选每次 pick 都扫模型表（写锁内 O(账号×模型)）
//	C5-20 占比抽签不复检在途满额（单号并发可突破上限）
//	C5-30 过期冷却墓碑无保质期（每个用过的 (号,模型) 永久占内存）
//	C5-32 成本观测时间戳在未来 → 永不过期
//	C5-52 恢复路径不钳负余额
//	C5-65 探针结果按字节截断，切断 UTF-8
//	C5-72 策略游标 map 随客户端模型名无界增长
//	C5-73 纯读 Strategy() 抢选号写锁
//	C5-74 顺序耗尽被防撞号兜底借走别的号
//	C5-84 分池可用集合缺保底/保留积分判定
package pool

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// testAcct 造一个健康账号（凭证齐全、余额充足）。
func testAcct(t *testing.T, p *Pool, uid string, credits int64) *auth.Auth {
	t.Helper()
	a := &auth.Auth{UID: uid, AccessToken: "at-" + uid, RefreshToken: "rt", ExpiresAt: 9999999999}
	p.Add(a)
	p.SetCredits(uid, credits, credits)
	return a
}

// fracRand 注入确定性随机源：每次抽签返回 total*frac（0..1）。
// 这样既能落在任意候选的权重区间里，又不受 rand 全局状态影响。
func fracRand(p *Pool, frac float64) {
	p.randInt64N = func(n int64) int64 {
		v := int64(float64(n) * frac)
		if v >= n {
			v = n - 1
		}
		if v < 0 {
			v = 0
		}
		return v
	}
}

// TestPickByUIDRespectsCreditFloor 无模型粘性直取必须有积分保底（审查 C5-01）。
//
// 修复前 PickByUID 只判 healthy + inFlightFull（PickByUIDForModel 才有保底），
// 走无模型粘性路由的请求可以持续选中已触底账号，一路打穿上游余额撞 402。
func TestPickByUIDRespectsCreditFloor(t *testing.T) {
	p := New("")
	p.SetCreditFloor(100) // 保留积分保持默认 0：本用例只隔离「积分保底」这一条判据

	// u1：实测过收费模型（消耗型）+ 已触底 → 必须被拦。
	testAcct(t, p, "u1", 1000)
	p.NoteModelCost("u1", "glm-5.3", 5, 1000) // per1k = 5 > 0
	p.SetCredits("u1", 5, 1000)

	// u2：只观测到免费模型 + 同样触底 → 不该拦（保底是「把余额留给免费模型」）。
	// 余额取 60：仍低于保底线 100，但高于保留积分阈值 50，避免被 reserve 规则误伤。
	testAcct(t, p, "u2", 1000)
	p.NoteModelCost("u2", "free-model", 0, 1000) // per1k = 0
	p.SetCredits("u2", 60, 1000)

	if a := p.PickByUID("u1"); a != nil {
		t.Fatalf("触底且实测过收费模型的号仍被无模型粘性直取选中：%s（审查 C5-01）", a.UID)
	}
	if a := p.PickByUID("u2"); a == nil {
		t.Fatal("纯免费观测的触底号被误拦：保底只该拦「消耗型」账号")
	}
}

// TestPickByUIDRespectsReserve 无模型粘性直取同样要守保留积分（审查 C5-01 保留积分口径）。
//
// 与保底分开建池：两个判据叠在一起时，保底先拦，reserve 分支就测不到了。
func TestPickByUIDRespectsReserve(t *testing.T) {
	p := New("")
	p.SetReserveCredits(50)
	testAcct(t, p, "reserved", 1000)
	p.SetCredits("reserved", 10, 1000) // 余额已知且 <= 阈值 → 停单
	if a := p.PickByUID("reserved"); a != nil {
		t.Fatalf("低于保留积分的号仍被粘性直取选中：%s（审查 C5-01 保留积分口径）", a.UID)
	}
}

// TestAvailableUIDsExcludesFloorBlocked 可用集合必须与选号同口径（审查 C5-02）。
//
// 修复前 AvailableUIDs 只判 healthy+inFlightFull，会话层据此**绑定**候选，
// 于是每请求都要先命中触底号、再被 handler 打回（unbind 抖动 + 一条 WARN/请求）。
func TestAvailableUIDsExcludesFloorBlocked(t *testing.T) {
	p := New("")
	p.SetCreditFloor(100)
	testAcct(t, p, "poor", 1000)
	p.NoteModelCost("poor", "glm-5.3", 5, 1000)
	p.SetCredits("poor", 5, 1000)
	testAcct(t, p, "rich", 5000)

	got := p.AvailableUIDs()
	if len(got) != 1 || got[0] != "rich" {
		t.Fatalf("AvailableUIDs=%v，触底号不该出现在会话绑定候选里（审查 C5-02）", got)
	}
}

// TestRealmAvailableUIDsRespectFloor 分池可用集合（生产调用方）同样要判保底（审查 C5-84）。
func TestRealmAvailableUIDsRespectFloor(t *testing.T) {
	p := New("")
	p.SetCreditFloor(100)
	p.SetReserveCredits(50)
	testAcct(t, p, "poor", 1000)
	p.NoteModelCost("poor", "glm-5.3", 5, 1000)
	p.SetCredits("poor", 5, 1000)
	testAcct(t, p, "reserved", 1000)
	p.SetCredits("reserved", 10, 1000)
	testAcct(t, p, "ok", 5000)

	for name, got := range map[string][]string{
		"AvailableUIDsForRealm":         p.AvailableUIDsForRealm(""),
		"AvailableUIDsForModelRealm":    p.AvailableUIDsForModelRealm("", ""),
		"WeightedAvailableUIDsForModel": p.WeightedAvailableUIDsForModelRealm("", ""),
	} {
		if len(got) != 1 || got[0] != "ok" {
			t.Fatalf("%s=%v，触底/保留积分号不该出现在可用集合里（审查 C5-84）", name, got)
		}
	}
}

// TestPickWeightedUsesPassedUsageCtx 抽签必须复用截断阶段的窗口用量分母（审查 C5-11）。
//
// 构造让「全集口径」与「子集重算」给出相反结论的边界：
//
//	全集 mean=2000/3 → hot 的 div=2.5 → 权重 11/2.5=4.4，total=15.4
//	子集 mean=1000/2 → hot 的 div=3.0 → 权重 11/3=3.67，total=14.67
//
// 取 frac=0.27：全集口径落在 hot（4.158 < 4.4），子集口径落到 cold（3.96 > 3.67）。
func TestPickWeightedUsesPassedUsageCtx(t *testing.T) {
	p := New("")
	p.SetBalanceWindow(time.Hour)
	testAcct(t, p, "hot", 100)
	testAcct(t, p, "cold", 100)
	now := time.Now()
	p.mu.Lock()
	hot := p.byUID["hot"]
	cold := p.byUID["cold"]
	for _, e := range []*entry{hot, cold} {
		e.lastUsed = now // 闲置补偿归零，权重只由 credits 项决定
		e.modelCost = map[string]modelCostEntry{
			"m1": {CostPer1k: 1, LastSeen: now, WinStart: now.Add(-time.Minute)},
		}
	}
	hot.modelCost["m1"] = modelCostEntry{
		CostPer1k: 1, LastSeen: now, WinStart: now.Add(-time.Minute), WinTokens: 1000,
	}
	p.mu.Unlock()

	// 截断阶段在「本层全集 ws」上算出的上下文（含第三个号：1000 + 1000 + 0）。
	full := usageCtx{model: "m1", n: 3, sum: 2000}
	fracRand(p, 0.27)
	if got := p.pickWeighted([]*entry{hot, cold}, "m1", 100, full); got != hot {
		t.Fatalf("抽签用了子集重算的分母（选中 %s，应 hot）：两阶段权重基准不一致（审查 C5-11）", got.a.UID)
	}
}

// TestPickCreditDenominatorUsesPrePrioritySet 余额分母必须在优先级收窄前算（审查 C5-13）。
//
// u1/u2 配了优先级 1（先烧层），u3 无优先级但余额最大。
// 全集分母 100000：w(u1)=1.1、w(u2)=1.001 → frac 0.7 落在 u2；
// 收窄后分母 1000：w(u1)=11、w(u2)=1.1 → frac 0.7 落在 u1。
func TestPickCreditDenominatorUsesPrePrioritySet(t *testing.T) {
	p := New("")
	p.SetAccountRouting(map[string]int{"u1": 1, "u2": 1}, nil)
	testAcct(t, p, "u1", 1000)
	testAcct(t, p, "u2", 10)
	testAcct(t, p, "u3", 100000)
	now := time.Now()
	p.mu.Lock()
	for _, e := range p.byUID {
		e.lastUsed = now.Add(-time.Second) // 跨过 minPickGap，闲置补偿≈0
	}
	p.mu.Unlock()

	fracRand(p, 0.7)
	got := p.Pick()
	if got == nil {
		t.Fatal("选号返回 nil")
	}
	if got.UID != "u2" {
		t.Fatalf("选中 %s，want u2：分母应在优先级收窄前用原始 healthy 候选算（审查 C5-13）", got.UID)
	}
}

// TestShuffleSeedDistinctWithinSameTick 同一时刻的两次洗牌必须不同（审查 C5-14）。
//
// 修复前只用 now.UnixNano() 播种，粗时钟平台（Windows ~0.5ms）同一 tick 内多次
// pick 得到同一置换 → 等权重池在并发突发时排序固定，截断饿死回归。
func TestShuffleSeedDistinctWithinSameTick(t *testing.T) {
	p := New("")
	now := time.Now()
	const span = int64(1) << 62
	a := p.shuffleRNG(now, 1).Int64N(span)
	b := p.shuffleRNG(now, 1).Int64N(span)
	if a == b {
		t.Fatalf("同一 tick 两次洗牌得到同一序列（%d）：缺少进程级自增序号（审查 C5-14）", a)
	}
	c := p.shuffleRNG(now, 2).Int64N(span)
	if a == c {
		t.Fatalf("不同 salt 的洗牌得到同一序列（%d）（审查 C5-14）", a)
	}
}

// TestShortlistPromotesIdleTail 久置长尾号必须能进短名单（审查 C5-18）。
//
// 9 个号权重严格递减（u1 最高、u9 最低）。u9 余额最低但已闲置 45 分钟 ——
// 纯权重截断时它永远排在第 9，短名单只有前 5，抽签取最后一个候选 → 命中 u5。
// 长尾补偿位把它补进短名单后 → 命中 u9。
//
// 反向约束：同池里「刚被用过」的 u6~u8 不得占补偿位（否则补偿退化成扩大截断，
// 会破坏 TestPickWeightedTopFiveOnly 钉住的「前 5 之外不参与权重抽签」语义）。
func TestShortlistPromotesIdleTail(t *testing.T) {
	p := New("")
	now := time.Now()
	for i := 1; i <= 9; i++ {
		testAcct(t, p, fmt.Sprintf("u%d", i), int64(1000-100*i))
	}
	p.mu.Lock()
	for _, e := range p.byUID {
		e.lastUsed = now.Add(-time.Second)
	}
	p.byUID["u9"].lastUsed = now.Add(-45 * time.Minute) // 久置长尾
	p.mu.Unlock()

	p.randInt64N = func(n int64) int64 { return n - 1 } // 取短名单最末位的候选
	got := p.Pick()
	if got == nil {
		t.Fatal("选号返回 nil")
	}
	if got.UID != "u9" {
		t.Fatalf("选中 %s，want u9：久置长尾号没拿到补偿位，短名单仍是纯权重前 5（审查 C5-18）", got.UID)
	}
}

// TestPruneThrottledPerEntry 惰性清理必须按 lastPrune 节流（审查 C5-19）。
//
// 修复前每个候选每次 pick 都在写锁内扫 modelCooldowns / modelCost（O(账号×模型)），
// 高 QPS + 多模型时选号本身成为锁热点。节流后 30s 内不再重复扫描。
func TestPruneThrottledPerEntry(t *testing.T) {
	p := New("")
	testAcct(t, p, "u1", 1000)

	expired := modelCooldown{Until: time.Now().Add(-time.Minute)}
	p.mu.Lock()
	e := p.byUID["u1"]
	e.modelCooldowns = map[string]modelCooldown{"stale-1": expired}
	p.mu.Unlock()

	p.Pick() // 首次：lastPrune 零值 → 立即清一次
	p.mu.RLock()
	_, firstGone := p.byUID["u1"].modelCooldowns["stale-1"]
	pruned := !p.byUID["u1"].lastPrune.IsZero()
	p.mu.RUnlock()
	if !pruned || firstGone {
		t.Fatal("首次 pick 应清理过期模型冷却并记 lastPrune")
	}

	// 再塞一条过期条目：30s 内不该被扫（节流生效）。
	p.mu.Lock()
	p.byUID["u1"].modelCooldowns["stale-2"] = expired
	p.mu.Unlock()
	p.Pick()
	p.mu.RLock()
	_, stillThere := p.byUID["u1"].modelCooldowns["stale-2"]
	p.mu.RUnlock()
	if !stillThere {
		t.Fatal("30s 内第二次 pick 仍在扫描清理：节流失效（审查 C5-19）")
	}

	// 把 lastPrune 推到 31s 前：下一次 pick 必须清理。
	p.mu.Lock()
	p.byUID["u1"].lastPrune = time.Now().Add(-31 * time.Second)
	p.mu.Unlock()
	p.Pick()
	p.mu.RLock()
	_, stillThere = p.byUID["u1"].modelCooldowns["stale-2"]
	p.mu.RUnlock()
	if stillThere {
		t.Fatal("超过节流间隔仍未清理（审查 C5-19）")
	}
}

// TestPickByShareSkipsInFlightFull 占比抽签必须复检在途满额（审查 C5-20）。
//
// 修复前 pickByShare 完全信任调用方已过滤：占比号是唯一候选时单号并发可打满，
// 突破 max_in_flight —— 正是上游 6004 / WAF 403 的触发条件。
func TestPickByShareSkipsInFlightFull(t *testing.T) {
	p := New("")
	p.SetMaxInFlight(1)
	p.SetAccountRouting(nil, map[string]float64{"a": 0.5, "b": 0.5})
	testAcct(t, p, "a", 1000)
	testAcct(t, p, "b", 1000)
	now := time.Now()

	p.mu.Lock()
	full := p.byUID["a"]
	ok := p.byUID["b"]
	full.inFlight.Add(1) // 满额
	p.mu.Unlock()

	for _, frac := range []float64{0, 0.1, 0.5, 0.9, 0.999} {
		f := frac
		fracRand(p, f)
		if got := p.pickByShare([]*entry{full, ok}, now); got != ok {
			t.Fatalf("frac=%.3f 选中满额号 %s：占比抽签没复检 inFlightFull（审查 C5-20）", f, got.a.UID)
		}
	}

	// 占比号全部满额 → 返回 nil，让位给下面的加权/策略路径。
	p.mu.Lock()
	ok.inFlight.Add(1)
	p.mu.Unlock()
	if got := p.pickByShare([]*entry{full, ok}, now); got != nil {
		t.Fatalf("全部满额时仍返回 %s，应 nil（审查 C5-20）", got.a.UID)
	}
}

// TestModelCooldownTombstoneExpires 过期冷却的墓碑必须有保质期（审查 C5-30）。
//
// 修复前墓碑（只留 Hits）不含时间戳，下次 prune 又命中同一分支写回同一结构 →
// 每个用过的 (号, 模型) 永久占一条内存，与「防止 map 无限膨胀」的注释相反。
func TestModelCooldownTombstoneExpires(t *testing.T) {
	now := time.Now()
	e := &entry{modelCooldowns: map[string]modelCooldown{
		"m1": {Until: now.Add(-time.Minute), Reason: "11102 model not found", Hits: 2},
		"m2": {Until: now.Add(-time.Minute), Reason: "6004"},
	}}
	e.pruneExpiredModelCooldowns(now)
	mc, ok := e.modelCooldowns["m1"]
	if !ok || mc.Hits != 2 || mc.PrunedAt.IsZero() {
		t.Fatalf("11102 墓碑形态不对（Hits 必须保留、PrunedAt 必须记下）：%+v ok=%v", mc, ok)
	}
	if _, ok := e.modelCooldowns["m2"]; ok {
		t.Fatal("无 Hits 的过期条目应直接删除")
	}
	e.pruneExpiredModelCooldowns(now.Add(modelBlockTombstoneTTL + time.Minute))
	if _, ok := e.modelCooldowns["m1"]; ok {
		t.Fatal("墓碑超过保质期仍未删除：map 无界增长（审查 C5-30）")
	}
}

// TestModelCostFutureLastSeenTreatedStale 时间戳在未来的成本观测视为过期（审查 C5-32）。
func TestModelCostFutureLastSeenTreatedStale(t *testing.T) {
	now := time.Now()
	e := &entry{modelCost: map[string]modelCostEntry{
		"m1": {CostPer1k: 5, LastSeen: now.Add(time.Hour)},
	}}
	if _, ok := e.modelCostOf("m1", now); ok {
		t.Fatal("LastSeen 在未来的观测被当成有效：时钟回拨后陈旧单价长期生效（审查 C5-32）")
	}
}

// TestRestoreClampsNegativeCredits 恢复路径必须钳负余额（审查 C5-52）。
func TestRestoreClampsNegativeCredits(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	if err := os.WriteFile(fp, []byte(`{"accounts":{"u1":{"credits":-50,"credits_total":100}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	defer p.Close()
	p.mu.RLock()
	c := p.byUID["u1"].credits
	p.mu.RUnlock()
	if c != 0 {
		t.Fatalf("恢复出负余额 %d：weightOf 的 credits 项变负、floorBlocked 恒真（审查 C5-52）", c)
	}
}

// TestWindowProbeResultTruncatedByRune 探针结果按 rune 截断（审查 C5-65）。
func TestWindowProbeResultTruncatedByRune(t *testing.T) {
	p := New("")
	now := time.Now()
	testAcct(t, p, "u1", 1000)
	if !p.ScheduleModelWindow("u1", "m1", now.Add(time.Hour), false, now) {
		t.Fatal("ScheduleModelWindow 失败")
	}
	p.RecordWindowProbe("u1", "m1", time.Time{}, 1, strings.Repeat("测", 300), now)
	ws := p.ModelWindows()
	if len(ws) != 1 {
		t.Fatalf("窗口数=%d want 1", len(ws))
	}
	if !utf8.ValidString(ws[0].LastResult) {
		t.Fatal("截断切断了多字节字符：落盘/展示会成 U+FFFD（审查 C5-65）")
	}
	if n := len([]rune(ws[0].LastResult)); n != 200 {
		t.Fatalf("rune 数=%d want 200（审查 C5-65）", n)
	}
}

// TestScheduleCursorsBounded 策略游标 map 必须有硬上限（审查 C5-72）。
//
// 键是 realm+"\x1f"+model，model 来自客户端请求（任意字符串都会进 pick 一次）：
// round_robin/sequential 模式下没有上限就会被任意模型名顶到无界且常驻进程。
func TestScheduleCursorsBounded(t *testing.T) {
	p := New("")
	p.SetStrategy(StrategyRoundRobin)
	testAcct(t, p, "u1", 1000)
	now := time.Now()

	p.mu.Lock()
	cands := []*entry{p.byUID["u1"]}
	for i := 0; i < maxScheduleCursors+200; i++ {
		p.pickByStrategyLocked(cands, fmt.Sprintf("m%d", i), "", now)
	}
	n := len(p.scheduling)
	p.mu.Unlock()
	if n > maxScheduleCursors {
		t.Fatalf("策略游标 %d 条 > 上限 %d：客户端可把 map 顶到无界（审查 C5-72）", n, maxScheduleCursors)
	}
}

// TestStrategyReadDoesNotTakeWriteLock 纯读的 Strategy() 不得抢写锁（审查 C5-73）。
//
// 判据：外部持读锁时调用 Strategy()。RLock 可重入（无写者等待），Lock 不可 →
// 修复前这里必然阻塞到超时。
func TestStrategyReadDoesNotTakeWriteLock(t *testing.T) {
	p := New("")
	p.SetStrategy(StrategyRoundRobin)

	p.mu.RLock()
	defer p.mu.RUnlock()
	done := make(chan string, 1)
	go func() { done <- p.Strategy() }()
	select {
	case got := <-done:
		if got != StrategyRoundRobin {
			t.Fatalf("Strategy()=%q want %q", got, StrategyRoundRobin)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("持读锁时 Strategy() 阻塞：仍是写锁，面板轮询会与选号串行化（审查 C5-73）")
	}
}

// TestSequentialExhaustDoesNotBorrowOtherAccount 顺序耗尽不得被防撞号兜底借号（审查 C5-74）。
//
// 修复前：选中的号刚被用过（< minPickGap）时会从 order 里借一个别的号 → 「用完一个
// 再用下一个」退化成轮询，还顺带推进那个号的 usedSeq 把 LRU 带偏。
func TestSequentialExhaustDoesNotBorrowOtherAccount(t *testing.T) {
	p := New("")
	p.SetStrategy(StrategySequentialExhaust)
	testAcct(t, p, "u1", 100000)
	testAcct(t, p, "u2", 100000)

	first := p.Pick()
	if first == nil {
		t.Fatal("首次选号 nil")
	}
	// 不跨 minPickGap，立刻再选：仍应是同一个号。
	second := p.Pick()
	if second == nil || second.UID != first.UID {
		t.Fatalf("顺序耗尽换号了（%s → %v）：防撞号兜底借走了别的号（审查 C5-74）", first.UID, second)
	}
}

// TestReserveSlotRespectsInFlightLimit 锁内占位必须走 CAS 上限判据（审查 C5-36）。
//
// 修复前 pick 的占位是裸 `e.inFlight.Add(1)`：它只依赖上游 inFlightFull 的 Load 快照，
// 而 Acquire 是无锁 CAS、不在 p.mu 临界区内 —— 两者并发时 inFlight 会被推到 limit+1，
// 单号并发突破 max_in_flight（上游 6004 / WAF 403 的触发条件）。
func TestReserveSlotRespectsInFlightLimit(t *testing.T) {
	p := New("")
	p.SetMaxInFlight(2)
	testAcct(t, p, "u1", 1000)

	p.mu.Lock()
	e := p.byUID["u1"]
	ok1 := p.reserveSlotLocked(e)
	ok2 := p.reserveSlotLocked(e)
	ok3 := p.reserveSlotLocked(e) // 第三个必须被拒
	n := e.inFlight.Load()
	p.mu.Unlock()

	if !ok1 || !ok2 {
		t.Fatal("名额未满时占位应成功")
	}
	if ok3 {
		t.Fatal("名额已满仍占位成功：突破 max_in_flight（审查 C5-36）")
	}
	if n != 2 {
		t.Fatalf("inFlight=%d want 2：超额占位把计数推过了上限（审查 C5-36）", n)
	}
}

// TestStatusOfAccessorsDoNotSelfLock statusOf 的访问器不得自锁（审查 C5-03 误报钉）。
//
// 审查怀疑 priorityOf/shareOf/reserveThreshold/creditFloorFor 内部取 p.mu.Lock，
// 在 Status/List 的 RLock 内调用会自死锁。核实结论是误报（pool.go:348-372 全部无锁），
// 这条用例把「误报」变成回归钉：将来谁给这些访问器加锁，这里会挂死而不是线上挂死。
func TestStatusOfAccessorsDoNotSelfLock(t *testing.T) {
	p := New("")
	p.SetCreditFloor(100)
	p.SetCreditFloorByRealm(map[string]int64{"cn": 100})
	p.SetReserveCredits(10)
	p.SetAccountRouting(map[string]int{"u1": 1}, map[string]float64{"u1": 0.5})
	testAcct(t, p, "u1", 1000)

	done := make(chan struct{})
	go func() {
		p.List()
		p.Status("u1")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Status/List 自死锁：statusOf 里的访问器又加锁了（审查 C5-03）")
	}
}
