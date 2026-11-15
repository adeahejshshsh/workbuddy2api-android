// audit_round2_test.go 第二轮代码审计（CODE-AUDIT-FULL 第 10.12 节 pool）P2 项的回归用例。
//
// 一条用例对应一个审查编号，命名统一 TestC5_<编号>_<语义>，便于和报告对账。
// 只钉「修好之后必须成立」的行为/契约，不复制实现细节。
package pool

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// C5-09：冷却剩余秒数必须与 Cooling 判定用**同一个** now（此前 remaining 走
// time.Until 又取一次 time.Now，边界上会出现 Cooling=true 但 CoolRemaining=0）。
// 抽出的 coolRemainingOf 接受固定 now，正好把这条钉死。
func TestC5_09_CoolRemainingSingleClock(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	e := &entry{until: now.Add(500 * time.Millisecond), coolKind: CoolSoft}

	remaining, kind := coolRemainingOf(e, now)
	if remaining != 1 {
		t.Fatalf("remaining=%d want 1（向上取整）", remaining)
	}
	if kind != "" {
		t.Fatalf("常规软冷却不应返回 breaker/degraded，got %q", kind)
	}
	// 同一 now 下「Cooling 为真」必然推出 remaining >= 1：这正是原缺陷的矛盾点。
	if !now.Before(e.until) || remaining < 1 {
		t.Fatal("Cooling 与 CoolRemaining 自相矛盾")
	}
	// 已过截止：不再计冷却（不能因为 time.Until 二次取时拿到负数而返回负值）。
	if r, _ := coolRemainingOf(e, now.Add(time.Second)); r != 0 {
		t.Fatalf("过期后 remaining=%d want 0", r)
	}
	// 熔断更晚 → kind=breaker（既有语义不回归）。
	e.breakerUntil = now.Add(2 * time.Hour)
	r, k := coolRemainingOf(e, now)
	if k != "breaker" || r != 7200 {
		t.Fatalf("breaker: remaining=%d kind=%q want 7200/breaker", r, k)
	}
	// 降权更晚 → kind=degraded。
	e.degradeUntil = now.Add(3 * time.Hour)
	if r, k = coolRemainingOf(e, now); k != "degraded" || r != 10800 {
		t.Fatalf("degraded: remaining=%d kind=%q want 10800/degraded", r, k)
	}
}

// C5-10：reserveBlocked 的「余额已知」不能只看 creditsTotal>0 —— 免费号
// （total 恒 0）与「余额接口长期失败」的号否则永不保底。
func TestC5_10_ReserveBlockedUsesCreditsKnown(t *testing.T) {
	p := New("")
	p.SetReserveCredits(10)
	p.Add(&auth.Auth{UID: "u1"})

	// 从未查过余额：total=0、creditsKnown=false → 不停牌（新导入号不得被 0 值停掉）。
	if st, _ := p.Status("u1"); st.ReserveBlocked {
		t.Fatal("未确认余额的账号不应被保留积分停牌")
	}
	// 权威接口确认过余额（total 仍为 0 的免费号）→ 保底生效。
	p.SetCredits("u1", 0, 0)
	st, ok := p.Status("u1")
	if !ok || !st.ReserveBlocked {
		t.Fatalf("creditsKnown 后应停牌: %+v ok=%v", st, ok)
	}
	// 余额高于阈值 → 不停牌。
	p.SetCredits("u1", 100, 0)
	if st, _ := p.Status("u1"); st.ReserveBlocked {
		t.Fatal("余额高于阈值不应停牌")
	}
	// 标记要跨重启保留（否则重启后又回到「永不保底」）。
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p2 := New(fp)
	p2.SetReserveCredits(10)
	p2.Add(&auth.Auth{UID: "u1"})
	p2.SetCredits("u1", 0, 0)
	p2.Flush()
	p3 := New(fp)
	p3.SetReserveCredits(10)
	p3.Add(&auth.Auth{UID: "u1"})
	if st, _ := p3.Status("u1"); !st.ReserveBlocked {
		t.Fatal("credits_known 未持久化：重启后保底失效")
	}
}

