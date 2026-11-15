package scheduler

import (
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 账号级族锁：同账号同族互斥，不同账号不互斥（审查 E4）。
func TestAccountFamilyLockScopes(t *testing.T) {
	s := New(Config{})
	if !s.TryBeginAccountTask("u1", TaskTravel) {
		t.Fatal("首次应拿得到")
	}
	if s.TryBeginAccountTask("u1", TaskTravel) {
		t.Fatal("同账号同族必须互斥（手动入口与排程撞车就是这里漏了）")
	}
	if !s.TryBeginAccountTask("u2", TaskTravel) {
		t.Fatal("不同账号不该互斥（按号端点的意义就是每号独立时点）")
	}
	if !s.TryBeginAccountTask("u1", TaskKeepalive) {
		t.Fatal("同账号不同族不该互斥")
	}
	s.EndAccountTask("u2", TaskTravel)
	s.EndAccountTask("u1", TaskKeepalive)
	s.EndAccountTask("u1", TaskTravel)
	if !s.TryBeginAccountTask("u1", TaskTravel) {
		t.Fatal("释放后应能重新拿到")
	}
	s.EndAccountTask("u1", TaskTravel)
}

// 并发下只能有一个赢家（族锁不是装饰）。
func TestAccountFamilyLockSingleWinner(t *testing.T) {
	s := New(Config{})
	var wins atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.TryBeginAccountTask("u1", TaskCheckin) {
				wins.Add(1)
				time.Sleep(2 * time.Millisecond)
				s.EndAccountTask("u1", TaskCheckin)
			}
		}()
	}
	wg.Wait()
	if wins.Load() == 0 {
		t.Fatal("至少要有一次成功")
	}
}

// 空闲条目会被清理，表不会无限增长（账号反复增删场景）。
func TestFamilyLockPrunesIdle(t *testing.T) {
	f := &familyLocks{}
	// 直接塞满到超过软上限，全部标成很久以前用过。
	f.mu.Lock()
	f.m = make(map[string]*familyLock, maxFamilyLocks+16)
	for i := 0; i < maxFamilyLocks+16; i++ {
		l := &familyLock{}
		l.used.Store(time.Now().Add(-2 * familyLockIdle).UnixNano())
		f.m[string(rune(i))+"\x00x"] = l
	}
	f.pruneLocked()
	n := len(f.m)
	f.mu.Unlock()
	if n >= maxFamilyLocks+16 {
		t.Fatalf("清理没生效：仍有 %d 条", n)
	}
}

// 排程签到的 accountPace 必须在国际版分支之前（审查 E4）：
// 此前 global 号 `continue` 跳过它，一池国际号会在同一秒连发。
func TestCheckinPacesBeforeGlobalBranch(t *testing.T) {
	raw := mustReadFile(t, "scheduler.go")
	src := string(raw)
	i := mustIndex(t, src, "plan := s.spreadPlan(taskCheckin")
	rest := src[i:]
	paceAt := mustIndex(t, rest, "s.accountPace(ctx, plan, st.UID, checkinAccountDelay)")
	globalAt := mustIndex(t, rest, "if a.IsGlobal() {")
	if paceAt > globalAt {
		t.Fatal("accountPace 必须在 IsGlobal 分支之前，否则国际号跳过节流")
	}
}

func mustReadFile(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("读 %s: %v", name, err)
	}
	return raw
}

func mustIndex(t *testing.T, s, sub string) int {
	t.Helper()
	i := strings.Index(s, sub)
	if i < 0 {
		t.Fatalf("找不到 %q", sub)
	}
	return i
}
