package panel

import (
	"sync"
	"sync/atomic"
	"time"
)

// 可中断等待（issue ISSUE-11）。
//
// 为什么需要：一键完成/任务动作里有大量等待，最长的 mp 事件节流是 45~55s/条、
// 最多 10 条 ≈ 8 分钟。这些等待原本是裸 time.Sleep —— 客户端断连后流水线仍跑完
// 全程，还一直持着 per-account 锁（用户重试全撞 409）。
//
// 实现取舍：不改动十几个动作函数的签名（它们不持有 ctx），改用「按账号登记的
// 取消标记」+ 可中断睡眠：HTTP 侧 ctx 结束时给该账号打标记，睡眠每 250ms 检查一次。
// 标记在每次任务开始前清除，任务结束（含超时）后由 watcher 清理。

var abortedAccounts sync.Map // uid -> struct{}

// markAborted 标记某账号的任务应尽快中止。
func markAborted(uid string) { abortedAccounts.Store(uid, struct{}{}) }

// clearAborted 清除标记（任务开始/结束时调用）。
func clearAborted(uid string) { abortedAccounts.Delete(uid) }

// isAborted 该账号的任务是否已被要求中止。
func isAborted(uid string) bool {
	_, ok := abortedAccounts.Load(uid)
	return ok
}

// sleepAB 可中断睡眠：返回 false 表示期间该账号被要求中止，调用方应尽快返回。
// 每 250ms 检查一次，对秒级以上的等待几乎没有额外开销。
func sleepAB(uid string, d time.Duration) bool {
	const step = 250 * time.Millisecond
	deadline := time.Now().Add(d)
	for {
		if isAborted(uid) {
			return false
		}
		left := time.Until(deadline)
		if left <= 0 {
			return true
		}
		if left > step {
			left = step
		}
		time.Sleep(left)
	}
}

// watchAbort 在 ctx 结束（客户端断连/HTTP 超时）时给账号打中止标记。
// 返回的 stop 用于任务正常结束时停止监听并清标记。
func watchAbort(done <-chan struct{}, uid string) (stop func()) {
	finished := make(chan struct{})
	var once sync.Once
	var stopped atomic.Bool
	go func() {
		select {
		case <-done:
			// 二次确认（bug B9）：select 在 done 与 finished 同时就绪时随机选一个，
			// 若 stop() 已经 clearAborted 之后这里才 markAborted，标记就残留了 ——
			// 而队列路径（runQueueItems）不调 clearAborted，下一次任务会在 sleepAB
			// 处直接返回 false 提前退出。
			if !stopped.Load() {
				markAborted(uid)
			}
		case <-finished:
		}
	}()
	return func() {
		once.Do(func() {
			stopped.Store(true)
			close(finished)
			clearAborted(uid)
		})
	}
}
