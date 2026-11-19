package pool

// W4 分片修复的回归用例（C-02 / W4-03 / W4-07 / W4-08 / W4-12 / W4-13 / W4-18）。
//
// 每条用例都是「把修复改坏 → 必须变红」的形态。Android/arm64 没有 race detector
// （`go test -race` 报 not supported），所以并发类问题一律用**可控同步点**或
// **结构断言（编译期）**钉住，不依赖「跑得快就能撞上」的运气。

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// readStateAccounts 读 state.json 的账号表；文件不存在返回 nil。
func readStateAccounts(t *testing.T, path string) map[string]stateAccount {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	var sf stateFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		t.Fatalf("%s 不是合法 JSON（%v）：%s", path, err, string(raw))
	}
	return sf.Accounts
}

// TestConcurrentFlushNeverRegressesState C-02 / W4-01 的核心用例。
//
// 复现形态：G1 先取快照 S1（2 个账号）后停在同步点；G2（面板删号 Remove）取新快照
// S2（1 个账号）并落盘；随后放行 G1。没有 flushMu 时 G1 会把 S1 写回去 ——
// state.json 账号数从 1 回退到 2（已删账号复活），.bak 也被污染成 S2。
//
// 变异验证：去掉 flushMu（flushNowErr 不锁）→ 本用例在「state.json 账号数 == 1」
// 处必红；恢复后绿。
func TestConcurrentFlushNeverRegressesState(t *testing.T) {
	dir := t.TempDir()
	stateFp := filepath.Join(dir, "state.json")
	p := New(stateFp)
	defer p.Close()

	p.Add(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999})
	if err := p.flushNowErr(); err != nil {
		t.Fatalf("基线落盘: %v", err)
	}
	if n := len(readStateAccounts(t, stateFp)); n != 2 {
		t.Fatalf("基线账号数 = %d，want 2", n)
	}

	var hookCalls atomic.Int32
	snapTaken := make(chan struct{})
	release := make(chan struct{})
	hook := func() {
		// 只有**第一次** flush 停在同步点：让「旧快照」与「新快照」的落盘窗口重叠。
		// 这里不能用 sync.Once：Once.Do 会让后续调用方一起等第一个函数返回，
		// 那样 G2 也会被卡住，乱序窗口根本不会出现（变异验证会假绿）。
		if hookCalls.Add(1) == 1 {
			close(snapTaken)
			<-release
		}
	}
	p.flushHook.Store(&hook)

	oldDone := make(chan error, 1)
	go func() { oldDone <- p.flushNowErr() }() // G1：快照含 u1+u2

	select {
	case <-snapTaken:
	case <-time.After(5 * time.Second):
		t.Fatal("G1 没有走到「快照已取」同步点")
	}

	// G2：删掉 u2 并落盘（新快照只剩 u1）。它会等 flushMu（修复后）或直接写（修复前）。
	removeDone := make(chan struct{})
	go func() {
		p.Remove("u2")
		close(removeDone)
	}()

	// 给 G2 一个窗口把「1 个账号」写进去。修复后 G2 被 flushMu 挡住，这里会等满；
	// 修复前 G2 会很快写完 —— 正是我们要放行旧快照的时机。
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(stateFp); err == nil {
			var sf stateFile
			if json.Unmarshal(raw, &sf) == nil && len(sf.Accounts) == 1 {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)

	if err := <-oldDone; err != nil {
		t.Fatalf("G1 flush: %v", err)
	}
	select {
	case <-removeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("G2 Remove 未返回（flushMu 死锁？）")
	}

	if n := len(readStateAccounts(t, stateFp)); n != 1 {
		t.Fatalf("state.json 账号数 = %d，want 1：旧快照覆盖了新快照（已删账号复活，C-02 回归）", n)
	}
	// .bak 必须是「真正的上一版」：最后一次写入的前一版是 G1 写下的 2 个账号。
	if n := len(readStateAccounts(t, stateFp+".bak")); n != 2 {
		t.Fatalf(".bak 账号数 = %d，want 2（.bak 轮转被污染：它不是上一版）", n)
	}
}

