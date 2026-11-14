package panel

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 护栏 6：目录里一个账号文件都没有而池不空 → 不删。
// 挂载掉了、目录被临时清空时，「每个号都缺席」会让缺席计数一满就把整池清掉；
// 这种删除还会落盘，下次启动也不复活。
func TestReloadKeepsPoolWhenAuthDirEmptied(t *testing.T) {
	e := newReloadEnv(t)
	e.panel.SetReloadConfig(0, 1) // confirm=1：只要判一次就该删，好让护栏效果可见
	now := time.Now().Add(24 * time.Hour).Unix()
	e.writeAuth("u1", "at1", now)
	e.writeAuth("u2", "at2", now)
	if res := e.reload(); res.Added != 2 {
		t.Fatalf("准备阶段 added=%d want 2", res.Added)
	}

	// 模拟「挂载掉了」：目录还在，但里面的账号文件全没了。
	for _, name := range []string{"workbuddy-u1.json", "workbuddy-u2.json"} {
		if err := os.Remove(filepath.Join(e.dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		res := e.reload()
		if res.Removed != 0 {
			t.Fatalf("第 %d 轮：空目录把账号删了（removed=%d）", i+1, res.Removed)
		}
		if res.Reason == "" {
			t.Fatalf("第 %d 轮：应给出「疑似未挂载」的说明", i+1)
		}
	}
	if uids := e.poolUIDs(); !uids["u1"] || !uids["u2"] {
		t.Fatalf("空目录不该动池子，实际池内=%v", uids)
	}
	// 目录恢复后账号照旧（没有缺席计数残留导致的延迟删除）。
	e.writeAuth("u1", "at1", now)
	e.writeAuth("u2", "at2", now)
	if res := e.reload(); res.Removed != 0 {
		t.Fatalf("目录恢复后仍删了账号：removed=%d", res.Removed)
	}
	if uids := e.poolUIDs(); !uids["u1"] || !uids["u2"] {
		t.Fatalf("目录恢复后账号应还在池里，实际=%v", uids)
	}
}

// 目录读不动（权限/IO 错误）时，LoadDirReport 必须把错误抛出来，而不是当成「0 个文件」——
// 后者会让热重载把整池删光。
func TestLoadDirReportSurfacesIOError(t *testing.T) {
	e := newReloadEnv(t)
	if err := os.RemoveAll(e.dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.dir, []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.panel.ReloadAccountsReason("manual"); err == nil {
		t.Fatal("目录读不动时应返回错误，而不是当作空目录继续删号")
	}
}