// C5-39：一次扫描缺席超过一半时不能直接删状态 —— 走 parked（不进池、但状态留在
// state.json 的 parked_accounts），文件回来时原样接回。
func TestC5_39_SyncToDirMassAbsenceParksState(t *testing.T) {
	p := New("")
	for _, uid := range []string{"u1", "u2", "u3", "u4"} {
		p.Add(&auth.Auth{UID: uid})
	}
	p.SetCredits("u1", 42, 0)

	// 只扫到 u4：缺席 3/4 > 50% → 暂存而不是丢弃。
	p.SyncToDir([]*auth.Auth{{UID: "u4"}})
	if _, ok := p.Status("u1"); ok {
		t.Fatal("u1 应已移出池（不参与选号）")
	}
	p.mu.RLock()
	_, parked := p.parked["u1"]
	p.mu.RUnlock()
	if !parked {
		t.Fatal("大面积缺席应转 parked 保留状态，而不是直接删除")
	}
	// 文件回来 → 状态（credits）原样接回。
	p.Add(&auth.Auth{UID: "u1"})
	if st, ok := p.Status("u1"); !ok || st.Credits != 42 {
		t.Fatalf("接回后 credits=%d want 42 (ok=%v)", st.Credits, ok)
	}

	// 少数缺席（1/2，不超过一半）保持原有删除语义（不回归）。
	p2 := New("")
	p2.Add(&auth.Auth{UID: "a1"})
	p2.Add(&auth.Auth{UID: "a2"})
	p2.SyncToDir([]*auth.Auth{{UID: "a2"}})
	p2.mu.RLock()
	_, parked2 := p2.parked["a1"]
	p2.mu.RUnlock()
	if parked2 {
		t.Fatal("1/2 缺席不应触发 parked 保护（保持删除语义）")
	}
}

// C5-40：Reconcile 的更新必须落在**池里当前那个** auth 指针上（热重载换指针后
// 不能写到孤儿对象上）。这里钉住三条返回值语义 + 更新确实可见。
func TestC5_40_ReconcileLandsOnCurrentPointer(t *testing.T) {
	p := New("")
	old := &auth.Auth{UID: "u1", AccessToken: "old-at", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	p.Add(old)

	newer := &auth.Auth{UID: "u1", AccessToken: "new-at", ExpiresAt: time.Now().Add(2 * time.Hour).Unix()}
	if got := p.Reconcile(newer); got != ReconcileUpdated {
		t.Fatalf("Reconcile=%v want Updated", got)
	}
	if a := p.AuthByUID("u1"); a == nil || a.AccessToken != "new-at" {
		t.Fatalf("池内指针未更新: %+v", a)
	}
	// 幂等：同样的内容再来一次不再算更新。
	if got := p.Reconcile(newer); got != ReconcileUnchanged {
		t.Fatalf("重复 Reconcile=%v want Unchanged", got)
	}
	// 新号 → Added。
	if got := p.Reconcile(&auth.Auth{UID: "u2", AccessToken: "x", ExpiresAt: time.Now().Add(time.Hour).Unix()}); got != ReconcileAdded {
		t.Fatalf("新号 Reconcile=%v want Added", got)
	}
}

// C5-41：Close 幂等且可并发（stopCh 的读写在 p.mu 内，不再有数据竞争）。
func TestC5_41_CloseConcurrentIdempotent(t *testing.T) {
	dir := t.TempDir()
	p := New(filepath.Join(dir, "state.json"))
	p.Add(&auth.Auth{UID: "u1"})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.Close()
		}()
	}
	wg.Wait()
	// 纯内存池（无 flusher）Close 也必须安全返回。
	New("").Close()
}

