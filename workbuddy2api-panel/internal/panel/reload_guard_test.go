package panel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 回归：目录里一个账号文件都没有（挂载掉了、被临时清空）而池不空时，整池不能被删。
//
// 缺陷现场：Glob 把读目录失败吞成「0 个文件」，缺席计数连续 2 轮就把整池剔除，
// 剔除结果还会持久化；目录恢复后禁用/暂停/分组全丢。
//
// 变异判据：去掉 ReloadAccountsForce 里 report.Files == 0 的早退，本用例红。
func TestReloadNeverEmptiesPool(t *testing.T) {
	e := newReloadEnv(t)
	now := time.Now().Add(24 * time.Hour).Unix()
	e.writeAuth("u1", "at1", now)
	e.writeAuth("u2", "at2", now)
	if res := e.reload(); res.Added != 2 {
		t.Fatalf("准备阶段 added=%d want 2", res.Added)
	}
	for _, uid := range []string{"u1", "u2"} {
		if err := os.Remove(filepath.Join(e.dir, "workbuddy-"+uid+".json")); err != nil {
			t.Fatal(err)
		}
	}
	for round := 1; round <= 5; round++ {
		res := e.reload()
		if res.Removed != 0 || res.Reason == "" {
			t.Fatalf("第 %d 轮目录被清空却删了号或没说明原因：%+v", round, res)
		}
	}
	if got := e.poolUIDs(); !got["u1"] || !got["u2"] {
		t.Fatalf("目录清空期间池被删了：%v", got)
	}

	// 目录整个不见了（挂载点没了）同理，轮询路径也一样。
	e.panel.cfg.AuthDir = filepath.Join(e.dir, "gone")
	for round := 1; round <= 5; round++ {
		if _, err := e.panel.reloadIfChanged("poll"); err != nil {
			t.Fatalf("第 %d 轮: %v", round, err)
		}
	}
	if got := e.poolUIDs(); !got["u1"] || !got["u2"] {
		t.Fatalf("目录不存在期间池被删了：%v", got)
	}
}

// 回归：删文件的号只是暂存，文件回来时禁用、暂停、分组原样复活；同样适用于进程重启之后。
//
// 变异判据：把 removeMissingLocked 里的 ParkMany 换回 RemoveMany，本用例红。
func TestReloadParkedAccountKeepsStateWhenFileReturns(t *testing.T) {
	e := newReloadEnv(t)
	now := time.Now().Add(24 * time.Hour).Unix()
	for _, uid := range []string{"keep", "u1", "u2"} {
		e.writeAuth(uid, "at-"+uid, now)
	}
	e.reload()
	e.pool.Disable("u1", "人工禁用")
	if !e.pool.Pause("u2") || !e.pool.SetGroup("u2", "vip") {
		t.Fatal("准备 u2 的暂停/分组失败")
	}

	for _, uid := range []string{"u1", "u2"} {
		if err := os.Remove(filepath.Join(e.dir, "workbuddy-"+uid+".json")); err != nil {
			t.Fatal(err)
		}
	}
	e.reload()
	if res := e.reload(); res.Removed != 2 {
		t.Fatalf("连续 2 轮后应移出 u1/u2：%+v", res)
	}

	e.writeAuth("u1", "at-u1", now)
	e.writeAuth("u2", "at-u2", now)
	if res := e.reload(); res.Added != 2 {
		t.Fatalf("文件回来应重新入池：%+v", res)
	}
	st := map[string]bool{}
	for _, s := range e.pool.List() {
		switch s.UID {
		case "u1":
			st["u1-disabled"] = s.Disabled
		case "u2":
			st["u2-paused"] = s.Paused
			st["u2-group"] = s.Group == "vip"
		}
	}
	if !st["u1-disabled"] || !st["u2-paused"] || !st["u2-group"] {
		t.Fatalf("文件回来后人工状态丢了：%v", st)
	}
}

// 单轮删除上限：一轮要删的号超过池的 1/5（至少放行 2 个）时一个都不删，手动 force 才放行。
//
// 变异判据：去掉 removeMissingLocked 里的上限判断，本用例红。
func TestReloadCapsRemovalsPerRound(t *testing.T) {
	e := newReloadEnv(t)
	now := time.Now().Add(24 * time.Hour).Unix()
	uids := []string{"u0", "u1", "u2", "u3", "u4", "u5", "u6", "u7", "u8", "u9"}
	for _, uid := range uids {
		e.writeAuth(uid, "at-"+uid, now)
	}
	e.reload()
	for _, uid := range uids[:4] { // 4 > max(2, 10/5)
		if err := os.Remove(filepath.Join(e.dir, "workbuddy-"+uid+".json")); err != nil {
			t.Fatal(err)
		}
	}
	for round := 1; round <= 4; round++ {
		res := e.reload()
		if res.Removed != 0 {
			t.Fatalf("第 %d 轮超过上限仍删了 %d 个", round, res.Removed)
		}
	}
	if res := e.reload(); !strings.Contains(res.Reason, "force=1") || res.Pending != 4 {
		t.Fatalf("应提示 force 且 pending=4：%+v", res)
	}
	if len(e.poolUIDs()) != 10 {
		t.Fatalf("上限拦下后池应保持 10 个：%v", e.poolUIDs())
	}
	res, err := e.panel.ReloadAccountsForce("manual", true)
	if err != nil || res.Removed != 4 {
		t.Fatalf("force 后应移出 4 个：%+v err=%v", res, err)
	}
}

// 回归：重载不能把刚刷新的令牌换回文件里的旧值，也不能换掉池里的 *auth.Auth 指针。
//
// 缺陷现场：upsert 直接 e.a = 新读出的对象，扫描早于刷新写回时，新对象带的是已被轮换
// 作废的 refresh token。
//
// 变异判据：Reconcile 换回 upsertLocked 的换指针语义，本用例红。
func TestReloadKeepsRefreshedTokenInPlace(t *testing.T) {
	e := newReloadEnv(t)
	now := time.Now().Add(24 * time.Hour).Unix()
	path := e.writeAuth("u1", "at-old", now)
	e.reload()
	cur := e.pool.AuthByUID("u1")

	// 模拟刷新：内存里令牌已换新、过期更晚；磁盘上还是旧值（写回落后于扫描）。
	cur.Lock()
	cur.AccessToken, cur.RefreshToken, cur.ExpiresAt = "at-refreshed", "rt-refreshed", now+7200
	cur.Unlock()
	// 让磁盘内容变化（触发重载）但令牌不比内存新：同一个旧令牌 + 补个昵称。
	stale := &auth.Auth{UID: "u1", AccessToken: "at-old", RefreshToken: "rt-u1", ExpiresAt: now, Nickname: "n", FilePath: path}
	if err := stale.SaveAtomic(); err != nil {
		t.Fatal(err)
	}

	res := e.reload()
	if res.Updated != 0 {
		t.Fatalf("磁盘令牌更旧，不该算更新：%+v", res)
	}
	if e.pool.AuthByUID("u1") != cur {
		t.Fatal("池里的 *auth.Auth 指针被换掉了")
	}
	if got := cur.AccessTokenValue(); got != "at-refreshed" {
		t.Fatalf("刷新后的令牌被盖回旧值：%q", got)
	}
}
