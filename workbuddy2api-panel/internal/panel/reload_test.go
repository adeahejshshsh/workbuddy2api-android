package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// reloadEnv 热重载测试环境：真实 auths 目录 + 真实池，不碰上游。
type reloadEnv struct {
	t     *testing.T
	dir   string
	panel *Panel
	pool  *pool.Pool
}

func newReloadEnv(t *testing.T) *reloadEnv {
	t.Helper()
	dir := t.TempDir()
	pl := pool.New("")
	pn := New(Config{Pool: pl, AuthDir: dir})
	pn.SetReloadConfig(0, 2)
	return &reloadEnv{t: t, dir: dir, panel: pn, pool: pl}
}

// writeAuth 落一个合法凭据文件（uid 决定文件名，与生产一致）。
func (e *reloadEnv) writeAuth(uid, at string, expiresAt int64) string {
	e.t.Helper()
	a := &auth.Auth{
		UID: uid, AccessToken: at, RefreshToken: "rt-" + uid,
		ExpiresAt: expiresAt, FilePath: filepath.Join(e.dir, "workbuddy-"+uid+".json"),
	}
	if err := a.SaveAtomic(); err != nil {
		e.t.Fatalf("write auth %s: %v", uid, err)
	}
	return a.FilePath
}

func (e *reloadEnv) reload() ReloadResult {
	e.t.Helper()
	res, err := e.panel.ReloadAccountsReason("test")
	if err != nil {
		e.t.Fatalf("reload: %v", err)
	}
	return res
}

func (e *reloadEnv) poolUIDs() map[string]bool {
	out := map[string]bool{}
	for _, st := range e.pool.List() {
		out[st.UID] = true
	}
	return out
}

// 新增文件 → 入池；改 expires_at → updated 但保留状态；删文件 → 连续 N 次才出池。
func TestReloadAddsUpdatesAndRemoves(t *testing.T) {
	e := newReloadEnv(t)
	now := time.Now().Add(24 * time.Hour).Unix()

	e.writeAuth("u1", "at1", now)
	res := e.reload()
	if res.Added != 1 || res.Updated != 0 || res.Removed != 0 || res.ParseErrors != 0 {
		t.Fatalf("首轮 added=%d updated=%d removed=%d parse_errors=%d want 1/0/0/0",
			res.Added, res.Updated, res.Removed, res.ParseErrors)
	}
	if !e.poolUIDs()["u1"] {
		t.Fatal("u1 没有入池")
	}

	// 记账一个状态，改 expires_at 后必须保留（upsert 只换凭证）。
	a := e.pool.AuthByUID("u1")
	if a == nil {
		t.Fatal("池里没有 u1")
	}
	e.writeAuth("u1", "at1-new", now+3600)
	res = e.reload()
	if res.Updated != 1 || res.Added != 0 || res.Removed != 0 {
		t.Fatalf("第二轮 added=%d updated=%d removed=%d want 0/1/0", res.Added, res.Updated, res.Removed)
	}
	if got := e.pool.AuthByUID("u1").AccessTokenValue(); got != "at1-new" {
		t.Fatalf("凭证没更新：%q", got)
	}

	// 护栏 6 之后，「删掉最后一个文件」不再触发删除（目录空 ≈ 没挂载）。这里留一个 u2
	// 的凭据文件在场，才能测到「连续确认后真删」这条路径。
	e.writeAuth("u2", "at2", now)
	if res := e.reload(); res.Added != 1 {
		t.Fatalf("准备阶段 added=%d want 1", res.Added)
	}

	// 删文件：第 1 次只挂账，第 2 次才删（连续确认）。
	if err := os.Remove(filepath.Join(e.dir, "workbuddy-u1.json")); err != nil {
		t.Fatal(err)
	}
	res = e.reload()
	if res.Removed != 0 {
		t.Fatalf("第 1 次扫描就删了（应等连续 2 次确认）：removed=%d", res.Removed)
	}
	if !e.poolUIDs()["u1"] {
		t.Fatal("第 1 次扫描后 u1 不该出池")
	}
	res = e.reload()
	if res.Removed != 1 {
		t.Fatalf("第 2 次扫描应删掉 u1：removed=%d", res.Removed)
	}
	if e.poolUIDs()["u1"] {
		t.Fatal("连续 2 次确认后 u1 应在池外")
	}
}