// TestTombstoneKeepsReasonCode W4-07 / W4-25：过期模型冷却的墓碑重写必须保留 Reason。
//
// 11102/11158 的识别全靠 reason 前缀（BlockModelClear、ModelBlockedForRealm、
// cooldown.go 的 strings.HasPrefix 判定）。抹成空串后墓碑不再被认作「官方确定的
// 负缓存」，退避历史（Hits）在下次 BlockModel 时从头学起，面板还会出现无因条目。
//
// 变异验证：把 `Reason: mc.Reason` 从墓碑结构里删掉 → 本用例红。
func TestTombstoneKeepsReasonCode(t *testing.T) {
	now := time.Now()
	e := &entry{modelCooldowns: map[string]modelCooldown{
		"glm-5.3": {
			Until:  now.Add(-time.Minute), // 已过期 → 走墓碑分支
			Reason: "11102 model not found",
			Hits:   3,
		},
	}}
	e.pruneExpiredModelCooldowns(now)

	mc, ok := e.modelCooldowns["glm-5.3"]
	if !ok {
		t.Fatal("墓碑被整条删了（Hits>0 时应保留退避历史）")
	}
	if mc.Hits != 3 {
		t.Fatalf("Hits = %d，want 3（退避历史丢失）", mc.Hits)
	}
	if !strings.HasPrefix(mc.Reason, "11102") {
		t.Fatalf("墓碑 Reason = %q，want 以 11102 开头（前缀是负缓存的唯一识别依据）", mc.Reason)
	}
	if mc.PrunedAt.IsZero() {
		t.Fatal("PrunedAt 未记录：墓碑会永不过期（C5-30 回归）")
	}
}

// TestUpsertKeepsFreshToken W4-12：SyncToDir/Add 合并凭证时不能把刚刷新的令牌盖回旧值。
//
// 变异验证：upsertLocked 恢复成 `e.a = a` → 第一段断言（fresh）红。
func TestUpsertKeepsFreshToken(t *testing.T) {
	p := New("")
	now := time.Now()
	p.Add(&auth.Auth{UID: "u1", AccessToken: "fresh", ExpiresAt: now.Add(2 * time.Hour).Unix()})

	// 磁盘扫描回来的旧副本（expiry 更早，token 也是旧的）。
	p.SyncToDir([]*auth.Auth{{UID: "u1", AccessToken: "stale", ExpiresAt: now.Add(time.Hour).Unix()}})

	a := p.PickByUID("u1")
	if a == nil {
		t.Fatal("账号应从池里取到")
	}
	if got := a.AccessTokenValue(); got != "fresh" {
		t.Fatalf("AccessToken = %q，want fresh（旧副本覆盖了内存里刚刷新的令牌）", got)
	}

	// 反向：来源确实更新（expiry 更晚）时必须采纳，否则外部轮换的令牌永远进不来。
	p.SyncToDir([]*auth.Auth{{UID: "u1", AccessToken: "newer", ExpiresAt: now.Add(3 * time.Hour).Unix()}})
	if got := p.PickByUID("u1").AccessTokenValue(); got != "newer" {
		t.Fatalf("AccessToken = %q，want newer（更新的令牌没有被采纳）", got)
	}
}

// TestSyncToDirDropsTierFact W4-13：SyncToDir 真删除账号时要一起摘掉等级事实。
//
// 变异验证：删掉 SyncToDir 里的 dropTierUID(droppedTier...) → 本用例红。
func TestSyncToDirDropsTierFact(t *testing.T) {
	dir := t.TempDir()
	stateFp := filepath.Join(dir, "state.json")
	p := New(stateFp)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999})
	p.NoteModelTierDenied("u2", "free", "glm-5.3")

	p.mu.RLock()
	_, had := p.tier.UIDType["u2"]
	p.mu.RUnlock()
	if !had {
		t.Fatal("前提失败：u2 的等级事实没记上")
	}

	// 只扫到 u1 → u2 缺席 1/2（不超过一半，走真删除而不是暂存）。
	p.SyncToDir([]*auth.Auth{{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}})

	p.mu.RLock()
	_, still := p.tier.UIDType["u2"]
	p.mu.RUnlock()
	if still {
		t.Fatal("SyncToDir 删了账号却没摘等级事实（model_tier.json 会永久残留已删 uid）")
	}
	raw, err := os.ReadFile(p.tierFile)
	if err == nil && strings.Contains(string(raw), `"u2"`) {
		t.Fatalf("model_tier.json 里仍残留已删账号 u2: %s", raw)
	}
}

