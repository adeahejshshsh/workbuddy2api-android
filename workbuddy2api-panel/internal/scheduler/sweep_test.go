package scheduler

// 补领清扫的周期循环：起跑 / 热改 / 暂停 / 恢复。
//
// 这几条对应两类真实事故形态：
//   - 起步 interval=0 时**提前 return** 会让面板「打开开关」只 poke 一个没有监听者的
//     channel —— 补领永远不恢复，只能重启进程（余额刷新在 bug P0-2 上踩过同款）；
//   - 回调 panic 不兜底会让整个进程退出（这里不是 net/http 起的 goroutine）。

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestStartTaskSweepRunsAndPauses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var runs atomic.Int32
	s := New(Config{SweepRunner: func() (int, string) {
		runs.Add(1)
		return 0, "ok"
	}})
	s.StartTaskSweep(ctx, 5*time.Millisecond)

	waitFor(t, func() bool { return runs.Load() >= 2 }, "清扫循环应至少跑两轮")

	// 热改为 0 = 暂停：之后再等一会，轮次不该继续涨。
	s.SetTaskSweepInterval(0)
	time.Sleep(20 * time.Millisecond)
	before := runs.Load()
	time.Sleep(40 * time.Millisecond)
	if got := runs.Load(); got != before {
		t.Fatalf("暂停后仍在跑：%d → %d", before, got)
	}

	// 再打开：循环必须自己醒过来（不重启进程）。
	s.SetTaskSweepInterval(5 * time.Millisecond)
	waitFor(t, func() bool { return runs.Load() > before }, "重新启用后应继续清扫")
}

func TestStartTaskSweepStartsWithZeroStillWakes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var runs atomic.Int32
	s := New(Config{SweepRunner: func() (int, string) {
		runs.Add(1)
		return 0, "ok"
	}})
	// 起步即暂停（schedule.task_sweep_enabled=false 的形态）。
	s.StartTaskSweep(ctx, 0)
	time.Sleep(20 * time.Millisecond)
	if runs.Load() != 0 {
		t.Fatalf("暂停中不该执行，实际 %d 次", runs.Load())
	}
	s.SetTaskSweepInterval(5 * time.Millisecond)
	waitFor(t, func() bool { return runs.Load() >= 1 }, "起步暂停后启用必须生效（不能要求重启）")
}

func TestTaskSweepPanicDoesNotKillLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var runs atomic.Int32
	s := New(Config{SweepRunner: func() (int, string) {
		if runs.Add(1) == 1 {
			panic("boom")
		}
		return 0, "ok"
	}})
	s.StartTaskSweep(ctx, 5*time.Millisecond)
	waitFor(t, func() bool { return runs.Load() >= 2 }, "首轮 panic 后循环应继续（不能整个进程退出）")
	// panic 后任务族锁必须已释放，否则面板按钮恒 409。
	//
	// 不能「runs>=2 就立刻断言锁可获取」：第 2 轮可能正在执行（它持锁），这会让用例
	// 在整包连跑时偶发假红（实测 3 次里红 2 次）。改成等锁自己放出来，超时才失败。
	cancel()
	locked := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.TryBeginTask(TaskSweep) {
			locked = true
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !locked {
		t.Fatal("panic 后 TaskSweep 锁未释放")
	}
	s.EndTask(TaskSweep)
}

func TestRunTaskSweepNowWithoutRunner(t *testing.T) {
	s := New(Config{})
	s.RunTaskSweepNow() // 不该 panic（未装配面板的部署）
}

// waitFor 轮询等待条件成立（上限 2s），失败即报错。
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(msg)
}
