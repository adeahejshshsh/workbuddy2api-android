package panel

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSkipGate 「执行了但没进展」连续 N 次 → 跳过；别的账号不连坐；有进展即清零。
func TestSkipGate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task_skip.json")
	p := &Panel{cfg: Config{SkipFile: path}}

	if blocked, _ := p.skipBlocked("u1", "chat_5"); blocked {
		t.Fatal("空名单不该拦任何任务")
	}
	for i := 1; i < skipFailLimit; i++ {
		if added, n := p.skipNote("u1", "chat_5", "进度未变化"); added || n != i {
			t.Fatalf("第 %d 次不该跳过（added=%v n=%d）", i, added, n)
		}
	}
	added, n := p.skipNote("u1", "chat_5", "进度未变化")
	if !added || n != skipFailLimit {
		t.Fatalf("第 %d 次该跳到跳过线（added=%v n=%d）", skipFailLimit, added, n)
	}
	if blocked, why := p.skipBlocked("u1", "chat_5"); !blocked || why == "" {
		t.Fatalf("到线后该拦（blocked=%v why=%q）", blocked, why)
	}
	if blocked, _ := p.skipBlocked("u2", "chat_5"); blocked {
		t.Fatal("跳过是按 (账号,任务码) 记的，不该连坐别的账号")
	}
	// 真有一次进展 → 计数清零，回到待办
	p.skipReset("u1", "chat_5")
	if blocked, _ := p.skipBlocked("u1", "chat_5"); blocked {
		t.Fatal("有进展（reset）后不该再拦")
	}
	// 落盘：重启进程后名单还在（否则每次重启都会重试一遍做不了的任务）
	if raw, err := os.ReadFile(path); err != nil || len(raw) == 0 {
		t.Fatalf("跳过名单没落盘：err=%v size=%d", err, len(raw))
	}
}

// TestSkipCooldownExpiry 冷却到期自动放回：做不了不该是永久判决。
func TestSkipCooldownExpiry(t *testing.T) {
	p := &Panel{cfg: Config{SkipFile: filepath.Join(t.TempDir(), "s.json")}}
	for i := 0; i < skipFailLimit; i++ {
		p.skipNote("u", "c", "x")
	}
	if blocked, _ := p.skipBlocked("u", "c"); !blocked {
		t.Fatal("到线该拦")
	}
	s := p.sk()
	s.mu.Lock()
	st := s.load()
	st.Entries["u|c"].Until = time.Now().Add(-time.Second) // 手动拨到过去
	s.save(st)
	s.mu.Unlock()

	if blocked, _ := p.skipBlocked("u", "c"); blocked {
		t.Fatal("冷却到期应自动放回待办")
	}
	if _, ok := p.sk().load().Entries["u|c"]; ok {
		t.Fatal("到期放回后该把这条记录清掉（否则下次执行立刻又被判跳过）")
	}
}

// TestSkipClear 清空（全清 / 指定账号）。
func TestSkipClear(t *testing.T) {
	p := &Panel{cfg: Config{}}
	for i := 0; i < skipFailLimit; i++ {
		p.skipNote("u1", "a", "x")
		p.skipNote("u2", "b", "x")
	}
	if got := p.skipClear("u1", ""); got != 1 {
		t.Fatalf("按账号清该只剩 1 条被清（got=%d）", got)
	}
	if blocked, _ := p.skipBlocked("u2", "b"); !blocked {
		t.Fatal("清 u1 不该动 u2")
	}
	if got := p.skipClear("", ""); got != 1 {
		t.Fatalf("全清该清掉剩下 1 条（got=%d）", got)
	}
	if rows := p.skipRows(); len(rows) != 0 {
		t.Fatalf("全清后该是空表（got %d）", len(rows))
	}
}

// TestAttemptGate 尝试型默认不放行；预留/未验证的任务必须被标成尝试型。
func TestAttemptGate(t *testing.T) {
	p := &Panel{cfg: Config{}}
	if p.attemptAllowed() {
		t.Fatal("尝试型默认必须为关：任务中心默认只做有把握的")
	}
	p.setAllowAttempt(true)
	if !p.attemptAllowed() {
		t.Fatal("开关没生效")
	}
	// 这些是"判据未验证/预留"的：必须标 Attempt，否则会天天进待办白跑
	for _, code := range []string{
		"black_cat", "Sequential_Tasks_4", "Sequential_Tasks_5",
		"Sequential_Tasks_6", "Sequential_Tasks_7",
	} {
		act := autoActionFor(code)
		if act == nil {
			t.Fatalf("%s 没有动作实现", code)
		}
		if !act.Attempt {
			t.Fatalf("%s 应标为尝试型（上游未证实可点亮）", code)
		}
	}
	// 已验证的动作不该被误标（否则"能做的"会被一起砍掉）
	for _, code := range []string{"chat_5", "RichMeow_Chat", "template_5", "school_season"} {
		if act := autoActionFor(code); act != nil && act.Attempt {
			t.Fatalf("%s 是已验证动作，不该标成尝试型", code)
		}
	}
}

// TestSkipFileCorrupt 状态文件损坏不能把面板搞崩（读不动就当空表）。
func TestSkipFileCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &Panel{cfg: Config{SkipFile: path}}
	if blocked, _ := p.skipBlocked("u", "c"); blocked {
		t.Fatal("损坏文件该被当成空表")
	}
	if _, n := p.skipNote("u", "c", "y"); n != 1 {
		t.Fatalf("损坏文件后写入该从 1 计数（n=%d）", n)
	}
}