// TestPickByUIDUsesReadLockPath W4-18：PickByUID 的校验段必须走读锁。
//
// 锁模式本身无法从行为上稳定观测（两种实现都能返回正确结果），所以按仓库既有做法
// （familylock_test.go / plan_test.go）做源码级断言：读取锁必须出现在校验段，
// 且**第一个写锁必须在第一次 RUnlock 之后**（即只用于记录 lastUsed/usedSeq）。
//
// 变异验证：把开头的 p.mu.RLock() 改回 p.mu.Lock() → 本用例红。
func TestPickByUIDUsesReadLockPath(t *testing.T) {
	src := string(mustRead(t, "state.go"))
	start := strings.Index(src, "func (p *Pool) PickByUID(uid string) *auth.Auth {")
	if start < 0 {
		t.Fatal("找不到 PickByUID")
	}
	body := src[start:]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}
	firstRLock := strings.Index(body, "p.mu.RLock()")
	firstRUnlock := strings.Index(body, "p.mu.RUnlock()")
	firstLock := strings.Index(body, "p.mu.Lock()")
	if firstRLock < 0 || firstRUnlock < 0 {
		t.Fatal("PickByUID 没有读锁路径（W4-18 回归：整段写锁会把粘性命中路径与 pick/Status 串行化）")
	}
	if firstLock >= 0 && firstLock < firstRUnlock {
		t.Fatalf("PickByUID 在第一次 RUnlock 之前就抢了写锁（firstLock=%d firstRUnlock=%d）", firstLock, firstRUnlock)
	}
	if firstRLock > firstRUnlock {
		t.Fatal("PickByUID 的锁顺序异常")
	}
}

// TestStoreHotSwapWhileFlushing W4-03：SetStore 热切换与镜像落盘并发时不得撕裂。
//
// 修复后 store 走 atomic.Pointer，读侧永远看到「完整的旧 store」或「完整的新 store」；
// 修复前是裸接口字段，-race 下必报、生产上可表现为对 nil data 指针调用 SaveState。
// Android/arm64 没有 race detector，这里用高频交替 + 结构断言兜住：
//   - 交替期间不能 panic（撕裂的 iface 调方法会崩）；
//   - 结构断言（见下方 var _）保证字段类型不许退回裸接口。
//
// 变异验证：把 storeRef 改回 `store StoreSnapshotter` 并让 mirrorToStore 直接读它
// → 结构断言编译失败（run.sh 报「编译失败」= 红）。
func TestStoreHotSwapWhileFlushing(t *testing.T) {
	// 纯内存池：本用例只关心 store 热切换，不让 state.json 的 fsync 干扰节奏
	// （磁盘写在这里会把 2000 轮交替拖成分钟级）。
	p := New("")
	defer p.Close()
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})

	var a, b countingStore
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 交替热切换（含 nil：等价于关闭镜像）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			switch i % 3 {
			case 0:
				p.SetStore(&a)
			case 1:
				p.SetStore(&b)
			default:
				p.SetStore(nil)
			}
		}
		close(stop)
	}()

	// 并发 flush（镜像每轮都跑）。
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				p.dirty.Store(true)
				_ = p.flushNowErr()
			}
		}()
	}
	wg.Wait()

	// 收尾：装回 b 再落一次，镜像必须到达 b（证明读侧看到的是完整的新 store）。
	p.SetStore(&b)
	p.dirty.Store(true)
	if err := p.flushNowErr(); err != nil {
		t.Fatalf("收尾 flush: %v", err)
	}
	if b.saves.Load() == 0 {
		t.Fatal("最终 store 一条快照都没收到（热切换后镜像丢失）")
	}
	if a.saves.Load()+b.saves.Load() == 0 {
		t.Fatal("整个交替期间没有任何镜像写入")
	}
}

// countingStore 计数用的最小 StoreSnapshotter。
type countingStore struct {
	saves atomic.Int64
}

func (c *countingStore) SaveState([]byte)          { c.saves.Add(1) }
func (c *countingStore) LoadState() ([]byte, bool) { return nil, false }