// C5-43：Set*ByRealm / SetAccountRouting 必须拷贝入参 —— 否则 config 热重载
// 复用同一个 map 改写时与选号读侧构成并发 map 读写（fatal）。
func TestC5_43_ConfigMapsAreCloned(t *testing.T) {
	p := New("")
	floor := map[string]int64{"cn": 5}
	reserve := map[string]int64{"cn": 7}
	priority := map[string]int{"u1": 1}
	share := map[string]float64{"u1": 0.5}
	p.SetCreditFloorByRealm(floor)
	p.SetReserveCreditsByRealm(reserve)
	p.SetAccountRouting(priority, share)

	// 调用方原地改写：池内取值必须不受影响。
	floor["cn"] = 99
	reserve["cn"] = 99
	priority["u1"] = 99
	share["u1"] = 99

	p.mu.RLock()
	gotFloor := p.creditFloorFor("cn")
	gotReserve := p.reserveThreshold("cn")
	gotPriority := p.priorityOf("u1")
	gotShare := p.shareOf("u1")
	p.mu.RUnlock()
	if gotFloor != 5 {
		t.Fatalf("creditFloor=%d want 5（入参未拷贝）", gotFloor)
	}
	if gotReserve != 7 {
		t.Fatalf("reserve=%d want 7（入参未拷贝）", gotReserve)
	}
	if gotPriority != 1 {
		t.Fatalf("priority=%d want 1（入参未拷贝）", gotPriority)
	}
	if gotShare != 0.5 {
		t.Fatalf("share=%v want 0.5（入参未拷贝）", gotShare)
	}
}

// C5-44：暂存区满时丢弃必须留痕（不静默），且不能把账号留在池里。
func TestC5_44_ParkTableFullIsBounded(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.mu.Lock()
	p.parked = make(map[string]*entry, maxParked)
	for i := 0; i < maxParked; i++ {
		p.parked[string(rune('a'+i%26))+string(rune('a'+i/26))] = &entry{a: &auth.Auth{}}
	}
	p.mu.Unlock()

	removed := p.ParkMany([]string{"u1"})
	if len(removed) != 1 || removed[0] != "u1" {
		t.Fatalf("ParkMany=%v want [u1]（移出池的语义不受暂存区满影响）", removed)
	}
	p.mu.RLock()
	_, stillInPool := p.byUID["u1"]
	n := len(p.parked)
	p.mu.RUnlock()
	if stillInPool {
		t.Fatal("u1 应已移出池")
	}
	if n > maxParked {
		t.Fatalf("暂存区 %d 条超过上限 %d", n, maxParked)
	}
}

// C5-49：RestoreFromSnapshot 在锁内快照 store/stateFp；这里钉住行为面 ——
// 本地文件缺失时采用快照（该分支依赖锁内读到的两个字段）。
func TestC5_49_RestoreFromSnapshotUsesStoreWhenLocalMissing(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 77, 0)
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	// 快照内容 = 当前 state + saved_at（未来时刻，保证比本地新）。
	snapRaw := strings.TrimSuffix(string(raw), "}") + `,"saved_at":"2999-01-01T00:00:00Z"}`
	ms := &memStore{loadData: []byte(snapRaw), loadOK: true}
	if err := os.Remove(fp); err != nil {
		t.Fatal(err)
	}
	p2 := New(fp)
	p2.SetStore(ms)
	p2.RestoreFromSnapshot()
	p2.SyncToDir([]*auth.Auth{{UID: "u1"}})
	if st, ok := p2.Status("u1"); !ok || st.Credits != 77 {
		t.Fatalf("快照未生效: %+v ok=%v", st, ok)
	}
}

// C5-50：快照恢复不能把在途计数清零（否则并发上限瞬间失效、Release 会误扣别人名额）。
func TestC5_50_SnapshotRestoreKeepsInFlight(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetMaxInFlight(4)
	if !p.Acquire("u1") || !p.Acquire("u1") {
		t.Fatal("Acquire 应成功")
	}
	before := p.InFlight("u1")
	if before != 2 {
		t.Fatalf("inFlight=%d want 2", before)
	}
	// 直接用快照覆盖内存（模拟 SetStore 晚于接单的运行期恢复）。
	s := snapshot{stateFile: p.stateOverviewForTest(), SavedAt: time.Now()}
	p.mu.Lock()
	p.applySnapshotLocked(s)
	p.mu.Unlock()
	if got := p.InFlight("u1"); got != before {
		t.Fatalf("快照恢复后在途=%d want %d（运行态被清零）", got, before)
	}
}

