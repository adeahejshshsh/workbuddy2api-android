package scheduler

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 同一天重启多次只补跑一次：第一次启动补了哪几类，重启后就不该再补哪几类。
func TestStartupCatchUpRunsOncePerDay(t *testing.T) {
	stamp := filepath.Join(t.TempDir(), "task-runs.json")
	cfg := Config{CheckinHours: []int{9}, RunStampFile: stamp}
	now := time.Date(2026, 10, 20, 15, 0, 0, 0, time.Local) // 签到的 9 点已过

	first := New(cfg)
	missed := first.missedToday(now)
	if !hasKind(missed, taskCheckin) {
		t.Fatalf("首轮应补签到，got %v", missed)
	}
	first.markRan(missed, now)

	// 模拟重启：新实例、同一份标记文件。
	if got := New(cfg).missedToday(now); len(got) != 0 {
		t.Fatalf("重启后不该再补，got %v", got)
	}
	// 第二天照旧补。
	if got := New(cfg).missedToday(now.AddDate(0, 0, 1)); !hasKind(got, taskCheckin) {
		t.Fatalf("次日应重新补，got %v", got)
	}
}

// 未注入标记文件时不落盘、行为与旧实现一致（每次启动都补）。
func TestStartupCatchUpWithoutStampFile(t *testing.T) {
	s := New(Config{CheckinHours: []int{9}})
	now := time.Date(2026, 10, 20, 15, 0, 0, 0, time.Local)
	s.markRan([]taskKind{taskCheckin}, now) // 空路径 = 不写盘，不能 panic
	if got := s.missedToday(now); !hasKind(got, taskCheckin) {
		t.Fatalf("got %v", got)
	}
}

// 标记文件损坏：宁可多补一轮，也不能因此不跑。
func TestStartupCatchUpToleratesCorruptStamp(t *testing.T) {
	stamp := filepath.Join(t.TempDir(), "task-runs.json")
	if err := os.WriteFile(stamp, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 20, 15, 0, 0, 0, time.Local)
	if got := New(Config{CheckinHours: []int{9}, RunStampFile: stamp}).missedToday(now); !hasKind(got, taskCheckin) {
		t.Fatalf("got %v", got)
	}
}