// 结构断言（W4-03 / W4-08）：这两个字段必须保持原子类型。
// 谁把它们改回裸 int / 裸接口，本文件就编译不过 —— 这就是「改坏 → 红」的形态
// （run.sh 判定为「编译失败」）。
var (
	_ = func(p *Pool) { var _ atomic.Pointer[storeHolder] = p.storeRef; p.storeRef.Load() }
	_ = func(p *Pool) { var _ atomic.Int64 = p.persistFails; p.persistFails.Load() }
)

// TestPersistFailCounterNoLostUpdate W4-08：并发失败计数不得丢更新。
//
// 变异验证：persistFails 改回裸 int（notePersistFail 里 Load/Add 的写法也要跟着
// 退回 `p.persistFails++`）→ 结构断言先编译失败；即使绕过，下面 100×100 次并发
// 自增在 ARM 上也会丢更新 → 计数 != 10000 必红。
func TestPersistFailCounterNoLostUpdate(t *testing.T) {
	p := New("")
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				p.notePersistFail(errors.New("boom"))
			}
		}()
	}
	wg.Wait()
	if got := p.persistFails.Load(); got != 10000 {
		t.Fatalf("persistFails = %d，want 10000（并发自增丢更新）", got)
	}
}

// TestAdoptVerifiedKeepsShorterExpiryCredential 跨分片回归（W4-12 求证入口）：
// panel 导入「已 refresh 求证」的凭据时，新凭据的 expiry 往往更短（72h → 1h），
// 但旧 RT 已被上游轮换作废 —— 必须无条件采纳，不能按 expiry 判新旧。
//
// 变异验证：把 AdoptVerified 里的 `e.a = a` 换成 `p.upsertLocked(a)`（退回 expiry
// 比较）→ 本用例红。
func TestAdoptVerifiedKeepsShorterExpiryCredential(t *testing.T) {
	p := New("")
	now := time.Now()
	p.Add(&auth.Auth{UID: "u", AccessToken: "old-at", RefreshToken: "old-rt", ExpiresAt: now.Add(72 * time.Hour).Unix()})

	replaced := p.AdoptVerified(&auth.Auth{UID: "u", AccessToken: "rotated-at", RefreshToken: "rotated-rt", ExpiresAt: now.Add(time.Hour).Unix()})
	if !replaced {
		t.Fatal("池内已有该 uid 时应报告替换")
	}
	a := p.PickByUID("u")
	if a == nil {
		t.Fatal("账号不应从池里消失")
	}
	if got := a.RefreshTokenValue(); got != "rotated-rt" {
		t.Fatalf("RefreshToken = %q，want rotated-rt（求证后的新凭据被 expiry 比较丢掉了）", got)
	}
	if got := a.AccessTokenValue(); got != "rotated-at" {
		t.Fatalf("AccessToken = %q，want rotated-at", got)
	}
}

// TestAdoptVerifiedPreservesEntryState：只换凭证，credits/禁用/在途等条目状态必须保留。
func TestAdoptVerifiedPreservesEntryState(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u", AccessToken: "old-at", RefreshToken: "old-rt", ExpiresAt: 9999999999})
	p.SetCredits("u", 4242, 5000)
	p.Disable("u", "手工停用")

	p.AdoptVerified(&auth.Auth{UID: "u", AccessToken: "rotated-at", RefreshToken: "rotated-rt", ExpiresAt: 9999999999})

	st, ok := p.Status("u")
	if !ok {
		t.Fatal("账号不应消失")
	}
	// 禁用号的 Status.Credits 按口径报 0（「可用量」），真实余额看 CreditsRaw。
	if st.CreditsRaw != 4242 {
		t.Fatalf("CreditsRaw = %d，want 4242（换凭证不该重置积分）", st.CreditsRaw)
	}
	if !st.Disabled {
		t.Fatal("禁用状态被换凭证冲掉了")
	}
}

// TestAdoptVerifiedAddsUnknownAccount：池里没有的 uid 走常规入池（panel 导入新号）。
func TestAdoptVerifiedAddsUnknownAccount(t *testing.T) {
	p := New("")
	if p.AdoptVerified(&auth.Auth{UID: "new", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}) {
		t.Fatal("池内没有该 uid 时应返回 false")
	}
	if a := p.PickByUID("new"); a == nil || a.RefreshTokenValue() != "rt" {
		t.Fatalf("新账号没有入池: %+v", a)
	}
}
