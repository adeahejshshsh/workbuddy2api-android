// sweep.go 补领清扫排程：分钟级周期，只 accept 待接受的、只 claim 已达标未领的。
//
// 执行体在面板（panel/tasksweep.go）：main 装配期用 SetSweepRunner 注入（panel 依赖
// 本包，反向 import 会成环）。独立于 Run 的小时制排程——补领是分钟级观测量，且不跑
// 行为链路，代价只是每账号 2 次列表请求。
//
// 与 growth_hours（一天跑一次成长队列）的区别见 issue #174：新号的待办在自动跑那一刻
// 往往还没达标（awaiting_progress），而队列一天只跑一次；白名单（autoActions）之外的
// 码更是永远进不了队列。补领清扫不看白名单，判据只有 progress，所以周期跑就能兜住。
package scheduler

import (
	"context"
	"fmt"
	"log"
	"time"
)

// RunTaskSweepNow 立即执行一轮补领清扫。未注入执行器（nil）时静默返回：
// 测试与未装配面板的部署不必额外判断。
//
// 任务族锁：与面板「立即补领」按钮共用 TaskSweep（bug P0-3 同一纪律）——两路同时
// 扇出会把每个账号打两遍上游，且领奖接口虽然幂等，重复请求仍会计进上游风控面。
func (s *Scheduler) RunTaskSweepNow() {
	s.schedMu.Lock()
	fn := s.cfg.SweepRunner
	s.schedMu.Unlock()
	if fn == nil {
		return
	}
	if !s.TryBeginTask(TaskSweep) {
		log.Printf("scheduler: 补领清扫跳过：上一轮仍在执行")
		return
	}
	// EndTask 必须 defer + 整条链 recover：本 goroutine 不是 net/http 起的，
	// panic 无人兜底就是整个进程退出；且任务族锁不释放会让面板按钮恒 409。
	defer s.EndTask(TaskSweep)
	defer func() {
		if r := recover(); r != nil {
			log.Printf("ERR: scheduler: task sweep panic: %v", r)
		}
	}()
	_, msg := fn()
	log.Printf("补领清扫：%s", msg)
}

// StartTaskSweep 后台周期补领清扫（独立 ticker goroutine，ctx 取消即停）。
// interval<=0 起步即暂停，但**循环照样创建**（与 StartBalanceRefresh 同一理由）：
// 直接 return 会让「面板打开开关」只 poke 一个没有监听者的 channel，补领永远不会
// 恢复，只能重启进程。循环体本身能处理 cur<=0（等 rearm）。
// 运行期可用 SetTaskSweepInterval 热改间隔（下一轮生效）。
func (s *Scheduler) StartTaskSweep(ctx context.Context, interval time.Duration) {
	if interval < 0 {
		interval = 0
	}
	s.sweepInterval.Store(int64(interval))
	go func() {
		// 哨兵初值 -1：启动即暂停（interval=0）时也要打出「暂停中显示 0s」那条日志。
		logged := time.Duration(-1)
		round := 0
		for {
			cur := time.Duration(s.sweepInterval.Load())
			// 抖动 key 随轮次变化（同余额刷新）：key 恒定只会得到「恒定间隔 + 固定相位」，
			// 防机械节拍的目标落空。
			round++
			delay := cur + periodicJitter(cur, s.sweepJitterFrac(), fmt.Sprintf("sweep:%d", round))
			if delay < 0 {
				delay = 0
			}
			if cur != logged {
				log.Printf("scheduler: 补领清扫每 %s（暂停中显示 0s）", cur)
				logged = cur
			}
			if cur <= 0 {
				select {
				case <-ctx.Done():
					return
				case <-s.rearmSweep:
					continue
				}
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-s.rearmSweep:
				timer.Stop() // 间隔已变：立刻按新值重算
			case <-timer.C:
				s.RunTaskSweepNow()
			}
		}
	}()
}

// SetTaskSweepInterval 热改补领清扫间隔；<=0 表示暂停循环（面板关闭该开关时）。
func (s *Scheduler) SetTaskSweepInterval(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.sweepInterval.Store(int64(d))
	poke(s.rearmSweep)
}

// SetTaskSweepJitter 热改补领清扫的抖动幅度（千分比，负数按 0）。
func (s *Scheduler) SetTaskSweepJitter(permill int) {
	if permill < 0 {
		permill = 0
	}
	s.sweepJitter.Store(int64(permill))
	poke(s.rearmSweep)
}

// sweepJitterFrac 抖动比例（0–0.5）：千分比 / 1000。
func (s *Scheduler) sweepJitterFrac() float64 {
	return float64(s.sweepJitter.Load()) / 1000
}

// SetSweepRunner 挂载/替换补领清扫执行器（panel 构造晚于 scheduler，装配期注入）。
func (s *Scheduler) SetSweepRunner(fn func() (int, string)) {
	s.schedMu.Lock()
	s.cfg.SweepRunner = fn
	s.schedMu.Unlock()
}