// stateOverviewForTest 取一份当前状态快照（持锁）。
func (p *Pool) stateOverviewForTest() stateFile {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.stateOverviewLocked()
}

// C5-53：一次 flush 只构建一次全池状态（此前 stateOverviewLocked 被调两次）。
// 行为面钉住：flush 后落盘内容与内存一致，且 Redis 镜像与本地文件同源。
func TestC5_53_FlushBuildsOverviewOnce(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 12, 34)
	ms := &memStore{}
	p.SetStore(ms)
	if err := p.flushNowErr(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"credits": 12`) {
		t.Fatalf("state.json 未含 credits: %s", raw)
	}
	ms.mu.Lock()
	saved := append([]byte(nil), ms.saved...)
	ms.mu.Unlock()
	// 镜像走 json.Marshal（紧凑格式），本地文件走 MarshalIndent（带空格）。
	if len(saved) == 0 || !strings.Contains(string(saved), `"credits":12`) {
		t.Fatal("Redis 镜像与本地落盘不同源（sf 复用被破坏）")
	}
}

// C5-54：落盘周期在 New 时固化到 Pool 字段，flusher 不再读包级 var。
func TestC5_54_FlushIntervalSnapshot(t *testing.T) {
	old := flushInterval
	flushInterval = 37 * time.Millisecond
	defer func() { flushInterval = old }()

	p := New(filepath.Join(t.TempDir(), "state.json"))
	defer p.Close()
	if p.flushEvery != 37*time.Millisecond {
		t.Fatalf("flushEvery=%v want 37ms", p.flushEvery)
	}
	// 构造后改包级 var 不再影响该池（goroutine 只读自己的字段）。
	flushInterval = time.Hour
	if p.flushEvery != 37*time.Millisecond {
		t.Fatal("Pool.flushEvery 被包级 var 覆盖")
	}
}

// C5-55：恢复时暂存区满不能静默丢（行为面：不 panic、不超过上限）。
func TestC5_55_LoadParkedBounded(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	var b strings.Builder
	b.WriteString(`{"accounts":{},"parked_accounts":{`)
	for i := 0; i < maxParked+10; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"p`)
		b.WriteString(strings.Repeat("x", 1))
		b.WriteString(string(rune('a' + i%26)))
		b.WriteString(string(rune('a' + i/26)))
		b.WriteString(`":{"credits":1}`)
	}
	b.WriteString(`}}`)
	if err := os.WriteFile(fp, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	p.mu.RLock()
	n := len(p.parked)
	p.mu.RUnlock()
	if n > maxParked {
		t.Fatalf("暂存区恢复 %d 条超过上限 %d", n, maxParked)
	}
}

// C5-58：DeleteRule 不能原地压缩 ruleIDs（持有旧切片的代码会看到内容被凭空改写）。
func TestC5_58_DeleteRuleNoInPlaceCompaction(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	r1, err := p.UpsertRule(ModelRule{Name: "deny-a", Mode: RuleModeDeny, Models: []string{"m1"}})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := p.UpsertRule(ModelRule{Name: "deny-b", Mode: RuleModeDeny, Models: []string{"m2"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.AssignRules("u1", []string{r1.ID, r2.ID}); err != nil {
		t.Fatal(err)
	}
	snapshot := p.RuleIDsOf("u1")
	if len(snapshot) != 2 {
		t.Fatalf("前置: ruleIDs=%v", snapshot)
	}
	if !p.DeleteRule(r1.ID) {
		t.Fatal("DeleteRule 应成功")
	}
	// 删除前拿到的切片必须原样保留（未被原地压缩改写）。AssignRules 会排序，
	// 故按集合比较而不是按位置。
	seen := map[string]bool{snapshot[0]: true, snapshot[1]: true}
	if !seen[r1.ID] || !seen[r2.ID] {
		t.Fatalf("旧切片被原地改写: %v（want 含 %s 与 %s）", snapshot, r1.ID, r2.ID)
	}
	if got := p.RuleIDsOf("u1"); len(got) != 1 || got[0] != r2.ID {
		t.Fatalf("删除后 ruleIDs=%v want [%s]", got, r2.ID)
	}
}

// C5-59：暂存号（parked）也要能挂/摘规则，否则热重载窗口期的配置静默丢失。
func TestC5_59_RulesApplyToParkedAccounts(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	r, err := p.UpsertRule(ModelRule{Name: "deny", Mode: RuleModeDeny, Models: []string{"m1"}})
	if err != nil {
		t.Fatal(err)
	}
	p.ParkMany([]string{"u1"})
	if _, ok := p.Status("u1"); ok {
		t.Fatal("前置: u1 应在暂存区")
	}
	if _, err := p.AssignRules("u1", []string{r.ID}); err != nil {
		t.Fatalf("暂存号 AssignRules 失败: %v", err)
	}
	if got := p.RuleIDsOf("u1"); len(got) != 1 || got[0] != r.ID {
		t.Fatalf("暂存号 ruleIDs=%v", got)
	}
	// 文件回来 → 规则仍然生效（被规则拦住的模型不可选）。
	p.Add(&auth.Auth{UID: "u1"})
	p.mu.RLock()
	e := p.byUID["u1"]
	blocked := e.blockedByRules("m1")
	p.mu.RUnlock()
	if !blocked {
		t.Fatal("接回后规则未生效")
	}
	// ApplyRule 同样覆盖暂存号。
	p.ParkMany([]string{"u1"})
	if n, err := p.ApplyRule(r.ID, []string{"u1"}, false); err != nil || n != 1 {
		t.Fatalf("ApplyRule(parked)=%d,%v want 1,nil", n, err)
	}
}

// C5-64：ScheduleModelWindow 内的 prune 必须节流（限流风暴时不能每个 429 都全表扫）。
func TestC5_64_WindowPruneThrottled(t *testing.T) {
	p := New("")
	now := time.Now()
	p.Add(&auth.Auth{UID: "u1"})
	// 造一条 8 天前的死窗口（prune 会删它）。
	p.mu.Lock()
	p.windows = map[string]*ModelWindow{
		"dead\x1fmodel": {UID: "dead", Model: "model", ResetAt: now.Add(-8 * 24 * time.Hour)},
	}
	p.lastWindowPrune = now // 刚刚 prune 过 → 本次登记应被节流
	p.mu.Unlock()

	p.ScheduleModelWindow("u1", "m1", now.Add(time.Hour), false, now)
	p.mu.RLock()
	_, kept := p.windows["dead\x1fmodel"]
	p.mu.RUnlock()
	if !kept {
		t.Fatal("节流期内不应执行 prune（死窗口被删说明没节流）")
	}
	// 超过间隔后应真的清掉。
	p.ScheduleModelWindow("u1", "m2", now.Add(2*time.Hour), false, now.Add(windowPruneInterval))
	p.mu.RLock()
	_, kept = p.windows["dead\x1fmodel"]
	p.mu.RUnlock()
	if kept {
		t.Fatal("超过 windowPruneInterval 后应清理死窗口")
	}
}

// C5-67：窗口表清理也要挂在 flusher tick 上（否则不再 429 的部署只增不减）。
func TestC5_67_PruneWindowsOnTick(t *testing.T) {
	p := New("")
	now := time.Now()
	p.mu.Lock()
	p.windows = map[string]*ModelWindow{
		"dead\x1fmodel": {UID: "dead", Model: "model", ResetAt: now.Add(-8 * 24 * time.Hour)},
		"live\x1fmodel": {UID: "live", Model: "model", ResetAt: now.Add(time.Hour)},
	}
	p.mu.Unlock()
	p.pruneWindowsTick()
	p.mu.RLock()
	n := len(p.windows)
	_, live := p.windows["live\x1fmodel"]
	p.mu.RUnlock()
	if n != 1 || !live {
		t.Fatalf("tick 清理后 windows=%d live=%v want 1/true", n, live)
	}
}

// C5-69：model_tier.json 损坏要留档（.corrupt-*）+ 告警，不能静默当空后被覆盖。
func TestC5_69_CorruptTierFileArchived(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	tierFp := filepath.Join(dir, "model_tier.json")
	if err := os.WriteFile(tierFp, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteModelTierDenied("u1", "free_user", "m1")

	matches, err := filepath.Glob(tierFp + ".corrupt-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("损坏的 model_tier.json 未留档")
	}
	// 新事实仍然生效（不因损坏阻断）。
	if !p.ModelDeniedForAllAccounts("m1") {
		t.Fatal("损坏后新事实未生效")
	}
}

// C5-70：删除账号要摘掉 UIDType 残留；Deny 表有硬上限。
func TestC5_70_TierFactsPrunedAndCapped(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteModelTierDenied("u1", "free_user", "m1")
	p.mu.RLock()
	_, has := p.tier.UIDType["u1"]
	p.mu.RUnlock()
	if !has {
		t.Fatal("前置: UIDType 应已记录")
	}
	p.Remove("u1")
	p.mu.RLock()
	_, has = p.tier.UIDType["u1"]
	p.mu.RUnlock()
	if has {
		t.Fatal("删除账号后 UIDType 残留")
	}

	// Deny 上限：到顶后不再记新模型，但已有事实保留。
	p2 := New("")
	p2.Add(&auth.Auth{UID: "u1"})
	p2.mu.Lock()
	p2.loadTierLocked()
	p2.tier.Deny["free_user"] = make(map[string]bool, maxTierDenyModels)
	for i := 0; i < maxTierDenyModels; i++ {
		p2.tier.Deny["free_user"][string(rune('a'+i%26))+string(rune('a'+i/26))] = true
	}
	p2.mu.Unlock()
	p2.NoteModelTierDenied("u1", "free_user", "brand-new-model")
	p2.mu.RLock()
	n := len(p2.tier.Deny["free_user"])
	_, added := p2.tier.Deny["free_user"]["brand-new-model"]
	p2.mu.RUnlock()
	if n > maxTierDenyModels || added {
		t.Fatalf("Deny 表未设上限: n=%d added=%v", n, added)
	}
}

// C5-76：连字符别名补齐，未知值回落 weighted 且不再静默（日志可见）。
func TestC5_76_StrategyAliases(t *testing.T) {
	cases := map[string]string{
		"sequential-exhaust": StrategySequentialExhaust,
		"sequential_exhaust": StrategySequentialExhaust,
		"seq":                StrategySequentialExhaust,
		"round-robin":        StrategyRoundRobin,
		"round_robin":        StrategyRoundRobin,
		"rr":                 StrategyRoundRobin,
		"weighted":           StrategyWeighted,
		"":                   StrategyWeighted,
		"nonsense":           StrategyWeighted,
	}
	for in, want := range cases {
		if got := NormalizeStrategy(in); got != want {
			t.Errorf("NormalizeStrategy(%q)=%q want %q", in, got, want)
		}
	}
}

// C5-81 / C5-83：ModelLockView 的 Servable/Total 口径 —— 单次遍历统计必须与
// 「逐行全量重扫」等价，且 disabled/paused/needAuth 与 Total 用同一组谓词。
func TestC5_81_ModelLockViewServable(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "u2", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "u3", Domain: "www.codebuddy.cn"})
	now := time.Now()
	resetAt := now.Add(time.Hour)
	p.CooldownSoftForModel("u1", time.Minute, resetAt, "m1", "6004")
	p.CooldownSoftForModel("u2", time.Minute, resetAt, "m1", "6004")
	p.Disable("u3", "test")

	rows := p.ModelLockView()
	if len(rows) != 1 {
		t.Fatalf("rows=%d want 1: %+v", len(rows), rows)
	}
	row := rows[0]
	if row.Realm != "cn" || row.Model != "m1" {
		t.Fatalf("row=%+v", row)
	}
	// Total 排除 disabled 的 u3 → 2；Locked 2 → 状态 locked、Servable 0。
	if row.Total != 2 || row.Locked != 2 || row.Servable != 0 || row.State != "locked" {
		t.Fatalf("row=%+v want Total=2 Locked=2 Servable=0 locked", row)
	}
	// 解除一个号的模型冷却 → Servable=1、state=partial。
	p.ClearQuotaCooldown("u1", "m1", now.Add(2*time.Hour))
	rows = p.ModelLockView()
	if len(rows) != 1 {
		t.Fatalf("解除后 rows=%d want 1: %+v", len(rows), rows)
	}
	if rows[0].Servable != 1 || rows[0].State != "partial" || rows[0].Locked != 1 {
		t.Fatalf("解除后 row=%+v want Servable=1 Locked=1 partial", rows[0])
	}
}

// C5-85：PickExistingAuth 一次加锁查完（行为面：按顺序返回第一个仍在的号）。
func TestC5_85_PickExistingAuthOrder(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at1"})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at2"})
	if a := p.PickExistingAuth([]string{"gone", "u2", "u1"}); a == nil || a.UID != "u2" {
		t.Fatalf("PickExistingAuth=%v want u2", a)
	}
	if a := p.PickExistingAuth([]string{"gone"}); a != nil {
		t.Fatalf("全不在应返回 nil, got %v", a)
	}
	if a := p.PickExistingAuth(nil); a != nil {
		t.Fatalf("空入参应返回 nil, got %v", a)
	}
}

// C5-88：禁用清冷却域时只清 6004，保留 11102/11158 负缓存与退避历史。
func TestC5_88_DisableKeepsNegativeCache(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(30*time.Minute), "m-6004", "6004 quota")
	p.BlockModelBackoff("u1", "m-11102", "11102 model not exist")

	p.Disable("u1", "test")
	p.mu.RLock()
	_, has6004 := p.byUID["u1"].modelCooldowns["m-6004"]
	_, has11102 := p.byUID["u1"].modelCooldowns["m-11102"]
	p.mu.RUnlock()
	if has6004 {
		t.Fatal("6004 模型级冷却应随禁用清掉")
	}
	if !has11102 {
		t.Fatal("11102 负缓存是上游事实，不应随禁用清掉（审查 C5-88）")
	}
}