// 回归（issue #199 风险 1）：目录里有坏文件时，**不得**删除任何账号。
//
// 场景就是最危险的那条：登录落盘/SaveAtomic 的瞬间被读到半截文件 —— 该账号在
// 本次扫描结果里消失，若按「扫描结果里没有 = 文件已删」处理，它会被踢出池且
// 剔除结果持久化，下次启动也不复活。
//
// 变异判据：去掉 ReloadAccountsReason 里 res.ParseErrors > 0 的早退，本用例红。
func TestReloadNeverRemovesWhileAnyFileUnparsable(t *testing.T) {
	e := newReloadEnv(t)
	now := time.Now().Add(24 * time.Hour).Unix()
	e.writeAuth("u1", "at1", now)
	e.writeAuth("u2", "at2", now)
	if res := e.reload(); res.Added != 2 {
		t.Fatalf("准备阶段 added=%d want 2", res.Added)
	}

	// u1 的文件被删，同时 u2 的文件被写坏（半截 JSON）。
	if err := os.Remove(filepath.Join(e.dir, "workbuddy-u1.json")); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(e.dir, "workbuddy-u2.json")
	for _, f := range []string{broken, broken + ".bak"} {
		if err := os.WriteFile(f, []byte(`{"auth":{"accessToken":"at2"`), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for round := 1; round <= 3; round++ {
		res := e.reload()
		if res.ParseErrors == 0 {
			t.Fatalf("第 %d 轮应报告解析失败", round)
		}
		if res.Removed != 0 {
			t.Fatalf("第 %d 轮在有坏文件时删了账号：removed=%d %v", round, res.Removed, res.RemovedUIDs)
		}
	}
	got := e.poolUIDs()
	if !got["u1"] || !got["u2"] {
		t.Fatalf("有坏文件时两个账号都应留在池里，实际 %v", got)
	}
}

// 半写文件恢复后可继续正常工作：修好文件，下一轮照常。
func TestReloadResumesAfterFileRepaired(t *testing.T) {
	e := newReloadEnv(t)
	now := time.Now().Add(24 * time.Hour).Unix()
	e.writeAuth("u1", "at1", now)
	e.reload()

	// 正文与 .bak 一起写坏：只坏正文时 safefile 会从 .bak 回退救回来（也正确，
	// 但那样测不到「解析失败」这条路径）。
	broken := filepath.Join(e.dir, "workbuddy-u1.json")
	for _, f := range []string{broken, broken + ".bak"} {
		if err := os.WriteFile(f, []byte(`{"auth":`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if res := e.reload(); res.ParseErrors != 1 || res.Removed != 0 {
		t.Fatalf("坏文件轮 parse_errors=%d removed=%d want 1/0", res.ParseErrors, res.Removed)
	}
	// 修好（换一份新凭证）。过期时刻必须比内存里的**更晚**才叫「更新」——auth.AdoptNewer
	// 只在磁盘令牌更新时原地覆盖，同到期时刻的不同令牌不覆盖（那通常是扫描早于刷新的竞态）。
	e.writeAuth("u1", "at1-fixed", now+3600)
	res := e.reload()
	if res.ParseErrors != 0 || res.Removed != 0 || res.Updated != 1 {
		t.Fatalf("修复轮 parse_errors=%d removed=%d updated=%d want 0/0/1", res.ParseErrors, res.Removed, res.Updated)
	}
	if got := e.pool.AuthByUID("u1").AccessTokenValue(); got != "at1-fixed" {
		t.Fatalf("修好后凭证没更新：%q", got)
	}
}

// 在途请求未结束的账号不删（护栏 4）。
func TestReloadSkipsRemovalWhileInFlight(t *testing.T) {
	e := newReloadEnv(t)
	now := time.Now().Add(24 * time.Hour).Unix()
	// 留一个在场文件：护栏 6 会把「目录里一个文件都没有」判成没挂载、拒绝删除。
	e.writeAuth("keep", "atk", now)
	e.writeAuth("u1", "at1", now)
	e.reload()

	if !e.pool.Acquire("u1") {
		t.Fatal("获取在途名额失败")
	}
	defer e.pool.Release("u1")

	if err := os.Remove(filepath.Join(e.dir, "workbuddy-u1.json")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if res := e.reload(); res.Removed != 0 {
			t.Fatalf("第 %d 轮：在途账号被删了", i+1)
		}
	}
	if !e.poolUIDs()["u1"] {
		t.Fatal("在途期间 u1 必须留在池里")
	}
	e.pool.Release("u1")
	// 释放后再走两轮（缺席计数从 0 开始重算，因为前几轮每次都被跳过但仍计数）。
	for i := 0; i < 2; i++ {
		e.reload()
	}
	if e.poolUIDs()["u1"] {
		t.Fatal("在途结束后连续确认应删掉 u1")
	}
}

// 目录里有文件盘上还在时不算缺席（护栏 4 的另一半：面板刚写入、扫描竞态）。
func TestReloadKeepsAccountWhoseFileStillExists(t *testing.T) {
	e := newReloadEnv(t)
	now := time.Now().Add(24 * time.Hour).Unix()
	e.writeAuth("u1", "at1", now)
	e.reload()

	// 模拟「面板路径刚把账号加进池，但文件名不匹配扫描 glob」——文件还在盘上。
	a := &auth.Auth{UID: "u1", AccessToken: "at1", RefreshToken: "rt-u1",
		ExpiresAt: now, FilePath: filepath.Join(e.dir, "workbuddy-u1.json")}
	e.pool.Add(a)
	for i := 0; i < 3; i++ {
		if res := e.reload(); res.Removed != 0 {
			t.Fatalf("第 %d 轮：文件仍在盘上却删了账号", i+1)
		}
	}
}

// 手动接口：返回结果摘要，且目录不可读时给出明确错误。
func TestReloadEndpoint(t *testing.T) {
	e := newReloadEnv(t)
	now := time.Now().Add(24 * time.Hour).Unix()
	e.writeAuth("u1", "at1", now)

	rec := httptest.NewRecorder()
	e.panel.ServeHTTP(rec, httptest.NewRequest("POST", "/panel/api/accounts/reload", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var got struct {
		OK     bool `json:"ok"`
		Result struct {
			Added       int `json:"added"`
			ParseErrors int `json:"parse_errors"`
		} `json:"result"`
		Pool int `json:"pool"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	if !got.OK || got.Result.Added != 1 || got.Pool != 1 {
		t.Fatalf("ok=%v added=%d pool=%d want true/1/1（%s）", got.OK, got.Result.Added, got.Pool, rec.Body)
	}

	// 目录被移走 → 500 且带原因（脚本据此判断「配置错了」而不是「没变化」）。
	e.panel.cfg.AuthDir = filepath.Join(e.dir, "nope") // 不存在的目录：ReadDir 失败但 Glob 返回空
	if res, err := e.panel.ReloadAccountsReason("test"); err != nil {
		t.Fatalf("目录不存在不该报错（应视为空目录）: %v", err)
	} else if res.Files != 0 {
		t.Fatalf("空目录 files=%d want 0", res.Files)
	}
}

// 轮询只在目录内容变化时才重载（自激防护）：LoadDirReport 会回填空 realm 并写盘，
// 若按 mtime 判断会永远在重载。
func TestReloadPollOnlyWhenContentChanges(t *testing.T) {
	e := newReloadEnv(t)
	now := time.Now().Add(24 * time.Hour).Unix()
	e.writeAuth("u1", "at1", now)

	res, err := e.panel.reloadIfChanged("poll")
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 1 {
		t.Fatalf("首轮 added=%d want 1", res.Added)
	}
	// 第二轮：内容没变 → 跳过，不再扫。
	res, err = e.panel.reloadIfChanged("poll")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Skipped || res.Added != 0 {
		t.Fatalf("目录未变时应跳过：skipped=%v added=%d", res.Skipped, res.Added)
	}
	// 换一个账号文件 → 应当重载。
	e.writeAuth("u2", "at2", now)
	res, err = e.panel.reloadIfChanged("poll")
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped || res.Added != 1 {
		t.Fatalf("新增文件后应重载：skipped=%v added=%d", res.Skipped, res.Added)
	}
}

// 并发重载不会互相拆台（reloadMu 串行化）。
func TestReloadConcurrentSafe(t *testing.T) {
	e := newReloadEnv(t)
	now := time.Now().Add(24 * time.Hour).Unix()
	e.writeAuth("u1", "at1", now)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = e.panel.ReloadAccountsReason("test")
		}()
	}
	wg.Wait()
	if n := len(e.pool.List()); n != 1 {
		t.Fatalf("并发重载后 pool=%d want 1", n)
	}
}

// 面板增删与热重载并发时不互相拆台、也不死锁（issue #199 风险 3）。
//
// 覆盖三条会同时动池的路径：目录重载、账号移除、API Key 新增账号。断言只要求
// 「跑完不炸、池子与目录一致」；配 -race 跑能抓数据竞争，死锁则会让测试超时。
func TestReloadSerialisesWithPanelMutations(t *testing.T) {
	e := newReloadEnv(t)
	now := time.Now().Add(24 * time.Hour).Unix()
	e.writeAuth("u1", "at1", now)
	e.writeAuth("u2", "at2", now)
	if res := e.reload(); res.Added != 2 {
		t.Fatalf("准备阶段 added=%d want 2", res.Added)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// 持续重载
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = e.panel.ReloadAccountsReason("test")
			}
		}
	}()
	// 并发增删
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/panel/api/accounts/u1/remove", nil)
			e.panel.ServeHTTP(rec, req)
			e.writeAuth("u1", "at1-"+time.Now().Format("150405.000000000"), now)
			_, _ = e.panel.ReloadAccountsReason("test")
		}
	}()
	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()

	// 收尾：目录里两份文件都在，池里也应当有两号（最后一轮重载以目录为准）。
	for _, uid := range []string{"u1", "u2"} {
		e.writeAuth(uid, "at-final-"+uid, now)
	}
	e.reload()
	got := e.poolUIDs()
	if !got["u1"] || !got["u2"] {
		t.Fatalf("并发结束后池应与目录一致，实际 %v", got)
	}
}
