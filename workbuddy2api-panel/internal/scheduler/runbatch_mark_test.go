package scheduler

// 「今天已跑」标记只能记在**真的跑完**的任务族上。
//
// 此前 runBatch 之后无条件 markRan：09:00 触发时收到 SIGTERM（goroutine 秒退）、
// 或上一轮仍在执行被互斥挡掉，都会写下 checkin=今天 —— 重启后 missedToday 判定
// 「今天已补跑」，当天整轮签到/保活不再执行，而且没有任何报错。

import (
	"context"
	"testing"
)

// ctx 已取消：各任务族只是收尾返回，不能算跑完。
func TestRunBatchSkipsMarkWhenCancelled(t *testing.T) {
	s := New(Config{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := s.runBatch(ctx, []taskKind{taskGrowth})
	if len(done) != 0 {
		t.Fatalf("取消的 ctx 不该产出「已完成」的族，实际 %v", done)
	}
}

// 上一轮仍在执行（互斥抢不到）：本轮被跳过，不能算跑完。
func TestRunBatchSkipsMarkWhenFamilyBusy(t *testing.T) {
	s := New(Config{})
	key := taskKindKey(taskGrowth)
	if !s.TryBeginTask(key) {
		t.Fatal("前置条件：应能抢到锁")
	}
	defer s.EndTask(key)

	done := s.runBatch(context.Background(), []taskKind{taskGrowth})
	if len(done) != 0 {
		t.Fatalf("被互斥跳过的族不该记为已完成，实际 %v", done)
	}
}

// 正常跑完：应记为已完成（否则当天不会再跑，任务被永久跳过）。
func TestRunBatchMarksCompletedFamily(t *testing.T) {
	s := New(Config{})
	done := s.runBatch(context.Background(), []taskKind{taskGrowth})
	if len(done) != 1 || done[0] != taskGrowth {
		t.Fatalf("跑完的族应被标记，实际 %v", done)
	}
}