// C5-90：手动关闭的键必须归一化（大小写/域前缀/空白），写入与读取同口径。
func TestC5_90_ModelOffNormalized(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	if !p.SetModelOff("u1", " GPT-4o ", "test") {
		t.Fatal("SetModelOff 应成功")
	}
	p.mu.RLock()
	e := p.byUID["u1"]
	p.mu.RUnlock()
	for _, probe := range []string{"gpt-4o", "GPT-4o", "global:gpt-4o", "cn:GPT-4O"} {
		if !e.modelOffed(probe) {
			t.Errorf("modelOffed(%q)=false，归一化失效", probe)
		}
	}
	if e.modelOffed("gpt-4o-mini") {
		t.Error("不同模型不应命中")
	}
	// 解除也要归一化。
	if !p.SetModelOn("u1", "GLOBAL:gpt-4o") {
		t.Fatal("SetModelOn 应命中同一键")
	}
	if e.modelOffed("gpt-4o") {
		t.Fatal("解除后仍被关闭")
	}

	// 旧文件的键在恢复时归一化（否则旧开关全部作废）。
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	raw := `{"accounts":{"u1":{"credits":1,"model_off":{"GPT-4o":{"since":"2026-01-01T00:00:00Z"}}}}}`
	if err := os.WriteFile(fp, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	p2.mu.RLock()
	e2 := p2.byUID["u1"]
	p2.mu.RUnlock()
	if !e2.modelOffed("gpt-4o") {
		t.Fatal("旧文件的键未归一化，手动关闭失效")
	}
}

// C5-91：暂存号的手动开关也要生效（热重载窗口期）。
func TestC5_91_ModelOffOnParked(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.ParkMany([]string{"u1"})
	if !p.SetModelOff("u1", "m1", "parked") {
		t.Fatal("暂存号的 SetModelOff 应成功")
	}
	if got := p.ModelOffOf("u1"); len(got) != 1 || got[0].Model != "m1" {
		t.Fatalf("ModelOffOf(parked)=%+v", got)
	}
	if !p.SetModelOn("u1", "m1") {
		t.Fatal("暂存号的 SetModelOn 应成功")
	}
}

// C5-92：ModelOffOf 是纯读，不能抢选号写锁（持读锁调用不得阻塞）。
func TestC5_92_ModelOffOfUsesReadLock(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetModelOff("u1", "m1", "x")

	done := make(chan struct{})
	go func() {
		p.mu.RLock()
		defer p.mu.RUnlock()
		_ = p.ModelOffOf("u1")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ModelOffOf 在别人持读锁时阻塞：说明仍用写锁")
	}
}

// C5-93 / C5-94：出口计数表用独立锁 —— 持池写锁时记账/查询不得阻塞，
// 且清表路径不 panic（超限重置）。
func TestC5_94_EgressIndependentLock(t *testing.T) {
	p := New("")
	now := time.Now()
	done := make(chan struct{})
	go func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if got := p.NoteEgressFailure("direct:cn", now); got != 1 {
			t.Errorf("streak=%d want 1", got)
		}
		if got := p.EgressStreak("direct:cn", now); got != 1 {
			t.Errorf("streak=%d want 1", got)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("出口记账阻塞在池锁上：未使用独立锁（审查 C5-94）")
	}
	// 超限重置路径（C5-93）：不 panic、表被清空后重新计数。
	for i := 0; i < maxEgressKeys+5; i++ {
		p.NoteEgressFailure("p:node-"+string(rune('a'+i%26))+string(rune('a'+i/26)), now)
	}
	if n := len(p.egress); n > maxEgressKeys+1 {
		t.Fatalf("计数表 %d 超限未回收", n)
	}
}

// C5-95 / C5-96：套餐分类按整词匹配（"surplus" 不再命中 plus），关键词表预转小写。
func TestC5_95_PlanWordBoundary(t *testing.T) {
	cases := []struct {
		pkgs []string
		want PlanKind
	}{
		{[]string{"surplus credits pack"}, PlanUnknown}, // 含 plus 但不是整词
		{[]string{"credits plus"}, PlanStandard},        // plus 是整词
		{[]string{"Free Plan"}, PlanFree},               // 原有强信号不受影响
		{[]string{"Pro Plan Annual"}, PlanStandard},     // 多词整词匹配
		{[]string{"premium-tier"}, PlanStandard},        // 连字符算分隔符
		{[]string{"国内运营裂变包", "拉新权益包"}, PlanActivity},    // CJK 仍走子串
	}
	for _, c := range cases {
		if got, ev := ClassifyPlan(c.pkgs); got != c.want {
			t.Errorf("ClassifyPlan(%v)=%s(%q) want %s", c.pkgs, got, ev, c.want)
		}
	}
}

// C5-98：连败降权的日志移到锁外后，判定行为不变（达阈仍出池）。
func TestC5_98_DegradeStillTriggers(t *testing.T) {
	p := New("")
	p.SetDegrade(3, time.Minute, time.Hour)
	p.Add(&auth.Auth{UID: "u1"})
	for i := 0; i < 3; i++ {
		p.NoteFailures("u1")
	}
	st, _ := p.Status("u1")
	if !st.Cooling || st.CoolKind != "degraded" {
		t.Fatalf("连败降权未触发: %+v", st)
	}
}

// C5-61：随机源不可用时的退化 id 必须唯一（同微秒内两次创建不能碰撞）。
func TestC5_61_FallbackRuleIDUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		id := fallbackRuleID()
		if id == "" {
			t.Fatal("退化 id 不得为空")
		}
		if seen[id] {
			t.Fatalf("退化 id 碰撞: %s", id)
		}
		seen[id] = true
	}
}
