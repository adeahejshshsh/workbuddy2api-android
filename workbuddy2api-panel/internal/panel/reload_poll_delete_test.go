package panel

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 轮询路径必须能把删掉的账号移出去（issue #199 的删除语义在轮询下要真的生效）。
//
// 缺陷现场：reloadIfChanged 用内容指纹做门控，指纹没变就直接 Skipped 返回；而
// 缺席计数 reloadMissing[uid]++ 在 ReloadAccountsReason 内部。删掉一个 auth 文件
// 只让指纹变一次，计到 1 之后每个 tick 都被指纹门挡在外面 —— reload_delete_confirm
// 只要 >1 就永远到不了，删号静默失效。
//
// 变异判据：把 reloadIfChanged 里的「指纹没变就返回」放回无条件早退，本用例红。
func TestReloadPollRemovesDeletedAccountWithConfirmTwo(t *testing.T) {
	e := newReloadEnv(t)
	e.panel.SetReloadConfig(0, 2) // 与生产默认一致：删除需连续 2 次确认
	now := time.Now().Add(24 * time.Hour).Unix()

	e.writeAuth("u1", "at1", now)
	e.writeAuth("u2", "at2", now)
	if res, err := e.panel.reloadIfChanged("poll"); err != nil {
		t.Fatal(err)
	} else if res.Added != 2 {
		t.Fatalf("准备阶段 added=%d want 2", res.Added)
	}

	// 删掉 u2 的文件，之后再没有人动目录。
	if err := os.Remove(filepath.Join(e.dir, "workbuddy-u2.json")); err != nil {
		t.Fatal(err)
	}

	// 连续 5 个轮询 tick（远超 confirm=2）。
	var last ReloadResult
	for i := 0; i < 5; i++ {
		res, err := e.panel.reloadIfChanged("poll")
		if err != nil {
			t.Fatalf("第 %d 轮: %v", i+1, err)
		}
		last = res
	}
	if got := e.poolUIDs(); got["u2"] {
		t.Fatalf("删文件后 5 个 tick 仍未移出池（confirm=2）；最后一轮 %+v", last)
	}
	if got := e.poolUIDs(); !got["u1"] {
		t.Fatal("未删的 u1 被误删")
	}
}

// 删除必须在**恰好** confirm 次缺席后发生，早一次不行、晚一次也不行。
// 这一条同时钉住两件事：不在第 1 次就删（护栏 2 的本意），以及不会拖到第 3 次
// （拖太久会让运维以为功能坏了，正是 issue 报的现象）。
//
// 变异判据：把 `<` 改成 `<=`（变成要 N+1 次）时本用例红。
func TestReloadPollRemovesExactlyAtConfirmCount(t *testing.T) {
	e := newReloadEnv(t)
	e.panel.SetReloadConfig(0, 3) // 用 3 才能区分「第 2 次」「第 3 次」
	now := time.Now().Add(24 * time.Hour).Unix()

	e.writeAuth("solo", "at", now)
	if _, err := e.panel.reloadIfChanged("poll"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(e.dir, "workbuddy-solo.json")); err != nil {
		t.Fatal(err)
	}

	// tick1：完整重载发现缺席，计到 1，不删。
	if _, err := e.panel.reloadIfChanged("poll"); err != nil {
		t.Fatal(err)
	}
	if !e.poolUIDs()["solo"] {
		t.Fatal("第 1 次缺席就删了，护栏 2 失效")
	}
	// tick2：计到 2 < 3，仍不删。
	if _, err := e.panel.reloadIfChanged("poll"); err != nil {
		t.Fatal(err)
	}
	if !e.poolUIDs()["solo"] {
		t.Fatal("第 2 次缺席就删了，confirm=3 没被遵守")
	}
	// tick3：计到 3，必须删。
	res3, err := e.panel.reloadIfChanged("poll")
	if err != nil {
		t.Fatal(err)
	}
	if e.poolUIDs()["solo"] {
		t.Fatal("第 3 次缺席（confirm=3）仍未删，删除被推迟")
	}
	// 删除必须被记账：脚本/面板靠 removed/removed_uids 判断「删的是谁」。
	if res3.Removed != 1 {
		t.Fatalf("删了但 removed=%d，运维看不见", res3.Removed)
	}
	if len(res3.RemovedUIDs) != 1 || res3.RemovedUIDs[0] != "solo" {
		t.Fatalf("removed_uids=%v want [solo]", res3.RemovedUIDs)
	}
	if res3.Skipped {
		t.Fatal("真删了却仍标 Skipped")
	}
}

