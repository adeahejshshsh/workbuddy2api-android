// familylock.go 账号级 × 任务族 的互斥（审查 E4）。
//
// 现有的 `taskLocks` 是**族级**（整个 checkin 族一把锁），它只能挡「两轮全池排程同时跑」，
// 挡不住「手动按号入口 vs 排程」这类同账号同族的并发 —— 两条路径会同时对这个号发起同一批
// 上游写请求。实测到的后果：
//
//   - 手动旅行与排程旅行并发 → 同一天重复兑换；
//   - 补签循环与 21:00 轮 TOCTOU → 同一个号被签两次；
//   - 余额 401 的 refresh 与保活族的 refresh 并发轮换同一个 RT → 一个健康号被判 session dead；
//   - 后台窗口探针与面板探针同时打同一个额度窗口 → 探针请求翻倍（每次都是真实计费）。
//
// 键是 uid + 族：**不同账号之间不互斥**（按号端点存在的意义就是让外部调度器做到每号独立
// 随机时点，叠加全局锁等于把指纹加回去）。
package scheduler

import (
	"sync"
	"sync/atomic"
	"time"
)

// TaskWindowProbe 额度窗口探针族（后台循环与面板手动触发共用一把账号锁）。
const TaskWindowProbe = "windowprobe"

// maxFamilyLocks 账号级锁表的软上限。条目数 = 账号数 × 族数（几十 × 8），正常远达不到；
// 超了才做一次清理，避免「账号反复增删」把它撑成泄漏。
const maxFamilyLocks = 4096

// familyLockIdle 空闲多久的锁条目可以被清理。
const familyLockIdle = 10 * time.Minute

type familyLock struct {
	mu   sync.Mutex
	used atomic.Int64 // 最近一次使用时刻（unix nano）
}

// familyLocks 账号级族锁表。
type familyLocks struct {
	mu sync.Mutex
	m  map[string]*familyLock
}

// key 账号级族键。用 \x00 分隔，uid 里不会出现该字节。
func familyKey(uid, family string) string { return uid + "\x00" + family }

func (f *familyLocks) entry(uid, family string) *familyLock {
	k := familyKey(uid, family)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.m == nil {
		f.m = make(map[string]*familyLock, 64)
	}
	l := f.m[k]
	if l == nil {
		l = &familyLock{}
		f.m[k] = l
		if len(f.m) > maxFamilyLocks {
			f.pruneLocked()
		}
	}
	return l
}

// pruneLocked 清掉空闲且当前没人持有的条目。调用方持有 f.mu。
func (f *familyLocks) pruneLocked() {
	cutoff := time.Now().Add(-familyLockIdle).UnixNano()
	for k, l := range f.m {
		if l.used.Load() > cutoff {
			continue
		}
		if l.mu.TryLock() { // 拿得到 = 没人在用
			l.mu.Unlock()
			delete(f.m, k)
		}
	}
}

// TryBeginAccountTask 尝试占用「某账号的某任务族」。false = 该号该族正在跑。
//
// 调用方必须在成功后 defer EndAccountTask（成对使用；未配 uid/family 时恒 true）。
func (s *Scheduler) TryBeginAccountTask(uid, family string) bool {
	if s == nil || uid == "" || family == "" {
		return true
	}
	l := s.family.entry(uid, family)
	if !l.mu.TryLock() {
		return false
	}
	l.used.Store(time.Now().UnixNano())
	return true
}

// EndAccountTask 释放 TryBeginAccountTask 占用的锁。
func (s *Scheduler) EndAccountTask(uid, family string) {
	if s == nil || uid == "" || family == "" {
		return
	}
	l := s.family.entry(uid, family)
	l.used.Store(time.Now().UnixNano())
	l.mu.Unlock()
}

// AccountTaskRunning 报告某账号某族是否在跑（只读，不占锁）。
func (s *Scheduler) AccountTaskRunning(uid, family string) bool {
	if s == nil || uid == "" || family == "" {
		return false
	}
	l := s.family.entry(uid, family)
	if l.mu.TryLock() {
		l.mu.Unlock()
		return false
	}
	return true
}