// 单账号（池里只有被删的那个）同样要能删掉：确认不是「因为还有别的账号在动才触发」。
func TestReloadPollRemovesLastAccount(t *testing.T) {
	e := newReloadEnv(t)
	e.panel.SetReloadConfig(0, 2)
	now := time.Now().Add(24 * time.Hour).Unix()

	e.writeAuth("solo", "at", now)
	if res, err := e.panel.reloadIfChanged("poll"); err != nil {
		t.Fatal(err)
	} else if res.Added != 1 {
		t.Fatalf("准备阶段 added=%d want 1", res.Added)
	}
	if err := os.Remove(filepath.Join(e.dir, "workbuddy-solo.json")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, err := e.panel.reloadIfChanged("poll"); err != nil {
			t.Fatalf("第 %d 轮: %v", i+1, err)
		}
	}
	if e.poolUIDs()["solo"] {
		t.Fatal("唯一账号删文件后仍未移出池")
	}
}

// 在途请求未结束时，轮询路径也不能删——护栏 2 的后半条必须在新路径里同样生效。
//
// 变异判据：confirmMissing 里去掉 `InFlight(st.UID) > 0` 判断时本用例红。
func TestReloadPollSkipsRemovalWhileInFlight(t *testing.T) {
	e := newReloadEnv(t)
	e.panel.SetReloadConfig(0, 2)
	now := time.Now().Add(24 * time.Hour).Unix()

	e.writeAuth("u1", "at1", now)
	if _, err := e.panel.reloadIfChanged("poll"); err != nil {
		t.Fatal(err)
	}
	if !e.pool.Acquire("u1") {
		t.Fatal("获取在途名额失败")
	}
	if err := os.Remove(filepath.Join(e.dir, "workbuddy-u1.json")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if _, err := e.panel.reloadIfChanged("poll"); err != nil {
			t.Fatalf("第 %d 轮: %v", i+1, err)
		}
	}
	if !e.poolUIDs()["u1"] {
		t.Fatal("在途请求未结束时账号被删了")
	}
	e.pool.Release("u1")
	if _, err := e.panel.reloadIfChanged("poll"); err != nil {
		t.Fatal(err)
	}
	if e.poolUIDs()["u1"] {
		t.Fatal("在途结束后应立刻删掉（缺席计数已累计到 confirm）")
	}
}

// 删完之后缺席计数表必须清干净：残留的 uid 会让下一轮误判「还有待确认」，
// 把空闲轮询永久拖进 stat 路径。
//
// 变异判据：confirmMissing 里去掉 `delete(p.reloadMissing, uid)` 时本用例红。
func TestReloadPollClearsMissingTableAfterRemoval(t *testing.T) {
	e := newReloadEnv(t)
	e.panel.SetReloadConfig(0, 2)
	now := time.Now().Add(24 * time.Hour).Unix()

	e.writeAuth("u1", "at1", now)
	if _, err := e.panel.reloadIfChanged("poll"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(e.dir, "workbuddy-u1.json")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := e.panel.reloadIfChanged("poll"); err != nil {
			t.Fatalf("第 %d 轮: %v", i+1, err)
		}
	}
	if e.poolUIDs()["u1"] {
		t.Fatal("前提失败：u1 没被删掉")
	}
	e.panel.reloadMu.Lock()
	left := len(e.panel.reloadMissing)
	e.panel.reloadMu.Unlock()
	if left != 0 {
		t.Fatalf("删完后缺席计数表还剩 %d 条（应为 0）", left)
	}
	// 残留的缺席计数会让空闲轮询永远走进 stat 路径：真实判据是「下一轮必须干净地跳过」。
	res, err := e.panel.reloadIfChanged("poll")
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 0 || res.Added != 0 || res.Updated != 0 {
		t.Fatalf("删完后空闲轮询仍有动作：%+v", res)
	}
	if !res.Skipped {
		t.Fatalf("删完后空闲轮询应 Skipped，实际 %+v", res)
	}
}

// 指纹门控仍然要挡自激：没人动目录时轮询不能真的重复扫描/重载。
// 这条与上面几条是同一个改动的两侧约束——放宽删除不能把「无变化就跳过」弄丢。
// 同时钉住「没有待确认项时绝不去走 stat 路径」。
func TestReloadPollStaysIdleWhenNothingChanges(t *testing.T) {
	e := newReloadEnv(t)
	e.panel.SetReloadConfig(0, 2)
	now := time.Now().Add(24 * time.Hour).Unix()

	e.writeAuth("u1", "at1", now)
	e.writeAuth("u2", "at2", now)
	if res, err := e.panel.reloadIfChanged("poll"); err != nil {
		t.Fatal(err)
	} else if res.Added != 2 {
		t.Fatalf("准备阶段 added=%d want 2", res.Added)
	}
	for i := 0; i < 5; i++ {
		res, err := e.panel.reloadIfChanged("poll")
		if err != nil {
			t.Fatalf("第 %d 轮: %v", i+1, err)
		}
		if !res.Skipped {
			t.Fatalf("第 %d 轮目录没变却做了完整重载：%+v", i+1, res)
		}
		if res.Removed != 0 || res.Added != 0 || res.Updated != 0 {
			t.Fatalf("第 %d 轮空闲却动了池：%+v", i+1, res)
		}
	}
}
